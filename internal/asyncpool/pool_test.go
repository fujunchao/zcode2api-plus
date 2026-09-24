// Async 空闲池测试：移植 Python 版 tests/test_async_pool.py 全部用例，
// 覆盖验证码刷新转发、白名单、ticket 生命周期、网络错误与流中断语义。
package asyncpool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// fakeSolver 依次返回预置令牌；tokens 耗尽时回退默认令牌（不关注
// 验证码内容的用例可直接使用），err 非空时始终报错。
type fakeSolver struct {
	mu     sync.Mutex
	tokens []string
	err    error
	calls  int
}

func (s *fakeSolver) Solve(ctx context.Context, cfg captcha.Config) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	if len(s.tokens) == 0 {
		return "tok-default", nil
	}
	tok := s.tokens[0]
	s.tokens = s.tokens[1:]
	return tok, nil
}

func (s *fakeSolver) Close() error { return nil }

func (s *fakeSolver) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// upstreamSpec 上游第 n 次调用的应答脚本。
type upstreamSpec struct {
	status      int
	contentType string
	body        string   // 非 SSE 应答体
	lines       []string // SSE 应答逐行写出（自动补 \n\n）
	abort       bool     // 写完后强制断连（模拟流中断）
	reset       bool     // 接受连线后立即断开（模拟连线层网络错误）
}

// scriptedUpstream 脚本化上游服务器。
type scriptedUpstream struct {
	mu    sync.Mutex
	specs []upstreamSpec
	calls []http.Header
}

func (s *scriptedUpstream) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.calls)
		s.calls = append(s.calls, r.Header.Clone())
		spec := upstreamSpec{status: http.StatusBadGateway}
		if n < len(s.specs) {
			spec = s.specs[n]
		}
		s.mu.Unlock()

		if spec.reset {
			// 未写任何响应头即断开：客户端 client.Do 直接返回网络错误
			panic(http.ErrAbortHandler)
		}
		if spec.lines != nil {
			w.Header().Set("Content-Type", orDefault(spec.contentType, "text/event-stream"))
			w.WriteHeader(spec.status)
			flusher := w.(http.Flusher)
			for _, line := range spec.lines {
				_, _ = fmt.Fprintf(w, "%s\n\n", line)
				flusher.Flush()
			}
			if spec.abort {
				panic(http.ErrAbortHandler) // 强制断连：客户端读流出错
			}
			return
		}
		w.Header().Set("Content-Type", orDefault(spec.contentType, "application/json"))
		w.WriteHeader(spec.status)
		_, _ = w.Write([]byte(spec.body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *scriptedUpstream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *scriptedUpstream) header(n int) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[n]
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// newTestPool 打开隔离存储与空闲池；返回池、存储、验证码管理器与求解器桩。
func newTestPool(t *testing.T) (*Pool, *store.Store, *captcha.Manager, *fakeSolver) {
	t.Helper()
	oldDB, oldData, oldZai, oldFB, oldBrowser, oldMaxRetries := config.DBPath, config.DataDir,
		config.UpstreamZai, config.UpstreamZaiFallback, config.CaptchaBrowserEnabled, config.AsyncMaxRetries
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir() // DeviceMid 持久化路径隔离
	config.CaptchaBrowserEnabled = true
	config.AsyncMaxRetries = 0 // 避免退避等待
	t.Cleanup(func() {
		config.DBPath, config.DataDir, config.UpstreamZai, config.UpstreamZaiFallback = oldDB, oldData, oldZai, oldFB
		config.CaptchaBrowserEnabled, config.AsyncMaxRetries = oldBrowser, oldMaxRetries
	})

	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	solver := &fakeSolver{}
	cm := captcha.NewManager()
	cm.SetSolver(solver)
	// 固定配置源：避免 FetchConfig 触网（真实端点经代理约 2s，离线则回退
	// DefaultConfig 的 cn region，会让 region 断言随环境漂移）
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, Prefix: "no8xfe", Region: "sgp", SceneID: "11xygtvd"}, nil
	})
	p := NewPool(st, auth.New(st), cm)
	p.RateLimitRetryDelay = 0 // 与 config.AsyncMaxRetries=0 同理：避免退避等待
	return p, st, cm, solver
}

// addJWTAccount 添加一个可被 Select 选中的 jwt 账号。
func addJWTAccount(t *testing.T, st *store.Store, name string) *model.Account {
	t.Helper()
	acc, err := st.AddAccount(model.ProviderZai, name, "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// addJWTAccountN 添加第 n 个 jwt 账号。
//
// ⚠️ 凭据必须各不相同：AddAccount 按「同一账号」判重（凭据相同即命中既有记录），
// 用同一个 secret 加三次只会得到一个账号——池里只有 1 个号，多账号用例全部失真。
func addJWTAccountN(t *testing.T, st *store.Store, name string, n int) *model.Account {
	t.Helper()
	acc, err := st.AddAccount(model.ProviderZai, name, fmt.Sprintf("header.payload.sig%d", n))
	if err != nil {
		t.Fatal(err)
	}
	return acc
}

// insertTicket 手工建票（不启动后台任务），供同步驱动 processTicket。
func insertTicket(p *Pool, id string, body map[string]any) *ticket {
	tk := &ticket{
		status:    "pending",
		body:      body,
		queue:     make(chan ticketEvent, 256),
		createdAt: time.Now(),
		shortID:   newShortID(), // 与 newTicket 同形：诊断行要用它当 reqID
	}
	p.mu.Lock()
	p.tickets[id] = tk
	p.mu.Unlock()
	return tk
}

// drainEvents 取走队列中的全部事件。
func drainEvents(tk *ticket) []ticketEvent {
	var out []ticketEvent
	for {
		select {
		case ev := <-tk.queue:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestCode3007RefreshesTokenAndForwardsSSE(t *testing.T) {
	// 3007 后刷新验证码令牌重试，成功流转发为 chunk + done。
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusBadRequest, body: `{"code":3007,"msg":"captcha verify failed"}`},
		{status: http.StatusOK, lines: []string{`data: {"id":"ok"}`, `data: [DONE]`}},
	}}
	config.UpstreamZai = up.start(t).URL
	solver.tokens = []string{"first-token", "fresh-token"}

	tk := insertTicket(p, "ticket-captcha", map[string]any{
		"model": "GLM-5.3", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	})
	p.processTicket(context.Background(), "ticket-captcha")

	events := drainEvents(tk)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "ready,chunk,done" {
		t.Fatalf("事件序列不符: %v", events)
	}
	if chunk, ok := events[1].Data.(map[string]any); !ok || chunk["id"] != "ok" {
		t.Fatalf("chunk 内容不符: %v", events[1].Data)
	}
	if up.callCount() != 2 {
		t.Fatalf("应两次请求上游: %d", up.callCount())
	}
	if got := up.header(0).Get("X-Aliyun-Captcha-Verify-Param"); got != "first-token" {
		t.Fatalf("首次应携带 first-token: %q", got)
	}
	if got := up.header(0).Get("X-Aliyun-Captcha-Verify-Region"); got != "sgp" {
		t.Fatalf("region 头不符: %q", got)
	}
	if got := up.header(1).Get("X-Aliyun-Captcha-Verify-Param"); got != "fresh-token" {
		t.Fatalf("重试应携带 fresh-token: %q", got)
	}
	if solver.callCount() != 2 {
		t.Fatalf("验证码应求解两次: %d", solver.callCount())
	}
}

func TestMissingCaptchaTokenIsNotSentBare(t *testing.T) {
	// 求解失败时不得裸打上游；耗尽后返回 captcha_required。
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc")

	up := &scriptedUpstream{}
	config.UpstreamZai = up.start(t).URL
	solver.err = errors.New("browser down")

	tk := insertTicket(p, "ticket-no-token", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-no-token")

	events := drainEvents(tk)
	if up.callCount() != 0 {
		t.Fatalf("求解失败不得请求上游: %d", up.callCount())
	}
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("末事件应为 error: %v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "captcha_required" {
		t.Fatalf("错误类型应为 captcha_required: %v", last.Data)
	}
	if !strings.Contains(fmt.Sprint(errObj["message"]), "browser down") {
		t.Fatalf("错误信息应含求解失败原因: %v", errObj)
	}
	if solver.callCount() != gateway.MaxCaptchaRetries {
		t.Fatalf("应求解 %d 次: %d", gateway.MaxCaptchaRetries, solver.callCount())
	}
}

func newTestMux(t *testing.T, p *Pool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	p.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAsyncDisabledReturns503(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = false
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	// 鉴权先于功能开关校验（对齐 FastAPI Depends），需携带有效 key 才能触达 503
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages",
		strings.NewReader(`{"model":"GLM-5.3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("关闭时应 503: %d", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj := payload["error"].(map[string]any)
	if errObj["type"] != "feature_disabled" {
		t.Fatalf("错误类型应为 feature_disabled: %v", payload)
	}
}

func TestMessagesRejectsModelOutsideWhitelist(t *testing.T) {
	// 入口端點必須在建票前擋下白名單外的模型，不得轉發上游。
	p, st, _, _ := newTestPool(t)
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	body := `{"model":"GLM-5-Turbo","max_tokens":8,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("白名单外应 400: %d", resp.StatusCode)
	}
	var payload map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	errObj := payload["error"].(map[string]any)
	if errObj["type"] != "model_not_allowed" {
		t.Fatalf("错误类型应为 model_not_allowed: %v", payload)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("不应建票: %d", remaining)
	}
}

func TestMessagesAcceptsWhitelistedModel(t *testing.T) {
	// 白名单内模型正常建票并返回 SSE 流；后台任务失败时以 error 事件收尾并清票。
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc") // 无账号会先报 no_account，需提供可用账号
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")
	solver.err = errors.New("browser down") // 后台任务走 captcha_required 失败路径

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	body := `{"model":"glm-5.3-flash","max_tokens":8,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("白名单内应 200: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("应为 SSE 响应: %s", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "event: ticket\ndata: ") {
		t.Fatalf("首事件应为 ticket: %q", text)
	}
	if !strings.Contains(text, `"status":"pending"`) {
		t.Fatalf("ticket 事件应含 pending: %s", text)
	}
	if !strings.Contains(text, "event: error") || !strings.Contains(text, "captcha_required") {
		t.Fatalf("流应以 captcha_required 错误收尾: %s", text)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("流结束后应释放票务: %d", remaining)
	}
}

func TestSSEDoneReleasesTicket(t *testing.T) {
	// 正常 done 事件後同樣要釋放 ticket，不得殘留。
	p, _, _, _ := newTestPool(t)
	tk := insertTicket(p, "ticket-done", map[string]any{"messages": []any{}})
	tk.queue <- ticketEvent{Type: "done"}

	var out []string
	p.streamTicket(context.Background(), func(s string) error {
		out = append(out, s)
		return nil
	}, "ticket-done")

	if last := out[len(out)-1]; last != "event: done\ndata: {}\n\n" {
		t.Fatalf("末事件应为 done: %v", out)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("done 后应释放票务: %d", remaining)
	}
}

func TestClientDisconnectReleasesTicket(t *testing.T) {
	// 客戶端中途斷開（ctx 取消）時應清票並中止後台任務。
	p, _, _, _ := newTestPool(t)
	insertTicket(p, "ticket-drop", map[string]any{"messages": []any{}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.streamTicket(ctx, func(string) error { return nil }, "ticket-drop")
	}()
	cancel()
	<-done

	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("断开后应释放票务: %d", remaining)
	}
}

// 票务逾时必须显式投递终止事件，否则客户端只看到连接关闭，
// 无法区分「已完成」与「被超时截断」。
func TestTicketTimeoutEmitsErrorEvent(t *testing.T) {
	p, _, _, _ := newTestPool(t)
	old := config.AsyncTicketTimeout
	config.AsyncTicketTimeout = 30 // 下限；用 createdAt 回拨触发立即逾时
	t.Cleanup(func() { config.AsyncTicketTimeout = old })

	tk := insertTicket(p, "ticket-timeout", map[string]any{"messages": []any{}})
	tk.createdAt = time.Now().Add(-time.Duration(config.AsyncTicketTimeout+1) * time.Second)

	var out []string
	p.streamTicket(context.Background(), func(s string) error {
		out = append(out, s)
		return nil
	}, "ticket-timeout")

	if len(out) == 0 {
		t.Fatal("逾时应至少投递 ticket 事件与终止事件")
	}
	last := out[len(out)-1]
	if !strings.Contains(last, "event: error") || !strings.Contains(last, "ticket_timeout") {
		t.Fatalf("逾时应投递 ticket_timeout 错误事件: %q", last)
	}
	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("逾时后应释放票务: %d", remaining)
	}
}

func TestReleaseTicketIgnoresUnknownIDAndFinishedTask(t *testing.T) {
	// 未知 id 與已結束任務都應安全跳過。
	p, _, _, _ := newTestPool(t)

	p.releaseTicket("no-such-ticket") // 不應拋錯

	_, cancel := context.WithCancel(context.Background())
	cancel() // 任务已结束：cancel 无操作
	insertTicket(p, "ticket-gone", map[string]any{}).cancel = cancel
	p.releaseTicket("ticket-gone")

	p.mu.Lock()
	remaining := len(p.tickets)
	p.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("应移除票务: %d", remaining)
	}
}

func TestSweepRemovesExpiredOrphansAndKeepsFresh(t *testing.T) {
	// 超過生命周期 + 寬限的孤兒 ticket 應被清掃，新建的不受影響。
	p, _, _, _ := newTestPool(t)

	stale := &ticket{
		status: "pending",
		queue:  make(chan ticketEvent, 1),
		createdAt: time.Now().Add(-time.Duration(config.AsyncTicketTimeout)*time.Second -
			120*time.Second),
	}
	p.mu.Lock()
	p.tickets["ticket-stale"] = stale
	p.mu.Unlock()
	insertTicket(p, "ticket-fresh", map[string]any{})

	p.sweepExpiredTickets()

	p.mu.Lock()
	_, hasStale := p.tickets["ticket-stale"]
	_, hasFresh := p.tickets["ticket-fresh"]
	p.mu.Unlock()
	if hasStale {
		t.Fatal("过期孤儿应被清扫")
	}
	if !hasFresh {
		t.Fatal("新建票务应保留")
	}
}

func TestNewTicketSweepsOrphans(t *testing.T) {
	// _new_ticket 建票前先清扫（对齐 Python 版顺序）。
	p, _, _, _ := newTestPool(t)

	stale := &ticket{
		status: "pending",
		queue:  make(chan ticketEvent, 1),
		createdAt: time.Now().Add(-time.Duration(config.AsyncTicketTimeout)*time.Second -
			120*time.Second),
	}
	p.mu.Lock()
	p.tickets["ticket-stale"] = stale
	p.mu.Unlock()

	p.newTicket(map[string]any{"messages": []any{}}) // 无账号：后台任务只会投递 no_account

	p.mu.Lock()
	_, hasStale := p.tickets["ticket-stale"]
	p.mu.Unlock()
	if hasStale {
		t.Fatal("建票应先清扫孤儿")
	}
}

func TestNetworkErrorDeliversMaxRetries(t *testing.T) {
	// 上游連線異常必須走到報錯路徑，不得因日誌呼叫炸掉 ticket。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "async-acc")

	// 上游接受连线后立即断开：client.Do 返回网络错误
	up := &scriptedUpstream{specs: []upstreamSpec{{reset: true}}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-neterr", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-neterr")

	events := drainEvents(tk)
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("末事件应为 error: %v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "max_retries" {
		t.Fatalf("错误类型应为 max_retries: %v", last.Data)
	}
	if up.callCount() != 1 {
		t.Fatalf("ASYNC_MAX_RETRIES=0 应只尝试一次: %d", up.callCount())
	}
}

func TestMidStreamFailureTerminatesWithoutRetry(t *testing.T) {
	// 已發出 chunk 後流中斷：終止票務並回 error 事件，不得重發。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "midstream")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{`data: {"id":"msg1"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-midstream", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-midstream")

	events := drainEvents(tk)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "ready,chunk,error" {
		t.Fatalf("事件序列不符: %v", events)
	}
	if chunk, ok := events[1].Data.(map[string]any); !ok || chunk["id"] != "msg1" {
		t.Fatalf("chunk 内容不符: %v", events[1].Data)
	}
	errObj, _ := events[2].Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_stream_interrupted" {
		t.Fatalf("错误类型应为 upstream_stream_interrupted: %v", events[2].Data)
	}
	if up.callCount() != 1 {
		t.Fatalf("流中断后不得换号重发: %d", up.callCount())
	}
}

// 上游已交出终值（message_delta 带 stop_reason + usage）之后才断流：用量是准的，必须计入。
// 计入判据是「上游有没有交出终值」而不是「客户端有没有读完」，与同步路径同口径
// （见 gateway.UsageCollector.UsageComplete）。
func TestMidStreamFailureAfterFinalUsageStillCounts(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "finalusage")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":9}}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-finalusage", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-finalusage")
	_ = drainEvents(tk)

	if got := st.Find(model.ProviderZai, acc.ID); got.TotalInputTokens != 9 || got.TotalOutputTokens != 42 {
		t.Fatalf("上游终值已到齐，中断后仍应计入: %+v", got)
	}
	if up.callCount() != 1 {
		t.Fatalf("已发出 chunk 后不得换号重发: %d", up.callCount())
	}
}

func TestFailureBeforeFirstChunkStillRetries(t *testing.T) {
	// 一個 chunk 都沒發出時中斷：仍按網絡錯誤走換號重試路徑。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "prestream")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-prestream", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-prestream")

	events := drainEvents(tk)
	last := events[len(events)-1]
	if last.Type != "error" {
		t.Fatalf("末事件应为 error: %v", events)
	}
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "max_retries" {
		t.Fatalf("错误类型应为 max_retries: %v", last.Data)
	}
	if up.callCount() != 1 {
		t.Fatalf("ASYNC_MAX_RETRIES=0 应只尝试一次: %d", up.callCount())
	}
}

func TestUpstreamErrorDeliveredAsEvent(t *testing.T) {
	// 非 200 且非验证码/限流错误：错误体原样投递并终止，不得换号重试。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "upstream-err")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusInternalServerError, body: `{"code":9999,"msg":"boom"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-upstream", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-upstream")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_error" {
		t.Fatalf("错误类型应为 upstream_error: %v", events)
	}
	if !strings.Contains(fmt.Sprint(errObj["message"]), "boom") {
		t.Fatalf("错误体应原样透传: %v", errObj)
	}
	if up.callCount() != 1 {
		t.Fatalf("已投递错误后不得重试: %d", up.callCount())
	}
}

func TestRateLimitMarksCoolingAndRetries(t *testing.T) {
	// 429：先对同一账号原地重试一次，用尽才递进冷却并换号；无更多账号时报 max_retries。
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "cooling-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":1302,"msg":"rate limited"}`},
		{status: http.StatusTooManyRequests, body: `{"code":1302,"msg":"rate limited"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-429", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-429")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "max_retries" {
		t.Fatalf("重试耗尽应报 max_retries: %v", events)
	}
	// 首次 + 一次原地重试 = 2 次上游调用，之后才标冷却
	if up.callCount() != 1+gateway.MaxRateLimitRetries {
		t.Fatalf("应原地重试 %d 次: %d", gateway.MaxRateLimitRetries, up.callCount())
	}
	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusCooling {
		t.Fatalf("429 应进入冷却: %s", acc.Status)
	}
	if acc.LastError == nil || !strings.Contains(*acc.LastError, "HTTP 429") {
		t.Fatalf("last_error 应记录 429: %v", acc.LastError)
	}
	if acc.RateLimitStreak != 1 {
		t.Fatalf("连续限流计数应为 1: %d", acc.RateLimitStreak)
	}
	wantErrorKind(t, acc, model.ErrorKindRateLimited)
}

// 瞬时限流原地重试成功时不得把账号标成冷却（与 engine 的 sync 路径同语义）。
func TestRateLimitRetrySucceedsInPlace(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "retry-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":1302,"msg":"rate limited"}`},
		{status: http.StatusOK, lines: []string{`data: {"id":"ok"}`, `data: [DONE]`}},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-retry", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-retry")

	events := drainEvents(tk)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "ready,chunk,done" {
		t.Fatalf("限流后原地重试应成功: %v", events)
	}
	if up.callCount() != 1+gateway.MaxRateLimitRetries {
		t.Fatalf("应恰好原地重试 %d 次: %d", gateway.MaxRateLimitRetries, up.callCount())
	}
	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusActive {
		t.Fatalf("重试成功不得把账号标为冷却: %s", acc.Status)
	}
	if acc.RateLimitStreak != 0 {
		t.Fatalf("成功后连续限流计数应清零: %d", acc.RateLimitStreak)
	}
}

// 529 / 业务码 1305 是平台服务过载（官方明确它与单一账户的调用行为无关）。
// 异步链必须与 sync 路径同语义：不标状态、不冷却、不换号、不计 fail_count，
// 只按 gateway.OverloadRetryDelays 原地退避重试；用尽后原样投递上游错误体并终止本票。
func Test529OverloadInAsyncPool(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "overload-acc")
	p.OverloadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}

	overloadBody := `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`
	// 每次调用都要给一档脚本：超出 specs 长度会拿到默认 502，混淆断言。
	specs := make([]upstreamSpec, 0, 1+len(p.OverloadRetryDelays))
	for range 1 + len(p.OverloadRetryDelays) {
		specs = append(specs, upstreamSpec{status: 529, body: overloadBody})
	}
	up := &scriptedUpstream{specs: specs}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-529", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-529")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["message"] != overloadBody {
		t.Fatalf("过载用尽后应原样投递上游错误体: %v", events)
	}
	if n := up.callCount(); n != 1+len(p.OverloadRetryDelays) {
		t.Fatalf("应按档位退避重试后放弃，上游调用 %d 次", n)
	}
	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusActive || acc.CoolingUntil != nil {
		t.Fatalf("平台过载不应改变账号状态: status=%s cooling=%v", acc.Status, acc.CoolingUntil)
	}
	if acc.FailCount != 0 {
		t.Fatalf("平台过载不是账号的错，不应计入 fail_count: %d", acc.FailCount)
	}
	if acc.RateLimitStreak != 0 {
		t.Fatalf("平台过载不应累加限流阶梯计数: %d", acc.RateLimitStreak)
	}
	wantErrorKind(t, acc, model.ErrorKindUpstreamOverload)
}

// 过载退避后恢复就必须成功：这是「不换号、只退避」这个选择的意义所在。
func Test529OverloadRecoversInAsyncPool(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "overload-recover")
	p.OverloadRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: 529, body: `{"code":1305,"msg":"该模型当前访问量过大，请您稍后再试"}`},
		{status: http.StatusOK, lines: []string{`data: {"id":"ok"}`, `data: [DONE]`}},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-529-ok", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-529-ok")

	events := drainEvents(tk)
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	if strings.Join(types, ",") != "ready,chunk,done" {
		t.Fatalf("过载退避一次后应成功: %v", events)
	}
	if up.callCount() != 2 {
		t.Fatalf("应恰好重试一次: %d", up.callCount())
	}
	if acc := st.ListAccounts(model.ProviderZai)[0]; acc.Status != model.StatusActive {
		t.Fatalf("恢复后账号应仍为正常: %s", acc.Status)
	}
}

// 405 + 风控文案：异步路径与 sync 同语义——冷却整个账号并按阶梯选档，然后换号。
// 两条路径对同一账号必须标出相同状态（项目硬不变式）。
func Test405RiskControlInAsyncPool(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "risk-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusMethodNotAllowed,
			body: `{"error":{"message":"Request has been blocked due to unusual activity."}}`},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-risk", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-risk")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusCooling {
		t.Fatalf("风控 405 应冷却整个账号: %s", acc.Status)
	}
	if acc.RiskControlStreak != 1 {
		t.Fatalf("连续命中计数应为 1: %d", acc.RiskControlStreak)
	}
	if acc.CoolingUntil == nil {
		t.Fatal("应写入冷却截止时间")
	}
	wantErrorKind(t, acc, model.ErrorKindRiskControl)
}

// 请求级风控（同一 body 在 ≥2 个不同账号上都被拒）：与 sync 同判定——不冷却任何账号、
// 不继续换号，把上游原文投递一次并终止本票。
//
// 2026-09-24 事故的异步侧等价形态：一票最多试 AsyncMaxRetries+1 个账号，若无此判定，
// 一份被上游拒绝的请求体会把整票的账号额度全部烧掉（sync 侧当日烧掉 67 个）。
func Test405RequestLevelRiskInAsyncPool(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	// fixture 默认 AsyncMaxRetries=0（只试 1 个账号），而判定成立需要试到第 2 个账号。
	// 这里放开到 3（生产默认值），第 3 个账号是否被试才成为有意义的断言。
	// 换号退避是 1<<retries 秒（此用例付出 2s），是该路径既有代价，不为测试改动。
	config.AsyncMaxRetries = 3
	t.Cleanup(func() { config.AsyncMaxRetries = 0 })
	addJWTAccountN(t, st, "req-level-1", 1)
	addJWTAccountN(t, st, "req-level-2", 2)
	addJWTAccountN(t, st, "req-level-3", 3)

	riskBody := `{"error":{"message":"Request has been blocked due to unusual activity."}}`
	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusMethodNotAllowed, body: riskBody},
		{status: http.StatusMethodNotAllowed, body: riskBody},
		{status: http.StatusMethodNotAllowed, body: riskBody},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-req-level", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-req-level")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_error" {
		t.Fatalf("请求级风控应投递上游原文（upstream_error）: %v", events)
	}
	if msg, _ := errObj["message"].(string); msg != riskBody {
		t.Fatalf("应原样投递上游错误体: %q", msg)
	}
	// 第 2 个账号给出同一信号即判定成立，第 3 个账号不该再被试。
	if n := up.callCount(); n != 2 {
		t.Fatalf("判定成立后应停止换号，上游调用应为 2 次: %d", n)
	}
	for _, acc := range st.ListAccounts(model.ProviderZai) {
		if acc.Status != model.StatusActive || acc.CoolingUntil != nil {
			t.Fatalf("请求级风控不该冷却账号 %s: status=%s until=%v", acc.Name, acc.Status, acc.CoolingUntil)
		}
		if acc.RiskControlStreak != 0 {
			t.Fatalf("请求级风控不该推进账号 %s 的风控阶梯: %d", acc.Name, acc.RiskControlStreak)
		}
	}
}

// 非风控 405 在异步路径同样不冷却（与 sync 一致），落「其余错误」投递事件。
func Test405WithoutRiskBodyInAsyncPool(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "plain-405-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusMethodNotAllowed, body: `{"error":{"message":"method not allowed"}}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-405", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-405")

	events := drainEvents(tk)
	last := events[len(events)-1]
	errObj, _ := last.Data.(map[string]any)["error"].(map[string]any)
	if errObj == nil || errObj["type"] != "upstream_error" {
		t.Fatalf("非风控 405 应投递 upstream_error: %v", events)
	}
	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusActive || acc.CoolingUntil != nil {
		t.Fatalf("非风控 405 不该冷却账号: status=%s until=%v", acc.Status, acc.CoolingUntil)
	}
	if acc.RiskControlStreak != 0 {
		t.Fatalf("非风控 405 不该推进风控阶梯: %d", acc.RiskControlStreak)
	}
	wantErrorKind(t, acc, model.ErrorKindUpstreamError)
}

func TestSSEJSONEscapesNonASCII(t *testing.T) {
	// sseJSON 对齐 Python json.dumps 默认 ensure_ascii=True。
	got := sseJSON(map[string]any{"id": "中文", "n": float64(3)})
	if !strings.Contains(got, `\u4e2d\u6587`) {
		t.Fatalf("非 ASCII 应转义为小写 \\u 序列: %s", got)
	}
	if strings.Contains(got, "中") {
		t.Fatalf("不应包含原始字符: %s", got)
	}
	// ASCII 保持原样、HTML 字符不转义（对齐 Python 行为）
	got = sseJSON(map[string]any{"s": "<a>&</a>"})
	if !strings.Contains(got, "<a>&</a>") {
		t.Fatalf("HTML 字符不应转义: %s", got)
	}
}

// async 路径必须与 engine 用同一套分类：429 的额度上限码族标「该模型耗尽」，
// 而非一律标 cooling（曾因两条路径各自实现而分歧）。
func TestQuotaExhaustedCodeMarksModelNotCooling(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "quota-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":1310,"msg":"weekly limit"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-quota", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-quota")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status == model.StatusCooling {
		t.Fatalf("额度上限码族不应标 cooling（应与 engine 一致）: %s", acc.Status)
	}
	if !containsStr(acc.ExhaustedModels, "glm-5.3") {
		t.Fatalf("应标记该模型耗尽（正規化為小寫）: %v", acc.ExhaustedModels)
	}
	wantErrorKind(t, acc, model.ErrorKindQuotaExhausted)
}

// 200 包业务错误（code=1005 每日额度耗尽）：异步路径必须与同步路径一样标记该模型
// 耗尽并换号。此前 200 会直接进 forwardSSE——客户端拿到空流，账号也不被标任何状态，
// 下一次选号还会选中它。
func TestBusinessCode1005In200MarksModelExhausted(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "daily-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, body: `{"code":1005,"msg":"exceed quota limit"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-1005", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-1005")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status == model.StatusCooling {
		t.Fatalf("额度耗尽不应记为冷却（应与 engine 一致）: %s", acc.Status)
	}
	if !containsStr(acc.ExhaustedModels, "glm-5.3") {
		t.Fatalf("200+1005 应标记该模型耗尽: %v", acc.ExhaustedModels)
	}

	var sawError, sawDone bool
	for _, ev := range drainEvents(tk) {
		switch ev.Type {
		case "error":
			sawError = true
		case "done":
			sawDone = true
		}
	}
	if !sawError {
		t.Fatal("应立即换号并最终投递 error 事件")
	}
	if sawDone {
		t.Fatal("额度耗尽不得投递 done——那等于告诉客户端流已正常结束")
	}
}

// 200 + JSON 但没有业务码：本票据承诺的是 SSE 流，上游却回了普通 JSON 正文。
// 这种情况不能当成功转发（那会破坏 chunk 契约），但也不能据此改写账号状态。
func TestPlainJSON200IsNotForwardedAsStream(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "plain-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, body: `{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":2}}`},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-plain", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-plain")

	var sawError, sawDone bool
	for _, ev := range drainEvents(tk) {
		switch ev.Type {
		case "error":
			sawError = true
		case "done":
			sawDone = true
		}
	}
	if !sawError || sawDone {
		t.Fatalf("无业务码的 JSON 应报错而非当成功流（error=%v done=%v）", sawError, sawDone)
	}
	if acc := st.ListAccounts(model.ProviderZai)[0]; acc.Status != model.StatusActive {
		t.Fatalf("上游返回体形态异常不应改写账号状态: %s", acc.Status)
	}
}

// 401 应标 invalid（账号失效），不得落入冷却分支。
func TestUnauthorizedMarksInvalid(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "bad-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusUnauthorized, body: ""},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-401", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-401")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusInvalid {
		t.Fatalf("401 应标 invalid: %s", acc.Status)
	}
	wantErrorKind(t, acc, model.ErrorKindAuthFailed)
}

// 3010 并发准入限制：账号仍可用，不得标 cooling 或 invalid。
func TestConcurrencyLimitKeepsAccountState(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "busy-acc")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusTooManyRequests, body: `{"code":3010,"msg":"model admission concurrency limit"}`},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-3010", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-3010")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusActive {
		t.Fatalf("3010 不应改变账号状态（当前 %s）", acc.Status)
	}
	wantErrorKind(t, acc, model.ErrorKindModelBusy)
}

// wantErrorKind 断言账号上记录的「最近错误归类」。
// 归类是前端筛选账号的依据，异步路径必须与同步路径写出相同的值。
func wantErrorKind(t *testing.T, acc *model.Account, want string) {
	t.Helper()
	if acc.LastErrorKind == nil {
		t.Fatalf("应写出错误归类（last_error=%v）", acc.LastError)
	}
	if *acc.LastErrorKind != want {
		t.Fatalf("错误归类不符: got %q want %q", *acc.LastErrorKind, want)
	}
	if acc.LastErrorAt == nil {
		t.Fatal("应写出错误发生时间")
	}
	if !model.ErrorKindValid(*acc.LastErrorKind) {
		t.Fatalf("归类必须是已登记的枚举值: %q", *acc.LastErrorKind)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// async 路径的 503 必须与 engine 用同一套递进冷却：首档 30s（而非固定 300s）、
// last_error 带 body 预览、FailCount 与 streak 同一次落库。
// （newTestPool 里 AsyncMaxRetries=0，单账号 503 后换号无号可选，最终以
// max_retries 收尾——本用例只验证账号侧状态，收尾事件见下一个用例。）
func TestUpstream503LadderAsync(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	addJWTAccount(t, st, "acc-503")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusServiceUnavailable, body: `{"error":{"message":"upstream maintenance"}}`},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-503", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-503")

	acc := st.ListAccounts(model.ProviderZai)[0]
	if acc.Status != model.StatusCooling || acc.CoolingUntil == nil {
		t.Fatalf("503 应 cooling: %+v", acc)
	}
	want := float64(time.Now().Add(30*time.Second).UnixNano()) / 1e9
	if diff := *acc.CoolingUntil - want; diff > 5 || diff < -5 {
		t.Fatalf("async 首次 503 冷却应约 30s（阶梯最低档）: %v", *acc.CoolingUntil)
	}
	if acc.Upstream503Streak != 1 {
		t.Fatalf("连续 503 计数应为 1: %d", acc.Upstream503Streak)
	}
	if acc.FailCount != 1 {
		t.Fatalf("FailCount 应 +1: %d", acc.FailCount)
	}
	if acc.LastError == nil || !strings.Contains(*acc.LastError, "upstream maintenance") {
		t.Fatalf("last_error 应带 body 预览: %v", acc.LastError)
	}
	wantErrorKind(t, acc, model.ErrorKindUpstreamUnavailable)
}

// async 选不出号的 no_account 事件必须携带池状态分解：message 内联文案 +
// error.details 结构化字段（与 sync 路径的 no_available_account 同一形态）。
// 预置一个冷却中的 jwt 账号，首轮 Select 即空。
func TestAsyncNoAccountCarriesPoolDetails(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "acc-cooling")
	until := float64(time.Now().Add(120*time.Second).UnixNano()) / 1e9
	kind := model.ErrorKindUpstreamUnavailable
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.Status = model.StatusCooling
		a.CoolingUntil = &until
		a.LastErrorKind = &kind
	}); err != nil {
		t.Fatal(err)
	}

	tk := insertTicket(p, "ticket-noacc", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-noacc")

	events := drainEvents(tk)
	var noAccount *ticketEvent
	for i := range events {
		ev := events[i]
		if ev.Type != "error" {
			continue
		}
		body, _ := ev.Data.(map[string]any)
		errObj, _ := body["error"].(map[string]any)
		if errObj != nil && errObj["type"] == "no_account" {
			noAccount = &events[i]
		}
	}
	if noAccount == nil {
		t.Fatal("应投递 no_account 错误事件")
	}
	body := noAccount.Data.(map[string]any)["error"].(map[string]any)
	details, _ := body["details"].(map[string]any)
	if details == nil {
		t.Fatalf("no_account 事件应带 details: %v", body)
	}
	// 事件 Data 未做 JSON 往返，数值保持 int；兼容 float64 以防未来改为序列化传递
	cooling := 0
	switch v := details["cooling"].(type) {
	case int:
		cooling = v
	case float64:
		cooling = int(v)
	}
	if cooling != 1 {
		t.Fatalf("details.cooling 应为 1: %v", details)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "冷卻中") || !strings.Contains(msg, "上游503") {
		t.Fatalf("message 应内联池状态分解: %s", msg)
	}
}

// ── 观测诊断行（async 侧） ────────────────────────────────────────────────────

// diagBuf 并发安全的日志缓冲：后台任务跑在别的 goroutine 里，裸 bytes.Buffer
// 会在 -race 下报错。
type diagBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *diagBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *diagBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func (b *diagBuf) diagLine(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.Contains(line, "[#]") {
			return ansiRE.ReplaceAllString(line, "")
		}
	}
	t.Fatalf("async 日志里没有诊断行：\n%s", ansiRE.ReplaceAllString(b.String(), ""))
	return ""
}

// TestNewTicketAssignsShortID：async 此前只用 36 字符 ticketID，与 sync 的 6 位
// 十六进制 reqID 风格不一致，排查时无法用同一套检索习惯对照两条路径。
func TestNewTicketAssignsShortID(t *testing.T) {
	p, _, _, _ := newTestPool(t)
	id := p.newTicket(map[string]any{"model": "GLM-5.3"})
	t.Cleanup(func() { p.releaseTicket(id) })

	tk := p.getTicket(id)
	if tk == nil {
		t.Fatal("票务未建立")
	}
	if !regexp.MustCompile(`^[0-9a-f]{6}$`).MatchString(tk.shortID) {
		t.Fatalf("shortID 应为 6 位十六进制，实得 %q", tk.shortID)
	}
}

// TestAsyncDiagLineOnMidStreamBreak：async 的中断必须与 sync 同口径落到诊断行上
// ——哪个账号、哪条线路、usage 是否完整、是不是上游侧掐断。
func TestAsyncDiagLineOnMidStreamBreak(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "diag-async")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{`data: {"id":"msg1"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	buf := &diagBuf{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })

	tk := insertTicket(p, "ticket-diag", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-diag")
	_ = drainEvents(tk)

	line := buf.diagLine(t)
	if !regexp.MustCompile(`\[#\] [0-9a-f]{6} `).MatchString(line) {
		t.Fatalf("诊断行应带 6 位十六进制 reqID：%s", line)
	}
	for _, want := range []string{
		"model=GLM-5.3",
		"stream=true",
		"ticket=ticket-diag", // 既有按 ticketID 的检索不受影响
		"acc=diag-async",
		"route=direct",
		"attempts=1",
		"complete=0",
		"trunc_total=1",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("async 诊断行缺少 %q\n行=%s", want, line)
		}
	}
	// 上游侧掐断记到 live 账号上（不是副本）。
	if got := st.Find(model.ProviderZai, acc.ID); got.StreamTruncateCount != 1 {
		t.Fatalf("StreamTruncateCount=%d，期望 1", got.StreamTruncateCount)
	}
	// 既有「流转发中断」Warn 行未受影响。
	if !strings.Contains(ansiRE.ReplaceAllString(buf.String(), ""), "流转发中断") {
		t.Fatalf("既有中断日志丢失：\n%s", ansiRE.ReplaceAllString(buf.String(), ""))
	}
}

// TestAsyncDiagRouteReportsAccountProxyLine 钉住 async 的出口读数：async 现在与
// 网关一样套用 acc.ProxyURL，账号绑了线路就必须报出那个线路名。
//
// 反向历史：这条用例原先叫 TestAsyncDiagRouteIgnoresAccountProxyLine，断言恰好相反
// （「async 恒直连，不得出现线路名」）。那是 async 漏挑上游 0d370e5 时期的护栏——
// 当时 route=direct 是真的，但出口是硬直连而非策略选择，还顺带泄露部署 IP。
// 代理路由修好后硬编码 direct 变成了误报，故本用例连同 PLAN §5.6 的出口说明一起反转。
//
// 归因仍然靠 route 读数：要对照「线路侧 vs 上游侧」时，用后台「Async 強制直連」
// 开关把这一臂再显式关掉（见 TestAsyncForceDirectIgnoresAccountProxyLine），
// 而不是靠实现缺陷。
func TestAsyncDiagRouteReportsAccountProxyLine(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	line, err := st.AddProxyProfile("line-归因", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	acc := addJWTAccount(t, st, "diag-async-line")
	if ok, err := st.AssignProxyProfile(acc.ID, line.ID); !ok || err != nil {
		t.Fatalf("指派线路失败: ok=%v err=%v", ok, err)
	}
	// 前置：账号确实绑上了线路 —— 否则这条用例什么也没验证（能空过）。
	if got := st.ProxyLabel(acc); got != "line-归因" {
		t.Fatalf("前置条件不成立：账号应绑定 line-归因，ProxyLabel=%q", got)
	}

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{`data: {"id":"msg1"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	buf := &diagBuf{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })

	tk := insertTicket(p, "ticket-diag-route", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-diag-route")
	_ = drainEvents(tk)

	got := buf.diagLine(t)
	if !strings.Contains(got, "route=line-归因") {
		t.Fatalf("账号绑了线路，诊断行应报该线路名：%s", got)
	}
	if strings.Contains(got, "route=direct") {
		t.Fatalf("async 已套用账号代理，仍报 direct 说明读数与实际出口不一致：%s", got)
	}
}

// TestAsyncForceDirectIgnoresAccountProxyLine 打开「Async 強制直連」后必须回到
// 硬直连，且诊断行如实报 direct。
//
// 这个开关的存在理由就是保留「线路 vs 直连」的归因控制臂：代理路由修好后，
// 那根本来靠实现缺陷得到的对照组就没了，改由显式旋钮提供，两个方向都可复现。
func TestAsyncForceDirectIgnoresAccountProxyLine(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	t.Cleanup(func() { _ = st.SetSetting(store.AsyncForceDirectKey, "false") })

	line, err := st.AddProxyProfile("line-强制直连", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	acc := addJWTAccount(t, st, "diag-force-direct")
	if ok, err := st.AssignProxyProfile(acc.ID, line.ID); !ok || err != nil {
		t.Fatalf("指派线路失败: ok=%v err=%v", ok, err)
	}
	if err := st.SetSetting(store.AsyncForceDirectKey, "true"); err != nil {
		t.Fatalf("打开开关失败: %v", err)
	}
	// 前置：开关确实读到了（否则下面会因账号仍走线路而红，却看不出是开关没生效）。
	if !st.AsyncForceDirect() {
		t.Fatal("前置条件不成立：AsyncForceDirect 应为 true")
	}

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{`data: {"id":"msg1"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	buf := &diagBuf{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })

	tk := insertTicket(p, "ticket-force-direct", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-force-direct")
	_ = drainEvents(tk)

	got := buf.diagLine(t)
	if !strings.Contains(got, "route=direct") {
		t.Fatalf("强制直连开关打开时诊断行应报 route=direct：%s", got)
	}
	if strings.Contains(got, "line-强制直连") {
		t.Fatalf("强制直连开关未生效，仍打出了线路名：%s", got)
	}
}

// TestAccountProxyIsUsed 账号配置的 proxy_url 必须作用于 async 路径。
//
// README 与 PLAN §5.9 都承诺「该账号的网关请求、额度查询与套餐领取均走对应代理」。
// async 池曾忽略 proxy_url 直接出站：配置代理的账号在这条路径上以服务器真实 IP 连
// 上游，正是使用者配置代理要规避的（IP 绑定、地区限制、风控）。
// 本用例是缺口 0d370e5 的回归守卫：去掉 clientFor 的代理查找后，下面的 connects
// 会变成 0（上游被直连访问），用例即红。
func TestAccountProxyIsUsed(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "proxied")

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, contentType: "text/event-stream", lines: []string{
			`data: {"type":"message_delta","usage":{"output_tokens":1}}`,
		}},
	}}
	config.UpstreamZai = up.start(t).URL

	// 最小转发代理：记录被请求的目标，再把请求原样转发到真实上游。
	// 用裸 TCP listener 而非 httptest，因为需要接管连接后按代理协议转发。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var targets []string
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				// 明文 http 目标走绝对 URI（Proxy 字段的标准行为），
				// https 目标才走 CONNECT；两种都记为该代理被使用。
				target := req.Host
				if target == "" {
					target = req.URL.Host
				}
				mu.Lock()
				targets = append(targets, target)
				mu.Unlock()

				outReq := req.Clone(context.Background())
				outReq.RequestURI = ""
				if outReq.URL.Host == "" {
					outReq.URL.Host = target
				}
				// 刻意用零值 Transport：其 Proxy 为 nil ⇒ 不吃环境代理，
				// 避免本机 HTTP_PROXY 把转发又绕一层。
				resp, err := (&http.Transport{}).RoundTrip(outReq)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				_ = resp.Write(c)
			}(conn)
		}
	}()

	st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		proxyURL := "http://" + ln.Addr().String()
		a.ProxyURL = &proxyURL
	})

	tk := insertTicket(p, "ticket-proxy", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-proxy")
	_ = drainEvents(tk)

	mu.Lock()
	gotTargets := len(targets)
	mu.Unlock()
	if gotTargets == 0 {
		t.Fatalf("账号配置了代理，请求却未经代理出站（上游调用=%d）", up.callCount())
	}
	if up.callCount() == 0 {
		t.Fatal("上游应收到请求")
	}
}

// TestSkipsAPIKeyAccountsInMixedPool 混合池里轮到 apiKey 账号时应跳过，而不是终止整张票。
//
// async 仅支持 JWT 账号，但池中可以混有 apiKey 账号；Select 是 round-robin，一次只回
// 一个。曾经的写法是「非 jwt 就 emitError 并 return」，且 tried 标记在检查之后，
// 于是轮询再次轮到同一 apiKey 账号时依旧失败——池里明明有可用 JWT 账号，请求却
// 间歇性、与账号状态无关地失败。本用例是缺口 e86c5bc 的回归守卫。
func TestSkipsAPIKeyAccountsInMixedPool(t *testing.T) {
	p, st, _, _ := newTestPool(t)

	// 交错添加，确保 Select 的轮询顺序里 apiKey 账号排在 JWT 之前
	if _, err := st.AddAccount(model.ProviderZai, "key-1", "sk-plain-key"); err != nil {
		t.Fatal(err)
	}
	addJWTAccount(t, st, "jwt-1")

	// 每次请求都要一个成功规格（scriptedUpstream 用完后回退 502）
	okSpec := upstreamSpec{status: http.StatusOK, contentType: "text/event-stream", lines: []string{
		`data: {"type":"message_delta","usage":{"output_tokens":1}}`,
	}}
	up := &scriptedUpstream{specs: []upstreamSpec{okSpec, okSpec, okSpec, okSpec}}
	config.UpstreamZai = up.start(t).URL

	// 多跑几次：无论轮询从哪个账号开始，都必须落到 JWT 账号上
	for i := range 4 {
		id := fmt.Sprintf("ticket-mixed-%d", i)
		tk := insertTicket(p, id, map[string]any{"model": "GLM-5.3", "messages": []any{}})
		p.processTicket(context.Background(), id)

		events := drainEvents(tk)
		last := events[len(events)-1]
		if last.Type != "done" {
			t.Fatalf("第 %d 次：应跳过 apiKey 账号并成功交付，实际 %+v", i, events)
		}
	}
	if up.callCount() == 0 {
		t.Fatal("上游应收到请求")
	}
}

// TestSuccessRecordsUsageAndRevivesStatus async 成功交付后必须与 engine.success
// 记出相同的账号状态。
//
// 曾只累加 token：后台用量页漏算 async 流量，且冷却到期的账号即使这里已经成功
// 返回，状态仍停在 cooling，只能等下一轮额度轮询（默认 60s）才恢复调度。
// 本用例是缺口 5105b5d 的回归守卫：去掉 MarkSuccess 调用即在 use_count 上变红。
func TestSuccessRecordsUsageAndRevivesStatus(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "revive")

	// 制造「冷却已到期」的前置状态：这是最需要被成功路径复位的情形
	pastCooling := float64(time.Now().Add(-time.Minute).UnixNano()) / 1e9
	msg := "上游限流 HTTP 429"
	st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Status = model.StatusCooling
		a.CoolingUntil = &pastCooling
		a.LastError = &msg
	})

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, contentType: "text/event-stream", lines: []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
			`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
		}},
	}}
	config.UpstreamZai = up.start(t).URL

	insertTicket(p, "ticket-success", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-success")

	got := st.Find(model.ProviderZai, acc.ID)
	if got.UseCount != 1 {
		t.Fatalf("成功交付应累计 use_count（与 engine 一致）: %d", got.UseCount)
	}
	if got.LastUsedAt == nil {
		t.Fatal("成功交付应写入 last_used_at")
	}
	if got.Status != model.StatusActive {
		t.Fatalf("成功后应复位为 active: %s", got.Status)
	}
	if got.TotalInputTokens != 7 || got.TotalOutputTokens != 3 {
		t.Fatalf("token 统计不符: in=%d out=%d", got.TotalInputTokens, got.TotalOutputTokens)
	}
}

// TestEntryNormalizesProviderPrefixedModel 入口必须与 /v1/messages 一样先归一化
// 模型名再判白名单，且票内 model 名已归一。
//
// `anthropic/GLM-5.3` 这类 `provider/model` 写法在 /v1/messages 能过、在 async 却
// 400 model_not_allowed，与「模型白名单与 /v1/messages 一致」的承诺矛盾；票内
// model 名不归一还会让 Select 的模型分档与额度比对用错键。本用例是缺口 6da8df6
// 的回归守卫：去掉入口 NormalizeBody 后首条断言即 400 变红。
func TestEntryNormalizesProviderPrefixedModel(t *testing.T) {
	p, st, _, solver := newTestPool(t)
	addJWTAccount(t, st, "async-acc")
	srv := newTestMux(t, p)
	_ = st.SetSetting("gateway_key", "sk-test")
	solver.err = errors.New("browser down") // 后台任务走 captcha_required 失败路径，避免触网

	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })

	buf := &diagBuf{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })

	// 带 provider 前缀：入口归一化后才在白名单内
	body := `{"model":"anthropic/GLM-5.3","max_tokens":8,"messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anthropic/GLM-5.3 应经归一化后 200，实际 %d", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}

	// 票内 model 名也必须是归一化后的形态（否则 Select/额度比对会用错键）
	got := buf.diagLine(t)
	if !strings.Contains(got, "model=GLM-5.3") {
		t.Fatalf("票内 model 未归一化，诊断行应报 model=GLM-5.3：%s", got)
	}
	if strings.Contains(got, "anthropic/") {
		t.Fatalf("诊断行仍带 provider 前缀，说明归一化没作用到票上：%s", got)
	}

	// 对照：不在白名单的模型仍应 400 —— 证明归一化没有放宽白名单
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/async/v1/messages",
		strings.NewReader(`{"model":"bogus/GLM-9","messages":[]}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("白名单外模型应 400，实际 %d", resp2.StatusCode)
	}
}

// TestSuccessClearsFailureStreaks async 成功交付必须把三个「连续失败」计数清零，
// 并把冷却已到期的账号复位为 active。
//
// 这三个 Reset* 是本仓库相对上游的实现（上游的 MarkSuccess 只记 UseCount/
// LastUsedAt 与状态复位），所以按上游窄版回移时会静默丢掉。丢掉不会立刻报错，
// 但下一次失败会直接跳到高位冷却档：已连续 3 次 503 的账号，冷却到期后成功打通
// 一次本该回到 30s 档，却会继续按 60/120 递进。
//
// 此前没有任何用例盯住这一点——TestRateLimitRetrySucceedsInPlace 的账号起始
// streak 就是 0，断言恒真。故补上本用例：把 MarkSuccess 里的三个 Reset* 或整个
// MarkSuccess 调用删掉，本用例即红。
func TestSuccessClearsFailureStreaks(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "streak-reset")

	// 冷却已到期（过期时间在过去）⇒ 账号可被选中，成功即应恢复调度。
	pastCooling := float64(time.Now().Add(-time.Minute).UnixNano()) / 1e9
	msg := "上游服務不可用 HTTP 503"
	st.Update(acc.Provider, acc.ID, func(a *model.Account) {
		a.Status = model.StatusCooling
		a.CoolingUntil = &pastCooling
		a.LastError = &msg
		a.RateLimitStreak = 2
		a.RiskControlStreak = 1
		a.Upstream503Streak = 3
	})

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, contentType: "text/event-stream", lines: []string{
			`data: {"type":"message_delta","usage":{"output_tokens":1}}`,
		}},
	}}
	config.UpstreamZai = up.start(t).URL

	tk := insertTicket(p, "ticket-streak-reset", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	p.processTicket(context.Background(), "ticket-streak-reset")
	events := drainEvents(tk)
	if last := events[len(events)-1]; last.Type != "done" {
		t.Fatalf("应成功交付，实际 %+v", events)
	}

	got := st.Find(model.ProviderZai, acc.ID)
	if got.RateLimitStreak != 0 {
		t.Fatalf("成功后限流连续计数应清零: %d", got.RateLimitStreak)
	}
	if got.RiskControlStreak != 0 {
		t.Fatalf("成功后风控连续计数应清零: %d", got.RiskControlStreak)
	}
	if got.Upstream503Streak != 0 {
		t.Fatalf("成功后 503 连续计数应清零: %d", got.Upstream503Streak)
	}
	if got.Status != model.StatusActive {
		t.Fatalf("冷却已到期的账号成功后应复位为 active: %s", got.Status)
	}
}
