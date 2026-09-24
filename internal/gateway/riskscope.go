// 风控的「账号级 / 请求级」分离：判定范围、前像快照与冷却回滚。
//
// 背景（2026-09-24 整池雪崩，docs/analysis-riskcontrol-pool-cascade-20260924.md）：
// 上游用 HTTP 405 + "unusual activity" 表达风控拦截，这句话有两种完全不同的语义，
// 而项目此前只有账号级一种处置——
//
//   - **账号级**：换号即成功。2026-09-23 全天 17 次零星风控，17/17 都在第 2 个账号上
//     拿到 200（attempts=2），说明拦截确实跟着身份走；
//   - **请求级**：同一 body 在多个不同账号 + 多个出口线路上全部被拒。09-24 当日 67/67，
//     14 次重放共冷却 67 个账号、把 70 号池整池清空，GLM-5.3 中断约 5 分钟。
//
// 判据：同一请求内 ≥2 个**不同账号**给出同一风控信号 ⇒ 身份已不是变量，剩下的只有请求体。
// 阈值取 2 的依据就是上面那组对照——账号级风控在第 2 个账号上就已经成功了。
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
// ⚠️ 必须按请求创建（engine.runWithAccounts / asyncpool.processTicket 内），不能挂在
// Engine 或 Pool 上：两者都是跨请求共享的，任何共享的可变状态都会让并发的两个请求互相
// 误判——A 请求的账号级风控会被 B 请求的命中计数顶成请求级。
//
// 导出供 asyncpool 复用：两条请求路径对同一份 body 必须给出相同的判定，否则「sync 安全、
// async 打穿池子」的分歧迟早出现。
type RiskScope struct {
	hit    map[string]bool // 给出过风控信号的账号 ID
	cooled []RiskControlSnapshot
}

// NewRiskScope 创建一次请求的判定范围。
func NewRiskScope() *RiskScope { return &RiskScope{hit: map[string]bool{}} }

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

// Record 登记一次已施加的风控冷却，供请求级判定成立时回滚。
func (s *RiskScope) Record(snap RiskControlSnapshot) {
	if s == nil {
		return
	}
	s.cooled = append(s.cooled, snap)
}

// Rollback 撤销本请求已施加的全部风控冷却，返回（成功回滚数, 放弃数）。
//
// 只回滚风控分支写下的快照：本请求内因 503/429 冷却的账号与这次 405 无关，回滚它们等于
// 篡改别的判定。已升级 invalid、或被并发改动的账号由 RollbackRiskControl 自行放弃。
func (s *RiskScope) Rollback(st *store.Store) (restored, skipped int) {
	if s == nil {
		return 0, 0
	}
	for _, snap := range s.cooled {
		ok, skip := RollbackRiskControl(st, snap)
		if ok {
			restored++
		} else if skip {
			skipped++
		}
	}
	return restored, skipped
}

// ── 风控冷却的前像 / 后像与回滚 ───────────────────────────────────────────────

// RiskControlSnapshot 一次风控冷却的前像与后像。
//
// 只覆盖**调度效果**三个字段（Status / CoolingUntil / RiskControlStreak），刻意不含
// last_error / last_error_kind / last_error_at：
//
//   - 那三个是**证据**。判定「这次拦截不该惩罚这个账号」不等于「这次拦截没发生过」，
//     抹掉会让后台再也看不见这次命中，下一次排查又回到「日志里什么都没有」的老路。
//   - 留痕不会误触发额度守卫：quota.isRiskControlInvalid 要求 Status==invalid 才读
//     LastErrorKind，而被回滚的账号必然是 active/cooling。
//
// Prev* 与 Written* 成对：前者用于还原，后者用于「当前值是否仍是我们写入的值」这个并发守卫。
type RiskControlSnapshot struct {
	Provider string
	ID       string

	PrevStatus string
	PrevUntil  *float64
	PrevStreak int

	WrittenInvalid bool
	WrittenUntil   *float64
	WrittenStreak  int
}

// MarkRiskControlWithSnapshot 与 MarkRiskControl 同语义，额外回传前像与后像。
//
// 两者必须共用同一段锁内逻辑（MarkRiskControl 是它的薄包装）：分开实现迟早漂移——
// 那条分支记前像、这条不记，回滚就会在其中一条路径上少字段。
func MarkRiskControlWithSnapshot(
	st *store.Store,
	provider, idOrName, errMsg string,
	now time.Time,
) (secs, streak int, invalid bool, snap RiskControlSnapshot) {
	snap.Provider = provider
	_, _ = st.Update(provider, idOrName, func(acc *model.Account) {
		snap.ID = acc.ID
		snap.PrevStatus = acc.Status
		snap.PrevUntil = cloneFloatPtr(acc.CoolingUntil)
		snap.PrevStreak = acc.RiskControlStreak

		acc.RiskControlStreak++
		streak = acc.RiskControlStreak
		snap.WrittenStreak = streak
		secs, invalid = riskControlCoolingSeconds(st, streak)
		if invalid {
			acc.Status = model.StatusInvalid
			acc.CoolingUntil = nil
			StampAccountError(acc, model.ErrorKindRiskControl, errMsg, now)
			snap.WrittenInvalid = true
			return
		}
		until := float64(now.Add(time.Duration(secs)*time.Second).UnixNano()) / 1e9
		acc.Status = model.StatusCooling
		acc.CoolingUntil = &until
		StampAccountError(acc, model.ErrorKindRiskControl, errMsg, now)
		snap.WrittenUntil = cloneFloatPtr(acc.CoolingUntil)
	})
	if streak == 0 {
		// 账号已被删除（并发删除）：不落库，但仍给日志一个可用的档位。
		secs, invalid = riskControlCoolingSeconds(st, 1)
	}
	return secs, streak, invalid, snap
}

// RollbackRiskControl 撤销一次风控冷却：请求级判定成立后释放被误伤的账号。
//
// 保守回滚，两道闸：
//
//  1. 已升级 invalid **不回滚**——走到 invalid 说明该账号的风控阶梯早已接近耗尽，本次
//     只是压垮它的最后一根稻草；复活它只会让下一个请求再吃一次同样的 405。
//  2. 只在「当前值仍等于我们写入的值」时才还原。期间被管理员改过状态、或另一路风控 /
//     成功路径动过这个账号，都说明决定权已不属于本请求，放弃回滚并在日志里报出跳过数。
//
// 返回 (restored, skipped)；账号已被并发删除时两者皆 false。
func RollbackRiskControl(st *store.Store, snap RiskControlSnapshot) (restored, skipped bool) {
	if snap.WrittenInvalid {
		return false, true
	}
	if snap.ID == "" {
		// 前像没落库（账号在写入前就被删了），无从回滚。
		return false, true
	}
	_, _ = st.Update(snap.Provider, snap.ID, func(acc *model.Account) {
		if acc.Status != model.StatusCooling ||
			acc.RiskControlStreak != snap.WrittenStreak ||
			!sameFloatPtr(acc.CoolingUntil, snap.WrittenUntil) {
			skipped = true
			return
		}
		acc.Status = snap.PrevStatus
		acc.CoolingUntil = cloneFloatPtr(snap.PrevUntil)
		acc.RiskControlStreak = snap.PrevStreak
		restored = true
	})
	return restored, skipped
}

// cloneFloatPtr 复制指针指向的值：快照必须与 live 对象解耦，否则「前像」会被后续写入
// 就地改掉（CoolingUntil 是 *float64，直接存指针就是存了个别名）。
func cloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// sameFloatPtr 比较两个可空时间戳是否相等（两个 nil 视为相等）。
func sameFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
