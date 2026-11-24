// 模型权益（entitlement）：把「对外支持清单」与「账号池当前实际拥有的额度」分开。
//
// 背景：2026-11-24 生产核实，80 个 zai/jwt（Start Plan）账号的额度快照只剩
// glm-5.3-flash 与 glm-5.4，GLM-5.3 归零；Start Plan 官方目录 revision 48 也已把
// 5.3 换成 5.4。支持清单仍静态列着 GLM-5.3，客户端挑中后只会拿到 503
// no_available_account——客户端/new-api 会把它当临时故障反复重试，把一次必然失败的
// 请求放大成整池压力。本文件负责三件事：
//   - 只公布「池内确有额度的模型」（/v1/models）；
//   - 判定「模型确定性不可用」（池内没有任何账号持有它，且池子确实刷新过额度）；
//   - 给出同口径的繁体文案与结构化 details（sync 400 与 async no_account 共用）。
//
// 三件事共用同一份权益口径：/v1/models 公布的清单、400 的判定依据、管理端额度卡
// （accounts==0 且 snapshot_accounts>0）回答的是同一个问题——「池子里到底有没有这个模型」。
package gateway

import (
	"fmt"
	"strings"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// ModelEntitlement 单个模型在账号池里的权益分布。
// Accounts 是持有该模型额度条目的非归档账号数（不代表还有余量）；
// Available 是其中 ModelAvailability 判定为 available 的账号数。
type ModelEntitlement struct {
	Model     string `json:"model"`
	Accounts  int    `json:"accounts"`
	Available int    `json:"available"`
}

// ModelEntitlements 按 AvailableModels 顺序汇总每个模型的权益分布。
// 归档账号不计：归档即停止调用，不该继续支撑对外清单。
func ModelEntitlements(accounts []*model.Account) []ModelEntitlement {
	entitlements := make([]ModelEntitlement, 0, len(AvailableModels))
	for _, name := range AvailableModels {
		entitlement := ModelEntitlement{Model: name}
		for _, account := range accounts {
			if account == nil || account.ArchivedAt != nil {
				continue
			}
			if len(account.QuotaEntriesForModel(name)) == 0 {
				continue
			}
			entitlement.Accounts++
			if account.ModelAvailability(name) == "available" {
				entitlement.Available++
			}
		}
		entitlements = append(entitlements, entitlement)
	}
	return entitlements
}

// EntitledModels 当前池内有权益的模型（顺序严格跟 AvailableModels）。
//
// 一个都没有时退化为完整 AvailableModels：新装、还没刷新过额度快照的部署不能因为
// 没有快照就拿到空的 /v1/models——那会让客户端直接无法选模型。
func EntitledModels(accounts []*model.Account) []string {
	models := make([]string, 0, len(AvailableModels))
	for _, entitlement := range ModelEntitlements(accounts) {
		if entitlement.Accounts > 0 {
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
// 「还没刷新过，不知道」。
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

// DeterministicModelUnavailable 该模型是否「池内确定不含」：没有任何非归档账号
// 持有它的额度条目，且至少有一个账号刷新过额度（否则无从判断）。
//
// 为什么不再看 store.PoolStat.ModelStat（2026-11-24 修正）：
// ModelStat 只统计通过健康门槛的 Active 账号，不含冷却中 / 风控静默 / 停用的账号。
// 一个正在冷却、但**确实持有该模型**的账号不会被计入，于是「其余健康账号都不含」
// 会被误判成永久不可用并回 400——客户端据此不再重试，比 503 更糟（冷却到期后它
// 本可以成功）。改用运维能一眼核对的账号维度判据：
//   - 只要还有一个非归档账号持有该模型的额度条目 → 此刻没号（冷却/耗尽/停用都
//     可能），一律留 503 让客户端可以重试；
//   - 一个额度快照都没有（新装 / 刚入池还没刷新）→ 无从判断，同样留 503。
//
// 该判据与 /v1/models 的权益口径、管理端额度卡（accounts==0 且 snapshot_accounts>0）
// 完全一致：三处对同一池况给出同一个答案。
func DeterministicModelUnavailable(accounts []*model.Account, modelName string) bool {
	name := model.NormalizeModelName(modelName)
	if name == "" {
		return false
	}
	for _, account := range accounts {
		if account == nil || account.ArchivedAt != nil {
			continue
		}
		if len(account.QuotaEntriesForModel(name)) > 0 {
			return false
		}
	}
	return SnapshotAccountCount(accounts) > 0
}

// SuggestedModels 池内当前有权益、且不等于 exclude 的模型（顺序跟 AvailableModels，
// 大小写不敏感比较）。仅用于错误文案的「請改用 …」，因此没有权益时返回空列表而不是
// 退化成完整清单：不能让错误文案推荐一个池子里同样没有的模型。
func SuggestedModels(accounts []*model.Account, exclude string) []string {
	skip := model.NormalizeModelName(exclude)
	suggested := make([]string, 0, len(AvailableModels))
	for _, entitlement := range ModelEntitlements(accounts) {
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
// snapshotAccounts 是「已刷新额度的非归档账号数」——文案里的数字限定在真正取过
// 快照的账号上，避免把还没刷新额度的账号算成「套餐不含」。
// 没有替代模型时退化为不带「請改用」的版本。
func DeterministicUnavailableMessage(modelName string, snapshotAccounts int, suggested []string) string {
	if len(suggested) == 0 {
		return fmt.Sprintf("模型 %s 無可用帳號：目前 %d 個帳號的套餐均不含此模型（該模型可能已下架或未開通）",
			modelName, snapshotAccounts)
	}
	return fmt.Sprintf("模型 %s 無可用帳號：目前 %d 個帳號的套餐均不含此模型（該模型可能已下架或未開通），請改用 %s",
		modelName, snapshotAccounts, strings.Join(suggested, "、"))
}

// DeterministicUnavailableDetails 「模型确定性不可用」的结构化 details。
//
// 以 PoolStatusDetails 为底（total_accounts / active / cooling / model_unavailable …），
// 再补齐契约字段：model（请求模型）、active_accounts（健康账号数）、
// snapshot_accounts（已刷新额度的账号数，判定依据就落在它上面）、
// suggested_models（仅在池内确有替代模型时输出）。
func DeterministicUnavailableDetails(stat store.PoolStat, modelName string, snapshotAccounts int, suggested []string) map[string]any {
	details := PoolStatusDetails(stat, modelName)
	details["model"] = modelName
	details["active_accounts"] = stat.Active
	details["snapshot_accounts"] = snapshotAccounts
	if len(suggested) > 0 {
		details["suggested_models"] = suggested
	}
	return details
}
