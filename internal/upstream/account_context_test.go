package upstream

import (
	"errors"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

// 即使后续新增调用者绕过 HTTP 层，请求构造器也不能生成混搭账号的可发送请求。
func TestAccountContextRequestBuilderFailsClosed(t *testing.T) {
	builders := []struct {
		name  string
		build func(*model.Account, map[string]string) (Request, error)
	}{
		{"derived_attribution", func(acc *model.Account, headers map[string]string) (Request, error) {
			return BuildRequest(acc, "", "", headers)
		}},
		{"explicit_attribution", func(acc *model.Account, headers map[string]string) (Request, error) {
			return BuildRequestWithAttribution(acc, "", "", headers, Attribution{SessionID: "synthetic-session"})
		}},
	}
	for _, builder := range builders {
		for _, acc := range []*model.Account{jwtAccount(), keyAccount(), bareAccount()} {
			for _, field := range []string{
				"X-Highspeed-Card-ID", "X-Bigmodel-Authorization",
				"Bigmodel-Target-Type", "Bigmodel-Organization", "Bigmodel-Project",
			} {
				for _, name := range []string{field, strings.ToLower(field), strings.ToUpper(field)} {
					for _, value := range []string{"synthetic-private-context", ""} {
						incoming := map[string]string{name: value, "X-Custom-Safe": "keep"}
						before := maps.Clone(incoming)
						req, err := builder.build(acc, incoming)
						if !errors.Is(err, ErrUnsupportedAccountContext) {
							t.Fatalf("%s/%s 必须按客户端上下文错误拒绝，实际 %v", builder.name, name, err)
						}
						if req.URL != "" || len(req.Headers) != 0 {
							t.Fatal("拒绝结果不得携带可发送的 URL 或认证头")
						}
						if !strings.Contains(err.Error(), field) || value != "" && strings.Contains(err.Error(), value) {
							t.Fatal("错误应只报告规范头名，不回显调用方材料")
						}
						if !reflect.DeepEqual(incoming, before) {
							t.Fatal("校验不得改写调用方的请求头")
						}
					}
				}
			}
		}
	}
}

func TestAccountContextAllowedBuilderHeaders(t *testing.T) {
	for _, acc := range []*model.Account{jwtAccount(), keyAccount()} {
		incoming := map[string]string{
			"Anthropic-Beta":            "interleaved-thinking-2025-05-14",
			"X-Custom-Safe":             "safe-extension",
			"X-Highspeed-Card-ID-Trace": "safe-not-account-context",
			"X-Session-Id":              "synthetic-session",
			"X-Zcode-Trace-Id":          "synthetic-trace",
			"X-Zcode-Session-Type":      "subagent",
			"X-Query-Id":                "synthetic-query",
		}
		before := maps.Clone(incoming)
		req, err := BuildRequest(acc, "", "", incoming)
		if err != nil {
			t.Fatal(err)
		}
		wireHeaders := make(http.Header)
		for name, value := range req.Headers {
			wireHeaders.Set(name, value)
		}
		for name, value := range incoming {
			if wireHeaders.Get(name) != value {
				t.Errorf("不相关的协议/归因头 %s 应保持：%q", name, wireHeaders.Get(name))
			}
		}
		if !reflect.DeepEqual(incoming, before) {
			t.Fatal("构造请求不能修改输入头")
		}
	}
}
