package asyncpool

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

func TestLegacyTicketAttributionMigratesOnceAndCannotReplayTerminal(t *testing.T) {
	p, _, _, _ := newTestPool(t)
	t.Cleanup(p.Close)
	tk := insertTicket(p, "legacy", map[string]any{
		"model": "GLM-5.3", "metadata": map[string]any{"user_id": `{"session_id":"sess_sess_legacy"}`},
	})
	_, first := p.claimTicket("legacy")
	if first.Version != upstream.AttributionSnapshotVersion || first.SessionID != "sess_legacy" {
		t.Fatal("旧票据缺省快照应惰性迁移且只规范化一次")
	}
	// 仅模拟尚未派发的内部重排；正式执行中的重试由同一个生产者完成。
	p.mu.Lock()
	tk.status = "pending"
	p.mu.Unlock()
	_, second := p.claimTicket("legacy")
	if first != second {
		t.Fatal("重排不能因缺少入站头而重建逻辑身份")
	}
	a, err := first.Restore()
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.Restore()
	if err != nil || a.RequestID == b.RequestID {
		t.Fatal("重排后的实际尝试仍需更新 request ID")
	}
	if !p.emit(context.Background(), "legacy", ticketEvent{Type: "done"}) {
		t.Fatal("完成事件应能入队")
	}
	if claimed, _ := p.claimTicket("legacy"); claimed != nil {
		t.Fatal("终态票据不能重新进入派发状态")
	}
}

func TestConcurrentLegacyTicketClaimOnlyCreatesOneProducer(t *testing.T) {
	p, _, _, _ := newTestPool(t)
	t.Cleanup(p.Close)
	tk := insertTicket(p, "concurrent-legacy", map[string]any{"model": "GLM-5.3"})
	var count atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if claimed, _ := p.claimTicket("concurrent-legacy"); claimed != nil {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 || tk.attribution == nil || tk.attribution.Version != upstream.AttributionSnapshotVersion {
		t.Fatal("首次并发领取必须原子创建唯一身份和唯一生产者")
	}
}

func TestUnknownTicketAttributionVersionStopsBeforeUpstream(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	t.Cleanup(p.Close)
	addJWTAccount(t, st, "version-fixture")
	up := &scriptedUpstream{}
	config.UpstreamZai = up.start(t).URL
	tk := insertTicket(p, "future-version", map[string]any{"model": "GLM-5.3"})
	snapshot := upstream.NewAttributionSnapshot(nil, "synthetic-session")
	snapshot.Version++
	tk.attribution = &snapshot
	p.processTicket(context.Background(), "future-version")
	events := drainEvents(tk)
	if len(events) != 1 || events[0].Type != "error" || events[0].Data.(map[string]any)["error"].(map[string]any)["type"] != "invalid_attribution_snapshot" || up.callCount() != 0 {
		t.Fatalf("未知快照版本不能静默生成新身份：events=%v calls=%d", events, up.callCount())
	}
}

func TestCanceledOrCompletedTicketDoesNotExecuteAgain(t *testing.T) {
	p, st, _, _ := newTestPool(t)
	t.Cleanup(p.Close)
	addJWTAccount(t, st, "terminal-fixture")
	up := &scriptedUpstream{specs: []upstreamSpec{{status: http.StatusOK, lines: []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`data: {"type":"message_stop"}`,
	}}}}
	config.UpstreamZai = up.start(t).URL
	body := map[string]any{"model": "GLM-5.3", "messages": []any{}}
	tk := insertTicket(p, "canceled", body)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.processTicket(ctx, "canceled")
	p.processTicket(context.Background(), "canceled")
	if up.callCount() != 0 || len(drainEvents(tk)) != 0 {
		t.Fatal("取消票据不能通过重新领取发起请求")
	}
	tk = insertTicket(p, "completed", body)
	p.processTicket(context.Background(), "completed")
	p.processTicket(context.Background(), "completed")
	if up.callCount() != 1 {
		t.Fatal("已交付票据再次调用生产者不能导致重复上游执行")
	}
	done := 0
	for _, event := range drainEvents(tk) {
		if event.Type == "done" {
			done++
		}
	}
	if done != 1 {
		t.Fatal("成功票据只能产生一个终态")
	}
}
