// 网关端到端测试：httptest mock 上游，覆盖鉴权、透传、错误分类链与账号状态迁移。
// 对应移植 Python 版 tests/test_gateway_captcha.py / test_model_routing.py 的路由级用例。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// upstreamCall 记录一次上游收包。
type upstreamCall struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

// responder 由测试注入：第 n 次上游调用返回什么。
type responder func(call int, r *http.Request) (status int, header http.Header, body string)

type fixture struct {
	srv      *httptest.Server // 网关入口
	upstream *httptest.Server // mock 上游
	st       *store.Store
	cm       *captcha.Manager
	eng      *Engine

	mu      sync.Mutex
	calls   []upstreamCall
	respond responder
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		f.mu.Lock()
		f.calls = append(f.calls, upstreamCall{Path: r.URL.Path, Header: r.Header.Clone(), Body: parsed})
		n := len(f.calls)
		respond := f.respond
		f.mu.Unlock()
		if respond == nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		status, header, body := respond(n, r)
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	f.upstream = up
	t.Cleanup(up.Close)

	f.st = openStore(t)
	_ = f.st.SetSetting("gateway_key", "sk-test")
	f.cm = captcha.NewManager()
	f.eng = NewEngine(f.st, f.cm, nil)
	f.eng.BusyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	// 瞬时限流的原地重试同样注入零延迟，避免每个相关用例都真等 1 秒
	f.eng.RateLimitRetryDelay = 0
	h := &Handler{Engine: f.eng, Auth: auth.New(f.st)}
	mux := http.NewServeMux()
	h.Register(mux)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	oldZai, oldFB := config.UpstreamZai, config.UpstreamZaiFallback
	config.UpstreamZai = up.URL + "/zai"
	config.UpstreamZaiFallback = up.URL + "/fallback"
	t.Cleanup(func() { config.UpstreamZai, config.UpstreamZaiFallback = oldZai, oldFB })
	return f
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	old := config.DBPath
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	t.Cleanup(func() { config.DBPath = old })
	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func (f *fixture) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fixture) lastCall() upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fixture) post(t *testing.T, body map[string]any, key string) (int, string) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func msgBody() map[string]any {
	return map[string]any{
		"model":      "GLM-5.3",
		"max_tokens": 8,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

func jsonResp(status int, body string) responder {
	return func(int, *http.Request) (int, http.Header, string) {
		return status, http.Header{"Content-Type": []string{"application/json"}}, body
	}
}

const okUpstreamJSON = `{"id":"msg_1","usage":{"input_tokens":11,"output_tokens":22}}`

func TestAuthEnforced(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)

	status, body := f.post(t, msgBody(), "")
	if status != 401 || !strings.Contains(body, "缺少 API Key") {
		t.Fatalf("无密钥应 401: %d %s", status, body)
	}
	status, body = f.post(t, msgBody(), "sk-wrong")
	if status != 403 || !strings.Contains(body, "API Key 无效") {
		t.Fatalf("错密钥应 403: %d %s", status, body)
	}
	_ = f.st.SetSetting("gateway_key", "")
	status, body = f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(body, "网关未配置") {
		t.Fatalf("密钥被清空应 503 fail-closed: %d %s", status, body)
	}
	_ = f.st.SetSetting("gateway_key", "sk-test")
	// 鉴权通过后引擎需选中账号才能 200；Python 版只测 verify 函数，这里走全链路
	if _, err := f.st.AddAccount(model.ProviderZai, "a", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("正确密钥应 200: %d", status)
	}
}

func TestJSONPassthroughAndUsage(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	acc, _ := f.st.AddAccount(model.ProviderZai, "k", "sk-abc")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || raw != okUpstreamJSON {
		t.Fatalf("应字节级透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.UseCount != 1 {
		t.Fatalf("use_count 应为 1: %d", got.UseCount)
	}
	if got.TotalInputTokens != 11 || got.TotalOutputTokens != 22 {
		t.Fatalf("usage 统计不符: %+v", got)
	}
	if f.callCount() != 1 {
		t.Fatalf("上游应被调用 1 次: %d", f.callCount())
	}
	call := f.lastCall()
	if call.Path != "/fallback" {
		t.Fatalf("apiKey 应走回退端点: %s", call.Path)
	}
	if call.Header.Get("x-api-key") != "sk-abc" {
		t.Fatalf("x-api-key 不符: %v", call.Header)
	}
	// apiKey 账号不注入 system，content 字符串已桥接
	if _, ok := call.Body["system"]; ok {
		t.Fatal("apiKey 不应注入 system")
	}
	msgs := call.Body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("content 应桥接: %v", content)
	}
}

func TestStreamPassthroughAndUsage(t *testing.T) {
	f := newFixture(t)
	sse := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\ndata: [DONE]\n\n"
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		return 200, http.Header{"Content-Type": []string{"text/event-stream"}}, sse
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "k", "sk-abc")

	body := msgBody()
	body["stream"] = true
	status, raw := f.post(t, body, "sk-test")
	if status != 200 || raw != sse {
		t.Fatalf("SSE 应字节级透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.TotalInputTokens != 9 || got.TotalOutputTokens != 42 {
		t.Fatalf("流式 usage 统计不符: %+v", got)
	}
}

// 交付中断时「这笔用量还能不能计入」的判据是上游有没有交出终值，不是客户端有没有读完。
// zcode 编辑器收到 finish_reason 就立刻关流是常态，此时 message_delta 的终值已经到手，
// 整笔丢掉就会系统性少算（线上曾漏计一次 57,352 output token 的生成，见
// docs/analysis-flash-30min-stream-cut.md §5）；没有终值的半截流仍不能计。
func TestAbortedDeliveryStillCountsFinalUsage(t *testing.T) {
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "k", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}

	complete := NewUsageCollector(true)
	complete.FeedLine(`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`)
	complete.FeedLine(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`)

	// 客户端断开：请求 ctx 已结束 ⇒ 属于客户端侧，观测计数不得被污染。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	res := f.eng.finishDelivery(canceled, "t1", acc, complete, errors.New("write tcp: 客户端已断开"), nil)
	if !res.final.Delivered {
		t.Fatal("交付中断仍应记为 Delivered（200 已发出，不得再写响应）")
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.TotalInputTokens != 9 || got.TotalOutputTokens != 42 {
		t.Fatalf("上游终值已到齐，应计入: %+v", got)
	}

	// 反例：流在半途断掉（只有中途的 usage 更新、没有 stop_reason）时不得记半截数字。
	partial := NewUsageCollector(true)
	partial.FeedLine(`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`)
	partial.FeedLine(`data: {"type":"message_delta","usage":{"output_tokens":17}}`)
	if res := f.eng.finishDelivery(context.Background(), "t2", acc, partial, errors.New("上游串流在 message_stop 之前结束: unexpected EOF"), nil); !res.final.Delivered {
		t.Fatal("交付中断仍应记为 Delivered")
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.TotalOutputTokens != 42 {
		t.Fatalf("usage 不完整不应计入: %+v", got)
	}

	// 观测计数只认「上游掐断」：t1（客户端断开）不计，t2（上游截断）计一次。
	if got := f.st.Find(model.ProviderZai, acc.ID); got.StreamTruncateCount != 1 {
		t.Fatalf("StreamTruncateCount 应只统计上游侧掐断，实得 %d", got.StreamTruncateCount)
	}
}

func TestModelNotAllowed(t *testing.T) {
	f := newFixture(t)
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Fatal("白名单外模型不应触达上游")
		return 200, nil, ""
	}
	body := msgBody()
	body["model"] = "glm-5.2"
	status, raw := f.post(t, body, "sk-test")
	if status != 400 || !strings.Contains(raw, "model_not_allowed") {
		t.Fatalf("应 400 model_not_allowed: %d %s", status, raw)
	}
	if f.callCount() != 0 {
		t.Fatal("上游不应被调用")
	}
}

func Test401MarksInvalidAndSwitches(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(401, "")
	a1, _ := f.st.AddAccount(model.ProviderZai, "a1", "sk-1")
	a2, _ := f.st.AddAccount(model.ProviderZai, "a2", "sk-2")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(raw, "no_available_account") {
		t.Fatalf("全部失效应 503: %d %s", status, raw)
	}
	for _, id := range []string{a1.ID, a2.ID} {
		got := f.st.Find(model.ProviderZai, id)
		if got.Status != model.StatusInvalid {
			t.Fatalf("账号 %s 应 invalid: %s", id, got.Status)
		}
		if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindAuthFailed {
			t.Fatalf("账号 %s 应归类为 auth_failed: %v", id, got.LastErrorKind)
		}
	}
	if f.callCount() != 2 {
		t.Fatalf("应各试一次: %d", f.callCount())
	}
}

func Test402MarksModelExhausted(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(402, `{"error":{"message":"payment required"}}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(raw, "no_available_account") {
		t.Fatalf("唯一账号耗尽应 503: %d %s", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("仅单模型耗尽时账号应保持 active: %s", got.Status)
	}
	if len(got.ExhaustedModels) != 1 || got.ExhaustedModels[0] != "glm-5.3" {
		t.Fatalf("应只标记请求模型: %v", got.ExhaustedModels)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "額度已用完") {
		t.Fatalf("last_error 不符: %v", got.LastError)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindQuotaExhausted {
		t.Fatalf("402 应归类为 quota_exhausted: %v", got.LastErrorKind)
	}
}

func Test429QuotaFamilyExhaustsAndRateLimitCools(t *testing.T) {
	t.Run("1310 用量上限族→模型耗尽", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(429, `{"code":1310,"msg":"usage cap reached"}`)
		acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")
		f.post(t, msgBody(), "sk-test")
		got := f.st.Find(model.ProviderZai, acc.ID)
		if got.Status != model.StatusActive || len(got.ExhaustedModels) != 1 {
			t.Fatalf("上限族应标记模型耗尽而保留账号: %+v", got)
		}
		if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindQuotaExhausted {
			t.Fatalf("上限族应归类为 quota_exhausted: %v", got.LastErrorKind)
		}
	})
	t.Run("1302 瞬时限流→原地重试一次后递进冷却", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(429, `{"code":1302,"msg":"rate limited"}`)
		acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")
		f.post(t, msgBody(), "sk-test")
		got := f.st.Find(model.ProviderZai, acc.ID)
		if got.Status != model.StatusCooling || got.CoolingUntil == nil {
			t.Fatalf("瞬时限流应 cooling: %+v", got)
		}
		if len(got.ExhaustedModels) != 0 {
			t.Fatalf("瞬时限流不应标记耗尽: %v", got.ExhaustedModels)
		}
		// 首次 429 先原地重试一次，用尽才冷却换号
		if f.callCount() != 1+MaxRateLimitRetries {
			t.Fatalf("应原地重试 %d 次后放弃: callCount=%d", MaxRateLimitRetries, f.callCount())
		}
		if got.RateLimitStreak != 1 {
			t.Fatalf("连续限流计数应为 1: %d", got.RateLimitStreak)
		}
		if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRateLimited {
			t.Fatalf("瞬时限流应归类为 rate_limited: %v", got.LastErrorKind)
		}
		// 首次被限流走阶梯最低档 30s，而不是连接失败/503 的 CoolingSeconds(300s)
		want := float64(time.Now().Add(30*time.Second).UnixNano()) / 1e9
		if diff := *got.CoolingUntil - want; diff > 5 || diff < -5 {
			t.Fatalf("首次限流冷却应约 30s: cooling_until=%v", *got.CoolingUntil)
		}
	})
}

// 瞬时限流只值一次对冲：原地重试成功就不该把账号标成冷却。
func TestTransientRateLimitRetrySucceedsInPlace(t *testing.T) {
	f := newFixture(t)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return jsonResp(429, `{"code":1302,"msg":"rate limited"}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || !strings.Contains(raw, "msg_1") {
		t.Fatalf("限流后原地重试应成功: %d %s", status, raw)
	}
	if f.callCount() != 1+MaxRateLimitRetries {
		t.Fatalf("应恰好原地重试 %d 次: %d", MaxRateLimitRetries, f.callCount())
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("重试成功不得把账号标为冷却: %s", got.Status)
	}
	if got.RateLimitStreak != 0 {
		t.Fatalf("成功后连续限流计数应清零: %d", got.RateLimitStreak)
	}
}

// 递进冷却阶梯：连续被限流才逐级加重，任意一次成功调用后回到最低档。
func TestTransientRateLimitCoolingEscalates(t *testing.T) {
	st := openStore(t)
	acc, _ := st.AddAccount(model.ProviderZai, "a", "sk-1")
	now := time.Now()

	want := []int{30, 60, 120, config.CoolingSeconds}
	for i, secs := range want {
		got, streak := MarkRateLimited(st, model.ProviderZai, acc.ID, "上游限流 HTTP 429", now)
		if got != secs {
			t.Fatalf("第 %d 次限流冷却应为 %ds，实得 %ds", i+1, secs, got)
		}
		if streak != i+1 {
			t.Fatalf("连续计数应递增到 %d: %d", i+1, streak)
		}
	}
	// 超出阶梯长度后封顶，不再继续加重
	if got, _ := MarkRateLimited(st, model.ProviderZai, acc.ID, "上游限流 HTTP 429", now); got != config.CoolingSeconds {
		t.Fatalf("超出阶梯应封顶在 %ds: %d", config.CoolingSeconds, got)
	}
	// 成功调用后计数清零，下次从最低档重新起算（清零须对 live 对象做才落库）
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		ResetRateLimitStreak(a)
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := MarkRateLimited(st, model.ProviderZai, acc.ID, "上游限流 HTTP 429", now); got != 30 {
		t.Fatalf("成功调用后应回到最低档 30s: %d", got)
	}
}

// 三类重试各有独立预算：一次验证码重试不得吃掉 3010 的等待次数。
// 此前三者共用循环变量 captchaAttempt，验证码先重试一次后 3010 就只剩一次机会，
// 到第三次迭代时 2 < len(BusyRetryDelays) 为假，直接把 429 回传客户端。
func TestCaptchaRetryDoesNotConsumeBusyBudget(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil // 避免测试触网
	})
	f.cm.SetSolver(stubSolver{token: "token-1"})

	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		switch n {
		case 1:
			return jsonResp(400, `{"code":3007,"msg":"verify token invalid"}`)(n, nil)
		case 2, 3:
			return jsonResp(429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`)(n, nil)
		default:
			return jsonResp(200, okUpstreamJSON)(n, nil)
		}
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "j", "header.payload.sig")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || !strings.Contains(raw, "msg_1") {
		t.Fatalf("验证码重试后 3010 应仍保有完整预算: %d %s", status, raw)
	}
	// 1 次验证码被拒 + 2 次 3010 等待 + 1 次成功 = 4 次上游调用
	if f.callCount() != 4 {
		t.Fatalf("应为 4 次上游调用（各类预算互不挤占）: %d", f.callCount())
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("全链路不应改变账号状态: %s", got.Status)
	}
}

func Test3010RetriesThenSucceeds(t *testing.T) {
	f := newFixture(t)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n <= 2 {
			return jsonResp(429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || !strings.Contains(raw, "msg_1") {
		t.Fatalf("3010 两次后应成功: %d %s", status, raw)
	}
	if f.callCount() != 3 {
		t.Fatalf("应重试到第 3 次: %d", f.callCount())
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("3010 不应改变账号状态: %s", got.Status)
	}
}

func Test500PassthroughVerbatim(t *testing.T) {
	f := newFixture(t)
	upstreamBody := `{"error":{"message":"boom","quota_hint":"yes"}}`
	f.respond = jsonResp(500, upstreamBody)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 500 || raw != upstreamBody {
		t.Fatalf("未识别错误应原样透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || len(got.ExhaustedModels) != 0 {
		t.Fatalf("未识别错误不应推断状态: %+v", got)
	}
	if got.FailCount != 1 {
		t.Fatalf("失败计数应 +1: %d", got.FailCount)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindUpstreamError {
		t.Fatalf("未识别错误应归类为 upstream_error: %v", got.LastErrorKind)
	}
}

// 529 / 业务码 1305 是平台服务过载，官方明确它「与单一账户的调用行为无直接关系」。
// 它必须走自己的退避分支：不标状态、不冷却、不计 fail_count，重试用尽后原样透传。
//
// 防的是线上真实故障：分类链只认 HTTP 429，而 z.ai 在 Anthropic 兼容面用 529 承载
// 1305，于是整类过载落到「其余错误」分支被原样透传，网关一次都不重试，全部重试压力
// 推给上层中转（实测以 80ms 间隔连冲 26 次）。
func Test529OverloadRetriesThenPassesThrough(t *testing.T) {
	f := newFixture(t)
	f.eng.OverloadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	upstreamBody := `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`
	f.respond = jsonResp(529, upstreamBody)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 529 || raw != upstreamBody {
		t.Fatalf("平台过载用尽后应原样透传: %d %q", status, raw)
	}
	if n := f.callCount(); n != 1+len(f.eng.OverloadRetryDelays) {
		t.Fatalf("应按档位退避重试后放弃，上游调用 %d 次", n)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("平台过载不应改变账号状态: %s", got.Status)
	}
	if got.CoolingUntil != nil {
		t.Fatalf("平台过载不应冷却账号: %v", got.CoolingUntil)
	}
	if got.FailCount != 0 {
		t.Fatalf("平台过载不是账号的错，不应计入 fail_count: %d", got.FailCount)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindUpstreamOverload {
		t.Fatalf("平台过载应归类为 upstream_overload: %v", got.LastErrorKind)
	}
	if got.LastErrorAt == nil {
		t.Fatal("平台过载应记录发生时间")
	}
}

// 分类链的每个出口都必须写出对应的错误归类——这是前端「按错误类型筛选账号」的数据
// 基础，也是「同步与异步两条路径对同一账号标出相同状态」这条不变式的延伸。
// 405 + 风控文案：冷却整个账号（不是只灰一个模型）并按阶梯选档。
// 风控看身份维度（账号/设备指纹/出口 IP/请求头），与模型无关，所以处置是停账号。
func Test405RiskControlCoolsAndSwitches(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(405, `{"error":{"message":"Request has been blocked due to unusual activity."}}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	// 唯一账号被冷却后无号可用。
	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 503 {
		t.Fatalf("唯一账号被冷却后应 503: %d", status)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusCooling {
		t.Fatalf("风控 405 应冷却整个账号: %s", got.Status)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindRiskControl {
		t.Fatalf("应归类为 risk_control: %v", got.LastErrorKind)
	}
	if got.RiskControlStreak != 1 {
		t.Fatalf("连续命中计数应为 1: %d", got.RiskControlStreak)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "风控") {
		t.Fatalf("last_error 应说明是风控: %v", got.LastError)
	}
	// 首档应约 300s（默认阶梯第一档）。
	want := float64(time.Now().Add(300*time.Second).UnixNano()) / 1e9
	if got.CoolingUntil == nil || *got.CoolingUntil-want > 5 || *got.CoolingUntil-want < -5 {
		t.Fatalf("首档应约 300s: %v", got.CoolingUntil)
	}
}

// 非风控 405 不得冷却：那多半是 JWT 缺顶层 system 注入时上游回的错（body.go 有说明），
// 属我方请求构造缺陷，每个账号都会一样地失败——冷却账号等于把代码 bug 变成集体惩罚。
func Test405WithoutRiskBodyDoesNotCool(t *testing.T) {
	f := newFixture(t)
	upstreamBody := `{"error":{"message":"method not allowed"}}`
	f.respond = jsonResp(405, upstreamBody)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 405 || raw != upstreamBody {
		t.Fatalf("非风控 405 应原样透传: %d %q", status, raw)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || got.CoolingUntil != nil {
		t.Fatalf("非风控 405 不该冷却账号: status=%s until=%v", got.Status, got.CoolingUntil)
	}
	if got.RiskControlStreak != 0 {
		t.Fatalf("非风控 405 不该推进风控阶梯: %d", got.RiskControlStreak)
	}
	if got.FailCount != 1 {
		t.Fatalf("失败计数应 +1: %d", got.FailCount)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindUpstreamError {
		t.Fatalf("非风控 405 应归类为 upstream_error: %v", got.LastErrorKind)
	}
}

func TestErrorKindRecordedPerBranch(t *testing.T) {
	cases := []struct {
		name    string
		respond responder
		stream  bool
		want    string
	}{
		{"401 鉴权失败", jsonResp(401, ""), false, model.ErrorKindAuthFailed},
		{"403 鉴权失败", jsonResp(403, `{"error":{"message":"forbidden"}}`), false, model.ErrorKindAuthFailed},
		{"402 该模型额度用完", jsonResp(402, `{"error":{"message":"payment required"}}`), false, model.ErrorKindQuotaExhausted},
		{"429+1302 瞬时限流", jsonResp(429, `{"code":1302,"msg":"rate limited"}`), false, model.ErrorKindRateLimited},
		{"429+1310 用量上限", jsonResp(429, `{"code":1310,"msg":"usage cap reached"}`), false, model.ErrorKindQuotaExhausted},
		{"429+3010 并发准入", jsonResp(429, `{"code":3010,"msg":"model admission concurrency limit exceeded"}`), false, model.ErrorKindModelBusy},
		{"529+1305 平台过载", jsonResp(529, `{"code":1305,"msg":"overloaded"}`), false, model.ErrorKindUpstreamOverload},
		{"429+1305 平台过载（官方表形态）", jsonResp(429, `{"code":1305,"msg":"overloaded"}`), false, model.ErrorKindUpstreamOverload},
		{"503 上游不可用", jsonResp(503, `{"error":{"message":"unavailable"}}`), false, model.ErrorKindUpstreamUnavailable},
		{"500 未识别错误", jsonResp(500, `{"error":{"message":"boom"}}`), false, model.ErrorKindUpstreamError},
		{"200+1005 每日额度", jsonResp(200, `{"code":1005,"msg":"exceed quota limit"}`), false, model.ErrorKindQuotaExhausted},
		{"200+未知业务码", jsonResp(200, `{"code":9999,"msg":"weird"}`), false, model.ErrorKindUpstreamError},
		{"200 非 SSE JSON", jsonResp(200, `{"unexpected":"json"}`), true, model.ErrorKindInvalidResponse},
		{"405+风控文案", jsonResp(405, `{"error":{"message":"Request has been blocked due to unusual activity."}}`), false, model.ErrorKindRiskControl},
		{"405 无风控文案", jsonResp(405, `{"error":{"message":"method not allowed"}}`), false, model.ErrorKindUpstreamError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			// 缩短平台过载档位，避免每个用例真等 1s+3s。
			f.eng.OverloadRetryDelays = []time.Duration{time.Millisecond}
			f.respond = tc.respond
			acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

			body := msgBody()
			if tc.stream {
				body["stream"] = true
			}
			f.post(t, body, "sk-test")

			got := f.st.Find(model.ProviderZai, acc.ID)
			if got.LastErrorKind == nil {
				t.Fatalf("应写出错误归类（last_error=%v）", got.LastError)
			}
			if *got.LastErrorKind != tc.want {
				t.Fatalf("错误归类不符: got %q want %q", *got.LastErrorKind, tc.want)
			}
			if got.LastErrorAt == nil {
				t.Fatal("应写出错误发生时间")
			}
			if !model.ErrorKindValid(*got.LastErrorKind) {
				t.Fatalf("归类必须是已登记的枚举值: %q", *got.LastErrorKind)
			}
		})
	}
}

// 过载是瞬时拥塞：退避一次后恢复就必须成功，而不是把整个档位等完。
func Test529RecoversOnRetry(t *testing.T) {
	f := newFixture(t)
	f.eng.OverloadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return jsonResp(529, `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 || !strings.Contains(raw, "msg_1") {
		t.Fatalf("过载退避一次后应成功: %d %s", status, raw)
	}
	if f.callCount() != 2 {
		t.Fatalf("应恰好重试一次: %d", f.callCount())
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusActive {
		t.Fatalf("恢复后账号应仍为正常: %s", got.Status)
	}
}

// 1305 若以 HTTP 429 返回（官方错误码表就是这么标的），也必须走平台过载分支，
// 不能被「瞬时限流」分支接走——那条路会给账号打 30→60→120→300s 的递进冷却，
// 而平台过载冷却账号既不解决问题、又白白缩小可用池。
func Test429With1305IsOverloadNotRateLimit(t *testing.T) {
	f := newFixture(t)
	f.eng.OverloadRetryDelays = []time.Duration{time.Millisecond}
	f.respond = jsonResp(429, `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 429 {
		t.Fatalf("1305 应走平台过载分支并原样透传: %d", status)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || got.CoolingUntil != nil {
		t.Fatalf("1305 不应冷却账号: status=%s cooling=%v", got.Status, got.CoolingUntil)
	}
	if got.RateLimitStreak != 0 {
		t.Fatalf("1305 不应累加限流阶梯计数: %d", got.RateLimitStreak)
	}
}

func TestBusinessCode1005ExhaustsDailyQuota(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, `{"code":1005,"msg":"exceed quota limit"}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 503 {
		t.Fatalf("业务码 1005 应换号并 503: %d", status)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if len(got.ExhaustedModels) != 1 || got.ExhaustedModels[0] != "glm-5.3" {
		t.Fatalf("1005 应标记该模型耗尽: %v", got.ExhaustedModels)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "每日額度已用完") {
		t.Fatalf("last_error 不符: %v", got.LastError)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindQuotaExhausted {
		t.Fatalf("1005 应归类为 quota_exhausted: %v", got.LastErrorKind)
	}
}

// 错误/异常响应的读取限长 64KB：限内照常分类，限外不再参与分类。上游或账号级
// 代理异常时可能回一个任意大的 body，不限长会直接吃光内存（正常流式路径边读边发，
// 不受影响）。
func TestOversizedErrorBodyIsBounded(t *testing.T) {
	t.Run("限内仍能分类", func(t *testing.T) {
		f := newFixture(t)
		f.respond = jsonResp(200, `{"code":1005,"msg":"exceed quota limit","pad":"`+strings.Repeat(" ", 8<<10)+`"}`)
		acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

		if status, _ := f.post(t, msgBody(), "sk-test"); status != 503 {
			t.Fatalf("业务码在限内应照常换号并 503: %d", status)
		}
		if got := f.st.Find(model.ProviderZai, acc.ID); len(got.ExhaustedModels) != 1 {
			t.Fatalf("应标记该模型耗尽: %v", got.ExhaustedModels)
		}
	})

	t.Run("限外不再参与分类", func(t *testing.T) {
		f := newFixture(t)
		// 业务码排在 64KB 之后：读取被截断后应看不到它，而不是把整个 body 读进内存
		f.respond = jsonResp(200, `{"pad":"`+strings.Repeat(" ", 100<<10)+`","code":1005}`)
		acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

		if status, _ := f.post(t, msgBody(), "sk-test"); status != 200 {
			t.Fatalf("限外的业务码不参与分类，应按无业务码处理: %d", status)
		}
		if got := f.st.Find(model.ProviderZai, acc.ID); len(got.ExhaustedModels) != 0 {
			t.Fatalf("限外的业务码不应标记耗尽: %v", got.ExhaustedModels)
		}
	})
}

// 额度信号不得把更强的账号状态刷回 active：invalid 需人工介入、cooling 在冷却窗口内、
// disabled 是管理员主动停用——三者都与「额度用没用完」无关。并发下若不设防，一条 402
// 就能把刚被判失效的账号放回轮询。
func TestQuotaSignalKeepsStrongStatus(t *testing.T) {
	for _, strong := range []string{model.StatusInvalid, model.StatusCooling, model.StatusDisabled} {
		t.Run(strong, func(t *testing.T) {
			st := openStore(t)
			acc, err := st.AddAccount(model.ProviderZai, "a", "sk-1")
			if err != nil {
				t.Fatal(err)
			}
			until := float64(time.Now().Add(time.Hour).UnixNano()) / 1e9
			if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
				a.Status = strong
				a.CoolingUntil = &until
			}); err != nil {
				t.Fatal(err)
			}

			// 账号没有额度快照 → anyState=false，正是原先会落到 else 分支
			// 把状态写成 active 的那条路径。
			MarkModelExhausted(st, model.ProviderZai, acc.ID, "glm-5.3", "GLM-5.3 額度已用完", time.Now())

			got := st.Find(model.ProviderZai, acc.ID)
			if got.Status != strong {
				t.Fatalf("状态被额度信号改写: %q → %q", strong, got.Status)
			}
			if got.CoolingUntil == nil {
				t.Fatal("冷却截止时间不应被清空")
			}
			// 额度事实仍要照常记录：模型耗尽清单与账号状态是两件事。
			if len(got.ExhaustedModels) != 1 {
				t.Fatalf("模型耗尽清单应照常更新: %v", got.ExhaustedModels)
			}
		})
	}
}

// 客户端断连（context 取消）与账号健康无关：既不该冷却账号，也不该换号白烧下一个。
func TestClientCancelDoesNotCoolAccount(t *testing.T) {
	f := newFixture(t)
	called := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// 上游收到请求后挂住，直到测试放行——保证取消发生在 Do 进行期间。
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		once.Do(func() { close(called) })
		<-release
		return http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}
	// Cleanup 逆序执行：本行晚于 newFixture 内的 up.Close 注册，因此先跑——
	// 先解开上游阻塞，再关服务，避免 Close 等待未完成的请求。
	t.Cleanup(func() { close(release) })

	acc, err := f.st.AddAccount(model.ProviderZai, "a", "sk-1")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	data, err := json.Marshal(msgBody())
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			return // 取消后客户端拿到的正是这个错误，也就是被测场景
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	<-called // 上游已收到请求 → 网关已经进入 Do
	cancel() // 模拟客户端断开连接
	<-done

	// 给网关留出走完取消分支的时间；期间只要出现冷却即判定回归。
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := f.st.Find(model.ProviderZai, acc.ID); got.Status == model.StatusCooling {
			t.Fatalf("客户端断连把账号冷却了: %v", got.LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.Status != model.StatusActive || got.CoolingUntil != nil {
		t.Fatalf("取消不应改变账号状态: status=%s cooling=%v", got.Status, got.CoolingUntil)
	}
}

// 拨号超时（错误链携带 DeadlineExceeded，但请求 ctx 未结束）是线路故障：
// 必须冷却该账号并换号重试。修复前它被「错误值像取消」的旧判据误判成客户端
// 取消——不冷却、不换号，客户端还会收到误导性的 502「请求已取消」。
func TestDialTimeoutCoolsAccountAndSwitches(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)

	// 模拟 net.Dialer.Timeout：Go 把 Timeout 实现为 context deadline，超时错误
	// 链携带 context.DeadlineExceeded（本机实测；线上代理超时即此形态）。
	// 首次拨号失败，之后放行走真实拨号。
	var firstDial atomic.Bool
	firstDial.Store(true)
	f.eng.Client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if firstDial.Swap(false) {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}

	bad, err := f.st.AddAccount(model.ProviderZai, "bad", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "good", "sk-2"); err != nil {
		t.Fatal(err)
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("拨号超时应换号后成功: %d %s", status, raw)
	}
	got := f.st.Find(model.ProviderZai, bad.ID)
	if got.Status != model.StatusCooling {
		t.Fatalf("拨号超时应冷却账号，实际 status=%s err=%v", got.Status, got.LastError)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "连接失败") {
		t.Fatalf("冷却原因应记为连接失败: %v", got.LastError)
	}
}

func TestJWTInjectsSystemAndCaptcha(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil // 避免测试触网
	})
	f.cm.SetSolver(stubSolver{token: "token-1"})

	f.respond = jsonResp(200, okUpstreamJSON)
	acc, _ := f.st.AddAccount(model.ProviderZai, "j", "header.payload.sig")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("JWT 应成功: %d %s", status, raw)
	}
	call := f.lastCall()
	if call.Path != "/zai" {
		t.Fatalf("JWT 应走主端点: %s", call.Path)
	}
	if call.Header.Get("Authorization") != "Bearer header.payload.sig" {
		t.Fatalf("Authorization 不符: %v", call.Header)
	}
	if call.Header.Get("X-Aliyun-Captcha-Verify-Param") != "token-1" {
		t.Fatalf("验证码头不符: %v", call.Header)
	}
	sys, ok := call.Body["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("JWT 必须注入 system: %v", call.Body["system"])
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got.UseCount != 1 {
		t.Fatalf("use_count 应为 1")
	}
}

func TestJWTWithoutSolverReturnsCaptchaRequired(t *testing.T) {
	f := newFixture(t)
	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil // 避免测试触网
	})
	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Fatal("无求解器时不应触达上游")
		return 200, nil, ""
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "j", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != 503 || !strings.Contains(raw, "captcha_required") {
		t.Fatalf("应 503 captcha_required: %d %s", status, raw)
	}
	if f.callCount() != 0 {
		t.Fatal("上游不应被调用")
	}
}

func TestModelsEndpoint(t *testing.T) {
	f := newFixture(t)
	// Python 版 /v1/models 带 Depends(verify_gateway_key)，同样需要鉴权
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("应 200: %d %s", resp.StatusCode, raw)
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" || len(payload.Data) != 2 {
		t.Fatalf("模型清单不符: %s", raw)
	}
	if payload.Data[0].Type != "model" || payload.Data[0].DisplayName != payload.Data[0].ID {
		t.Fatalf("模型条目不符: %+v", payload.Data[0])
	}
}

type stubSolver struct{ token string }

func (s stubSolver) Solve(_ context.Context, _ captcha.Config) (string, error) {
	return s.token, nil
}

func (s stubSolver) Close() error { return nil }
