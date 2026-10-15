package adminapi

import (
	"context"
	"slices"
	"zcode2api/internal/modelprobe"
	"zcode2api/internal/store"
)

// ProxyHealthScheduler 只保留旧 Go 调用兼容，不再拥有独立自动清理循环。
type ProxyHealthScheduler struct {
	manager *modelprobe.Manager
	ctx     context.Context
	stop    <-chan struct{}
}

func NewProxyHealthScheduler(st *store.Store, parents ...context.Context) *ProxyHealthScheduler {
	ctx := context.Background()
	if len(parents) > 0 {
		ctx = parents[0]
	}
	return &ProxyHealthScheduler{manager: modelprobe.NewWithNetworkTargets(ctx, st, func() []string { return slices.Clone(upstreamProbeTargets) }), ctx: ctx, stop: ctx.Done()}
}
func (s *ProxyHealthScheduler) Start() { s.manager.Start() }
func (s *ProxyHealthScheduler) Stop()  { s.manager.Close() }
func (s *ProxyHealthScheduler) runOnce(_ <-chan struct{}) {
	if s.manager.QueueNetwork("") == nil {
		_, _ = s.manager.RunNetworkDue(s.ctx)
	}
}
