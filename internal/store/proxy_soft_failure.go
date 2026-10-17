package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/web"
)

// 代理软失败：模型请求在「代理确实被使用且目标已确认」前提下没有完成。
// 它与删除证据完全分离——软失败只产生隔离（移出自动选线），从不写删除决定；
// 删除仍只由网络复核的 confirmed_failure 触发。
const (
	// ProxySignalTransportTimeout 传输层超时（context deadline / net timeout）。
	ProxySignalTransportTimeout = "transport_timeout"
	// ProxySignalTransportReset 连接被重置或读到 EOF。
	ProxySignalTransportReset = "transport_reset"

	proxySoftFailureKey     = "proxy_soft_failure_state"
	proxySoftFailureLimit   = 4096
	proxySoftFailureTTL     = 24 * time.Hour
	proxySoftQuietReset     = 30 * time.Minute
	proxySoftStrikeCooldown = 5 * time.Minute
	proxyRelaxLogInterval   = time.Minute
)

// ProxySoftFailure 是一条线路的软失败计数与隔离状态；只保存去敏身份，不含凭据。
type ProxySoftFailure struct {
	Identity        string `json:"identity"`
	ProfileID       string `json:"profile_id"`
	Generation      uint64 `json:"generation"`
	Attempts        int    `json:"attempts"`
	Failures        int    `json:"failures"`
	WindowStart     int64  `json:"window_start"`
	LastAt          int64  `json:"last_at"`
	LastStrikeAt    int64  `json:"last_strike_at,omitempty"`
	LastSignal      string `json:"last_signal,omitempty"`
	LastStatus      int    `json:"last_status,omitempty"`
	Strikes         int    `json:"strikes"`
	QuarantineUntil int64  `json:"quarantine_until,omitempty"`
}

type proxySoftState struct {
	Entries map[string]ProxySoftFailure `json:"entries"`
}

// ProxySoftDecision 描述本次记录是否触发了隔离动作，供调用方记日志/统计。
type ProxySoftDecision struct {
	Action  string
	Until   time.Time
	Strikes int
}

// RecordProxySoftOutcome 记录一次「业务可用性」样本：ok 为完整成功，否则为软失败。
// 只有 200/2xx 成功与超时类失败会进入样本，405/402/429 等业务错误不参与，
// 否则失败率会被与线路无关的上游语义稀释。
func (s *Store) RecordProxySoftOutcome(e ProxyEgress, signal string, status int, ok bool, now time.Time) ProxySoftDecision {
	out := ProxySoftDecision{}
	if e.Mode != "proxy" || e.identity == "" || e.ID == "" {
		return out
	}
	if !ok && signal == "" {
		return out
	}
	// 总开关关闭时仍然记账（便于后台/日志观察），只是不产生隔离动作。
	allowQuarantine := config.ProxySoftQuarantine
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed {
		return out
	}
	if s.proxySoft.Entries == nil {
		s.proxySoft.Entries = map[string]ProxySoftFailure{}
	}
	nowMs := now.UnixMilli()
	windowMs := config.ProxySoftWindow.Milliseconds()
	ent, exists := s.proxySoft.Entries[e.identity]
	if !exists {
		ent = ProxySoftFailure{Identity: e.identity, WindowStart: nowMs}
	}
	// 窗口只在「不在隔离期且已过期」时重开，隔离期内继续累计便于判续期。
	if ent.QuarantineUntil <= nowMs && nowMs-ent.WindowStart >= windowMs {
		ent.WindowStart = nowMs
		ent.Attempts = 0
		ent.Failures = 0
	}
	// 隔离结束且长时间无新失败 → 连续次数归零；否则一次偶发抖动会一直顶在高档。
	if ent.Strikes > 0 && ent.QuarantineUntil <= nowMs && ent.LastAt > 0 && nowMs-ent.LastAt >= proxySoftQuietReset.Milliseconds() {
		ent.Strikes = 0
	}
	ent.ProfileID, ent.Generation = e.ID, e.Generation
	ent.Attempts++
	if !ok {
		ent.Failures++
	}
	ent.LastAt = nowMs
	if signal != "" {
		ent.LastSignal = signal
	}
	if status > 0 {
		ent.LastStatus = status
	}
	quarantined := ent.QuarantineUntil > nowMs
	switch {
	case !ok && allowQuarantine && !quarantined && ent.Attempts >= config.ProxySoftMinAttempts && ent.Failures >= config.ProxySoftMinFailures &&
		ent.Failures*100 >= config.ProxySoftMinRatePercent*ent.Attempts:
		ent.Strikes++
		ent.LastStrikeAt = nowMs
		out = ProxySoftDecision{Action: "quarantine", Until: now.Add(s.softQuarantineDuration(ent.Strikes)), Strikes: ent.Strikes}
		ent.QuarantineUntil = out.Until.UnixMilli()
	case !ok && allowQuarantine && quarantined && nowMs-ent.LastStrikeAt >= proxySoftStrikeCooldown.Milliseconds():
		// 隔离期内再命中 → 续期，但最多每 5 分钟升一档，避免一次故障刷满档位。
		ent.Strikes++
		ent.LastStrikeAt = nowMs
		out = ProxySoftDecision{Action: "extended", Until: now.Add(s.softQuarantineDuration(ent.Strikes)), Strikes: ent.Strikes}
		ent.QuarantineUntil = out.Until.UnixMilli()
	}
	s.proxySoft.Entries[e.identity] = ent
	if out.Action == "" {
		return out
	}
	if err := s.commitProxySoftLocked(now); err != nil {
		web.Warn("proxy-soft", "软失败状态落库失败: "+err.Error())
	}
	// 隔离与续期都复用既有网络复核队列：软失败本身不是删除证据，
	// 但值得让巡检用新鲜探测再确认一次（探测失败才会进入现有的删除链）。
	s.queueSoftRecheckLocked(ent, now)
	reassigned, noAlt := 0, 0
	if result, err := s.reassignOffQuarantineLocked(map[string]bool{ent.Identity: true}); err != nil {
		web.Warn("proxy-soft", fmt.Sprintf("id=%s 隔离后改派失败: %v", ent.ProfileID, err))
	} else {
		reassigned, noAlt = len(result.Assigned), len(result.Direct)
	}
	web.Warn("proxy-soft", fmt.Sprintf(
		"id=%s signal=%s failures=%d attempts=%d rate=%d%% action=%s until=%s strikes=%d reassigned=%d no_alt=%d",
		ent.ProfileID, ent.LastSignal, ent.Failures, ent.Attempts, proxySoftRatePercent(ent), out.Action,
		out.Until.Format(time.RFC3339), ent.Strikes, reassigned, noAlt))
	return out
}

// queueSoftRecheckLocked 把刚被隔离的线路登记进网络复核队列；已有通知时不重复登记。
// 不要求绑定账号：隔离可能发生在账号已经改派走之后。
func (s *Store) queueSoftRecheckLocked(ent ProxySoftFailure, now time.Time) {
	if ent.ProfileID == "" {
		return
	}
	if s.proxyFailureRechecks == nil {
		s.proxyFailureRechecks = map[string]proxyFailureRecheck{}
	}
	if _, exists := s.proxyFailureRechecks[ent.ProfileID]; exists {
		return
	}
	if len(s.proxyFailureRechecks) >= proxyFailureRecheckLimit {
		return
	}
	s.proxyFailureRechecks[ent.ProfileID] = proxyFailureRecheck{
		Route: ProxyEgress{
			ID: ent.ProfileID, Mode: "proxy",
			Generation: ent.Generation, identity: ent.Identity,
		},
		PolicyRevision: s.modelProbe.Policy.Revision,
		At:             now,
		Pending:        true,
	}
}

// ProxyQuarantine 返回当前仍在隔离期的线路（identity → 到期时间）。
func (s *Store) ProxyQuarantine(now time.Time) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyQuarantineLocked(now)
}

func (s *Store) proxyQuarantineLocked(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	nowMs := now.UnixMilli()
	for identity, ent := range s.proxySoft.Entries {
		if ent.QuarantineUntil > nowMs {
			out[identity] = time.UnixMilli(ent.QuarantineUntil)
		}
	}
	return out
}

// ProxySoftFailures 供后台展示；按最近活跃倒序返回深拷贝。
func (s *Store) ProxySoftFailures() []ProxySoftFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProxySoftFailure, 0, len(s.proxySoft.Entries))
	for _, ent := range s.proxySoft.Entries {
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

func (s *Store) commitProxySoftLocked(now time.Time) error {
	s.pruneProxySoftLocked(now)
	raw, err := json.Marshal(s.proxySoft)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", proxySoftFailureKey, string(raw))
	return err
}

// pruneProxySoftLocked 清理过旧条目并按最近活跃截断；调用方须持有 s.mu。
func (s *Store) pruneProxySoftLocked(now time.Time) {
	if s.proxySoft.Entries == nil {
		s.proxySoft.Entries = map[string]ProxySoftFailure{}
		return
	}
	nowMs := now.UnixMilli()
	cut := now.Add(-proxySoftFailureTTL).UnixMilli()
	for identity, ent := range s.proxySoft.Entries {
		if ent.LastAt < cut && ent.QuarantineUntil <= nowMs {
			delete(s.proxySoft.Entries, identity)
		}
	}
	if len(s.proxySoft.Entries) <= proxySoftFailureLimit {
		return
	}
	type entry struct {
		identity string
		at       int64
	}
	list := make([]entry, 0, len(s.proxySoft.Entries))
	for identity, ent := range s.proxySoft.Entries {
		list = append(list, entry{identity: identity, at: ent.LastAt})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at < list[j].at })
	for i := 0; i < len(list)-proxySoftFailureLimit; i++ {
		delete(s.proxySoft.Entries, list[i].identity)
	}
}

// softQuarantineDuration 按连续次数取 1/2/4/8 倍首档时长，避免长尾线路无限升级。
func (s *Store) softQuarantineDuration(strikes int) time.Duration {
	base := time.Duration(config.ProxySoftQuarantineMinutes) * time.Minute
	if strikes <= 1 {
		return base
	}
	shift := strikes - 1
	if shift > 3 {
		shift = 3
	}
	return base * time.Duration(1<<shift)
}

func proxySoftRatePercent(ent ProxySoftFailure) int {
	if ent.Attempts <= 0 {
		return 0
	}
	return ent.Failures * 100 / ent.Attempts
}
