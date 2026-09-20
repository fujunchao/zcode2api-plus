package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 日志预览要能一眼看出「是哪一类失败」：业务码/类型 + 可读文案，单行、截断。
// 只记状态码时，1305 平台过载与风控 405 这类关键信息在 body 里的失败在日志里
// 完全不可见——这是这次线上排查最大的障碍。
func TestErrorPreview(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"zcode-plan 业务码形态", `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`,
			"code=1305 该模型当前访问量过大，请您稍后再试"},
		{"Anthropic 错误形态", `{"error":{"type":"overloaded_error","message":"Overloaded"}}`,
			"type=overloaded_error Overloaded"},
		{"msg 优先于 message", `{"code":9999,"msg":"first","message":"second"}`, "code=9999 first"},
		{"只有 code", `{"code":1310}`, "code=1310"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorPreview(tc.body); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}

	t.Run("非 JSON 退回原文（去空白）", func(t *testing.T) {
		if got := ErrorPreview("  plain text error  "); got != "plain text error" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("多行压成单行", func(t *testing.T) {
		if got := ErrorPreview("line1\n\n   line2"); got != "line1 line2" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("超长按 rune 截断且不留半个字符", func(t *testing.T) {
		got := ErrorPreview(strings.Repeat("长", 500))
		runes := []rune(got)
		if len(runes) > errorPreviewLimit+1 {
			t.Fatalf("预览未被截断: %d 个字符", len(runes))
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("截断应留下省略号: %q", got)
		}
		if strings.ContainsRune(got, '\uFFFD') {
			t.Fatalf("截断不应留下半个 UTF-8 字符: %q", got)
		}
	})
}

// RecordAccountError 只写「归类 + 时间」，不碰账号状态与失败计数；
// detail 为空时必须保留既有 last_error 文案——客户端取消走这条路，用户按一下 ESC
// 不该把上一次真实故障的文案冲掉。
func TestRecordAccountError(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "a", "sk-1")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1700000000, 500000000)
	RecordAccountError(st, model.ProviderZai, acc.ID, model.ErrorKindClientCanceled, "", now)

	got := st.Find(model.ProviderZai, acc.ID)
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindClientCanceled {
		t.Fatalf("应写入错误归类: %v", got.LastErrorKind)
	}
	if got.LastErrorAt == nil || *got.LastErrorAt != 1700000000.5 {
		t.Fatalf("应写入错误时间: %v", got.LastErrorAt)
	}
	if got.LastError != nil {
		t.Fatalf("detail 为空时不应覆盖 last_error: %v", *got.LastError)
	}
	if got.Status != model.StatusActive || got.FailCount != 0 {
		t.Fatalf("RecordAccountError 不该动状态与失败计数: status=%s fail=%d", got.Status, got.FailCount)
	}

	// detail 非空 → 覆盖文案，归类同步更新。
	RecordAccountError(st, model.ProviderZai, acc.ID, model.ErrorKindRateLimited, "上游限流 HTTP 429", now)
	got = st.Find(model.ProviderZai, acc.ID)
	if got.LastError == nil || *got.LastError != "上游限流 HTTP 429" {
		t.Fatalf("detail 非空应覆盖 last_error: %v", got.LastError)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRateLimited {
		t.Fatalf("归类应被更新: %v", got.LastErrorKind)
	}
	if got.Status != model.StatusActive {
		t.Fatalf("归类更新不应改变状态: %s", got.Status)
	}
}

// stamp 语义的边界：detail 为空串是「保留旧文案」，而不是「把文案清空」。
func TestStampAccountErrorKeepsTextWhenDetailEmpty(t *testing.T) {
	old := "旧文案"
	acc := &model.Account{}
	acc.LastError = &old
	at := time.Unix(1700000001, 0)

	StampAccountError(acc, model.ErrorKindUpstreamOverload, "", at)
	if acc.LastError == nil || *acc.LastError != old {
		t.Fatalf("空 detail 不应覆盖文案: %v", acc.LastError)
	}
	if acc.LastErrorKind == nil || *acc.LastErrorKind != model.ErrorKindUpstreamOverload {
		t.Fatalf("归类应更新: %v", acc.LastErrorKind)
	}

	StampAccountError(acc, model.ErrorKindUpstreamOverload, "新文案", at)
	if acc.LastError == nil || *acc.LastError != "新文案" {
		t.Fatalf("非空 detail 应覆盖文案: %v", acc.LastError)
	}
}

// 风控判定必须靠 body：405 在本项目里有三种含义（风控拦截 / 计费重复查询 /
// 缺 system 注入），只看状态码会把第三种——我方请求构造缺陷——当成风控惩罚账号。
func TestIsRiskControlBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"实测原文", "request has been blocked due to unusual activity.", true},
		{"大小写无关", "Request has been BLOCKED due to UNUSUAL ACTIVITY.", true},
		{"blocked 同族", `{"error":{"message":"Request blocked."}}`, true},
		{"risk 兜底", `{"msg":"risk detected"}`, true},
		{"空 body", "", false},
		{"缺 system 注入的普通 405", `{"error":{"message":"method not allowed"}}`, false},
		{"其它 405 文案", "405 Method Not Allowed", false},
		{"非 JSON 且无风控词", "upstream refused", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := model.IsRiskControlBody(tc.body); got != tc.want {
				t.Fatalf("IsRiskControlBody(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// 风控阶梯：连续第 N 次命中取第 N 档；连续次数超过档位数则置失效。
// 默认 3 档 ⇒ 前 3 次冷却（5/15/60 分钟），第 4 次升 invalid。
func TestMarkRiskControlLadder(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "risk", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)

	// 前三档冷却，第四档升失效。
	want := []struct {
		secs    int
		invalid bool
	}{{300, false}, {900, false}, {3600, false}, {0, true}}
	for i, w := range want {
		secs, streak, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "风控拦截", now)
		if streak != i+1 || secs != w.secs || invalid != w.invalid {
			t.Fatalf("第 %d 次: got (secs=%d streak=%d invalid=%v) want (secs=%d streak=%d invalid=%v)",
				i+1, secs, streak, invalid, w.secs, i+1, w.invalid)
		}
		got := st.Find(model.ProviderZai, acc.ID)
		if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRiskControl {
			t.Fatalf("第 %d 次应归类为 risk_control: %v", i+1, got.LastErrorKind)
		}
		if got.LastError == nil || *got.LastError != "风控拦截" {
			t.Fatalf("第 %d 次应写入错误文案: %v", i+1, got.LastError)
		}
		if w.invalid {
			if got.Status != model.StatusInvalid {
				t.Fatalf("超限应置 invalid: %s", got.Status)
			}
			// 失效没有等待窗口，必须清掉冷却截止时间，否则前台会把它显示成一个
			// 到点就会自己好的冷却。
			if got.CoolingUntil != nil {
				t.Fatalf("超限应清掉 CoolingUntil: %v", got.CoolingUntil)
			}
			continue
		}
		if got.Status != model.StatusCooling {
			t.Fatalf("第 %d 次应 cooling: %s", i+1, got.Status)
		}
		if got.CoolingUntil == nil {
			t.Fatalf("第 %d 次应写入 CoolingUntil", i+1)
		}
		inWant := float64(now.Add(time.Duration(w.secs)*time.Second).UnixNano()) / 1e9
		if diff := *got.CoolingUntil - inWant; diff > 1 || diff < -1 {
			t.Fatalf("第 %d 次冷却截止应约 %d 秒后: got=%v want=%v", i+1, w.secs, *got.CoolingUntil, inWant)
		}
	}
}

// 成功调用（冷却到期后真正打通一次）把连续计数清零，回到最低档——
// 否则一次偶发拦截会永久抬高该账号的档位。
func TestResetRiskControlStreakReturnsToFirstRung(t *testing.T) {
	st := openStore(t)
	acc, _ := st.AddAccount(model.ProviderZai, "risk-reset", "sk-1")
	now := time.Unix(1700000000, 0)

	MarkRiskControl(st, model.ProviderZai, acc.ID, "第一次", now)
	if _, streak, _ := MarkRiskControl(st, model.ProviderZai, acc.ID, "第二次", now); streak != 2 {
		t.Fatalf("连续计数应为 2: %d", streak)
	}
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		ResetRiskControlStreak(a)
	}); err != nil {
		t.Fatal(err)
	}
	secs, streak, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "清零后再来", now)
	if streak != 1 || secs != 300 || invalid {
		t.Fatalf("清零后应从最低档重来: secs=%d streak=%d invalid=%v", secs, streak, invalid)
	}
}

// 阶梯设定驱动档位与升级点：档位数就是升级点，管理员改设定即可改「第几次封禁」。
func TestRiskCoolingStepsDriveEscalationPoint(t *testing.T) {
	st := openStore(t)
	acc, _ := st.AddAccount(model.ProviderZai, "risk-steps", "sk-1")
	now := time.Unix(1700000000, 0)

	if err := st.SetSetting(store.RiskCoolingStepsKey, "60,120"); err != nil {
		t.Fatal(err)
	}
	if secs, _, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "1", now); secs != 60 || invalid {
		t.Fatalf("第 1 档应为 60: %d %v", secs, invalid)
	}
	if secs, _, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "2", now); secs != 120 || invalid {
		t.Fatalf("第 2 档应为 120: %d %v", secs, invalid)
	}
	// 只有两档 ⇒ 第 3 次即失效（默认三档时是第 4 次）。
	if _, streak, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "3", now); !invalid || streak != 3 {
		t.Fatalf("两档设定下第 3 次应失效: streak=%d invalid=%v", streak, invalid)
	}
	if got := st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusInvalid {
		t.Fatalf("应置 invalid: %s", got.Status)
	}
}

// 设定缺失或非法时回退默认，绝不返回空切片（空切片会让「第 1 次命中」直接越界成失效）。
func TestRiskCoolingStepsFallback(t *testing.T) {
	st := openStore(t)
	acc, _ := st.AddAccount(model.ProviderZai, "risk-fallback", "sk-1")
	now := time.Unix(1700000000, 0)

	for _, bad := range []string{"", "0", "-5", "abc", "300,,900", "300,abc,900", "   "} {
		if err := st.SetSetting(store.RiskCoolingStepsKey, bad); err != nil {
			t.Fatal(err)
		}
		steps := st.RiskCoolingSteps()
		if len(steps) != 3 || steps[0] != 300 || steps[1] != 900 || steps[2] != 3600 {
			t.Fatalf("非法设定 %q 应回退默认三档: %v", bad, steps)
		}
	}
	// 默认设定下第 1 次命中仍是 300s（证明回退真的生效，而不是空阶梯直接判失效）。
	if secs, _, invalid := MarkRiskControl(st, model.ProviderZai, acc.ID, "x", now); secs != 300 || invalid {
		t.Fatalf("回退后应取默认首档: secs=%d invalid=%v", secs, invalid)
	}
}

func TestIsCaptchaError(t *testing.T) {
	if !IsCaptchaError("F001", 400, http.Header{}) {
		t.Fatal("F001 + 400 应识别为验证码挑战")
	}
	if !IsCaptchaError(`{"error":"F001"}`, 403, http.Header{}) {
		t.Fatal("F001 + 403 应识别为验证码挑战")
	}
	if IsCaptchaError("F001", 500, http.Header{}) {
		t.Fatal("5xx 的 F001 不应重分类")
	}
	if IsCaptchaError("invalid token", 403, http.Header{}) {
		t.Fatal("非验证码鉴权错误不应被重分类")
	}

	header := http.Header{"X-Aliyun-Captcha-Verify-Param": []string{"challenge"}}
	if !IsCaptchaError("ordinary response", 200, header) {
		t.Fatal("验证码响应头应识别为挑战")
	}
	if !IsCaptchaError(`{"code":3007,"msg":"captcha verify failed"}`, 400, http.Header{}) {
		t.Fatal("code 3007 应识别为挑战")
	}
	if !IsCaptchaError(`{"error":{"code":3007}}`, 400, http.Header{}) {
		t.Fatal("嵌套 error.code=3007 应识别为挑战")
	}
	if !IsCaptchaError("please verify token again", 403, http.Header{}) {
		t.Fatal("verify token 文本应识别为挑战")
	}
	if IsCaptchaError("please verify token again", 500, http.Header{}) {
		t.Fatal("5xx 的验证码文本不应重分类")
	}
}

func TestUpstreamBusinessCode(t *testing.T) {
	if got := UpstreamBusinessCode(`{"code":1310,"msg":"usage cap reached"}`); got != "1310" {
		t.Fatalf("zcode-plan 包装格式不符: %q", got)
	}
	if got := UpstreamBusinessCode(`{"error":{"message":"Authentication Failed","type":"1000"}}`); got != "1000" {
		t.Fatalf("Anthropic 风格不符: %q", got)
	}
	if got := UpstreamBusinessCode("not json"); got != "" {
		t.Fatalf("非 JSON 应返回空: %q", got)
	}
	if got := UpstreamBusinessCode(`{"error":"flat"}`); got != "" {
		t.Fatalf("扁平 error 无码应返回空: %q", got)
	}
	if got := UpstreamBusinessCode(`{}`); got != "" {
		t.Fatalf("空对象应返回空: %q", got)
	}
}

func TestIsModelConcurrencyLimit(t *testing.T) {
	if !IsModelConcurrencyLimit(429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`) {
		t.Fatal("429+3010 应识别为并发准入限制")
	}
	if !IsModelConcurrencyLimit(429, "model admission concurrency limit exceeded") {
		t.Fatal("文本形态也应识别")
	}
	if IsModelConcurrencyLimit(429, `{"code":3007,"msg":"captcha"}`) {
		t.Fatal("3007 不应误判为 3010")
	}
	if IsModelConcurrencyLimit(500, "model admission concurrency limit exceeded") {
		t.Fatal("非 429 不应识别")
	}
}

// 平台过载的判据必须同时覆盖「状态码 529」与「业务码 1305」，两种形态都要认：
// 线上实测 z.ai 在 Anthropic 兼容面用 529 承载 1305，而官方错误码表把它标成 429。
func TestIsUpstreamOverload(t *testing.T) {
	overloadBody := `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"529 + 1305（线上实测形态）", 529, overloadBody, true},
		{"529 + 空 body", 529, "", true},
		{"429 + 1305（官方错误码表形态）", 429, overloadBody, true},
		{"200 + 1305（包裹在成功码里）", 200, overloadBody, true},
		// Anthropic 原生过载形态没有 code 字段，只能靠状态码兜住。
		{"529 + Anthropic 原生 overloaded_error", 529,
			`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, true},
		// 不能把账户维度的限流与额度上限吞进来：它们的处置方式与过载完全不同。
		{"429 + 1302 账户限流", 429, `{"code":1302,"msg":"rate limited"}`, false},
		{"429 + 1310 用量上限", 429, `{"code":1310,"msg":"usage cap reached"}`, false},
		{"429 + 3010 并发准入", 429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`, false},
		{"402 额度用完", 402, `{"error":{"message":"payment required"}}`, false},
		{"503 服务不可用", 503, `{"error":{"message":"service unavailable"}}`, false},
		{"500 未识别错误", 500, `{"error":{"message":"boom"}}`, false},
		{"非 JSON body 不 panic", 529, "not-json", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUpstreamOverload(tc.status, tc.body); got != tc.want {
				t.Fatalf("IsUpstreamOverload(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestModelAllowed(t *testing.T) {
	for _, m := range []any{"glm-5.3-flash", "GLM-5.3", " glm-5.3 "} {
		if !ModelAllowed(m) {
			t.Fatalf("%v 应在开放清单内", m)
		}
	}
	// GLM-5_3 正規化後為 glm-5-3，不还原点号，与 Python 版同语义：不在清单内
	for _, m := range []any{"glm-5.2", "glm-5-turbo", "GLM-5_3", "", "gpt-4o"} {
		if ModelAllowed(m) {
			t.Fatalf("%v 不应在开放清单内", m)
		}
	}
}
