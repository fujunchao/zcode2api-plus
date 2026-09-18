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
	if h["X-Device-Mid"] == "" {
		t.Fatal("应携带设备标识")
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
	if h["Accept"] != "application/json" || h["X-Custom-Trace"] != "keep-me" {
		t.Fatalf("普通透传头应保留: %v", h)
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

// 客户端送来的设备指纹头必须无效：每账号独立的 X-Device-Mid 是账号隔离的基础，
// 被客户端指定等于让上游把多个账号看成同一台设备。
//
// 同时钉住「只占一个键」——固定头与客户端头若因大小写不同各占一个 map 键，
// 下游 Header.Set 会把它们归一到同一名字，最终取值便取决于 map 迭代顺序。
func TestClientCannotOverrideDeviceMid(t *testing.T) {
	acc := jwtAccount()
	want := acc.DeviceMidOr(config.DeviceMid())

	incomings := []map[string]string{
		{"X-Device-Mid": "spoofed"},
		{"x-device-mid": "spoofed"},
		{"X-DEVICE-MID": "spoofed"},
		{"x-device-mid": "spoofed", "Accept": "application/json"},
	}
	for _, incoming := range incomings {
		req, err := BuildRequest(acc, "", "", incoming)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := req.Headers["X-Device-Mid"]
		if !ok {
			t.Fatalf("应携带固定的设备标识: %v", req.Headers)
		}
		if got != want {
			t.Fatalf("设备指纹被客户端覆盖: %q（期望 %q）", got, want)
		}
		count := 0
		for key := range req.Headers {
			if strings.EqualFold(key, "x-device-mid") {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("设备指纹头应只占一个键，实际 %d 个: %v", count, req.Headers)
		}
	}
}
