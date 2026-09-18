// 代理线路端点与探测。
//
// 探测只验证一件事：经这条线路能不能连上 z.ai。之所以不再做 ip.sb 之类的出口
// 查询——出口 IP / ASN 只能证明「线路活着、落地在哪」，证明不了「z.ai 接受这个
// 出口」；被边缘拦截、被代理按域名白名单挡住都测不出来，而后者才决定这条线路
// 对本项目有没有用。详见 upstreamProbeTargets / probeUpstream。
package adminapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

func (h *Handler) handleListProxies(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"profiles": h.Store.ListProxyProfiles()})
}

func (h *Handler) handleAddProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	profile, err := h.Store.AddProxyProfile(
		strOf(firstTruthy(payload["name"])),
		strOf(firstTruthy(payload["url"])),
		payloadEnabled(payload),
	)
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

func (h *Handler) handleUpdateProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	profile, err := h.Store.UpdateProxyProfile(
		r.PathValue("profile_id"),
		strOf(firstTruthy(payload["name"])),
		strOf(firstTruthy(payload["url"])),
		payloadEnabled(payload),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, errNotFound("代理配置不存在"))
			return
		}
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// handleDeleteProxy 删除线路。原绑定该线路的账号会被自动改派到其它空閒线路，
// 确实没有空閒线路时才退回直连；响应回报两种处置各覆盖多少个账号。
func (h *Handler) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	ok, reassign, err := h.Store.DeleteProxyProfile(r.PathValue("profile_id"))
	if err != nil {
		writeError500(w, err)
		return
	}
	if !ok {
		writeAPIError(w, errNotFound("代理配置不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"reassigned":      len(reassign.Assigned),
		"direct_fallback": len(reassign.Direct),
	})
}

func (h *Handler) handleAssignProxy(w http.ResponseWriter, r *http.Request) {
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	accountID := strings.TrimSpace(strOf(firstTruthy(payload["account_id"])))
	if accountID == "" {
		writeAPIError(w, errBadRequest("缺少帳號 ID"))
		return
	}
	profileID := strOf(firstTruthy(payload["proxy_id"]))
	ok, err := h.Store.AssignProxyProfile(accountID, profileID)
	if err != nil {
		if errors.Is(err, store.ErrProxyNotFound) {
			writeAPIError(w, errBadRequest(err.Error()))
			return
		}
		writeError500(w, err)
		return
	}
	if !ok {
		writeAPIError(w, errNotFound("帳號不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleTestCurrentProxy(w http.ResponseWriter, r *http.Request) {
	h.writeProbe(w, "")
}

func (h *Handler) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	profileID := r.PathValue("profile_id")
	var target string
	for _, p := range h.Store.ListProxyProfiles() {
		if p.ID == profileID {
			target = p.URL
			break
		}
	}
	if target == "" {
		writeAPIError(w, errNotFound("代理配置不存在"))
		return
	}
	h.writeProbe(w, target)
}

// testAllConcurrency 批量探测的并发上限：单条最坏 12s（两个 z.ai 入口依次 6s
// 超时），取 8 与额度刷新的并发度一致；正常情况数百毫秒即返回。
const testAllConcurrency = 8

// handleTestAllProxies 并发探测全部「启用」的线路，逐条返回结果供页面逐行渲染。
//
// 刻意不落库（与单条检测一致）：可达性是瞬时观测值，缓存它反而会掩盖线路已经失效。
// 未启用的线路不参与——它们的线路本来就不会被账号使用。
//
// 结果里的 ok 是「线路可用」的判据（能否连上 z.ai），不是「探测请求本身成功」。
func (h *Handler) handleTestAllProxies(w http.ResponseWriter, r *http.Request) {
	profiles := []store.ProxyProfile{}
	for _, p := range h.Store.ListProxyProfiles() {
		if p.Enabled {
			profiles = append(profiles, p)
		}
	}

	// 每 goroutine 只写自己的下标，无需额外加锁。
	results := make([]map[string]any, len(profiles))
	sem := make(chan struct{}, testAllConcurrency)
	var wg sync.WaitGroup
	for i, p := range profiles {
		wg.Add(1)
		go func(i int, p store.ProxyProfile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			entry := map[string]any{"id": p.ID, "name": p.Name}
			info, err := h.probe(p.URL)
			if err != nil {
				entry["ok"] = false
				entry["error"] = err.Error()
				results[i] = entry
				return
			}
			entry["ok"] = true
			for k, v := range info {
				entry[k] = v
			}
			results[i] = entry
		}(i, p)
	}
	wg.Wait()

	okCount := 0
	for _, e := range results {
		if ok, _ := e["ok"].(bool); ok {
			okCount++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"summary": map[string]any{
			"total": len(results), "ok": okCount, "fail": len(results) - okCount,
		},
	})
}

func payloadEnabled(payload map[string]any) bool {
	if v, ok := payload["enabled"]; ok {
		return truthy(v)
	}
	return true
}

// writeProbe 执行 z.ai 可达性探测并写响应。连不上 z.ai 不算接口错误——那正是
// 探测要报告的结论，所以仍返回 200 + ok=false，让后台按线路不可用展示；只有
// 代理地址本身不可用（协议不支持等）才报 502。
func (h *Handler) writeProbe(w http.ResponseWriter, proxyURL string) {
	result, err := h.probe(proxyURL)
	if err != nil {
		writeAPIError(w, &apiError{
			status:  http.StatusBadGateway,
			message: fmt.Sprintf("线路不可用（%s）", errorTypeName(err)),
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// upstreamProbeTimeout z.ai 可达性探测的单目标超时：只发一个浅请求，正常应在
// 数百毫秒内拿到响应。
const upstreamProbeTimeout = 6 * time.Second

// blockedMarkers 典型的 CDN/WAF 拦截页特征：无凭据请求本应得到 401/403 这类
// 接口回应，命中这些标记说明拿到的是拦截页，而不是 z.ai 的应答。
var blockedMarkers = []string{
	"Just a moment", "Attention Required", "cf-chl-", "Enable JavaScript and cookies",
}

// upstreamProbeTargets z.ai 侧的真实入口：取各上游端点的 origin 去重。
//
// 探测 origin 而不是完整 API 路径：CDN/WAF 的放行与拒绝发生在 host 层，无凭据
// 请求打到具体路径并不增加信息，反而会在真实端点上留下无谓的调用记录。
var upstreamProbeTargets = defaultUpstreamProbeTargets()

func defaultUpstreamProbeTargets() []string {
	seen := map[string]bool{}
	targets := make([]string, 0, 3)
	for _, raw := range []string{
		config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase,
	} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			continue
		}
		origin := parsed.Scheme + "://" + parsed.Host + "/"
		if seen[origin] {
			continue
		}
		seen[origin] = true
		targets = append(targets, origin)
	}
	return targets
}

// probe 经指定出口验证 z.ai 侧入口是否可达，返回：
//
//	ok       线路可用于 z.ai（至少一个入口拿到非拦截响应）
//	error    不可用时的原因，直接给后台展示
//	upstream 逐入口明细（状态码 / 耗时 / 是否拦截页）
//
// 只有「连不上 z.ai」这一种结论。出口 IP 与 ASN 对判断可用性没有帮助，所以
// 不再查询；线路本身活没活，由请求能不能拿到响应体现。
func (h *Handler) probe(proxyURL string) (map[string]any, error) {
	client, err := newProbeClient(proxyURL)
	if err != nil {
		return nil, err
	}
	upstream := probeUpstream(client)
	result := map[string]any{"upstream": upstream}
	reachable, _ := upstream["ok"].(bool)
	result["ok"] = reachable
	if !reachable {
		result["error"] = upstreamReason(upstream)
	}
	return result, nil
}

// upstreamReason 把上游探测的结论压成一句给后台看的话。
func upstreamReason(upstream map[string]any) string {
	if msg, _ := upstream["error"].(string); msg != "" {
		return msg
	}
	if upstream["blocked"] == true {
		return "z.ai 返回边缘拦截页"
	}
	return "z.ai 无响应"
}

// probeUpstream 经指定出口逐个探测 upstreamProbeTargets，汇总为：
//
//	ok      至少一个目标拿到了非拦截响应 —— 线路可用于 z.ai
//	blocked 没有可用目标，但至少一个目标的响应像 CDN/WAF 拦截页
//	error   全部目标都没拿到响应（超时 / 连接重置 / DNS 失败 / 被代理拒绝）
//
// targets 保留逐个目标的明细，便于后台区分「主站不通」与「备援站不通」。
func probeUpstream(client *http.Client) map[string]any {
	report := map[string]any{"ok": false, "blocked": false, "error": "", "ms": 0}
	entries := make([]any, 0, len(upstreamProbeTargets))
	reachable, blocked := false, false
	firstErr := ""
	for _, target := range upstreamProbeTargets {
		entry := probeUpstreamTarget(client, target)
		entries = append(entries, entry)
		switch {
		case entry["ok"] == true && entry["blocked"] != true:
			if !reachable {
				report["ms"] = entry["ms"]
			}
			reachable = true
		case entry["blocked"] == true:
			blocked = true
		default:
			if firstErr == "" {
				firstErr, _ = entry["error"].(string)
			}
		}
	}
	report["targets"] = entries
	report["ok"] = reachable
	report["blocked"] = !reachable && blocked
	if !reachable && firstErr != "" {
		report["error"] = firstErr
	}
	return report
}

// probeUpstreamTarget 探测单个上游目标：拿到任何 HTTP 响应即 ok=true。
// 无凭据请求本就该被 z.ai 拒（401/403），能拿到状态码恰恰证明请求已到达它的边缘；
// 真正要区分的是「拿不到响应」与「拿到的其实是拦截页」。
func probeUpstreamTarget(client *http.Client, target string) map[string]any {
	entry := map[string]any{"url": target, "host": hostOfURL(target)}
	started := time.Now()
	// 用生产同款 UA：自造 UA 打在 z.ai 上可能被 WAF 直接拦掉，
	// 那会把「线路通、只是 UA 不招人待见」误报成「线路不通」。
	resp, err := probeGet(client, target)
	entry["ms"] = time.Since(started).Milliseconds()
	if err != nil {
		entry["ok"] = false
		entry["error"] = err.Error()
		return entry
	}
	defer resp.Body.Close()
	// 只读开头一段：够识别拦截页即可，不把整页 HTML 拉进来。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	entry["ok"] = true
	entry["status"] = resp.StatusCode
	entry["blocked"] = looksBlocked(resp, string(body))
	return entry
}

// looksBlocked 判断响应是不是 CDN/WAF 的拦截页，而不是 z.ai 的正常应答。
func looksBlocked(resp *http.Response, body string) bool {
	if resp.Header.Get("Cf-Mitigated") != "" {
		return true
	}
	for _, marker := range blockedMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return resp.StatusCode == http.StatusForbidden &&
		strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare")
}

// hostOfURL 取 URL 的 host，仅用于展示；解析失败时原样返回。
func hostOfURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return raw
	}
	return parsed.Host
}

// newProbeClient 构造探测客户端：超时 upstreamProbeTimeout、跟随重定向。
// 直连（未传代理）时显式置 Proxy=nil，与网关真实出站 defaultUpstreamClient
// 同语义——绝不跟随环境代理变量，否则在设置了 HTTP_PROXY 的环境里「测直连」
// 实际测到的是环境代理的出口，结论与生产直连不符。
// Go 标准库支持 http/https/socks5 代理；socks5 拨号即远程解析主机名，
// 与 socks5h 语义一致；socks4 无标准库支持，直接报错（呈 502 形态）。
func newProbeClient(proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if strings.TrimSpace(proxyURL) != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			socks := *u
			socks.Scheme = "socks5"
			transport.Proxy = http.ProxyURL(&socks)
		default:
			return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
		}
	}
	return &http.Client{Timeout: upstreamProbeTimeout, Transport: transport}, nil
}

// probeGet 发起探测用的 GET，带生产同款 UA：自造 UA 打在 z.ai 上可能被 WAF
// 直接拦掉，那会把「线路通、只是 UA 不招人待见」误报成「线路不通」。
func probeGet(client *http.Client, endpoint string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", config.UserAgent)
	return client.Do(req)
}

// errorTypeName 近似 Python 的 type(last_error).__name__：取错误链最深一层的类型名。
func errorTypeName(err error) string {
	for unwrapped := errors.Unwrap(err); unwrapped != nil; {
		err = unwrapped
		unwrapped = errors.Unwrap(err)
	}
	name := fmt.Sprintf("%T", err)
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimPrefix(name, "*")
}
