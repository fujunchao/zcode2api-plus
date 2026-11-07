// 断流终态语义与预缓冲的回归测试。
//
// 守的是 2026-11-07 的事故：ZCode 收到 api_error 后判成 reason=unknown /
// retryable=false，直接把一整轮 turn 判死；而它本来有 11 次重试预算。
// 详见 docs/plan-truncate-resilience-20261107.md。
package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// truncatingUpstream 先下发给定帧，然后掐断连接（chunked 不收尾）。
func truncatingUpstream(frames []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
			time.Sleep(2 * time.Millisecond)
		}
		panic(http.ErrAbortHandler)
	})
}

// contentFrames 一段「已经产出内容」的流前缀：断流发生在它之后。
var contentFrames = []string{
	sseStart,
	sseFrame("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}),
	sseFrame("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "你好"},
	}),
}

// TestTruncateTerminalPolicies：四种终态策略在「已下发内容之后断流」时的逐字节形态。
func TestTruncateTerminalPolicies(t *testing.T) {
	cases := []struct {
		name    string
		policy  string
		want    []string
		notWant []string
	}{
		{
			name:   "retryable_可重试错误帧",
			policy: config.TruncateTerminalRetryable,
			// 双通道缺一不可：code 供直连命中码表；[1312][ 前缀供 new-api 重建后仍能穿透
			// （实测 new-api 只保留 type 与 message，附加字段会被丢弃）。
			want: []string{
				"event: error",
				`"type":"overloaded_error"`,
				`"code":"overloaded_error"`,
				`"message":"[1312][`,
			},
			notWant: []string{`"type":"api_error"`},
		},
		{
			name:    "legacy_回滚形态",
			policy:  config.TruncateTerminalLegacy,
			want:    []string{"event: error", `"type":"api_error"`, "上游流式响应中断"},
			notWant: []string{"overloaded_error", "[1312]"},
		},
		{
			name:   "graceful_补齐协议正常结束",
			policy: config.TruncateTerminalGraceful,
			want: []string{
				"content_block_stop", "message_delta", "message_stop",
				"本次回答被截断", // 可见提示：不能把截断伪装成完整回答
			},
			notWant: []string{"event: error"},
		},
		{
			name:   "abort_不补帧直接断连",
			policy: config.TruncateTerminalAbort,
			// 客户端只能读到断流前的内容：既没有终态帧，也没有正常收尾。
			notWant: []string{"event: error", "overloaded_error", "message_stop"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := config.TruncateTerminal
			config.TruncateTerminal = tc.policy
			t.Cleanup(func() { config.TruncateTerminal = old })

			f := newDiagFixture(t, truncatingUpstream(contentFrames))
			if _, err := f.st.AddAccount(model.ProviderZai, "acc-1", "sk-up"); err != nil {
				t.Fatal(err)
			}
			status, body := f.post(t, msgBody())
			if status != http.StatusOK {
				t.Fatalf("交付已开始应保持 200，实际 %d: %s", status, body)
			}
			for _, want := range tc.want {
				if !strings.Contains(body, want) {
					t.Fatalf("响应缺少 %q：%s", want, body)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(body, notWant) {
					t.Fatalf("响应不应包含 %q：%s", notWant, body)
				}
			}
		})
	}
}

// TestGracefulFallsBackWhenToolUseOpen：tool_use 块没闭合时绝不能 graceful——
// 半截工具调用被当成成功交付，客户端会拿去执行，比报错危险得多。
func TestGracefulFallsBackWhenToolUseOpen(t *testing.T) {
	old := config.TruncateTerminal
	config.TruncateTerminal = config.TruncateTerminalGraceful
	t.Cleanup(func() { config.TruncateTerminal = old })

	frames := []string{
		sseStart,
		sseFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "tool_1", "name": "Read", "input": map[string]any{}},
		}),
		sseFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": "{\"path\":"},
		}),
	}
	f := newDiagFixture(t, truncatingUpstream(frames))
	if _, err := f.st.AddAccount(model.ProviderZai, "acc-1", "sk-up"); err != nil {
		t.Fatal(err)
	}
	_, body := f.post(t, msgBody())
	if !strings.Contains(body, "overloaded_error") {
		t.Fatalf("工具块未闭合时应降级为可重试错误帧：%s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("工具块未闭合时不得伪造 message_stop：%s", body)
	}
}

// TestEarlyTruncateSwitchesAccountBeforeCommit：上游在产出内容之前就断，
// 客户端一个字节都没收到，引擎应换号重打，用户看到的是第二跳的完整回答。
func TestEarlyTruncateSwitchesAccountBeforeCommit(t *testing.T) {
	var calls int32
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, sseStart) // 只有 message_start：没有任何内容
		flusher.Flush()
		if n == 1 {
			time.Sleep(2 * time.Millisecond)
			panic(http.ErrAbortHandler)
		}
		for _, frame := range []string{
			sseFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			}),
			sseFrame("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "第二跳"},
			}),
			sseFrame("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": 3},
			}),
			sseFrame("message_stop", map[string]any{"type": "message_stop"}),
		} {
			_, _ = io.WriteString(w, frame)
		}
		flusher.Flush()
	})

	f := newDiagFixture(t, up)
	for _, name := range []string{"acc-1", "acc-2"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-"+name); err != nil {
			t.Fatal(err)
		}
	}

	status, body := f.post(t, msgBody())
	if status != http.StatusOK {
		t.Fatalf("换号后应成功交付，实际 %d: %s", status, body)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应当在未交付时换号重打一次，实际上游调用 %d 次", got)
	}
	if !strings.Contains(body, "第二跳") || !strings.Contains(body, "message_stop") {
		t.Fatalf("客户端应收到第二跳的完整回答：%s", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("早期断流对客户端应完全不可见：%s", body)
	}
}

// TestEarlyTruncateExhaustedReturns5xx：所有尝试都没能产出内容时，返回 5xx
// （客户端按 statusCode>=500 判可重试），而不是 200 + 错误帧。
func TestEarlyTruncateExhaustedReturns5xx(t *testing.T) {
	f := newDiagFixture(t, truncatingUpstream([]string{sseStart}))
	if _, err := f.st.AddAccount(model.ProviderZai, "acc-1", "sk-up"); err != nil {
		t.Fatal(err)
	}
	status, body := f.post(t, msgBody())
	if status != http.StatusBadGateway {
		t.Fatalf("全部尝试都在产出前中断应返回 502，实际 %d: %s", status, body)
	}
	if !strings.Contains(body, "upstream_stream_interrupted") {
		t.Fatalf("错误类型应为 upstream_stream_interrupted：%s", body)
	}
}

// TestDrillChannel：演练通道只在密钥命中时生效，且按当前策略收尾。
func TestDrillChannel(t *testing.T) {
	oldKey, oldPolicy := config.DrillKey, config.TruncateTerminal
	config.DrillKey = "drill-secret"
	config.TruncateTerminal = config.TruncateTerminalRetryable
	t.Cleanup(func() { config.DrillKey, config.TruncateTerminal = oldKey, oldPolicy })

	f := newDiagFixture(t, truncatingUpstream(contentFrames))
	// 演练通道不接触账号池：没有账号也应能产出。
	status, body := postDrill(t, f.srv.URL+"/v1/messages", "drill-secret", "truncate")
	if status != http.StatusOK {
		t.Fatalf("演练应返回 200，实际 %d: %s", status, body)
	}
	if !strings.Contains(body, "演练") || !strings.Contains(body, "overloaded_error") {
		t.Fatalf("演练输出应包含内容与终态帧：%s", body)
	}

	// 密钥不匹配时走正常链路：没有账号 ⇒ 503。
	status, body = postDrill(t, f.srv.URL+"/v1/messages", "wrong", "truncate")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("密钥不匹配应走正常链路（无账号 503），实际 %d: %s", status, body)
	}
}

func postDrill(t *testing.T, url, key, mode string) (int, string) {
	t.Helper()
	raw, err := marshalJSON(msgBody())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("X-Zcode-Drill", key)
	req.Header.Set("X-Zcode-Drill-Mode", mode)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}
