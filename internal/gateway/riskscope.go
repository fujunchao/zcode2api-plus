// 风控保护按账号阶梯冷却；同请求达到不同账号阈值后停止换号并启用重放保护。
// 阈值仅限制进一步尝试，不诊断供应商的触发原因。默认只按阶梯冷却；显式开启
// 额外静默隔离（config.RiskSilentHours）时，窗口内的账号不参与选号；
// 代理失败历史不等于账号封禁次数。
package gateway

import (
	"fmt"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// RiskControlRequestLevelThreshold 判定请求级风控所需的不同「干净线路」数。
// 1 = 关闭判定（任何一次 405 都算请求级，仅用于极端回退）；默认取配置 3。
var RiskControlRequestLevelThreshold = config.RiskRequestLevelLines

// RiskScope 一次请求的风控判定范围。
//
// ⚠️ 必须按请求创建（engine.RunMessages / asyncpool.processTicket 内），不能挂在
// Engine 或 Pool 上：两者都是跨请求共享的，任何共享的可变状态都会让并发的两个请求互相
// 误判——A 请求的账号级风控会被 B 请求的命中计数顶成请求级。
//
// 判定单位是**线路**而不是账号：2026-10-17 日志里 21 条线路出过 405 且成功数为 0，
// 同一份内容换条线 41–149 秒后即可成功；按账号计数会把「抽到两条脏线」误判成
// 「内容被拒」，进而锁死整条请求（9 次判定 8 次错）。
//
// 导出供 asyncpool 复用：两条请求路径对同一份 body 必须给出相同的判定，否则「sync 安全、
// async 打穿池子」的分歧迟早出现。
type RiskScope struct {
	lines      map[string]bool // 给出过 405 的「干净」线路（按 LineKey 去重）
	accounts   map[string]bool // 仅用于日志：命中过 405 的账号数
	contentKey string          // 重放防护键（模型 + 内容指纹）；请求级判定成立时登记进 ReplayGuard
}

// NewRiskScope 创建一次请求的判定范围；contentKey 为重放防护键（可为空，空则不登记）。
func NewRiskScope(contentKey string) *RiskScope {
	return &RiskScope{lines: map[string]bool{}, accounts: map[string]bool{}, contentKey: contentKey}
}

// ContentKey 重放防护键。
func (s *RiskScope) ContentKey() string {
	if s == nil {
		return ""
	}
	return s.contentKey
}

// Verdict 按不同「干净线路」去重记次。
//
// lineDirty 表示本次 405 **之前**该线路已被判脏（隔离中）：脏线的 405 只能证明
// 「这条线脏」，不能当作「内容有问题」的证据，因此不计入阈值。
// 结论不表示正文就是风控原因。
func (s *RiskScope) Verdict(acc *model.Account, lineKey string, lineDirty bool) bool {
	if s == nil || acc == nil {
		return false
	}
	if s.lines == nil {
		s.lines = map[string]bool{}
	}
	if s.accounts == nil {
		s.accounts = map[string]bool{}
	}
	s.accounts[acc.ID] = true
	if !lineDirty && lineKey != "" {
		s.lines[lineKey] = true
	}
	return len(s.lines) >= RiskControlRequestLevelThreshold
}

// Lines 已命中 405 的不同干净线路数（供日志）。
func (s *RiskScope) Lines() int {
	if s == nil {
		return 0
	}
	return len(s.lines)
}

// Accounts 已给出风控信号的不同账号数（供日志；不参与判定）。
func (s *RiskScope) Accounts() int {
	if s == nil {
		return 0
	}
	return len(s.accounts)
}

// MarkRiskControl 施加风控冷却（连续命中次数递进，超阶梯置失效），并自动换代理。
//
// last_error / last_error_kind 由 StampAccountError 统一处理。风控有「账号级」与
// 「请求级」两种语义（本文件头注释），但冷却动作相同 —— 请求级时账号同样被上游标记，
// 本地冷却必须保留。snapshot 为本次请求的出口快照，避免迟到响应重复换线。
func MarkRiskControl(st *store.Store, snapshot *model.Account, errMsg string, now time.Time, attempt ...RiskAttempt) (secs, streak int, invalid bool) {
	_, _ = st.UpdateForRequest(snapshot, func(acc *model.Account) {
		secs, streak, invalid = model.ApplyRiskControl(acc, errMsg, now, st.RiskCoolingSteps())
	})
	if streak == 0 {
		// 已删除、人工停用或已换凭据：不更新状态/换线，仍给日志一个可用的档位。
		secs, invalid = riskControlCoolingSeconds(st, 1)
		return secs, streak, invalid
	}
	observed := RiskAttempt{Egress: st.ProxyEgress(snapshot)}
	if len(attempt) > 0 {
		observed = attempt[0]
	}
	result, err := rotateRiskProxy(st, snapshot, observed, now)
	from, to := observed.Egress, st.ProxyEgress(st.FindAny(snapshot.ID))
	if result.Reason == "changed" {
		copy := snapshot.Clone()
		copy.ProxyID, copy.ProxyURL = &result.Next.ID, &result.Next.URL
		to = st.ProxyEgress(copy)
	}
	state := model.StatusCooling
	if invalid {
		state = model.StatusInvalid
	}
	action := "风控后未更换代理，保留当前绑定与冷却/失效状态"
	if result.Reason == "changed" {
		action = "风控后已更换代理，保留风控冷却/失效状态"
	}
	_, recheck := st.FindAny(snapshot.ID).LocalSchedulingBarrier(now, config.RiskSilentWindow())
	// history_blocked兼容旧解析器；新增名字明确这是候选代理排除数。
	msg := fmt.Sprintf("账号 %s %s req=%q ticket=%q result=%s from_id=%q from_route=%q from_endpoint=%q from_mode=%s to_id=%q to_route=%q to_endpoint=%q to_mode=%s history_blocked=%d history_ttl_s=%d state=%s streak=%d cooling_s=%d history_blocked_candidates=%d history_scope=account_route_candidates local_recheck_at=%s",
		snapshot.ID, action, observed.RequestID, observed.Ticket, result.Reason, from.ID, from.Label, from.Endpoint, from.Mode, to.ID, to.Label, to.Endpoint, to.Mode,
		result.HistoryBlocked, int(store.RiskProxyHistoryTTL.Seconds()), state, streak, secs, result.HistoryBlocked, diagnosticTimestamp(recheck))
	if err != nil {
		msg += fmt.Sprintf(" error=%q", err.Error())
	}
	web.Warn("risk-proxy", msg)
	return secs, streak, invalid
}

// rotateRiskProxy 是「风控后换线」的唯一实现，冷却与不冷却两条路径共用。
//
// 强制直连 / 无效代理回退 / 注入客户端没有使用绑定线路时不动绑定：那些出口不是
// 账号自己的选择，换掉会污染它的失败历史。
func rotateRiskProxy(st *store.Store, snapshot *model.Account, observed RiskAttempt, now time.Time) (store.ProxyRotation, error) {
	result := store.ProxyRotation{Reason: "egress_override"}
	if observed.Egress.Mode != "proxy" && observed.Egress.Mode != "direct" {
		return result, nil
	}
	return st.RotateAccountProxyAt(snapshot, now)
}

// NoteRiskControlWithoutCooling 记录一次「线路归因」的 405：落错误章与命中时刻、
// 换一条线路，但**不改账号状态**（不冷却、不推进 RiskControlStreak）。
//
// 这是 2026-10-17 日志的直接结论：405 过的账号换线即可成功（5/5），首次 405 冷却
// 账号等于拿池子容量替脏线买单。只有当同号在不同线路上再次 405 时，才由调用方
// 升级到 MarkRiskControl 的阶梯冷却。
func NoteRiskControlWithoutCooling(st *store.Store, snapshot *model.Account, errMsg string, now time.Time, attempt ...RiskAttempt) {
	_, _ = st.UpdateForRequest(snapshot, func(acc *model.Account) {
		ts := float64(now.UnixNano()) / 1e9
		kind := model.ErrorKindRiskControl
		acc.LastErrorKind, acc.LastErrorAt, acc.LastError = &kind, &ts, &errMsg
		acc.LastRiskControlAt = &ts
	})
	observed := RiskAttempt{Egress: st.ProxyEgress(snapshot)}
	if len(attempt) > 0 {
		observed = attempt[0]
	}
	if st.FindAny(snapshot.ID) == nil {
		return // 账号已被并发删除：不换线、不落日志
	}
	result, err := rotateRiskProxy(st, snapshot, observed, now)
	from, to := observed.Egress, st.ProxyEgress(st.FindAny(snapshot.ID))
	if result.Reason == "changed" {
		copy := snapshot.Clone()
		copy.ProxyID, copy.ProxyURL = &result.Next.ID, &result.Next.URL
		to = st.ProxyEgress(copy)
	}
	streak := 0
	if live := st.FindAny(snapshot.ID); live != nil {
		streak = live.RiskControlStreak
	}
	msg := fmt.Sprintf("账号 %s 命中风控（HTTP 405，线路归因）本次不冷却 req=%q ticket=%q result=%s from_id=%q from_route=%q to_id=%q to_route=%q streak=%d cooling_s=0",
		snapshot.ID, observed.RequestID, observed.Ticket, result.Reason, from.ID, from.Label, to.ID, to.Label, streak)
	if err != nil {
		msg += fmt.Sprintf(" error=%q", err.Error())
	}
	web.Warn("risk-proxy", msg)
}
