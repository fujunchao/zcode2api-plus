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

import "strings"

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
