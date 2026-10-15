package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

const ModelProbeModel = "glm-5.3-flash"
const ModelProbeRuleVersion = "proxy-model-probe-3012-v1"
const ModelProbeTemplateVersion = "minimal-ok-v1"
const modelProbeStateKey = "model_probe_v1"
const ModelProbeDirect = "direct"

var ErrModelProbeUnavailable = errors.New("模型巡检未启用、账号或线路尚未就绪")
var ErrModelProbeConflict = errors.New("模型巡检配置或执行权已变化，请刷新后重试")
var ErrModelProbeBudget = errors.New("模型巡检请求预算已用完")

// ModelProbeSettings 的账号列表同时声明专用角色，关闭 Enabled 不自动释放角色。
type ModelProbeSettings struct {
	Enabled            bool              `json:"enabled"`
	Mode               string            `json:"mode"`
	Model              string            `json:"model"`
	IntervalMinutes    int               `json:"interval_minutes"`
	Concurrency        int               `json:"concurrency"`
	TimeoutSeconds     int               `json:"timeout_seconds"`
	GlobalDailyLimit   int               `json:"global_daily_limit"`
	AccountDailyLimit  int               `json:"account_daily_limit"`
	AccountIDs         []string          `json:"account_ids"`
	TargetProxyIDs     []string          `json:"target_proxy_ids"`
	ReferenceProxyIDs  []string          `json:"reference_proxy_ids"`
	EgressGroups       map[string]string `json:"egress_groups"`
	AllowDirectControl bool              `json:"allow_direct_control"`
	Revision           uint64            `json:"revision"`
}

func DefaultModelProbeSettings() ModelProbeSettings {
	return ModelProbeSettings{Mode: "observe", Model: ModelProbeModel, IntervalMinutes: 30, Concurrency: 1, TimeoutSeconds: 30,
		GlobalDailyLimit: 200, AccountDailyLimit: 100, AccountIDs: []string{}, TargetProxyIDs: []string{}, ReferenceProxyIDs: []string{}, EgressGroups: map[string]string{}}
}

func (p ModelProbeSettings) clone() ModelProbeSettings {
	p.AccountIDs = append([]string{}, p.AccountIDs...)
	p.TargetProxyIDs = append([]string{}, p.TargetProxyIDs...)
	p.ReferenceProxyIDs = append([]string{}, p.ReferenceProxyIDs...)
	groups := map[string]string{}
	for k, v := range p.EgressGroups {
		groups[k] = v
	}
	p.EgressGroups = groups
	return p
}

type modelProbePersistent struct {
	Version       int                `json:"version"`
	Policy        ModelProbeSettings `json:"policy"`
	NextDue       map[string]int64   `json:"next_due"`
	LastDiagnosis map[string]int64   `json:"last_diagnosis"`
	PausedUntil   int64              `json:"paused_until"`
}

type modelProbeState struct {
	modelProbePersistent
	runs         map[string]*ModelProbeRun
	leases       map[string]*ModelProbeLease
	business     map[string]modelProbeBusiness
	hints        map[string]time.Time
	storageError bool
}

type modelProbeBusiness struct {
	ProxyObservation
	identity, credential, account string
	generation                    uint64
}

type ModelProbeSample struct {
	Source       string      `json:"source"`
	RequestID    string      `json:"request_id"`
	AccountRef   string      `json:"account_ref"`
	RouteID      string      `json:"route_id"`
	Role         string      `json:"role"`
	Round        int         `json:"round"`
	At           int64       `json:"at"`
	Status       int         `json:"status"`
	BusinessCode string      `json:"business_code"`
	Kind         string      `json:"kind"`
	Complete     bool        `json:"complete"`
	Usage        model.Usage `json:"usage"`
	Stale        bool        `json:"stale"`
}

type ModelProbeRun struct {
	ID              string             `json:"id"`
	TargetID        string             `json:"target_id"`
	Source          string             `json:"source"`
	State           string             `json:"state"`
	Verdict         string             `json:"verdict"`
	Reason          string             `json:"reason"`
	Mode            string             `json:"mode"`
	RuleVersion     string             `json:"rule_version"`
	TemplateVersion string             `json:"template_version"`
	DecisionID      string             `json:"decision_id,omitempty"`
	CreatedAt       int64              `json:"created_at"`
	UpdatedAt       int64              `json:"updated_at"`
	ObservedAt      int64              `json:"observed_at"`
	NextAt          int64              `json:"next_at"`
	Deadline        int64              `json:"deadline"`
	Attempts        int                `json:"attempts"`
	ConfirmedRounds int                `json:"confirmed_rounds"`
	Samples         []ModelProbeSample `json:"samples"`
	policy          ModelProbeSettings
	target          ProxyProfile
	accounts        []string
	accountIndex    int
	phase           string
	round           int
	referenceID     string
	proofs          []modelProbeEvidence
	credentials     map[string]string
	bindings        map[string]uint64
	targetURL       string
}

func (r *ModelProbeRun) view() ModelProbeRun {
	v := *r
	v.Samples = append([]ModelProbeSample{}, r.Samples...)
	v.proofs, v.accounts = nil, nil
	v.credentials, v.bindings = nil, nil
	v.target, v.policy = ProxyProfile{}, ModelProbeSettings{}
	return v
}

func probeRunOpen(r *ModelProbeRun) bool {
	return r.State == "queued" || r.State == "running" || r.State == "waiting"
}

func (s *Store) loadModelProbe() error {
	s.modelProbe = modelProbeState{modelProbePersistent: modelProbePersistent{Version: 1, Policy: DefaultModelProbeSettings(), NextDue: map[string]int64{}, LastDiagnosis: map[string]int64{}},
		runs: map[string]*ModelProbeRun{}, leases: map[string]*ModelProbeLease{}, business: map[string]modelProbeBusiness{}, hints: map[string]time.Time{}}
	if raw := s.settings[modelProbeStateKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.modelProbe.modelProbePersistent); err != nil {
			return fmt.Errorf("读取模型巡检状态失败: %w", err)
		}
		if s.modelProbe.Version != 1 {
			return errors.New("模型巡检状态版本不兼容")
		}
	}
	if s.modelProbe.NextDue == nil {
		s.modelProbe.NextDue = map[string]int64{}
	}
	if s.modelProbe.LastDiagnosis == nil {
		s.modelProbe.LastDiagnosis = map[string]int64{}
	}
	delete(s.settings, modelProbeStateKey)
	s.publishSettings()
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS model_probe_runs (id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, data TEXT NOT NULL);
	CREATE TABLE IF NOT EXISTS model_probe_attempts (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, started_at INTEGER NOT NULL);
	CREATE INDEX IF NOT EXISTS idx_model_probe_attempts_at ON model_probe_attempts(started_at);`); err != nil {
		return err
	}
	rows, err := s.db.Query("SELECT data FROM model_probe_runs ORDER BY created_at DESC LIMIT 10000")
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw string
		var r ModelProbeRun
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			rows.Close()
			return fmt.Errorf("读取模型巡检历史失败: %w", err)
		}
		if probeRunOpen(&r) {
			r.State, r.Reason, r.Verdict = "interrupted", "process_restarted", "inconclusive"
		}
		s.modelProbe.runs[r.ID] = &r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range s.modelProbe.runs {
		if r.State == "interrupted" {
			if err := s.persistModelProbeRunLocked(r); err != nil {
				return err
			}
		}
	}
	s.publishModelProbeRolesLocked()
	return nil
}

func (s *Store) ModelProbeSettings() ModelProbeSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelProbe.Policy.clone()
}

func (s *Store) isModelProbeAccountLocked(id string) bool {
	return slices.Contains(s.modelProbe.Policy.AccountIDs, id)
}

func (s *Store) publishModelProbeRolesLocked() {
	for _, a := range s.allAccountsLocked() {
		a.ModelProbeOnly = s.isModelProbeAccountLocked(a.ID)
	}
}

func (s *Store) SaveModelProbeSettings(p ModelProbeSettings, now time.Time) (ModelProbeSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.modelProbe.Policy
	if p.Revision != old.Revision {
		return old.clone(), ErrModelProbeConflict
	}
	if p.Model != ModelProbeModel || (p.Mode != "observe" && p.Mode != "auto_retire") || p.IntervalMinutes < 1 || p.IntervalMinutes > 1440 || p.Concurrency < 1 || p.Concurrency > 2 || p.TimeoutSeconds < 5 || p.TimeoutSeconds > 60 || p.GlobalDailyLimit < 1 || p.GlobalDailyLimit > 10000 || p.AccountDailyLimit < 1 || p.AccountDailyLimit > 10000 {
		return old.clone(), errors.New("模型巡检模式、间隔或请求预算无效")
	}
	p.AccountIDs, p.TargetProxyIDs, p.ReferenceProxyIDs = dedupeIDs(p.AccountIDs), dedupeIDs(p.TargetProxyIDs), dedupeIDs(p.ReferenceProxyIDs)
	if len(p.AccountIDs) > 32 || len(p.TargetProxyIDs) > 1000 || len(p.ReferenceProxyIDs) > 32 || len(p.EgressGroups) > 1033 {
		return old.clone(), errors.New("模型巡检选择数量超过上限")
	}
	for _, id := range p.AccountIDs {
		if !p.Enabled && slices.Contains(old.AccountIDs, id) {
			continue
		}
		a := s.findAnyLocked(id)
		if a == nil || a.Mode != "jwt" || s.initializationManagedLocked(id) {
			return old.clone(), errors.New("探测账号必须是存在且未受初始化或固定出口托管的 JWT")
		}
		if !slices.Contains(old.AccountIDs, id) && (!a.Enabled || a.ArchivedAt != nil || a.Status == model.StatusInvalid) {
			return old.clone(), errors.New("不能将停用、归档或失效账号加入探测专用集合")
		}
	}
	profiles := map[string]ProxyProfile{}
	for _, v := range s.listProxyProfilesLocked() {
		profiles[v.ID] = v
	}
	if p.Enabled && (len(p.AccountIDs) == 0 || len(p.TargetProxyIDs) == 0) {
		return old.clone(), errors.New("启用前请选择探测账号与目标代理")
	}
	if p.Enabled {
		for _, id := range append(slices.Clone(p.TargetProxyIDs), p.ReferenceProxyIDs...) {
			v, ok := profiles[id]
			if !ok || !v.Enabled || s.strictProxyBlockedLocked(v.URL) {
				return old.clone(), errors.New("所选目标或参考代理不可用")
			}
		}
	}
	for k, v := range p.EgressGroups {
		v = strings.TrimSpace(v)
		if len(v) > 64 || strings.ContainsAny(v, "\r\n\t") || (k != ModelProbeDirect && profiles[k].ID == "" && p.Enabled) {
			return old.clone(), errors.New("出口组标识无效")
		}
		p.EgressGroups[k] = v
	}
	if p.Enabled && p.Mode == "auto_retire" {
		if len(p.AccountIDs) < 2 {
			return old.clone(), errors.New("自动处置至少需要两个探测专用账号")
		}
		for _, id := range p.TargetProxyIDs {
			if s.probeReferenceLocked(p, profiles[id]) == "" {
				return old.clone(), errors.New("自动处置的每个目标必须有不同出口组的参考代理或明确允许的直连")
			}
		}
	}
	p.Revision++
	next := s.modelProbe.modelProbePersistent
	next.Policy, next.NextDue = p.clone(), map[string]int64{}
	for _, id := range p.TargetProxyIDs {
		next.NextDue[id] = now.Add(time.Duration(p.IntervalMinutes) * time.Minute).UnixMilli()
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return old.clone(), err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return old.clone(), err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES (?,?)", modelProbeStateKey, string(raw)); err != nil {
		return old.clone(), err
	}
	changed := map[string]*ModelProbeRun{}
	for id, r := range s.modelProbe.runs {
		if !probeRunOpen(r) {
			continue
		}
		copy := *r
		copy.State, copy.Verdict, copy.Reason = "interrupted", "inconclusive", "policy_changed"
		copy.UpdatedAt = now.UnixMilli()
		data, err := json.Marshal(&copy)
		if err != nil {
			return old.clone(), err
		}
		if _, err = tx.Exec("UPDATE model_probe_runs SET data=? WHERE id=?", string(data), id); err != nil {
			return old.clone(), err
		}
		changed[id] = &copy
	}
	if err = tx.Commit(); err != nil {
		return old.clone(), err
	}
	s.modelProbe.modelProbePersistent = next
	s.modelProbe.storageError = false
	for id, r := range changed {
		s.modelProbe.runs[id] = r
	}
	s.publishModelProbeRolesLocked()
	return p.clone(), nil
}

func (s *Store) probeReferenceLocked(p ModelProbeSettings, target ProxyProfile) string {
	for _, id := range p.ReferenceProxyIDs {
		for _, ref := range s.listProxyProfilesLocked() {
			if ref.ID != id || !ref.Enabled || strictProxyIdentity(ref.URL) == strictProxyIdentity(target.URL) || s.strictProxyBlockedLocked(ref.URL) {
				continue
			}
			if p.EgressGroups[id] != "" && p.EgressGroups[id] == p.EgressGroups[target.ID] {
				continue
			}
			if p.Mode == "auto_retire" && (p.EgressGroups[id] == "" || p.EgressGroups[target.ID] == "") {
				continue
			}
			return id
		}
	}
	if p.AllowDirectControl && (p.Mode != "auto_retire" || (p.EgressGroups[ModelProbeDirect] != "" && p.EgressGroups[target.ID] != "" && p.EgressGroups[ModelProbeDirect] != p.EgressGroups[target.ID])) {
		return ModelProbeDirect
	}
	return ""
}

func (s *Store) persistModelProbeRunLocked(r *ModelProbeRun) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT OR REPLACE INTO model_probe_runs(id,created_at,data) VALUES (?,?,?)", r.ID, r.CreatedAt, string(raw))
	return err
}

func (s *Store) ModelProbeHistory(target string, offset, limit int) []ModelProbeRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modelProbeHistoryLocked(target, offset, limit)
}

func (s *Store) modelProbeHistoryLocked(target string, offset, limit int) []ModelProbeRun {
	var runs []*ModelProbeRun
	for _, r := range s.modelProbe.runs {
		if target == "" || r.TargetID == target {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].CreatedAt == runs[j].CreatedAt {
			return runs[i].ID > runs[j].ID
		}
		return runs[i].CreatedAt > runs[j].CreatedAt
	})
	offset, limit = max(0, offset), max(1, min(100, limit))
	out := []ModelProbeRun{}
	for i := offset; i < len(runs) && len(out) < limit; i++ {
		out = append(out, runs[i].view())
	}
	return out
}

type ModelProbeSummary struct {
	ObservedTargets   int                `json:"observed_targets"`
	TotalTargets      int                `json:"total_targets"`
	StorageError      bool               `json:"storage_error"`
	Settings          ModelProbeSettings `json:"settings"`
	Runs              []ModelProbeRun    `json:"runs"`
	UsedRequests      int                `json:"used_requests"`
	RemainingRequests int                `json:"remaining_requests"`
	PausedUntil       int64              `json:"paused_until"`
	NextDue           map[string]int64   `json:"next_due"`
	Active            int                `json:"active"`
	RuleVersion       string             `json:"rule_version"`
}

func (s *Store) ModelProbeSummary(now time.Time) (ModelProbeSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var used int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM model_probe_attempts WHERE started_at>?", now.Add(-24*time.Hour).UnixMilli()).Scan(&used); err != nil {
		return ModelProbeSummary{}, err
	}
	due := map[string]int64{}
	for k, v := range s.modelProbe.NextDue {
		due[k] = v
	}
	latest := map[string]*ModelProbeRun{}
	for _, r := range s.modelProbe.runs {
		old := latest[r.TargetID]
		if old == nil || r.CreatedAt > old.CreatedAt || (r.CreatedAt == old.CreatedAt && r.ID > old.ID) {
			latest[r.TargetID] = r
		}
	}
	views := []ModelProbeRun{}
	observed := 0
	for _, id := range s.modelProbe.Policy.TargetProxyIDs {
		if r := latest[id]; r != nil {
			v := r.view()
			v.Samples = []ModelProbeSample{}
			views = append(views, v)
			if r.ObservedAt > now.Add(-30*time.Minute).UnixMilli() {
				observed++
			}
		}
	}
	return ModelProbeSummary{ObservedTargets: observed, TotalTargets: len(s.modelProbe.Policy.TargetProxyIDs), StorageError: s.modelProbe.storageError, Settings: s.modelProbe.Policy.clone(), Runs: views, UsedRequests: used, RemainingRequests: max(0, s.modelProbe.Policy.GlobalDailyLimit-used), PausedUntil: s.modelProbe.PausedUntil, NextDue: due, Active: len(s.modelProbe.leases), RuleVersion: ModelProbeRuleVersion}, nil
}

// 清理展示历史不影响仍在滚动预算窗口内的尝试登记。
func (s *Store) pruneModelProbeLocked(now time.Time) error {
	cutoff := now.Add(-7 * 24 * time.Hour).UnixMilli()
	if _, err := s.db.Exec("DELETE FROM model_probe_attempts WHERE started_at<?", now.Add(-24*time.Hour).UnixMilli()); err != nil {
		return err
	}
	if _, err := s.db.Exec("DELETE FROM model_probe_runs WHERE created_at<? OR id NOT IN (SELECT id FROM model_probe_runs ORDER BY created_at DESC LIMIT 10000)", cutoff); err != nil {
		return err
	}
	if len(s.modelProbe.runs) > 10000 {
		var expired []*ModelProbeRun
		for _, r := range s.modelProbe.runs {
			if !probeRunOpen(r) {
				expired = append(expired, r)
			}
		}
		sort.Slice(expired, func(i, j int) bool { return expired[i].CreatedAt < expired[j].CreatedAt })
		for _, r := range expired {
			if len(s.modelProbe.runs) <= 10000 {
				break
			}
			delete(s.modelProbe.runs, r.ID)
		}
	}
	for id, r := range s.modelProbe.runs {
		if !probeRunOpen(r) && r.CreatedAt < cutoff {
			delete(s.modelProbe.runs, id)
		}
	}
	return nil
}

func (s *Store) persistProbeStateLocked() error {
	raw, err := json.Marshal(s.modelProbe.modelProbePersistent)
	if err != nil {
		return err
	}
	return s.setMeta(modelProbeStateKey, string(raw))
}

// StopModelProbes 终止未完成诊断，保留已发生的请求预算和展示历史。
func (s *Store) StopModelProbes(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.modelProbe.runs {
		if !probeRunOpen(r) {
			continue
		}
		copy := *r
		copy.State, copy.Verdict, copy.Reason = "interrupted", "inconclusive", "process_stopped"
		copy.UpdatedAt = now.UnixMilli()
		if err := s.persistModelProbeRunLocked(&copy); err != nil {
			return err
		}
		s.modelProbe.runs[r.ID] = &copy
	}
	return nil
}

// 仅保存去敏观测，不读取日志或正文。业务证据与主动探测证据不互相凑数。
func (s *Store) observeModelProbeBusinessLocked(acc *model.Account, egress ProxyEgress, o ProxyObservation) {
	if egress.ID == "" || !s.modelProbe.Policy.Enabled || o.Model != ModelProbeModel {
		return
	}
	if o.Complete && o.Status >= 200 && o.Status < 300 {
		s.modelProbe.business[egress.ID] = modelProbeBusiness{ProxyObservation: o, identity: egress.identity, generation: egress.Generation, credential: s.strictCredentialRefLocked(acc), account: acc.ID}
	}
	if o.Status == 405 && o.BusinessCode == "3012" {
		s.modelProbe.hints[egress.ID] = o.At
	}
}

func (s *Store) recentProbeBusinessLocked(profile ProxyProfile, now time.Time, window time.Duration) (ProxyObservation, bool) {
	e, ok := s.modelProbe.business[profile.ID]
	if !ok || e.generation != profile.Generation || e.identity != strictProxyIdentity(profile.URL) || e.At.Before(now.Add(-window)) || e.At.After(now) {
		return ProxyObservation{}, false
	}
	a := s.findAnyLocked(e.account)
	if a == nil || s.strictCredentialRefLocked(a) != e.credential {
		return ProxyObservation{}, false
	}
	// 同模型名的 API Key 备用端点不能替 JWT 目标作可用性证明。
	if !probeTargetMatches(config.UpstreamZai, e.ProxyObservation) {
		return ProxyObservation{}, false
	}
	return e.ProxyObservation, true
}

// 独立 SQL 事务同时保存探测结果和账号实际消耗，失败时不提前发布账号快照。
func (s *Store) commitProbeResultLocked(r *ModelProbeRun, a *model.Account) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a != nil {
		if err := persistAccountOnTx(tx, a); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO model_probe_runs(id,created_at,data) VALUES (?,?,?)", r.ID, r.CreatedAt, string(raw)); err != nil {
		return err
	}
	state, err := json.Marshal(s.modelProbe.modelProbePersistent)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES (?,?)", modelProbeStateKey, string(state)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if a != nil {
		for i, v := range s.accounts[a.Provider] {
			if v.ID == a.ID {
				s.accounts[a.Provider][i] = a
				break
			}
		}
	}
	s.modelProbe.runs[r.ID] = r
	return nil
}
