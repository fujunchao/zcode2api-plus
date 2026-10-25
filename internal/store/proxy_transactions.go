package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/proxysub"
	"zcode2api/internal/web"
)

// commitProxyStateLocked 将线路与关联账号作为一个事务提交，再发布内存状态。
// profiles=nil 表示本次只修改账号指派；pending 都是尚未发布的副本。
func (s *Store) commitProxyStateLocked(profiles []ProxyProfile, pending []*model.Account) error {
	return s.commitProxyStateWithHistoryLocked(profiles, pending, nil)
}

// history 非 nil 时和线路改派一起落库；失败时两者均不发布到内存。
func (s *Store) commitProxyStateWithHistoryLocked(profiles []ProxyProfile, pending []*model.Account, history riskProxyHistory, reasons ...string) error {
	var sub *proxysub.State
	var err error
	if profiles != nil {
		reason := "automatic"
		if len(reasons) > 0 {
			reason = reasons[0]
		}
		sub, err = s.subscriptionMutationLocked(profiles, reason)
		if err != nil {
			return err
		}
	}
	return s.commitProxyBundleLocked(context.Background(), profiles, pending, history, sub)
}

// 已退役且不再被任何线路/账号引用的端点才释放缓存；已有流不会被关闭。
func (s *Store) retireProxyTransportsLocked(oldProfiles, profiles []ProxyProfile) {
	used := map[string]bool{}
	for _, p := range profiles {
		used[p.URL] = true
	}
	for _, a := range s.allAccountsLocked() {
		if a.ProxyURL != nil && !s.strictProxyBlockedLocked(*a.ProxyURL) {
			used[*a.ProxyURL] = true
		}
	}
	for _, p := range oldProfiles {
		if !used[p.URL] {
			proxy.ForgetTransport(p.URL)
		}
	}
}

// removeProxiesLocked 在副本上统一按空闲优先、最少绑定计算改派。
// excludedURL 用于空额度/端点故障清理时避开相同出口；所有删除入口共用同一选线策略。
func (s *Store) removeProxiesLocked(ids map[string]bool, excludedURL string, reasons ...string) ([]string, ProxyReassign, error) {
	excluded := map[string]bool{}
	if excludedURL != "" {
		excluded[excludedURL] = true
	}
	return s.removeProxiesAvoidingLocked(ids, excluded, reasons...)
}

// PurgeProxyProfilesIfCurrent 供巡检使用：失败证据和删除检查在同一配置代次上。
// 与只接收 ID 的管理删除不同，旧 URL/旧启停代次的探测绝不能误删新配置。
func (s *Store) PurgeProxyProfilesIfCurrent(expected []ProxyProfile) ([]string, ProxyReassign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.purgeProxyProfilesIfCurrentLocked(expected)
}

func (s *Store) purgeProxyProfilesIfCurrentLocked(expected []ProxyProfile) ([]string, ProxyReassign, error) {
	wanted := map[string]bool{}
	for _, p := range s.listProxyProfilesLocked() {
		for _, snapshot := range expected {
			if p.ID == snapshot.ID && p.Enabled && snapshot.Enabled && p.Generation == snapshot.Generation &&
				strictProxyIdentity(p.URL) == strictProxyIdentity(snapshot.URL) {
				wanted[p.ID] = true
				break
			}
		}
	}
	// 巡检专用：最近成功保护窗口（ProxyProtectRecentHours）内有过成功交付的线路
	// 不进删除名单。只影响这一条删除路径——严格退役、断流熔断、登录清理与手动
	// 删除都不看它。
	if recent := s.recentSuccessProtectionsLocked(time.Now()); len(recent) > 0 {
		for id := range wanted {
			p, ok := s.probeProfileLocked(id)
			if !ok {
				continue
			}
			if at, hit := recent[strictProxyIdentity(p.URL)]; hit {
				delete(wanted, id)
				s.logRecentProtectLocked(id, at, time.Now())
			}
		}
	}
	return s.removeProxiesLocked(wanted, "")
}

func (s *Store) removeProxiesAvoidingLocked(ids map[string]bool, excluded map[string]bool, reasons ...string) ([]string, ProxyReassign, error) {
	result := ProxyReassign{Assigned: map[string]string{}}
	manual := len(reasons) > 0 && reasons[0] == "manual"
	if !manual && s.modelProbe.Policy.Mode != "auto_retire" {
		return nil, result, nil
	}
	protected := s.proxyAutoDeleteProtectionsLocked()
	// 只处理真正允许删除的 ID；后面的账号解绑不能继续使用未过滤的原集合。
	allowed := map[string]bool{}
	remaining := []ProxyProfile{}
	var removed []string
	for _, p := range s.listProxyProfilesLocked() {
		if ids[p.ID] && (manual || protected[strictProxyIdentity(p.URL)] == "") {
			removed = append(removed, p.ID)
			allowed[p.ID] = true
		} else {
			remaining = append(remaining, p)
		}
	}
	if len(removed) == 0 {
		return nil, result, nil
	}
	assignable := proxyProfilesExcluding(remaining, excluded)
	result, pending, next := s.reassignAccountsLocked(func(a *model.Account) bool {
		return a.ProxyID != nil && allowed[*a.ProxyID]
	}, assignable, excluded)
	reason := "automatic"
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	sub, err := s.subscriptionMutationLocked(remaining, reason)
	if err != nil {
		return nil, ProxyReassign{Assigned: map[string]string{}}, err
	}
	if err := s.commitProxyBundleStrictLocked(context.Background(), remaining, pending, nil, sub, next); err != nil {
		return nil, ProxyReassign{Assigned: map[string]string{}}, err
	}
	for _, id := range removed {
		s.clearLineTruncateLocked(id)
	}
	return removed, result, nil
}

// reassignAccountsLocked 把满足 match 的账号改派到 assignable（空闲优先、其次绑定最少）。
// 删除与软失败隔离两个入口共用同一段改派逻辑，避免出现两套选线策略。
// 返回的 pending 是尚未提交的账号副本，由调用方决定与哪些数据一起落库。
func (s *Store) reassignAccountsLocked(match func(*model.Account) bool, assignable []ProxyProfile, excluded map[string]bool) (ProxyReassign, []*model.Account, *strictProxyState) {
	result := ProxyReassign{Assigned: map[string]string{}}
	occupancy := s.proxyOccupancyLocked()
	var pending []*model.Account
	next := s.cloneStrictProxyLocked()
	for _, a := range s.allAccountsLocked() {
		if !match(a) {
			continue
		}
		copy := a.Clone()
		copy.ProxyID, copy.ProxyURL = nil, nil
		pending = append(pending, copy)
	}
	for _, a := range pending {
		if s.initializationManagedLocked(a.ID) {
			intent := initializationIntent
			if s.initializations[a.ID].GrantedAt > 0 {
				intent = fixedProxyIntent
			}
			setStrictIntent(&next, a, intent)
			result.Waiting = append(result.Waiting, a.ID)
			continue
		}
		p, ok := s.chooseProxyLocked(assignable, occupancy, "", "")
		if !ok {
			// 没有可用替代：保留「待分配」意图而不是落到直连——直连是风控风险最高的选择。
			result.Direct = append(result.Direct, a.ID)
			setStrictIntent(&next, a, "auto_pending_proxy")
			for raw, skip := range excluded {
				if skip {
					next.PendingAvoid[a.ID] = append(next.PendingAvoid[a.ID], strictProxyIdentity(raw))
				}
			}
			continue
		}
		id, url := p.ID, p.URL
		a.ProxyID, a.ProxyURL = &id, &url
		occupancy[id]++
		result.Assigned[a.ID] = id
		setStrictIntent(&next, a, "auto_proxy")
	}
	if len(result.Assigned) > 0 {
		// 落点分布是复盘「删线后账号都压到哪条线上」的唯一证据；只记计数看不出集中度。
		web.Warn("proxy-reassign", fmt.Sprintf("assigned=%d top_targets=%s", len(result.Assigned), reassignTargets(result.Assigned, 3)))
	}
	return result, pending, &next
}

// reassignTargets 汇总改派落点（accountID → profileID），返回 "p1:6,p2:4,+2" 形式。
// 按绑定数降序、同数按 ID 升序，保证日志可对比；top<=0 时输出全部。
func reassignTargets(assigned map[string]string, top int) string {
	if len(assigned) == 0 {
		return ""
	}
	counts := map[string]int{}
	for _, id := range assigned {
		counts[id]++
	}
	type target struct {
		id string
		n  int
	}
	list := make([]target, 0, len(counts))
	for id, n := range counts {
		list = append(list, target{id, n})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].id < list[j].id
	})
	var b strings.Builder
	for i, t := range list {
		if top > 0 && i == top {
			fmt.Fprintf(&b, ",+%d", len(list)-top)
			break
		}
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%s:%d", t.id, t.n)
	}
	return b.String()
}

// reassignOffQuarantineLocked 把绑定在隔离线路上的账号改派走：不改线路配置、不删任何东西。
// 没有可用替代时保持原绑定（宁可继续用一条慢线路，也不掉直连）。
func (s *Store) reassignOffQuarantineLocked(quarantined map[string]bool) (ProxyReassign, error) {
	if len(quarantined) == 0 {
		return ProxyReassign{Assigned: map[string]string{}}, nil
	}
	result, pending, next := s.reassignAccountsLocked(func(a *model.Account) bool {
		if a.ProxyID == nil || *a.ProxyID == "" {
			return false // 裸 URL 是人工选定的出口，不自动改派。
		}
		url := derefStr(a.ProxyURL)
		return url != "" && quarantined[strictProxyIdentity(url)]
	}, s.listProxyProfilesLocked(), nil)
	if len(pending) == 0 {
		return result, nil
	}
	if err := s.commitProxyBundleStrictLocked(context.Background(), nil, pending, nil, nil, next); err != nil {
		return ProxyReassign{Assigned: map[string]string{}}, err
	}
	return result, nil
}
