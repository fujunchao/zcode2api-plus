// 风控的「账号级 / 请求级」分离：判定范围与处置。
//
// 背景（2026-09-24 整池雪崩，docs/analysis-riskcontrol-pool-cascade-20260924.md；
// 两段式模型修正见 docs/analysis-auth-chain-vs-official.md §九）：
// 上游用 HTTP 405 + "unusual activity" 表达风控拦截，这句话有两种完全不同的语义，
// 而项目此前只有账号级一种处置——
//
//   - **账号级**：换号即成功。2026-09-23 全天 17 次零星风控，17/17 都在第 2 个账号上
//     拿到 200（attempts=2），说明拦截确实跟着身份走；
//   - **请求级**：同一 body 在多个不同账号 + 多个出口线路上全部被拒。09-24 当日 67/67，
//     14 次重放共冷却 67 个账号、把 70 号池整池清空。
//
// 判据：同一请求内 ≥2 个**不同账号**给出同一风控信号 ⇒ 身份已不是变量，剩下的只有请求体。
// 阈值取 2 的依据就是上面那组对照——账号级风控在第 2 个账号上就已经成功了。
//
// ⚠️ 请求级判定的处置语义已随账号级标记模型修正（2026-09-24 傍晚）：上游会给触发过
// 的账号**打服务端标记**（标记期内该账号无论发什么都 405，实测 ≥14h）。因此
// 「请求级 ⇒ 不冷却、回滚」的旧处置方向反了 —— 每个被尝试的账号都已被上游标记，
// 本地冷却必须**保留**（否则账号立即被再次选中、再吃一个 405），并停止换号、登记
// 重放防护（ReplayGuard）阻止同内容继续扩散标记。曾有的 Rollback/RiskControlSnapshot
// 机制随之整体移除：回滚冷却等于替服务端解封账号。
package gateway

import (
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
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

// Verdict 记一次风控命中并给出判定：true 表示已达阈值，本次应判为请求级。
//
// 按账号 **ID** 去重，而不是按命中次数：同一账号可能因验证码刷新 / 瞬时限流原地重试而
// 重复进入风控分支，按次数计会把「同一个号被拒两次」误判成请求级。
func (s *RiskScope) Verdict(acc *model.Account) bool {
	if s == nil || acc == nil {
		return false
	}
	s.hit[acc.ID] = true
	return len(s.hit) >= RiskControlRequestLevelThreshold
}

// Accounts 已给出风控信号的不同账号数（供日志）。
func (s *RiskScope) Accounts() int {
	if s == nil {
		return 0
	}
	return len(s.hit)
}

// MarkRiskControl 施加一次账号级风控冷却（连续命中次数递进，超阶梯置失效）。
//
// 只动调度效果三个字段（Status / CoolingUntil / RiskControlStreak）并盖证据章；
// last_error / last_error_kind 由 StampAccountError 统一处理。风控有「账号级」与
// 「请求级」两种语义（本文件头注释），但冷却动作相同 —— 请求级时账号同样被上游标记，
// 本地冷却必须保留。
func MarkRiskControl(st *store.Store, provider, idOrName, errMsg string, now time.Time) (secs, streak int, invalid bool) {
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		acc.RiskControlStreak++
		streak = acc.RiskControlStreak
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
	}
	return secs, streak, invalid
}
