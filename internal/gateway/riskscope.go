// 风控保护按账号阶梯冷却；同请求达到不同账号阈值后停止换号并启用重放保护。
// 阈值仅限制进一步尝试，不诊断供应商的触发原因。默认在阶梯冷却之外叠加
// 24h 静默隔离（config.RiskSilentHours），窗口内的账号不参与选号；
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

// RiskControlRequestLevelThreshold 判定请求级风控所需的不同账号数。
// 1 = 关闭判定（退回「逐号冷却」的旧行为），作为线上紧急回退开关；默认 2。
var RiskControlRequestLevelThreshold = 2

// RiskScope 一次请求的风控判定范围。
//
// ⚠️ 必须按请求创建（engine.RunMessages / asyncpool.processTicket 内），不能挂在
// Engine 或 Pool 上：两者都是跨请求共享的，任何共享的可变状态都会让并发的两个请求互相
// 误判——A 请求的账号级风控会被 B 请求的命中计数顶成请求级。
//
// 导出供 asyncpool 复用：两条请求路径对同一份 body 必须给出相同的判定，否则「sync 安全、
// async 打穿池子」的分歧迟早出现。
type RiskScope struct {
	hit        map[string]bool // 给出过风控信号的账号 ID
	contentKey string          // 重放防护键（模型 + 内容指纹）；请求级判定成立时登记进 ReplayGuard
}

// NewRiskScope 创建一次请求的判定范围；contentKey 为重放防护键（可为空，空则不登记）。
func NewRiskScope(contentKey string) *RiskScope {
	return &RiskScope{hit: map[string]bool{}, contentKey: contentKey}
}

// ContentKey 重放防护键。
func (s *RiskScope) ContentKey() string {
	if s == nil {
		return ""
	}
	return s.contentKey
}

// Verdict 按不同账号去重记次；静默隔离中的账号（旧账）不参与本地停止换号阈值。
// 阈值只看本次请求新命中的账号；结论不表示正文就是风控原因。
func (s *RiskScope) Verdict(acc *model.Account, now time.Time) bool {
	if s == nil || acc == nil {
		return false
	}
	if !s.hit[acc.ID] && !acc.InRiskControlSilentWindow(now, config.RiskSilentWindow()) {
		s.hit[acc.ID] = true
	}
	return len(s.hit) >= RiskControlRequestLevelThreshold
}

// Accounts 已给出风控信号的不同账号数（供日志）。
func (s *RiskScope) Accounts() int {
	if s == nil {
		return 0
	}
	return len(s.hit)
}

// MarkRiskControl 施加风控冷却（连续命中次数递进，超阶梯置失效），并自动换代理。
//
// last_error / last_error_kind 由 StampAccountError 统一处理。风控有「账号级」与
// 「请求级」两种语义（本文件头注释），但冷却动作相同 —— 请求级时账号同样被上游标记，
// 本地冷却必须保留。snapshot 为本次请求的出口快照，避免迟到响应重复换线。
func MarkRiskControl(st *store.Store, snapshot *model.Account, errMsg string, now time.Time, attempt ...RiskAttempt) (secs, streak int, invalid bool) {
	_, _ = st.Update(snapshot.Provider, snapshot.ID, func(acc *model.Account) {
		acc.RiskControlStreak++
		streak = acc.RiskControlStreak
		// 持久化观测时间；默认在阶梯冷却之外叠加 RiskSilentHours（默认 24h）静默隔离。
		nowF := float64(now.UnixNano()) / 1e9
		acc.LastRiskControlAt = &nowF
		secs, invalid = riskControlCoolingSeconds(st, streak)
		if invalid {
			acc.Status = model.StatusInvalid
			acc.CoolingUntil = nil
			StampAccountError(acc, model.ErrorKindRiskControl, errMsg, now)
			return
		}
		until := float64(now.Add(time.Duration(secs)*time.Second).UnixNano()) / 1e9
		acc.Status = model.StatusCooling
		acc.CoolingUntil = &until
		StampAccountError(acc, model.ErrorKindRiskControl, errMsg, now)
	})
	if streak == 0 {
		// 账号已被删除（并发删除）：不落库，但仍给日志一个可用的档位。
		secs, invalid = riskControlCoolingSeconds(st, 1)
		return secs, streak, invalid
	}
	observed := RiskAttempt{Egress: st.ProxyEgress(snapshot)}
	if len(attempt) > 0 {
		observed = attempt[0]
	}
	result := store.ProxyRotation{Reason: "egress_override"}
	var err error
	// 强制直连/无效代理回退/注入客户端没有使用绑定线路，不污染它的失败历史。
	if observed.Egress.Mode == "proxy" || observed.Egress.Mode == "direct" {
		result, err = st.RotateAccountProxyAt(snapshot, now)
	}
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
