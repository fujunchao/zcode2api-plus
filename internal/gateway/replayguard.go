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
	entries map[string]time.Time
	ttl     time.Duration
	now     func() time.Time
}

// NewReplayGuard 创建防护表；ttl <= 0 返回 nil（禁用，调用方判空跳过）。
func NewReplayGuard(ttl time.Duration, now func() time.Time) *ReplayGuard {
	if ttl <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &ReplayGuard{entries: map[string]time.Time{}, ttl: ttl, now: now}
}

// Blocked 该内容指纹是否仍在防护窗口内。
func (g *ReplayGuard) Blocked(key string) bool {
	if g == nil || key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	at, ok := g.entries[key]
	if !ok {
		return false
	}
	if g.now().Sub(at) >= g.ttl {
		delete(g.entries, key)
		return false
	}
	return true
}

// Record 登记一次请求级风控判定；顺带懒清理过期项。
func (g *ReplayGuard) Record(key string) {
	if g == nil || key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for k, at := range g.entries {
		if now.Sub(at) >= g.ttl {
			delete(g.entries, k)
		}
	}
	if len(g.entries) >= replayGuardMaxEntries {
		g.entries = map[string]time.Time{}
	}
	g.entries[key] = now
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
