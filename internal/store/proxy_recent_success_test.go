package store

import (
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	return buf
}

func waitForLog(t *testing.T, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("日志里没有出现 %q：%s", want, buf.String())
}

func TestAddProxyProfileLogsImport(t *testing.T) {
	s := newTestStore(t)
	buf := captureLog(t)
	if _, err := s.AddProxyProfile("手工线路", "http://127.0.0.1:18500", true); err != nil {
		t.Fatal(err)
	}
	// 日志在锁外异步输出：删除有 proxy-health 行，导入也必须有可对账的一行。
	// 断言按字段拆开：`web.Warn` 会在模块名与正文之间插入 ANSI 复位序列，
	// 整段子串匹配会假失败。
	waitForLog(t, buf, "proxy-import")
	waitForLog(t, buf, "source=manual")
	waitForLog(t, buf, "total=1")
}

// setLastUsed 模拟一次上游成功交付（MarkSuccess 写的就是这个字段）。
func setLastUsed(t *testing.T, s *Store, a *model.Account, at time.Time) {
	t.Helper()
	ts := float64(at.UnixNano()) / 1e9
	if _, err := s.Update(a.Provider, a.ID, func(live *model.Account) { live.LastUsedAt = &ts }); err != nil {
		t.Fatal(err)
	}
}

func enableAutoRetire(t *testing.T, s *Store) {
	t.Helper()
	policy := s.ModelProbeSettings()
	policy.Mode = "auto_retire"
	if _, err := s.SaveModelProbeSettings(policy, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRecentSuccessProtectsFromHealthRemoval(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "在用线路", 18501, true)
	a := addSelectionAccount(t, s, "在用账号", p.ID)
	enableAutoRetire(t, s)
	setLastUsed(t, s, a, time.Now().Add(-time.Hour))

	removed, _, err := s.ApplyNetworkHealth(t.Context(), s.ModelProbeSettings(), s.ListProxyProfiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("最近成功过的线路不应被巡检删除：%v", removed)
	}
	if prot := s.ProxyAutoDeleteProtections(); prot[p.ID] != "recent_success" {
		t.Fatalf("后台应能解释为什么没删：%v", prot)
	}
	if left := len(s.ListProxyProfiles()); left != 1 {
		t.Fatalf("线路不应消失：%d", left)
	}
}

func TestRecentSuccessExpires(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "过期线路", 18502, true)
	a := addSelectionAccount(t, s, "过期账号", p.ID)
	enableAutoRetire(t, s)
	setLastUsed(t, s, a, time.Now().Add(-25*time.Hour))

	removed, _, err := s.ApplyNetworkHealth(t.Context(), s.ModelProbeSettings(), s.ListProxyProfiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != p.ID {
		t.Fatalf("超过窗口应正常删除：%v", removed)
	}
}

func TestRecentSuccessDisabledByConfig(t *testing.T) {
	old := config.ProxyProtectRecentHours
	config.ProxyProtectRecentHours = 0
	t.Cleanup(func() { config.ProxyProtectRecentHours = old })

	s := newTestStore(t)
	p := addSelectionProxy(t, s, "关闭保护", 18503, true)
	a := addSelectionAccount(t, s, "关闭账号", p.ID)
	enableAutoRetire(t, s)
	setLastUsed(t, s, a, time.Now().Add(-time.Hour))

	removed, _, err := s.ApplyNetworkHealth(t.Context(), s.ModelProbeSettings(), s.ListProxyProfiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("窗口为 0 时应回到旧行为：%v", removed)
	}
}

// 最近成功只挡巡检删除，不能否决严格退役：脏线不能靠「有账号在上面成功过」拿到免死金牌。
func TestRecentSuccessDoesNotBlockStrictRetirement(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "脏线", 18504, true)
	a := addSelectionAccount(t, s, "脏线账号", p.ID)
	enableAutoRetire(t, s)
	setLastUsed(t, s, a, time.Now().Add(-time.Minute))

	if reason := s.automaticProxyRemovalReasonLocked(strictProxyIdentity(p.URL)); reason != "" {
		t.Fatalf("严格退役不得被最近成功否决：%s", reason)
	}
}
