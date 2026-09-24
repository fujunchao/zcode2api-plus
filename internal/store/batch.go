// Package store 账号批量操作：批量新增 / 批量删除 / 批量启用禁用 / 条件查询。
// 设计规格见 docs/plan-account-batch-management.md，要点：
//
//   - 事务一致性：单锁 + 单 SQLite 事务，**commit 成功后才应用内存变更**；
//     任一持久化失败整批回滚、内存零改动。commit 与内存应用之间进程崩溃也一致
//     ——内存态启动时从 DB 载入，DB 是真相源。
//   - 逐条明细：每条操作以 ok/duplicate/not_found/error 状态随响应回报，
//     不再出现「批量里某几条成功但调用方不知道是哪几条」。
//   - 兼容既有约定：判重复用 duplicateLocked 三轮语义；启用/禁用的字段转移与
//     单账号 SetEnabled 共用 applyEnabled；落库 SQL 与 persistAccountLocked
//     逐字一致（事务版镜像）；不新增 Account 字段，不触碰 34 键 JSON 契约。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// MaxBatchSize 单次批量操作的条目上限。
// adminapi 层校验先行（超限 400），store 层再设一道防线；上限同时为批量事务的
// 持锁时长定界——事务期间其它账号操作都在排队，量级与既有 PurgeProxyProfiles
// 批量改派相当。
const MaxBatchSize = 500

// MaxSecretLength 批量新增单条凭据的长度上限（防误粘整个文件）。
const MaxSecretLength = 8192

// 批量操作逐条明细的状态取值。
const (
	BatchStatusOK        = "ok"
	BatchStatusDuplicate = "duplicate"
	BatchStatusNotFound  = "not_found"
	BatchStatusError     = "error"
)

// proxyFilterDirect 条件查询 proxy_id 的保留值：筛「未指派任何线路」的账号
// （含手工代理与直连——对调度归因而言它们都不走 profile 线路）。
const proxyFilterDirect = "__direct__"

// ErrBatchMissingIDs 严格模式下批量目标中存在不存在的账号（此时零改动）。
var ErrBatchMissingIDs = errors.New("批量目标中存在不存在的账号")

// ErrBatchValidation 预校验未通过（任何写入发生之前，零副作用）。
// handler 层据此把响应映射为 400 并附上逐条明细。
var ErrBatchValidation = errors.New("批量条目未通过校验")

// BatchItemResult 批量操作的单条明细。
type BatchItemResult struct {
	Index   int    `json:"index"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// BatchResult 批量操作的汇总结果与逐条明细。
type BatchResult struct {
	Total      int               `json:"total"`
	Succeeded  int               `json:"succeeded"`
	Duplicated int               `json:"duplicated"`
	NotFound   int               `json:"not_found"`
	Failed     int               `json:"failed"`
	IDs        []string          `json:"ids,omitempty"` // 新建（新增）或命中（删除）的账号 ID
	Items      []BatchItemResult `json:"items"`
}

// BatchAddItem 批量新增的单条目。
type BatchAddItem struct {
	Name   string // 可空：调用方负责兜底命名（与现有 handleAddAccounts 同规则）
	Secret string // 必填；进入 store 前已 trim
	Email  string // 可选
}

// ── 事务版持久化镜像 ────────────────────────────────────────────────────────
//
// SQL 与 persistAccountLocked / deleteAccountLocked 逐字一致。批量事务期间只能
// 经 tx 写库：SetMaxOpenConns(1)，此时唯一连接被事务占用，再走 s.db 会排队等
// 自己提交，形成自占死锁。

func persistAccountOnTx(tx *sql.Tx, acc *model.Account) error {
	data, err := marshalJSON(acc)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		fmt.Sprintf(`INSERT OR REPLACE INTO %s
			(id, provider, name, mode, status, enabled, created_at, data)
			VALUES (?,?,?,?,?,?,?,?)`, accountsTable),
		acc.ID, acc.Provider, acc.Name, acc.Mode, acc.Status, boolToInt(acc.Enabled), acc.CreatedAt, string(data))
	return err
}

func deleteAccountOnTx(tx *sql.Tx, id string) error {
	_, err := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", accountsTable), id)
	return err
}

// dedupeIDs 账号 ID 去重保序（对齐 parseTokens 的去重保序语义），空串剔除。
func dedupeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// markBatchRollback 把明细里所有 ok 项改标为 error（整批回滚的统一收尾）。
// 新增场景同时清掉 ID 与计数——账号实际不存在，不能让调用方拿着假 ID 去指派线路。
func markBatchRollback(res *BatchResult, cause error) {
	for i := range res.Items {
		if res.Items[i].Status == BatchStatusOK {
			res.Items[i].Status = BatchStatusError
			res.Items[i].ID = ""
			res.Items[i].Message = "持久化失败，整批已回滚: " + cause.Error()
		}
	}
	res.Succeeded = 0
	res.IDs = nil
	res.Failed = len(res.Items) - res.Duplicated - res.NotFound
}

// ── 批量新增 ────────────────────────────────────────────────────────────────

// BatchAddAccounts 批量新增账号。
//
// 语义：
//   - 预校验（凭据非空、长度上限）在任何写入前完成；存在非法项时整体报错、
//     逐条标注、零副作用；
//   - 判重复用 duplicateLocked 三轮语义（user_id → email → 凭据），命中既有
//     记录记 duplicate、不算失败——与 AddAccountWithIdentity 的幂等行为一致；
//   - 全部新账号走**单个** SQLite 事务插入，任一条失败整批回滚且内存零改动。
//
// 返回的 IDs 只含本次真正新建的账号（重复项不进 IDs），可直接交给
// AutoAssignProxies / 额度刷新链路使用。
func (s *Store) BatchAddAccounts(provider string, items []BatchAddItem) (BatchResult, error) {
	res := BatchResult{Total: len(items)}
	if len(items) == 0 {
		return res, errors.New("批量新增列表为空")
	}
	if len(items) > MaxBatchSize {
		return res, fmt.Errorf("批量新增条目数超过上限 %d", MaxBatchSize)
	}
	if !s.providersSet()[provider] {
		return res, fmt.Errorf("不支持的 provider: %s", provider)
	}

	// 预校验：全部坏项一次报出（调用方 400 + 明细），而不是报一个错让用户改一轮。
	bad := 0
	for i, item := range items {
		secret := strings.TrimSpace(item.Secret)
		switch {
		case secret == "":
			bad++
			res.Items = append(res.Items, BatchItemResult{
				Index: i, Name: item.Name, Status: BatchStatusError, Message: "凭据（token / API Key）不能为空",
			})
		case len(secret) > MaxSecretLength:
			bad++
			res.Items = append(res.Items, BatchItemResult{
				Index: i, Name: item.Name, Status: BatchStatusError,
				Message: fmt.Sprintf("凭据长度超过上限 %d", MaxSecretLength),
			})
		}
	}
	if bad > 0 {
		res.Failed = bad
		return res, fmt.Errorf("%w：%d 条，尚未写入任何数据", ErrBatchValidation, bad)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var fresh []*model.Account
	for i, item := range items {
		// 构造逻辑与 AddAccountWithIdentity 逐字一致：jwt 派生 user_id、email trim。
		acc := model.Create(provider, item.Name, item.Secret)
		if acc.Mode == "jwt" {
			if uid := model.JWTUserID(acc.Secret()); uid != "" {
				acc.UserID = &uid
			}
		}
		if trimmed := strings.TrimSpace(item.Email); trimmed != "" {
			acc.Email = &trimmed
		}
		if existing := s.duplicateLocked(provider, acc); existing != nil {
			res.Duplicated++
			res.Items = append(res.Items, BatchItemResult{
				Index: i, ID: existing.ID, Name: existing.Name,
				Status: BatchStatusDuplicate, Message: "同一账号已存在（user_id / email / 凭据判重），返回既有记录",
			})
			continue
		}
		// 每账号独立设备指纹（与单账号入池同一规则）。
		mid := config.NewDeviceMid()
		acc.VirtualDeviceMid = &mid
		fresh = append(fresh, acc)
		res.Succeeded++
		res.IDs = append(res.IDs, acc.ID)
		res.Items = append(res.Items, BatchItemResult{Index: i, ID: acc.ID, Name: acc.Name, Status: BatchStatusOK})
	}

	if len(fresh) == 0 {
		return res, nil // 全部重复：无需写库，幂等达成
	}

	// 单事务插入；commit 成功才追加进内存（先落库再改内存的批量形态）。
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	for _, acc := range fresh {
		if err := persistAccountOnTx(tx, acc); err != nil {
			logPersistFailure("batch-add", provider, err)
			markBatchRollback(&res, err)
			_ = tx.Rollback()
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		logPersistFailure("batch-add", provider, err)
		markBatchRollback(&res, err)
		return res, err
	}
	s.accounts[provider] = append(s.accounts[provider], fresh...)
	return res, nil
}

// ── 批量删除 ────────────────────────────────────────────────────────────────

// BatchRemoveAccounts 批量删除账号。ID 按 findAnyLocked 跨 provider 解析，
// 与现有删除端点（FindAny → RemoveAccount）的解析方式一致。
//
// missingOK=false（严格）：任一 ID 不存在 → 返回 ErrBatchMissingIDs + 逐条
// not_found 明细，一个都不删；missingOK=true：跳过不存在项并逐条标注
// （对齐旧 DELETE 端点的宽松语义）。
func (s *Store) BatchRemoveAccounts(ids []string, missingOK bool) (BatchResult, error) {
	ids = dedupeIDs(ids)
	res := BatchResult{Total: len(ids)}
	if len(ids) == 0 {
		return res, errors.New("批量删除列表为空")
	}
	if len(ids) > MaxBatchSize {
		return res, fmt.Errorf("批量删除条目数超过上限 %d", MaxBatchSize)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var targets []*model.Account
	for i, id := range ids {
		acc := s.findAnyLocked(id)
		if acc == nil {
			res.NotFound++
			res.Items = append(res.Items, BatchItemResult{
				Index: i, ID: id, Status: BatchStatusNotFound, Message: "账号不存在",
			})
			continue
		}
		targets = append(targets, acc)
		res.Succeeded++
		res.IDs = append(res.IDs, acc.ID)
		res.Items = append(res.Items, BatchItemResult{Index: i, ID: acc.ID, Name: acc.Name, Status: BatchStatusOK})
	}

	if !missingOK && res.NotFound > 0 {
		// 严格模式：存在缺失即整体拒绝，零改动。
		return res, ErrBatchMissingIDs
	}
	if len(targets) == 0 {
		return res, nil // 全部缺失（宽松模式）：无需写库
	}

	// 单事务删除；commit 成功才改内存。删除常用于撤销外泄凭证，宁可整批失败
	// 也不能出现「部分删除 + 调用方以为全删了」。
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	for _, acc := range targets {
		if err := deleteAccountOnTx(tx, acc.ID); err != nil {
			logPersistFailure("batch-delete", acc.Provider, err)
			markBatchRollback(&res, err)
			_ = tx.Rollback()
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		logPersistFailure("batch-delete", "batch", err)
		markBatchRollback(&res, err)
		return res, err
	}
	removed := make(map[string]bool, len(targets))
	for _, acc := range targets {
		removed[acc.ID] = true
	}
	for p, list := range s.accounts {
		remaining := list[:0:0]
		for _, a := range list {
			if !removed[a.ID] {
				remaining = append(remaining, a)
			}
		}
		s.accounts[p] = remaining
	}
	return res, nil
}

// ── 批量启用 / 禁用 ────────────────────────────────────────────────────────

// BatchSetEnabled 批量启用/禁用账号。字段转移与单账号 SetEnabled 共用
// applyEnabled，保证两个入口语义永不漂移；不触碰归档（ArchivedAt）与
// invalid/cooling/exhausted 的状态机归属——归档 ≠ 停用 ≠ invalid。
//
// 事务里落的是「转移后」的克隆快照，live 对象在 commit 成功后才就地应用
// applyEnabled——失败回滚时内存零改动。
func (s *Store) BatchSetEnabled(ids []string, enabled, missingOK bool) (BatchResult, error) {
	ids = dedupeIDs(ids)
	res := BatchResult{Total: len(ids)}
	if len(ids) == 0 {
		return res, errors.New("批量启用/禁用列表为空")
	}
	if len(ids) > MaxBatchSize {
		return res, fmt.Errorf("批量启用/禁用条目数超过上限 %d", MaxBatchSize)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var targets []*model.Account
	for i, id := range ids {
		acc := s.findAnyLocked(id)
		if acc == nil {
			res.NotFound++
			res.Items = append(res.Items, BatchItemResult{
				Index: i, ID: id, Status: BatchStatusNotFound, Message: "账号不存在",
			})
			continue
		}
		targets = append(targets, acc)
		res.Succeeded++
		res.IDs = append(res.IDs, acc.ID)
		res.Items = append(res.Items, BatchItemResult{Index: i, ID: acc.ID, Name: acc.Name, Status: BatchStatusOK})
	}

	if !missingOK && res.NotFound > 0 {
		return res, ErrBatchMissingIDs
	}
	if len(targets) == 0 {
		return res, nil
	}

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	for _, acc := range targets {
		snapshot := acc.Clone()
		applyEnabled(snapshot, enabled)
		if err := persistAccountOnTx(tx, snapshot); err != nil {
			logPersistFailure("batch-update", acc.Provider, err)
			markBatchRollback(&res, err)
			_ = tx.Rollback()
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		logPersistFailure("batch-update", "batch", err)
		markBatchRollback(&res, err)
		return res, err
	}
	for _, acc := range targets {
		applyEnabled(acc, enabled)
	}
	return res, nil
}

// ── 条件查询 ────────────────────────────────────────────────────────────────

// AccountQuery 账号列表的条件过滤器。零值 = 不过滤不分页，结果与
// ListAccounts("") 一致（顺序同为 created_at 升序）。
type AccountQuery struct {
	Keyword       string   // 子串匹配 name / email / id / user_id（不区分大小写）
	Statuses      []string // 状态机原始 status 精确匹配（空 = 不过滤）
	Enabled       *bool    // nil = 不过滤
	Mode          string   // "jwt" / "apiKey"（空 = 不过滤）
	Model         string   // 模型可用性过滤，与 ModelStatus 搭配
	ModelStatus   string   // available/exhausted/disabled/absent/unknown；空且 Model 非空时默认 available
	ProxyID       string   // 线路 profile ID 精确匹配；__direct__ = 未指派线路的账号
	LastErrorKind string   // 最近失败类别精确匹配
	CreatedAfter  *float64 // CreatedAt >= CreatedAfter
	CreatedBefore *float64 // CreatedAt <= CreatedBefore
	Archived      string   // "all"（默认）/ "false"（仅未归档）/ "true"（仅已归档）
	Sort          string   // created_at_asc（默认）/ created_at_desc / name / use_count_desc
	Limit         int      // ≤0 = 不分页（调用方默认 100、上限 500）
	Offset        int      // <0 按 0 处理
}

// QueryAccounts 条件查询账号：返回（过滤 + 排序 + 分页后的克隆列表, 过滤后总数）。
// 返回的是副本（与 ListAccounts 同一约定），调用方不得长期持有内部指针。
func (s *Store) QueryAccounts(q AccountQuery) ([]*model.Account, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	all := s.allAccountsLocked()

	statusSet := map[string]bool{}
	for _, st := range q.Statuses {
		if st = strings.TrimSpace(st); st != "" {
			statusSet[st] = true
		}
	}
	keyword := strings.ToLower(strings.TrimSpace(q.Keyword))
	modelName := model.NormalizeModelName(q.Model)
	wantAvail := q.ModelStatus
	if wantAvail == "" {
		wantAvail = "available"
	}

	filtered := make([]*model.Account, 0, len(all))
	for _, acc := range all {
		if len(statusSet) > 0 && !statusSet[acc.Status] {
			continue
		}
		if q.Enabled != nil && acc.Enabled != *q.Enabled {
			continue
		}
		if q.Mode != "" && acc.Mode != q.Mode {
			continue
		}
		switch q.Archived {
		case "false":
			if acc.ArchivedAt != nil {
				continue
			}
		case "true":
			if acc.ArchivedAt == nil {
				continue
			}
		}
		if q.ProxyID != "" {
			if q.ProxyID == proxyFilterDirect {
				if acc.ProxyID != nil {
					continue
				}
			} else if acc.ProxyID == nil || *acc.ProxyID != q.ProxyID {
				continue
			}
		}
		if q.LastErrorKind != "" && (acc.LastErrorKind == nil || *acc.LastErrorKind != q.LastErrorKind) {
			continue
		}
		if q.CreatedAfter != nil && acc.CreatedAt < *q.CreatedAfter {
			continue
		}
		if q.CreatedBefore != nil && acc.CreatedAt > *q.CreatedBefore {
			continue
		}
		if modelName != "" && acc.ModelAvailability(modelName) != wantAvail {
			continue
		}
		if keyword != "" && !accountMatchKeyword(acc, keyword) {
			continue
		}
		filtered = append(filtered, acc)
	}
	total := len(filtered)

	sortAccountsBy(filtered, q.Sort)

	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if offset >= total {
			return []*model.Account{}, total
		}
		filtered = filtered[offset:]
	}
	if q.Limit > 0 && len(filtered) > q.Limit {
		filtered = filtered[:q.Limit]
	}

	out := make([]*model.Account, len(filtered))
	for i, acc := range filtered {
		out[i] = acc.Clone()
	}
	return out, total
}

// accountMatchKeyword 关键词命中：name / email / id / user_id 任一小写子串匹配。
func accountMatchKeyword(acc *model.Account, keyword string) bool {
	hay := []string{acc.ID, acc.Name, derefStr(acc.Email), derefStr(acc.UserID)}
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), keyword) {
			return true
		}
	}
	return false
}

// sortAccountsBy 排序；未知取值回落默认 created_at 升序（与 DB 载入顺序一致）。
func sortAccountsBy(list []*model.Account, sortKey string) {
	switch sortKey {
	case "created_at_desc":
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt > list[j].CreatedAt })
	case "name":
		sort.SliceStable(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	case "use_count_desc":
		sort.SliceStable(list, func(i, j int) bool { return list[i].UseCount > list[j].UseCount })
	default: // created_at_asc
		sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	}
}
