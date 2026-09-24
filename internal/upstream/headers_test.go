// 出站请求头对齐官方客户端 3.14.3 的守卫（见 docs/plan-client-format-parity.md §1.2/§1.4）。
//
// 三组头的分工：
//   - 来源头：全部由 config.ZcodeClientProfile() 派生，与 body 的 # Environment 同源；
//   - 归因头：每请求必带，优先沿用下游值、缺失则合成；
//   - 鉴权/传输头。
//
// 本文件盯住的失败模式是「缺头」「多头」「头体不同源」三类——
// 三者都是上游可交叉比对的差异。
package upstream

import (
	"regexp"
	"strings"
	"testing"

	"zcode2api/internal/config"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestSourceHeadersMatchOfficial 官方来源头恒带 11 项（加 anthropic-version）。
// 缺任何一项都是差异——这正是修复前的问题：只发了 8 项里的 4 项。
func TestSourceHeadersMatchOfficial(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	prof := config.ZcodeClientProfile()
	want := map[string]string{
		// 模型请求的 UA 是 ai-sdk 拼的（带 provider-utils/runtime 后缀），
		// 与裸 `ZCode/<版本>`（非模型接口用）不同 —— 见 TestModelUserAgentShape。
		"User-Agent":          prof.ModelUserAgent(),
		"X-ZCode-App-Version": config.ZcodeClientVersion,
		"X-ZCode-Agent":       "glm",
		"X-Title":             prof.Title(),
		"X-Release-Channel":   prof.ReleaseChannel,
		"X-Client-Language":   prof.Language,
		"X-Client-Timezone":   prof.Timezone,
		"X-Platform":          prof.Platform(),
		"X-Os-Category":       prof.OSCategory(),
		"X-Os-Version":        prof.OSRelease,
		"HTTP-Referer":        config.ZcodeEndpointOrigin,
	}
	for key, expect := range want {
		if expect == "" {
			t.Fatalf("夹具期望值为空：%s", key)
		}
		if got := req.Headers[key]; got != expect {
			t.Errorf("%s=%q，期望 %q", key, got, expect)
		}
	}
}

// TestModelUserAgentShape 模型请求的 UA 形态由 golden 抓包实测得到，与裸串不同。
//
// 实测（2026-09-24，见 docs/analysis-client-golden-diff-20260924.md）：
//
//	POST /v1/messages
//	user-agent: ZCode/0.16.9 ai-sdk/provider-utils/4.0.27 runtime/node.js/22
//	GET  /api/v1/agent/configs
//	user-agent: ZCode/0.16.9            ← 非模型接口是裸串
//
// 若把两者合并成一个常量，模型请求就会丢掉 ai-sdk 后缀这个上游可见的差异。
func TestModelUserAgentShape(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ua := req.Headers["User-Agent"]
	want := "ZCode/" + config.ZcodeClientVersion +
		" ai-sdk/provider-utils/" + config.ZcodeClientAISDKVersion +
		" runtime/node.js/" + config.ZcodeClientNodeMajor
	if ua != want {
		t.Errorf("模型请求 UA=%q，期望 %q", ua, want)
	}
	if !strings.Contains(ua, "ai-sdk/provider-utils/") || !strings.Contains(ua, "runtime/node.js/") {
		t.Errorf("模型请求 UA 必须带 ai-sdk 后缀: %q", ua)
	}
	// 非模型路径仍用裸串——两者刻意分开。
	if config.UserAgent != "ZCode/"+config.ZcodeClientVersion {
		t.Errorf("非模型 UA 应保持裸串: %q", config.UserAgent)
	}
	if strings.Contains(config.UserAgent, "ai-sdk") {
		t.Errorf("非模型 UA 不应带 ai-sdk 后缀: %q", config.UserAgent)
	}
}

// TestRefererHasNoTrailingSlash 官方 HTTP-Referer 是 origin，不带尾斜杠。
// 多一个字符就是与官方不同的请求头。
func TestRefererHasNoTrailingSlash(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Headers["HTTP-Referer"]; strings.HasSuffix(got, "/") {
		t.Fatalf("HTTP-Referer 不应带尾斜杠: %q", got)
	}
}

// TestAttributionHeadersSynthesized 下游什么都不送时，四项归因头必须齐备
// 且形态与官方一致（x-request-id 是 UUIDv4，而不是网关内部的短 ticket id）。
func TestAttributionHeadersSynthesized(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Headers

	if got := h["X-Request-Id"]; !uuidV4Re.MatchString(got) {
		t.Errorf("X-Request-Id 应为 UUIDv4，实际 %q", got)
	}
	if got := h["X-Zcode-Trace-Id"]; !uuidV4Re.MatchString(got) {
		t.Errorf("X-Zcode-Trace-Id 应为 UUIDv4，实际 %q", got)
	}
	if got := h["X-Zcode-Session-Type"]; got != "main" {
		t.Errorf("X-Zcode-Session-Type 缺省应为 main，实际 %q", got)
	}
	if got, ok := h["X-Session-Id"]; !ok || got != h["X-Request-Id"] {
		t.Errorf("X-Session-Id 缺失时应回落到本次请求 id，实际 %q", got)
	}
	// x-query-id 恒在：官方每个轮次都会生成一个，实测抓包确认它从不缺席。
	if got := h["X-Query-Id"]; !uuidV4Re.MatchString(got) {
		t.Errorf("X-Query-Id 应恒在且为 UUIDv4，实际 %q", got)
	}
}

// TestAttributionHeadersPreferClientValues 下游送来真实值时必须沿用：
// 同一会话的 trace/session 才连续（否则每次请求都换会话，本身就是异常形态）。
func TestAttributionHeadersPreferClientValues(t *testing.T) {
	incoming := map[string]string{
		"x-session-id":         "sess_abc123",
		"x-zcode-trace-id":     "trace-xyz",
		"x-zcode-session-type": "subagent",
		"x-query-id":           "query_q777",
		"X-Request-Id":         "client-rid",
	}
	req, err := BuildRequest(jwtAccount(), "", "", incoming)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Headers

	if h["X-Zcode-Trace-Id"] != "trace-xyz" {
		t.Errorf("应沿用客户端的 trace id，实际 %q", h["X-Zcode-Trace-Id"])
	}
	if h["X-Zcode-Session-Type"] != "subagent" {
		t.Errorf("应沿用客户端的 session type，实际 %q", h["X-Zcode-Session-Type"])
	}
	// 官方会剥掉 sess_ / query_ 前缀，输出的头里从不含它们。
	if h["X-Session-Id"] != "abc123" {
		t.Errorf("sess_ 前缀应被剥离，实际 %q", h["X-Session-Id"])
	}
	if h["X-Query-Id"] != "q777" {
		t.Errorf("query_ 前缀应被剥离，实际 %q", h["X-Query-Id"])
	}
	// x-request-id 是「本次请求」的标识，官方对每个请求 attempt 都重新生成，
	// 不能沿用下游的值。
	if h["X-Request-Id"] == "client-rid" {
		t.Error("X-Request-Id 不应沿用下游值")
	}
	if !uuidV4Re.MatchString(h["X-Request-Id"]) {
		t.Errorf("X-Request-Id 应为 UUIDv4，实际 %q", h["X-Request-Id"])
	}
}

// TestAttributionHeaderEmptyValuesTreatedAsMissing 空串/空白不算「客户端提供了值」，
// 否则会出现空头的畸形请求。
func TestAttributionHeaderEmptyValuesTreatedAsMissing(t *testing.T) {
	incoming := map[string]string{
		"x-session-id":         "",
		"x-zcode-trace-id":     "   ",
		"x-zcode-session-type": "",
	}
	req, err := BuildRequest(jwtAccount(), "", "", incoming)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Headers
	if !uuidV4Re.MatchString(h["X-Zcode-Trace-Id"]) {
		t.Errorf("空白 trace id 应被视为缺失并合成，实际 %q", h["X-Zcode-Trace-Id"])
	}
	if h["X-Zcode-Session-Type"] != "main" {
		t.Errorf("空白 session type 应回落到 main，实际 %q", h["X-Zcode-Session-Type"])
	}
	if !uuidV4Re.MatchString(h["X-Session-Id"]) {
		t.Errorf("空白 session id 应回落到本次请求 id，实际 %q", h["X-Session-Id"])
	}
}

// TestClientZcodeHeadersOutcome x-zcode-* 的两条通路必须区分清楚：
//
//   - **未知**的 x-zcode-*（下游自身上下文）不得透传；
//   - 官方必带的 x-zcode-session-type / x-zcode-trace-id 必须**照原值**到达上游。
//
// 后者由 attributionHeaders 从原始 incoming 取回并作为固定头写出，与透传循环里
// 对 x-zcode-* 的一刀切丢弃无关。本用例同时钉住这两点：若有人删掉归因合成，
// 后两个断言会红；若有人放开未知 x-zcode-* 的透传，第一个断言会红。
func TestClientZcodeHeadersOutcome(t *testing.T) {
	incoming := map[string]string{
		"x-zcode-trace-id":        "trace-from-client",
		"x-zcode-session-type":    "subagent",
		"x-zcode-rpc-client-mode": "weird",
		"x-zcode-app-version":     "9.9.9",
	}
	req, err := BuildRequest(jwtAccount(), "", "", incoming)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Headers

	if _, ok := h["X-Zcode-Rpc-Client-Mode"]; ok {
		t.Error("未知 x-zcode-* 不应透传")
	}
	if h["X-ZCode-App-Version"] != config.ZcodeClientVersion {
		t.Errorf("透传不得覆盖版本头，实际 %q", h["X-ZCode-App-Version"])
	}
	if h["X-Zcode-Trace-Id"] != "trace-from-client" {
		t.Errorf("官方必带的 trace id 应照原值上行，实际 %q", h["X-Zcode-Trace-Id"])
	}
	if h["X-Zcode-Session-Type"] != "subagent" {
		t.Errorf("官方必带的 session type 应照原值上行，实际 %q", h["X-Zcode-Session-Type"])
	}
}

// TestClientSigningHeadersDropped 客户端签名头一律剔除：签名与 apiKey/session 绑定，
// 下游的签名对本账号无效（透传只会换来 401 VERIFY_SIGNATURE_INVALID），
// 而本网关不实现签名，不能自己造。
func TestClientSigningHeadersDropped(t *testing.T) {
	incoming := map[string]string{
		"X-Client-Sig":           "sig",
		"X-Client-Ts":            "123",
		"X-Client-Version":       "3.14.3",
		"X-Client-Nonce":         "nonce",
		"X-Client-Pow":           "pow",
		"X-Client-Sign-Verified": "1",
		"X-App-Id":               "zcode",
	}
	req, err := BuildRequest(jwtAccount(), "", "", incoming)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"X-Client-Sig", "X-Client-Ts", "X-Client-Version", "X-Client-Nonce",
		"X-Client-Pow", "X-Client-Sign-Verified", "X-App-Id",
	} {
		if _, ok := req.Headers[key]; ok {
			t.Errorf("签名头 %s 不应透传", key)
		}
	}
}

// TestDeviceMidNotSentOnModelRequests 模型请求**不带** X-Device-Mid。
//
// golden 抓包实测：官方模型请求的头里没有这个头，设备指纹在 body 的
// metadata.user_id.device_id 里（见 metadata.go）。所以对齐的方向是「头去掉、
// body 加上」——两头都去掉等于设备身份消失，反而更像伪造。
//
// 开关只为应急回滚保留（设 true 恢复历史行为），默认必须是 false。
func TestDeviceMidNotSentOnModelRequests(t *testing.T) {
	old := config.UpstreamSendDeviceMid
	t.Cleanup(func() { config.UpstreamSendDeviceMid = old })

	if config.UpstreamSendDeviceMid {
		t.Fatal("默认应为 false：官方模型请求不带 X-Device-Mid，设备身份改走 body 的 metadata")
	}
	req, err := BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := req.Headers["X-Device-Mid"]; ok {
		t.Fatalf("模型请求不应携带 X-Device-Mid: %v", req.Headers)
	}

	// 回滚开关仍须有效——否则线上出问题时没有退路。
	config.UpstreamSendDeviceMid = true
	req, err = BuildRequest(jwtAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Headers["X-Device-Mid"] == "" {
		t.Fatal("开关开启时应恢复发送 X-Device-Mid")
	}
}

// TestHeaderAndBodyPlatformAgree 头体同源守卫（GAP-3 的正面回归）：
// 请求头声称的平台，必须与 body 里 # Environment 段声称的一致。
// 修复前这里是「头说 win32-x64、body 说 linux-x64」。
func TestHeaderAndBodyPlatformAgree(t *testing.T) {
	oldBare, oldArch := config.ZcodeClientBarePlatform, config.ZcodeClientArch
	t.Cleanup(func() {
		config.ZcodeClientBarePlatform, config.ZcodeClientArch = oldBare, oldArch
	})

	for _, tc := range []struct{ bare, arch, header, envPlatform string }{
		{"win32", "x64", "win32-x64", "win32"},
		{"darwin", "arm64", "darwin-arm64", "darwin"},
	} {
		config.ZcodeClientBarePlatform, config.ZcodeClientArch = tc.bare, tc.arch

		req, err := BuildRequest(jwtAccount(), "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Headers["X-Platform"]; got != tc.header {
			t.Fatalf("X-Platform=%q，期望 %q", got, tc.header)
		}

		envPlatform := environmentPlatformFromBlocks(t)
		if envPlatform != tc.envPlatform {
			t.Fatalf("body 的 # Environment 声称 %q，而请求头声称 %q —— 头体不同源",
				envPlatform, req.Headers["X-Platform"])
		}
		if want := tc.envPlatform + "-" + tc.arch; tc.header != want {
			t.Fatalf("头的复合形态 %q 与 %q 不自洽", tc.header, want)
		}
	}
}

// environmentPlatformFromBlocks 从注入的 # Environment 段里取出 Platform 行。
func environmentPlatformFromBlocks(t *testing.T) string {
	t.Helper()
	for _, b := range ZcodeSystemBlocks() {
		m, _ := b.(map[string]any)
		text, _ := m["text"].(string)
		for _, line := range strings.Split(text, "\n") {
			if rest, ok := strings.CutPrefix(line, "- Platform: "); ok {
				return rest
			}
		}
	}
	t.Fatal("未在系统提示词块里找到 Platform 行")
	return ""
}

// TestAuthDualHeadersSameValue 鉴权双头同值（GAP-15）：官方把凭据作为 AI SDK 的
// apiKey 传入（注入 x-api-key），client 层再补 Authorization: Bearer —— 两个头恒为
// 同一个值，且**两种账号模式都如此**（start-plan 用 JWT、coding-plan 用铸造 key，
// 出站形态相同）。只发其一与官方形态不同。
func TestAuthDualHeadersSameValue(t *testing.T) {
	jwt := jwtAccount()
	jwtTok := *jwt.JWTToken
	req, err := BuildRequest(jwt, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Headers["Authorization"] != "Bearer "+jwtTok {
		t.Errorf("JWT 模式 Authorization 应为 Bearer <jwt>，实际 %q", req.Headers["Authorization"])
	}
	if req.Headers["X-Api-Key"] != jwtTok {
		t.Errorf("JWT 模式应补 X-Api-Key 且与 Authorization 同值，实际 %q", req.Headers["X-Api-Key"])
	}

	keyAcc := jwtAccount()
	keyAcc.Mode = "apikey"
	keyAcc.APIKey = strPtr("id.secret")
	req, err = BuildRequest(keyAcc, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Headers["X-Api-Key"] != "id.secret" {
		t.Errorf("APIKey 模式 X-Api-Key 应为凭据本身，实际 %q", req.Headers["X-Api-Key"])
	}
	if req.Headers["Authorization"] != "Bearer id.secret" {
		t.Errorf("APIKey 模式应补 Authorization: Bearer <同一凭据>，实际 %q", req.Headers["Authorization"])
	}
}

// TestUndiciTransportHeaders 官方经 undici（Node fetch）出站，三个传输层默认头恒在
// （golden 实测 accept: */*、accept-language: *、sec-fetch-mode: cors）。缺它们与缺
// 身份头一样可被指纹化。同时钉住客户端不得改写（固定头恒胜）。
func TestUndiciTransportHeaders(t *testing.T) {
	incoming := map[string]string{"Accept": "application/json", "Accept-Language": "zh-CN"}
	req, err := BuildRequest(jwtAccount(), "", "", incoming)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"Accept":          "*/*",
		"Accept-Language": "*",
		"Sec-Fetch-Mode":  "cors",
	} {
		if got := req.Headers[key]; got != want {
			t.Errorf("%s 应为官方固定值 %q（不受客户端透传影响），实际 %q", key, want, got)
		}
	}
}

// TestAttributionFreshRequestID 换号/重试 = 新的上游 HTTP 请求 ⇒ x-request-id 换新，
// 而轮次（query）、会话（session/trace/type）保持 —— 官方 CLI 日志实证：同一 queryId
// 的 4 次重试有 4 个不同的 requestId。整轮共享一个 id 会让「同一请求 id 出现在多个
// 账号上」，直接暴露多账号同源。
func TestAttributionFreshRequestID(t *testing.T) {
	orig := NewAttribution(map[string]string{
		"X-Session-Id":         "sess_demo",
		"X-Zcode-Trace-Id":     "trace_demo",
		"X-Zcode-Session-Type": "subagent",
		"X-Query-Id":           "query_demo",
	}, "")
	fresh := orig.WithFreshRequestID()

	if fresh.RequestID == orig.RequestID {
		t.Fatal("RequestID 必须换新")
	}
	if fresh.SessionID != orig.SessionID || fresh.TraceID != orig.TraceID ||
		fresh.SessionType != orig.SessionType || fresh.QueryID != orig.QueryID {
		t.Fatalf("其余归因标识必须保持: %+v vs %+v", fresh, orig)
	}
	// 值必须是合法 UUIDv4（官方形态），不是空串或占位。
	if len(fresh.RequestID) != 36 || strings.Count(fresh.RequestID, "-") != 4 {
		t.Fatalf("新 RequestID 应为 UUID 形态: %q", fresh.RequestID)
	}
	// 原值不被修改（值语义）。
	if orig.RequestID == "" {
		t.Fatal("原 Attribution 不得被就地修改")
	}
}
