package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// diagLines 取出捕获日志里的全部 `[#]` 诊断行（去 ANSI）。
func (b *syncBuffer) diagLines(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.Contains(line, "[#]") {
			out = append(out, stripANSI(line))
		}
	}
	if len(out) == 0 {
		t.Fatalf("日志里没有诊断行：\n%s", stripANSI(b.String()))
	}
	return out
}

// cutUp 返回一个可切换「中途掐断 / 完整收尾」的上游：abortN 内的请求掐断，其余完整。
type cutUp struct {
	mu     sync.Mutex
	calls  int
	abortN map[int]bool // 1-based 调用序号；命中则中途掐断
}

func (u *cutUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.calls++
	n := u.calls
	abort := u.abortN[n]
	u.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	_, _ = io.WriteString(w, sseStart)
	fl.Flush()
	time.Sleep(5 * time.Millisecond)
	if abort {
		// 中途掐断：chunked 编码不收尾 ⇒ 客户端读到 unexpected EOF。
		panic(http.ErrAbortHandler)
	}
	_, _ = io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\n")
	fl.Flush()
	_, _ = io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	fl.Flush()
}

// bindToLine 建两条启用线路（地址都用 mock 上游，能真实承载请求）并把账号绑到坏线路。
func bindToLine(t *testing.T, st *store.Store, acc *model.Account, proxyURL string) (badID, goodID string) {
	t.Helper()
	bad, err := st.AddProxyProfile("bad-line", proxyURL, true)
	if err != nil {
		t.Fatal(err)
	}
	good, err := st.AddProxyProfile("good-line", proxyURL, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssignProxyProfile(acc.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	return bad.ID, good.ID
}

func hasProfile(st *store.Store, id string) bool {
	for _, p := range st.ListProxyProfiles() {
		if p.ID == id {
			return true
		}
	}
	return false
}

// setQuota 给账号写一份 GLM-5.3 的额度快照（构造「额度优先」排序的确定性输入）。
func setQuota(t *testing.T, st *store.Store, idOrName string, remaining float64) {
	t.Helper()
	if _, err := st.Update(model.ProviderZai, idOrName, func(a *model.Account) {
		a.Quota = map[string]map[string]any{
			"col": {"model": "GLM-5.3", "remaining": remaining, "available": remaining},
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLineTruncateStrikesPurgeLineAndReassign：同一线路连续 3 次上游侧断流后，
// 线路必须被移除、绑定账号被改派到幸存线路——2026-09-22 事故里 3 连切全部落在
// mihomo-zai-024 同一账号同一线路（trunc_total 1→2→3），当时没有任何机制止损。
func TestLineTruncateStrikesPurgeLineAndReassign(t *testing.T) {
	up := &cutUp{abortN: map[int]bool{1: true, 2: true, 3: true}}
	f := newDiagFixture(t, up)
	acc, err := f.st.AddAccount(model.ProviderZai, "strike-acct", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}
	badID, goodID := bindToLine(t, f.st, acc, f.ups.URL)
	if err := f.st.SetSetting(store.LineTruncateStrikesKey, "3"); err != nil {
		t.Fatal(err)
	}
	// 单账号池不会被回避饿死（全池被回避则不过滤），但这里显式关掉以专注熔断语义。
	if err := f.st.SetSetting(store.LineTruncateAvoidSecondsKey, "0"); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 3; i++ {
		if status, _ := f.post(t, msgBody()); status != http.StatusOK {
			t.Fatalf("第 %d 次请求应已交付 200: %d", i, status)
		}
		if i < 3 && !hasProfile(f.st, badID) {
			t.Fatalf("未达阈值（%d<3）线路不应被移除", i)
		}
	}
	if hasProfile(f.st, badID) {
		t.Fatal("连续 3 次断流后线路应被移除")
	}
	if !hasProfile(f.st, goodID) {
		t.Fatal("幸存线路不应被误删")
	}
	live := f.st.Find(model.ProviderZai, acc.ID)
	if live.ProxyID == nil || *live.ProxyID != goodID {
		t.Fatalf("绑定账号应被改派到幸存线路，实际 proxy_id=%v", live.ProxyID)
	}
	if live.StreamTruncateCount != 3 {
		t.Fatalf("账号累计断流应为 3，实际 %d", live.StreamTruncateCount)
	}
	logs := stripANSI(f.buf.String())
	for _, want := range []string{"line-guard", "bad-line", "已移除", "改派 1 个账号"} {
		if !strings.Contains(logs, want) {
			t.Errorf("line-guard 日志缺少 %q\n日志=\n%s", want, logs)
		}
	}
	// 移除后计数条目一并清理。
	if _, ok := f.st.LineTruncateStats()[badID]; ok {
		t.Fatal("被移除线路的计数条目应被清理")
	}
}

// TestLineTruncateStrikesZeroDisables：阈值 0 = 关闭熔断——只计数不动线路。
func TestLineTruncateStrikesZeroDisables(t *testing.T) {
	up := &cutUp{abortN: map[int]bool{1: true, 2: true, 3: true, 4: true}}
	f := newDiagFixture(t, up)
	acc, err := f.st.AddAccount(model.ProviderZai, "off-acct", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}
	badID, _ := bindToLine(t, f.st, acc, f.ups.URL)
	for _, kv := range [][2]string{
		{store.LineTruncateStrikesKey, "0"},
		{store.LineTruncateAvoidSecondsKey, "0"},
	} {
		if err := f.st.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		f.post(t, msgBody())
	}
	if !hasProfile(f.st, badID) {
		t.Fatal("阈值 0 时线路不应被移除")
	}
	if got := f.st.Find(model.ProviderZai, acc.ID).StreamTruncateCount; got != 4 {
		t.Fatalf("计数应照常累计（4），实际 %d", got)
	}
}

// TestLineTruncateSuccessResetsStreak：熔断判据是「无一次完整成功的连击」。第
// 1、2 次断流后插一次完整成功，连击必须归零——否则成功反而救不了好线路。
func TestLineTruncateSuccessResetsStreak(t *testing.T) {
	up := &cutUp{abortN: map[int]bool{1: true, 2: true, 4: true, 5: true}}
	f := newDiagFixture(t, up)
	acc, err := f.st.AddAccount(model.ProviderZai, "reset-acct", "sk-abc")
	if err != nil {
		t.Fatal(err)
	}
	badID, _ := bindToLine(t, f.st, acc, f.ups.URL)
	for _, kv := range [][2]string{
		{store.LineTruncateStrikesKey, "3"},
		{store.LineTruncateAvoidSecondsKey, "0"},
	} {
		if err := f.st.SetSetting(kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}

	// 断流 ×2（连击 2）→ 完整成功 ×1（归零）→ 断流 ×2（连击又是 2，不熔断）。
	for i := 0; i < 5; i++ {
		f.post(t, msgBody())
	}
	if !hasProfile(f.st, badID) {
		t.Fatal("插入成功复位后，2+2 次断流不应触发 3 连击熔断")
	}
	if got := f.st.Find(model.ProviderZai, acc.ID).StreamTruncateCount; got != 4 {
		t.Fatalf("账号累计断流应为 4（成功不计），实际 %d", got)
	}
}

// TestTruncateAvoidsAccountInSelection：断流后账号在回避期内暂不被选号——
// 2026-09-22 事故的直接止血点。用额度差构造「额度优先黏性」：没有回避机制时，
// 最富的账号会被逐次重新选中（这正是当时三次重试全撞同一账号的成因）。
func TestTruncateAvoidsAccountInSelection(t *testing.T) {
	up := &cutUp{abortN: map[int]bool{1: true}} // 仅首次断流，其余完整成功
	f := newDiagFixture(t, up)
	if _, err := f.st.AddAccount(model.ProviderZai, "avoid-a", "sk-aaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "avoid-b", "sk-bbb"); err != nil {
		t.Fatal(err)
	}
	// avoid-a 额度远高于 avoid-b：第 4 层「额度优先」会唯一选中它——回避是唯一
	// 能让第二次请求换号的机制（纯轮询救不了，两号额度不并列）。
	setQuota(t, f.st, "avoid-a", 1000)
	setQuota(t, f.st, "avoid-b", 100)
	if err := f.st.SetSetting(store.LineTruncateAvoidSecondsKey, "60"); err != nil {
		t.Fatal(err)
	}

	f.post(t, msgBody()) // 第 1 次：额度最富的 avoid-a 被选中并被上游掐断
	f.post(t, msgBody()) // 第 2 次：avoid-a 回避中，必须落到 avoid-b
	f.post(t, msgBody()) // 第 3 次：回避期内仍是 avoid-b

	lines := f.buf.diagLines(t)
	if len(lines) < 3 {
		t.Fatalf("应有三条诊断行，实际 %d", len(lines))
	}
	if got := fieldOf(lines[0], "acc="); got != "avoid-a" {
		t.Errorf("首次请求应由额度最富的 avoid-a 承接: %s", lines[0])
	}
	if !strings.Contains(lines[0], "trunc_total=1") {
		t.Errorf("首次请求应为上游侧断流: %s", lines[0])
	}
	if got := fieldOf(lines[1], "acc="); got != "avoid-b" {
		t.Errorf("断流后紧接的请求必须换号（回避生效），实际 %s\n%s", got, lines[1])
	}
	if got := fieldOf(lines[2], "acc="); got != "avoid-b" {
		t.Errorf("回避期内应继续使用 avoid-b: %s", lines[2])
	}
	if strings.Contains(lines[1], "trunc_total=1") || strings.Contains(lines[2], "trunc_total=1") {
		t.Errorf("后续请求不应再断流\n%s\n%s", lines[1], lines[2])
	}
}

// TestTruncateFiresQuotaRefresh：断流即刷新额度快照（fireRefresh）——断流账号的
// 额度读数停在旧值，会以「幽灵最富」持续黏住额度优先调度。
func TestTruncateFiresQuotaRefresh(t *testing.T) {
	st := openStore(t)
	eng := NewEngine(st, nil, nil)
	var fired atomic.Int32
	eng.OnQuotaRefresh = func(acc *model.Account) { fired.Add(1) }
	// fireRefresh 只对 JWT 账号生效（额度接口是 JWT 侧的）。
	acc, err := st.AddAccount(model.ProviderZai, "refresh-acct", "header.payload.signature")
	if err != nil {
		t.Fatal(err)
	}
	usage := NewUsageCollector(true)

	// 客户端主动断开（ctx 结束）：不记断流、不触发刷新。
	ctxCancel, cancel := context.WithCancel(context.Background())
	cancel()
	eng.finishDelivery(ctxCancel, "req-cancel", acc, usage, errors.New("context canceled"), nil)
	if fired.Load() != 0 {
		t.Fatalf("客户端侧断开不应触发刷新: %d", fired.Load())
	}
	// 上游侧掐断（ctx 存活）：必须触发刷新（fireRefresh 是 go 出去的异步钩子，轮询等待）。
	eng.finishDelivery(context.Background(), "req-cut", acc, usage, errors.New("unexpected EOF"), nil)
	deadline := time.Now().Add(2 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fired.Load(); got != 1 {
		t.Fatalf("上游侧断流应触发一次刷新: %d", got)
	}
}

// fieldOf 从 "k=v" 空格分隔的字段串里取值（诊断行断言的小工具）。
func fieldOf(line, prefix string) string {
	for _, part := range strings.Fields(line) {
		if strings.HasPrefix(part, prefix) {
			return strings.TrimPrefix(part, prefix)
		}
	}
	return ""
}
