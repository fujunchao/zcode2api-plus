package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// syncBuffer 可被多个 goroutine 安全写入的日志缓冲。
//
// 请求处理跑在 http.Serve 自己的 goroutine 里，断言跑在测试 goroutine 里，
// 因此不能用裸 bytes.Buffer——CI 会带 -race 跑。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// diagLine 从捕获到的日志里取出 `[#]` 诊断行（去 ANSI 后）。
func (b *syncBuffer) diagLine(t *testing.T) string {
	t.Helper()
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.Contains(line, "[#]") {
			return stripANSI(line)
		}
	}
	t.Fatalf("日志里没有诊断行：\n%s", stripANSI(b.String()))
	return ""
}

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

type diagFixture struct {
	srv *httptest.Server
	ups *httptest.Server // mock 上游（其 URL 也充当测试里的「代理线路」地址）
	st  *store.Store
	eng *Engine
	buf *syncBuffer
}

// newDiagFixture 建一个「自定义 mock 上游 + 网关入口 + 日志捕获」的夹具。
func newDiagFixture(t *testing.T, up http.Handler) *diagFixture {
	t.Helper()

	ups := httptest.NewServer(up)
	t.Cleanup(ups.Close)

	st := openStore(t)
	if err := st.SetSetting("gateway_key", "sk-test"); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(st, captcha.NewManager(), nil)
	eng.BusyRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	eng.RateLimitRetryDelay = 0

	h := &Handler{Engine: eng, Auth: auth.New(st)}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldZai, oldFB := config.UpstreamZai, config.UpstreamZaiFallback
	config.UpstreamZai = ups.URL + "/zai"
	config.UpstreamZaiFallback = ups.URL + "/fallback"
	t.Cleanup(func() { config.UpstreamZai, config.UpstreamZaiFallback = oldZai, oldFB })

	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })

	return &diagFixture{srv: srv, ups: ups, st: st, eng: eng, buf: buf}
}

func (f *diagFixture) post(t *testing.T, body map[string]any) (int, string) {
	t.Helper()
	raw, err := marshalJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

const sseStart = `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}` + "\n\n"

// TestDiagLineOnUpstreamTruncation：上游在响应体中途掐断时，诊断行必须能独立回答
// 「哪个账号、哪条线路、有没有在流、usage 是否完整、是不是上游侧掐断」——这正是
// 2026-09-21 那次 300s 时长墙事故里无法从日志回答的问题。
func TestDiagLineOnUpstreamTruncation(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, sseStart)
		fl.Flush()
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"...念..."}}`+"\n\n")
		fl.Flush()
		time.Sleep(20 * time.Millisecond)
		// 中途掐断：chunked 编码不收尾 ⇒ 客户端读到 unexpected EOF。
		panic(http.ErrAbortHandler)
	})

	f := newDiagFixture(t, up)
	acc, err := f.st.AddAccount(model.ProviderZai, "diag-acct", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}
	status, _ := f.post(t, msgBody())
	if status != http.StatusOK {
		t.Fatalf("已交付 200，客户端不应收到错误状态: %d", status)
	}

	line := f.buf.diagLine(t)
	for _, want := range []string{
		"model=GLM-5.3",
		"stream=false",
		"attempts=1",
		"acc=diag-acct",
		"route=direct",  // 未配代理 ⇒ 直连
		"trunc_total=1", // 上游侧掐断记一次
		"complete=0",    // 没有拿到终值
		"unexpected EOF",
		"firstbyte=", // 已测到首字节（不是 -）
	} {
		if !strings.Contains(line, want) {
			t.Errorf("诊断行缺少 %q\n行=%s", want, line)
		}
	}
	if strings.Contains(line, "firstbyte=-") {
		t.Errorf("流式请求应测到首字节延迟: %s", line)
	}
	if strings.Contains(line, "status=-") {
		t.Errorf("上游已返回 200，状态码应可见: %s", line)
	}

	// 计数落在 live 账号上（不是副本）。
	if got := f.st.Find(model.ProviderZai, acc.ID); got.StreamTruncateCount != 1 {
		t.Fatalf("StreamTruncateCount=%d，期望 1", got.StreamTruncateCount)
	}
	// 既有行必须仍在、且形态未变（红线：外部分析脚本依赖它们）。
	if !strings.Contains(stripANSI(f.buf.String()), "流传输中断: unexpected EOF") {
		t.Fatalf("既有 <!> 行丢失：\n%s", stripANSI(f.buf.String()))
	}
}

// TestDiagLineFirstByteAndMaxGap：验证「数据一直在流」与「中间静默」可区分。
// 固定时长墙（数据从头流到尾）与空闲超时（中途长时间无数据）的修法完全相反，
// 这两个指标就是把它们分开的依据。
func TestDiagLineFirstByteAndMaxGap(t *testing.T) {
	const (
		firstDelay = 60 * time.Millisecond
		gapDelay   = 40 * time.Millisecond
	)
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		fl.Flush()
		time.Sleep(firstDelay) // 首字节延迟
		_, _ = io.WriteString(w, sseStart)
		fl.Flush()
		time.Sleep(gapDelay)
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"a"}}`+"\n\n")
		fl.Flush()
		time.Sleep(gapDelay)
		_, _ = io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`+"\n\n"+
			`data: {"type":"message_stop"}`+"\n\n")
		fl.Flush()
	})

	f := newDiagFixture(t, up)
	if _, err := f.st.AddAccount(model.ProviderZai, "diag-acct", "sk-abc"); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.post(t, msgBody()); status != http.StatusOK {
		t.Fatalf("期望 200，实得 %d", status)
	}

	line := f.buf.diagLine(t)
	if !strings.Contains(line, "complete=1") {
		t.Fatalf("上游交出终值，应标记 complete=1: %s", line)
	}
	if !strings.Contains(line, "trunc_total=0") {
		t.Fatalf("正常交付不应累计截断次数: %s", line)
	}

	first := parseDiagMs(t, line, "firstbyte")
	gap := parseDiagMs(t, line, "maxgap")
	if first < int(firstDelay.Milliseconds())-15 {
		t.Errorf("首字节延迟测得 %dms，期望 ≥ %dms", first, firstDelay.Milliseconds()-15)
	}
	if gap < int(gapDelay.Milliseconds())-15 {
		t.Errorf("最大 chunk 间隔测得 %dms，期望 ≥ %dms", gap, gapDelay.Milliseconds()-15)
	}
	if !strings.Contains(line, "bytes=") || strings.Contains(line, "bytes=-") {
		t.Errorf("应记录读到的字节数: %s", line)
	}
}

// TestDiagLineOnNoAvailableAccount：选不出号的早退路径也必须有一行诊断，
// 且账号/线路字段显式为 `-`（这是最缺池状态解释的场景，不能静默无输出）。
func TestDiagLineOnNoAvailableAccount(t *testing.T) {
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("没有账号时不应触达上游")
		w.WriteHeader(http.StatusBadGateway)
	})
	f := newDiagFixture(t, up) // 刻意不添加任何账号

	status, body := f.post(t, msgBody())
	if status != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实得 %d（%s）", status, body)
	}

	line := f.buf.diagLine(t)
	for _, want := range []string{"acc=-", "route=-", "attempts=0", "complete=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("早退路径的诊断行缺少 %q\n行=%s", want, line)
		}
	}
	// 既有 <!> 行必须仍在（文案是繁體，别改它——外部分析脚本按它做匹配）。
	if !strings.Contains(stripANSI(f.buf.String()), "無可用帳號") {
		t.Fatalf("既有 <!> 行丢失：\n%s", stripANSI(f.buf.String()))
	}
}

// TestErrorBranchLogsBodyPreview：业务码分支此前只打状态码，body 里的业务码与文案
// 在日志里完全不可见（排查只能靠用户贴客户端报错）。这里钉住「有 body 就带预览、
// 空 body 不留孤立冒号」两种形态。
func TestErrorBranchLogsBodyPreview(t *testing.T) {
	t.Run("带 body 的分支补上预览", func(t *testing.T) {
		up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_token","message":"token expired"}}`)
		})
		f := newDiagFixture(t, up)
		if _, err := f.st.AddAccount(model.ProviderZai, "diag-acct", "sk-abc"); err != nil {
			t.Fatal(err)
		}
		f.post(t, msgBody())

		logs := stripANSI(f.buf.String())
		if !strings.Contains(logs, "鉴权失败 401") || !strings.Contains(logs, "token expired") {
			t.Fatalf("401 分支应带上上游 body 预览：\n%s", logs)
		}
	})

	t.Run("空 body 不留孤立冒号", func(t *testing.T) {
		up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized) // JWT 上游的裸 401：空 body
		})
		f := newDiagFixture(t, up)
		if _, err := f.st.AddAccount(model.ProviderZai, "diag-acct", "sk-abc"); err != nil {
			t.Fatal(err)
		}
		f.post(t, msgBody())

		logs := stripANSI(f.buf.String())
		if !strings.Contains(logs, "鉴权失败 401") {
			t.Fatalf("401 分支日志丢失：\n%s", logs)
		}
		if strings.Contains(logs, "切换下一个:") {
			t.Fatalf("空 body 不应在文案后留下孤立冒号：\n%s", logs)
		}
	})
}

// TestStreamTruncateCountAccumulatesAcrossSuccess：计数是「累计」而不是「连续」。
//
// 这不是风格选择，而是被代码结构逼出来的正确语义：sync 路径的 e.success(acc) 在
// 读流**之前**就被调用（deliverStream 首行），若做成「成功即归零」，计数会被同一
// 次请求提前清掉、永远到不了 2。而「断流是否集中在某账号/某线路」本来就要用累计值
// 比较才有意义。
func TestStreamTruncateCountAccumulatesAcrossSuccess(t *testing.T) {
	var call int32
	var mu sync.Mutex
	up := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		call++
		n := call
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, sseStart)
		fl.Flush()
		if n >= 3 {
			// 第三次：正常收尾（交出终值）。
			_, _ = io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":11}}`+"\n\n"+
				`data: {"type":"message_stop"}`+"\n\n")
			fl.Flush()
			return
		}
		panic(http.ErrAbortHandler)
	})

	f := newDiagFixture(t, up)
	acc, err := f.st.AddAccount(model.ProviderZai, "diag-acct", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}

	for i, want := range []int{1, 2, 2} {
		if status, _ := f.post(t, msgBody()); status != http.StatusOK {
			t.Fatalf("第 %d 次请求期望 200，实得 %d", i+1, status)
		}
		if got := f.st.Find(model.ProviderZai, acc.ID); got.StreamTruncateCount != want {
			t.Fatalf("第 %d 次请求后 StreamTruncateCount=%d，期望 %d（成功不得清零累计值）",
				i+1, got.StreamTruncateCount, want)
		}
	}
}

func parseDiagMs(t *testing.T, line, key string) int {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(key) + `=(\d+)ms`)
	m := re.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("诊断行里找不到 %s=<ms>：%s", key, line)
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", key, err)
	}
	return v
}
