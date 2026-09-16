package model

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// pythonAccountJSON 模拟 Python 版 json.dumps(asdict(account)) 的输出形态：
// 全部 32 个键都在（含 null），Account 的 json tag 必须与之完全对齐。
const pythonAccountJSON = `{
  "id": "glm-acc-1a2b3c4d",
  "name": "test",
  "provider": "zai",
  "mode": "jwt",
  "email": null,
  "jwt_token": "header.payload.sig",
  "api_key": null,
  "enabled": true,
  "status": "active",
  "quota": {
    "GLM-5.3 · pro": {"total": 100, "used": 10, "remaining": 90, "available": 90,
      "period": "monthly", "period_start": null, "period_end": null, "expires_at": null,
      "model": "GLM-5.3", "plan_name": "pro", "plan_is_trial": false},
    "GLM-5.3-Flash · trial": {"total": 50, "used": 50, "remaining": 0, "available": 0,
      "period": "daily", "period_start": null, "period_end": null, "expires_at": null,
      "model": "GLM-5.3-Flash", "plan_name": "trial", "plan_is_trial": true}
  },
  "exhausted_models": [],
  "disabled_models": ["glm_5.3_flash"],
  "plan": {"plan_name": "GLM Coding Pro"},
  "plans": [{"plan_name": "GLM Coding Pro", "entitlements": []}],
  "usage": {},
  "use_count": 3,
  "fail_count": 1,
  "total_input_tokens": 11,
  "total_output_tokens": 22,
  "total_cache_creation_tokens": 2,
  "total_cache_read_tokens": 5,
  "last_used_at": 1700000000.0,
  "last_checked_at": 1700000000.5,
  "cooling_until": null,
  "last_error": null,
  "proxy_url": null,
  "proxy_id": null,
  "created_at": 1700000001.5,
  "archived_at": null,
  "user_id": "u-1700000000-a1b2c3",
  "virtual_device_mid": "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
  "claim": {"claimed_at": 1700000002.5, "next_at": 1700003600.0, "last_error": null}
}`

// pythonAccountKeys 序列化契约的全部键。
// 前四个 Go 增量字段：archived_at（归档）、user_id（身份判据）、
// virtual_device_mid（每账号设备指纹）、claim（领取状态）——Python 侧已退休，
// 旧版 json.loads 对多出的键会原样保留在 dict 中，不影响旧数据互读；
// 本测试现在保护的是 Go 自身的往返一致性。
var pythonAccountKeys = []string{
	"id", "name", "provider", "mode", "email", "jwt_token", "api_key",
	"enabled", "status", "quota", "exhausted_models", "disabled_models",
	"plan", "plans", "usage", "use_count", "fail_count",
	"total_input_tokens", "total_output_tokens", "total_cache_creation_tokens",
	"total_cache_read_tokens", "last_used_at", "last_checked_at", "cooling_until",
	"last_error", "proxy_url", "proxy_id", "created_at", "archived_at",
	"user_id", "virtual_device_mid", "claim",
}

func TestJSONContractWithPython(t *testing.T) {
	acc, err := FromJSON([]byte(pythonAccountJSON))
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}

	if acc.ID != "glm-acc-1a2b3c4d" || acc.Name != "test" || acc.Mode != "jwt" {
		t.Fatalf("基本字段不符: %+v", acc)
	}
	if acc.Secret() != "header.payload.sig" {
		t.Fatalf("Secret 不符: %q", acc.Secret())
	}
	if acc.Email != nil || acc.CoolingUntil != nil || acc.LastError != nil {
		t.Fatalf("null 字段应为 nil 指针")
	}
	if acc.LastUsedAt == nil || *acc.LastUsedAt != 1700000000.0 {
		t.Fatalf("last_used_at 不符: %v", acc.LastUsedAt)
	}
	if acc.TotalCacheCreationTokens != 2 || acc.TotalCacheReadTokens != 5 {
		t.Fatalf("token 统计不符")
	}
	if !reflect.DeepEqual(acc.ExhaustedModels, []string{}) {
		t.Fatalf("空列表应保持 [] 而非 nil: %v", acc.ExhaustedModels)
	}
	if acc.UserID == nil || *acc.UserID != "u-1700000000-a1b2c3" {
		t.Fatalf("user_id 不符: %v", acc.UserID)
	}
	if acc.VirtualDeviceMid == nil || *acc.VirtualDeviceMid != "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d" {
		t.Fatalf("virtual_device_mid 不符: %v", acc.VirtualDeviceMid)
	}
	if acc.Claim == nil || acc.Claim.ClaimedAt == nil || acc.Claim.NextAt == nil {
		t.Fatalf("claim 未解析: %+v", acc.Claim)
	}
	if *acc.Claim.ClaimedAt != 1700000002.5 || *acc.Claim.NextAt != 1700003600.0 {
		t.Fatalf("claim 时间不符: %+v", acc.Claim)
	}
	if acc.Claim.LastError != nil {
		t.Fatalf("claim.last_error 应为 nil: %v", acc.Claim.LastError)
	}
	// 嵌套对象的键名也要钉住，否则 claim 内部改错 tag 不会被上面那层发现。
	claimOut, err := json.Marshal(acc.Claim)
	if err != nil {
		t.Fatalf("claim 序列化失败: %v", err)
	}
	var claimKeys map[string]any
	if err := json.Unmarshal(claimOut, &claimKeys); err != nil {
		t.Fatalf("claim 回读失败: %v", err)
	}
	if len(claimKeys) != 3 {
		t.Fatalf("claim 键数量不符: %v", claimKeys)
	}
	for _, k := range []string{"claimed_at", "next_at", "last_error"} {
		if _, ok := claimKeys[k]; !ok {
			t.Fatalf("claim 缺少键 %s: %v", k, claimKeys)
		}
	}

	// 序列化回 JSON 后，键集合必须与 Python asdict 完全一致（双向互通的前提）。
	out, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	want := map[string]bool{}
	for _, k := range pythonAccountKeys {
		want[k] = true
	}
	if len(got) != len(want) {
		t.Fatalf("键数量不符: got %d, want %d；差异: %v vs %v", len(got), len(want), keysOf(got), pythonAccountKeys)
	}
	for k := range got {
		if !want[k] {
			t.Fatalf("多出的键: %s", k)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// jwtWithPayload 构造 header.<payload>.sig 形态的 token（payload 为明文 JSON）。
func jwtWithPayload(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return "header." + enc + ".sig"
}

func TestJWTUserID(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"user_id 优先", `{"user_id":"u-1","sub":"s-1"}`, "u-1"},
		{"仅 sub 时兜底", `{"sub":"s-1"}`, "s-1"},
		{"user_id 空串则看 sub", `{"user_id":"","sub":"s-1"}`, "s-1"},
		{"都没有", `{"email":"a@b.c"}`, ""},
		{"payload 非 JSON", `not-json`, ""},
		{"user_id 非字符串", `{"user_id":12345}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := JWTUserID(jwtWithPayload(tc.payload)); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	t.Run("畸形 token 不 panic", func(t *testing.T) {
		for _, token := range []string{"", "abc", "a.b", "..", "a..c", ".", "a.b.c.d"} {
			if got := JWTUserID(token); got != "" {
				t.Fatalf("token %q 应返回空串，实际 %q", token, got)
			}
		}
	})

	t.Run("带 base64 填充", func(t *testing.T) {
		// 部分实现保留 '=' 填充，两种解码器都要能处理。
		padded := base64.URLEncoding.EncodeToString([]byte(`{"user_id":"u-pad"}`))
		if got := JWTUserID("h." + padded + ".s"); got != "u-pad" {
			t.Fatalf("填充形态解析失败: %q", got)
		}
	})
}

func TestDeviceMidOr(t *testing.T) {
	acc := &Account{}
	if got := acc.DeviceMidOr("global"); got != "global" {
		t.Fatalf("未分配时应回退全局值: %q", got)
	}
	blank := "   "
	acc.VirtualDeviceMid = &blank
	if got := acc.DeviceMidOr("global"); got != "global" {
		t.Fatalf("空白值应视为未分配: %q", got)
	}
	mid := "mid-acc-1"
	acc.VirtualDeviceMid = &mid
	if got := acc.DeviceMidOr("global"); got != "mid-acc-1" {
		t.Fatalf("应优先用账号自己的指纹: %q", got)
	}
}

func TestClaimNextAt(t *testing.T) {
	acc := &Account{}
	if acc.ClaimNextAt() != nil {
		t.Fatal("无 claim 状态时应返回 nil")
	}
	acc.Claim = &ClaimState{}
	if acc.ClaimNextAt() != nil {
		t.Fatal("claim 存在但 next_at 缺失时应返回 nil")
	}
	next := 1700003600.0
	acc.Claim = &ClaimState{NextAt: &next}
	if got := acc.ClaimNextAt(); got == nil || *got != next {
		t.Fatalf("ClaimNextAt 不符: %v", got)
	}
}

func TestClaimStateRoundTrip(t *testing.T) {
	claimed, next := 1700000002.5, 1700003600.0
	msg := "上游限流"
	acc := &Account{ID: "a", Claim: &ClaimState{ClaimedAt: &claimed, NextAt: &next, LastError: &msg}}
	raw, err := json.Marshal(acc)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	back, err := FromJSON(raw)
	if err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if back.Claim == nil || back.Claim.ClaimedAt == nil || back.Claim.NextAt == nil || back.Claim.LastError == nil {
		t.Fatalf("claim 往返丢失字段: %+v", back.Claim)
	}
	if *back.Claim.ClaimedAt != claimed || *back.Claim.NextAt != next || *back.Claim.LastError != msg {
		t.Fatalf("claim 往返值不符: %+v", back.Claim)
	}
}

func TestNewFieldsSerializedAsNull(t *testing.T) {
	// 契约要求可空字段不带 omitempty：Python 的 json.dumps 会输出 null 键，
	// 缺键会让两边序列化形态不一致。
	raw, err := json.Marshal(&Account{})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	for _, k := range []string{"user_id", "virtual_device_mid", "claim"} {
		v, ok := got[k]
		if !ok {
			t.Fatalf("缺少键 %s", k)
		}
		if string(v) != "null" {
			t.Fatalf("%s 应为 null，实际 %s", k, v)
		}
	}
}

func TestCreate(t *testing.T) {
	jwt := Create(ProviderZai, "main", "header.payload.sig")
	if jwt.Mode != "jwt" || jwt.JWTToken == nil || jwt.APIKey != nil {
		t.Fatalf("jwt 凭证判定错误: %+v", jwt)
	}
	if jwt.Status != StatusActive || !jwt.Enabled || jwt.Provider != ProviderZai {
		t.Fatalf("默认状态错误: %+v", jwt)
	}
	key := Create(ProviderZai, "", "sk-abc.def")
	if key.Name != "zai-account" {
		t.Fatalf("空名称应兜底 zai-account: %q", key.Name)
	}
	if key.Mode != "apiKey" || key.APIKey == nil {
		t.Fatalf("apiKey 凭证判定错误")
	}
}

func TestIsSelectable(t *testing.T) {
	now := time.Unix(1700000000, 0)
	acc := &Account{Enabled: true, Status: StatusActive}
	if !acc.IsSelectable(now) {
		t.Fatal("active 应可选")
	}
	acc.Status = StatusExhausted
	if acc.IsSelectable(now) {
		t.Fatal("exhausted 不可选")
	}
	acc.Status = StatusInvalid
	if acc.IsSelectable(now) {
		t.Fatal("invalid 不可选")
	}
	acc.Status = StatusDisabled
	if acc.IsSelectable(now) {
		t.Fatal("disabled 不可选")
	}

	future := 1700000300.0
	past := 1699999700.0
	acc.Status = StatusCooling
	acc.CoolingUntil = &future
	if acc.IsSelectable(now) {
		t.Fatal("冷却未到期不可选")
	}
	acc.CoolingUntil = &past
	if !acc.IsSelectable(now) {
		t.Fatal("冷却到期应可选")
	}
	acc.CoolingUntil = nil
	if acc.IsSelectable(now) {
		t.Fatal("cooling 但无到期时间不可选")
	}
}

func TestModelAvailability(t *testing.T) {
	acc, _ := FromJSON([]byte(pythonAccountJSON))

	if got := acc.ModelAvailability("GLM-5.3"); got != "available" {
		t.Fatalf("GLM-5.3 应 available: %s", got)
	}
	if got := acc.ModelAvailability("glm_5.3_flash"); got != "disabled" {
		t.Fatalf("停用模型应 disabled（停用优先于快照耗尽）: %s", got)
	}
	if got := acc.ModelAvailability("GLM-4.7"); got != "absent" {
		t.Fatalf("快照中不存在的模型应 absent: %s", got)
	}
	// 快照列存在但缺 remaining 数值 → unknown
	acc.Quota["Weird"] = map[string]any{"total": 1}
	if got := acc.ModelAvailability("Weird"); got != "unknown" {
		t.Fatalf("缺数值额度列应 unknown: %s", got)
	}
	// 无任何快照 → unknown
	empty := &Account{Quota: map[string]map[string]any{}}
	if got := empty.ModelAvailability("GLM-5.3"); got != "unknown" {
		t.Fatalf("无快照应 unknown: %s", got)
	}
}

func TestSyncExhaustedModels(t *testing.T) {
	acc := &Account{
		Quota: map[string]map[string]any{
			"A": {"model": "GLM-5.3", "remaining": float64(0)},
			"B": {"model": "GLM-5.3", "remaining": float64(0)}, // 同模型多订阅均为 0 → 耗尽
			"C": {"model": "GLM-5.3-Flash", "remaining": float64(3)},
			"D": {"model": "GLM-5-Turbo"}, // 缺 remaining → 不参与判定
		},
	}
	acc.SyncExhaustedModels()
	want := []string{"glm-5.3"}
	if !reflect.DeepEqual(acc.ExhaustedModels, want) {
		t.Fatalf("耗尽模型不符: got %v want %v", acc.ExhaustedModels, want)
	}

	// 任一订阅恢复余额 → 自动解除耗尽
	acc.Quota["B"]["remaining"] = float64(7)
	acc.SyncExhaustedModels()
	if len(acc.ExhaustedModels) != 0 {
		t.Fatalf("有订阅恢复后应清空耗尽标记: %v", acc.ExhaustedModels)
	}
}

func TestDisabledAndExhaustedMarks(t *testing.T) {
	acc := &Account{}
	acc.SetDisabledModels([]string{"GLM_5.3-Flash", "", "glm-5.3-flash", "GLM-5.3"})
	want := []string{"glm-5.3-flash", "glm-5.3"}
	if !reflect.DeepEqual(acc.DisabledModels, want) {
		t.Fatalf("停用模型应正規化去重: %v", acc.DisabledModels)
	}

	if acc.MarkModelExhausted("") {
		t.Fatal("空模型名不能建立标记")
	}
	if !acc.MarkModelExhausted("GLM-5.3") || !acc.MarkModelExhausted("glm_5.3") {
		t.Fatal("标记失败")
	}
	if len(acc.ExhaustedModels) != 1 {
		t.Fatalf("重复标记不应追加: %v", acc.ExhaustedModels)
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"GLM-5.3": "glm-5.3",
		// 底线变体（Python 版同语义：只替换 _ 与空格，不还原点号）
		"glm_5.3_flash": "glm-5.3-flash",
		"glm_5_3_flash": "glm-5-3-flash",
		"  GLM--5.3  ":  "glm-5.3",
		"GLM 5.3 Flash": "glm-5.3-flash",
		"":              "",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Fatalf("NormalizeModelName(%v) = %q, want %q", in, got, want)
		}
	}
	if got := NormalizeModelName(nil); got != "" {
		t.Fatalf("NormalizeModelName(nil) = %q, want 空", got)
	}
}

func TestPlanTextAndTrial(t *testing.T) {
	if got := PlanText(map[string]any{"plan_name": "GLM Coding Pro"}); got != "GLM Coding Pro" {
		t.Fatalf("PlanText 单方案不符: %q", got)
	}
	multi := []any{
		map[string]any{"plan_name": "Pro"},
		map[string]any{"plan_name": "Pro"},
		map[string]any{"name": "Trial"},
	}
	if got := PlanText(multi); got != "Pro / Trial" {
		t.Fatalf("PlanText 多方案去重串接不符: %q", got)
	}
	if got := PlanText(map[string]any{}); got != "" {
		t.Fatalf("空方案应为空: %q", got)
	}

	if !IsTrialPlan(map[string]any{"plan_name": "GLM Coding 體驗版"}) {
		t.Fatal("名称含「體驗」应识别为体验方案")
	}
	if !IsTrialPlan(map[string]any{"is_trial": true}) {
		t.Fatal("is_trial=true 应识别为体验方案")
	}
	if IsTrialPlan(map[string]any{"plan_name": "GLM Coding Pro", "is_trial": false}) {
		t.Fatal("付费方案不应误判")
	}
}

func TestAccumulateAndPublicView(t *testing.T) {
	acc := &Account{
		Name:     "acc",
		Mode:     "jwt",
		JWTToken: strPtr("0123456789abcdef-verylongsecret"),
		Status:   StatusCooling,
		Plans:    []map[string]any{{"plan_name": "Pro"}},
		Quota:    map[string]map[string]any{},
	}
	acc.AccumulateTokens(Usage{Input: 10, Output: 20, CacheCreation: 1, CacheRead: 2})
	acc.AccumulateTokens(Usage{Input: 5})
	if acc.TotalInputTokens != 15 || acc.TotalOutputTokens != 20 || acc.TotalCacheCreationTokens != 1 || acc.TotalCacheReadTokens != 2 {
		t.Fatalf("累计不符: %+v", acc)
	}
	acc.ResetTokenStats()
	if acc.TotalInputTokens != 0 {
		t.Fatal("重置失败")
	}

	view := acc.PublicView(time.Now())
	if got := view["token_masked"]; got != "01234567…secret" {
		t.Fatalf("脱敏不符: %v", got)
	}
	if view["plan_name"] != "Pro" {
		t.Fatalf("plan_name 不符: %v", view["plan_name"])
	}
	if view["status"] != StatusCooling {
		t.Fatalf("cooling 状态应原样显示: %v", view["status"])
	}
	tokens := view["total_tokens"].(map[string]int)
	if tokens["input"] != 0 || tokens["output"] != 0 {
		t.Fatalf("重置后视图应为 0: %v", tokens)
	}
}

func strPtr(s string) *string { return &s }
