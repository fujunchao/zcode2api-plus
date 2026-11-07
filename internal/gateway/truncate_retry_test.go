// 重试感知选线的集成验证：同一份内容在一条线路上断流后，紧接着的重试请求
// 必须绕开整条线路（而不是换个账号仍落回同一条线）。
//
// 背景（docs/plan-line-truncate-rate-gate-20261107.md）：一条出口线路常挂多个账号，
// 账号级回避挡不住「换了账号还是同一条线」；客户端重试又原样重放请求体，
// 因此按内容指纹整线避让才是对的抓手。
package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// goodStreamUpstream 完整交付一段带内容的 SSE，并统计被调用次数。
func goodStreamUpstream(calls *int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, frame := range []string{
			sseStart,
			sseFrame("content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			}),
			sseFrame("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "好线"},
			}),
			sseFrame("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": 2},
			}),
			sseFrame("message_stop", map[string]any{"type": "message_stop"}),
		} {
			_, _ = w.Write([]byte(frame))
		}
		flusher.Flush()
		time.Sleep(2 * time.Millisecond)
	})
}

// countingUpstream 包一层调用计数（断流侧用 truncatingUpstream 的内容）。
func countingUpstream(calls *int32, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		inner.ServeHTTP(w, r)
	})
}

func TestRetryRequestAvoidsTruncatedLine(t *testing.T) {
	var badCalls, goodCalls int32

	bad := httptest.NewServer(countingUpstream(&badCalls, truncatingUpstream(contentFrames)))
	t.Cleanup(bad.Close)
	good := httptest.NewServer(goodStreamUpstream(&goodCalls))
	t.Cleanup(good.Close)

	f := newDiagFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 两个账号都绑定了线路，请求不会打到夹具自带的上游；这里只做兜底。
		w.WriteHeader(http.StatusBadGateway)
	}))
	badLine, err := f.st.AddProxyProfile("bad-line", bad.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	goodLine, err := f.st.AddProxyProfile("good-line", good.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	// 关掉账号级回避与 line-guard，把本用例隔离成「只验证重试感知选线」。
	for _, kv := range [][2]string{
		{store.LineTruncateStrikesKey, "0"},
		{store.LineTruncateAvoidSecondsKey, "0"},
	} {
		if err := f.st.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}

	badAcc, err := f.st.AddAccount(model.ProviderZai, "bad-acc", "sk-bad")
	if err != nil {
		t.Fatal(err)
	}
	goodAcc, err := f.st.AddAccount(model.ProviderZai, "good-acc", "sk-good")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AssignProxyProfile(badAcc.ID, badLine.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AssignProxyProfile(goodAcc.ID, goodLine.ID); err != nil {
		t.Fatal(err)
	}
	// 首请求必须落在坏线上（否则证不了避让）：先把好线账号临时置为冷却，
	// 首请求结束后再放开——这比依赖额度优先级更确定。
	setCooling := func(accID string, until *time.Time) {
		t.Helper()
		if _, err := f.st.Update(model.ProviderZai, accID, func(a *model.Account) {
			if until == nil {
				a.Status, a.CoolingUntil = model.StatusActive, nil
				return
			}
			ts := float64(until.UnixNano()) / 1e9
			a.Status, a.CoolingUntil = model.StatusCooling, &ts
		}); err != nil {
			t.Fatal(err)
		}
	}
	coolUntil := time.Now().Add(10 * time.Minute)
	setCooling(goodAcc.ID, &coolUntil)

	// 第一次：内容已产出后断流，收尾写错误帧，并登记「这份内容在这条线上断过」。
	if status, body := f.post(t, msgBody()); status != http.StatusOK ||
		!strings.Contains(body, "event: error") {
		t.Fatalf("首请求应交付后断流（200 + 错误帧），实际 %d：%s", status, body)
	}
	if got := atomic.LoadInt32(&badCalls); got != 1 {
		t.Fatalf("首请求应走坏线一次，实际 %d", got)
	}

	// 放开好线账号：此后唯一阻碍它被选中的就是「整线避让」。
	setCooling(goodAcc.ID, nil)

	// 第二次：同一份内容（客户端重试）。必须整线避让，落到好线并完整交付。
	status, body := f.post(t, msgBody())
	if status != http.StatusOK || !strings.Contains(body, "message_stop") {
		t.Fatalf("重试应落到好线并完整交付，实际 %d：%s", status, body)
	}
	if got := atomic.LoadInt32(&badCalls); got != 1 {
		t.Fatalf("重试不得再走断流线路（坏线调用应为 1），实际 %d", got)
	}
	if got := atomic.LoadInt32(&goodCalls); got != 1 {
		t.Fatalf("重试应走好线一次，实际 %d", got)
	}
	if logs := stripANSI(f.buf.String()); !strings.Contains(logs, "同内容重试：避让") {
		t.Fatalf("缺少重试避让日志：\n%s", logs)
	}
}
