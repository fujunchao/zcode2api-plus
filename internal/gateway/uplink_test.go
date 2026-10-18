package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/config"
)

// timedReader 每次 Read 推进注入的时钟，用来在毫秒级测试里模拟「上传很慢」。
// 真实链路上 Read 之间阻塞的正是写 socket 的时间，所以这个模型与闸门的采样口径一致。
type timedReader struct {
	chunk int
	left  int
	step  time.Duration
	clock *time.Time
}

func (r *timedReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	*r.clock = r.clock.Add(r.step)
	n := len(p)
	if n > r.chunk {
		n = r.chunk
	}
	if n > r.left {
		n = r.left
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.left -= n
	return n, nil
}

func withUplinkGate(t *testing.T, minBytes, minMS, margin int) {
	t.Helper()
	oldB, oldMS, oldM, oldOn := config.UplinkGateMinBytes, config.UplinkGateMinMS, config.UplinkGateMarginSeconds, config.UplinkGate
	config.UplinkGateMinBytes, config.UplinkGateMinMS, config.UplinkGateMarginSeconds, config.UplinkGate = minBytes, minMS, margin, true
	t.Cleanup(func() {
		config.UplinkGateMinBytes, config.UplinkGateMinMS, config.UplinkGateMarginSeconds, config.UplinkGate = oldB, oldMS, oldM, oldOn
	})
}

// 8.34 MiB 在 19–40 KB/s 的线路上要几百秒：闸门必须在采样窗口后立刻放弃，
// 而不是等首字节看门狗到点（2026-10-18：三次尝试各约 90s，240s 后 503）。
func TestUplinkGateAbortsWhenBodyCannotFinish(t *testing.T) {
	withUplinkGate(t, 262144, 1000, 0)
	clock := time.Unix(1_800_000_000, 0)
	r := &timedReader{chunk: 32 << 10, left: 1 << 20, step: time.Second, clock: &clock}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	m := newUplinkMeter(r, int64(r.left), 10*time.Second, cancel, func() time.Time { return clock })

	_, _ = io.Copy(io.Discard, m)
	if !m.Aborted() {
		t.Fatal("32 KB/s 传 1 MiB 在 10s 预算内不可能完成，应被闸门中断")
	}
	if !UplinkTooSlow(ctx) {
		t.Fatal("中断原因应为 errUplinkTooSlow，而不是普通取消")
	}
	if rate := m.RateKBps(); rate < 30 || rate > 34 {
		t.Fatalf("实测速率应约 32 KB/s，实际 %.1f", rate)
	}
}

func TestUplinkGateAllowsFastBody(t *testing.T) {
	withUplinkGate(t, 262144, 1000, 0)
	clock := time.Unix(1_800_000_000, 0)
	r := &timedReader{chunk: 32 << 10, left: 1 << 20, step: 10 * time.Millisecond, clock: &clock}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	m := newUplinkMeter(r, int64(r.left), 10*time.Second, cancel, func() time.Time { return clock })

	n, _ := io.Copy(io.Discard, m)
	if m.Aborted() || UplinkTooSlow(ctx) {
		t.Fatal("3 MB/s 的线路不应被闸门中断")
	}
	if n != 1<<20 {
		t.Fatalf("应完整读完 1 MiB，实际 %d", n)
	}
}

func TestUplinkGateIgnoresSmallBody(t *testing.T) {
	withUplinkGate(t, 262144, 1000, 0)
	clock := time.Unix(1_800_000_000, 0)
	r := &timedReader{chunk: 8 << 10, left: 64 << 10, step: time.Second, clock: &clock}
	cancel := func(error) {}
	m := newUplinkMeter(r, int64(r.left), time.Second, cancel, func() time.Time { return clock })

	_, _ = io.Copy(io.Discard, m)
	if m.Aborted() {
		t.Fatal("低于体量门槛的请求不参与判定：慢启动不能代表线路带宽")
	}
}

// 包装必须保留 ContentLength：否则出站退化成 chunked，字节形态偏离官方客户端。
func TestAttachUplinkGateKeepsContentLength(t *testing.T) {
	withUplinkGate(t, 1, 0, 0)
	payload := bytes.Repeat([]byte("a"), 4096)
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/x", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	want := req.ContentLength
	if want != int64(len(payload)) {
		t.Fatalf("前置失败：NewRequest 应自动填 ContentLength，实际 %d", want)
	}
	m := AttachUplinkGate(req, time.Minute, func(error) {}, nil)
	if m == nil {
		t.Fatal("达到门槛时应安装闸门")
	}
	if req.ContentLength != want {
		t.Fatalf("包装后 ContentLength 被改写：%d → %d", want, req.ContentLength)
	}
	got, err := io.ReadAll(req.Body)
	if err != nil || len(got) != len(payload) {
		t.Fatalf("包装后应能完整读取请求体：len=%d err=%v", len(got), err)
	}
	if req.GetBody == nil {
		t.Fatal("GetBody 不应被清空")
	}
}

func TestAttachUplinkGateRespectsSwitchAndThreshold(t *testing.T) {
	withUplinkGate(t, 1024, 0, 0)
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/x", bytes.NewReader(make([]byte, 2048)))
	config.UplinkGate = false
	if m := AttachUplinkGate(req, time.Minute, func(error) {}, nil); m != nil {
		t.Fatal("开关关闭时不应安装闸门")
	}
	config.UplinkGate = true
	small, _ := http.NewRequest(http.MethodPost, "http://example.invalid/x", bytes.NewReader(make([]byte, 512)))
	if m := AttachUplinkGate(small, time.Minute, func(error) {}, nil); m != nil {
		t.Fatal("低于门槛时不应安装闸门")
	}
}
