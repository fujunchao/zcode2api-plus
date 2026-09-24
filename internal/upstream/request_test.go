package upstream

import (
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

func jwtAccount() *model.Account  { return model.Create(model.ProviderZai, "t", "header.payload.sig") }
func keyAccount() *model.Account  { return model.Create(model.ProviderZai, "t", "sk-secret") }
func bareAccount() *model.Account { return &model.Account{Provider: model.ProviderZai, Mode: "jwt"} }

func TestBuildRequestJWT(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "server-token", "sgp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != config.UpstreamZai {
		t.Fatalf("JWT 应走主端点: %s", req.URL)
	}
	h := req.Headers
	if h["Authorization"] != "Bearer header.payload.sig" {
		t.Fatalf("Authorization 不符: %q", h["Authorization"])
	}
	if h["Content-Type"] != "application/json" || h["Anthropic-Version"] != "2023-06-01" {
		t.Fatalf("固定头不符: %v", h)
	}
	if h["X-ZCode-App-Version"] != config.ZcodeClientVersion || h["X-ZCode-Agent"] != "glm" {
		t.Fatalf("ZCode 标识头不符: %v", h)
	}
	if _, ok := h["X-Device-Mid"]; ok {
		t.Fatal("模型请求不应携带 X-Device-Mid（设备身份在 body 的 metadata.user_id 里）")
	}
	if h["X-Aliyun-Captcha-Verify-Param"] != "server-token" ||
		h["X-Aliyun-Captcha-Verify-Region"] != "sgp" {
		t.Fatalf("验证码头不符: %v", h)
	}
}

func TestBuildRequestAPIKey(t *testing.T) {
	req, err := BuildRequest(keyAccount(), "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL != config.UpstreamZaiFallback {
		t.Fatalf("API Key 应走回退端点: %s", req.URL)
	}
	if req.Headers["X-Api-Key"] != "sk-secret" {
		t.Fatalf("x-api-key 不符: %v", req.Headers)
	}
	if _, ok := req.Headers["X-Aliyun-Captcha-Verify-Param"]; ok {
		t.Fatal("无验证码令牌时不应携带验证码头")
	}
	if _, ok := req.Headers["X-Aliyun-Captcha-Verify-Region"]; ok {
		t.Fatal("无 region 时不应携带 region 头")
	}
}

func TestBuildRequestNoCredentials(t *testing.T) {
	if _, err := BuildRequest(bareAccount(), "", "", nil); err == nil {
		t.Fatal("缺少凭证应报错")
	}
	foreign := &model.Account{Provider: "unknown", Mode: "apiKey", APIKey: strPtr("k")}
	if _, err := BuildRequest(foreign, "", "", nil); err == nil {
		t.Fatal("未知提供商应报错")
	}
}

func TestClientHeadersFiltered(t *testing.T) {
	incoming := map[string]string{
		"Cookie":                         "session=1",
		"Authorization":                  "Bearer client-key",
		"X-ZCode-Agent":                  "spoof",
		"x-zcode-app-version":            "9.9.9",
		"x-aliyun-captcha-verify-param":  "client-token",
		"X-Aliyun-Captcha-Verify-Region": "client-region",
		"Accept":                         "application/json",
		"X-Custom-Trace":                 "keep-me",
	}
	req, err := BuildRequest(jwtAccount(), "server-token", "sgp", incoming)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Headers
	if _, ok := h["Cookie"]; ok {
		t.Fatal("cookie 应被剔除")
	}
	if h["Authorization"] != "Bearer header.payload.sig" {
		t.Fatal("客户端不得覆盖鉴权头")
	}
	if _, ok := h["X-ZCode-Agent"]; !ok || h["X-ZCode-Agent"] == "spoof" {
		t.Fatal("x-zcode* 透传应被剔除")
	}
	if _, ok := h["x-zcode-app-version"]; ok {
		t.Fatal("x-zcode*（小写）透传应被剔除")
	}
	if h["X-ZCode-App-Version"] != config.ZcodeClientVersion {
		t.Fatalf("x-zcode* 透传不得覆盖版本头: %v", h["X-ZCode-App-Version"])
	}
	if h["X-Aliyun-Captcha-Verify-Param"] != "server-token" {
		t.Fatal("客户端不得覆盖验证码头")
	}
	if h["X-Aliyun-Captcha-Verify-Region"] != "sgp" {
		t.Fatal("客户端不得覆盖验证码 region 头")
	}
	// Accept 是官方恒带的传输层固定头（undici 默认 */*），客户端透传值被覆盖。
	if h["Accept"] != "*/*" {
		t.Fatalf("Accept 应为官方固定值 */*（客户端透传值被覆盖）: %v", h)
	}
	if h["X-Custom-Trace"] != "keep-me" {
		t.Fatalf("普通透传头应保留: %v", h)
	}
	// 鉴权双头同值（官方形态）：Authorization 与 X-Api-Key 是同一凭据。
	if h["X-Api-Key"] != "header.payload.sig" || h["Authorization"] != "Bearer header.payload.sig" {
		t.Fatalf("JWT 账号应发鉴权双头且同值: %q / %q", h["X-Api-Key"], h["Authorization"])
	}
}

func TestZcodeSystemBlocks(t *testing.T) {
	blocks := ZcodeSystemBlocks()
	if len(blocks) == 0 {
		t.Fatal("zcode_system.json 应解析出提示词块")
	}
	first, ok := blocks[0].(map[string]any)
	if !ok || first["type"] != "text" {
		t.Fatalf("首个块应为 text: %v", blocks[0])
	}
	again := ZcodeSystemBlocks()
	if len(again) != len(blocks) {
		t.Fatal("重复调用应返回同一份缓存")
	}
}

func strPtr(s string) *string { return &s }

// 客户端送来的 x-device-mid 必须**整个丢弃**。
//
// 我们不再自己写这个头（官方模型请求不带它），于是「固定头覆盖透传头」这条防线也
// 没了 —— 若不显式拦掉，下游送来的值会原样到达上游，等于让客户端指定本账号的设备
// 指纹，每账号一份指纹的账号隔离形同虚设。设备身份只走 body 的 metadata.user_id。
func TestClientDeviceMidHeaderNeverReachesUpstream(t *testing.T) {
	incomings := []map[string]string{
		{"X-Device-Mid": "spoofed"},
		{"x-device-mid": "spoofed"},
		{"X-DEVICE-MID": "spoofed"},
		{"x-device-mid": "spoofed", "X-Custom-Trace": "keep-me"},
	}
	for _, incoming := range incomings {
		req, err := BuildRequest(jwtAccount(), "", "", incoming)
		if err != nil {
			t.Fatal(err)
		}
		for key := range req.Headers {
			if strings.EqualFold(key, "x-device-mid") {
				t.Fatalf("客户端 x-device-mid 不得出现在出站头: %v", req.Headers)
			}
		}
		// 剔除是定点拦截，不能把其它透传头一起带走（Accept 除外——它现在是
		// 官方恒带的传输层固定头，恒胜透传）。
		if v, ok := incoming["X-Custom-Trace"]; ok && req.Headers["X-Custom-Trace"] != v {
			t.Fatalf("普通透传头不应受影响: %v", req.Headers)
		}
	}
}
