package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"zcode2api/internal/config"
)

// errUplinkTooSlow 是本进程内的哨兵：本次尝试被上行速率闸门主动中断。
//
// 它必须与「首字节看门狗到点」区分开：看门狗表示「预算用完了，不知道还要多久」，
// 闸门表示「按实测速率算，剩余字节在预算内根本传不完」。两者的日志与终态不同，
// 判错方向会让运维以为是上游慢，而实际是这条线路的上行带宽不够。
var errUplinkTooSlow = errors.New("upstream uplink too slow")

// UplinkTooSlow 判断本次尝试是否被上行速率闸门中断。
//
// 必须在 watchdog.Fired() 之前判断：两者都会让 ctx 结束，但闸门是先 cancel 的那个，
// context.Cause 只保留第一个原因。
func UplinkTooSlow(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errUplinkTooSlow)
}

// uplinkMeter 包装请求体，按「已写字节 / 已耗时间」估算剩余上传时间。
//
// 为什么需要它：首字节看门狗只给整段 90s 预算，上传阶段没有独立判据。一条 19 KB/s
// 的线传 8.3 MiB 要 450 秒，必然超预算，但代码要等满 90 秒才知道（2026-10-18 日志：
// 8.34 MiB 请求三次尝试全部无首字节，240 秒后 503）。按实测速率外推，2 秒内即可判定。
//
// 语义边界：只判「明显传不完」。采样需同时满足最短时长与最小字节数，避免 TLS/首包
// 慢启动被误判；判据用**本次尝试的预算**（与看门狗同值），不是请求总预算——尝试本来
// 就会在预算处被掐断，拿总预算比等于永不触发。
type uplinkMeter struct {
	r      io.Reader
	total  int64
	budget time.Duration
	abort  func(error)
	minB   int64
	minDur time.Duration
	margin time.Duration
	now    func() time.Time

	mu      sync.Mutex
	started time.Time
	sent    int64
	kbps    float64
	aborted bool
}

func newUplinkMeter(r io.Reader, total int64, budget time.Duration, abort func(error), now func() time.Time) *uplinkMeter {
	if now == nil {
		now = time.Now
	}
	return &uplinkMeter{
		r: r, total: total, budget: budget, abort: abort,
		minB:   int64(config.UplinkGateMinBytes),
		minDur: time.Duration(config.UplinkGateMinMS) * time.Millisecond,
		margin: time.Duration(config.UplinkGateMarginSeconds) * time.Second,
		now:    now, started: now(),
	}
}

// Read 读取一块请求体，并在采样足够时判断是否需要放弃本次尝试。
func (m *uplinkMeter) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n <= 0 {
		return n, err
	}
	now := m.now()
	var fire bool
	var rate, eta float64

	m.mu.Lock()
	m.sent += int64(n)
	elapsed := now.Sub(m.started)
	if elapsed >= m.minDur && m.sent >= m.minB {
		secs := elapsed.Seconds()
		if secs > 0 {
			rate = float64(m.sent) / secs // bytes/s
			m.kbps = rate / 1024
		}
		remaining := m.total - m.sent
		if rate > 0 && remaining > 0 {
			eta = float64(remaining) / rate
			// 剩余上传时间已超过本次尝试还能用的时间：现在放弃，比等看门狗更省。
			if time.Duration(eta*float64(time.Second)) > m.budget-m.margin {
				m.aborted = true
				fire = true
			}
		}
	}
	m.mu.Unlock()

	if fire && m.abort != nil {
		m.abort(errUplinkTooSlow)
	}
	return n, err
}

// Close 透传底层关闭（http.Client 会在请求结束后关闭 Body）。
func (m *uplinkMeter) Close() error {
	if c, ok := m.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// RateKBps 返回实测上传速率（KB/s）；未采到样本时为 0。
func (m *uplinkMeter) RateKBps() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.kbps
}

// Aborted 表示本次尝试已被闸门中断。
func (m *uplinkMeter) Aborted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.aborted
}

// UplinkMeter 导出别名供 asyncpool 复用（与 FirstByteWatchdog 同一做法）：
// 两条请求路径对「传不完」必须给出同一判定。
type UplinkMeter = uplinkMeter

// AttachUplinkGate 给请求体装上行闸门；返回 nil 表示未启用或未达体量门槛。
//
// 只替换 req.Body，**不动 ContentLength**：net/http 只有在 body 是 *bytes.Reader /
// *strings.Reader / *bytes.Buffer 时才自动填 ContentLength，包装后若把它清零，
// 出站会退化成 Transfer-Encoding: chunked，字节形态偏离官方客户端。
func AttachUplinkGate(req *http.Request, budget time.Duration, abort func(error), now func() time.Time) *UplinkMeter {
	if req == nil || req.Body == nil || !config.UplinkGate {
		return nil
	}
	if req.ContentLength < int64(config.UplinkGateMinBytes) {
		return nil
	}
	m := newUplinkMeter(req.Body, req.ContentLength, budget, abort, now)
	req.Body = m
	return m
}

// uplinkRateKBps 取实测速率；未启用闸门（nil）时返回 0，供日志与诊断使用。
func uplinkRateKBps(m *uplinkMeter) float64 {
	if m == nil {
		return 0
	}
	return m.RateKBps()
}
