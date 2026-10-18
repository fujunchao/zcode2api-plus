// 重放防护（P1-3）：同一份请求内容刚刚被判定「请求级风控」后，TTL 内不再把它喂给
// 任何账号 —— 直接快速失败。
//
// 为什么需要它（2026-09-24 账号级标记模型，docs/analysis-auth-chain-vs-official.md §九）：
// 上游 405 风控是两段式 —— 内容特征触发 + 账号标记。请求级判定成立意味着**每一个被
// 尝试过的账号都已被服务端标记**；此后窗口期内任何同内容请求都是纯破坏：它不会成功，
// 只会把池里下一个健康账号再标记一个。凌晨事故里 14 次重放冷却 67 个账号、整池清空，
// 正是「没有这层闸」的代价。60s 窗口的依据：标记持续 ≥14h，远超窗口；窗口只负责
// 挡住紧随其后的自动重试风暴（客户端重试、下游轮询），不试图替代账号级冷却。
package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"zcode2api/internal/config"
)

// ReplayGuardTTL 重放防护窗口（秒）。0 = 禁用（应急回退开关）。
var ReplayGuardTTL = config.ReplayGuardTTLSeconds

// replayGuardMaxEntries 防护表上限：触顶即整体清空。正常情况下 60s 窗口内被拦的
// 不同内容屈指可数，触顶说明发生了异常规模的事件——此时清空（而不是放任增长）
// 是安全的一侧：顶多让后续同内容请求再走一遍账号（再一次标记扩散），不会把内存吃穿。
const replayGuardMaxEntries = 1024

// ReplayGuard 按「内容指纹 + 模型」记录最近一次请求级风控判定。
type ReplayGuard struct {
	mu      sync.Mutex
	entries map[string]replayEntry
	ttl     time.Duration
	now     func() time.Time
}

// replayEntry 一条防护记录。
//
// allowances 是「一次性放行」余额：判定成立后允许同内容再试一次，用来区分
// 「内容确实被拒」与「连撞几条脏线」。2026-10-18 日志里后者占多数——客户端
// 57 秒内重试 11 次全被挡，而 207 秒后同内容换线即成功。
type replayEntry struct {
	at         time.Time
	allowances int
}

// NewReplayGuard 创建防护表；ttl <= 0 返回 nil（禁用，调用方判空跳过）。
func NewReplayGuard(ttl time.Duration, now func() time.Time) *ReplayGuard {
	if ttl <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &ReplayGuard{entries: map[string]replayEntry{}, ttl: ttl, now: now}
}

// Blocked 该内容指纹是否仍在防护窗口内。
func (g *ReplayGuard) Blocked(key string) bool {
	return g.Remaining(key) > 0
}

// Remaining 返回防护剩余时间，只读命中项；连续重放不会延长窗口。
// 过期清理与 Blocked 的原行为一致。
func (g *ReplayGuard) Remaining(key string) time.Duration {
	if g == nil || key == "" {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ent, ok := g.entries[key]
	if !ok {
		return 0
	}
	remaining := g.ttl - g.now().Sub(ent.at)
	if remaining <= 0 {
		delete(g.entries, key)
		return 0
	}
	return remaining
}

// Take 是入口检查：返回是否应拦截，以及剩余时间。
//
// 与 Remaining 的区别是它会**消耗一次性放行余额**。判定成立后允许同内容再试一次
// （默认开，见 config.ReplayAllowOnce）：真正的请求级风控会让这次重试再次撞墙，
// 而「三条脏线连击」会被这次重试救回来。余额每窗口只武装一次，不会形成无限放行。
func (g *ReplayGuard) Take(key string) (bool, time.Duration) {
	if g == nil || key == "" {
		return false, 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ent, ok := g.entries[key]
	if !ok {
		return false, 0
	}
	remaining := g.ttl - g.now().Sub(ent.at)
	if remaining <= 0 {
		delete(g.entries, key)
		return false, 0
	}
	if ent.allowances > 0 {
		ent.allowances--
		g.entries[key] = ent
		return false, 0
	}
	return true, remaining
}

// Record 登记一次请求级风控判定；顺带懒清理过期项。
//
// 同一窗口内重复登记**不重新武装**放行余额：否则每次判定都放行一次，客户端重试
// 风暴会把脏线当免费额度用。
func (g *ReplayGuard) Record(key string) {
	if g == nil || key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for k, ent := range g.entries {
		if now.Sub(ent.at) >= g.ttl {
			delete(g.entries, k)
		}
	}
	if len(g.entries) >= replayGuardMaxEntries {
		g.entries = map[string]replayEntry{}
	}
	allowances := 0
	if ent, ok := g.entries[key]; ok {
		// 窗口内已登记：保留现有余额（含已用完的 0），不重新武装。
		allowances = ent.allowances
	} else if config.ReplayAllowOnce {
		allowances = 1
	}
	g.entries[key] = replayEntry{at: now, allowances: allowances}
}

// ContentKey 重放防护的键：模型 + 内容指纹（请求体 sha256 前 12 位）。
//
// 用**入口原始 body** 而不是整形后的内容视图：防护判定发生在选号之前，那时还没有
// 逐账号的整形产物；下游内容相同 + 模型相同即视为同一重放，粒度对本目标足够。
func ContentKey(modelName string, body map[string]any) string {
	raw, err := marshalJSON(body)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return modelName + "|" + hex.EncodeToString(sum[:])[:12]
}
