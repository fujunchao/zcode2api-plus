package gateway

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
)

const quotaRecoveryMessage = `{"id":"msg_recovered","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

// TestAPIKeyQuotaRecoveryAfterUpstreamReset 从真实 HTTP 入口验证恢复闭环：
// 耗尽时不反复冲击上游，等待后重新观测成功，而不是永远返回无可用账号。
func TestAPIKeyQuotaRecoveryAfterUpstreamReset(t *testing.T) {
	f := newFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	f.eng.SetNow(func() time.Time { return time.Unix(0, clock.Load()) })
	f.respond = func(call int, _ *http.Request) (int, http.Header, string) {
		if call == 1 {
			return jsonResp(http.StatusPaymentRequired, `{"error":{"message":"quota exhausted"}}`)(call, nil)
		}
		return jsonResp(http.StatusOK, `{"id":"msg_recovered","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)(call, nil)
	}
	acc, err := f.st.AddAccount(model.ProviderZai, "quota-recovery", "sk-synthetic-recovery")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		status, body := f.post(t, msgBody(), "sk-test")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("耗尽后应暂时排除模型，得到 %d: %s", status, body)
		}
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("等待窗口内不应重复访问上游，得到 %d 次", got)
	}
	clock.Add(int64(25 * time.Hour))
	status, body := f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK {
		t.Fatalf("上游额度恢复且等待窗口已过，模型应恢复调用，得到 %d: %s", status, body)
	}
	if got := f.st.SnapshotAccount(model.ProviderZai, acc.ID); got.ModelAvailability("GLM-5.3") == "exhausted" {
		t.Fatal("成功探测后仍保留该模型的耗尽标记")
	}
	status, body = f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK || f.callCount() != 3 {
		t.Fatalf("成功后应回到正常调度，得到 %d、调用 %d 次: %s", status, f.callCount(), body)
	}
}

func TestAPIKeyQuotaRecoveryErrorFamilies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		long   bool
	}{
		{"402", 402, `{"error":{"message":"quota exhausted"}}`, false},
		{"周用量", 429, `{"code":1310,"msg":"weekly usage cap"}`, false},
		{"每日额度", 200, `{"code":1005,"msg":"daily quota"}`, false},
		{"欠费", 429, `{"code":1113,"msg":"insufficient balance"}`, true},
		{"过期", 429, `{"code":1309,"msg":"expired"}`, true},
		{"不包含模型", 429, `{"code":1311,"msg":"model not included"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			f.eng.SetNow(func() time.Time { return time.Unix(0, clock.Load()) })
			f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
				if n == 1 {
					return jsonResp(tc.status, tc.body)(n, nil)
				}
				return jsonResp(200, quotaRecoveryMessage)(n, nil)
			}
			if _, err := f.st.AddAccount(model.ProviderZai, "family", "sk-synthetic-family"); err != nil {
				t.Fatal(err)
			}
			if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 {
				t.Fatalf("首次应耗尽: %d", status)
			}
			clock.Add(int64(5 * time.Minute))
			if tc.long {
				if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 || f.callCount() != 1 {
					t.Fatal("非短时窗口错误不应在 5 分钟后重试")
				}
				clock.Add(int64(6*time.Hour - 5*time.Minute))
			}
			if status, body := f.post(t, msgBody(), "sk-test"); status != 200 {
				t.Fatalf("等待到期且上游恢复后应成功: %d %s", status, body)
			}
		})
	}
}

func TestAPIKeyQuotaRecoveryPrefersHealthyAndLimitsProbes(t *testing.T) {
	t.Run("健康账号优先", func(t *testing.T) {
		f := newFixture(t)
		base := time.Now().Add(-time.Hour)
		bad, _ := f.st.AddAccount(model.ProviderZai, "bad", "sk-synthetic-bad")
		good, _ := f.st.AddAccount(model.ProviderZai, "good", "sk-synthetic-good")
		MarkModelExhausted(f.st, bad.Provider, bad.ID, "GLM-5.3", "quota", base)
		f.respond = jsonResp(200, quotaRecoveryMessage)
		status, _ := f.post(t, msgBody(), "sk-test")
		if status != 200 || f.lastCall().Header.Get("x-api-key") != good.Secret() {
			t.Fatal("应先使用健康账号，而不是探测已耗尽账号")
		}
	})
	t.Run("每个入站请求最多一个恢复探测", func(t *testing.T) {
		f := newFixture(t)
		base := time.Now().Add(-time.Hour)
		for _, secret := range []string{"sk-synthetic-a", "sk-synthetic-b"} {
			a, _ := f.st.AddAccount(model.ProviderZai, secret, secret)
			MarkModelExhausted(f.st, a.Provider, a.ID, "GLM-5.3", "quota", base)
		}
		f.respond = jsonResp(429, `{"code":1310,"msg":"still exhausted"}`)
		if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 || f.callCount() != 1 {
			t.Fatalf("一次失败请求不应探测整个账号池: %d, %d", status, f.callCount())
		}
	})
}

func TestAPIKeyQuotaRecoveryRequiresValidUncancelledDelivery(t *testing.T) {
	sse := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, body, contentType              string
		stream, cancel, cancelError, recover bool
	}{
		{name: "完整 JSON", body: quotaRecoveryMessage, contentType: "application/json", recover: true},
		{name: "完整 SSE", body: sse, contentType: "text/event-stream", stream: true, recover: true},
		{name: "读取后取消但返回 nil", body: quotaRecoveryMessage, contentType: "application/json", cancel: true},
		{name: "取消交付", body: quotaRecoveryMessage, contentType: "application/json", cancel: true, cancelError: true},
		{name: "非消息 JSON 携带 usage", body: `{"usage":{"input_tokens":1,"output_tokens":1}}`, contentType: "application/json"},
		{name: "SSE 业务错误", body: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n", contentType: "text/event-stream", stream: true},
		{name: "SSE 无终态", body: "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n", contentType: "text/event-stream", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			base := time.Now()
			now := base.Add(5 * time.Minute)
			f.eng.SetNow(func() time.Time { return now })
			a, _ := f.st.AddAccount(model.ProviderZai, "delivery", "sk-synthetic-delivery")
			MarkModelExhausted(f.st, a.Provider, a.ID, "GLM-5.3", "quota", base)
			f.respond = func(int, *http.Request) (int, http.Header, string) {
				return 200, http.Header{"Content-Type": []string{tc.contentType}}, tc.body
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := msgBody()
			body["stream"] = tc.stream
			f.eng.RunMessages(ctx, body, nil, func(d Delivery) error {
				_, err := io.Copy(io.Discard, d.Body)
				if tc.cancel {
					cancel()
				}
				if tc.cancelError {
					return context.Canceled
				}
				return err
			})
			got := f.st.SnapshotAccount(a.Provider, a.ID)
			if recovered := got.ModelAvailability("GLM-5.3") != "exhausted"; recovered != tc.recover {
				t.Fatalf("恢复=%v，应为 %v，不能用 HTTP 200 或 usage 单独认定成功", recovered, tc.recover)
			}
			if !tc.recover {
				probe, err := f.st.AcquireQuotaProbe(a.Provider, nil, "GLM-5.3", now.Add(15*time.Minute))
				if probe == nil || err != nil {
					t.Fatalf("失败/取消必须释放在途租约: %v", err)
				}
				_, _ = f.st.FinishQuotaProbe(probe, false, now.Add(15*time.Minute))
			}
		})
	}
}

func TestAPIKeyQuotaRecoveryDoesNotUseJWTQuotaProbe(t *testing.T) {
	f := newFixture(t)
	a, _ := f.st.AddAccount(model.ProviderZai, "jwt", "synthetic.payload.signature")
	MarkModelExhausted(f.st, a.Provider, a.ID, "GLM-5.3", "quota", time.Now().Add(-24*time.Hour))
	probe, err := f.st.AcquireQuotaProbe(a.Provider, nil, "GLM-5.3", time.Now())
	if probe != nil || err != nil {
		t.Fatalf("JWT 仍只经余额刷新恢复: %v %v", probe, err)
	}
}
