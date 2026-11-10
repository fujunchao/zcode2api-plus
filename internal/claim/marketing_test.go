// 营销触达领取链路回归：锁定「billing/preview 恒空」这个真实事故
// （2026-11-10，docs/analysis-claim-marketing-touch-20261110.md）。
//
// 上游现场形态：preview 返回 {"code":0,"msg":"","logid":...}（没有 data），
// 可领套餐号只在 marketing/touch 的 delivery 里。用例全部用注入的假客户端，
// 不允许访问真实上游。
package claim

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// marketingFake 在 fakeBilling 之上补营销触达路由（fakeBilling 只按 /billing/ 末段分发）。
type marketingFake struct {
	inner   *fakeBilling
	touch   func(upstreamCall) (int, string)
	action  func(upstreamCall) (int, string)
	touches []upstreamCall
	actions []upstreamCall
}

func captureCall(r *http.Request) upstreamCall {
	call := upstreamCall{Method: r.Method, Path: r.URL.Path, Headers: r.Header.Clone()}
	if r.Body != nil {
		call.Body, _ = io.ReadAll(r.Body)
	}
	return call
}

func (f *marketingFake) Do(r *http.Request) (*http.Response, error) {
	switch {
	case strings.Contains(r.URL.Path, marketingActionPath):
		call := captureCall(r)
		f.actions = append(f.actions, call)
		if f.action != nil {
			status, body := f.action(call)
			return jsonResponse(status, body)
		}
		return jsonResponse(200, `{"code":0}`)
	case strings.Contains(r.URL.Path, marketingTouchPath):
		call := captureCall(r)
		f.touches = append(f.touches, call)
		if f.touch != nil {
			status, body := f.touch(call)
			return jsonResponse(status, body)
		}
		return jsonResponse(200, `{"code":0,"data":{"deliveries":[]}}`)
	}
	return f.inner.Do(r)
}

// deadPreview 现场形态：HTTP 200 + 业务码 0，但没有 data（billing/preview 已停投）。
const deadPreview = `{"code":0,"msg":"","logid":"20261110120625deadbeef"}`

// bannerDelivery 服务器实测拿到的 banner 投递（保留关键结构，去掉文案与资源）。
const bannerDelivery = `{"code":0,"msg":"","data":{"server_time":1794312332,"language":"zh-CN","deliveries":[
{"campaign_id":"2101141259813175296","resource_position":"banner","priority":50,"banner":{
  "buttons":[
    {"text":{"format":"","content":""},"action":{"type":"close","args":null}},
    {"text":{"format":"","content":""},"action":{"type":"claim_zcode_plan","args":{"plan_id":"zcode-v3-start-plan-1110-nt"}}}],
  "background":{"type":"bundle","bundle":{"bundle":{"src":"https://cdn-zcode.z.ai/x.zip","sha256":"aa"},"entry":"index.html"},
    "args":{"zcode_plan":{"plan_id":"zcode-v3-start-plan-1110-nt","name":"ZCODE GLOBAL BUILD","priority":111,
      "starts_at":1794308400,"ends_at":1794358800,"entitlements":[{"entitlement_id":"ent-zcode-v3-start-plan-1110-nt-1",
        "show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token","capabilities":["model:glm-5.3-flash"],
        "grant_units":300000000,"period":"one_time","effective_at":1794322800,"priority":101}]}}},
  "success_popup":{"buttons":[],"hero":{"type":"bundle","bundle":{"bundle":{"src":"https://cdn-zcode.z.ai/y.zip","sha256":"bb"},"entry":"index.html"},
    "args":{"zcode_plan":{"plan_id":"zcode-v3-start-plan-1110-nt","name":"ZCODE GLOBAL BUILD","priority":111}}}}}}]}}`

// popupDelivery popup 形态：按钮与 zcode_plan 都挂在 popup 下。
const popupDelivery = `{"code":0,"data":{"deliveries":[{"campaign_id":"cmp-popup","resource_position":"popup","priority":10,
"popup":{"buttons":[{"action":{"type":"claim_zcode_plan","args":{"plan_id":"plan-popup"}}}],
"hero":{"args":{"zcode_plan":{"plan_id":"plan-popup","name":"弹窗套餐","priority":7,
"entitlements":[{"show_name":"GLM-5.3-Flash","meter":"model_usage","unit_type":"token","grant_units":1000,"period":"one_time"}]}}}}}]}}`

func newMarketingService(t *testing.T, up *marketingFake) *Service {
	t.Helper()
	return &Service{Captcha: newSolvedManager(t), Client: up}
}

// marketingServer 真实 HTTP 入口：直接观察线协议（query / 头 / 方法），
// 并用 MarketingBaseURL 覆写把出站钉在本机 —— 用例绝不访问真实上游。
//
// 刻意不再从 upstreamCall.Path 里解 query：url.URL.Path 不含 query（既有
// fakeBilling 的 lastSegment 匹配就是靠这一点成立的），从 Path 找 "?" 的写法
// 恒取不到 seq。
type marketingServer struct {
	server  *httptest.Server
	mu      sync.Mutex
	queries []url.Values
	headers []http.Header
	methods []string
}

func newMarketingServer(t *testing.T, reply string) *marketingServer {
	t.Helper()
	s := &marketingServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.queries = append(s.queries, r.URL.Query())
		s.headers = append(s.headers, r.Header.Clone())
		s.methods = append(s.methods, r.Method)
		s.mu.Unlock()
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(s.server.Close)
	old := MarketingBaseURL
	MarketingBaseURL = s.server.URL
	t.Cleanup(func() { MarketingBaseURL = old })
	return s
}

func (s *marketingServer) snapshot() ([]url.Values, []http.Header, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.queries...), append([]http.Header(nil), s.headers...), append([]string(nil), s.methods...)
}

// TestMarketingPlansParsesBannerAndPopupDeliveries 覆盖两种互斥投递形态与三处 zcode_plan 元数据。
func TestMarketingPlansParsesBannerAndPopupDeliveries(t *testing.T) {
	acc := newTestAccount(t)

	t.Run("banner", func(t *testing.T) {
		up := &marketingFake{inner: &fakeBilling{}, touch: func(upstreamCall) (int, string) { return 200, bannerDelivery }}
		plans, err := newMarketingService(t, up).MarketingPlans(acc)
		if err != nil || len(plans) != 1 {
			t.Fatalf("应解析出一条活动套餐: %v %v", plans, err)
		}
		got := plans[0]
		if got.PlanID != "zcode-v3-start-plan-1110-nt" || got.Name != "ZCODE GLOBAL BUILD" || got.CampaignID != "2101141259813175296" || got.Priority != 111 {
			t.Fatalf("套餐字段不符: %+v", got)
		}
		if len(got.Grants) != 1 {
			t.Fatalf("应解析出 1 项权益: %+v", got.Grants)
		}
		grant, _ := got.Grants[0].(map[string]any)
		if grant["name"] != "GLM-5.3-Flash" || grant["units"] != float64(300000000) || grant["period"] != "one_time" {
			t.Fatalf("权益字段不符: %+v", grant)
		}
	})

	t.Run("popup", func(t *testing.T) {
		up := &marketingFake{inner: &fakeBilling{}, touch: func(upstreamCall) (int, string) { return 200, popupDelivery }}
		plans, err := newMarketingService(t, up).MarketingPlans(acc)
		if err != nil || len(plans) != 1 || plans[0].PlanID != "plan-popup" || plans[0].Name != "弹窗套餐" {
			t.Fatalf("popup 形态未解析: %v %v", plans, err)
		}
	})

	t.Run("无投递不算失败", func(t *testing.T) {
		up := &marketingFake{inner: &fakeBilling{}, touch: func(upstreamCall) (int, string) {
			return 200, `{"code":0,"data":{"deliveries":[]}}`
		}}
		plans, err := newMarketingService(t, up).MarketingPlans(acc)
		if err != nil || len(plans) != 0 {
			t.Fatalf("空投递应返回空列表且不报错: %v %v", plans, err)
		}
	})
}

// TestMarketingTouchRequestShape 锁定官方形态：seq 后置自增、四头、GET 不带 Content-Type。
func TestMarketingTouchRequestShape(t *testing.T) {
	acc := newTestAccount(t)
	mid := "11111111-2222-3333-4444-555555555555"
	acc.VirtualDeviceMid = &mid
	srv := newMarketingServer(t, `{"code":0,"data":{"deliveries":[]}}`)
	svc := NewService(nil) // 真实 HTTP 客户端：出站被 MarketingBaseURL 钉在本机
	if _, err := svc.MarketingPlans(acc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarketingPlans(acc); err != nil {
		t.Fatal(err)
	}
	queries, headers, methods := srv.snapshot()
	if len(queries) != 2 || len(headers) != 2 {
		t.Fatalf("应发出 2 次营销触达查询: %d", len(queries))
	}
	if methods[0] != http.MethodGet {
		t.Fatalf("查询必须是 GET: %s", methods[0])
	}
	if got := headers[0].Get("Content-Type"); got != "" {
		t.Fatalf("GET 不得带 Content-Type: %q", got)
	}
	for _, key := range []string{"X-Title", "X-Platform", "X-Os-Category", "HTTP-Referer"} {
		if got := headers[0].Get(key); got != "" {
			t.Fatalf("营销触达不得混入 billing 来源头 %s=%q", key, got)
		}
	}
	if got := headers[0].Get("X-Device-Mid"); got != mid {
		t.Fatalf("应带账号设备指纹: %q", got)
	}
	if !strings.HasPrefix(headers[0].Get("Authorization"), "Bearer ") {
		t.Fatal("JWT 账号必须带 Authorization")
	}
	if headers[0].Get("X-ZCode-App-Version") == "" || headers[0].Get("X-Client-Language") == "" {
		t.Fatal("版本与语言头缺失")
	}
	first, err := strconv.Atoi(queries[0].Get("seq"))
	if err != nil {
		t.Fatalf("seq 必须是整数: %q", queries[0].Get("seq"))
	}
	second, err := strconv.Atoi(queries[1].Get("seq"))
	if err != nil || second != first+1 {
		t.Fatalf("seq 必须每次自增: %q → %q", queries[0].Get("seq"), queries[1].Get("seq"))
	}
}

// TestClaimablePlansPrefersMarketingWhenPreviewIsDead 事故回归用例：
// preview 恒空时旧实现返回空套餐（领取永远失败），修复后必须拿到营销触达的 plan_id。
func TestClaimablePlansPrefersMarketingWhenPreviewIsDead(t *testing.T) {
	acc := newTestAccount(t)
	up := &marketingFake{
		inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
		}},
		touch: func(upstreamCall) (int, string) { return 200, bannerDelivery },
	}
	plans, err := newMarketingService(t, up).ClaimablePlans(acc)
	if err != nil {
		t.Fatalf("营销触达命中时不应报错: %v", err)
	}
	if len(plans) != 1 || plans[0]["plan_id"] != "zcode-v3-start-plan-1110-nt" {
		t.Fatalf("必须拿到营销触达的套餐: %v", plans)
	}
	if plans[0]["campaign_id"] != "2101141259813175296" {
		t.Fatalf("套餐视图必须带 campaign_id（回执要用）: %v", plans[0])
	}
}

// TestClaimWithoutPlanIDClaimsMarketingPlan 端到端：无 plan_id → 营销触达发现 → claim → 回执。
func TestClaimWithoutPlanIDClaimsMarketingPlan(t *testing.T) {
	acc := newTestAccount(t)
	up := &marketingFake{
		inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
			"/billing/claim": func(upstreamCall) (int, string) {
				return 200, `{"code":0,"data":{"plan":{"starts_at":1,"ends_at":1794358800}}}`
			},
		}},
		touch: func(upstreamCall) (int, string) { return 200, bannerDelivery },
	}
	svc := newMarketingService(t, up)
	result, err := svc.Claim(acc, "")
	if err != nil {
		t.Fatalf("领取应成功: %v", err)
	}
	if result["plan_id"] != "zcode-v3-start-plan-1110-nt" || result["campaign_id"] != "2101141259813175296" {
		t.Fatalf("结果字段不符: %v", result)
	}
	if result["plan_name"] != "ZCODE GLOBAL BUILD" {
		t.Fatalf("套餐名应取自营销触达元数据: %v", result)
	}
	var claimCalls int
	for _, call := range up.inner.requests {
		if lastSegment(call.Path) == "claim" {
			claimCalls++
			if got := call.Headers.Get(captchaHeader); got == "" {
				t.Fatal("领取必须带验证码头")
			}
			var body map[string]string
			if err := json.Unmarshal(call.Body, &body); err != nil || body["plan_id"] != "zcode-v3-start-plan-1110-nt" {
				t.Fatalf("领取体必须用营销触达的 plan_id: %s", call.Body)
			}
		}
	}
	if claimCalls != 1 {
		t.Fatalf("应恰好领取 1 次: %d", claimCalls)
	}
	if len(up.actions) != 1 {
		t.Fatalf("领取成功后应恰好回执 1 次: %d", len(up.actions))
	}
	if up.actions[0].Method != http.MethodPost || up.actions[0].Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("回执形态不符: %s %v", up.actions[0].Method, up.actions[0].Headers)
	}
	var actionBody map[string]string
	if err := json.Unmarshal(up.actions[0].Body, &actionBody); err != nil {
		t.Fatalf("回执体不是 JSON: %s", up.actions[0].Body)
	}
	if actionBody["campaign_id"] != "2101141259813175296" || actionBody["action_type"] != "confirm" {
		t.Fatalf("回执体不符: %v", actionBody)
	}
}

// TestClaimWithoutPlanIDAndNoDelivery 无投递：给明确文案与 1005 长冷却，且不得发出领取请求。
func TestClaimWithoutPlanIDAndNoDelivery(t *testing.T) {
	acc := newTestAccount(t)
	up := &marketingFake{
		inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
			"/billing/claim":   func(upstreamCall) (int, string) { return 200, `{"code":0}` },
		}},
		touch: func(upstreamCall) (int, string) { return 200, `{"code":0,"data":{"deliveries":[]}}` },
	}
	svc := newMarketingService(t, up)
	_, err := svc.Claim(acc, "")
	if err == nil {
		t.Fatal("无活动套餐必须报错")
	}
	if !strings.Contains(err.Error(), "沒有可領取的活動套餐") {
		t.Fatalf("文案必须说明「没有活动套餐」，不能沿用 1005 的名额文案: %v", err)
	}
	// 不带业务码 = 走短冷却。营销触达的「0 条」是瞬态（同一账号十几分钟后又能
	// 看到投递），落长冷却会把可领账号锁住，与本次要修的 bug 同类。
	ce, ok := err.(*ClaimError)
	if !ok || ce.Code() != 0 {
		t.Fatalf("无投递必须是可重试语义（不带业务码）: %v", err)
	}
	failure := FailureOutcome(acc, "", err)
	if failure["code"] == float64(1005) || failure["code"] == 1005 {
		t.Fatalf("无投递不得借用 1005（长冷却）: %v", failure)
	}
	for _, call := range up.inner.requests {
		if lastSegment(call.Path) == "claim" {
			t.Fatalf("无套餐时不得发出领取请求: %v", call.Path)
		}
	}
	if len(up.actions) != 0 {
		t.Fatal("没有领取成功就没有回执")
	}
}

// TestReportMarketingClaimFailureKeepsClaimSuccess 回执失败只 warn，不改领取结论。
func TestReportMarketingClaimFailureKeepsClaimSuccess(t *testing.T) {
	acc := newTestAccount(t)
	up := &marketingFake{
		inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
			"/billing/claim":   func(upstreamCall) (int, string) { return 200, `{"code":0,"data":{}}` },
		}},
		touch:  func(upstreamCall) (int, string) { return 200, bannerDelivery },
		action: func(upstreamCall) (int, string) { return 500, `{"code":500}` },
	}
	result, err := newMarketingService(t, up).Claim(acc, "")
	if err != nil {
		t.Fatalf("回执失败不得影响领取: %v", err)
	}
	if result["plan_id"] != "zcode-v3-start-plan-1110-nt" {
		t.Fatalf("结果不符: %v", result)
	}
}

// TestClaimablePlansMarketingUnavailableFallsBack 营销触达不可用时回退 preview，
// 且回退路径的业务错误必须照样冒出来（不能被营销触达的裸错误盖掉）。
func TestClaimablePlansMarketingUnavailableFallsBack(t *testing.T) {
	acc := newTestAccount(t)

	t.Run("回退到 preview 的套餐", func(t *testing.T) {
		up := &marketingFake{
			inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/preview": func(upstreamCall) (int, string) {
					return 200, `{"code":0,"data":{"plans":[{"plan_id":"legacy","name":"旧套餐","priority":1}]}}`
				},
			}},
			touch: func(upstreamCall) (int, string) { return 500, `{"code":500}` },
		}
		plans, err := newMarketingService(t, up).ClaimablePlans(acc)
		if err != nil || len(plans) != 1 || plans[0]["plan_id"] != "legacy" {
			t.Fatalf("应回退到 preview: %v %v", plans, err)
		}
	})

	t.Run("回退路径的业务错误优先", func(t *testing.T) {
		up := &marketingFake{
			inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/preview": func(upstreamCall) (int, string) { return 200, `{"code":1004}` },
			}},
			touch: func(upstreamCall) (int, string) { return 500, `{"code":500}` },
		}
		_, err := newMarketingService(t, up).ClaimablePlans(acc)
		ce, ok := err.(*ClaimError)
		if !ok || ce.Code() != 1004 {
			t.Fatalf("必须保留回退路径的业务码: %v", err)
		}
	})

	t.Run("两边都没有套餐时按无套餐处理（不升级为错误）", func(t *testing.T) {
		up := &marketingFake{
			inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
				"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
			}},
			touch: func(upstreamCall) (int, string) { return 503, `{"code":0}` },
		}
		// preview 恒空是上游常态：这里不能变成错误态，否则「刷新资格」与自动领取
		// 会整链路失败。营销触达的异常只留日志（可观测性由 Claim 的 warn 兜底）。
		plans, err := newMarketingService(t, up).ClaimablePlans(acc)
		if err != nil || len(plans) != 0 {
			t.Fatalf("两边都空时应返回空列表且不报错: %v %v", plans, err)
		}
	})
}

// TestAutoClaimAllPlansUsesMarketingDelivery 自动路径（定时/入池）必须也能看到营销套餐。
func TestAutoClaimAllPlansUsesMarketingDelivery(t *testing.T) {
	acc := newTestAccount(t)
	up := &marketingFake{
		inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
			"/billing/claim": func(upstreamCall) (int, string) {
				return 200, `{"code":0,"data":{"plan":{"ends_at":1794358800}}}`
			},
		}},
		touch: func(upstreamCall) (int, string) { return 200, bannerDelivery },
	}
	svc := newMarketingService(t, up)
	out := svc.AutoClaimAllPlans(acc)
	if len(out) != 1 || out[0]["ok"] != true {
		t.Fatalf("自动领取应成功一条: %v", out)
	}
	if out[0]["plan_id"] != "zcode-v3-start-plan-1110-nt" {
		t.Fatalf("自动领取必须用营销触达的 plan_id: %v", out[0])
	}
	var claims int
	for _, call := range up.inner.requests {
		if lastSegment(call.Path) == "claim" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("自动领取应发出 1 次 claim: %d", claims)
	}
	if len(up.actions) != 1 {
		t.Fatalf("自动领取成功后应回执一次: %d", len(up.actions))
	}
	if len(up.touches) != 1 {
		// 上游在领到活动后就不再投递：任何「领取成功后再反查 campaign_id」的实现
		// 都会在这里多出一次必然落空的查询，回执也就丢了。
		t.Fatalf("发现阶段之后不得再查营销触达: %d", len(up.touches))
	}
}

// TestMarketingClaimDoesNotRequeryAfterClaim 固化上游真实语义：
// 账号领到该活动后投递立刻消失。若实现改成「领取成功后再反查 campaign_id」，
// 第二次查询会返回 0 条 → 回执丢失 → 本用例变红。
func TestMarketingClaimDoesNotRequeryAfterClaim(t *testing.T) {
	acc := newTestAccount(t)
	claimed := false
	up := &marketingFake{
		inner: &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
			"/billing/preview": func(upstreamCall) (int, string) { return 200, deadPreview },
			"/billing/claim": func(upstreamCall) (int, string) {
				claimed = true
				return 200, `{"code":0,"data":{"plan":{"ends_at":1794358800}}}`
			},
		}},
	}
	up.touch = func(upstreamCall) (int, string) {
		if claimed {
			// 领完之后上游不再投递这条 delivery。
			return 200, `{"code":0,"data":{"deliveries":[]}}`
		}
		return 200, bannerDelivery
	}
	out := newMarketingService(t, up).AutoClaimAllPlans(acc)
	if len(out) != 1 || out[0]["ok"] != true {
		t.Fatalf("自动领取应成功: %v", out)
	}
	if out[0]["campaign_id"] != "2101141259813175296" {
		t.Fatalf("结果必须带上发现阶段的 campaign_id: %v", out[0])
	}
	if len(up.actions) != 1 {
		t.Fatalf("领取成功后必须恰好回执一次（不能因反查落空而丢回执）: %d", len(up.actions))
	}
	var actionBody map[string]string
	if err := json.Unmarshal(up.actions[0].Body, &actionBody); err != nil || actionBody["campaign_id"] != "2101141259813175296" {
		t.Fatalf("回执体不符: %s", up.actions[0].Body)
	}
}

// TestMarketingSequenceNeverRepeats 同进程内 seq 不复用。
//
// 这是**形态护栏**，不是功能护栏：实测上游完全不校验 seq（重复、跳号、负数、
// 非数字、甚至不带 seq，全部照常返回投递），跳号不会导致领取失败。留这条用例
// 只为对齐官方 String(Xde++) 的线协议形态，删掉它不会影响功能，
// 但进程重启后计数器归零同样无害。
func TestMarketingSequenceNeverRepeats(t *testing.T) {
	acc := newTestAccount(t)
	srv := newMarketingServer(t, `{"code":0,"data":{"deliveries":[]}}`)
	svc := NewService(nil)
	for i := 0; i < 3; i++ {
		if _, err := svc.MarketingPlans(acc); err != nil {
			t.Fatal(err)
		}
	}
	queries, _, _ := srv.snapshot()
	if len(queries) != 3 {
		t.Fatalf("应发出 3 次查询: %d", len(queries))
	}
	seen := map[int]bool{}
	for _, values := range queries {
		seq, err := strconv.Atoi(values.Get("seq"))
		if err != nil {
			t.Fatalf("seq 必须是整数: %q", values.Get("seq"))
		}
		if seen[seq] {
			t.Fatalf("seq 重复: %d", seq)
		}
		seen[seq] = true
	}
}

var uuidShape = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

// TestMarketingDeviceMidIsUUIDForm 设备指纹必须是 UUID 形态：
// 官方对 deviceMid 有 uuid() 断言，非 UUID 会被上游判 400/3001，静默拿不到投递。
func TestMarketingDeviceMidIsUUIDForm(t *testing.T) {
	acc := newTestAccount(t)
	if !uuidShape.MatchString(marketingHeaders(acc)["X-Device-Mid"]) {
		t.Fatalf("回退到全局设备指纹时也必须是 UUID: %q", marketingHeaders(acc)["X-Device-Mid"])
	}
	mid := "abcdefab-1234-5678-9abc-def012345678"
	acc.VirtualDeviceMid = &mid
	if got := marketingHeaders(acc)["X-Device-Mid"]; got != mid {
		t.Fatalf("应优先用账号自己的指纹: %q", got)
	}
}
