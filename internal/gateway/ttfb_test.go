package gateway

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 首字节预算命中时：换下一个账号，且**不冷却被超时的账号**——慢的是线路，
// 24 个账号共享同一条慢线时逐个冷却会瞬间清空池子。
func TestFirstByteTimeoutSwitchesWithoutCooling(t *testing.T) {
	f := newFixture(t)
	slow, err := f.st.AddAccount(model.ProviderZai, "slow-line", "sk-slow")
	if err != nil {
		t.Fatal(err)
	}
	fast, err := f.st.AddAccount(model.ProviderZai, "fast-line", "sk-fast")
	if err != nil {
		t.Fatal(err)
	}
	old := config.UpstreamFirstByteTimeout
	config.UpstreamFirstByteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { config.UpstreamFirstByteTimeout = old })

	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			time.Sleep(400 * time.Millisecond)
		}
		return 200, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}

	status, body := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("换线后应成功，实际 %d %s", status, body)
	}
	if f.callCount() != 2 {
		t.Fatalf("应发生两次上游调用，实际 %d", f.callCount())
	}
	if got := f.st.Find(model.ProviderZai, slow.ID); got.Status == model.StatusCooling {
		t.Fatalf("首字节超时不得冷却账号：%s", got.Status)
	}
	if got := f.st.Find(model.ProviderZai, fast.ID); got.Status != model.StatusActive {
		t.Fatalf("承接重试的账号应保持可用：%s", got.Status)
	}
}

// 524 属于边缘/代理超时，与账号无关：换一条线路重试一次，预算用尽后才透传。
func TestUpstream524RetriesOnce(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.AddAccount(model.ProviderZai, "line-1", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "line-2", "sk-2"); err != nil {
		t.Fatal(err)
	}
	old := config.UpstreamTimeoutRetries
	config.UpstreamTimeoutRetries = 1
	t.Cleanup(func() { config.UpstreamTimeoutRetries = old })

	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			return 524, http.Header{"Content-Type": []string{"text/plain"}}, "origin timeout"
		}
		return 200, http.Header{"Content-Type": []string{"application/json"}}, okUpstreamJSON
	}
	status, body := f.post(t, msgBody(), "sk-test")
	if status != 200 {
		t.Fatalf("524 换线后应成功，实际 %d %s", status, body)
	}
	if f.callCount() != 2 {
		t.Fatalf("524 应换线重试一次，实际调用 %d 次", f.callCount())
	}
}

// 预算用尽后必须原样透传 524，不能无限重试。
func TestUpstream524StopsAfterBudget(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.AddAccount(model.ProviderZai, "line-1", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "line-2", "sk-2"); err != nil {
		t.Fatal(err)
	}
	old := config.UpstreamTimeoutRetries
	config.UpstreamTimeoutRetries = 1
	t.Cleanup(func() { config.UpstreamTimeoutRetries = old })

	f.respond = func(int, *http.Request) (int, http.Header, string) {
		return 524, http.Header{"Content-Type": []string{"text/plain"}}, "origin timeout"
	}
	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 524 {
		t.Fatalf("预算用尽后应透传 524，实际 %d", status)
	}
	if f.callCount() != 2 {
		t.Fatalf("只允许重试一次，实际调用 %d 次", f.callCount())
	}
}

// 关闭预算时，普通上游错误仍然原样透传（不引入额外换号）。
func TestFirstByteTimeoutDisabledKeepsOldBehavior(t *testing.T) {
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "line-1", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	old := config.UpstreamFirstByteTimeout
	config.UpstreamFirstByteTimeout = 0
	t.Cleanup(func() { config.UpstreamFirstByteTimeout = old })

	f.respond = func(int, *http.Request) (int, http.Header, string) {
		return 502, http.Header{"Content-Type": []string{"text/plain"}}, "bad gateway"
	}
	status, _ := f.post(t, msgBody(), "sk-test")
	if status != 502 {
		t.Fatalf("关闭预算后应透传上游错误，实际 %d", status)
	}
	if got := f.st.Find(model.ProviderZai, acc.ID); got == nil {
		t.Fatal("账号不应丢失")
	}
}
