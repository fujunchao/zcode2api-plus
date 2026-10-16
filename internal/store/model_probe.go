package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
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

// 自动择优的账号滚动窗口：有界、随巡检状态持久化；只作启发式排序，不参与删除证据。
const (
	probeAccountWindowEntries = 20
	probeAccountWindowTTL     = 24 * time.Hour
	probeAccountWindowLimit   = 500
	probeAccountViewLimit     = 8
)

var ErrModelProbeUnavailable = errors.New("模型巡检未启用、账号或线路尚未就绪")
var ErrModelProbeConflict = errors.New("模型巡检配置或执行权已变化，请刷新后重试")
var ErrModelProbeBudget = errors.New("模型巡检请求预算已用完")

// ModelProbeSettings 的账号列表同时声明专用角色，关闭 Enabled 不自动释放角色。
type ModelProbeSettings struct {
	HealthVersion          int    `json:"health_version"`
	NetworkEnabled         bool   `json:"network_enabled"`
	NetworkIntervalMinutes int    `json:"network_interval_minutes"`
	NetworkConfirmSeconds  int    `json:"network_confirm_seconds"`
	MigrationNotice        string `json:"migration_notice,omitempty"`
	Enabled                bool   `json:"enabled"`
	Mode                   string `json:"mode"`
	Model                  string `json:"model"`
	IntervalMinutes        int    `json:"interval_minutes"`
	Concurrency            int    `json:"concurrency"`
	TimeoutSeconds         int    `json:"timeout_seconds"`
	GlobalDailyLimit       int    `json:"global_daily_limit"`
	AccountDailyLimit      int    `json:"account_daily_limit"`
	// TargetMode 决定模型巡检覆盖哪些线路：auto 动态覆盖全部启用线路（可配排除名单），
	// manual 只巡检显式选择的 TargetProxyIDs。
	TargetMode      string   `json:"target_mode"`
	ExcludeProxyIDs []string `json:"exclude_proxy_ids"`
	// AccountMode 决定探测账号来源：auto 从全部合格账号按近期成功率自动择优，
	// dedicated 只使用显式选择的 AccountIDs（这些账号退出普通业务选号）。
	AccountMode        string            `json:"account_mode"`
	AccountIDs         []string          `json:"account_ids"`
	TargetProxyIDs     []string          `json:"target_proxy_ids"`
	ReferenceProxyIDs  []string          `json:"reference_proxy_ids"`
	EgressGroups       map[string]string `json:"egress_groups"`
	AllowDirectControl bool              `json:"allow_direct_control"`
	Revision           uint64            `json:"revision"`
}

func DefaultModelProbeSettings() ModelProbeSettings {
	// HealthVersion 保持 0：全新安装仍需经过 migrateNetworkHealthPolicy 继承网络巡检的
	// 环境默认值，再由 migrateAutoHealthPolicy 落到 v2（自动覆盖 + 自动择优）。
	return ModelProbeSettings{TargetMode: "auto", AccountMode: "auto", NetworkIntervalMinutes: 30, NetworkConfirmSeconds: 10, Mode: "observe", Model: ModelProbeModel, IntervalMinutes: 30, Concurrency: 1, TimeoutSeconds: 30,
		GlobalDailyLimit: 200, AccountDailyLimit: 100, ExcludeProxyIDs: []string{}, AccountIDs: []string{}, TargetProxyIDs: []string{}, ReferenceProxyIDs: []string{}, EgressGroups: map[string]string{}}
}

func (p ModelProbeSettings) clone() ModelProbeSettings {
	p.ExcludeProxyIDs = append([]string{}, p.ExcludeProxyIDs...)
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
	Version                 int                            `json:"version"`
	Policy                  ModelProbeSettings             `json:"policy"`
	NextDue                 map[string]int64               `json:"next_due"`
	LastDiagnosis           map[string]int64               `json:"last_diagnosis"`
	PausedUntil             int64                          `json:"paused_until"`
	EvidenceIncompleteUntil int64                          `json:"evidence_incomplete_until"`
	AccountWindows          map[string]*probeAccountWindow `json:"account_windows,omitempty"`
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
	s.modelProbe = modelProbeState{modelProbePersistent: modelProbePersistent{Version: 1, Policy: DefaultModelProbeSettings(), NextDue: map[string]int64{}, LastDiagnosis: map[string]int64{}, AccountWindows: map[string]*probeAccountWindow{}},
		runs: map[string]*ModelProbeRun{}, leases: map[string]*ModelProbeLease{}, business: map[string]modelProbeBusiness{}, hints: map[string]time.Time{}}
	existing := s.settings[modelProbeStateKey] != ""
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
	if s.modelProbe.AccountWindows == nil {
		s.modelProbe.AccountWindows = map[string]*probeAccountWindow{}
	}
	delete(s.settings, modelProbeStateKey)
	s.publishSettings()
	s.migrateNetworkHealthPolicy()
	s.migrateAutoHealthPolicy(existing)
	if err := s.initNetworkHealth(); err != nil {
		return err
	}
	if err := s.ClearInterruptedNetworkHealth(); err != nil {
		return err
	}
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
		// 历史只作展示。重启丢掉了私有可比证据，不能靠新样本宣称整个窗口没有反证。
		for _, sample := range r.Samples {
			if sample.Status > 0 {
				s.modelProbe.EvidenceIncompleteUntil = max(s.modelProbe.EvidenceIncompleteUntil, sample.At+int64((10*time.Minute+time.Millisecond)/time.Millisecond))
			}
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
	return s.persistProbeStateLocked()
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
	if p.NetworkIntervalMinutes < 1 || p.NetworkIntervalMinutes > 1440 || p.NetworkConfirmSeconds < 1 || p.NetworkConfirmSeconds > 300 {
		return old.clone(), errors.New("网络巡检间隔或复核等待无效")
	}
	if (p.TargetMode != "auto" && p.TargetMode != "manual") || (p.AccountMode != "auto" && p.AccountMode != "dedicated") {
		return old.clone(), errors.New("模型巡检目标或账号模式无效")
	}
	p.HealthVersion = 2
	p.MigrationNotice = ""
	if p.Model != ModelProbeModel || (p.Mode != "observe" && p.Mode != "auto_retire") || p.IntervalMinutes < 1 || p.IntervalMinutes > 1440 || p.Concurrency < 1 || p.Concurrency > 2 || p.TimeoutSeconds < 5 || p.TimeoutSeconds > 60 || p.GlobalDailyLimit < 1 || p.GlobalDailyLimit > 10000 || p.AccountDailyLimit < 1 || p.AccountDailyLimit > 10000 {
		return old.clone(), errors.New("模型巡检模式、间隔或请求预算无效")
	}
	p.AccountIDs, p.TargetProxyIDs, p.ReferenceProxyIDs = dedupeIDs(p.AccountIDs), dedupeIDs(p.TargetProxyIDs), dedupeIDs(p.ReferenceProxyIDs)
	// 自动择优不使用专用账号集合：切换即显式释放旧专用角色，让这些账号回到业务池。
	if p.AccountMode == "auto" {
		p.AccountIDs = []string{}
	}
	if len(p.AccountIDs) > 32 || len(p.TargetProxyIDs) > 1000 || len(p.ReferenceProxyIDs) > 32 || len(p.EgressGroups) > 1033 {
		return old.clone(), errors.New("模型巡检选择数量超过上限")
	}
	profiles := map[string]ProxyProfile{}
	for _, v := range s.listProxyProfilesLocked() {
		profiles[v.ID] = v
	}
	// 排除名单剪除已删除线路：残留 ID 不应让配置无法保存；数量保持有界。
	excluded := make([]string, 0, len(p.ExcludeProxyIDs))
	for _, id := range dedupeIDs(p.ExcludeProxyIDs) {
		if profiles[id].ID != "" {
			excluded = append(excluded, id)
		}
	}
	if len(excluded) > 1000 {
		return old.clone(), errors.New("排除线路数量超过上限")
	}
	p.ExcludeProxyIDs = excluded
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
	if p.Enabled && p.AccountMode == "dedicated" && len(p.AccountIDs) == 0 {
		return old.clone(), errors.New("启用前请选择探测账号")
	}
	if p.Enabled && p.TargetMode == "manual" && len(p.TargetProxyIDs) == 0 {
		return old.clone(), errors.New("启用前请选择目标代理")
	}
	if p.Enabled {
		// 手工模式校验所选目标；参考线路无论哪种模式都必须是可用线路。
		checked := p.ReferenceProxyIDs
		if p.TargetMode == "manual" {
			checked = append(slices.Clone(p.TargetProxyIDs), p.ReferenceProxyIDs...)
		}
		for _, id := range checked {
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
		if p.AccountMode == "dedicated" && len(p.AccountIDs) < 2 {
			return old.clone(), errors.New("自动处置至少需要两个探测专用账号")
		}
		// 自动覆盖在运行期逐目标确认独立参考；手工模式保持原有保存期门槛。
		if p.TargetMode == "manual" {
			for _, id := range p.TargetProxyIDs {
				if s.probeReferenceLocked(p, profiles[id], now) == "" {
					return old.clone(), errors.New("自动处置的每个目标必须有不同出口组的参考代理或明确允许的直连")
				}
			}
		}
	}
	p.Revision++
	next := s.modelProbe.modelProbePersistent
	next.Policy, next.NextDue = p.clone(), map[string]int64{}
	if p.TargetMode == "manual" {
		for _, id := range p.TargetProxyIDs {
			next.NextDue[id] = now.Add(time.Duration(p.IntervalMinutes) * time.Minute).UnixMilli()
		}
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
	aliases := map[string]string{"proxy_health_enabled": strconv.FormatBool(p.NetworkEnabled), "proxy_health_interval": strconv.Itoa(p.NetworkIntervalMinutes)}
	for key, value := range aliases {
		if _, err = tx.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES (?,?)", key, value); err != nil {
			return old.clone(), err
		}
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
	for key, value := range aliases {
		s.settings[key] = value
	}
	s.publishSettings()
	s.modelProbe.storageError = false
	for id, r := range changed {
		s.modelProbe.runs[id] = r
	}
	s.publishModelProbeRolesLocked()
	return p.clone(), nil
}

// probeReferenceLocked 解析目标线路的对照参考：手工列表优先；自动覆盖模式在未指定
// 手工参考时，从其余启用线路中挑选独立参考（别名与已声明的同出口组不互证）。
func (s *Store) probeReferenceLocked(p ModelProbeSettings, target ProxyProfile, now time.Time) string {
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
	if len(p.ReferenceProxyIDs) == 0 && p.TargetMode == "auto" {
		type candidate struct {
			id    string
			fresh int64
		}
		cands := []candidate{}
		for _, ref := range s.listProxyProfilesLocked() {
			if ref.ID == target.ID || !ref.Enabled || strictProxyIdentity(ref.URL) == strictProxyIdentity(target.URL) || s.strictProxyBlockedLocked(ref.URL) {
				continue
			}
			if p.EgressGroups[ref.ID] != "" && p.EgressGroups[ref.ID] == p.EgressGroups[target.ID] {
				continue
			}
			if p.Mode == "auto_retire" && (p.EgressGroups[ref.ID] == "" || p.EgressGroups[target.ID] == "") {
				continue
			}
			// 近期有完整业务成功者优先，其余按 ID 定序，保证结论可复现。
			fresh := int64(0)
			if e, ok := s.recentProbeBusinessLocked(ref, now, 30*time.Minute); ok {
				fresh = e.At.UnixMilli()
			}
			cands = append(cands, candidate{id: ref.ID, fresh: fresh})
		}
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].fresh != cands[j].fresh {
				return cands[i].fresh > cands[j].fresh
			}
			return cands[i].id < cands[j].id
		})
		if len(cands) > 0 {
			return cands[0].id
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

// ModelProbeAccountView 是自动择优账号的可解释视图（只读，供管理端展示选择依据）。
type ModelProbeAccountView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Eligible      bool   `json:"eligible"`
	Reason        string `json:"reason,omitempty"`
	Rank          int    `json:"rank,omitempty"`
	RecentSuccess int    `json:"recent_success"`
	RecentTotal   int    `json:"recent_total"`
	LastSuccessAt int64  `json:"last_success_at,omitempty"`
	LastPickAt    int64  `json:"last_pick_at,omitempty"`
}

type ModelProbeSummary struct {
	EvidenceIncompleteUntil  int64                   `json:"evidence_incomplete_until"`
	ObservedTargets          int                     `json:"observed_targets"`
	TotalTargets             int                     `json:"total_targets"`
	ExcludedTargets          int                     `json:"excluded_targets"`
	EffectiveIntervalMinutes int                     `json:"effective_interval_minutes"`
	StorageError             bool                    `json:"storage_error"`
	Settings                 ModelProbeSettings      `json:"settings"`
	Runs                     []ModelProbeRun         `json:"runs"`
	Accounts                 []ModelProbeAccountView `json:"accounts"`
	UsedRequests             int                     `json:"used_requests"`
	RemainingRequests        int                     `json:"remaining_requests"`
	PausedUntil              int64                   `json:"paused_until"`
	NextDue                  map[string]int64        `json:"next_due"`
	Active                   int                     `json:"active"`
	RuleVersion              string                  `json:"rule_version"`
}

func (s *Store) ModelProbeSummary(now time.Time) (ModelProbeSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var used int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM model_probe_attempts WHERE started_at>?", now.Add(-24*time.Hour).UnixMilli()).Scan(&used); err != nil {
		return ModelProbeSummary{}, err
	}
	p := s.modelProbe.Policy
	targets := s.effectiveProbeTargetsLocked(p)
	due := map[string]int64{}
	for _, t := range targets {
		if v, ok := s.modelProbe.NextDue[t.ID]; ok {
			due[t.ID] = v
		}
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
	for _, t := range targets {
		if r := latest[t.ID]; r != nil {
			v := r.view()
			v.Samples = []ModelProbeSample{}
			views = append(views, v)
			if r.ObservedAt > now.Add(-30*time.Minute).UnixMilli() {
				observed++
			}
		}
	}
	excluded := 0
	if p.TargetMode == "auto" {
		for _, v := range s.listProxyProfilesLocked() {
			if v.Enabled && slices.Contains(p.ExcludeProxyIDs, v.ID) {
				excluded++
			}
		}
	}
	_, effective := s.probeRotationLocked(p, len(targets))
	return ModelProbeSummary{EvidenceIncompleteUntil: s.modelProbe.EvidenceIncompleteUntil, ObservedTargets: observed, TotalTargets: len(targets), ExcludedTargets: excluded,
		EffectiveIntervalMinutes: int(effective / time.Minute), StorageError: s.modelProbe.storageError, Settings: p.clone(), Runs: views, Accounts: s.probeAccountViewsLocked(now),
		UsedRequests: used, RemainingRequests: max(0, p.GlobalDailyLimit-used), PausedUntil: s.modelProbe.PausedUntil, NextDue: due, Active: len(s.modelProbe.leases), RuleVersion: ModelProbeRuleVersion}, nil
}

// 清理展示历史不影响仍在滚动预算窗口内的尝试登记。
func (s *Store) pruneModelProbeLocked(now time.Time) error {
	cutoff := now.Add(-7 * 24 * time.Hour).UnixMilli()
	var completed []*ModelProbeRun
	for _, r := range s.modelProbe.runs {
		if !probeRunOpen(r) {
			completed = append(completed, r)
		}
	}
	sort.Slice(completed, func(i, j int) bool {
		if completed[i].CreatedAt == completed[j].CreatedAt {
			return completed[i].ID < completed[j].ID
		}
		return completed[i].CreatedAt < completed[j].CreatedAt
	})
	remove := []string{}
	size := len(s.modelProbe.runs)
	for _, r := range completed {
		if r.CreatedAt >= cutoff && size < 10000 {
			break
		}
		remove = append(remove, r.ID)
		size--
		for _, e := range r.proofs {
			s.modelProbe.EvidenceIncompleteUntil = max(s.modelProbe.EvidenceIncompleteUntil, e.At.Add(10*time.Minute+time.Millisecond).UnixMilli())
		}
	}
	// 账号滚动窗口：24 小时过期 + 账号数上限，保持状态有界（只作排序启发，不参与证据）。
	windowCutoff := now.Add(-probeAccountWindowTTL).UnixMilli()
	for id, w := range s.modelProbe.AccountWindows {
		if w == nil || w.LastAt < windowCutoff {
			delete(s.modelProbe.AccountWindows, id)
		}
	}
	if len(s.modelProbe.AccountWindows) > probeAccountWindowLimit {
		type aged struct {
			id string
			at int64
		}
		all := make([]aged, 0, len(s.modelProbe.AccountWindows))
		for id, w := range s.modelProbe.AccountWindows {
			all = append(all, aged{id: id, at: w.LastAt})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
		for _, v := range all[:len(all)-probeAccountWindowLimit] {
			delete(s.modelProbe.AccountWindows, v.id)
		}
	}
	// 禁止窗口先落盘，即使随后清理失败也不能从缺失的缓存产生决定。
	if err := s.persistProbeStateLocked(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM model_probe_attempts WHERE started_at<?", now.Add(-24*time.Hour).UnixMilli()); err != nil {
		return err
	}
	for _, id := range remove {
		if _, err = tx.Exec("DELETE FROM model_probe_runs WHERE id=?", id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, id := range remove {
		delete(s.modelProbe.runs, id)
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
