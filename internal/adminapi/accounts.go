// 账号池、验证码、设置、导入导出端点。
// 对应 Python 版 admin_api.py 的对应段落；M6 OAuth 登录留 stub 接入点。
package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
)

func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	quotaPool := map[string]int{}
	for _, p := range store.Providers {
		n := 0
		for _, a := range h.Store.ListAccounts(p) {
			if a.IsSelectable(now) {
				n++
			}
		}
		quotaPool[p] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":              store.Providers,
		"gateway_key_set":        h.Store.GatewayKey() != "",
		"quota_refresh_interval": h.Store.QuotaRefreshInterval(),
		"quota_pool":             quotaPool,
	})
}

func (h *Handler) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, stats := h.accountSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":  accounts,
		"stats":     stats,
		"providers": store.Providers,
		"models":    gateway.AvailableModels,
		"proxies":   h.Store.ListProxyProfiles(),
		"ts":        nowFloat(),
	})
}

// 添加账号时 proxy_id 的两个保留值：前端用它区分「自动挑一条空闲线路」与「就要直连」。
// 其余取值一律按代理配置 ID 处理；null / 键缺省同样按「自动」处理（兼容旧前端）。
const (
	proxyIDAuto   = "__auto__"
	proxyIDDirect = "__direct__"
)

func (h *Handler) handleAddAccounts(w http.ResponseWriter, r *http.Request) {
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
	tokens := parseTokens(payload["tokens"])
	if len(tokens) == 0 {
		writeAPIError(w, errBadRequest("请输入至少一个 Token / API Key"))
		return
	}

	// proxy_id 三种语义：
	//   __auto__ / null / 键缺省 → 建号后自动挑一条「未被占用的线路」；
	//   __direct__ / 空串        → 显式直连（不分配）；
	//   其余取值                 → 按代理配置 ID 指派。
	// null 视作「自动」是为了兼容旧前端——它把「直連」编码成 null 发出来。
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

	// 手工代理只在「显式直连（含 __direct__/空串）」时才看：自动分配会自己挑线路，
	// 指定了 profile 时则以 profile 为准（与原语义一致）。
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

	added := []string{}
	seen := map[string]bool{}
	for _, tok := range tokens {
		if seen[tok] {
			continue // 去重保序（对齐 dict.fromkeys）
		}
		seen[tok] = true
		name := strOf(firstTruthy(payload["name"]))
		if name == "" {
			name = fmt.Sprintf("%s-%d", provider, len(h.Store.ListAccounts(provider))+1)
		}
		acc, err := h.Store.AddAccount(provider, name, tok)
		if err != nil {
			writeError500(w, err)
			return
		}
		if hasProfile {
			if _, err := h.Store.AssignProxyProfile(acc.ID, profileID); err != nil {
				writeError500(w, err)
				return
			}
		} else if hasProxy {
			if _, err := h.Store.SetProxyURL(provider, acc.ID, proxyURL); err != nil {
				writeError500(w, err)
				return
			}
		}
		added = append(added, acc.ID)
	}
	// 自动分配必须排在额度刷新之前：刷新要出站，而 clientFor 只认账号上的 ProxyURL，
	// 线路得先落到账号上（与登录链路「先写线路再兑换/刷新」的约定一致）。
	assignedCount, fallbackCount := 0, 0
	if autoAssign {
		assigned, fallback := h.Store.AutoAssignProxies(added)
		assignedCount, fallbackCount = len(assigned), len(fallback)
	}
	// 对新增的 jwt 账号立即刷新一次额度（仅 zai；对齐 add_accounts 尾段）
	addedSet := map[string]bool{}
	for _, id := range added {
		addedSet[id] = true
	}
	fresh := []*model.Account{}
	for _, a := range h.Store.ListAccounts(provider) {
		if addedSet[a.ID] && a.Mode == "jwt" {
			fresh = append(fresh, a)
		}
	}
	if len(fresh) > 0 {
		h.Quota.RefreshAccounts(fresh)
		// 入池即自动领取（后台 fire-and-forget；对齐 Python add_accounts 尾段）
		for _, acc := range fresh {
			h.scheduleAutoClaim(acc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count": len(added), "ids": added,
		// assigned/direct_fallback 供前端提示「有账号没能用上线路」。
		"assigned": assignedCount, "direct_fallback": fallbackCount,
	})
}

// parseTokens 归一 tokens 字段：字符串按行拆分、数组逐项 strip，过滤空值。
func parseTokens(raw any) []string {
	var out []string
	appendToken := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch t := raw.(type) {
	case string:
		for _, line := range strings.Split(t, "\n") {
			appendToken(line)
		}
	case []any:
		for _, item := range t {
			appendToken(strOf(item))
		}
	}
	return out
}

func allowedProvider(p string) bool {
	for _, provider := range store.Providers {
		if provider == p {
			return true
		}
	}
	return false
}

func (h *Handler) profileExists(profileID string) bool {
	for _, p := range h.Store.ListProxyProfiles() {
		if p.ID == profileID {
			return true
		}
	}
	return false
}

// handleDeleteAccounts 请求体直接是账号 ID 字符串数组（对齐 Python Body(list[str])）。
func (h *Handler) handleDeleteAccounts(w http.ResponseWriter, r *http.Request) {
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeAPIError(w, errBadRequest("请求体应为账号 ID 数组"))
		return
	}
	deleted := 0
	var failed []string
	for _, aid := range ids {
		acc := h.Store.FindAny(aid)
		if acc == nil {
			continue
		}
		ok, err := h.Store.RemoveAccount(acc.Provider, aid)
		switch {
		case err != nil:
			// 落库失败必须让调用方知道：吞掉错误会返回 200 而账号仍在库里，
			// 前端据此提示「已删除」，管理员以为凭证已撤销。
			failed = append(failed, aid)
		case ok:
			deleted++
		}
	}
	resp := map[string]any{"deleted": deleted}
	if len(failed) > 0 {
		resp["failed"] = failed
		writeJSON(w, http.StatusInternalServerError, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) handleEditAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	hasProfile := false
	if _, ok := payload["proxy_id"]; ok {
		hasProfile = true
	}
	profileID := strOf(firstTruthy(payload["proxy_id"]))
	if profileID != "" && !h.profileExists(profileID) {
		writeAPIError(w, errBadRequest("代理配置不存在"))
		return
	}
	// 先把请求体解析、校验成一份补丁，再交给 store 在锁内落库——校验失败时
	// 账号一个字段都不会被改到（旧写法是逐个字段直接改在活对象上）。
	var edit store.AccountEdit
	if v, ok := payload["name"]; ok && truthy(v) {
		name := strings.TrimSpace(strOf(v))
		edit.Name = &name
	}
	if secret := firstTruthy(payload["token"], payload["secret"]); truthy(secret) {
		s := strings.TrimSpace(strOf(secret))
		edit.SetSecret = true
		edit.Secret = s
		edit.SecretMode = "apiKey"
		if strings.Count(s, ".") == 2 && acc.Provider == model.ProviderZai {
			edit.SecretMode = "jwt"
		}
	}
	if v, ok := payload["proxy_url"]; ok && !hasProfile {
		proxyURL, err := proxy.NormalizeProxyURL(strOf(v))
		if err != nil {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		edit.SetProxyURL = true
		edit.ProxyURL = proxyURL
	}
	if v, ok := payload["disabled_models"]; ok {
		models, apiErr := parseDisabledModels(v)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		edit.SetDisabled = true
		edit.Disabled = models
	}
	if _, err := h.Store.EditAccount(acc.Provider, acc.ID, edit); err != nil {
		writeError500(w, err)
		return
	}
	if hasProfile {
		if _, err := h.Store.AssignProxyProfile(acc.ID, profileID); err != nil {
			writeError500(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// parseDisabledModels 校验停用模型列表：数组、不超过 64 项、每项为 ≤100 字符的字符串。
func parseDisabledModels(raw any) ([]string, *apiError) {
	list, ok := raw.([]any)
	if !ok {
		return nil, errBadRequest("停用模型必須是陣列")
	}
	if len(list) > 64 {
		return nil, errBadRequest("停用模型數量過多")
	}
	models := make([]string, 0, len(list))
	for _, item := range list {
		s, isStr := item.(string)
		if !isStr || len(strings.TrimSpace(s)) > 100 {
			return nil, errBadRequest("停用模型格式無效")
		}
		models = append(models, s)
	}
	return models, nil
}

func (h *Handler) handleSetEnabled(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	enabled := true
	if v, ok := payload["enabled"]; ok {
		enabled = truthy(v) // 对齐 bool(payload.get("enabled", True))：null 视为假
	}
	if ok, err := h.Store.SetEnabled(acc.Provider, acc.ID, enabled); err != nil {
		writeError500(w, err)
		return
	} else if !ok {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSetArchived(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	archived := true
	if v, ok := payload["archived"]; ok {
		archived = truthy(v)
	}
	if ok, err := h.Store.SetArchived(acc.Provider, acc.ID, archived); err != nil {
		writeError500(w, err)
		return
	} else if !ok {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	// 对齐 refresh：payload.all → 全部 zai jwt 账号；否则按 ids 过滤（仅 jwt）。
	// 请求体可空（FastAPI Body(default=None) 语义）。
	payload := map[string]any{}
	if raw, err := io.ReadAll(r.Body); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			writeAPIError(w, errBadRequest("请求体不是合法 JSON"))
			return
		}
	}
	var targets []*model.Account
	if truthy(payload["all"]) {
		for _, a := range h.Store.ListAccounts(model.ProviderZai) {
			if a.Mode == "jwt" {
				targets = append(targets, a)
			}
		}
	} else {
		ids := map[string]bool{}
		if raw, ok := payload["ids"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					ids[s] = true
				}
			}
		}
		for _, a := range h.Store.ListAccounts("") {
			if ids[a.ID] && a.Mode == "jwt" {
				targets = append(targets, a)
			}
		}
	}
	summary := h.Quota.RefreshAccounts(targets)
	writeJSON(w, http.StatusOK, map[string]any{"summary": summary, "count": len(targets)})
}

func (h *Handler) handleRefreshAccount(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	if acc.Mode != "jwt" {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"message": "仅 Coding Plan (JWT) 账号支持额度查询",
		})
		return
	}
	res := h.Quota.FetchQuota(acc)
	updated := h.Store.FindAny(r.PathValue("account_id"))
	if updated == nil {
		updated = acc
	}
	_, hasErr := res["error"]
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      !hasErr,
		"result":  res,
		"account": updated.PublicView(time.Now()),
	})
}

func (h *Handler) handleResetStats(w http.ResponseWriter, r *http.Request) {
	acc := h.Store.FindAny(r.PathValue("account_id"))
	if acc == nil {
		writeAPIError(w, errNotFound("账号不存在"))
		return
	}
	if _, err := h.Store.ResetTokenStats(acc.Provider, acc.ID); err != nil {
		writeError500(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleCaptchaConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.Captcha.FetchConfig(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": cfg.Enabled,
		"prefix":  cfg.Prefix,
		"region":  cfg.Region,
		"sceneId": cfg.SceneID,
	})
}

func (h *Handler) handleCaptchaSubmit(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	param := strings.TrimSpace(strOf(firstTruthy(payload["verify_param"])))
	if param == "" {
		writeAPIError(w, errBadRequest("verify_param 不能为空"))
		return
	}
	cfg := h.Captcha.FetchConfig(r.Context())
	if err := h.Captcha.SetManualParam(param, cfg.Region); err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	captchaSec, retrySec, previewSec := h.Store.ClaimCooldowns()
	writeJSON(w, http.StatusOK, map[string]any{
		"admin_key":                  h.Store.AdminKey(),
		"gateway_key":                h.Store.GatewayKey(),
		"quota_refresh_interval":     h.Store.QuotaRefreshInterval(),
		"claim_auto_enabled":         h.Store.ClaimAutoEnabled(),
		"claim_schedule_enabled":     h.Store.ClaimScheduleEnabled(),
		"claim_schedule_time":        h.Store.ClaimScheduleTime(),
		"claim_captcha_cooldown":     captchaSec,
		"claim_retry_cooldown":       retrySec,
		"claim_preview_cooldown":     previewSec,
		"proxy_health_enabled":       h.Store.ProxyHealthEnabled(),
		"proxy_health_interval":      h.Store.ProxyHealthIntervalMinutes(),
		"risk_cooling_steps":         h.Store.RiskCoolingStepsString(),
		"upstream_503_cooling_steps": h.Store.Upstream503CoolingStepsString(),
	})
}

func (h *Handler) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	if v, ok := payload["admin_key"]; ok {
		key := strings.TrimSpace(strOf(v))
		if key == "" {
			writeAPIError(w, errBadRequest("后台密钥不能为空"))
			return
		}
		if err := h.Store.SetSetting("admin_key", key); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["gateway_key"]; ok {
		key := strings.TrimSpace(strOf(v))
		if key == "" {
			// 网关密钥必填：存空值会让 verify_gateway_key 拒绝所有请求，而非回到免鉴权
			writeAPIError(w, errBadRequest("网关 API Key 不能为空"))
			return
		}
		if err := h.Store.SetSetting("gateway_key", key); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["quota_refresh_interval"]; ok {
		interval, valid := pyInt(v)
		if !valid {
			writeAPIError(w, errBadRequest("刷新间隔必须是非负整数"))
			return
		}
		interval = max(0, interval)
		if err := h.Store.SetSetting("quota_refresh_interval", strconv.Itoa(interval)); err != nil {
			writeError500(w, err)
			return
		}
	}
	// ── 套餐领取 ── 全部即时生效：冷却与定时调度每轮都重新读设置。
	if v, ok := payload["claim_auto_enabled"]; ok {
		b, valid := pyBool(v)
		if !valid {
			writeAPIError(w, errBadRequest("入池自动领取开关需为布尔值"))
			return
		}
		if err := h.Store.SetSetting("claim_auto_enabled", boolText(b)); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["claim_schedule_enabled"]; ok {
		b, valid := pyBool(v)
		if !valid {
			writeAPIError(w, errBadRequest("每日定时领取开关需为布尔值"))
			return
		}
		if err := h.Store.SetSetting("claim_schedule_enabled", boolText(b)); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["claim_schedule_time"]; ok {
		t := strings.TrimSpace(strOf(v))
		if !store.ValidClaimScheduleTime(t) {
			writeAPIError(w, errBadRequest("定时时间需为 HH:MM（如 23:00）"))
			return
		}
		if err := h.Store.SetSetting("claim_schedule_time", t); err != nil {
			writeError500(w, err)
			return
		}
	}
	// 三个冷却共用同一套解析；负数按既有 quota_refresh_interval 的语义钳到下限。
	cooldownKeys := []struct {
		key string
		min int
	}{
		{"claim_captcha_cooldown", 60},
		{"claim_retry_cooldown", 30},
		{"claim_preview_cooldown", 0},
	}
	for _, item := range cooldownKeys {
		v, ok := payload[item.key]
		if !ok {
			continue
		}
		n, valid := pyInt(v)
		if !valid {
			writeAPIError(w, errBadRequest(item.key+" 必须是非负整数（秒）"))
			return
		}
		if err := h.Store.SetSetting(item.key, strconv.Itoa(max(item.min, n))); err != nil {
			writeError500(w, err)
			return
		}
	}
	// ── 線路自動巡檢 ── 同樣即時生效：調度器每輪重新讀設置。
	if v, ok := payload["proxy_health_enabled"]; ok {
		b, valid := pyBool(v)
		if !valid {
			writeAPIError(w, errBadRequest("线路自动巡检开关需为布尔值"))
			return
		}
		if err := h.Store.SetSetting("proxy_health_enabled", boolText(b)); err != nil {
			writeError500(w, err)
			return
		}
	}
	if v, ok := payload["proxy_health_interval"]; ok {
		n, valid := pyInt(v)
		if !valid || n < 1 {
			writeAPIError(w, errBadRequest("巡检间隔必须是 ≥1 的整数（分钟）"))
			return
		}
		if err := h.Store.SetSetting("proxy_health_interval", strconv.Itoa(n)); err != nil {
			writeError500(w, err)
			return
		}
	}
	// ── 風控冷卻階梯 ── 逗號分隔的秒數；檔位數同時是升級點（連續命中超過檔位數
	// 就把帳號置為失效），所以非法值一律 400 拒收，不做「跳過壞項」的寬容解析：
	// 靜默少一檔會讓管理員拿到一個他不知道有幾檔的階梯。
	if v, ok := payload[store.RiskCoolingStepsKey]; ok {
		raw, valid := store.NormalizeRiskCoolingSteps(strOf(v))
		if !valid {
			writeAPIError(w, errBadRequest("風控冷卻階梯需為逗號分隔的正整數秒，如 300,900,3600"))
			return
		}
		if err := h.Store.SetSetting(store.RiskCoolingStepsKey, raw); err != nil {
			writeError500(w, err)
			return
		}
	}
	// ── 上游 503 冷卻階梯 ── 同樣嚴格校驗（整串都是正整數秒）。與風控的差異：
	// 連續次數超過檔位數只封頂冷卻、不升級為失效——503 是上游健康信號而非帳號問題。
	if v, ok := payload[store.Upstream503CoolingStepsKey]; ok {
		raw, valid := store.NormalizeUpstream503CoolingSteps(strOf(v))
		if !valid {
			writeAPIError(w, errBadRequest("上游 503 冷卻階梯需為逗號分隔的正整數秒，如 30,60,120"))
			return
		}
		if err := h.Store.SetSetting(store.Upstream503CoolingStepsKey, raw); err != nil {
			writeError500(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// pyBool 对应 Python bool() 的宽松转换：bool 原样、数字 0/1、常见布尔字符串；
// 其余类型视为 TypeError（调用方回 400）。
func pyBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case float64:
		switch t {
		case 0:
			return false, true
		case 1:
			return true, true
		}
		return false, false
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off":
			return false, true
		}
		return false, false
	}
	return false, false
}

// boolText 布尔值的规范存储形态（store.claimBool 可解析）。
func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// pyInt 对应 Python int() 的宽松转换：float 截断、数字字符串解析、bool 转换；
// 其余类型（null/数组/对象）视为 TypeError。
func pyInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	}
	return 0, false
}

// handleExport 导出含明文凭证，仅用于备份/迁移（对齐 Python 版）。
func (h *Handler) handleExport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.Store.Export())
}

func (h *Handler) handleImport(w http.ResponseWriter, r *http.Request) {
	var payload store.ImportPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeAPIError(w, errBadRequest("请求体不是合法 JSON"))
		return
	}
	count, newIDs, err := h.Store.ImportAccounts(payload)
	if err != nil {
		writeError500(w, err)
		return
	}
	// 导入的账号同样自动分配未占用线路（只针对本次新建的，重复导入不会动老号）；
	// 线路不足的部分保持直连并通过 direct_fallback 回报，供前端提示。
	assigned, fallback := h.Store.AutoAssignProxies(newIDs)
	writeJSON(w, http.StatusOK, map[string]any{
		"count":    count,
		"assigned": len(assigned), "direct_fallback": len(fallback),
	})
}
