// claim 包单测：对照 Python 主仓 tests/test_claim.py 的用例语义。
// billing 上游用注入的假 HTTPClient 模拟（离线稳定）；遥测端点覆写 EventReportURL。
package claim

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// fakeBilling 假计费上游：按注册路径返回响应体；记录每次收包。
type fakeBilling struct {
	responses map[string]func(call upstreamCall) (int, string)
	requests  []upstreamCall
}

type upstreamCall struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

func (f *fakeBilling) Do(r *http.Request) (*http.Response, error) {
	call := upstreamCall{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone()}
	if r.Body != nil {
		call.Body, _ = io.ReadAll(r.Body)
	}
	f.requests = append(f.requests, call)
	// 真实 URL 含 zcode-plan 前缀；按末段匹配（/billing/preview、/billing/claim）
	handler, ok := f.responses["/billing/"+lastSegment(call.Path)]
	if !ok {
		return jsonResponse(http.StatusNotFound, `{"code":3001}`)
	}
	status, body := handler(call)
	return jsonResponse(status, body)
}

// lastSegment 取路径最后一段。
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func jsonResponse(status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func newTestAccount(t *testing.T) *model.Account {
	t.Helper()
	oldDB, oldData := config.DBPath, config.DataDir
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DBPath, config.DataDir = oldDB, oldData })
	jwt := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u123"}`)) + ".sig"
	return &model.Account{Provider: model.ProviderZai, Name: "t", Mode: "jwt", JWTToken: &jwt}
}

// stubSolver 验证码假求解器（按序列返回参数；用尽后固定 "ok"）。
type stubSolver struct{ params []string }

func (s *stubSolver) Solve(context.Context, captcha.Config) (string, error) {
	if len(s.params) > 1 {
		p := s.params[0]
		s.params = s.params[1:]
		return p, nil
	}
	if len(s.params) == 1 {
		return s.params[0], nil
	}
	return "ok", nil
}

func (s *stubSolver) Close() error { return nil }

// newSolvedManager 假求解 + 固定配置（离线稳定）的 manager。
func newSolvedManager(t *testing.T, params ...string) *captcha.Manager {
	t.Helper()
	old := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = old })
	cm := captcha.NewManager()
	cm.SetSolver(&stubSolver{params: params})
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "no8xfe", Region: "sgp", SceneID: "11xygtvd"}, nil
	})
	return cm
}

func TestParsePlan(t *testing.T) {
	plan := ParsePlan(map[string]any{
		"plan_id": "p1", "name": "新人套餐", "priority": float64(10),
		"entitlements": []any{
			map[string]any{"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3", "grant_units": float64(1000000)},
			map[string]any{"meter": "other", "unit_type": "token", "show_name": "忽略"},
			map[string]any{"meter": "model_usage", "unit_type": "request", "show_name": "忽略2"},
		},
	})
	if plan == nil {
		t.Fatal("应解析出套餐")
	}
	if plan["plan_id"] != "p1" || plan["name"] != "新人套餐" {
		t.Fatalf("字段不符: %v", plan)
	}
	grants, _ := plan["grants"].([]any)
	if len(grants) != 1 {
		t.Fatalf("应 1 个 token 授权: %v", grants)
	}
	if ParsePlan(map[string]any{"name": "无 id"}) != nil {
		t.Fatal("缺 plan_id 应返回 nil")
	}
}

func TestClaimSuccessWith3007Retry(t *testing.T) {
	acc := newTestAccount(t)
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"p1","name":"A","priority":5}]}}`
		},
		"/billing/claim": func(call upstreamCall) (int, string) {
			if call.Headers.Get("X-Aliyun-Captcha-Verify-Param") == "first" {
				return 200, `{"code":3007,"msg":"captcha"}`
			}
			return 200, `{"code":0,"data":{}}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t, "first", "second"), Client: up}

	result, err := svc.Claim(acc, "p1")
	if err != nil {
		t.Fatalf("3007 后换码重试应成功: %v", err)
	}
	if result["plan_id"] != "p1" {
		t.Fatalf("结果不符: %v", result)
	}
	if len(up.requests) != 2 { // 显式 plan_id 无需 preview：2 次 claim
		t.Fatalf("应 2 次上游请求: %d", len(up.requests))
	}
	// 最后一次 claim 应带换新后的验证码与区域头
	claimCall := up.requests[len(up.requests)-1]
	if claimCall.Headers.Get(captchaHeader) != "second" {
		t.Fatalf("换码重试应携带新验证码: %q", claimCall.Headers.Get(captchaHeader))
	}
	if claimCall.Headers.Get(captchaRegionHeader) != "sgp" {
		t.Fatalf("应携带验证码区域头: %q", claimCall.Headers.Get(captchaRegionHeader))
	}
}

func TestClaimBusinessCodes(t *testing.T) {
	cases := []struct {
		code    string
		wantSub string
	}{
		{"1003", "領取過"}, {"1005", "名額已用完"}, {"401", "登入"}, {"9999", "領取失敗"},
	}
	for _, c := range cases {
		t.Run(c.wantSub, func(t *testing.T) {
			acc := newTestAccount(t)
			up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/claim": func(upstreamCall) (int, string) {
					return 200, `{"code":` + c.code + `}`
				},
			}}
			svc := &Service{Captcha: newSolvedManager(t), Client: up}
			_, err := svc.Claim(acc, "p1")
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("code %s 应报 %q: %v", c.code, c.wantSub, err)
			}
		})
	}
}

func TestClaimSurfacesUpstreamNextAt(t *testing.T) {
	// 上游在成功响应与 1005 里都会给 data.plan.ends_at——这是它自己算好的
	// 下次可领时间，此前只取了错误文案把时间丢掉。
	t.Run("成功响应回带 next_at", func(t *testing.T) {
		acc := newTestAccount(t)
		up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/claim": func(upstreamCall) (int, string) {
				return 200, `{"code":0,"data":{"plan":{"starts_at":1700000000,"ends_at":1700003600}}}`
			},
		}}
		svc := &Service{Captcha: newSolvedManager(t), Client: up}
		result, err := svc.Claim(acc, "p1")
		if err != nil {
			t.Fatalf("领取应成功: %v", err)
		}
		if next, ok := result["next_at"].(float64); !ok || next != 1700003600 {
			t.Fatalf("应回带上游 ends_at: %v", result)
		}
	})

	t.Run("1005 带出 ends_at 与业务码", func(t *testing.T) {
		acc := newTestAccount(t)
		up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/claim": func(upstreamCall) (int, string) {
				return 200, `{"code":1005,"msg":"quota","data":{"plan":{"ends_at":1700007200}}}`
			},
		}}
		svc := &Service{Captcha: newSolvedManager(t), Client: up}
		_, err := svc.Claim(acc, "p1")
		var ce *ClaimError
		if !errors.As(err, &ce) {
			t.Fatalf("应为业务失败: %v", err)
		}
		if ce.Code() != 1005 {
			t.Fatalf("业务码不符: %d", ce.Code())
		}
		if ce.NextAt() == nil || *ce.NextAt() != 1700007200 {
			t.Fatalf("应带出 ends_at: %v", ce.NextAt())
		}
	})

	t.Run("毫秒时间戳归一为秒", func(t *testing.T) {
		acc := newTestAccount(t)
		up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/claim": func(upstreamCall) (int, string) {
				return 200, `{"code":0,"data":{"plan":{"ends_at":1700003600000}}}`
			},
		}}
		svc := &Service{Captcha: newSolvedManager(t), Client: up}
		result, err := svc.Claim(acc, "p1")
		if err != nil {
			t.Fatalf("领取应成功: %v", err)
		}
		if next, _ := result["next_at"].(float64); next != 1700003600 {
			t.Fatalf("毫秒应归一为秒: %v", result["next_at"])
		}
	})

	t.Run("缺 ends_at 时不写入该键", func(t *testing.T) {
		acc := newTestAccount(t)
		up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/claim": func(upstreamCall) (int, string) {
				return 200, `{"code":0,"data":{}}`
			},
		}}
		svc := &Service{Captcha: newSolvedManager(t), Client: up}
		result, err := svc.Claim(acc, "p1")
		if err != nil {
			t.Fatalf("领取应成功: %v", err)
		}
		if _, ok := result["next_at"]; ok {
			t.Fatalf("缺 ends_at 时不应有 next_at 键: %v", result)
		}
	})
}

func TestClaimUsesAccountDeviceMid(t *testing.T) {
	// 每账号独立指纹：有值时必须用账号自己的，缺失才回退全局值。
	newUpstream := func() *fakeBilling {
		return &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/claim": func(upstreamCall) (int, string) { return 200, `{"code":0,"data":{}}` },
		}}
	}

	acc := newTestAccount(t)
	mid := "mid-acc-1"
	acc.VirtualDeviceMid = &mid
	up := newUpstream()
	svc := &Service{Captcha: newSolvedManager(t), Client: up}
	if _, err := svc.Claim(acc, "p1"); err != nil {
		t.Fatalf("领取应成功: %v", err)
	}
	if got := up.requests[0].Headers.Get("X-Device-Mid"); got != "mid-acc-1" {
		t.Fatalf("应使用账号自己的设备指纹: %q", got)
	}

	acc2 := newTestAccount(t)
	up2 := newUpstream()
	svc2 := &Service{Captcha: newSolvedManager(t), Client: up2}
	if _, err := svc2.Claim(acc2, "p1"); err != nil {
		t.Fatalf("领取应成功: %v", err)
	}
	global := config.DeviceMid()
	if global == "" {
		t.Fatal("全局设备指纹不应为空")
	}
	if got := up2.requests[0].Headers.Get("X-Device-Mid"); got != global {
		t.Fatalf("未分配时应回退全局值: got %q want %q", got, global)
	}
}

func TestClaimRejectsAPIKeyAccount(t *testing.T) {
	acc := newTestAccount(t)
	key := "sk-x"
	acc.Mode = "apiKey"
	acc.JWTToken = nil
	acc.APIKey = &key
	svc := &Service{Captcha: newSolvedManager(t), Client: &fakeBilling{}}
	_, err := svc.Claim(acc, "p1")
	if err == nil || !strings.Contains(err.Error(), "JWT") {
		t.Fatalf("API Key 账号应拒绝: %v", err)
	}
}

func TestAutoClaimPriorityOrder(t *testing.T) {
	acc := newTestAccount(t)
	var claimed []string
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[
				{"plan_id":"low","name":"低","priority":1},
				{"plan_id":"high","name":"高","priority":9}]}}`
		},
		"/billing/claim": func(upstreamCall) (int, string) {
			claimed = append(claimed, "ok")
			return 200, `{"code":0}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t), Client: up}
	outcomes := svc.AutoClaimAllPlans(acc)
	if len(outcomes) != 2 {
		t.Fatalf("应领取 2 个套餐: %v", outcomes)
	}
	for _, o := range outcomes {
		if ok, _ := o["ok"].(bool); !ok {
			t.Fatalf("全部应成功: %v", outcomes)
		}
	}
	// preview 排序：high 在前
	if len(claimed) != 2 {
		t.Fatalf("应 2 次 claim: %v", claimed)
	}
	first := outcomes[0]["plan_id"]
	if first != "high" {
		t.Fatalf("应先领高优先级套餐: %v", outcomes[0]["plan_id"])
	}
}

func TestActivationReportingTolerated(t *testing.T) {
	acc := newTestAccount(t)
	oldURL := EventReportURL
	EventReportURL = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	})).URL

	// 零值 Service + 无代理账号 = 直连出站（clientFor 的无线路分支）。
	svc := &Service{}
	if err := svc.ReportActivationEvents(acc); err != "" {
		t.Fatalf("激活上报应成功: %s", err)
	}

	// 失败上报仅返回文案，不 panic；preview 不被阻断由 AutoClaimAllPlans 覆盖
	t.Cleanup(func() { EventReportURL = oldURL })
	EventReportURL = "http://127.0.0.1:1/nope"
	if err := svc.ReportActivationEvents(acc); err == "" {
		t.Fatal("不可达端点应返回错误文案")
	}
}

// TestActivationEventsUseAccountEgress：激活事件必须与 billing 请求走**同一个
// 出站客户端**（Service.clientFor）——真实客户端的 event/report 与
// billing/preview 永远同 IP；此前事件用独立的裸直连客户端，账号绑线路时同一
// device_mid 会从两个 IP 出现（2026-09-23 事故：激活成功、preview 恒空）。
func TestActivationEventsUseAccountEgress(t *testing.T) {
	acc := newTestAccount(t)
	var eventBodies []string
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		// EventReportURL 的末段是 report；fakeBilling 按「/billing/末段」分发。
		"/billing/report": func(call upstreamCall) (int, string) {
			eventBodies = append(eventBodies, string(call.Body))
			return 200, `{"code":0}`
		},
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[{"plan_id":"p1","name":"体验","priority":5}]}}`
		},
		"/billing/claim": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plan":{"plan_id":"p1","plan_name":"体验"}}}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t), Client: up}
	outcomes := svc.AutoClaimAllPlans(acc)
	if len(outcomes) != 1 || outcomes[0]["ok"] != true {
		t.Fatalf("应成功领取 1 个套餐: %v", outcomes)
	}

	// 两个激活事件先于 preview 发生，且都经过注入的同一个客户端。
	if len(eventBodies) != 2 {
		t.Fatalf("应上报 2 个激活事件，实际 %d（requests=%v）", len(eventBodies), up.requests)
	}
	if !strings.Contains(eventBodies[0], "app_launch") || !strings.Contains(eventBodies[1], "app_daily_active") {
		t.Fatalf("激活事件体缺失元素名: %v", eventBodies)
	}
	sawPreview := false
	for i, call := range up.requests {
		if strings.HasSuffix(call.Path, "/event/report") {
			if sawPreview {
				t.Fatalf("激活事件必须先于 preview 发出（第 %d 个请求才上报事件）", i)
			}
			continue
		}
		if strings.Contains(call.Path, "/billing/preview") {
			sawPreview = true
		}
	}
	if !sawPreview {
		t.Fatal("未见 preview 请求经过注入客户端")
	}
}

func TestJWTUserID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"u123"}`))
	if id := JWTUserID("h." + payload + ".sig"); id != "u123" {
		t.Fatalf("user_id 提取不符: %q", id)
	}
	if JWTUserID("not-a-jwt") != "" {
		t.Fatal("非 JWT 应返回空")
	}
}

func TestPreviewPlansSortedAndBusinessError(t *testing.T) {
	acc := newTestAccount(t)
	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"plans":[
				{"plan_id":"b","priority":2},{"plan_id":"a","priority":2},
				{"plan_id":"c","priority":5}]}}`
		},
	}}
	svc := &Service{Captcha: newSolvedManager(t), Client: up}
	plans, err := svc.PreviewPlans(acc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 3 || plans[0]["plan_id"] != "c" || plans[1]["plan_id"] != "a" {
		t.Fatalf("应按 priority 降序、plan_id 升序: %v", plans)
	}

	// 业务码非 0 → ClaimError 文案
	up.responses["/billing/preview"] = func(upstreamCall) (int, string) {
		return 200, `{"code":1002}`
	}
	_, err = svc.PreviewPlans(acc)
	if err == nil || !strings.Contains(err.Error(), "活動已結束") {
		t.Fatalf("1002 应翻译为文案: %v", err)
	}
}
