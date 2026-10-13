// Package proxysub 实现独立代理仓库的订阅接收与入库复检，不使用账号凭据。
package proxysub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	StateKey             = "proxy_subscription_v1"
	SourceID             = "proxy-checker"
	MaxResponse          = 2 << 20
	MaxItems             = 1000
	MaxHistory           = 50
	MaxBlocks            = 2000
	ProbeTarget          = "https://zcode.z.ai/api/v1/client/configs"
	HistoryReferenceOnly = "reference_only"
)

var (
	ErrBusy     = errors.New("已有订阅任务正在运行")
	ErrConflict = errors.New("订阅配置已变化，请刷新后重试")
	ErrState    = errors.New("订阅状态不可用")
)

type Config struct {
	Revision        int64  `json:"revision"`
	URL             string `json:"url"`
	Auto            bool   `json:"auto"`
	IntervalMinutes int    `json:"interval_minutes"`
	// Deprecated: 仅保留 v2.10.2 配置/客户端格式兼容，不参与筛选、排序或提交判定。
	MaxAgeMinutes  int  `json:"max_age_minutes"`
	Candidates     int  `json:"candidates"`
	ImportLimit    int  `json:"import_limit"`
	Capacity       int  `json:"capacity"`
	Concurrency    int  `json:"concurrency"`
	BudgetSeconds  int  `json:"budget_seconds"`
	EnableImported bool `json:"enable_imported"`
}

func DefaultConfig() Config {
	return Config{IntervalMinutes: 10, MaxAgeMinutes: 120, Candidates: 50, ImportLimit: 10, Capacity: 50, Concurrency: 4, BudgetSeconds: 180}
}

func (c Config) Validate() error {
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(c.URL) > 4096 {
			return errors.New("订阅地址必须是有效 HTTP/HTTPS 地址，不允许认证信息或片段")
		}
	}
	if c.Auto && c.URL == "" {
		return errors.New("开启自动同步前需要配置订阅地址")
	}
	for _, r := range []struct {
		name        string
		n, min, max int
	}{
		{"同步间隔", c.IntervalMinutes, 5, 1440}, {"结果有效期", c.MaxAgeMinutes, 5, 1440},
		{"候选上限", c.Candidates, 1, 500}, {"新增上限", c.ImportLimit, 1, 100},
		{"来源池容量", c.Capacity, 1, 500}, {"检测并发", c.Concurrency, 1, 8},
		{"任务预算", c.BudgetSeconds, 30, 900},
	} {
		if r.n < r.min || r.n > r.max {
			return errors.New(r.name + "超出允许范围")
		}
	}
	if c.ImportLimit > c.Candidates || c.ImportLimit > c.Capacity {
		return errors.New("新增上限不能超过候选上限或来源池容量")
	}
	return nil
}

// URL 的路径也可能包含 token，对外仅展示 origin。
func (c Config) Public() map[string]any {
	origin := ""
	if u, err := url.Parse(c.URL); err == nil && u.Host != "" {
		origin = u.Scheme + "://" + u.Host + "/…"
	}
	return map[string]any{
		"revision": c.Revision, "configured": c.URL != "", "source_display": origin,
		"auto": c.Auto, "interval_minutes": c.IntervalMinutes, "max_age_minutes": c.MaxAgeMinutes,
		"candidates": c.Candidates, "import_limit": c.ImportLimit, "capacity": c.Capacity,
		"concurrency": c.Concurrency, "budget_seconds": c.BudgetSeconds, "enable_imported": c.EnableImported,
	}
}

type Candidate struct {
	URL      string `json:"proxy"`
	Key      string `json:"fingerprint"`
	LastOK   int64  `json:"last_ok"`
	Streak   int    `json:"pass_streak"`
	IP       string `json:"ip,omitempty"`
	Shared   bool   `json:"ip_shared,omitempty"`
	Latency  int64  `json:"latency,omitempty"`
	ProbedAt int64  `json:"probed_at,omitempty"`
}

type Origin struct {
	Source     string `json:"source"`
	Key        string `json:"fingerprint"`
	ImportedAt int64  `json:"imported_at"`
	LastOK     int64  `json:"last_ok"`
	ProbedAt   int64  `json:"probed_at"`
	IP         string `json:"ip,omitempty"`
}

type Block struct {
	Key       string `json:"fingerprint"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"created_at"`
	Until     int64  `json:"until"` // 0 为人工禁止，必须显式解除。
}

func (b Block) Active(now time.Time) bool { return b.Until == 0 || now.UnixMilli() < b.Until }

type Detail struct {
	Key      string `json:"fingerprint"`
	Endpoint string `json:"endpoint"`
	OK       bool   `json:"ok"`
	Reason   string `json:"reason"`
	MS       int64  `json:"ms"`
}

type Run struct {
	ID            string   `json:"id"`
	Revision      int64    `json:"revision"`
	Mode          string   `json:"mode"`
	Automatic     bool     `json:"automatic"`
	Status        string   `json:"status"`
	HistoryPolicy string   `json:"history_policy,omitempty"`
	StartedAt     int64    `json:"started_at"`
	FinishedAt    int64    `json:"finished_at"`
	Error         string   `json:"error,omitempty"`
	Received      int      `json:"received"`
	Rejected      int      `json:"rejected"`
	Expired       int      `json:"expired"`
	Existing      int      `json:"existing"`
	Blocked       int      `json:"blocked"`
	Candidates    int      `json:"candidates"`
	Probed        int      `json:"probed"`
	Passed        int      `json:"passed"`
	Added         int      `json:"added"`
	CommitSkipped int      `json:"commit_skipped"`
	Details       []Detail `json:"details"`
}

func (r Run) Terminal() bool {
	return r.Status == "completed" || r.Status == "failed" || r.Status == "canceled" || r.Status == "interrupted"
}

type State struct {
	Version      int               `json:"version"`
	Config       Config            `json:"config"`
	Registry     map[string]Origin `json:"registry"`
	Blocks       map[string]Block  `json:"blocks"`
	Runs         []Run             `json:"runs"`
	ImportPaused bool              `json:"import_paused"` // 禁止记录预算耗尽时闭锁导入，不能静默遗忘人工意图。
}

func NewState() State {
	return State{Version: 1, Config: DefaultConfig(), Registry: map[string]Origin{}, Blocks: map[string]Block{}, Runs: []Run{}}
}

type Existing struct {
	ID, URL string
	Enabled bool
}
type Snapshot struct {
	State    State
	Profiles []Existing
}

func (s Snapshot) OwnedCount() int {
	n := 0
	for _, p := range s.Profiles {
		if o, ok := s.State.Registry[p.ID]; ok && o.Key == Fingerprint(p.URL) {
			n++
		}
	}
	return n
}

// Repository 的提交接缝同时重检配置、人工变更及容量，并原子保存终态与线路。
type Repository interface {
	SubscriptionSnapshot() (Snapshot, error)
	SaveSubscriptionConfig(Config) (Config, error)
	BeginSubscriptionRun(Run) error
	FinishSubscriptionRun(context.Context, Run, []Candidate) (Run, error)
	RecoverSubscriptionRuns() error
	UnblockSubscription(string) error
}

// Canonical 不改变协议/DNS 语义，亦不改写历史配置。
func Canonical(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("代理地址格式不支持")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "socks4", "socks5", "socks5h":
	default:
		return "", errors.New("代理协议不支持")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || ip.Zone() != "" {
		return "", errors.New("仅支持 IP 字面量代理")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("代理端口无效")
	}
	return scheme + "://" + net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(port)), nil
}

func Fingerprint(raw string) string {
	canonical, err := Canonical(raw)
	if err != nil {
		canonical = strings.TrimSpace(raw)
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

func PublicEndpoint(raw string) (string, error) {
	canonical, err := Canonical(raw)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(canonical)
	ip, _ := netip.ParseAddr(u.Hostname())
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		(ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return "", errors.New("不允许非公网代理")
	}
	for _, prefix := range reserved {
		if prefix.Contains(ip) {
			return "", errors.New("不允许保留地址代理")
		}
	}
	return canonical, nil
}

// FreshLocalProbe 只约束本机复检结果的提交时效，不检查 PRX 的历史成功时间。
func FreshLocalProbe(ms int64, now time.Time) bool {
	return ms > 0 && ms >= now.Add(-5*time.Minute).UnixMilli() && ms <= now.Add(5*time.Minute).UnixMilli()
}
