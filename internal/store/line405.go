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
	"errors"
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
	// ProbeState 定向模型巡检状态：none / queued / healthy / failed。
	//
	// 隔离只让这条线退出选号，真正决定「删不删」的是巡检：网络探针是匿名 GET，
	// 脏 IP 对 GET 是通的，结构上永远测不到 405。这里记录最近一次结论，供后台与
	// 日志对齐「隔离中」和「正在验证」两种不同状态。
	ProbeState string `json:"probe_state,omitempty"`
	ProbeAt    int64  `json:"probe_at,omitempty"`
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
	decision := Line405Decision{Action: "quarantine", ProfileID: ent.ProfileID, Until: until, Strikes: ent.Strikes}
	// 隔离生效后触发一次定向模型巡检：隔离只是「暂时别派发」，把它真正清出池子
	// 需要一次带凭据的模型请求。限频与预算都在这个函数内判定，失败不影响隔离。
	s.maybeQueueLine405ProbeLocked(&ent, now)
	s.line405.Entries[e.identity] = ent
	if err := s.commitLine405Locked(now); err != nil {
		web.Warn("line405", "线路 405 状态落库失败: "+err.Error())
	}
	return decision
}

// maybeQueueLine405ProbeLocked 为刚被隔离的线路入队一次定向模型巡检。
//
// 调用方必须持有 s.mu：这里用 queueModelProbeLocked 而不是导出的 QueueModelProbe，
// 后者会再次加锁（自锁）。入队成功只表示「已排队」，Manager 在下一个 tick 通过
// TakeModelProbe 取走，30–60 秒的延迟对隔离场景完全可接受。
func (s *Store) maybeQueueLine405ProbeLocked(ent *Line405Failure, now time.Time) {
	if ent == nil || ent.ProfileID == "" {
		return
	}
	limit := config.Line405ProbePerHour
	if limit <= 0 {
		web.Warn("line405-probe", fmt.Sprintf("skipped id=%s reason=disabled", ent.ProfileID))
		return
	}
	cooldown := time.Duration(config.Line405ProbeCooldownMinutes) * time.Minute
	if last, ok := s.line405ProbeAt[ent.ProfileID]; ok && cooldown > 0 && now.Sub(last) < cooldown {
		web.Warn("line405-probe", fmt.Sprintf("skipped id=%s reason=cooldown", ent.ProfileID))
		return
	}
	hour := now.Truncate(time.Hour).Unix()
	if s.line405ProbeHour != hour {
		s.line405ProbeHour, s.line405ProbeCount = hour, 0
	}
	if s.line405ProbeCount >= limit {
		web.Warn("line405-probe", fmt.Sprintf("skipped id=%s reason=budget used=%d limit=%d", ent.ProfileID, s.line405ProbeCount, limit))
		return
	}
	if _, err := s.queueModelProbeLocked(ent.ProfileID, "line405", now); err != nil {
		reason := "unavailable"
		if errors.Is(err, ErrModelProbeUnavailable) {
			// 巡检关闭 / 目标不在覆盖范围：静默跳过，隔离照常生效。
			reason = "disabled"
		}
		web.Warn("line405-probe", fmt.Sprintf("skipped id=%s reason=%s", ent.ProfileID, reason))
		return
	}
	if s.line405ProbeAt == nil {
		s.line405ProbeAt = map[string]time.Time{}
	}
	s.line405ProbeAt[ent.ProfileID] = now
	s.line405ProbeCount++
	ent.ProbeState, ent.ProbeAt = "queued", now.UnixMilli()
	web.Warn("line405-probe", fmt.Sprintf("queued id=%s source=line405 budget=%d/%d", ent.ProfileID, s.line405ProbeCount, limit))
}

// ApplyLine405ProbeVerdict 把一次模型巡检的结论回写到线路画像。
//
// 只做两件事：健康结论提前解封；失败结论保留隔离并记状态。删除永远由既有退休链
// （retireModelProbeLocked → startStrictRetirementLocked）完成，这里绝不自己删——
// 否则会绕过「两轮确认 + 差分对照 + 保护名单 + 配置代次」四道闸。
func (s *Store) ApplyLine405ProbeVerdict(profileID, verdict string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyLine405ProbeVerdictLocked(profileID, verdict, now)
}

// applyLine405ProbeVerdictLocked 返回 (state, identity, ok)：调用方（CompleteModelProbe）
// 需要知道结论是否落在一条「反复隔离」的线路上，以决定要不要走双证退休。
func (s *Store) applyLine405ProbeVerdictLocked(profileID, verdict string, now time.Time) (string, string, bool) {
	if s.strictClosed || profileID == "" {
		return "", "", false
	}
	state := ""
	switch verdict {
	case "healthy_recent", "mixed", "account_correlated":
		// 目标线路自己跑通了一次，或问题出在账号上：线路本身可用。
		state = "healthy"
	case "route_correlated", "suspect":
		// 确认问题在线路上；删除交给退休链，这里只保持隔离并留痕。
		state = "failed"
	default:
		return "", "", false
	}
	var profile ProxyProfile
	found := false
	for _, p := range s.listProxyProfilesLocked() {
		if p.ID == profileID {
			profile, found = p, true
			break
		}
	}
	if !found {
		return "", "", false
	}
	identity := strictProxyIdentity(profile.URL)
	ent, ok := s.line405.Entries[identity]
	if !ok || ent.Generation != profile.Generation {
		return "", "", false
	}
	ent.ProbeState, ent.ProbeAt = state, now.UnixMilli()
	if state == "healthy" {
		ent.Consecutive405, ent.Strikes, ent.QuarantineUntil = 0, 0, 0
	}
	s.line405.Entries[identity] = ent
	if err := s.commitLine405Locked(now); err != nil {
		web.Warn("line405", "线路 405 状态落库失败: "+err.Error())
	}
	web.Warn("line405-probe", fmt.Sprintf("verdict=%s id=%s", state, profileID))
	return state, identity, true
}

// maybeRetireLine405ProbeLocked 用「真实流量 + 巡检」双证退休一条 405 线路。
//
// 判据：该线路已被隔离过 ZCODE_LINE405_RETIRE_MIN_STRIKES 次（Strikes≥2 意味着
// 30 分钟放出去后又被真实流量 405 了一次），且巡检也给出 failed。
// 两条证据互相独立，比「同一个探针跑两轮」更接近真实使用，也才在线上可达。
func (s *Store) maybeRetireLine405ProbeLocked(r *ModelProbeRun, identity string, now time.Time) {
	if r == nil || identity == "" || !config.Line405ProbeRetire {
		return
	}
	if r.policy.Mode != "auto_retire" {
		return
	}
	ent, ok := s.line405.Entries[identity]
	if !ok || ent.Strikes < config.Line405RetireMinStrikes {
		return
	}
	rawURL := ""
	for _, p := range s.listProxyProfilesLocked() {
		if strictProxyIdentity(p.URL) == identity {
			rawURL = p.URL
			break
		}
	}
	if rawURL == "" {
		return
	}
	refs := make([]string, 0, 4)
	for _, e := range r.proofs {
		if e.RequestID == "" || len(refs) == 4 {
			continue
		}
		refs = append(refs, e.RequestID)
	}
	d, err := s.startStrictRetirementLocked(identity, rawURL, "line405_probe_control", r.targetURL, ModelProbeRuleVersion, refs, now)
	if err != nil {
		web.Warn("line405-probe", "退休失败: "+err.Error())
		return
	}
	if d.DecisionID == "" {
		web.Warn("line405-probe", fmt.Sprintf("retire-skipped id=%s reason=%s", r.TargetID, d.Reason))
		return
	}
	web.Warn("line405-probe", fmt.Sprintf("retire id=%s decision=%s removed=%d reassigned=%d strikes=%d",
		r.TargetID, d.DecisionID, len(d.Removed), d.Assigned, ent.Strikes))
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
