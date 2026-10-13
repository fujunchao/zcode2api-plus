// 上游风控拦截的识别。
//
// 这条判定必须放在唯一的权威实现里，因为它决定「要不要惩罚一个账号」。历史上
// 405 在本项目里有三种完全不同的含义：
//
//  1. 上游风控拦截（body 含 unusual activity / blocked）——真的该冷却账号；
//  2. 计费接口对重复查询回 405——幂等，可以安全忽略；
//  3. JWT 账号缺顶层 system 注入时上游也回 405（见 gateway/body.go 与
//     upstream/request.go 的说明）——那是我方构造请求的缺陷，换号与冷却都没用，
//     每个账号都会一样地失败。
//
// 只看状态码做冷却决策，等于把第 3 种（我们的 bug）变成对账号的集体惩罚。
// 因此判定必须落在 body 上。
package model

import (
	"strings"
	"time"
)

// IsRiskControlBody 判断上游错误体是否为风控拦截（大小写无关）。
//
// 三个关键词都保留：`unusual activity` 是实测原文（"request has been blocked due
// to unusual activity."），`blocked` 覆盖同族文案的其它措辞，`risk` 兜住上游改写。
func IsRiskControlBody(text string) bool {
	low := strings.ToLower(text)
	return strings.Contains(low, "unusual activity") ||
		strings.Contains(low, "blocked") ||
		strings.Contains(low, "risk")
}

// IsRiskControlInvalid 账号是否因上游风控被置为失效（账号级终态）。
//
// 与 IsRiskControlBody（判断一次响应是不是风控拦截）互补：这个判断的是「账号
// 当前是否处在风控失效状态」，两个条件必须同时成立：
//   - 只看 Status==invalid 不够：失效还有别的成因（凭据失效也置 invalid），
//     那些成因走各自的救济路径（如额度探测成功自动恢复），不属于风控处置；
//   - 只看 LastErrorKind 不够：错误归类会被后续失败覆盖，状态才是判决本身。
//
// 判定单一来源：quota 的额度保护（风控失效不被额度探测解封）与后台「手动恢复」
// 出口共用本函数，保证两边对「什么是风控失效」的理解永不漂移。
func IsRiskControlInvalid(acc *Account) bool {
	return acc.Status == StatusInvalid &&
		acc.LastErrorKind != nil && *acc.LastErrorKind == ErrorKindRiskControl
}

// InRiskControlSilentWindow 判断账号是否处于风控静默隔离窗口内（window<=0 关闭）。
// 这是本地计时器，不查询上游、不证明其仍被封禁。window<=0 时仅记录历史。
func (a *Account) InRiskControlSilentWindow(now time.Time, window time.Duration) bool {
	if a == nil || window <= 0 || a.LastRiskControlAt == nil {
		return false
	}
	return now.Before(unixTime(*a.LastRiskControlAt).Add(window))
}

// RiskControlSilentUntil 静默期结束时刻（Unix 秒）；无记录或 window <= 0 时为 nil。
// 供恢复护栏（保护冷却截止时刻 = 静默期终点）与展示使用。
func (a *Account) RiskControlSilentUntil(window time.Duration) *float64 {
	if a == nil || window <= 0 || a.LastRiskControlAt == nil {
		return nil
	}
	v := float64(unixTime(*a.LastRiskControlAt).Add(window).UnixNano()) / 1e9
	return &v
}
