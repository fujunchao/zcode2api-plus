package asyncpool

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

// bindAsyncLine：建两条线路（地址都用脚本化上游），把账号绑到坏线路。
func bindAsyncLine(t *testing.T, st *store.Store, acc *model.Account, proxyURL string) (badID, goodID string) {
	t.Helper()
	bad, err := st.AddProxyProfile("async-bad", proxyURL, true)
	if err != nil {
		t.Fatal(err)
	}
	good, err := st.AddProxyProfile("async-good", proxyURL, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(acc.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	return bad.ID, good.ID
}

func hasAsyncProfile(st *store.Store, id string) bool {
	for _, p := range st.ListProxyProfiles() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// TestAsyncLineTruncateStrikesPurgeLine：async 路径与 sync 同一套线路熔断——
// 同一线路连续 3 次上游侧断流即移除并改派；同时断流要触发额度刷新钩子。
func TestAsyncLineTruncateStrikesPurgeLine(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "async-strike")

	up := &scriptedUpstream{specs: []upstreamSpec{
		// 每张票换一个新上游脚本：三次「先发 chunk 再掐断」。
		{status: http.StatusOK, lines: []string{`data: {"id":"m1","delta":"x"}`}, abort: true},
		{status: http.StatusOK, lines: []string{`data: {"id":"m2","delta":"x"}`}, abort: true},
		{status: http.StatusOK, lines: []string{`data: {"id":"m3","delta":"x"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	badID, goodID := bindAsyncLine(t, st, acc, config.UpstreamZai)
	if err := st.SetSetting(store.LineTruncateStrikesKey, "3"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(store.LineTruncateAvoidSecondsKey, "0"); err != nil {
		t.Fatal(err)
	}
	var fired atomic.Int32
	p.OnQuotaRefresh = func(*model.Account) { fired.Add(1) }

	for i := 1; i <= 3; i++ {
		tk := insertTicket(p, "tk-"+string(rune('a'+i-1)), map[string]any{"model": "GLM-5.3", "messages": []any{}})
		p.processTicket(context.Background(), "tk-"+string(rune('a'+i-1)))
		drainEvents(tk)
		if i < 3 && !hasAsyncProfile(st, badID) {
			t.Fatalf("未达阈值（%d<3）线路不应被移除", i)
		}
	}
	if hasAsyncProfile(st, badID) {
		t.Fatal("async 路径连续 3 次断流后线路应被移除")
	}
	if !hasAsyncProfile(st, goodID) {
		t.Fatal("幸存线路不应被误删")
	}
	if live := st.Find(model.ProviderZai, acc.ID); live.ProxyID == nil || *live.ProxyID != goodID {
		t.Fatalf("绑定账号应被改派到幸存线路: %v", live.ProxyID)
	}
	// 断流即刷新：三次断流各触发一次（异步钩子，轮询等待）。
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fired.Load() < 1 {
		t.Fatal("async 断流应触发额度刷新钩子")
	}
}

// TestAsyncForwardSSENilDiagStillRecords：断流记录与线路熔断不得依赖诊断容器——
// 修复前 diag==nil 时整段漏记（诊断行只是展示，账号计数与线路熔断必须照常发生）。
func TestAsyncForwardSSENilDiagStillRecords(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "nil-diag")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"m1\",\"delta\":\"x\"}\n\n")
		fl.Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	tk := insertTicket(p, "tk-nildiag", map[string]any{"model": "GLM-5.3", "messages": []any{}})
	_, _ = p.forwardSSE(context.Background(), "tk-nildiag", resp, acc, nil)
	drainEvents(tk)

	if got := st.Find(model.ProviderZai, acc.ID).StreamTruncateCount; got != 1 {
		t.Fatalf("diag==nil 时断流也应记到账号，实际 %d", got)
	}
	// 无线路绑定（直连）不熔断，也不应 panic/报错。
	if got := len(st.LineTruncateStats()); got != 0 {
		t.Fatalf("直连账号不应产生线路计数: %v", st.LineTruncateStats())
	}
}

// TestAsyncSuccessResetsLineStreak：async 成功交付同样复位线路连击（与 sync 同口径）。
func TestAsyncSuccessResetsLineStreak(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	acc := addJWTAccount(t, st, "async-reset")
	_ = acc

	up := &scriptedUpstream{specs: []upstreamSpec{
		{status: http.StatusOK, lines: []string{`data: {"id":"m1","delta":"x"}`}, abort: true},
		{status: http.StatusOK, lines: []string{`data: {"id":"m2","delta":"x"}`}, abort: true},
		// 第三次完整收尾。
		{status: http.StatusOK, lines: []string{
			`data: {"id":"m3","delta":"x"}`,
			`data: {"type":"message_stop"}`,
		}},
		{status: http.StatusOK, lines: []string{`data: {"id":"m4","delta":"x"}`}, abort: true},
		{status: http.StatusOK, lines: []string{`data: {"id":"m5","delta":"x"}`}, abort: true},
	}}
	config.UpstreamZai = up.start(t).URL

	badID, _ := bindAsyncLine(t, st, acc, config.UpstreamZai)
	for _, kv := range [][2]string{
		{store.LineTruncateStrikesKey, "3"},
		{store.LineTruncateAvoidSecondsKey, "0"},
	} {
		if err := st.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	// web.SetOut 抑制噪音（成功路径会打 ReqOk）。
	web.SetOut(&noopWriter{})
	t.Cleanup(func() { web.SetOut(nil) })

	for _, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		tk := insertTicket(p, id, map[string]any{"model": "GLM-5.3", "messages": []any{}})
		p.processTicket(context.Background(), id)
		drainEvents(tk)
	}
	if !hasAsyncProfile(st, badID) {
		t.Fatal("2+成功+2 次断流不应触发 3 连击熔断")
	}
	stats := st.LineTruncateStats()
	if got := stats[badID]; got.Streak != 2 || got.Total != 4 {
		t.Fatalf("成功复位后 streak=2 total=4，实际 streak=%d total=%d", got.Streak, got.Total)
	}
}

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
