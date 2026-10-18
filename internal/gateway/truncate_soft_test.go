package gateway

import (
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 断流对客户端可见且无法重试，必须进线路软失败统计：两次即隔离，
// 而不是等到 line-guard 的第 3 次不可逆删除（2026-10-18 日志实证）。
func TestUpstreamTruncateCountsAsLineSoftFailure(t *testing.T) {
	old := config.ProxySoftTruncate
	config.ProxySoftTruncate = true
	t.Cleanup(func() { config.ProxySoftTruncate = old })

	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "trunc", "sk-trunc")
	if err != nil {
		t.Fatal(err)
	}
	line, err := f.st.AddProxyProfile("trunc-line", "http://127.0.0.1:9", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AssignProxyProfile(acc.ID, line.ID); err != nil {
		t.Fatal(err)
	}
	live := f.st.Find(model.ProviderZai, acc.ID)
	egress := f.st.ProxyEgress(live)
	if egress.Mode != "proxy" || egress.ID == "" {
		t.Fatalf("前置失败：线路未绑定：%+v", egress)
	}

	now := time.Now()
	RecordUpstreamTruncate(f.st, live, now, egress)
	RecordUpstreamTruncate(f.st, live, now.Add(time.Second), egress)

	list := f.st.ProxySoftFailures()
	if len(list) != 1 {
		t.Fatalf("应产生一条软失败画像，实际 %d：%+v", len(list), list)
	}
	if list[0].LastSignal != store.ProxySignalUpstreamTruncate {
		t.Fatalf("信号应为 upstream_truncate：%+v", list[0])
	}
	if list[0].Strikes < 1 {
		t.Fatalf("连续两次断流应触发隔离：%+v", list[0])
	}
}

// 开关关闭时断流只走 line-guard，不进软失败统计。
func TestUpstreamTruncateSwitchOffSkipsSoftFailure(t *testing.T) {
	old := config.ProxySoftTruncate
	config.ProxySoftTruncate = false
	t.Cleanup(func() { config.ProxySoftTruncate = old })

	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "trunc-off", "sk-trunc-off")
	if err != nil {
		t.Fatal(err)
	}
	line, err := f.st.AddProxyProfile("trunc-off-line", "http://127.0.0.1:9", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AssignProxyProfile(acc.ID, line.ID); err != nil {
		t.Fatal(err)
	}
	live := f.st.Find(model.ProviderZai, acc.ID)
	egress := f.st.ProxyEgress(live)

	now := time.Now()
	RecordUpstreamTruncate(f.st, live, now, egress)
	RecordUpstreamTruncate(f.st, live, now.Add(time.Second), egress)

	if got := f.st.ProxySoftFailures(); len(got) != 0 {
		t.Fatalf("开关关闭时不应记账：%+v", got)
	}
}
