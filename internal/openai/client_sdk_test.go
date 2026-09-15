package openai

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 显式开启真实 SDK 的本地回归，日常 go test 不要求安装 Python / Pi。
// 所有模型响应由 httptest 生成，不使用真实账号，也不调用外部模型或执行用户工具。
func TestClientSDKCompatibility(t *testing.T) {
	enabled := "," + os.Getenv("ZCODE_TEST_CLIENTS") + ","
	for _, client := range []string{"python", "pi"} {
		t.Run(client, func(t *testing.T) {
			if !strings.Contains(enabled, ","+client+",") {
				t.Skip("设置 ZCODE_TEST_CLIENTS=python,pi 后运行已安装客户端的本地闭环")
			}
			f := newFixture(t)
			mixed := mixedToolStream(t)
			f.respond = func(call int) (int, string, string) {
				up := f.lastUpstream().Body
				hasResult := false
				messages, _ := up["messages"].([]any)
				for _, raw := range messages {
					msg, _ := raw.(map[string]any)
					blocks, _ := msg["content"].([]any)
					for _, rawBlock := range blocks {
						block, _ := rawBlock.(map[string]any)
						hasResult = hasResult || block["type"] == "tool_result"
					}
				}
				stream, _ := up["stream"].(bool)
				if hasResult {
					if stream {
						return http.StatusOK, "text/event-stream", responseTextStream
					}
					return replyText(call)
				}
				if stream {
					return http.StatusOK, "text/event-stream", mixed
				}
				return http.StatusOK, "application/json", `{"id":"sdk_json","model":"GLM-5.3","stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":6},"content":[{"type":"thinking","thinking":"先查询两个城市。"},{"type":"text","text":"正在查询天气。"},{"type":"tool_use","id":"call_a","name":"get_weather","input":{"city":"杭州"}},{"type":"tool_use","id":"call_b","name":"get_weather","input":{"city":"上海"}}]}`
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			program, script := "python", "testdata/python_sdk_compat.py"
			if client == "pi" {
				program, script = "node", "testdata/pi_sdk_compat.mjs"
			}
			cmd := exec.CommandContext(ctx, program, script)
			cmd.Env = append(os.Environ(), "ZCODE_TEST_BASE_URL="+f.srv.URL+"/v1", "OPENAI_API_KEY=sk-test", "PYTHONUTF8=1")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s 客户端闭环失败: %v\n%s", client, err, output)
			}
			t.Logf("%s", output)
			f.mu.Lock()
			calls := append([]upstreamBody(nil), f.calls...)
			f.mu.Unlock()
			if len(calls) < 2 {
				t.Fatalf("至少应有工具请求和工具结果回传两轮: %d", len(calls))
			}
			for _, call := range calls {
				if _, leaked := call.Body["include_usage"]; leaked {
					t.Fatal("客户端的 include_usage 不能泄漏到上游")
				}
				thinking, _ := call.Body["thinking"].(map[string]any)
				if thinking["budget_tokens"] != float64(8192) {
					t.Fatalf("客户端 high 档位未实际到达上游: %v", call.Body["thinking"])
				}
			}
		})
	}
}
