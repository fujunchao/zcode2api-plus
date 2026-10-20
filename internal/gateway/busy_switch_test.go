package gateway

import (
	"net/http"
	"sync"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// busySwitchResponder 把「第一次见到的账号」固定判为 3009 并发准入，
// 其它账号正常成功。这样不依赖选号顺序：无论先选中哪个账号，
// 默认组只会在同一个账号上重试，实验组必然切到另一个账号。
type busySwitchResponder struct {
	mu   sync.Mutex
	bad  string
	keys []string
}

func (x *busySwitchResponder) handle(_ int, r *http.Request) (int, http.Header, string) {
	key := r.Header.Get("X-Api-Key")
	x.mu.Lock()
	x.keys = append(x.keys, key)
	if x.bad == "" {
		x.bad = key
	}
	bad := key == x.bad
	x.mu.Unlock()
	header := http.Header{"Content-Type": []string{"application/json"}}
	if bad {
		return http.StatusTooManyRequests, header, `{"code":3009,"msg":"model concurrency limit exceeded"}`
	}
	return http.StatusOK, header, okUpstreamJSON
}

func (x *busySwitchResponder) distinct() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	seen := map[string]bool{}
	for _, k := range x.keys {
		seen[k] = true
	}
	return len(seen)
}

func busySwitchFixture(t *testing.T) (*fixture, *busySwitchResponder) {
	t.Helper()
	f := newFixture(t)
	x := &busySwitchResponder{}
	f.respond = x.handle
	for _, name := range []string{"busy-a", "busy-b"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-"+name); err != nil {
			t.Fatal(err)
		}
	}
	return f, x
}

// 默认（开关 0）：保持「模型级准入、换号无用」的假设——只在同一账号上重试，
// 预算用尽后原样透传 429。
func TestBusySwitchDefaultRetriesSameAccount(t *testing.T) {
	old := config.BusySwitchAfter
	config.BusySwitchAfter = 0
	t.Cleanup(func() { config.BusySwitchAfter = old })

	f, x := busySwitchFixture(t)
	status, _ := f.post(t, msgBody(), "sk-test")
	if status != http.StatusTooManyRequests {
		t.Fatalf("默认组应透传 429，实际 %d", status)
	}
	if got := x.distinct(); got != 1 {
		t.Fatalf("默认组不应换号，实际用了 %d 个账号", got)
	}
	if f.callCount() != 3 {
		t.Fatalf("默认组应为 1 次 + 2 次原地重试，实际 %d 次", f.callCount())
	}
	for _, acc := range f.st.ListAccounts(model.ProviderZai) {
		if acc.Status != model.StatusActive || acc.CoolingUntil != nil {
			t.Fatalf("3009 不得冷却账号：%s %s", acc.Name, acc.Status)
		}
	}
}

// 实验组（开关 1）：原地重试 1 次后换号，第二个账号成功。
func TestBusySwitchAfterOneSwitchesAccount(t *testing.T) {
	old := config.BusySwitchAfter
	config.BusySwitchAfter = 1
	t.Cleanup(func() { config.BusySwitchAfter = old })

	f, x := busySwitchFixture(t)
	status, body := f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK {
		t.Fatalf("实验组应换号后成功，实际 %d %s", status, body)
	}
	if got := x.distinct(); got != 2 {
		t.Fatalf("实验组应使用两个账号，实际 %d 个", got)
	}
	for _, acc := range f.st.ListAccounts(model.ProviderZai) {
		if acc.Status != model.StatusActive || acc.CoolingUntil != nil {
			t.Fatalf("换号不得冷却账号：%s %s", acc.Name, acc.Status)
		}
	}
}
