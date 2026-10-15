package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// initializationEnrollmentReason 只决定能否人工接管，不据本地空快照断言上游有领取资格。
func initializationEnrollmentReason(a *model.Account, now time.Time) string {
	if !initializationHealthy(a, now) || a.JWTToken == nil || *a.JWTToken == "" {
		return "仅支持启用、未归档且不处于失效或冷却状态的 JWT 账号"
	}
	nowSeconds := float64(now.UnixNano()) / 1e9
	if (a.CoolingUntil != nil && *a.CoolingUntil > nowSeconds) || a.InRiskControlSilentWindow(now, config.RiskSilentWindow()) {
		return "账号仍在本地冷却或隔离期，不能通过补入初始化绕过"
	}
	if a.UseCount != 0 || a.LastUsedAt != nil || a.TotalInputTokens != 0 || a.TotalOutputTokens != 0 || a.TotalCacheCreationTokens != 0 || a.TotalCacheReadTokens != 0 || len(a.Usage) > 0 {
		return "账号已有使用记录，不作为漏处理新号补入"
	}
	if len(a.Plans) > 0 || len(a.Plan) > 0 {
		return "账号已有套餐记录，不作为未取额新号补入"
	}
	if claim := a.ClaimView(); claim != nil {
		if claim.ClaimedAt != nil {
			return "账号已有成功领取记录"
		}
		if claim.NextAt != nil && *claim.NextAt > nowSeconds {
			return "账号仍在领取等待期，不能清零后重试"
		}
	}
	for _, q := range a.Quota {
		for _, key := range []string{"total", "used", "remaining", "available"} {
			if raw, exists := q[key]; exists && raw != nil {
				value, valid := asNumber(raw)
				if !valid || value != 0 {
					return "账号存在非零或未知配额记录，请先核查额度"
				}
			}
		}
	}
	return ""
}

// BatchEnrollInitializations 只处理管理员明确选择并确认接管的集合，绝不扫描其他账号。
// 合格项在单事务内登记，已有任务幂等返回，不重置暂停、预算或到账事实。
func (s *Store) BatchEnrollInitializations(ids []string, confirmed bool) (BatchResult, error) {
	res := BatchResult{Total: len(ids)}
	if !confirmed {
		return res, fmt.Errorf("%w：请明确确认接管所选账号的代理路由", ErrBatchValidation)
	}
	if len(ids) == 0 || len(ids) > MaxBatchSize {
		return res, fmt.Errorf("%w：请选择 1–%d 个账号", ErrBatchValidation, MaxBatchSize)
	}
	ids = dedupeIDs(ids)
	res.Total = len(ids)
	if len(ids) == 0 {
		return res, fmt.Errorf("%w：账号 ID 不能为空", ErrBatchValidation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	policy, err := s.initializationPolicyLocked()
	if err != nil {
		return res, err
	}
	if !policy.Enabled {
		return res, fmt.Errorf("%w：请先在代理页面启用新账号初始化策略", ErrBatchValidation)
	}
	next := s.cloneStrictProxyLocked()
	var pending []*model.Account
	tasks := map[string]*InitializationTask{}
	seen := map[string]bool{}
	now := time.Now()
	for i, id := range ids {
		a := s.findAnyLocked(id)
		item := BatchItemResult{Index: i, ID: id}
		if a == nil {
			item.Status = BatchStatusNotFound
			item.Message = "账号不存在"
			res.NotFound++
			res.Items = append(res.Items, item)
			continue
		}
		item.ID, item.Name = a.ID, a.Name
		if seen[a.ID] || s.initializations[a.ID] != nil {
			item.Status = BatchStatusDuplicate
			item.Message = "已有初始化记录，未重置任务"
			res.Duplicated++
			res.Items = append(res.Items, item)
			continue
		}
		seen[a.ID] = true
		if reason := initializationEnrollmentReason(a, now); reason != "" {
			item.Status = BatchStatusError
			item.Message = reason
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		copy := a.Clone()
		task, err := s.newInitializationLocked(copy, []AccountCreationOptions{{InitializeProxy: true, Source: "manual_import_recovery"}})
		if err != nil {
			markBatchRollback(&res, err)
			return res, err
		}
		// 接管前已经在途的旧请求不得回写新任务身份；不修改凭据字节或健康状态。
		trackStrictAccountVersions(&next, copy)
		credential := next.Credentials[copy.ID]
		credential.Generation++
		next.Credentials[copy.ID] = credential
		copy.CredentialGeneration = credential.Generation
		task.CredentialGeneration = credential.Generation
		setStrictIntent(&next, copy, initializationIntent)
		pending = append(pending, copy)
		tasks[copy.ID] = task
		res.Succeeded++
		res.IDs = append(res.IDs, copy.ID)
		item.Status = BatchStatusOK
		res.Items = append(res.Items, item)
	}
	if len(pending) == 0 {
		return res, nil
	}
	raw, err := json.Marshal(next)
	if err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	defer tx.Rollback()
	for _, a := range pending {
		if err = persistAccountOnTx(tx, a); err == nil {
			err = persistInitializationOnTx(tx, tasks[a.ID])
		}
		if err != nil {
			markBatchRollback(&res, err)
			return res, err
		}
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", strictProxyStateKey, string(raw)); err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	if err = tx.Commit(); err != nil {
		markBatchRollback(&res, err)
		return res, err
	}
	s.strictProxy = next
	for id, task := range tasks {
		s.initializations[id] = task
	}
	for _, a := range pending {
		a.Initialization = s.initializationViewLocked(a)
		s.replaceInitializationAccountLocked(a)
	}
	return res, nil
}
