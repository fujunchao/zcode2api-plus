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

// 策略v3统一自动巡检；保留开关、处置授权、预算与拓扑，不自动允许直连或删除。
func (s *Store) migrateAutoHealthPolicy(existing bool) {
	p := &s.modelProbe.Policy
	if p.HealthVersion >= 3 {
		return
	}
	p.TargetMode, p.AccountMode, p.HealthVersion = "auto", "auto", 3
	p.TargetProxyIDs, p.AccountIDs, p.ReferenceProxyIDs = []string{}, []string{}, []string{}
	s.modelProbe.NextDue = map[string]int64{}
	s.modelProbe.AccountWindows = map[string]*probeAccountWindow{}
	if existing {
		p.Revision++
		p.MigrationNotice += " 已升级为全节点自动巡检与账号自动择优，旧探测专用角色已释放；开关、预算和处置权限保留，近期统计按新口径重新积累。"
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
	return quota, probeDispatchWindow(p) * time.Duration(rounds)
}

func probeDispatchWindow(p ModelProbeSettings) time.Duration {
	// 小预算也按真实供给节拍推进，不能强行每分钟入队一次再等硬预算拒绝。
	limit := max(1, p.GlobalDailyLimit)
	minutes := max(p.IntervalMinutes, (1440+limit-1)/limit)
	return time.Duration(minutes) * time.Minute
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
	for id := range s.modelProbe.Checks {
		if !valid[id] {
			delete(s.modelProbe.Checks, id)
			changed = true
		}
	}
	if len(targets) == 0 {
		if changed {
			return s.persistProbeStateLocked()
		}
		return nil
	}
	quota, _ := s.probeRotationLocked(p, len(targets))
	windowStart := now.Truncate(probeDispatchWindow(p))
	used := s.scheduledProbeCountLocked(windowStart)
	open := map[string]bool{}
	for _, r := range s.modelProbe.runs {
		if probeRunOpen(r) && r.policy.Revision == p.Revision {
			open[r.TargetID] = true
		}
	}
	due := []ProxyProfile{}
	for _, t := range targets {
		if open[t.ID] {
			continue
		}
		dueAt := s.modelProbe.NextDue[t.ID]
		if c, ok := s.modelProbe.Checks[t.ID]; ok && (c.Generation != t.Generation || c.Identity != strictProxyIdentity(t.URL) || c.Target != strictDigest(config.UpstreamZai)) {
			delete(s.modelProbe.Checks, t.ID)
			dueAt = 0
		}
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
		source := "scheduled"
		if hintDue {
			source = "hint"
		}
		if _, err := s.queueModelProbeLocked(t.ID, source, now); err != nil && !errors.Is(err, ErrModelProbeUnavailable) {
			return err
		}
		used = s.scheduledProbeCountLocked(windowStart)
		// 入队不等于已检查：真实目标请求完成后才推进NextDue；未发送的任务保留待检。
		delete(s.modelProbe.hints, t.ID)
		changed = true
	}
	if changed {
		return s.persistProbeStateLocked()
	}
	return nil
}
