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
	// ProxySignalUplinkSlow 上行带宽不足：按实测速率算，请求体在本次尝试预算内传不完。
	//
	// 与 transport_timeout 分开记：超时是「等不到首字节」，uplink_slow 是「明确算出
	// 传不完」。两者都是线路问题，但前者可能是上游慢，后者只能是这条线的上行带宽。
	ProxySignalUplinkSlow = "uplink_slow"
	// ProxySignalUpstreamTruncate 上游中途掐断流：已发出 chunk，客户端拿到残缺响应。
	//
	// 它比连接失败更贵——连接失败在请求内可以换线重试，断流不能。所以必须进「线路
	// 还能不能用」的统计，而不是只由 line-guard 的独立计数管（那个阈值是 3，且动作
	// 是不可逆删除）。
	ProxySignalUpstreamTruncate = "upstream_truncate"

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
	// Probation 观察期：隔离到期后线路仍可被选中，但再失败一次立即升档隔离。
	//
	// 唯一的正常出口是「一次完整成功」（清空档位）。没有时间窗出口——时间窗会让
	// 「每小时坏一次」的线路在隔离↔观察之间永久震荡，或在窗口末被无条件洗白。
	// 条目 24 小时无流量会被 TTL 清理，这等同于重新建档。
	Probation bool `json:"probation,omitempty"`
	// Slow 是「首字节超过 30 秒但最终成功」的次数。它只用于观察与后续选线排序，
	// **不参与隔离判定**：30–90 秒出首字节的线路仍然能用，记成失败会把仅存的
	// 可用线路也隔离掉。
	Slow int `json:"slow,omitempty"`
	// Consecutive / ConsecutiveSince 是「连续软失败」计数与起点。它与 Attempts/Failures
	// 的窗口统计独立：窗口按 30 分钟滚动，连续计数按自己的有效期（默认 2 小时）判过期。
	// 成功一次即清零；超过有效期重新从 1 计，避免把隔了半天的两次孤立失败当成连续。
	Consecutive      int   `json:"consecutive,omitempty"`
	ConsecutiveSince int64 `json:"consecutive_since,omitempty"`
}

type proxySoftState struct {
	Entries map[string]ProxySoftFailure `json:"entries"`
}

// ProxySoftDecision 描述本次记录是否触发了隔离动作，供调用方记日志/统计。
type ProxySoftDecision struct {
	Action  string
	Until   time.Time
	Strikes int
	// Trigger 本次处置的判据：probation / consecutive / rate；空串表示只记账。
	Trigger string
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
	// 隔离到期：不是直接回归选线池，而是进入观察期（不清档位）。
	// 2026-10-18 日志里这条线 17:58 隔离、18:28 到期即回池，18:48 与 19:01 又各断一次，
	// 正是因为「到期」被当成了「恢复」。
	if ent.QuarantineUntil > 0 && ent.QuarantineUntil <= nowMs {
		ent.QuarantineUntil = 0
		if config.ProxySoftProbation {
			ent.Probation = true
		}
	}
	// 回退开关：关闭观察期时保留旧的「隔离结束且长时间无新失败 → 档位归零」。
	if !config.ProxySoftProbation && ent.Strikes > 0 && ent.QuarantineUntil <= nowMs && ent.LastAt > 0 && nowMs-ent.LastAt >= proxySoftQuietReset.Milliseconds() {
		ent.Strikes, ent.Consecutive, ent.ConsecutiveSince, ent.Probation = 0, 0, 0, false
	}
	ent.ProfileID, ent.Generation = e.ID, e.Generation
	ent.Attempts++
	if !ok {
		ent.Failures++
		// 连续计数：有效期内累加，过期或首次失败重新起算。
		if ent.Consecutive == 0 || nowMs-ent.ConsecutiveSince > config.ProxySoftConsecutiveWindow.Milliseconds() {
			ent.Consecutive, ent.ConsecutiveSince = 1, nowMs
		} else {
			ent.Consecutive++
		}
	} else {
		ent.Consecutive, ent.ConsecutiveSince = 0, 0
		// 观察期内一次完整成功即洗白：这是观察期唯一的正常出口。
		if ent.Probation {
			ent.Probation, ent.Strikes = false, 0
		}
	}
	ent.LastAt = nowMs
	if signal != "" {
		ent.LastSignal = signal
	}
	if status > 0 {
		ent.LastStatus = status
	}
	quarantined := ent.QuarantineUntil > nowMs
	// 判据 A：观察期内首败。隔离到期只说明「时间到了」，不说明线路恢复；
	// 一次失败就足以回到隔离，且直接升档（strikes 已在原档位上累加）。
	probationHit := !ok && config.ProxySoftProbation && ent.Probation
	// 判据 B：连续失败。与流量密度无关，稀疏流量下也能在两三次尝试内止损。
	consecutiveHit := !ok && config.ProxySoftConsecutive > 0 && ent.Consecutive >= config.ProxySoftConsecutive &&
		nowMs-ent.ConsecutiveSince <= config.ProxySoftConsecutiveWindow.Milliseconds()
	// 判据 C：窗口失败率兜底，防止「成功一次清零」把高频坏线洗白。
	rateHit := !ok && ent.Attempts >= config.ProxySoftMinAttempts && ent.Failures >= config.ProxySoftMinFailures &&
		ent.Failures*100 >= config.ProxySoftMinRatePercent*ent.Attempts
	trigger := ""
	switch {
	case allowQuarantine && !quarantined && (probationHit || consecutiveHit || rateHit):
		switch {
		case probationHit:
			trigger = "probation"
		case consecutiveHit:
			trigger = "consecutive"
		default:
			trigger = "rate"
		}
		ent.Strikes++
		ent.Probation = false
		ent.LastStrikeAt = nowMs
		out = ProxySoftDecision{Action: "quarantine", Until: now.Add(s.softQuarantineDuration(ent.Strikes)), Strikes: ent.Strikes, Trigger: trigger}
		ent.QuarantineUntil = out.Until.UnixMilli()
	case !ok && allowQuarantine && quarantined && nowMs-ent.LastStrikeAt >= proxySoftStrikeCooldown.Milliseconds():
		// 隔离期内再命中 → 续期，但最多每 5 分钟升一档，避免一次故障刷满档位。
		ent.Strikes++
		ent.LastStrikeAt = nowMs
		out = ProxySoftDecision{Action: "extended", Until: now.Add(s.softQuarantineDuration(ent.Strikes)), Strikes: ent.Strikes, Trigger: "extend"}
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
	probationFlag, triggerText := 0, out.Trigger
	if ent.Probation {
		probationFlag = 1
	}
	if triggerText == "" {
		triggerText = "-"
	}
	web.Warn("proxy-soft", fmt.Sprintf(
		"id=%s signal=%s failures=%d attempts=%d rate=%d%% consecutive=%d probation=%d trigger=%s action=%s until=%s strikes=%d reassigned=%d no_alt=%d",
		ent.ProfileID, ent.LastSignal, ent.Failures, ent.Attempts, proxySoftRatePercent(ent), ent.Consecutive,
		probationFlag, triggerText, out.Action,
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

// proxySlowFirstByte 首字节超过此值即记一次慢样本；只观察，不计入失败率。
const proxySlowFirstByte = 30 * time.Second

// RecordProxySlowSample 记录一次「首字节很慢但最终成功」。它不改变 Failures，
// 因此不会把一条能用的线路推进隔离；只作为后台观察与后续选线排序的输入。
func (s *Store) RecordProxySlowSample(e ProxyEgress, ttfb time.Duration, now time.Time) {
	if e.Mode != "proxy" || e.identity == "" || e.ID == "" || ttfb < proxySlowFirstByte {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed {
		return
	}
	ent, ok := s.proxySoft.Entries[e.identity]
	if !ok {
		// 没有任何样本的线路不为了「慢」单独建档，避免一次性线路污染统计。
		return
	}
	ent.Slow++
	ent.LastAt = now.UnixMilli()
	s.proxySoft.Entries[e.identity] = ent
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
