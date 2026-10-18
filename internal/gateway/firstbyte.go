package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"
)

// errFirstByteTimeout 是本进程内的哨兵：只用来把「等不到首字节」与客户端取消区分开，
// 不参与上游错误分类（分类链只看 status / err_kind）。
var errFirstByteTimeout = errors.New("upstream first byte timeout")

// firstByteWatchdog 给「建连 + 上传 + 收到响应头」整段设预算。
//
// 为什么需要它：http.Transport 的 ResponseHeaderTimeout 是「**完整写完请求体之后**
// 才开始计时」。慢线路上 5MB 上传本身就要几百秒，计时器在那之后才启动，等于上传阶段
// 完全没有超时（2026-10-17：5.5MB 请求首字节 283–296 秒，用户盯着转圈 5 分钟）。
//
// 语义边界：只覆盖首字节之前。收到响应头就停表，流式交付继续用同一个 ctx，
// 长回答不受影响。
type firstByteWatchdog struct {
	ctx      context.Context
	cancel   context.CancelCauseFunc
	timer    *time.Timer
	fired    atomic.Bool
	identity string
}

func newFirstByteWatchdog(parent context.Context, timeout time.Duration, identity string) *firstByteWatchdog {
	ctx, cancel := context.WithCancelCause(parent)
	w := &firstByteWatchdog{ctx: ctx, cancel: cancel, identity: identity}
	if timeout > 0 {
		w.timer = time.AfterFunc(timeout, func() {
			w.fired.Store(true)
			cancel(errFirstByteTimeout)
		})
	}
	return w
}

// Context 是本次尝试实际使用的 ctx（保留父级的所有值）。
func (w *firstByteWatchdog) Context() context.Context { return w.ctx }

// Headers 在 client.Do 返回响应头时调用：停表，但不取消，流式交付继续。
func (w *firstByteWatchdog) Headers(*http.Response) {
	if w.timer != nil {
		w.timer.Stop()
	}
}

// Stop 在整次尝试结束时调用，释放计时器与 ctx。
func (w *firstByteWatchdog) Stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.cancel(nil)
}

// Fired 表示本次尝试是被首字节预算打断的（而不是客户端取消或普通连接失败）。
func (w *firstByteWatchdog) Fired() bool { return w.fired.Load() }

// Abort 用给定原因提前结束本次尝试（上行速率闸门用）。
//
// 刻意不置 fired：调用方必须能区分「预算到点、不知道还要多久」与「按实测速率算
// 剩余字节根本传不完」——两者的日志与终态不同。
func (w *firstByteWatchdog) Abort(cause error) {
	if cause != nil {
		w.cancel(cause)
	}
}

// Identity 返回本次出站线路的规范化身份，用于请求级回避集合。
func (w *firstByteWatchdog) Identity() string { return w.identity }

// FirstByteWatchdog 是首字节看门狗。导出类型别名供 asyncpool 复用——两条路径对
// 「首字节超时」必须用同一套语义，否则同一次慢线在 sync 侧换线、async 侧却算账号故障。
type FirstByteWatchdog = firstByteWatchdog

// NewFirstByteWatchdog 创建看门狗（导出包装，参数与行为同 newFirstByteWatchdog）。
func NewFirstByteWatchdog(parent context.Context, timeout time.Duration, identity string) *FirstByteWatchdog {
	return newFirstByteWatchdog(parent, timeout, identity)
}

// TimeoutStatus 判断上游状态码是否属于「边缘/代理超时」类。导出供 asyncpool 复用：
// 408/524 与具体账号无关，两条路径都应按「换线重试」处理，而不是记账号失败。
func TimeoutStatus(status int) bool { return timeoutStatus(status) }

// timeoutStatus 判断上游状态码是否属于「边缘/代理超时」类。
// 408 是代理侧请求超时，524 是边缘等待源站超时；两者都与具体账号无关。
func timeoutStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == 524
}
