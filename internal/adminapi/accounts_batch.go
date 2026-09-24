// 账号池批量管理端点：批量新增 / 批量删除 / 批量启用禁用。
// 设计规格见 docs/plan-account-batch-management.md；与既有端点的关系：
//   - 全部为新路由，旧端点（POST /admin/api/accounts、DELETE /admin/api/accounts、
//     POST /admin/api/accounts/{id}/enabled）零改动；
//   - 收尾链路（线路分配 → 额度刷新 → 自动领取）与单轮新增共用 postAddAccounts；
//   - 事务一致性由 store 层保证（单锁 + 单 SQLite 事务，commit 成功才动内存），
//     本层负责请求校验、错误映射与逐条明细回传。
package adminapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// ── 批量新增 ────────────────────────────────────────────────────────────────

// POST /admin/api/accounts/batch/add
// 请求体：{"provider"?, "name"?, "items": [{"name"?, "secret", "email"?}], "proxy_id"?}
// items ≤500；proxy_id 三态语义与现有 POST /admin/api/accounts 一致。
func (h *Handler) handleBatchAddAccounts(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	provider := model.ProviderZai
	if v, ok := payload["provider"]; ok {
		provider = strOf(v)
	}
	if !allowedProvider(provider) {
		writeAPIError(w, errBadRequest("不支持的 provider"))
		return
	}

	rawItems, ok := payload["items"].([]any)
	if !ok || len(rawItems) == 0 {
		writeAPIError(w, errBadRequest("items 必须是非空数组"))
		return
	}
	if len(rawItems) > store.MaxBatchSize {
		writeAPIError(w, errBadRequest(fmt.Sprintf("批量新增条目数超过上限 %d", store.MaxBatchSize)))
		return
	}
	// 命名兜底与现有端点同规则：item.name > 顶层 name > provider-序号。
	baseName := strings.TrimSpace(strOf(firstTruthy(payload["name"])))
	base := len(h.Store.ListAccounts(provider))
	items := make([]store.BatchAddItem, 0, len(rawItems))
	for i, raw := range rawItems {
		obj, isObj := raw.(map[string]any)
		if !isObj {
			writeAPIError(w, errBadRequest(fmt.Sprintf("items[%d] 必须是对象", i)))
			return
		}
		item := store.BatchAddItem{
			Secret: strings.TrimSpace(strOf(firstTruthy(obj["secret"], obj["token"]))),
			Email:  strings.TrimSpace(strOf(obj["email"])),
		}
		if item.Secret == "" {
			writeAPIError(w, errBadRequest(fmt.Sprintf("items[%d].secret 不能为空", i)))
			return
		}
		name := strings.TrimSpace(strOf(firstTruthy(obj["name"])))
		if name == "" {
			name = baseName
		}
		if name == "" {
			name = fmt.Sprintf("%s-%d", provider, base+len(items)+1)
		}
		item.Name = name
		items = append(items, item)
	}

	// proxy_id 三种语义（与 handleAddAccounts 逐字一致）：
	//   __auto__ / null / 键缺省 → 自动挑一条「未被占用的线路」；
	//   __direct__ / 空串        → 显式直连（不分配）；
	//   其余取值                 → 按代理配置 ID 指派。
	autoAssign := true
	profileID := ""
	if v, ok := payload["proxy_id"]; ok {
		switch raw := strings.TrimSpace(strOf(v)); {
		case v == nil, raw == proxyIDAuto:
			autoAssign = true
		case raw == "", raw == proxyIDDirect:
			autoAssign = false
		default:
			autoAssign = false
			profileID = raw
		}
	}
	hasProfile := !autoAssign && profileID != ""
	if hasProfile && !h.profileExists(profileID) {
		writeAPIError(w, errBadRequest("代理配置不存在"))
		return
	}
	hasProxy := false
	var proxyURL *string
	if v, ok := payload["proxy_url"]; ok && !autoAssign && !hasProfile {
		u, err := proxy.NormalizeProxyURL(strOf(v))
		if err != nil {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		hasProxy = true
		proxyURL = u
	}

	start := time.Now()
	res, err := h.Store.BatchAddAccounts(provider, items)
	if err != nil {
		h.logBatchOutcome("新增", res, err, start)
		writeJSON(w, batchErrorStatus(err), batchBody(res, map[string]any{"detail": err.Error()}))
		return
	}
	// 线路指派只作用于本次新建的账号（重复项不动老号，与导入语义一致）。
	// 指派失败时账号已入池（事务已提交），如实回 500——与现有端点的
	// 「中途失败保留已成功项」行为一致。
	if hasProfile || hasProxy {
		for _, id := range res.IDs {
			var err error
			if hasProfile {
				_, err = h.Store.AssignProxyProfile(id, profileID)
			} else {
				_, err = h.Store.SetProxyURL(provider, id, proxyURL)
			}
			if err != nil {
				h.logBatchOutcome("新增", res, err, start)
				writeError500(w, err)
				return
			}
		}
	}
	assignedCount, fallbackCount := h.postAddAccounts(provider, res.IDs, autoAssign)
	h.logBatchOutcome("新增", res, nil, start)
	writeJSON(w, http.StatusOK, batchBody(res, map[string]any{
		"assigned": assignedCount, "direct_fallback": fallbackCount,
	}))
}

// ── 批量删除 ────────────────────────────────────────────────────────────────

// POST /admin/api/accounts/batch/delete
// 请求体：{"ids": [...], "missing_ok"?: bool}
// missing_ok=false（默认，严格）：任一 ID 不存在 → 400 + 明细，一个都不删；
// missing_ok=true：跳过不存在项并逐条标注（对齐旧 DELETE 端点的宽松语义）。
func (h *Handler) handleBatchDeleteAccounts(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	ids, apiErr := parseIDList(payload, "ids")
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	missingOK, apiErr := parseMissingOK(payload)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	start := time.Now()
	res, err := h.Store.BatchRemoveAccounts(ids, missingOK)
	if err != nil {
		h.logBatchOutcome("删除", res, err, start)
		writeJSON(w, batchErrorStatus(err), batchBody(res, map[string]any{"detail": err.Error()}))
		return
	}
	h.logBatchOutcome("删除", res, nil, start)
	writeJSON(w, http.StatusOK, batchBody(res, nil))
}

// ── 批量启用 / 禁用 ────────────────────────────────────────────────────────

// POST /admin/api/accounts/batch/enable 与 /batch/disable 共用实现；
// 字段转移语义由 store 层 applyEnabled 与单账号端点结构性共享。
func (h *Handler) handleBatchEnable(w http.ResponseWriter, r *http.Request) {
	h.handleBatchSetEnabled(w, r, true)
}

func (h *Handler) handleBatchDisable(w http.ResponseWriter, r *http.Request) {
	h.handleBatchSetEnabled(w, r, false)
}

func (h *Handler) handleBatchSetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	ids, apiErr := parseIDList(payload, "ids")
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	missingOK, apiErr := parseMissingOK(payload)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	op := "启用"
	if !enabled {
		op = "禁用"
	}
	start := time.Now()
	res, err := h.Store.BatchSetEnabled(ids, enabled, missingOK)
	if err != nil {
		h.logBatchOutcome(op, res, err, start)
		writeJSON(w, batchErrorStatus(err), batchBody(res, map[string]any{"detail": err.Error()}))
		return
	}
	h.logBatchOutcome(op, res, nil, start)
	writeJSON(w, http.StatusOK, batchBody(res, nil))
}

// ── 条件查询（GET /admin/api/accounts 的 additive 扩展）────────────────────

// parseAccountQuery 解析列表端点的过滤与分页参数。
// 全部参数可选；不传任何参数时结果与旧版全量列表一致（内容、数量、顺序）。
// 未知/非法取值一律 400——与设置项校验同哲学：静默忽略会让管理员拿到一个
// 他不知道被窄化过的列表。
func parseAccountQuery(r *http.Request) (store.AccountQuery, *apiError) {
	q := r.URL.Query()
	out := store.AccountQuery{Archived: "all", Sort: "created_at_asc"}

	if v := q.Get("keyword"); v != "" {
		out.Keyword = v
	}
	if v := q.Get("status"); v != "" {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if !validAccountStatus(s) {
				return out, errBadRequest(fmt.Sprintf(
					"未知状态 %q（可选 active/exhausted/cooling/invalid/disabled）", s))
			}
			out.Statuses = append(out.Statuses, s)
		}
	}
	if v := q.Get("enabled"); v != "" {
		switch v {
		case "true", "false":
			b := v == "true"
			out.Enabled = &b
		default:
			return out, errBadRequest("enabled 需为 true/false")
		}
	}
	if v := q.Get("mode"); v != "" {
		if v != "jwt" && v != "apiKey" {
			return out, errBadRequest("mode 需为 jwt/apiKey")
		}
		out.Mode = v
	}
	if v := q.Get("model_status"); v != "" {
		switch v {
		case "available", "exhausted", "disabled", "absent", "unknown":
			out.ModelStatus = v
		default:
			return out, errBadRequest(
				"model_status 需为 available/exhausted/disabled/absent/unknown")
		}
	}
	if v := q.Get("model"); v != "" {
		out.Model = v
	}
	if v := q.Get("proxy_id"); v != "" {
		out.ProxyID = v
	}
	if v := q.Get("last_error_kind"); v != "" {
		out.LastErrorKind = v
	}
	// 时间戳为 unix 秒（可带小数，与账号时间字段同口径）。
	if v := q.Get("created_after"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return out, errBadRequest("created_after 需为 unix 秒数值")
		}
		out.CreatedAfter = &f
	}
	if v := q.Get("created_before"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return out, errBadRequest("created_before 需为 unix 秒数值")
		}
		out.CreatedBefore = &f
	}
	if v := q.Get("archived"); v != "" {
		switch v {
		case "all", "false", "true":
			out.Archived = v
		default:
			return out, errBadRequest("archived 需为 all/false/true")
		}
	}
	if v := q.Get("sort"); v != "" {
		switch v {
		case "created_at_asc", "created_at_desc", "name", "use_count_desc":
			out.Sort = v
		default:
			return out, errBadRequest(
				"sort 需为 created_at_asc/created_at_desc/name/use_count_desc")
		}
	}
	// limit 不传 = 不分页（保持旧版全量行为）；传则 1–500。
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > store.MaxBatchSize {
			return out, errBadRequest(fmt.Sprintf("limit 需为 1–%d 的整数", store.MaxBatchSize))
		}
		out.Limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return out, errBadRequest("offset 需为 ≥0 的整数")
		}
		out.Offset = n
	}
	return out, nil
}

// validAccountStatus 列表过滤接受的账号状态（与前端状态枚举同源）。
func validAccountStatus(s string) bool {
	switch s {
	case model.StatusActive, model.StatusExhausted, model.StatusCooling,
		model.StatusInvalid, model.StatusDisabled:
		return true
	}
	return false
}

// ── 共用助手 ────────────────────────────────────────────────────────────────

// parseIDList 解析并校验账号 ID 数组字段：非空字符串、去重保序、上限校验
// （store 层 dedupeIDs 再设一道防线）。
func parseIDList(payload map[string]any, key string) ([]string, *apiError) {
	raw, ok := payload[key].([]any)
	if !ok || len(raw) == 0 {
		return nil, errBadRequest(key + " 必须是非空数组")
	}
	if len(raw) > store.MaxBatchSize {
		return nil, errBadRequest(fmt.Sprintf("%s 条目数超过上限 %d", key, store.MaxBatchSize))
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(raw))
	for i, v := range raw {
		s := strings.TrimSpace(strOf(v))
		if s == "" {
			return nil, errBadRequest(fmt.Sprintf("%s[%d] 不能为空", key, i))
		}
		if seen[s] {
			continue // 去重保序（与 store.dedupeIDs 同语义）
		}
		seen[s] = true
		ids = append(ids, s)
	}
	if len(ids) == 0 {
		return nil, errBadRequest(key + " 去重后为空")
	}
	return ids, nil
}

// parseMissingOK 解析可选的 missing_ok 布尔字段（缺省 false = 严格模式）。
func parseMissingOK(payload map[string]any) (bool, *apiError) {
	v, ok := payload["missing_ok"]
	if !ok {
		return false, nil
	}
	b, valid := pyBool(v)
	if !valid {
		return false, errBadRequest("missing_ok 需为布尔值")
	}
	return b, nil
}

// batchErrorStatus 批量操作错误映射：预校验失败与严格模式缺失是调用方输入
// 问题（400），其余（持久化失败等）按服务端错误（500）处理。
func batchErrorStatus(err error) int {
	if errors.Is(err, store.ErrBatchValidation) || errors.Is(err, store.ErrBatchMissingIDs) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// batchBody 批量结果统一响应体；空明细序列化为 [] 而非 null（前端省一次判空）。
func batchBody(res store.BatchResult, extra map[string]any) map[string]any {
	body := map[string]any{
		"total":      res.Total,
		"succeeded":  res.Succeeded,
		"duplicated": res.Duplicated,
		"not_found":  res.NotFound,
		"failed":     res.Failed,
		"ids":        nonNilIDs(res.IDs),
		"items":      nonNilItems(res.Items),
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func nonNilIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

func nonNilItems(items []store.BatchItemResult) []store.BatchItemResult {
	if items == nil {
		return []store.BatchItemResult{}
	}
	return items
}

// logBatchOutcome 每次批量操作记一条汇总日志（module=batch）。逐条明细只进
// 响应体不进日志——500 条逐条打会刷爆日志；汇总计数足以回答「这批怎么样」。
func (h *Handler) logBatchOutcome(op string, res store.BatchResult, err error, start time.Time) {
	msg := fmt.Sprintf("批量%s：共 %d，成功 %d，重复 %d，缺失 %d，失败 %d，耗时 %dms",
		op, res.Total, res.Succeeded, res.Duplicated, res.NotFound, res.Failed,
		time.Since(start).Milliseconds())
	if err != nil {
		web.Err("batch", msg+"："+err.Error())
		return
	}
	web.Ok("batch", msg)
}
