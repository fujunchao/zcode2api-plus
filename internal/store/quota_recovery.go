package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"time"

	"zcode2api/internal/model"
)

const quotaRecoveryKey = "api_key_quota_recovery"

// APIKeyQuotaLongWait 用于欠费、套餐过期、不包含模型等非短时窗口错误。
const APIKeyQuotaLongWait = 6 * time.Hour

var quotaRetryDelays = [...]time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, APIKeyQuotaLongWait}

type quotaRetryState struct {
	CredentialHash string  `json:"credential_hash"`
	Generation     uint64  `json:"generation"`
	Step           int     `json:"step"`
	NextAt         float64 `json:"next_at"`
	MinimumSeconds int64   `json:"minimum_seconds"`
}

type quotaRecovery map[string]map[string]quotaRetryState

type quotaProbeKey struct{ account, model string }

// QuotaProbe 是一次真实模型请求的独占恢复租约。调用方不能自行构造有效租约；
// 无论请求成功、失败或取消，都必须调用 FinishQuotaProbe 释放。
type QuotaProbe struct {
	Account    *model.Account
	expected   *model.Account
	model      string
	generation uint64
}

func quotaCredentialHash(a *model.Account) string {
	sum := sha256.Sum256([]byte(a.Provider + "\x00" + a.Mode + "\x00" + a.Secret()))
	return hex.EncodeToString(sum[:])
}

func quotaSeconds(now time.Time) float64 { return float64(now.UnixNano()) / 1e9 }

func quotaDelay(state quotaRetryState) time.Duration {
	step := max(0, min(state.Step, len(quotaRetryDelays)-1))
	minimum := time.Duration(max(0, min(state.MinimumSeconds, int64(APIKeyQuotaLongWait/time.Second)))) * time.Second
	return max(quotaRetryDelays[step], minimum)
}

// cloneQuotaRecoveryLocked 同时丢弃已删除账号、已切换模式或已经清除的模型条目。
func (s *Store) cloneQuotaRecoveryLocked() quotaRecovery {
	next := quotaRecovery{}
	for _, a := range s.allAccountsLocked() {
		if a.Mode != "apiKey" {
			continue
		}
		for _, name := range a.ExhaustedModels {
			name = model.NormalizeModelName(name)
			if state, ok := s.quotaRecovery[a.ID][name]; ok {
				if next[a.ID] == nil {
					next[a.ID] = map[string]quotaRetryState{}
				}
				next[a.ID][name] = state
			}
		}
	}
	return next
}

// commitQuotaRecoveryLocked 先提交账号与重试元数据，再发布内存快照。
// 失败时既不发放租约，也不乐观解除模型标记。
func (s *Store) commitQuotaRecoveryLocked(next quotaRecovery, pending *model.Account) error {
	raw, err := marshalJSON(next)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if pending != nil {
		if err := persistAccountOnTx(tx, pending); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", quotaRecoveryKey, string(raw)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.quotaRecovery = next
	if pending != nil {
		live := s.findLocked(pending.Provider, pending.ID)
		live.ExhaustedModels, live.Quota = pending.ExhaustedModels, pending.Quota
		live.Status, live.CoolingUntil = pending.Status, pending.CoolingUntil
		live.LastError, live.LastErrorKind, live.LastErrorAt = pending.LastError, pending.LastErrorKind, pending.LastErrorAt
	}
	return nil
}

// MarkQuotaExhausted 原子记录模型耗尽和 API Key 恢复等待。
// JWT 保持原有余额刷新语义；迟到的旧凭据失败不应污染编辑后的账号。
func (s *Store) MarkQuotaExhausted(expected *model.Account, name, detail string, now time.Time, minimumWait time.Duration) error {
	if expected == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(expected.Provider, expected.ID)
	if live == nil || live.Mode != expected.Mode || live.Secret() != expected.Secret() {
		return nil
	}
	pending := live.Clone()
	name = model.NormalizeModelName(name)
	marked := pending.MarkModelExhausted(name)
	strong := pending.Status == model.StatusInvalid || pending.Status == model.StatusCooling || pending.Status == model.StatusDisabled
	if !strong {
		pending.Status = model.StatusExhausted
		if marked {
			all := len(pending.Quota) > 0
			for entryName, quota := range pending.Quota {
				if value, _ := quota["model"].(string); value != "" {
					entryName = value
				}
				if pending.ModelAvailability(entryName) != "exhausted" {
					all = false
					break
				}
			}
			if !all {
				pending.Status = model.StatusActive
			}
			pending.CoolingUntil = nil
		}
	}
	kind, ts := model.ErrorKindQuotaExhausted, quotaSeconds(now)
	pending.LastErrorKind, pending.LastErrorAt = &kind, &ts
	if detail != "" {
		pending.LastError = &detail
	}
	next := s.cloneQuotaRecoveryLocked()
	if marked && pending.Mode == "apiKey" {
		if next[pending.ID] == nil {
			next[pending.ID] = map[string]quotaRetryState{}
		}
		state := next[pending.ID][name]
		hash := quotaCredentialHash(pending)
		if state.CredentialHash != hash {
			state = quotaRetryState{Generation: state.Generation}
		}
		state.CredentialHash = hash
		// 切换模式会清理旧元数据，但旧请求仍可能在途；新代次不得撞回旧租约。
		if inFlight := s.quotaProbes[quotaProbeKey{pending.ID, name}]; inFlight != nil {
			state.Generation = max(state.Generation, inFlight.generation)
		}
		state.Generation++
		state.MinimumSeconds = int64(max(0, min(minimumWait, APIKeyQuotaLongWait)) / time.Second)
		state.NextAt = max(state.NextAt, quotaSeconds(now.Add(quotaDelay(state))))
		next[pending.ID][name] = state
	}
	return s.commitQuotaRecoveryLocked(next, pending)
}

func quotaProbeEligible(a *model.Account, name string, now time.Time) bool {
	if a.Mode != "apiKey" || a.Secret() == "" || a.ArchivedAt != nil || !a.Enabled ||
		a.Status == model.StatusInvalid || a.Status == model.StatusDisabled {
		return false
	}
	// 冷却证据独立于 Status，避免其他成功将状态改为 active 后误绕过未到期窗口。
	if a.CoolingUntil != nil && *a.CoolingUntil > quotaSeconds(now) {
		return false
	}
	if a.Status == model.StatusCooling && a.CoolingUntil == nil {
		return false
	}
	if a.ModelAvailability(name) != "exhausted" {
		return false
	}
	for _, item := range a.ExhaustedModels {
		if model.NormalizeModelName(item) == name {
			return true
		}
	}
	return false // 不能把 absent 或单纯旧余额为零都当成已收到的模型耗尽信号。
}

// AcquireQuotaProbe 仅供普通网关在正常候选用尽后调用；不修改 Select 的默认契约。
// 到期只允许借真实请求探测，并不提前宣布余额恢复。
func (s *Store) AcquireQuotaProbe(provider string, skip map[string]bool, name string, now time.Time) (*QuotaProbe, error) {
	name = model.NormalizeModelName(name)
	if name == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quotaProbes == nil {
		s.quotaProbes = map[quotaProbeKey]*QuotaProbe{}
	}
	for _, a := range s.accounts[provider] {
		key := quotaProbeKey{a.ID, name}
		if skip[a.ID] || s.quotaProbes[key] != nil || !quotaProbeEligible(a, name, now) {
			continue
		}
		state, exists := s.quotaRecovery[a.ID][name]
		hash := quotaCredentialHash(a)
		if !exists || state.CredentialHash != hash {
			base := now
			if !exists && a.LastErrorKind != nil && *a.LastErrorKind == model.ErrorKindQuotaExhausted &&
				a.LastErrorAt != nil && *a.LastErrorAt > 0 && *a.LastErrorAt <= quotaSeconds(now) &&
				!math.IsInf(*a.LastErrorAt, 0) && !math.IsNaN(*a.LastErrorAt) {
				base = time.Unix(0, int64(*a.LastErrorAt*1e9))
			}
			state = quotaRetryState{CredentialHash: hash, Generation: state.Generation + 1,
				NextAt: quotaSeconds(base.Add(quotaRetryDelays[0]))}
			next := s.cloneQuotaRecoveryLocked()
			if next[a.ID] == nil {
				next[a.ID] = map[string]quotaRetryState{}
			}
			next[a.ID][name] = state
			if err := s.commitQuotaRecoveryLocked(next, nil); err != nil {
				return nil, err
			}
		}
		if quotaSeconds(now) < state.NextAt {
			continue
		}
		state.Step = min(state.Step+1, len(quotaRetryDelays)-1)
		state.NextAt = quotaSeconds(now.Add(quotaDelay(state)))
		next := s.cloneQuotaRecoveryLocked()
		next[a.ID][name] = state
		if err := s.commitQuotaRecoveryLocked(next, nil); err != nil {
			return nil, err
		}
		probe := &QuotaProbe{Account: a.Clone(), expected: a.Clone(), model: name, generation: state.Generation}
		s.quotaProbes[key] = probe
		return probe, nil
	}
	return nil, nil
}

// FinishQuotaProbe 失败/取消只释放租约，保留已落盘退避。完整成功也必须仍匹配
// 失败代次、凭据、出站身份和账号保护条件，才可以原子恢复目标模型。
func (s *Store) FinishQuotaProbe(probe *QuotaProbe, confirmed bool, now time.Time) (bool, error) {
	if probe == nil || probe.expected == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expected := probe.expected
	key := quotaProbeKey{expected.ID, probe.model}
	if s.quotaProbes[key] != probe {
		return false, nil
	}
	delete(s.quotaProbes, key)
	live := s.findLocked(expected.Provider, expected.ID)
	state := s.quotaRecovery[expected.ID][probe.model]
	if !confirmed || live == nil || state.Generation != probe.generation ||
		quotaCredentialHash(live) != quotaCredentialHash(expected) ||
		derefStr(live.ProxyID) != derefStr(expected.ProxyID) || derefStr(live.ProxyURL) != derefStr(expected.ProxyURL) ||
		live.DeviceMidOr("") != expected.DeviceMidOr("") || !quotaProbeEligible(live, probe.model, now) {
		return false, nil
	}
	pending := live.Clone()
	pending.ExhaustedModels = []string{}
	for _, name := range live.ExhaustedModels {
		if model.NormalizeModelName(name) != probe.model {
			pending.ExhaustedModels = append(pending.ExhaustedModels, name)
		}
	}
	// API Key 不查询 Start Plan 数值余额。成功证明可调用，却不能编造余额；
	// 仅令目标模型的旧数值变为未知，保留其他模型的快照与耗尽标记。
	for key, entry := range pending.Quota {
		entryModel, _ := entry["model"].(string)
		if entryModel == "" {
			entryModel = key
		}
		if model.NormalizeModelName(entryModel) != probe.model {
			continue
		}
		if entry == nil {
			entry = map[string]any{"model": probe.model}
			pending.Quota[key] = entry
		}
		entry["remaining"], entry["available"] = nil, nil
	}
	if len(pending.Quota) > 0 && len(pending.QuotaEntriesForModel(probe.model)) == 0 {
		pending.Quota[probe.model] = map[string]any{"model": probe.model}
	}
	if pending.Status == model.StatusExhausted || pending.Status == model.StatusCooling {
		pending.Status, pending.CoolingUntil = model.StatusActive, nil
	}
	next := s.cloneQuotaRecoveryLocked()
	delete(next[expected.ID], probe.model)
	if len(next[expected.ID]) == 0 {
		delete(next, expected.ID)
	}
	if err := s.commitQuotaRecoveryLocked(next, pending); err != nil {
		return false, err
	}
	return true, nil
}
