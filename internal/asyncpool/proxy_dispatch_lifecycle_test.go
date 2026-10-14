package asyncpool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
)

const completeProxyFixtureSSE = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

func TestAsyncProxyPermitReleasedOnAllHTTPExits(t *testing.T) {
	for _, kind := range []string{"success", "error", "risk", "truncated", "network", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			p, st, _, _ := newTestPool(t)
			t.Cleanup(p.Close)
			acc := addJWTAccount(t, st, "permit-fixture")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				summary, err := st.StrictProxySummary()
				if err != nil || summary.ActiveDispatch != 1 {
					t.Errorf("真实 HTTP 进入代理时必须持有且只持有一个许可：%+v %v", summary, err)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				switch kind {
				case "network":
					panic(http.ErrAbortHandler)
				case "canceled":
					cancel()
					<-r.Context().Done()
				case "error":
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"code":9000}`)
				case "risk":
					w.WriteHeader(http.StatusMethodNotAllowed)
					_, _ = io.WriteString(w, `{"code":3012,"msg":"blocked due to unusual activity"}`)
				default:
					w.Header().Set("Content-Type", "text/event-stream")
					body := completeProxyFixtureSSE
					if kind == "truncated" {
						body = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n"
					}
					_, _ = io.WriteString(w, body)
				}
			}))
			t.Cleanup(up.Close)
			config.UpstreamZai = up.URL + "/v1/messages"
			line, err := st.AddProxyProfile("actual-proxy", up.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AssignProxyProfile(acc.ID, line.ID); err != nil {
				t.Fatal(err)
			}
			insertTicket(p, "permit", map[string]any{"model": "GLM-5.3", "messages": []any{}})
			p.processTicket(ctx, "permit")
			summary, err := st.StrictProxySummary()
			if calls.Load() != 1 || err != nil || summary.ActiveDispatch != 0 {
				t.Fatalf("结束/错误/取消都必须释放许可：calls=%d summary=%+v err=%v", calls.Load(), summary, err)
			}
		})
	}
}

func TestAsyncInvalidManualProxyIsTerminalWithoutDialOrAccountPenalty(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	t.Cleanup(p.Close)
	acc := addJWTAccount(t, st, "invalid-manual-route")
	config.AsyncMaxRetries = 3 // 确保终态不是偶然耗尽普通网络重试预算。
	addJWTAccountN(t, st, "another-account", 2)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completeProxyFixtureSSE)
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL
	if _, err := st.Update(acc.Provider, acc.ID, func(live *model.Account) {
		invalid := "unsupported-proxy://127.0.0.1:9999"
		live.ProxyURL, live.ProxyID = &invalid, nil
	}); err != nil {
		t.Fatal(err)
	}
	tk := insertTicket(p, "invalid-route", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "invalid-route")
	events := drainEvents(tk)
	if len(events) == 0 || events[len(events)-1].Type != "error" {
		t.Fatalf("无效手工路由应终态报错：%v", events)
	}
	errorObject := events[len(events)-1].Data.(map[string]any)["error"].(map[string]any)
	if errorObject["type"] != "selected_proxy_unavailable" || calls.Load() != 0 {
		t.Fatalf("不能静默直连/换号执行：error=%v calls=%d", errorObject, calls.Load())
	}
	for _, live := range st.ListAccounts(model.ProviderZai) {
		if live.FailCount != 0 || live.RiskControlStreak != 0 || live.CoolingUntil != nil || live.Status != model.StatusActive {
			t.Fatal("本地路由拒绝不能惩罚账号")
		}
	}
}

// 从真实模型 HTTP 收包到严格规则闭环：只有完整成功与完整 3012 能构成线路对照。
// 账号/路由切换仅发生在合成隔离存储，直接驱动一次派发以固定对照条件，不改变生产冷却策略。
func TestAsyncStrictProxyObservationRequiresCompleteBodies(t *testing.T) {
	for _, kind := range []string{"complete", "truncated_success", "json_not_stream", "oversized_3012", "aborted_3012"} {
		t.Run(kind, func(t *testing.T) {
			p, st, _, _ := newTestPool(t)
			t.Cleanup(p.Close)
			acc := addJWTAccount(t, st, "strict-observation")
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("本用例必须经实际代理，不能暗中直连")
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(target.Close)
			config.UpstreamZai = target.URL + "/v1/messages"
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "json_not_stream" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"type":"message","content":[],"stop_reason":"end_turn"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				body := completeProxyFixtureSSE
				if kind == "truncated_success" {
					body = strings.ReplaceAll(body, "data: {\"type\":\"message_stop\"}\n\n", "")
				}
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(good.Close)
			var badCalls atomic.Int32
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				badCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if kind == "aborted_3012" {
					w.Header().Set("Content-Length", "1024")
				}
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = io.WriteString(w, `{"code":3012,"msg":"blocked due to unusual activity"}`)
				if kind == "oversized_3012" {
					_, _ = io.WriteString(w, strings.Repeat(" ", 70*1024))
				}
			}))
			t.Cleanup(bad.Close)
			goodLine, err := st.AddProxyProfile("good-control", good.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			badLine, err := st.AddProxyProfile("bad-candidate", bad.URL, true)
			if err != nil {
				t.Fatal(err)
			}
			body := map[string]any{"model": "GLM-5.3", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "固定的合成线路对照"}}}
			gateway.NormalizeBody(body, true)
			attr := upstream.NewAttribution(map[string]string{"X-Session-Id": "same-logical-session", "X-Zcode-Query-Source": "main_turn"}, "")
			upstream.InjectDeviceMetadata(body, acc.DeviceMidOr(config.DeviceMid()), attr.SessionID)
			payload, err := marshalJSON(body)
			if err != nil {
				t.Fatal(err)
			}
			var lastBad *model.Account
			for i, line := range []store.ProxyProfile{goodLine, badLine, badLine} {
				if _, err := st.AssignProxyProfile(acc.ID, line.ID); err != nil {
					t.Fatal(err)
				}
				snapshot := st.FindAny(acc.ID)
				if i > 0 {
					lastBad = snapshot
				}
				req, err := upstream.BuildRequestWithAttribution(snapshot, "", "", nil, attr)
				if err != nil {
					t.Fatal(err)
				}
				id := fmt.Sprintf("independent-entry-%d", i)
				insertTicket(p, id, body)
				_, _ = p.attemptUpstreamOnce(context.Background(), id, snapshot, "GLM-5.3", req, payload, nil, nil)
			}
			summary, err := st.StrictProxySummary()
			if err != nil || summary.ActiveDispatch != 0 || badCalls.Load() != 2 {
				t.Fatalf("观察结束应释放所有许可：summary=%+v calls=%d err=%v", summary, badCalls.Load(), err)
			}
			wantBlocks := 0
			if kind == "complete" {
				wantBlocks = 1
			}
			if len(summary.Blocks) != wantBlocks {
				t.Fatalf("严格对照不能把不完整结果当证据：kind=%s blocks=%+v", kind, summary.Blocks)
			}
			if kind == "complete" {
				req, err := upstream.BuildRequestWithAttribution(lastBad, "", "", nil, attr)
				if err != nil {
					t.Fatal(err)
				}
				_, err = p.attemptUpstreamOnce(context.Background(), "late-entry", lastBad, "GLM-5.3", req, payload, nil, nil)
				if !errors.Is(err, store.ErrProxyRetired) || badCalls.Load() != 2 {
					t.Fatal("判定生效后即使持有旧账号快照，也不能取得新派发许可")
				}
			}
		})
	}
}

func TestAsyncForcedDirectTruncationDoesNotTouchBoundProxy(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	t.Cleanup(p.Close)
	acc := addJWTAccount(t, st, "forced-direct-truncation")
	unused, err := st.AddProxyProfile("unused", "http://127.0.0.1:1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(acc.ID, unused.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("async_force_direct", "true"); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n")
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL
	for i := range 3 {
		id := fmt.Sprintf("forced-truncation-%d", i)
		insertTicket(p, id, map[string]any{"model": "GLM-5.3", "messages": []any{}})
		p.processTicket(context.Background(), id)
		// 只把合成账号的断流避让期限调旧，保留计数，用于验证线路级阈值不被误推进。
		_, _ = st.Update(acc.Provider, acc.ID, func(live *model.Account) { live.TruncateAvoidUntil = float64(time.Now().Add(-time.Minute).Unix()) })
	}
	profiles := st.ListProxyProfiles()
	if len(profiles) != 1 || profiles[0].ID != unused.ID || st.FindAny(acc.ID).ProxyID == nil || *st.FindAny(acc.ID).ProxyID != unused.ID {
		t.Fatal("强制直连上的断流不能计入/删除未使用的绑定代理")
	}
}
