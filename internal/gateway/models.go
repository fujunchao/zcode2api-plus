// 模型权益（entitlement）：把「对外支持清单」与「账号池当前实际拥有的额度」分开。
//
// 背景：2026-11-24 生产核实，80 个 zai/jwt（Start Plan）账号的额度快照只剩
// glm-5.3-flash 与 glm-5.4，GLM-5.3 归零；Start Plan 官方目录 revision 48 也已把
// 5.3 换成 5.4。支持清单仍静态列着 GLM-5.3，客户端挑中后只会拿到 503
// no_available_account——客户端/new-api 会把它当临时故障反复重试，把一次必然失败的
// 请求放大成整池压力。本文件负责三件事：
//   - 只公布「池内确有额度的模型」（/v1/models）；
//   - 判定「模型确定性不可用」（每个非归档账号对该模型的权益都已确定，且都是不含）；
//   - 给出同口径的繁体文案与结构化 details（sync 400 与 async no_account 共用）。
//
// 三件事共用同一份权益口径：/v1/models 公布的清单、400 的判定依据、管理端额度卡
// （accounts==0 且 snapshot_accounts>0）回答的是同一个问题——「池子里到底有没有这个模型」。
//
// 权益分四态，判定时必须分开（2026-11-24 评审修正）：
//   - 持有（Accounts/Available）：账号快照里有该模型的额度列；
//   - 权益未知（Unknown）：账号还没刷新过额度快照、或旧快照模型身份不明。它可能就
//     能用这个模型，所以既不能当「确定没有」，也不能从 /v1/models 里摘掉；
//   - 已到期（Expired）：额度列全部是「不会自动刷新的已过期列」——余额数字还在，
//     实际已经作废（上游回 1005），不该继续支撑对外清单与「确定不含」判定；
//   - 确定不含（absent，见 model.Account.ModelAvailability）：有快照、快照里没有该模型。
package gateway

import (
	"fmt"
	"strings"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// ModelEntitlement 单个模型在账号池里的权益分布（一个非归档账号只落一个桶）。
// Accounts 是持有该模型额度条目的账号数（不代表还有余量）；
// Available 是其中 ModelAvailability 判定为 available 的账号数；
// Unknown 是权益无法确定的账号数（没有额度快照、旧快照模型身份不明等；持有该模型
// 条目但列里缺数值的账号仍计入 Accounts——每个账号只落一个桶，不重复计数）；
// Expired 是持有该模型、但额度列全是「已到期且不会自动刷新」的账号数。
type ModelEntitlement struct {
	Model     string `json:"model"`
	Accounts  int    `json:"accounts"`
	Available int    `json:"available"`
	Unknown   int    `json:"unknown"`
	Expired   int    `json:"expired"`
}

// ModelEntitlements 按 AvailableModels 顺序汇总每个模型的权益分布。
// 归档账号不计：归档即停止调用，不该继续支撑对外清单。
// now 只用于判断额度列是否已到期（Expired），不影响其他口径。
func ModelEntitlements(accounts []*model.Account, now time.Time) []ModelEntitlement {
	entitlements := make([]ModelEntitlement, 0, len(AvailableModels))
	for _, name := range AvailableModels {
		entitlement := ModelEntitlement{Model: name}
		for _, account := range accounts {
			if account == nil || account.ArchivedAt != nil {
				continue
			}
			switch {
			// 已作废的额度列不支撑权益：仍算持有会让下架/到期的模型继续挂在 /v1/models 上。
			case account.ModelEntitlementExpired(name, now):
				entitlement.Expired++
			case len(account.QuotaEntriesForModel(name)) > 0:
				entitlement.Accounts++
				if account.ModelAvailability(name) == "available" {
					entitlement.Available++
				}
			case account.ModelAvailability(name) == "unknown":
				// 没快照 / 旧快照身份不明：可能可以用，不能当「确定没有」。
				entitlement.Unknown++
			}
		}
		entitlements = append(entitlements, entitlement)
	}
	return entitlements
}

// EntitledModels 当前池内有权益的模型（顺序严格跟 AvailableModels）。
//
// 收录条件是「有账号持有」**或**「有账号权益未知」。后者是混合池的常态——JWT 账号
// 会刷额度，API Key 账号从不刷：把未知当「不含」会把 API Key 能用的模型从清单里
// 藏掉（2026-11-24 评审）。已到期（Expired）不收录：那部分配额实际已作废。
//
// 一个都没有时退化为完整 AvailableModels：新装、还没刷新过额度快照的部署不能因为
// 没有快照就拿到空的 /v1/models——那会让客户端直接无法选模型。
func EntitledModels(accounts []*model.Account, now time.Time) []string {
	models := make([]string, 0, len(AvailableModels))
	for _, entitlement := range ModelEntitlements(accounts, now) {
		if entitlement.Accounts > 0 || entitlement.Unknown > 0 {
			models = append(models, entitlement.Model)
		}
	}
	if len(models) == 0 {
		return append([]string(nil), AvailableModels...)
	}
	return models
}

// SnapshotAccountCount 池内已有额度快照（Quota 非空）且未归档的账号数。
//
// 与 model.SummarizeModelQuotas 的 snapshot_accounts 同口径：它回答「这个池级的
// 0 账号到底意味着什么」——有快照才算「确实没有这个模型」，一个快照都没有只是
// 「还没刷新过，不知道」。它是池级读数，不进「确定不含」判定（判定见
// DeterministicModelUnavailable）。
func SnapshotAccountCount(accounts []*model.Account) int {
	snapshots := 0
	for _, account := range accounts {
		if account == nil || account.ArchivedAt != nil {
			continue
		}
		if len(account.Quota) > 0 {
			snapshots++
		}
	}
	return snapshots
}

// UnavailableAccountCount 池内「确定不可用」的非归档账号数，供错误文案与 details
// 的 N 使用。
//
// 口径与 DeterministicModelUnavailable 的判定完全同源：absent（有快照、快照里没有
// 该模型条目、且没有旧版「身份未知」痕迹）+ 已到期的额度列。两者必须一起数，
// 否则全池都是「余额还在但已到期」时，文案会写成「目前 0 個帳號的套餐均不含此模型」，
// 自相矛盾。权益未知（没快照 / 身份不明）不计入——它们不属于「确定」。
func UnavailableAccountCount(accounts []*model.Account, modelName string, now time.Time) int {
	name := model.NormalizeModelName(modelName)
	if name == "" {
		return 0
	}
	unavailable := 0
	for _, account := range accounts {
		if account == nil || account.ArchivedAt != nil {
			continue
		}
		if account.ModelAvailability(name) == "absent" || account.ModelEntitlementExpired(name, now) {
			unavailable++
		}
	}
	return unavailable
}

// DeterministicModelUnavailable 该模型是否「池内确定不含」：每个非归档账号对该模型
// 的权益都已确定，且都是 absent（已到期的额度列按 absent 计）。
//
// 为什么不再用「没有任何账号持有条目 + 至少一个账号有快照」（2026-11-24 评审）：
// 那会把没有快照的账号算成「已知不含」。反例是 79 个已刷新账号都不含 GLM-5.3、
// 外加 1 个从未刷新或身份未知的账号——那个账号刷新后可能就能用，回 400「永久别
// 重试」等于让客户端彻底放弃一个本来可用的模型。现在只有全部账号都已确定不含才判，
// 任何一个账号是 available / exhausted / disabled / unknown 都不判。
//
// 为什么不再看 store.PoolStat.ModelStat：ModelStat 只统计通过健康门槛的 Active 账号，
// 不含冷却中 / 风控静默 / 停用的账号。一个正在冷却、但**确实持有该模型**的账号不会
// 被计入，于是「其余健康账号都不含」会被误判成永久不可用并回 400——客户端据此不再
// 重试，比 503 更糟（冷却到期后它本可以成功）。改用运维能一眼核对的账号维度判据。
//
// 该判据与 /v1/models 的权益口径、管理端额度卡（accounts==0 且 snapshot_accounts>0）
// 完全一致：三处对同一池况给出同一个答案。
func DeterministicModelUnavailable(accounts []*model.Account, modelName string, now time.Time) bool {
	name := model.NormalizeModelName(modelName)
	if name == "" {
		return false
	}
	absent := 0
	for _, account := range accounts {
		if account == nil || account.ArchivedAt != nil {
			continue
		}
		// 已到期的额度列等同 absent：快照里还有余额数字，但其实已经作废
		// （2026-11-23 的 GLM-5.3 就是 remaining>0 + expires_at 已过，上游回 1005）。
		if account.ModelAvailability(name) == "absent" || account.ModelEntitlementExpired(name, now) {
			absent++
			continue
		}
		return false
	}
	return absent > 0
}

// SuggestedModels 池内当前有权益、且不等于 exclude 的模型（顺序跟 AvailableModels，
// 大小写不敏感比较）。仅用于错误文案的「請改用 …」，因此没有权益时返回空列表而不是
// 退化成完整清单：不能让错误文案推荐一个池子里同样没有的模型。
//
// 只推 Accounts>0 的模型：权益未知（Unknown）与已到期（Expired）都不算「池里确实
// 可用」，推荐过去照样会失败。
func SuggestedModels(accounts []*model.Account, exclude string, now time.Time) []string {
	skip := model.NormalizeModelName(exclude)
	suggested := make([]string, 0, len(AvailableModels))
	for _, entitlement := range ModelEntitlements(accounts, now) {
		if entitlement.Accounts == 0 {
			continue
		}
		if skip != "" && model.NormalizeModelName(entitlement.Model) == skip {
			continue
		}
		suggested = append(suggested, entitlement.Model)
	}
	return suggested
}

// DeterministicUnavailableMessage 「模型确定性不可用」的繁体文案。
//
// sync（400 model_not_available）与 async（200 SSE 里的 no_account 事件）共用同一份
// 文案：同一池况在两条路径上不能各说各话（与 poolstatus.go 同一原则）。
// unavailableAccounts 是「确定不可用该模型的非归档账号数」（UnavailableAccountCount：
// 快照里没有它，或它的额度列已到期）——文案里的数字限定在接受过判定的账号上，
// 不把权益未知的账号算成「套餐不含」。没有替代模型时退化为不带「請改用」的版本。
func DeterministicUnavailableMessage(modelName string, unavailableAccounts int, suggested []string) string {
	if len(suggested) == 0 {
		return fmt.Sprintf("模型 %s 無可用帳號：目前 %d 個帳號的套餐均不含此模型（該模型可能已下架、未開通或已到期）",
			modelName, unavailableAccounts)
	}
	return fmt.Sprintf("模型 %s 無可用帳號：目前 %d 個帳號的套餐均不含此模型（該模型可能已下架、未開通或已到期），請改用 %s",
		modelName, unavailableAccounts, strings.Join(suggested, "、"))
}

// DeterministicUnavailableDetails 「模型确定性不可用」的结构化 details。
//
// 以 PoolStatusDetails 为底（total_accounts / active / cooling / model_unavailable …），
// 再补齐契约字段：model（请求模型）、active_accounts（健康账号数）、
// unavailable_accounts（判定口径本身：确定不可用该模型的账号数）、
// snapshot_accounts（池级：已刷新额度的账号数，用来说明判定样本有多大）、
// suggested_models（仅在池内确有替代模型时输出）。
//
// unavailable_accounts 与 snapshot_accounts 在「确定不可用」分支里数值通常相同
// （每个账号都得有快照才可能判 absent），但含义不同：前者进判定，后者是池级读数。
func DeterministicUnavailableDetails(stat store.PoolStat, modelName string, unavailableAccounts, snapshotAccounts int, suggested []string) map[string]any {
	details := PoolStatusDetails(stat, modelName)
	details["model"] = modelName
	details["active_accounts"] = stat.Active
	details["unavailable_accounts"] = unavailableAccounts
	details["snapshot_accounts"] = snapshotAccounts
	if len(suggested) > 0 {
		details["suggested_models"] = suggested
	}
	return details
}
