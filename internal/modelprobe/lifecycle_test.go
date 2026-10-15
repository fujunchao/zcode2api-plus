package modelprobe_test

import (
	"context"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

func TestModelProbeExpiredReferenceIsRecheckedBeforeTarget(t *testing.T) {
	f := newFixture(t)
	f.respond = func(route, account string, n int) (int, string) {
		if route == "target" {
			return 405, riskMessage
		}
		return 200, completeMessage
	}
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.now = time.UnixMilli(f.st.ModelProbeHistory(f.target.ID, 0, 1)[0].NextAt).Add(time.Millisecond)
	if _, err := f.m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(3 * time.Minute)
	if _, err := f.m.RunDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.st.ModelProbeHistory(f.target.ID, 0, 1)[0]
	if len(r.Samples) != 3 || r.Samples[2].Role != "reference" {
		t.Fatalf("过期参考必须先重新验证，不能继续打目标：%+v", r)
	}
}

func TestModelProbeDisablingCancelsQueuedRuns(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	p := f.st.ModelProbeSettings()
	p.Enabled = false
	if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
		t.Fatal(err)
	}
	r := f.st.ModelProbeHistory(f.target.ID, 0, 1)[0]
	if r.State != "interrupted" || r.Reason != "policy_changed" {
		t.Fatalf("关闭开关应立即结束未派发任务：%+v", r)
	}
	if f.st.Select(model.ProviderZai, nil, store.ModelProbeModel) != nil {
		t.Fatal("关闭开关不应释放探测专用角色")
	}
}

// 业务观察来自 Store 的公开接收契约；这里验证版本替换后的免测判断，而非拼造删除证据。
func TestModelProbeBusinessSuccessDoesNotSurviveRouteReplacement(t *testing.T) {
	f := newFixture(t)
	a, err := f.st.AddAccount(model.ProviderZai, "普通业务", "sk-synthetic-business")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AssignProxyProfile(a.ID, f.target.ID); err != nil {
		t.Fatal(err)
	}
	a = f.st.FindAny(a.ID)
	_, err = f.st.ObserveProxyOutcome(a, f.st.ProxyEgress(a), store.ProxyObservation{RequestID: "business-success", EntryRef: "entry-business", TargetOrigin: "http://model.invalid", TargetPath: "/v1/messages", Method: "POST", Model: store.ModelProbeModel, ContentRef: "content", ProfileRef: "profile", SessionRef: "session", DeviceRef: "device", Status: 200, Complete: true, At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.st.QueueModelProbe(f.target.ID, "scheduled", f.now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reason != "skipped_recent_success" {
		t.Fatalf("有效近期业务成功应该免测：%+v", r)
	}
	if _, err := f.st.UpdateProxyProfile(f.target.ID, "新线路", "http://127.0.0.1:18099", true); err != nil {
		t.Fatal(err)
	}
	r, err = f.st.QueueModelProbe(f.target.ID, "scheduled", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "queued" || r.Reason == "skipped_recent_success" {
		t.Fatalf("旧代理成功不能把新地址标绿：%+v", r)
	}
}

func TestModelProbeSSECompletionAllowsTrailingDoneButNotMissingID(t *testing.T) {
	for _, missingID := range []bool{false, true} {
		t.Run(map[bool]string{false: "完整消息", true: "缺消息ID"}[missingID], func(t *testing.T) {
			f := newFixture(t)
			f.contentType = map[string]string{"target": "text/event-stream"}
			id := `"id":"msg_sse",`
			if missingID {
				id = ""
			}
			body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{" + id + "\"role\":\"assistant\",\"usage\":{\"input_tokens\":3}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\ndata: [DONE]\n\n"
			f.respond = func(string, string, int) (int, string) { return 200, body }
			r := f.drive(t)
			if got := r.Verdict == "healthy_recent"; got == missingID {
				t.Fatalf("SSE 完整性判定错误：%+v", r)
			}
		})
	}
}
