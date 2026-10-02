package model

import "math"

// ModelQuotaSummary 汇总一个模型的上游额度快照，不代表当前可调度额度。
// 数值为 nil 表示尚无有效数据，0 则表示已确认额度为零。
type ModelQuotaSummary struct {
	Model     string   `json:"model"`
	Total     *float64 `json:"total"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Accounts  int      `json:"accounts"`
	Items     int      `json:"items"`
	Partial   bool     `json:"partial"`
}

// SummarizeModelQuotas 按调用方给定的模型顺序汇总非归档账号的所有套餐。
// 使用与调度相同的精确模型匹配，兼容旧版以模型名作为键的快照。
// 停用、冷却、失效和模型耗尽标记不改写上游快照；未知模型不摊入其他模型。
// 调用方应传入独立账号快照，函数不修改账号，也不触发上游查询。
func SummarizeModelQuotas(accounts []*Account, models []string) []ModelQuotaSummary {
	result := make([]ModelQuotaSummary, 0, len(models))
	for _, name := range models {
		summary := ModelQuotaSummary{Model: name}
		for _, account := range accounts {
			if account == nil || account.ArchivedAt != nil {
				continue
			}
			entries := account.QuotaEntriesForModel(name)
			if len(entries) > 0 {
				summary.Accounts++
			}
			for _, entry := range entries {
				summary.Items++
				// 分别累加，不能短路：缺少一个字段时仍保留其他已知数值。
				totalOK := addQuotaValue(&summary.Total, entry["total"])
				usedOK := addQuotaValue(&summary.Used, entry["used"])
				remainingOK := addQuotaValue(&summary.Remaining, entry["remaining"])
				if !totalOK || !usedOK || !remainingOK {
					summary.Partial = true
				}
			}
		}
		result = append(result, summary)
	}
	return result
}

// 缺失、非法、负数及非有限值不当成零，也不污染整个响应的 JSON 编码。
func addQuotaValue(sum **float64, raw any) bool {
	value, err := toFloat(raw)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return false
	}
	if *sum == nil {
		*sum = &value
		return true
	}
	if math.IsInf(**sum+value, 0) {
		return false
	}
	**sum += value
	return true
}
