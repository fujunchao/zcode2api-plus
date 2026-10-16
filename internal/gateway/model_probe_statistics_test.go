package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 从真实网关HTTP入口验证统计，不能只在Store手工塞入样本后声称调用链已接通。
func TestModelProbeAccountStatisticsSyncHTTP(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		success bool
	}{
		{"完整成功", 200, strictCompleteJSON, true},
		{"鉴权失败", 401, `{}`, false},
		{"限流", 429, `{"error":{"message":"rate limited"}}`, false},
		{"平台故障", 503, `{"error":{"message":"unavailable"}}`, false},
		{"200额度错误", 200, `{"code":"1005","message":"quota"}`, false},
		{"不完整模型消息", 200, `{"usage":{"input_tokens":1,"output_tokens":1}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldData, oldBrowser := config.DataDir, config.CaptchaBrowserEnabled
			config.DataDir, config.CaptchaBrowserEnabled = t.TempDir(), true
			t.Cleanup(func() { config.DataDir, config.CaptchaBrowserEnabled = oldData, oldBrowser })
			f := newFixture(t)
			f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) { return captcha.DefaultConfig, nil })
			f.cm.SetSolver(stubSolver{token: "synthetic"})
			a, err := f.st.AddAccount(model.ProviderZai, "合成统计账号", "header.payload.signature")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.Update(a.Provider, a.ID, func(live *model.Account) {
				live.Quota = map[string]map[string]any{store.ModelProbeModel: {"available": 10000, "remaining": 10000}}
			}); err != nil {
				t.Fatal(err)
			}
			f.respond = jsonResp(tc.status, tc.body)
			body := msgBody()
			body["model"] = store.ModelProbeModel
			f.post(t, body, "sk-test")
			if f.callCount() == 0 {
				t.Fatal("夹具没有触达实际模型HTTP")
			}
			view, err := f.st.ModelProbeSummary(time.Now())
			if err != nil || len(view.Accounts) != 1 {
				t.Fatalf("缺少账号统计：%+v %v", view, err)
			}
			got := view.Accounts[0]
			if got.RecentTotal != f.callCount() || (got.RecentSuccess > 0) != tc.success {
				t.Fatalf("每次真实尝试都应计入且只以完整模型成功为成功：calls=%d account=%+v", f.callCount(), got)
			}
			if got.ActiveRequests != 0 {
				t.Fatal("请求完成后业务占用未释放")
			}
			strict, err := f.st.StrictProxySummary()
			if err != nil || len(strict.Blocks) != 0 {
				t.Fatal("新增统计不得把普通失败变成退役证据")
			}
		})
	}
}

func TestModelProbeAccountStatisticsInterruptedBody(t *testing.T) {
	oldData, oldBrowser := config.DataDir, config.CaptchaBrowserEnabled
	config.DataDir, config.CaptchaBrowserEnabled = t.TempDir(), true
	t.Cleanup(func() { config.DataDir, config.CaptchaBrowserEnabled = oldData, oldBrowser })
	f := newFixture(t)
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) { return captcha.DefaultConfig, nil })
	f.cm.SetSolver(stubSolver{token: "synthetic"})
	if _, err := f.st.AddAccount(model.ProviderZai, "合成断流统计账号", "header.payload.signature"); err != nil {
		t.Fatal(err)
	}
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		return 200, http.Header{"Content-Type": {"application/json"}, "Content-Length": {"9999"}}, `{}`
	}
	body := msgBody()
	body["model"] = store.ModelProbeModel
	f.post(t, body, "sk-test")
	view, err := f.st.ModelProbeSummary(time.Now())
	if err != nil || len(view.Accounts) != 1 || view.Accounts[0].RecentTotal != 1 || view.Accounts[0].RecentSuccess != 0 || view.Accounts[0].NeutralFailures != 1 {
		t.Fatalf("读断不能漏出分母或误当账号自身故障：%+v %v", view, err)
	}
}
