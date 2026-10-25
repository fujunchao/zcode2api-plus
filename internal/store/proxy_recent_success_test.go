package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxysub"
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

// 窗口必须真的按配置生效：显式设 6 小时，5 小时前成功→保留，7 小时前成功→删除。
// 默认值从 24 调整到 6 后，这条用例守住「配置值 = 判定值」不被日志文案或别处常量带偏。
func TestRecentSuccessUsesConfiguredWindow(t *testing.T) {
	old := config.ProxyProtectRecentHours
	config.ProxyProtectRecentHours = 6
	t.Cleanup(func() { config.ProxyProtectRecentHours = old })

	s := newTestStore(t)
	kept := addSelectionProxy(t, s, "窗口内线路", 18507, true)
	keptAccount := addSelectionAccount(t, s, "窗口内账号", kept.ID)
	expired := addSelectionProxy(t, s, "超窗线路", 18508, true)
	expiredAccount := addSelectionAccount(t, s, "超窗账号", expired.ID)
	enableAutoRetire(t, s)
	setLastUsed(t, s, keptAccount, time.Now().Add(-5*time.Hour))
	setLastUsed(t, s, expiredAccount, time.Now().Add(-7*time.Hour))

	removed, _, err := s.ApplyNetworkHealth(t.Context(), s.ModelProbeSettings(), s.ListProxyProfiles())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != expired.ID {
		t.Fatalf("6 小时窗口应只保留窗口内的线路，实际删除：%v", removed)
	}
	if prot := s.ProxyAutoDeleteProtections(); prot[kept.ID] != "recent_success" {
		t.Fatalf("窗口内线路应显示 recent_success：%v", prot)
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

// 断流熔断走 PurgeFailedAccountProxy → removeProxiesLocked，只受基础保护约束。
//
// 这是收窄方案的结构守门：若有人把「最近成功」过滤从 purgeProxyProfilesIfCurrentLocked
// 「顺手统一」挪进 removeProxiesLocked / removeProxiesAvoidingLocked，上面那条严格退役
// 用例仍然是绿的（它走 automaticProxyRemovalReasonLocked），只有这条会红。
func TestRecentSuccessDoesNotBlockTruncatePurge(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "断流线路", 18505, true)
	a := addSelectionAccount(t, s, "断流账号", p.ID)
	enableAutoRetire(t, s)
	setLastUsed(t, s, a, time.Now().Add(-time.Minute))

	removed, _, err := s.PurgeFailedAccountProxy(s.FindAny(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("断流熔断必须能删除线路：最近成功只豁免巡检，不得豁免断流")
	}
	if left := len(s.ListProxyProfiles()); left != 0 {
		t.Fatalf("线路未被删除：%d", left)
	}
}

// 订阅导入与 proxy-health 的 removed=N 对账依赖这一行；没有它收支永远对不上。
func TestSubscriptionImportLogsForReconciliation(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)
	buf := captureLog(t)

	r := subscriptionRun(t, s, c, "import-log")
	result, err := s.FinishSubscriptionRun(context.Background(), r, []proxysub.Candidate{subscriptionCandidate(0)})
	if err != nil || result.Added != 1 {
		t.Fatalf("导入结果 %+v %v", result, err)
	}
	waitForLog(t, buf, "proxy-import")
	waitForLog(t, buf, "source=import-log")
	waitForLog(t, buf, "added=1")
	waitForLog(t, buf, "total=1")
}

// 一条没新增时不打日志：自动同步每 15 分钟一次，空导入刷屏会淹掉真正的变更。
func TestSubscriptionImportSilentWhenNothingAdded(t *testing.T) {
	s := newTestStore(t)
	c := subscriptionFixture(t, s)

	// 先把手工新增那条异步日志等出来再换缓冲，否则它会飘进后面的断言。
	manual := captureLog(t)
	if _, err := s.AddProxyProfile("已有线路", subscriptionCandidate(0).URL, false); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, manual, "proxy-import")

	buf := captureLog(t)
	r := subscriptionRun(t, s, c, "import-empty")
	if _, err := s.FinishSubscriptionRun(context.Background(), r, []proxysub.Candidate{subscriptionCandidate(0)}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "proxy-import") {
		t.Fatalf("没有新增线路时不应打导入日志：%s", got)
	}
}

// 保护原因只补空位：既是默认出口又最近成功过时，界面应继续显示 default_proxy——
// 否则「默认出口」这个更根本的原因会被一条瞬时证据盖掉。
func TestDefaultProxyReasonWinsOverRecentSuccess(t *testing.T) {
	s := newTestStore(t)
	p := addSelectionProxy(t, s, "默认出口", 18506, true)
	a := addSelectionAccount(t, s, "默认出口账号", p.ID)
	setLastUsed(t, s, a, time.Now().Add(-time.Minute))

	policy, err := s.NewAccountProxyPolicy()
	if err != nil {
		t.Fatal(err)
	}
	policy.Enabled = true
	policy.DefaultProxyID = p.ID
	if _, err := s.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if got := s.ProxyAutoDeleteProtections()[p.ID]; got != "default_proxy" {
		t.Fatalf("展示原因应保持 default_proxy，实际 %q", got)
	}
}
