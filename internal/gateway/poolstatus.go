// no_available_account / no_account 出口的池状态分解：文案（繁体，供错误体
// message 与日志内联）与结构化 details（供中转层/客户端程序化读取）。
//
// 背景：2026-09-20 线上事故里，整池冷却导致的 503 被静态文案「帳號未提供此模型
// 或額度已用完」掩盖成绑定/额度问题（docs/analysis-503-no-available-account-
// 20260920.md）。此后错误体如实回答「此刻池子里每个账号为什么不可选」。
//
// sync（engine.RunMessages）与 async（asyncpool.processTicket）共用本文件，
// 两条路径的 no_account 出口必须给出同一套分解。
package gateway

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// coolingKindLabel 冷却成因 last_error_kind → 繁体短标签。未登记的 kind 显示
// 原始字符串（ErrorKind 枚举上线后不可改名，标签映射只增不减）。
var coolingKindLabels = map[string]string{
	model.ErrorKindUpstreamUnavailable: "上游503",
	model.ErrorKindRiskControl:         "風控",
	model.ErrorKindRateLimited:         "限流",
}

// PoolStatusText 生成池状态分解短语（不含外层括号），用于错误 message 与日志。
// 例：`4 帳號冷卻中（3 上游503、1 風控），最早 23:36:48 恢復；1 帳號此模型額度已用完`。
// 池为空（Total==0）时返回空串，调用方退回静态文案。
//
// 注意：短语里**不含账号名**——错误体会下发到 API 客户端（可能经中转层对外），
// 而账号名常为邮箱，属于不该外泄的信息；只给计数与成因。
func PoolStatusText(stat store.PoolStat) string {
	var parts []string
	if stat.Cooling > 0 {
		type kv struct {
			kind string
			n    int
		}
		var kvs []kv
		for k, n := range stat.CoolingByKind {
			kvs = append(kvs, kv{k, n})
		}
		// 稳定排序：数量降序，同数按 kind 名——同一池况两次生成必须得到同一句话，
		// 否则中转层的错误去重/告警会把它当两种错误。
		sort.Slice(kvs, func(i, j int) bool {
			if kvs[i].n != kvs[j].n {
				return kvs[i].n > kvs[j].n
			}
			return kvs[i].kind < kvs[j].kind
		})
		segs := make([]string, 0, len(kvs))
		for _, e := range kvs {
			label, ok := coolingKindLabels[e.kind]
			if e.kind == "" {
				label = "其他"
			} else if !ok {
				label = e.kind
			}
			segs = append(segs, fmt.Sprintf("%d %s", e.n, label))
		}
		head := fmt.Sprintf("%d 帳號冷卻中", stat.Cooling)
		if len(segs) > 0 {
			head += "（" + strings.Join(segs, "、") + "）"
		}
		if stat.CoolingEarliest != nil {
			head += fmt.Sprintf("，最早 %s 恢復",
				time.Unix(int64(*stat.CoolingEarliest), 0).Format("15:04:05"))
		}
		parts = append(parts, head)
	}
	if stat.Exhausted > 0 {
		parts = append(parts, fmt.Sprintf("%d 帳號全部額度已用完", stat.Exhausted))
	}
	if stat.Invalid > 0 {
		parts = append(parts, fmt.Sprintf("%d 帳號已失效", stat.Invalid))
	}
	if stat.Disabled > 0 {
		parts = append(parts, fmt.Sprintf("%d 帳號已停用", stat.Disabled))
	}
	if stat.Archived > 0 {
		parts = append(parts, fmt.Sprintf("%d 帳號已歸檔", stat.Archived))
	}
	// 模型维度只统计「通过健康门槛却被模型层排除」的账号；Select 仍返回 nil
	// 却存在 available/unknown 账号，只可能是它们都已在本轮尝试过（skipIDs）。
	if stat.ModelStat != nil {
		if n := stat.ModelStat["exhausted"]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d 帳號此模型額度已用完", n))
		}
		if n := stat.ModelStat["absent"]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d 帳號未提供此模型", n))
		}
		if n := stat.ModelStat["disabled"]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d 帳號已停用此模型", n))
		}
		if stat.ModelStat["available"]+stat.ModelStat["unknown"] > 0 {
			parts = append(parts, "可用帳號已於本輪嘗試過")
		}
	}
	return strings.Join(parts, "；")
}

// PoolStatusDetails 生成结构化池状态（错误体 error.details 字段，蛇形键）。
// 与 PoolStatusText 同一数据源：文案给人看、结构给程序看，两者不会各说各话。
// modelName 为空时不输出模型维度（Select 未按模型过滤，无此口径）。
func PoolStatusDetails(stat store.PoolStat, modelName string) map[string]any {
	details := map[string]any{
		"total_accounts": stat.Total,
		"active":         stat.Active,
		"cooling":        stat.Cooling,
		"exhausted":      stat.Exhausted,
		"invalid":        stat.Invalid,
		"disabled":       stat.Disabled,
		"archived":       stat.Archived,
	}
	if stat.CoolingEarliest != nil {
		details["cooling_earliest"] = *stat.CoolingEarliest
	}
	if len(stat.CoolingByKind) > 0 {
		details["cooling_by_kind"] = stat.CoolingByKind
	}
	if modelName != "" && stat.ModelStat != nil {
		unavailable := map[string]int{}
		for _, kind := range []string{"exhausted", "absent", "disabled"} {
			if n := stat.ModelStat[kind]; n > 0 {
				unavailable[kind] = n
			}
		}
		if len(unavailable) > 0 {
			details["model_unavailable"] = unavailable
		}
	}
	return details
}
