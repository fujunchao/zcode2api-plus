package modelprobe

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/networkprobe"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

type networkRound struct {
	id, key             string
	policy              store.ModelProbeSettings
	profiles, all       []store.ProxyProfile
	targets             []string
	first               map[string]networkprobe.Report
	confirmAt, deadline time.Time
	manual              map[string]bool
	whole               bool
}

// NewWithNetworkTargets 只替换无凭据目标来源；测试仍使用真实 HTTP 和相同状态机。
func NewWithNetworkTargets(ctx context.Context, st *store.Store, targets func() []string, clock ...func() time.Time) *Manager {
	m := New(ctx, st, clock...)
	m.networkTargets = targets
	return m
}

func (m *Manager) QueueNetwork(id string) error {
	return m.queueNetwork(id, false)
}

func (m *Manager) queueNetwork(id string, manual bool) error {
	if m.ctx.Err() != nil {
		return m.ctx.Err()
	}
	p := m.store.ModelProbeSettings()
	if !p.NetworkEnabled {
		return store.ErrModelProbeUnavailable
	}
	if id != "" {
		found := false
		for _, v := range m.store.ListProxyProfiles() {
			if v.ID == id && v.Enabled {
				found = true
				break
			}
		}
		if !found {
			return store.ErrProxyNotFound
		}
	}
	m.netMu.Lock()
	defer m.netMu.Unlock()
	if m.netPending == nil || m.netPendingRevision != p.Revision {
		m.netPending = map[string]bool{}
	}
	m.netPendingRevision = p.Revision
	if m.netRound != nil {
		for _, v := range m.netRound.profiles {
			if v.ID == id {
				m.netRound.manual[id] = m.netRound.manual[id] || manual
				return nil
			}
		}
	}
	m.netPending[id] = m.netPending[id] || manual
	return nil
}

func (m *Manager) RequestHealth(id string) error {
	if err := m.queueNetwork(id, true); err != nil {
		return err
	}
	m.Start()
	return nil
}

func networkTargetsKey(targets []string) string {
	return networkprobe.TargetsKey(targets)
}

func (m *Manager) checkNetwork(ctx context.Context, p store.ProxyProfile, policy store.ModelProbeSettings, targets []string, deep bool) networkprobe.Report {
	client, err := networkprobe.Client(p.URL)
	if err != nil {
		return networkprobe.Report{Error: "invalid_proxy", Targets: []networkprobe.Target{}}
	}
	report := networkprobe.Check(ctx, m.store.GuardNetworkHealthClient(client, policy, p), targets)
	return withDeepProbe(ctx, p, report, deep)
}

// withDeepProbe 只在巡检 first 阶段跑隧道裸写。
//
// 为什么不在 second / direct / preflight 跑：
//   - second 只是对失败线路复核可达性，first 的隧道结果已经入库；
//   - direct 没有代理 URL，隧道无意义；
//   - preflight 每个模型探测租约都会跑，在那里写 1MiB 会把流量与特征放大到每次租约。
//
// shadow 只记录、不改 OK；enforce 才把隧道失败并入判定。
func withDeepProbe(ctx context.Context, p store.ProxyProfile, report networkprobe.Report, deep bool) networkprobe.Report {
	mode := config.DeepProbeMode()
	if !deep || mode == "off" || p.URL == "" {
		return report
	}
	t := networkprobe.CheckTunnelWrite(ctx, p.URL, config.DeepTarget, config.DeepBytes, config.DeepBudget, config.DeepMinBytes, config.DeepMinKbps)
	report.Tunnel = &t
	if mode == "enforce" {
		report.OK = report.OK && t.OK
	}
	web.Warn("proxy-deep", fmt.Sprintf("id=%s mode=%s ok=%t bytes=%d ms=%d kbps=%d target=%s early_stop=%t",
		p.ID, mode, t.OK, t.Bytes, t.MS, t.Kbps, config.DeepTarget, t.EarlyStop))
	return report
}

func (m *Manager) networkBatch(ctx context.Context, profiles []store.ProxyProfile, p store.ModelProbeSettings, targets []string, deep bool) map[string]networkprobe.Report {
	result := map[string]networkprobe.Report{}
	var lock sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, profile := range profiles {
		wg.Add(1)
		go func(profile store.ProxyProfile) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			r := func() (r networkprobe.Report) {
				defer func() {
					if recover() != nil {
						r = networkprobe.Report{Error: "internal_error"}
					}
				}()
				return m.checkNetwork(ctx, profile, p, targets, deep)
			}()
			lock.Lock()
			result[profile.ID] = r
			lock.Unlock()
		}(profile)
	}
	wg.Wait()
	return result
}

func independentNetwork(p store.ModelProbeSettings, a, b store.ProxyProfile) bool {
	if a.ID == b.ID {
		return false
	}
	ca, _ := proxy.CanonicalRoute(a.URL)
	cb, _ := proxy.CanonicalRoute(b.URL)
	if ca == cb {
		return false
	}
	bID := b.ID
	if bID == "" {
		bID = store.ModelProbeDirect
	}
	return p.EgressGroups[a.ID] == "" || p.EgressGroups[bID] == "" || p.EgressGroups[a.ID] != p.EgressGroups[bID]
}

// RunNetworkDue 是统一管理器的网络阶段，不需要账号；首轮绝不删除。
func (m *Manager) RunNetworkDue(parent context.Context) (worked bool, err error) {
	if !m.netMu.TryLock() {
		return false, nil
	}
	defer m.netMu.Unlock()
	defer func() {
		if recover() != nil {
			m.netRound = nil
			err = fmt.Errorf("网络巡检阶段异常，未提交删除")
		}
	}()
	if m.ctx.Err() != nil {
		return false, m.ctx.Err()
	}
	p := m.store.ModelProbeSettings()
	now := m.now()
	targets := slices.Clone(m.networkTargets())
	key := networkTargetsKey(targets)
	if !p.NetworkEnabled {
		m.netRound = nil
		m.netPending = nil
		return false, nil
	}
	if m.netRevision != p.Revision {
		m.netRound = nil
		m.netRevision = p.Revision
		m.netNext = now.Add(time.Duration(p.NetworkIntervalMinutes) * time.Minute)
		if m.netPendingRevision != p.Revision {
			m.netPending = map[string]bool{}
		}
	}
	if m.netRound != nil && (m.netRound.policy.Revision != p.Revision || m.netRound.key != key || !now.Before(m.netRound.deadline)) {
		m.netRound = nil
		return false, nil
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer cancel()
	if m.netRound == nil {
		// 计费异常仅触发新的无凭据网络检查；不把它当成已失败的首轮，也不续跑模型。
		for _, profile := range m.store.TakeProxyNetworkRechecks(p, now) {
			if _, pending := m.netPending[profile.ID]; !pending {
				m.netPending[profile.ID] = false
			}
		}
		if len(m.netPending) == 0 && now.Before(m.netNext) {
			return false, nil
		}
		all := []store.ProxyProfile{}
		selected := []store.ProxyProfile{}
		_, allPending := m.netPending[""]
		whole := !now.Before(m.netNext) || allPending
		for _, v := range m.store.ListProxyProfiles() {
			if v.Enabled {
				all = append(all, v)
				_, pending := m.netPending[v.ID]
				if whole || pending {
					selected = append(selected, v)
				}
			}
		}
		manual := m.netPending
		m.netPending = map[string]bool{}
		// 三个入口各六秒，48 条的两次检查及参考复核可在五分钟批次预算内完成。
		// 大池拆分后立即处理剩余批次，不因重新起轮一直只检查列表开头。
		if len(selected) > 48 {
			for _, v := range selected[48:] {
				m.netPending[v.ID] = manual[v.ID]
			}
			selected = selected[:48]
		}
		// 只有全量轮推进定时器。此前每一轮（含只查一条线路的故障复检）都会把
		// netNext 顺延一个间隔，活跃期复检间隔中位数十来秒，30 分钟的全量轮
		// 实际永远等不到——空闲线路的坏点因此从未被扫到。
		if whole {
			m.netNext = now.Add(time.Duration(p.NetworkIntervalMinutes) * time.Minute)
		}
		if len(selected) == 0 {
			return false, nil
		}
		r := &networkRound{id: fmt.Sprintf("network-%d", now.UnixNano()), key: key, policy: p, profiles: selected, all: all, targets: targets, manual: manual, whole: whole, deadline: now.Add(5*time.Minute + time.Duration(p.NetworkConfirmSeconds)*time.Second)}
		if whole {
			web.Warn("proxy-health", fmt.Sprintf("round=%s stage=probe whole=1 checked=%d mode=%s", r.id, len(r.profiles), p.Mode))
		}
		r.first = m.networkBatch(ctx, selected, p, targets, true)
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		r.confirmAt = m.now().Add(time.Duration(p.NetworkConfirmSeconds) * time.Second)
		records := []store.NetworkHealthRecord{}
		bad := false
		for _, v := range selected {
			first := r.first[v.ID]
			state := "healthy"
			at := int64(0)
			if !first.OK {
				state = "waiting"
				bad = true
				at = r.confirmAt.UnixMilli()
			}
			records = append(records, store.NetworkHealthRecord{ProfileID: v.ID, Generation: v.Generation, PolicyRevision: p.Revision, State: state, At: m.now().UnixMilli(), RecheckAt: at, RoundID: r.id, TargetsKey: key, First: first})
		}
		if err := m.store.SaveNetworkHealth(p, records); err != nil {
			return true, err
		}
		if bad {
			m.netRound = r
		} else {
			m.enqueueNetworkModels(r, records)
		}
		return true, nil
	}
	r := m.netRound
	if now.Before(r.confirmAt) {
		return false, nil
	}
	defer func() { m.netRound = nil }()
	bad := []store.ProxyProfile{}
	controls := []store.ProxyProfile{}
	for _, v := range r.profiles {
		if !r.first[v.ID].OK {
			bad = append(bad, v)
		}
	}
	for _, v := range r.all {
		isBad := false
		for _, b := range bad {
			if v.ID == b.ID {
				isBad = true
			}
		}
		if !isBad {
			controls = append(controls, v)
			if len(controls) == 2 {
				break
			}
		}
	}
	second := m.networkBatch(ctx, append(slices.Clone(bad), controls...), p, r.targets, false)
	var direct *networkprobe.Report
	needsDirect := false
	for _, b := range bad {
		if second[b.ID].OK {
			continue
		}
		good := false
		for _, c := range append(slices.Clone(controls), bad...) {
			if second[c.ID].OK && independentNetwork(p, b, c) {
				good = true
			}
		}
		if !good {
			needsDirect = true
		}
	}
	if needsDirect {
		v := m.checkNetwork(ctx, store.ProxyProfile{}, p, r.targets, false)
		direct = &v
	}
	if ctx.Err() != nil || !m.now().Before(r.deadline) {
		return true, context.Canceled
	}
	records := []store.NetworkHealthRecord{}
	remove := []store.ProxyProfile{}
	for _, v := range r.profiles {
		rec := store.NetworkHealthRecord{ProfileID: v.ID, Generation: v.Generation, PolicyRevision: p.Revision, State: "healthy", At: m.now().UnixMilli(), RoundID: r.id, TargetsKey: r.key, First: r.first[v.ID]}
		if !rec.First.OK {
			check := second[v.ID]
			rec.Second = &check
			rec.State = "recovered"
			if !check.OK {
				rec.State = "unconfirmed"
				for _, c := range append(slices.Clone(controls), bad...) {
					if second[c.ID].OK && independentNetwork(p, v, c) {
						cp := second[c.ID]
						rec.Control = &cp
						rec.ControlID = c.ID
						break
					}
				}
				if rec.Control == nil && direct != nil {
					rec.Control = direct
					rec.ControlID = store.ModelProbeDirect
				}
				confirmed := rec.Control != nil && rec.Control.OK && (rec.ControlID != store.ModelProbeDirect || independentNetwork(p, v, store.ProxyProfile{}))
				if confirmed && len(rec.First.Targets) > 0 && len(check.Targets) > 0 && rec.First.Error != "canceled" && check.Error != "canceled" {
					rec.State = "confirmed_failure"
					remove = append(remove, v)
				}
			}
		}
		records = append(records, rec)
	}
	// 全轮结束后才进入 Store 的同锁策略/代次检查，取消或改设置不会留下半轮删除。
	if err := m.store.SaveNetworkHealth(p, records); err != nil {
		return true, err
	}
	removed, _, err := m.store.ApplyNetworkHealth(ctx, p, remove)
	if err != nil {
		return true, err
	}
	protections := m.store.ProxyAutoDeleteProtections()
	for i := range records {
		if slices.Contains(removed, records[i].ProfileID) {
			records[i].Removed = true
			records[i].State = "removed"
		} else if records[i].State == "confirmed_failure" && protections[records[i].ProfileID] != "" {
			records[i].State = "protected_failure"
		}
	}
	if err := m.store.SaveNetworkHealth(p, records); err != nil {
		return true, err
	}
	line := fmt.Sprintf("round=%s stage=confirmed whole=%d checked=%d candidates=%d removed=%d mode=%s", r.id, boolInt(r.whole), len(r.profiles), len(remove), len(removed), p.Mode)
	if ids := removedIDsField(removed); ids != "" {
		line += " " + ids
	}
	web.Warn("proxy-health", line)
	m.enqueueNetworkModels(r, records)
	return true, nil
}

// removedIDsField 输出可核对的删除明细：最多 20 个 id，其余折成 +N。
//
// 只有 removed=14 这种计数，事后既无法核对删了哪条线，也无法复盘误删；而线路
// 身份又不在其它日志里逐条出现。上限与长度限制只是为了不让一行日志失控。
func removedIDsField(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	const (
		maxIDs   = 20
		maxBytes = 512
	)
	var b strings.Builder
	b.WriteString("removed_ids=")
	n := 0
	for _, id := range removed {
		if id == "" {
			continue
		}
		if n == maxIDs {
			break
		}
		add := id
		if n > 0 {
			add = "," + id
		}
		// 预留 "+NN" 的后缀空间，避免截断把数字截掉。
		if b.Len()+len(add)+4 > maxBytes {
			break
		}
		b.WriteString(add)
		n++
	}
	if extra := len(removed) - n; extra > 0 {
		fmt.Fprintf(&b, "+%d", extra)
	}
	return b.String()
}

func (m *Manager) enqueueNetworkModels(r *networkRound, records []store.NetworkHealthRecord) {
	if !r.policy.Enabled {
		return
	}
	for _, v := range records {
		// 目标资格（自动覆盖 / 手工选择 / 排除名单）由 Store 在入队时统一判定。
		if r.manual[v.ProfileID] && (v.State == "healthy" || v.State == "recovered") {
			_, _ = m.store.QueueModelProbe(v.ProfileID, "manual", m.now())
		}
	}
	m.Wake()
}

func (m *Manager) networkPreflight(ctx context.Context, l *store.ModelProbeLease) bool {
	p := m.store.ModelProbeSettings()
	if !p.NetworkEnabled {
		return true
	}
	profile := l.Profile()
	targets := m.networkTargets()
	key := networkTargetsKey(targets)
	if profile.ID != "" {
		records, err := m.store.NetworkHealth()
		if err != nil {
			return false
		}
		for _, r := range records {
			if store.NetworkHealthyAt(r, profile, p, key, m.now()) {
				return true
			}
		}
	}
	r := m.checkNetwork(ctx, profile, p, targets, false)
	if profile.ID != "" {
		state := "healthy"
		if !r.OK {
			state = "waiting"
		}
		if err := m.store.SaveNetworkHealth(p, []store.NetworkHealthRecord{{ProfileID: profile.ID, Generation: profile.Generation, PolicyRevision: p.Revision, State: state, At: m.now().UnixMilli(), TargetsKey: key, First: r}}); err != nil {
			return false
		}
		if !r.OK {
			_ = m.QueueNetwork(profile.ID)
		}
	}
	return r.OK
}

// boolInt 只用于日志字段，保持「whole=0/1」与既有数字型字段一致。
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
