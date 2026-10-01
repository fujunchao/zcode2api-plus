package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

func TestRiskProxyRotationUsesNewEgressAfterCooling(t *testing.T) {
	for _, risk := range []bool{false, true} {
		t.Run(map[bool]string{false: "普通405不换线", true: "风控405换线"}[risk], func(t *testing.T) {
			f := newFixture(t)
			body := `{"error":{"message":"method not allowed"}}`
			if risk {
				body = `{"error":{"message":"blocked due to unusual activity"}}`
			}
			f.respond = jsonResp(405, body)
			var newCalls atomic.Int32
			newProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				newCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(okUpstreamJSON))
			}))
			t.Cleanup(newProxy.Close)
			old, _ := f.st.AddProxyProfile("old", f.upstream.URL, true)
			next, _ := f.st.AddProxyProfile("next", newProxy.URL, true)
			a, _ := f.st.AddAccount(model.ProviderZai, "risk", "sk-test-risk")
			if _, err := f.st.AssignProxyProfile(a.ID, old.ID); err != nil {
				t.Fatal(err)
			}
			status, _ := f.post(t, msgBody(), "sk-test")
			got := f.st.FindAny(a.ID)
			if newCalls.Load() != 0 || f.callCount() != 1 {
				t.Fatal("换代理不得立即在同一账号重试")
			}
			if !risk {
				if status != 405 || got.ProxyID == nil || *got.ProxyID != old.ID {
					t.Fatal("普通 405 不应更换代理")
				}
				return
			}
			if status != 503 || got.ProxyID == nil || *got.ProxyID != next.ID || got.ProxyURL == nil || *got.ProxyURL != next.URL ||
				got.Status != model.StatusCooling || got.RiskControlStreak != 1 || got.CoolingUntil == nil {
				t.Fatalf("风控后应换线并保留整号冷却: status=%d account=%+v", status, got)
			}
			// 模拟冷却到期，下一次真正出站必须使用新代理。
			_, _ = f.st.Update(a.Provider, a.ID, func(live *model.Account) {
				expired := float64(time.Now().Add(-time.Second).Unix())
				live.CoolingUntil = &expired
			})
			if status, body := f.post(t, msgBody(), "sk-test"); status != 200 {
				t.Fatalf("冷却到期后应从新代理成功: %d %s", status, body)
			}
			if newCalls.Load() != 1 || f.callCount() != 1 {
				t.Fatal("后续请求未使用新代理")
			}
		})
	}
}

func TestRiskProxyAttemptLogsKeepEveryEgress(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(405, `{"error":{"message":"blocked due to unusual activity"}}`)
	nextServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okUpstreamJSON))
	}))
	t.Cleanup(nextServer.Close)
	old, _ := f.st.AddProxyProfile("old", f.upstream.URL, true)
	next, _ := f.st.AddProxyProfile("next", nextServer.URL, true)
	a, _ := f.st.AddAccount(model.ProviderZai, "first", "sk-first")
	b, _ := f.st.AddAccount(model.ProviderZai, "second", "sk-second")
	_, _ = f.st.AssignProxyProfile(a.ID, old.ID)
	_, _ = f.st.AssignProxyProfile(b.ID, next.ID)
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("换号应完成请求：%d", status)
	}
	logs := stripANSI(buf.String())
	for _, want := range []string{"[attempt]", "attempt=1", "attempt=2", "event=headers", "status=405", "status=200",
		`acc_id="` + a.ID + `"`, `acc_id="` + b.ID + `"`, `route_id="` + old.ID + `"`, `route_id="` + next.ID + `"`,
		`from_id="` + old.ID + `"`, `to_id="` + next.ID + `"`, "result=changed"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("缺少逐次出口/换线关联 %q：\n%s", want, logs)
		}
	}
}

func TestRiskProxyRotationPreservesInvalidEscalation(t *testing.T) {
	st := openStore(t)
	old, _ := st.AddProxyProfile("old", "http://127.0.0.1:18080", true)
	next, _ := st.AddProxyProfile("next", "http://127.0.0.1:18081", true)
	a, _ := st.AddAccount(model.ProviderZai, "risk", "sk-test-risk")
	_, _ = st.AssignProxyProfile(a.ID, old.ID)
	_, _ = st.Update(a.Provider, a.ID, func(live *model.Account) { live.RiskControlStreak = 3 })
	_, streak, invalid := MarkRiskControl(st, st.FindAny(a.ID), "风控拦截", time.Now())
	got := st.FindAny(a.ID)
	if !invalid || streak != 4 || got.Status != model.StatusInvalid || got.CoolingUntil != nil ||
		got.ProxyID == nil || *got.ProxyID != next.ID {
		t.Fatal("自动换代理不能阻止超阶梯失效")
	}
}
