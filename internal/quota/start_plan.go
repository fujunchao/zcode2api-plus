package quota

// missingInitialQuota 只接受完整的成功额度快照。空 preview、网络失败、缺字段均不能
// 证明 Start plan 未获配额；已有订阅或任何已分配/已使用额度也不得误当作新号空额度。
func missingInitialQuota(data map[string]any) bool {
	plans, plansOK := data["plans"].([]any)
	balances, balancesOK := data["balances"].([]any)
	if !plansOK || !balancesOK || len(plans) != 0 {
		return false
	}
	for _, raw := range balances {
		balance, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		// 没有总配额的非空余额条目属于未知，不按零额度推断。
		total, ok := asNumber(balance["total_units"])
		if !ok || total != 0 {
			return false
		}
		for _, key := range []string{"used_units", "remaining_units", "available_units"} {
			if raw, exists := balance[key]; exists {
				value, valid := asNumber(raw)
				if !valid || value != 0 {
					return false
				}
			}
		}
	}
	return true
}
