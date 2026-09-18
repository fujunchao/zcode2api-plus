// 代理线路自动巡检：周期性对全部「启用」线路做 z.ai 可达性检测，检测不通过
// 的线路自动移除，其绑定账号由 Store.PurgeProxyProfiles 按「空閒优先、其次
// 绑定账号数最少」改派。
//
// 与手动检测（proxies.go 的 test-all）刻意共享同一套探测实现（probeProxy），
// 自动与手动的「可用」结论永远同口径；差别只在触发方式与后续处置。
package adminapi

import (
	"fmt"
	"sync"
	"time"

	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// tagProxyHealth 巡检日志的来源标识。
const tagProxyHealth = "proxy-health"

// ProxyHealthScheduler 代理线路自动巡检调度器。
type ProxyHealthScheduler struct {
	store *store.Store
	start sync.Once
	stop  chan struct{}
	done  chan struct{}
}

// NewProxyHealthScheduler 创建巡检调度器（调用 Start 启动）。
func NewProxyHealthScheduler(st *store.Store) *ProxyHealthScheduler {
	return &ProxyHealthScheduler{store: st, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动调度循环；重复调用无操作。
func (s *ProxyHealthScheduler) Start() {
	s.start.Do(func() { go s.loop() })
}

// Stop 停止调度循环并等待退出。进行中的一轮会尽快中止：未发出的探测不再
// 发起、已发出的等其自然超时返回；移除与改派只发生在整轮探测完成之后，
// 所以中止不会留下半轮变更。
func (s *ProxyHealthScheduler) Stop() {
	close(s.stop)
	<-s.done
}

// loop 调度循环：开关与间隔每轮实时读后台设置（缺省回退环境变量），
// 关掉即空转、调间隔下一轮生效，均无需重启进程。
func (s *ProxyHealthScheduler) loop() {
	defer close(s.done)
	// 启动先避让，与领取调度器/额度监控一致，让监听与账号加载先跑完。
	select {
	case <-s.stop:
		return
	case <-time.After(5 * time.Second):
	}
	for {
		interval := time.Duration(s.store.ProxyHealthIntervalMinutes()) * time.Minute
		select {
		case <-s.stop:
			return
		case <-time.After(interval):
		}
		if !s.store.ProxyHealthEnabled() {
			continue
		}
		// 单轮异常不该终结调度循环。
		func() {
			defer func() {
				if r := recover(); r != nil {
					web.Warn(tagProxyHealth, "自动巡检异常（已兜底）")
				}
			}()
			s.runOnce(s.stop)
		}()
	}
}

// lineProbe 单条线路的巡检结论。
type lineProbe struct {
	profile store.ProxyProfile
	ok      bool
	reason  string // ok=false 时给人看的原因
}

// probeLine 探测单条线路并整理成巡检结论。
func probeLine(p store.ProxyProfile) lineProbe {
	info, err := probeProxy(p.URL)
	if err != nil {
		// 客户端构造失败（协议不支持等）：线路同样不可用。
		return lineProbe{profile: p, reason: err.Error()}
	}
	if ok, _ := info["ok"].(bool); ok {
		return lineProbe{profile: p, ok: true}
	}
	reason, _ := info["error"].(string)
	if reason == "" {
		reason = "z.ai 无响应"
	}
	return lineProbe{profile: p, reason: reason}
}

// runOnce 执行一轮巡检：探测全部启用线路 → 判定 → 移除不可用线路并改派账号。
// abort 关闭时尽快中止本轮，且不落任何变更（整轮原子：探测全部完成后才动线路表）。
func (s *ProxyHealthScheduler) runOnce(abort <-chan struct{}) {
	started := time.Now()
	profiles := []store.ProxyProfile{}
	for _, p := range s.store.ListProxyProfiles() {
		if p.Enabled {
			profiles = append(profiles, p)
		}
	}
	if len(profiles) == 0 {
		web.Ok(tagProxyHealth, "自动巡检：无启用线路，跳过本轮")
		return
	}

	// 并发探测（与手动批量检测同一并发上限）。排队中的探测在停机时直接放弃，
	// 在途的等其自然超时（单线路上界 12s）。
	results := make([]lineProbe, len(profiles))
	sem := make(chan struct{}, testAllConcurrency)
	var wg sync.WaitGroup
spawn:
	for i, p := range profiles {
		select {
		case <-abort:
			break spawn
		default:
		}
		wg.Add(1)
		go func(i int, p store.ProxyProfile) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-abort:
				return
			}
			defer func() { <-sem }()
			select {
			case <-abort:
				return
			default:
			}
			results[i] = probeLine(p)
		}(i, p)
	}
	wg.Wait()
	select {
	case <-abort:
		web.Warn(tagProxyHealth, fmt.Sprintf("自动巡检中止：收到停机信号（应测 %d 条）", len(profiles)))
		return
	default:
	}

	failed := []lineProbe{}
	for _, r := range results {
		if !r.ok {
			failed = append(failed, r)
		}
	}
	dur := time.Since(started).Round(time.Millisecond)
	if len(failed) == 0 {
		web.Ok(tagProxyHealth, fmt.Sprintf(
			"自动巡检完成：%d/%d 条线路可用，无需移除（耗时 %s）", len(profiles), len(profiles), dur))
		return
	}
	for _, f := range failed {
		web.Warn(tagProxyHealth, fmt.Sprintf("线路不可用：%s（%s）：%s",
			f.profile.Name, proxy.MaskURL(f.profile.URL), f.reason))
	}

	// 全部线路不可用时先看直连：连直连都够不着 z.ai，多半是本机网络或 z.ai
	// 侧的故障——此刻移除会把整个线路池清空。宁可本轮空过，留给人来判断。
	if len(failed) == len(profiles) {
		direct, err := probeProxy("")
		if err != nil || direct["ok"] != true {
			why := "探测出错"
			if err != nil {
				why = err.Error()
			} else if msg, _ := direct["error"].(string); msg != "" {
				why = msg
			}
			web.Warn(tagProxyHealth, fmt.Sprintf(
				"全部 %d 条线路不可用，且直连 z.ai 也不可达（%s）——疑为本机网络故障，本轮不移除任何线路",
				len(profiles), why))
			return
		}
		web.Warn(tagProxyHealth, fmt.Sprintf(
			"全部 %d 条线路不可用，但直连 z.ai 可达——判定为线路自身失效，照常移除", len(profiles)))
	}

	// 探测期间线路表可能已被人工改动：只移除「现在仍然启用」的失败线路。
	// 刚被人工停用的不动——停用表达的是「想保留但暂不使用」。
	stillEnabled := map[string]bool{}
	for _, p := range s.store.ListProxyProfiles() {
		if p.Enabled {
			stillEnabled[p.ID] = true
		}
	}
	purgeIDs := make([]string, 0, len(failed))
	for _, f := range failed {
		if stillEnabled[f.profile.ID] {
			purgeIDs = append(purgeIDs, f.profile.ID)
		}
	}
	if len(purgeIDs) == 0 {
		web.Ok(tagProxyHealth, "失败线路在探测期间已被停用或删除，本轮无需移除")
		return
	}

	purged, reassign, err := s.store.PurgeProxyProfiles(purgeIDs)
	if err != nil {
		web.Err(tagProxyHealth, "移除不可用线路失败: "+err.Error())
		return
	}

	// 名称解析（日志用）：被移除线路取探测前快照，存活线路与账号取当前值。
	profileNames := map[string]string{}
	for _, p := range s.store.ListProxyProfiles() {
		profileNames[p.ID] = p.Name
	}
	for _, f := range failed {
		profileNames[f.profile.ID] = f.profile.Name
	}
	accountNames := map[string]string{}
	for _, a := range s.store.ListAccounts("") {
		accountNames[a.ID] = a.Name
	}
	nameOf := func(m map[string]string, id string) string {
		if n := m[id]; n != "" {
			return n
		}
		return id
	}
	for _, id := range purged {
		web.Warn(tagProxyHealth, fmt.Sprintf("已移除线路：%s（%s）", nameOf(profileNames, id), id))
	}
	for accID, profileID := range reassign.Assigned {
		web.Ok(tagProxyHealth, fmt.Sprintf("账号 %s 已改派到线路 %s",
			nameOf(accountNames, accID), nameOf(profileNames, profileID)))
	}
	for _, accID := range reassign.Direct {
		web.Warn(tagProxyHealth, fmt.Sprintf("账号 %s 无可用线路，已退回直连", nameOf(accountNames, accID)))
	}
	web.Ok(tagProxyHealth, fmt.Sprintf(
		"自动巡检完成：可用 %d/%d，移除 %d 条线路，改派 %d 个账号，%d 个账号退回直连（耗时 %s）",
		len(profiles)-len(failed), len(profiles), len(purged),
		len(reassign.Assigned), len(reassign.Direct), dur))
}
