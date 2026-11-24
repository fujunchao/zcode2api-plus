// 模型权益（动态 /v1/models）与「确定性不可用」错误语义的回归测试。
//
// 背景：2026-11-24 生产核实，80 个 zai/jwt（Start Plan）账号的额度快照只剩
// glm-5.3-flash 与 glm-5.4，GLM-5.3 归零（Start Plan 官方目录 revision 48 已把
// 5.3 换成 5.4）。支持清单仍静态列着 GLM-5.3，客户端挑中后只会拿到 503
// no_available_account，中转层/new-api 又会把它当临时故障反复重试。本文件钉死四件事：
//   - /v1/models 只列池内有权益的模型（权益未知的账号也算有权益），全新池退回完整清单；
//   - 只有「每个账号都已确定不含该模型」才给确定性 400 model_not_available；
//   - 冷却/额度耗尽/1005/权益未知仍走 503，别把「此刻没号」或「还说不准」升级成「已下架」；
//   - 已作废的额度列（余额还在但 expires_at 已过且不会自动刷新）不算权益。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// testNow 固定权益判定用的时钟：到期口径必须可复现，不能跟着 time.Now() 抖动。
func testNow() time.Time { return time.Unix(1_760_000_000, 0) }

func TestModelEntitlementsOrderAndCounts(t *testing.T) {
	archived := 1.0
	accounts := []*model.Account{
		{Quota: map[string]map[string]any{"套餐": {"model": "GLM-5.3", "remaining": float64(0)}}},
		{Quota: map[string]map[string]any{"套餐": {"model": "glm-5.4", "remaining": float64(10)}}},
		{Quota: map[string]map[string]any{"套餐": {"model": "GLM-5.4", "remaining": float64(5), "available": float64(5)}}},
		// 归档账号不再支撑对外清单，即便它持有某个模型的额度条目。
		{ArchivedAt: &archived, Quota: map[string]map[string]any{"套餐": {"model": "GLM-5.3-Flash", "remaining": float64(100)}}},
		nil,
	}
	got := ModelEntitlements(accounts, testNow())
	if len(got) != len(AvailableModels) {
		t.Fatalf("权益清单长度应等于支持清单: %+v", got)
	}
	want := map[string][2]int{
		"glm-5.3-flash": {0, 0},
		"GLM-5.3":       {1, 0}, // 持有但额度已耗尽：算权益、不算可用
		"GLM-5.4":       {2, 2},
	}
	for i, name := range AvailableModels {
		if got[i].Model != name {
			t.Fatalf("第 %d 项应为 %s（顺序严格跟 AvailableModels）: %+v", i, name, got)
		}
		if got[i].Accounts != want[name][0] || got[i].Available != want[name][1] {
			t.Fatalf("模型 %s 权益不符: %+v", name, got[i])
		}
		// 这份夹具全是「有快照且条目已确定」的账号：未知与已到期两个桶必须为空。
		if got[i].Unknown != 0 || got[i].Expired != 0 {
			t.Fatalf("模型 %s 不应有未知/已到期账号: %+v", name, got[i])
		}
	}
}

// 权益四态要分开：持有/可用、未知（没刷过额度）、已到期（余额还在但配额作废）。
func TestModelEntitlementsUnknownAndExpired(t *testing.T) {
	now := testNow()
	accounts := []*model.Account{
		// 有快照，但快照里只有别的模型。
		{Quota: map[string]map[string]any{"套餐": {"model": "glm-5.3-flash", "remaining": float64(5)}}},
		// 从未刷新过额度：权益未知（混合池里的 API Key 账号就是这种形态）。
		{},
		// 持有 GLM-5.3，但额度列已到期且不会自动刷新：实际已作废。
		{Quota: map[string]map[string]any{"套餐": expiredQuota("GLM-5.3", 100, now)}},
	}
	got := ModelEntitlements(accounts, now)
	cases := map[string]ModelEntitlement{
		"glm-5.3-flash": {Model: "glm-5.3-flash", Accounts: 1, Available: 1, Unknown: 1},
		"GLM-5.3":       {Model: "GLM-5.3", Expired: 1, Unknown: 1},
		"GLM-5.4":       {Model: "GLM-5.4", Unknown: 1},
	}
	for _, entitlement := range got {
		if want, ok := cases[entitlement.Model]; ok {
			if entitlement != want {
				t.Fatalf("模型 %s 权益分布不符：got %+v want %+v", entitlement.Model, entitlement, want)
			}
			delete(cases, entitlement.Model)
		}
	}
	if len(cases) != 0 {
		t.Fatalf("缺少这些模型的权益分布: %v", cases)
	}
}

func TestEntitledModelsFallsBackToFullList(t *testing.T) {
	now := testNow()
	t.Run("只保留有权益的模型", func(t *testing.T) {
		accounts := []*model.Account{
			{Quota: map[string]map[string]any{"套餐": {"model": "glm-5.4", "remaining": float64(1)}}},
		}
		if got := EntitledModels(accounts, now); !reflect.DeepEqual(got, []string{"GLM-5.4"}) {
			t.Fatalf("应只保留有权益的模型: %v", got)
		}
	})

	// 权益未知的账号（混合池）必须为所有模型保留公布资格：它可能就能用，藏掉等于
	// 让客户端无法选择本来可用的模型。
	t.Run("权益未知也收录", func(t *testing.T) {
		accounts := []*model.Account{
			{Quota: map[string]map[string]any{"套餐": {"model": "glm-5.4", "remaining": float64(1)}}},
			{},
		}
		if got := EntitledModels(accounts, now); !reflect.DeepEqual(got, AvailableModels) {
			t.Fatalf("有权益未知的账号时三个模型都该保留: %v", got)
		}
	})

	for _, tc := range []struct {
		name     string
		accounts []*model.Account
	}{
		{"空池", nil},
		{"账号尚无快照", []*model.Account{{}, nil}},
		{"快照为空表", []*model.Account{{Quota: map[string]map[string]any{}}}},
		// 唯一持有的模型额度已作废：一个权益都不剩，必须退回完整清单。
		{"唯一模型已到期", []*model.Account{{Quota: map[string]map[string]any{"套餐": expiredQuota("GLM-5.3", 100, now)}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EntitledModels(tc.accounts, now)
			if !reflect.DeepEqual(got, AvailableModels) {
				t.Fatalf("没有权益时应退回完整支持清单: %v", got)
			}
			// 退化结果是副本：调用方改返回值不能污染全局支持清单。
			got[0] = "mutated"
			if AvailableModels[0] == "mutated" {
				t.Fatal("退化结果必须是 AvailableModels 的副本")
			}
		})
	}
}

// 判定口径是「账号维度」而不是「健康账号维度」：只要还有任何一个非归档账号持有
// 该模型，就只是此刻没号（冷却/耗尽/停用都可能），必须留 503。
func TestDeterministicModelUnavailable(t *testing.T) {
	now := testNow()
	archived := 1.0
	// 刷新过额度、但不含 GLM-5.3。
	withoutModel := map[string]map[string]any{"套餐": {"model": "glm-5.4", "remaining": float64(1)}}
	// 持有 GLM-5.3：额度为零也算持有（耗尽 ≠ 下架）。
	withModel := map[string]map[string]any{"套餐": {"model": "GLM-5.3", "remaining": float64(0)}}
	// 持有 GLM-5.3，但额度列已到期且不会自动刷新：配额实际已作废，等同不含。
	expiredModel := map[string]map[string]any{"套餐": expiredQuota("GLM-5.3", 100, now)}

	for _, tc := range []struct {
		name      string
		accounts  []*model.Account
		modelName string
		want      bool
	}{
		{"模型名为空", []*model.Account{{Quota: withoutModel}}, "", false},
		{"空池", nil, "GLM-5.3", false},
		{"一个额度快照都没有", []*model.Account{{}, nil}, "GLM-5.3", false},
		{"快照为空表不算刷新过", []*model.Account{{Quota: map[string]map[string]any{}}}, "GLM-5.3", false},
		{"没有任何账号持有该模型", []*model.Account{{Quota: withoutModel}, {Quota: withoutModel}}, "GLM-5.3", true},
		{"有账号持有该模型（额度已耗尽）", []*model.Account{{Quota: withoutModel}, {Quota: withModel}}, "GLM-5.3", false},
		{"只有归档账号持有该模型", []*model.Account{{Quota: withoutModel}, {Quota: withModel, ArchivedAt: &archived}}, "GLM-5.3", true},
		{"模型名写法归一后仍能匹配", []*model.Account{{Quota: withoutModel}, {Quota: withModel}}, "glm_5.3", false},
		// 权益未知的账号（没快照）刷新后可能就能用：不能判「确定缺失」。
		{"有快照不含 + 1 个权益未知", []*model.Account{{Quota: withoutModel}, {}}, "GLM-5.3", false},
		{"额度列已到期等同不含", []*model.Account{{Quota: expiredModel}}, "GLM-5.3", true},
		{"日额度过期仍在权益内", []*model.Account{{Quota: map[string]map[string]any{"套餐": dailyQuota("GLM-5.3", 100, now)}}}, "GLM-5.3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeterministicModelUnavailable(tc.accounts, tc.modelName, now); got != tc.want {
				t.Fatalf("判定不符（期望 %v）: %+v", tc.want, tc.accounts)
			}
		})
	}
}

// 池级快照口径：空快照、归档账号都不计入；停用账号的已取得快照仍然算。
func TestSnapshotAccountCount(t *testing.T) {
	archived := 1.0
	accounts := []*model.Account{
		{Quota: map[string]map[string]any{"套餐": {"model": "GLM-5.4", "remaining": float64(1)}}},
		{Status: model.StatusDisabled, Quota: map[string]map[string]any{"GLM-5.3": {"remaining": float64(1)}}},
		{Quota: map[string]map[string]any{}},
		{ArchivedAt: &archived, Quota: map[string]map[string]any{"GLM-5.3": {"remaining": float64(1)}}},
		nil,
	}
	if got := SnapshotAccountCount(accounts); got != 2 {
		t.Fatalf("已刷新额度的账号数应为 2: %d", got)
	}
	if got := SnapshotAccountCount(nil); got != 0 {
		t.Fatalf("空池应为 0: %d", got)
	}
}

// 错误文案里的 N 数「确定不可用」的账号：快照不含或额度已到期都算，权益未知不算。
func TestUnavailableAccountCount(t *testing.T) {
	now := testNow()
	archived := 1.0
	withoutModel := map[string]map[string]any{"套餐": {"model": "glm-5.4", "remaining": float64(1)}}
	expired := map[string]map[string]any{"套餐": {
		"model": "GLM-5.3", "remaining": float64(100),
		"period_start": float64(now.Unix() - 90*24*3600), "period_end": float64(now.Unix() - 24*3600),
		"expires_at": float64(now.Unix() - 24*3600),
	}}
	accounts := []*model.Account{
		{Quota: withoutModel},
		{Quota: withoutModel},
		{Quota: expired}, // 余额还在但已到期：确定不可用
		{ArchivedAt: &archived, Quota: withoutModel},
		{},                                   // 权益未知
		{Quota: map[string]map[string]any{}}, // 快照为空表：权益未知
		nil,
	}
	if got := UnavailableAccountCount(accounts, "GLM-5.3", now); got != 3 {
		t.Fatalf("确定不可用的账号数应为 3（2 个不含 + 1 个已到期；归档/未知都不算）: %d", got)
	}
	if got := UnavailableAccountCount(accounts, "", now); got != 0 {
		t.Fatalf("模型名为空应为 0: %d", got)
	}
	if got := UnavailableAccountCount(nil, "GLM-5.3", now); got != 0 {
		t.Fatalf("空池应为 0: %d", got)
	}
}

func TestSuggestedModels(t *testing.T) {
	now := testNow()
	accounts := []*model.Account{
		{Quota: map[string]map[string]any{
			"套餐-1": {"model": "glm-5.3-flash", "remaining": float64(100)},
			"套餐-2": {"model": "GLM-5.3", "remaining": float64(5)},
		}},
		{Quota: map[string]map[string]any{"套餐-3": {"model": "GLM-5.4", "remaining": float64(0)}}},
	}
	if got := SuggestedModels(accounts, "glm-5.3", now); !reflect.DeepEqual(got, []string{"glm-5.3-flash", "GLM-5.4"}) {
		t.Fatalf("应排除被下架模型并保持支持清单顺序: %v", got)
	}
	// 大小写不敏感：请求侧 glm/GLM 两种写法都会出现。
	if got := SuggestedModels(accounts, "GLM-5.4", now); !reflect.DeepEqual(got, []string{"glm-5.3-flash", "GLM-5.3"}) {
		t.Fatalf("大小写不敏感排除应生效: %v", got)
	}
	if got := SuggestedModels(nil, "GLM-5.3", now); len(got) != 0 {
		t.Fatalf("没有权益时不得推荐任何模型: %v", got)
	}
	// 只推确实有账号持有的模型：权益未知与已作废都不推——推过去照样会失败。
	expiredPool := []*model.Account{
		{Quota: map[string]map[string]any{"套餐": expiredQuota("GLM-5.3", 100, now)}},
		{},
	}
	if got := SuggestedModels(expiredPool, "GLM-5.4", now); len(got) != 0 {
		t.Fatalf("已到期与权益未知的模型都不该被推荐: %v", got)
	}
}

// /v1/models 只公布池内有权益的模型：官方下架 GLM-5.3 后，客户端不该再挑中它。
func TestModelsEndpointListsEntitledModelsOnly(t *testing.T) {
	f := newFixture(t)
	a, err := f.st.AddAccount(model.ProviderZai, "a", "sk-synthetic-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.st.AddAccount(model.ProviderZai, "b", "sk-synthetic-b")
	if err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, a, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})
	setSnapshot(t, f.st, b, map[string]float64{"GLM-5.4": 100})

	if got := modelIDs(t, f); !reflect.DeepEqual(got, []string{"glm-5.3-flash", "GLM-5.4"}) {
		t.Fatalf("应只公布池内有权益的模型: %v", got)
	}
}

// 混合池：JWT 账号有额度快照，API Key 账号从不刷额度（权益未知）→ /v1/models 不能
// 把 API Key 能用的模型藏掉，否则客户端在混合池里连 GLM-5.3 都选不到。
func TestModelsEndpointIncludesUnknownEntitlements(t *testing.T) {
	f := newFixture(t)
	withSnapshot, err := f.st.AddAccount(model.ProviderZai, "jwt-with-snapshot", "sk-synthetic-jwt")
	if err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, withSnapshot, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})
	if _, err := f.st.AddAccount(model.ProviderZai, "api-key-no-quota", "sk-synthetic-key"); err != nil {
		t.Fatal(err)
	}

	if got := modelIDs(t, f); !reflect.DeepEqual(got, AvailableModels) {
		t.Fatalf("权益未知的账号应让三个模型都保留: %v", got)
	}
}

// 全新池（有账号但还没刷过额度）必须退回完整清单，不能让客户端无模型可选。
func TestModelsEndpointFallsBackForFreshPool(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.AddAccount(model.ProviderZai, "fresh", "sk-synthetic-fresh"); err != nil {
		t.Fatal(err)
	}
	if got := modelIDs(t, f); !reflect.DeepEqual(got, AvailableModels) {
		t.Fatalf("没有额度快照时应退回完整支持清单: %v", got)
	}
}

// 池内没有任何账号持有请求模型（且每个账号的权益都已确定）→ 确定性 400：客户端据此
// 换模型，而不是把一次必然失败的请求当临时故障反复重投。
func TestModelUnavailableReturnsDeterministic400(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a", "b"} {
		acc, err := f.st.AddAccount(model.ProviderZai, name, "sk-synthetic-"+name)
		if err != nil {
			t.Fatal(err)
		}
		setSnapshot(t, f.st, acc, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})
	}
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Error("确定性不可用不应触达上游")
		return http.StatusBadGateway, nil, ""
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusBadRequest {
		t.Fatalf("应 400 而不是 503: %d %s", status, raw)
	}
	errObj := decodeError(t, raw)
	if errObj["type"] != "model_not_available" {
		t.Fatalf("type 不符: %v", errObj)
	}
	msg, _ := errObj["message"].(string)
	for _, want := range []string{"GLM-5.3", "可能已下架", "已到期", "請改用", "glm-5.3-flash", "GLM-5.4"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message 缺少 %q: %s", want, msg)
		}
	}
	details, _ := errObj["details"].(map[string]any)
	if details == nil {
		t.Fatalf("错误体缺少 details: %s", raw)
	}
	if details["model"] != "GLM-5.3" || detailInt(t, details["total_accounts"]) != 2 ||
		detailInt(t, details["active_accounts"]) != 2 || detailInt(t, details["snapshot_accounts"]) != 2 ||
		detailInt(t, details["unavailable_accounts"]) != 2 {
		t.Fatalf("details 计数不符（须含 active_accounts / unavailable_accounts / snapshot_accounts）: %v", details)
	}
	unavailable, _ := details["model_unavailable"].(map[string]any)
	if detailInt(t, unavailable["absent"]) != 2 {
		t.Fatalf("model_unavailable 应报 absent=2: %v", details)
	}
	suggested, _ := details["suggested_models"].([]any)
	if len(suggested) != 2 || suggested[0] != "glm-5.3-flash" || suggested[1] != "GLM-5.4" {
		t.Fatalf("suggested_models 不符: %v", details["suggested_models"])
	}
	if f.callCount() != 0 {
		t.Fatalf("确定性不可用不得消耗上游调用: %d", f.callCount())
	}
}

// 权益未知（从未刷新过额度的账号）刷新后可能就能用：必须留 503 让客户端可以重试，
// 不能升级成「永久别重试」的 400。
//
// 用生产形态复现：79 个账号刷新过快照、都不含 GLM-5.3，另加 1 个从未刷新的账号。
// 旧判据（「没有账号持有条目 + 至少一个账号有快照」）会把这 1 个算成「已知不含」，
// 于是回 400 让客户端彻底放弃一个刷新后可能就能用的模型。
func TestUnknownEntitlementKeeps503(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 79; i++ {
		acc, err := f.st.AddAccount(model.ProviderZai, fmt.Sprintf("refreshed-%02d", i), fmt.Sprintf("sk-synthetic-%02d", i))
		if err != nil {
			t.Fatal(err)
		}
		setSnapshot(t, f.st, acc, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "unknown", "sk-synthetic-unknown"); err != nil {
		t.Fatal(err)
	}
	// 权益未知的账号仍会被选中尝试，用上游 503 把它冷却掉，最终落到「此刻没号」。
	f.respond = jsonResp(http.StatusServiceUnavailable, "{\"error\":{\"message\":\"upstream maintenance\"}}")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("权益未知时应 503 而不是 400: %d %s", status, raw)
	}
	errObj := decodeError(t, raw)
	if errObj["type"] != "no_available_account" {
		t.Fatalf("type 不应改成确定性不可用: %v", errObj)
	}
}

// 「此刻没号」仍是 503：两个用例里池内都有账号**持有**该模型的额度条目（只是额度
// 耗尽或被冷却），按账号维度判定都不构成「下架」，必须留 503 让客户端可以重试。
func TestNoAvailableAccountStaysRetryable(t *testing.T) {
	t.Run("混有冷却与模型耗尽", func(t *testing.T) {
		f := newFixture(t)
		cooling, err := f.st.AddAccount(model.ProviderZai, "cooling", "sk-synthetic-cooling")
		if err != nil {
			t.Fatal(err)
		}
		until := float64(time.Now().Add(120*time.Second).UnixNano()) / 1e9
		kind := model.ErrorKindUpstreamUnavailable
		if _, err := f.st.Update(model.ProviderZai, cooling.ID, func(a *model.Account) {
			a.Status = model.StatusCooling
			a.CoolingUntil = &until
			a.LastErrorKind = &kind
		}); err != nil {
			t.Fatal(err)
		}
		exhausted, err := f.st.AddAccount(model.ProviderZai, "exhausted-model", "sk-synthetic-exhausted")
		if err != nil {
			t.Fatal(err)
		}
		setSnapshot(t, f.st, exhausted, map[string]float64{"GLM-5.3": 0, "GLM-5.4": 100})

		status, raw := f.post(t, msgBody(), "sk-test")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("混有冷却/模型耗尽时应保持 503: %d %s", status, raw)
		}
		errObj := decodeError(t, raw)
		if errObj["type"] != "no_available_account" {
			t.Fatalf("type 不应改成确定性不可用: %v", errObj)
		}
		msg, _ := errObj["message"].(string)
		if !strings.Contains(msg, "冷卻中") || !strings.Contains(msg, "額度已用完") {
			t.Fatalf("message 应如实分解池况: %s", msg)
		}
	})

	t.Run("全部账号该模型额度已用完", func(t *testing.T) {
		f := newFixture(t)
		for _, name := range []string{"a", "b"} {
			acc, err := f.st.AddAccount(model.ProviderZai, name, "sk-synthetic-"+name)
			if err != nil {
				t.Fatal(err)
			}
			setSnapshot(t, f.st, acc, map[string]float64{"GLM-5.3": 0, "GLM-5.4": 100})
		}
		status, raw := f.post(t, msgBody(), "sk-test")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("额度耗尽应仍 503（可重试）: %d %s", status, raw)
		}
		errObj := decodeError(t, raw)
		if errObj["type"] != "no_available_account" {
			t.Fatalf("type 不符: %v", errObj)
		}
		msg, _ := errObj["message"].(string)
		if !strings.Contains(msg, "額度已用完") {
			t.Fatalf("message 应说明是额度耗尽: %s", msg)
		}
		details, _ := errObj["details"].(map[string]any)
		unavailable, _ := details["model_unavailable"].(map[string]any)
		if detailInt(t, unavailable["exhausted"]) != 2 {
			t.Fatalf("model_unavailable 应报 exhausted=2: %v", details)
		}
	})
}

// 冷却账号的**快照**里也没有该模型 → 池子确实没有这个模型，仍判 400：冷却到期也
// 不会让它多出这个模型。快照必须存在，否则那是「权益未知」，只能留 503。
func TestDeterministicUnavailableWhenCoolingAccountLacksModel(t *testing.T) {
	f := newFixture(t)
	cooling, err := f.st.AddAccount(model.ProviderZai, "cooling", "sk-synthetic-cooling")
	if err != nil {
		t.Fatal(err)
	}
	until := float64(time.Now().Add(120*time.Second).UnixNano()) / 1e9
	if _, err := f.st.Update(model.ProviderZai, cooling.ID, func(a *model.Account) {
		a.Status = model.StatusCooling
		a.CoolingUntil = &until
	}); err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, cooling, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})
	absent, err := f.st.AddAccount(model.ProviderZai, "absent", "sk-synthetic-absent")
	if err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, absent, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusBadRequest {
		t.Fatalf("冷却账号的快照也不含该模型时应判 400: %d %s", status, raw)
	}
	details, _ := decodeError(t, raw)["details"].(map[string]any)
	if detailInt(t, details["cooling"]) != 1 || detailInt(t, details["active_accounts"]) != 1 ||
		detailInt(t, details["snapshot_accounts"]) != 2 || detailInt(t, details["unavailable_accounts"]) != 2 {
		t.Fatalf("details 应如实报出 1 个冷却账号与 2 个确定不含的账号: %v", details)
	}
}

// 冷却账号**持有**该模型的额度条目（额度为零也算持有），其余健康账号不含 → 只是
// 「此刻没号」，必须 503 而不是 400。
//
// 这正是「按 PoolStat.ModelStat 判 absent」会踩的坑：ModelStat 只统计 Active，正在
// 冷却的持有者被算漏，于是「其余账号都不含」被误判成永久下架，客户端从此不再重试。
func TestCoolingAccountHoldingModelKeeps503(t *testing.T) {
	f := newFixture(t)
	cooling, err := f.st.AddAccount(model.ProviderZai, "cooling-holder", "sk-synthetic-holder")
	if err != nil {
		t.Fatal(err)
	}
	until := float64(time.Now().Add(120*time.Second).UnixNano()) / 1e9
	if _, err := f.st.Update(model.ProviderZai, cooling.ID, func(a *model.Account) {
		a.Status = model.StatusCooling
		a.CoolingUntil = &until
	}); err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, cooling, map[string]float64{"GLM-5.3": 0, "GLM-5.4": 100})
	absent, err := f.st.AddAccount(model.ProviderZai, "absent", "sk-synthetic-absent")
	if err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, absent, map[string]float64{"glm-5.3-flash": 100, "GLM-5.4": 100})

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("池内仍有账号持有该模型时应 503（可重试）: %d %s", status, raw)
	}
	if errObj := decodeError(t, raw); errObj["type"] != "no_available_account" {
		t.Fatalf("type 不应改成确定性不可用: %v", errObj)
	}
}

// 已作废的额度列（余额还在，但 expires_at 已过且不会自动刷新）不支撑权益：
// /v1/models 不再公布它，全池如此时请求直接 400。
//
// 生产对照：2026-11-23 的 GLM-5.3 就是「快照 remaining>0 + expires_at 已过」，上游回
// 1005；只看剩余额度会把它当仍在售，客户端于是一直重试一个必然失败的模型。
func TestExpiredEntitlementIsNotPublished(t *testing.T) {
	now := time.Now()
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "expired", "sk-synthetic-expired")
	if err != nil {
		t.Fatal(err)
	}
	setQuotaColumns(t, f.st, acc, map[string]map[string]any{
		"套餐-1": expiredQuota("GLM-5.3", 100, now), // 余额 100，但配额已作废
		"套餐-2": dailyQuota("GLM-5.4", 100, now),   // 日额度，未过期
	})

	if got := modelIDs(t, f); !reflect.DeepEqual(got, []string{"GLM-5.4"}) {
		t.Fatalf("已到期的额度列不该支撑对外清单: %v", got)
	}
}

// 全池的该模型额度都已作废 → 400 model_not_available（而不是 503 让客户端重试）。
// 余额取 0 是为了让账号不被选中：仍在「有余额」的已作废账号依旧会先被选去打上游，
// 那条路径的行为本次不改（见 ModelEntitlementExpired 的注释：只影响权益层）。
func TestExpiredEntitlementReturnsDeterministic400(t *testing.T) {
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "expired", "sk-synthetic-expired")
	if err != nil {
		t.Fatal(err)
	}
	setQuotaColumns(t, f.st, acc, map[string]map[string]any{
		"套餐-1": expiredQuota("GLM-5.3", 0, time.Now()),
		"套餐-2": dailyQuota("GLM-5.4", 100, time.Now()),
	})
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Error("确定性不可用不应触达上游")
		return http.StatusBadGateway, nil, ""
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusBadRequest {
		t.Fatalf("全池额度已作废应 400: %d %s", status, raw)
	}
	errObj := decodeError(t, raw)
	if errObj["type"] != "model_not_available" {
		t.Fatalf("type 不符: %v", errObj)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "已到期") {
		t.Fatalf("message 应说明可能已到期: %s", msg)
	}
}

// 日额度（period 跨度 ≈24h）过期只代表快照旧了，次日会自己刷新：仍算权益，
// /v1/models 继续公布，请求也仍是 503（可重试）而不是 400。
func TestDailyQuotaExpiryKeepsEntitlement(t *testing.T) {
	now := time.Now()
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "daily", "sk-synthetic-daily")
	if err != nil {
		t.Fatal(err)
	}
	setQuotaColumns(t, f.st, acc, map[string]map[string]any{
		"套餐-1": dailyQuota("GLM-5.3", 100, now),
	})

	if got := modelIDs(t, f); !reflect.DeepEqual(got, []string{"GLM-5.3"}) {
		t.Fatalf("日额度过期不该把模型从清单里摘掉: %v", got)
	}
	// 账号仍可被选中（余额还在），用上游 503 把它冷却掉，最终落到「此刻没号」。
	f.respond = jsonResp(http.StatusServiceUnavailable, "{\"error\":{\"message\":\"upstream maintenance\"}}")
	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("日额度过期应仍 503（可重试）: %d %s", status, raw)
	}
	if errObj := decodeError(t, raw); errObj["type"] != "no_available_account" {
		t.Fatalf("type 不应改成确定性不可用: %v", errObj)
	}
}

// 池里一个额度快照都没有：无从判断是否下架，一律按临时不可用处理（503）。这里用
// 上游 503 把两个账号逐级冷却，复现「整池暂不可用」的形态。
func TestNoSnapshotPoolKeeps503(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"a", "b"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-synthetic-"+name); err != nil {
			t.Fatal(err)
		}
	}
	f.respond = jsonResp(http.StatusServiceUnavailable, "{\"error\":{\"message\":\"upstream maintenance\"}}")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("没有任何额度快照时应 503 而不是 400: %d %s", status, raw)
	}
	errObj := decodeError(t, raw)
	if errObj["type"] != "no_available_account" {
		t.Fatalf("type 应为 no_available_account: %v", errObj)
	}
	details, _ := errObj["details"].(map[string]any)
	if detailInt(t, details["cooling"]) != 2 {
		t.Fatalf("两个账号都应冷却: %v", details)
	}
}

// 1005（200 包装的额度耗尽）行为不变：账号快照里 5.3 明明有余量，只是被上游判成
// 耗尽，此时绝不能被当成「全部 absent」而升级成 400。
func TestModelExhaustionAfter1005Stays503(t *testing.T) {
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "a", "sk-synthetic-a")
	if err != nil {
		t.Fatal(err)
	}
	setSnapshot(t, f.st, acc, map[string]float64{"GLM-5.3": 100})
	f.respond = jsonResp(http.StatusOK, `{"code":1005,"msg":"exceed quota limit"}`)

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable || !strings.Contains(raw, "no_available_account") {
		t.Fatalf("1005 仍应 503 no_available_account: %d %s", status, raw)
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.ModelAvailability("GLM-5.3") != "exhausted" {
		t.Fatalf("1005 应只把该模型标成耗尽: %s", got.ModelAvailability("GLM-5.3"))
	}
}

// setSnapshot 给账号写一份「模型 → 剩余额度」的额度快照（0 即已耗尽）。
// 生产形态就是一个账号的套餐快照里同时含多个模型，因此这里按模型名建列。
func setSnapshot(t *testing.T, st *store.Store, acc *model.Account, remaining map[string]float64) {
	t.Helper()
	entries := make(map[string]map[string]any, len(remaining))
	for name, amount := range remaining {
		entries["套餐-"+model.NormalizeModelName(name)] = map[string]any{
			"model": name, "remaining": amount, "available": amount,
		}
	}
	setQuotaColumns(t, st, acc, entries)
}

// setQuotaColumns 直接写入额度快照：需要 period_start/period_end/expires_at 等字段时
// 用它（setSnapshot 只写「模型 → 剩余额度」）。名字与 linetruncate_test.go 里的
// setQuota 区分开，避免同包内重复声明。
func setQuotaColumns(t *testing.T, st *store.Store, acc *model.Account, entries map[string]map[string]any) {
	t.Helper()
	if _, err := st.Update(acc.Provider, acc.ID, func(a *model.Account) { a.Quota = entries }); err != nil {
		t.Fatal(err)
	}
}

// expiredQuota 构造一份「已到期且不会自动刷新」的额度列：period 跨度 > 36h（套餐/周/月
// 周期）且 expires_at 已过去。生产 2026-11-23 的 GLM-5.3 就是这种形态。
func expiredQuota(modelName string, remaining float64, now time.Time) map[string]any {
	start := float64(now.Add(-30 * 24 * time.Hour).Unix())
	end := float64(now.Add(-2 * 24 * time.Hour).Unix())
	return map[string]any{
		"model": modelName, "remaining": remaining, "available": remaining,
		"period_start": start, "period_end": end, "expires_at": end,
	}
}

// dailyQuota 构造一份「日额度」列：period 跨度 ≈24h，因此 expires_at 过期只说明快照
// 旧了，次日会自己刷新（不该被当成作废）。
func dailyQuota(modelName string, remaining float64, now time.Time) map[string]any {
	start := float64(now.Add(-24 * time.Hour).Unix())
	end := float64(now.Unix())
	return map[string]any{
		"model": modelName, "remaining": remaining, "available": remaining,
		"period_start": start, "period_end": end, "expires_at": end,
	}
}

// modelIDs 取 /v1/models 实际公布的模型 id（顺序即响应顺序）。
func modelIDs(t *testing.T, f *fixture) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models 应 200: %d %s", resp.StatusCode, raw)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("模型清单不是合法 JSON: %v %s", err, raw)
	}
	ids := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		ids = append(ids, item.ID)
	}
	return ids
}

// decodeError 解析错误体的 error 对象。
func decodeError(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v %s", err, raw)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("错误体缺少 error 对象: %s", raw)
	}
	return errObj
}

// detailInt 读取错误体 details 里的整数（错误体经过 JSON 往返，数值是 float64）。
func detailInt(t *testing.T, v any) int {
	t.Helper()
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("details 数值字段类型不符: %#v", v)
	}
	return int(n)
}
