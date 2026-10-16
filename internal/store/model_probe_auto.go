package store

import (
	"errors"
	"slices"
	"sort"
	"time"

	"zcode2api/internal/config"
)

// 本文件承载「自动全覆盖 + 账号自动择优」：有效目标集、轮转配额、账号评分窗口。
// 手工模式（TargetMode=manual / AccountMode=dedicated）完全沿用原有语义。

type probeAccountOutcome struct {
	At      int64 `json:"at"`
	Success bool  `json:"ok,omitempty"`
	Risk    bool  `json:"risk,omitempty"`
}

// probeAccountWindow 是单个账号的有界近期结果窗口（最近 20 条、24 小时）。
// 只用于挑选探测账号的启发式排序：重启丢失只会回到中性，不参与任何删除证据。
type probeAccountWindow struct {
	Entries    []probeAccountOutcome `json:"entries,omitempty"`
	LastPickAt int64                 `json:"last_pick,omitempty"`
	LastAt     int64                 `json:"last_at,omitempty"`
}

type probeAccountScore struct {
	Tier        int
	Rate        float64
	Success     int
	Total       int
	LastSuccess int64
}

// score 汇总窗口内的近期表现。分层（越小越优先）：
// 0 有成功且最近一次不是风控；1 有成功但最近一次是风控；2 无样本；3 只有失败。
func (w *probeAccountWindow) score(now time.Time) probeAccountScore {
	out := probeAccountScore{Tier: 2}
	if w == nil {
		return out
	}
	cutoff := now.Add(-probeAccountWindowTTL).UnixMilli()
	var last probeAccountOutcome
	lastSet := false
	for _, e := range w.Entries {
		if e.At < cutoff || e.At > now.UnixMilli() {
			continue
		}
		out.Total++
		if e.Success {
			out.Success++
			if e.At > out.LastSuccess {
				out.LastSuccess = e.At
			}
		}
		if !lastSet || e.At >= last.At {
			last, lastSet = e, true
		}
	}
	if out.Total == 0 {
		return out
	}
	if out.Success == 0 {
		out.Tier = 3
		return out
	}
	out.Rate = float64(out.Success) / float64(out.Total)
	out.Tier = 0
	if last.Risk {
		out.Tier = 1
	}
	return out
}

// migrateAutoHealthPolicy 把 v1 策略补齐为 v2：既有部署保留手工选择语义并提示可切换，
// 避免升级瞬间扩大真实模型请求范围；全新安装默认自动全覆盖与账号自动择优。
func (s *Store) migrateAutoHealthPolicy(existing bool) {
	p := &s.modelProbe.Policy
	if p.HealthVersion >= 2 {
		return
	}
	legacy := p.TargetMode == "" || p.AccountMode == ""
	if p.TargetMode == "" {
		p.TargetMode = "auto"
		if existing {
			p.TargetMode = "manual"
		}
	}
	if p.AccountMode == "" {
		p.AccountMode = "auto"
		if existing {
			p.AccountMode = "dedicated"
		}
	}
	p.HealthVersion = 2
	if existing && legacy && p.Enabled && p.MigrationNotice == "" {
		p.MigrationNotice = "已保留原有手工选择的目标与探测账号；可切换为自动全覆盖与账号自动择优。"
	}
}

// effectiveProbeTargetsLocked 计算当前有效的目标线路集合：
// 自动模式动态覆盖全部启用且未被严格禁止、未排除的线路；手工模式沿用所选列表。
func (s *Store) effectiveProbeTargetsLocked(p ModelProbeSettings) []ProxyProfile {
	out := []ProxyProfile{}
	if p.TargetMode == "auto" {
		for _, v := range s.listProxyProfilesLocked() {
			if v.Enabled && !s.strictProxyBlockedLocked(v.URL) && !slices.Contains(p.ExcludeProxyIDs, v.ID) {
				out = append(out, v)
			}
		}
		return out
	}
	for _, id := range p.TargetProxyIDs {
		if v, ok := s.probeProfileLocked(id); ok {
			out = append(out, v)
		}
	}
	return out
}

// probeRotationLocked 返回每轮配额与单线检测周期。
// 自动模式把「预算 × 间隔 / 一天」摊到每个轮转窗口：n 条线路需要 ceil(n/quota) 轮，
// 单线周期相应拉长为 间隔 × 轮数；手工模式保持逐目标间隔不变。
func (s *Store) probeRotationLocked(p ModelProbeSettings, n int) (int, time.Duration) {
	interval := time.Duration(p.IntervalMinutes) * time.Minute
	if p.TargetMode != "auto" || n == 0 {
		return max(1, n), interval
	}
	quota := p.GlobalDailyLimit * p.IntervalMinutes / 1440
	if quota < 1 {
		quota = 1
	}
	rounds := (n + quota - 1) / quota
	return quota, interval * time.Duration(rounds)
}

// scheduledProbeCountLocked 统计当前轮转窗口内已入队的自动巡检次数（含被免测跳过的）。
func (s *Store) scheduledProbeCountLocked(windowStart time.Time) int {
	count := 0
	for _, r := range s.modelProbe.runs {
		if r.Source == "scheduled" && r.CreatedAt >= windowStart.UnixMilli() {
			count++
		}
	}
	return count
}

// modelProbeTargetAllowedLocked 判断目标是否允许入队（模式 + 排除名单 + 线路状态）。
func (s *Store) modelProbeTargetAllowedLocked(p ModelProbeSettings, id string) bool {
	if p.TargetMode == "auto" {
		if slices.Contains(p.ExcludeProxyIDs, id) {
			return false
		}
	} else if !slices.Contains(p.TargetProxyIDs, id) {
		return false
	}
	_, ok := s.probeProfileLocked(id)
	return ok
}

// probeAccountEligibilityLocked 返回账号可否充当探测账号；不合格时给出可展示的原因。
func (s *Store) probeAccountEligibilityLocked(id string, now time.Time) (string, bool) {
	a := s.snapshotLocked(s.findAnyLocked(id))
	if a == nil {
		return "missing", false
	}
	if s.initializationManagedLocked(id) {
		return "initialization_managed", false
	}
	if a.Mode != "jwt" || a.Secret() == "" {
		return "not_jwt", false
	}
	a.ModelProbeOnly = false
	if !a.IsModelSelectable(ModelProbeModel, now) {
		return "not_selectable", false
	}
	if a.InRiskControlSilentWindow(now, config.RiskSilentWindow()) {
		return "risk_isolation", false
	}
	available, known := a.UsableQuotaForModel(ModelProbeModel)
	if !known {
		return "quota_unknown", false
	}
	if available <= 0 {
		return "quota_empty", false
	}
	return "", true
}

// recordProbeAccountOutcomeLocked 记录一次账号结果（业务观测或探测结果）。
func (s *Store) recordProbeAccountOutcomeLocked(id string, at time.Time, success, risk bool) {
	if id == "" {
		return
	}
	w := s.probeAccountWindowLocked(id)
	w.Entries = append(w.Entries, probeAccountOutcome{At: at.UnixMilli(), Success: success, Risk: risk})
	if len(w.Entries) > probeAccountWindowEntries {
		w.Entries = append([]probeAccountOutcome{}, w.Entries[len(w.Entries)-probeAccountWindowEntries:]...)
	}
	w.LastAt = at.UnixMilli()
}

// touchProbeAccountPickLocked 记录账号最近一次被选为探测主账号的时间，用于并列轮转。
func (s *Store) touchProbeAccountPickLocked(id string, now time.Time) {
	if id == "" {
		return
	}
	w := s.probeAccountWindowLocked(id)
	w.LastPickAt = now.UnixMilli()
}

func (s *Store) probeAccountWindowLocked(id string) *probeAccountWindow {
	if s.modelProbe.AccountWindows == nil {
		s.modelProbe.AccountWindows = map[string]*probeAccountWindow{}
	}
	w := s.modelProbe.AccountWindows[id]
	if w == nil {
		w = &probeAccountWindow{}
		s.modelProbe.AccountWindows[id] = w
	}
	return w
}

// probeAccountCandidatesLocked 返回探测账号候选顺序：
// 自动模式按近期成功率评分排序；手工模式返回专用集合的原始顺序（保持既有行为）。
func (s *Store) probeAccountCandidatesLocked(p ModelProbeSettings, now time.Time) []string {
	if p.AccountMode != "auto" {
		return p.AccountIDs
	}
	type row struct {
		id       string
		score    probeAccountScore
		lastPick int64
	}
	rows := []row{}
	for _, a := range s.allAccountsLocked() {
		if _, ok := s.probeAccountEligibilityLocked(a.ID, now); !ok {
			continue
		}
		lastPick := int64(0)
		if w := s.modelProbe.AccountWindows[a.ID]; w != nil {
			lastPick = w.LastPickAt
		}
		rows = append(rows, row{id: a.ID, score: s.modelProbe.AccountWindows[a.ID].score(now), lastPick: lastPick})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].score.Tier != rows[j].score.Tier {
			return rows[i].score.Tier < rows[j].score.Tier
		}
		if rows[i].score.Rate != rows[j].score.Rate {
			return rows[i].score.Rate > rows[j].score.Rate
		}
		if rows[i].lastPick != rows[j].lastPick {
			return rows[i].lastPick < rows[j].lastPick
		}
		if rows[i].score.LastSuccess != rows[j].score.LastSuccess {
			return rows[i].score.LastSuccess > rows[j].score.LastSuccess
		}
		return rows[i].id < rows[j].id
	})
	out := make([]string, 0, len(rows))
	for _, v := range rows {
		out = append(out, v.id)
	}
	return out
}

// probeAccountViewsLocked 为管理端给出自动择优的可解释视图：合格账号按评分排序并标注名次，
// 不合格账号附原因；只展示有限条数，避免大账号池拖垮摘要响应。
func (s *Store) probeAccountViewsLocked(now time.Time) []ModelProbeAccountView {
	out := []ModelProbeAccountView{}
	if s.modelProbe.Policy.AccountMode != "auto" {
		return out
	}
	type row struct {
		view     ModelProbeAccountView
		score    probeAccountScore
		lastPick int64
	}
	eligible := []row{}
	ineligible := []ModelProbeAccountView{}
	for _, a := range s.allAccountsLocked() {
		view := ModelProbeAccountView{ID: a.ID, Name: a.Name}
		w := s.modelProbe.AccountWindows[a.ID]
		if w != nil {
			view.LastPickAt = w.LastPickAt
		}
		score := w.score(now)
		view.RecentSuccess, view.RecentTotal, view.LastSuccessAt = score.Success, score.Total, score.LastSuccess
		reason, ok := s.probeAccountEligibilityLocked(a.ID, now)
		if !ok {
			view.Reason = reason
			ineligible = append(ineligible, view)
			continue
		}
		view.Eligible = true
		eligible = append(eligible, row{view: view, score: score, lastPick: view.LastPickAt})
	}
	sort.Slice(eligible, func(i, j int) bool {
		left, right := eligible[i], eligible[j]
		if left.score.Tier != right.score.Tier {
			return left.score.Tier < right.score.Tier
		}
		if left.score.Rate != right.score.Rate {
			return left.score.Rate > right.score.Rate
		}
		if left.lastPick != right.lastPick {
			return left.lastPick < right.lastPick
		}
		if left.score.LastSuccess != right.score.LastSuccess {
			return left.score.LastSuccess > right.score.LastSuccess
		}
		return left.view.ID < right.view.ID
	})
	for i := range eligible {
		eligible[i].view.Rank = i + 1
		out = append(out, eligible[i].view)
	}
	sort.Slice(ineligible, func(i, j int) bool { return ineligible[i].ID < ineligible[j].ID })
	out = append(out, ineligible...)
	if len(out) > probeAccountViewLimit {
		out = out[:probeAccountViewLimit]
	}
	return out
}

// scheduleAutoRotationLocked 自动覆盖模式的轮转调度：每个轮转窗口最多入队 quota 条，
// 到期线路按最久未测优先补齐；业务 405 提示触发的诊断不受配额限制（仍受总预算限制）。
func (s *Store) scheduleAutoRotationLocked(p ModelProbeSettings, targets []ProxyProfile, now time.Time) error {
	changed := false
	valid := map[string]bool{}
	for _, t := range targets {
		valid[t.ID] = true
	}
	// 已不在有效集合中的到期状态及时清理，保持状态有界。
	for id := range s.modelProbe.NextDue {
		if !valid[id] {
			delete(s.modelProbe.NextDue, id)
			changed = true
		}
	}
	if len(targets) == 0 {
		if changed {
			return s.persistProbeStateLocked()
		}
		return nil
	}
	quota, effInterval := s.probeRotationLocked(p, len(targets))
	windowStart := now.Add(-time.Duration(now.UnixMilli()%effInterval.Milliseconds()) * time.Millisecond)
	used := s.scheduledProbeCountLocked(windowStart)
	due := []ProxyProfile{}
	for _, t := range targets {
		dueAt := s.modelProbe.NextDue[t.ID]
		if dueAt == 0 {
			// 新纳入的线路立即可轮转，由窗口配额控制节奏。
			s.modelProbe.NextDue[t.ID] = now.UnixMilli()
			dueAt = now.UnixMilli()
			changed = true
		}
		hint := s.modelProbe.hints[t.ID]
		hintDue := !hint.IsZero() && now.UnixMilli() >= s.modelProbe.LastDiagnosis[t.ID]+int64(time.Hour/time.Millisecond)
		if dueAt <= now.UnixMilli() || hintDue {
			due = append(due, t)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		left, right := s.modelProbe.NextDue[due[i].ID], s.modelProbe.NextDue[due[j].ID]
		if left != right {
			return left < right
		}
		return due[i].ID < due[j].ID
	})
	for _, t := range due {
		hint := s.modelProbe.hints[t.ID]
		hintDue := !hint.IsZero() && now.UnixMilli() >= s.modelProbe.LastDiagnosis[t.ID]+int64(time.Hour/time.Millisecond)
		if !hintDue && used >= quota {
			continue
		}
		if _, err := s.queueModelProbeLocked(t.ID, "scheduled", now); err != nil && !errors.Is(err, ErrModelProbeUnavailable) {
			return err
		}
		used = s.scheduledProbeCountLocked(windowStart)
		// 确定性错峰；下界不低于一个窗口，避免同一窗口内重复入队。
		seed := strictDigest(t.ID, stringID(int(now.Unix()/60)))
		factor := 100 + int(seed[0])%21
		s.modelProbe.NextDue[t.ID] = now.Add(effInterval * time.Duration(factor) / 100).UnixMilli()
		delete(s.modelProbe.hints, t.ID)
		changed = true
	}
	if changed {
		return s.persistProbeStateLocked()
	}
	return nil
}
