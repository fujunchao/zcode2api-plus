package modelprobe

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

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
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(targets, "\n"))))
}

func (m *Manager) checkNetwork(ctx context.Context, p store.ProxyProfile, policy store.ModelProbeSettings, targets []string) networkprobe.Report {
	client, err := networkprobe.Client(p.URL)
	if err != nil {
		return networkprobe.Report{Error: "invalid_proxy", Targets: []networkprobe.Target{}}
	}
	return networkprobe.Check(ctx, m.store.GuardNetworkHealthClient(client, policy, p), targets)
}

func (m *Manager) networkBatch(ctx context.Context, profiles []store.ProxyProfile, p store.ModelProbeSettings, targets []string) map[string]networkprobe.Report {
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
				return m.checkNetwork(ctx, profile, p, targets)
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
		m.netNext = now.Add(time.Duration(p.NetworkIntervalMinutes) * time.Minute)
		if len(selected) == 0 {
			return false, nil
		}
		r := &networkRound{id: fmt.Sprintf("network-%d", now.UnixNano()), key: key, policy: p, profiles: selected, all: all, targets: targets, manual: manual, deadline: now.Add(5*time.Minute + time.Duration(p.NetworkConfirmSeconds)*time.Second)}
		r.first = m.networkBatch(ctx, selected, p, targets)
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
	second := m.networkBatch(ctx, append(slices.Clone(bad), controls...), p, r.targets)
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
		v := m.checkNetwork(ctx, store.ProxyProfile{}, p, r.targets)
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
	for i := range records {
		if slices.Contains(removed, records[i].ProfileID) {
			records[i].Removed = true
			records[i].State = "removed"
		}
	}
	if err := m.store.SaveNetworkHealth(p, records); err != nil {
		return true, err
	}
	web.Warn("proxy-health", fmt.Sprintf("round=%s stage=confirmed checked=%d candidates=%d removed=%d mode=%s", r.id, len(r.profiles), len(remove), len(removed), p.Mode))
	m.enqueueNetworkModels(r, records)
	return true, nil
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
	r := m.checkNetwork(ctx, profile, p, targets)
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
