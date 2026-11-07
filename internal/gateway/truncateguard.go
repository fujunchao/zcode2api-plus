// 重试感知选线：记住「哪份内容刚刚在哪些线路上被上游掐断」，供同一内容的重试请求绕开。
//
// 背景（docs/plan-line-truncate-rate-gate-20261107.md）：v2.13.2 起客户端遇到断流会自动
// 重试，而重试**原样重放请求体**；一条出口线路常挂多个账号，只靠账号级回避挡不住
// 「换了个账号、还是同一条线」。本表按内容指纹记住最近断流的线路，重试时把整条线加进
// 请求级 avoid（SelectAvoiding 会跳过绑定在这些线路上的账号）。
//
// 只服务 sync /v1/messages：async 池的消费方是票务轮询，没有「同内容重试」语义。
package gateway

import (
	"sync"
	"time"
)

// truncateRetryMaxEntries 表上限；触顶整体清空。与 ReplayGuard 同一侧安全选择：
// 正常流量下 TTL 窗口内的不同内容屈指可数，触顶说明发生了异常规模事件，
// 此时清空顶多让重试再多试一次，不会把内存吃穿。
const truncateRetryMaxEntries = 256

// truncateRetryEntry 一份内容在最近窗口内断流过的线路集合。
type truncateRetryEntry struct {
	// 每条线路独立计时：同一内容在不同线路上连续失败时，后一次失败不能被首次失败的 TTL 覆盖。
	lines map[string]time.Time
}

// TruncateRetryGuard 按内容指纹记录断流线路。
//
// 并发：每个请求一个 goroutine，读写都要加锁；now 由引擎注入，测试可控制时间。
type TruncateRetryGuard struct {
	mu      sync.Mutex
	entries map[string]truncateRetryEntry
	ttl     time.Duration
	now     func() time.Time
}

// NewTruncateRetryGuard 创建防护表；ttl <= 0 时返回 nil，未注入时钟时使用 time.Now。
func NewTruncateRetryGuard(ttl time.Duration, now func() time.Time) *TruncateRetryGuard {
	if ttl <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &TruncateRetryGuard{entries: map[string]truncateRetryEntry{}, ttl: ttl, now: now}
}

// Note 记一次「该内容在这条线路上断流」。
func (g *TruncateRetryGuard) Note(key, line string, now time.Time) {
	if g == nil || key == "" || line == "" {
		return
	}
	if now.IsZero() {
		now = g.now()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ent, ok := g.entries[key]
	if !ok && len(g.entries) >= truncateRetryMaxEntries {
		g.entries = map[string]truncateRetryEntry{}
	}
	if !ok {
		ent = truncateRetryEntry{lines: map[string]time.Time{}}
	}
	// 先清理这份内容已过期的线路；仍有效的线路保留独立时间戳。
	for identity, at := range ent.lines {
		if now.Sub(at) > g.ttl {
			delete(ent.lines, identity)
		}
	}
	// 同一线路再次断流从本次断流重新计时；不同线路各自拥有完整 TTL。
	// 并发收尾可能以较旧的时间稍后入锁，旧事件不能覆盖较新的断流时间。
	if at, exists := ent.lines[line]; !exists || now.After(at) {
		ent.lines[line] = now
	}
	g.entries[key] = ent
}

// Avoid 返回该内容在 TTL 内断流过的线路 identity；无记录或已过期返回 nil。
//
// 逐线路清理过期记录；内容下的全部线路都过期时，才删除内容条目。
func (g *TruncateRetryGuard) Avoid(key string, now time.Time) []string {
	if g == nil || key == "" {
		return nil
	}
	if now.IsZero() {
		now = g.now()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ent, ok := g.entries[key]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(ent.lines))
	for line, at := range ent.lines {
		if now.Sub(at) > g.ttl {
			delete(ent.lines, line)
			continue
		}
		out = append(out, line)
	}
	if len(ent.lines) == 0 {
		delete(g.entries, key)
		return nil
	}
	g.entries[key] = ent
	return out
}

// Cleanup 清掉过期条目；供长时间运行的表回收内存（可选，读路径也会顺手删过期项）。
func (g *TruncateRetryGuard) Cleanup(now time.Time) int {
	if g == nil {
		return 0
	}
	if now.IsZero() {
		now = g.now()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	removed := 0
	for key, ent := range g.entries {
		for line, at := range ent.lines {
			if now.Sub(at) > g.ttl {
				delete(ent.lines, line)
			}
		}
		if len(ent.lines) == 0 {
			delete(g.entries, key)
			removed++
		} else {
			g.entries[key] = ent
		}
	}
	return removed
}
