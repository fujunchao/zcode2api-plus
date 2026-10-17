// 线路级 405 画像：连续被上游风控拦截的线路退出选号。
//
// 与软失败（proxy_soft_failure.go）刻意分开：软失败衡量「这条线传不传得动」，
// 本文件衡量「这条线会不会被上游直接拒绝」。两者混用会让小请求被大请求的慢统计
// 连坐，也会让「慢但能用」的线被「脏」的计数拖下水。
//
// 设计边界：
//   - 只隔离、不删除。删除仍只由网络复核双证与严格代理证据决定；
//   - 只影响新派发（selectAccount 与 chooseProxyLocked），不改已有绑定；
//   - 连续计数而非累计：中间成功过一次就清零，避免误杀「时好时坏」的线路。
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

const (
	line405Key = "line_405_state"
	// line405Limit 条目上限：按最近活跃裁剪，避免免费池长期轮换把内存/落库撑大。
	line405Limit = 4096
	// line405TTL 静默过期：超过该时长没有新样本且不在隔离期的条目直接丢弃。
	line405TTL = 24 * time.Hour
)

// Line405Failure 是一条线路的 405 画像。只保存去敏身份，不含凭据与代理认证。
type Line405Failure struct {
	Identity string `json:"identity"`
	// ProfileID 仅用于后台展示与定向巡检入队；裸 URL 账号不建档，因此不会为空。
	ProfileID string `json:"profile_id"`
	// Generation 线路配置代次：编辑过 URL 后旧账作废，避免误伤新配置。
	Generation uint64 `json:"generation"`
	// Consecutive405 连续 405 次数（任何一次成功即清零）。
	Consecutive405 int `json:"consecutive_405"`
	// Total405 / OK 仅作后台统计，不参与任何判定。
	Total405 int `json:"total_405"`
	OK       int `json:"ok"`
	// Strikes 已隔离次数，决定续期档位（30→60→120→240 分钟）。
	Strikes         int   `json:"strikes"`
	FirstAt         int64 `json:"first_at"`
	LastAt          int64 `json:"last_at"`
	QuarantineUntil int64 `json:"quarantine_until,omitempty"`
}

type line405State struct {
	Entries map[string]Line405Failure `json:"entries"`
}

// Line405Decision 描述一次 405 记录的结果，供调用方落日志。
type Line405Decision struct {
	Action    string // ignored | recorded | quarantine
	ProfileID string
	Until     time.Time
	Strikes   int
}

// RecordLine405 记录一次上游风控拦截；连续命中达到阈值即隔离该线路。
func (s *Store) RecordLine405(e ProxyEgress, now time.Time) Line405Decision {
	out := Line405Decision{Action: "ignored"}
	if e.Mode != "proxy" || e.identity == "" || e.ID == "" {
		// 直连 / direct_fallback 没有「线路」可隔离，不建档。
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed {
		return out
	}
	if s.line405.Entries == nil {
		s.line405.Entries = map[string]Line405Failure{}
	}
	nowMs := now.UnixMilli()
	ent, exists := s.line405.Entries[e.identity]
	if !exists || ent.Generation != e.Generation {
		ent = Line405Failure{Identity: e.identity, FirstAt: nowMs}
	}
	ent.ProfileID, ent.Generation = e.ID, e.Generation
	ent.Consecutive405++
	ent.Total405++
	ent.LastAt = nowMs
	min := config.Line405MinStreak
	if min <= 0 || ent.Consecutive405 < min {
		s.line405.Entries[e.identity] = ent
		out.Action = "recorded"
		return out
	}
	ent.Strikes++
	until := now.Add(line405Quarantine(ent.Strikes))
	if until.UnixMilli() > ent.QuarantineUntil {
		ent.QuarantineUntil = until.UnixMilli()
	}
	s.line405.Entries[e.identity] = ent
	if err := s.commitLine405Locked(now); err != nil {
		web.Warn("line405", "线路 405 状态落库失败: "+err.Error())
	}
	return Line405Decision{Action: "quarantine", ProfileID: ent.ProfileID, Until: until, Strikes: ent.Strikes}
}

// RecordLineOK 记录一次完整成功：清零连续计数与隔离档位，并立即解除隔离。
//
// 一次成功足以证明这条线此刻可用；继续压着只会白丢容量。若它其实只是偶发可用，
// 连续两次 405 会很快把它重新隔离，代价有界。
func (s *Store) RecordLineOK(e ProxyEgress, now time.Time) {
	if e.Mode != "proxy" || e.identity == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.line405.Entries[e.identity]
	if !ok {
		return
	}
	changed := ent.Consecutive405 != 0 || ent.Strikes != 0 || ent.QuarantineUntil != 0
	ent.Consecutive405, ent.Strikes, ent.QuarantineUntil = 0, 0, 0
	ent.OK++
	ent.LastAt = now.UnixMilli()
	s.line405.Entries[e.identity] = ent
	if changed {
		if err := s.commitLine405Locked(now); err != nil {
			web.Warn("line405", "线路 405 状态落库失败: "+err.Error())
		}
	}
}

// Line405Quarantine 返回仍在隔离期的线路（identity → 到期时间）。
func (s *Store) Line405Quarantine(now time.Time) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.line405QuarantineLocked(now)
}

func (s *Store) line405QuarantineLocked(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	nowMs := now.UnixMilli()
	for identity, ent := range s.line405.Entries {
		if ent.QuarantineUntil > nowMs {
			out[identity] = time.UnixMilli(ent.QuarantineUntil)
		}
	}
	return out
}

// Line405Blocked 只读判断某条出站线路此刻是否被隔离（供网关记录「命中时是否已脏」）。
func (s *Store) Line405Blocked(e ProxyEgress, now time.Time) bool {
	if e.Mode != "proxy" || e.identity == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.line405.Entries[e.identity].QuarantineUntil > now.UnixMilli()
}

// line405BlockedLocked 返回隔离集合，供选号在持锁路径内一次取全。
// 没有任何隔离时返回 nil：选号是热路径，nil map 的读取同样安全，省一次分配。
func (s *Store) line405BlockedLocked(now time.Time) map[string]bool {
	if len(s.line405.Entries) == 0 {
		return nil
	}
	nowMs := now.UnixMilli()
	var out map[string]bool
	for identity, ent := range s.line405.Entries {
		if ent.QuarantineUntil > nowMs {
			if out == nil {
				out = map[string]bool{}
			}
			out[identity] = true
		}
	}
	return out
}

// Line405Failures 供后台展示；按最近活跃倒序返回深拷贝。
func (s *Store) Line405Failures() []Line405Failure {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Line405Failure, 0, len(s.line405.Entries))
	for _, ent := range s.line405.Entries {
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastAt != out[j].LastAt {
			return out[i].LastAt > out[j].LastAt
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

// boundLineIdentityLocked 返回账号当前绑定线路的去重键；直连统一记 "direct"。
// 选号与请求级判定必须用同一个口径，否则直连请求会被当成「同一条线」。
func (s *Store) boundLineIdentityLocked(a *model.Account) string {
	if a == nil {
		return "direct"
	}
	raw := derefStr(a.ProxyURL)
	if raw == "" {
		return "direct"
	}
	return strictProxyIdentity(raw)
}

// logLine405RelaxLocked 记录「全池线路都被 405 隔离、只能回落」的选号放宽事件。
// 与软失败的 relax 分开限频：两类放宽同时发生时不互相吞日志。
func (s *Store) logLine405RelaxLocked(now time.Time, candidates, blocked int) {
	if now.UnixMilli()-s.line405RelaxLoggedAt < proxyRelaxLogInterval.Milliseconds() {
		return
	}
	s.line405RelaxLoggedAt = now.UnixMilli()
	web.Warn("line405", fmt.Sprintf("relax=1 candidates=%d blocked=%d", candidates, blocked))
}

// line405Quarantine 按连续隔离次数取 1/2/4/8 倍首档时长，并封顶。
func line405Quarantine(strikes int) time.Duration {
	base := time.Duration(config.Line405QuarantineMinutes) * time.Minute
	if strikes <= 1 {
		return base
	}
	shift := strikes - 1
	if shift > 3 {
		shift = 3
	}
	d := base * time.Duration(1<<shift)
	if maxD := time.Duration(config.Line405MaxQuarantineMinutes) * time.Minute; d > maxD {
		return maxD
	}
	return d
}

func (s *Store) commitLine405Locked(now time.Time) error {
	s.pruneLine405Locked(now)
	raw, err := json.Marshal(s.line405)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", line405Key, string(raw))
	return err
}

// pruneLine405Locked 清理过旧条目并按最近活跃截断；调用方须持有 s.mu。
func (s *Store) pruneLine405Locked(now time.Time) {
	if s.line405.Entries == nil {
		s.line405.Entries = map[string]Line405Failure{}
		return
	}
	nowMs := now.UnixMilli()
	cut := now.Add(-line405TTL).UnixMilli()
	for identity, ent := range s.line405.Entries {
		if ent.LastAt < cut && ent.QuarantineUntil <= nowMs {
			delete(s.line405.Entries, identity)
		}
	}
	if len(s.line405.Entries) <= line405Limit {
		return
	}
	type entry struct {
		identity string
		at       int64
	}
	list := make([]entry, 0, len(s.line405.Entries))
	for identity, ent := range s.line405.Entries {
		list = append(list, entry{identity: identity, at: ent.LastAt})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at < list[j].at })
	for i := 0; i < len(list)-line405Limit; i++ {
		delete(s.line405.Entries, list[i].identity)
	}
}
