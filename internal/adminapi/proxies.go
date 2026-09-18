// 代理线路端点与出口探测。对应 Python 版 admin_api.py 的代理段落：
// CRUD、指派，以及 ip.sb / ipwho.is / ipapi.is 三级备援的出口查询。
//
// 出口查询回答的是「线路活没活、出口是谁」，但对本项目还不够：真正决定一条
// 线路能不能用的是 z.ai 认不认这个出口。故另加一路 z.ai 侧入口可达性探测
// （见 upstreamProbeTargets / probeUpstream），两者并发执行后合并结果。
package adminapi

import (
	"encoding/csv"
	"encoding/json"
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

const probeUserAgent = "zcode2api-plus/2.0"

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

func (h *Handler) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	ok, err := h.Store.DeleteProxyProfile(r.PathValue("profile_id"))
	if err != nil {
		writeError500(w, err)
		return
	}
	if !ok {
		writeAPIError(w, errNotFound("代理配置不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

// testAllConcurrency 批量探测的并发上限：单条最坏 36s（出口查询三个服务依次
// 12s 超时；z.ai 可达性侧并发跑、单目标 6s，不叠加），取 8 与额度刷新的并发度
// 一致；正常情况首个服务几百毫秒即返回。
const testAllConcurrency = 8

// handleTestAllProxies 并发探测全部「启用」的线路，逐条返回结果供页面逐行渲染。
//
// 刻意不落库（与单条检测一致）：出口信息是瞬时观测值，缓存它反而会掩盖线路已经失效。
// 未启用的线路不参与——它们的线路本来就不会被账号使用。
//
// 结果里的 ok 是「线路可用」的判据（含 z.ai 可达），不是「探测请求本身成功」。
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

// writeProbe 执行出口查询与 z.ai 可达性探测并写响应；两路全失败时返回 502。
func (h *Handler) writeProbe(w http.ResponseWriter, proxyURL string) {
	result, err := h.probe(proxyURL)
	if err != nil {
		writeAPIError(w, &apiError{
			status: http.StatusBadGateway,
			message: fmt.Sprintf(
				"线路不可达：出口查询与 z.ai 连接均失败（%s）", errorTypeName(err)),
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ipProbeProviders 依次尝试的 IP 查询服务（顺序对齐 Python 版）。
var ipProbeProviders = []struct{ name, endpoint string }{
	{"ip.sb", "https://api.ip.sb/geoip"},
	{"ipwho.is", "https://ipwho.is/"},
	{"ipapi.is", "https://api.ipapi.is/"},
}

const (
	// probeTimeout 出口查询服务的单次超时（三级备援，故该侧最坏 36s）。
	probeTimeout = 12 * time.Second
	// upstreamProbeTimeout z.ai 可达性探测的单目标超时：只发一个浅请求，
	// 正常应在数百毫秒内拿到响应，所以比出口查询短一截。
	upstreamProbeTimeout = 6 * time.Second
)

// blockedMarkers 典型的 CDN/WAF 拦截页特征：无凭据请求本应得到 401/403 这类
// 接口回应，命中这些标记说明拿到的是拦截页，而不是 z.ai 的应答。
var blockedMarkers = []string{
	"Just a moment", "Attention Required", "cf-chl-", "Enable JavaScript and cookies",
}

// upstreamProbeTargets z.ai 侧的真实入口：取各上游端点的 origin 去重。
//
// 探测 origin 而不是完整 API 路径：CDN/WAF 的放行与拒绝发生在 host 层，无凭据
// 请求打到具体路径并不增加信息，反而会在真实端点上留下无谓的调用记录。
// 之所以必须打真实入口：IP 查询站只能证明「线路活着、出口是谁」，证明不了
// 「z.ai 接受这个出口」——被边缘拦截、被代理按域名白名单挡住都测不出来。
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

// probe 经指定出口查两件事。二者互不依赖，故并发执行——最坏耗时取单侧的上限
// （出口查询 36s），不随探测目标数叠加：
//  1. 出口公网信息（IP / ASN / 国家）：线路活没活、落地在哪；
//  2. z.ai 侧入口可达性：这条线路能不能真的用来跑 z.ai。
//
// 只要其一有结果就算探测成功：出口查询站被代理的域名白名单挡掉、而 z.ai 通
// 的情况确实存在（反之亦然），任何一种都不该直接判成「线路坏」。
func (h *Handler) probe(proxyURL string) (map[string]any, error) {
	client, err := newProbeClient(proxyURL)
	if err != nil {
		return nil, err
	}
	var (
		wg        sync.WaitGroup
		egress    map[string]any
		egressErr error
		upstream  map[string]any
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		egress, egressErr = fetchEgressInfo(client)
	}()
	go func() {
		defer wg.Done()
		upstream = probeUpstream(proxyURL)
	}()
	wg.Wait()

	result := map[string]any{"upstream": upstream}
	for key, value := range egress { // egress 为 nil 时零次迭代
		result[key] = value
	}
	if egress == nil && !upstreamReachable(upstream) {
		if egressErr != nil {
			return nil, egressErr
		}
		return nil, errors.New("出口与上游均不可达")
	}
	// ok 是「这条线路能不能用」的最终判据，优先看 z.ai：出口查询站通了只说明
	// 线路活着，连不上 z.ai 的线路对本项目没有价值（有响应、有结论时才据此判否，
	// 避免「没配上游目标」被误判成线路坏）。
	result["ok"] = upstreamReachable(upstream) || (egress != nil && !upstreamAnswered(upstream))
	return result, nil
}

// upstreamReachable 上游拿到了非拦截的 HTTP 响应，线路可用于 z.ai。
func upstreamReachable(upstream map[string]any) bool {
	return upstream["ok"] == true
}

// upstreamAnswered 上游探测是否给出了结论（有响应，或明确被拦截，或有错误）。
// 用来区分「z.ai 不可达」与「压根没测上游」。
func upstreamAnswered(upstream map[string]any) bool {
	if upstream["ok"] == true || upstream["blocked"] == true {
		return true
	}
	msg, _ := upstream["error"].(string)
	return msg != ""
}

// fetchEgressInfo 依次尝试各个 IP 查询服务，返回出口公网信息；全部失败返回最后一个错误。
func fetchEgressInfo(client *http.Client) (map[string]any, error) {
	started := time.Now()
	var lastErr error
	for _, p := range ipProbeProviders {
		result, err := fetchIPInfo(client, p.name, p.endpoint)
		if err == nil {
			result["latency_ms"] = time.Since(started).Milliseconds()
			return result, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no probe provider attempted")
	}
	return nil, lastErr
}

// probeUpstream 经指定出口逐个探测 upstreamProbeTargets，汇总为：
//
//	ok      至少一个目标拿到了非拦截响应 —— 线路可用于 z.ai
//	blocked 没有可用目标，但至少一个目标的响应像 CDN/WAF 拦截页
//	error   全部目标都没拿到响应（超时 / 连接重置 / DNS 失败 / 被代理拒绝）
//
// targets 保留逐个目标的明细，便于后台区分「主站不通」与「备援站不通」。
func probeUpstream(proxyURL string) map[string]any {
	report := map[string]any{"ok": false, "blocked": false, "error": "", "ms": 0}
	client, err := newProbeClientWith(proxyURL, upstreamProbeTimeout)
	if err != nil {
		report["error"] = err.Error()
		report["targets"] = []any{}
		return report
	}
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
	resp, err := probeGetWithUA(client, target, config.UserAgent)
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

// newProbeClient 构造带默认超时、跟随重定向的探测客户端，默认透传环境代理。
func newProbeClient(proxyURL string) (*http.Client, error) {
	return newProbeClientWith(proxyURL, probeTimeout)
}

// newProbeClientWith 同 newProbeClient，仅超时可指定。
// Go 标准库支持 http/https/socks5 代理；socks5 拨号即远程解析主机名，
// 与 socks5h 语义一致；socks4 无标准库支持，直接报错（呈 502 形态）。
func newProbeClientWith(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
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
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// probeGet 以探测专用 UA 发起 GET（IP 查询服务用）。
func probeGet(client *http.Client, endpoint string) (*http.Response, error) {
	return probeGetWithUA(client, endpoint, probeUserAgent)
}

// probeGetWithUA 以指定 UA 发起 GET。
func probeGetWithUA(client *http.Client, endpoint, userAgent string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	return client.Do(req)
}

// fetchIPInfo 查询单个 IP 服务并整理结果；ASN 缺失时向 hackertarget 补查
// （补查失败仍保留 IP 结果，对齐 Python 版）。
func fetchIPInfo(client *http.Client, source, endpoint string) (map[string]any, error) {
	resp, err := probeGet(client, endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 { // raise_for_status
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	result, err := parseIPLookup(payload)
	if err != nil {
		return nil, err
	}
	if ip, _ := result["ip"].(string); result["asn"] == "" && ip != "" {
		if asn, operator, err := lookupASN(client, ip); err == nil {
			result["asn"] = asn
			if result["operator"] == "" {
				result["operator"] = operator
			}
		}
	}
	result["ok"] = true
	result["source"] = source
	return result, nil
}

// parseIPLookup 将不同 IP 查询服务的字段整理成稳定的后台 API 格式
// （对齐 Python _parse_ip_lookup）。
func parseIPLookup(payload map[string]any) (map[string]any, error) {
	connection, _ := payload["connection"].(map[string]any)
	ip := strings.TrimSpace(strOf(firstTruthy(payload["ip"])))
	if ip == "" {
		return nil, errors.New("查詢服務未回傳 IP")
	}
	asn := strings.ToUpper(strings.TrimSpace(strOf(firstTruthy(
		payload["asn"], payload["asn_num"], connection["asn"]))))
	if asn != "" && !strings.HasPrefix(asn, "AS") {
		asn = "AS" + asn
	}
	operator := strings.TrimSpace(strOf(firstTruthy(
		payload["asn_organization"], payload["asn_org"], payload["company_name"],
		payload["organization"], payload["isp"], connection["org"], connection["isp"],
	)))
	return map[string]any{
		"ip":       ip,
		"asn":      asn,
		"operator": operator,
		"country":  strings.TrimSpace(strOf(firstTruthy(payload["country"]))),
		"country_code": strings.ToUpper(strings.TrimSpace(strOf(firstTruthy(
			payload["country_code"], payload["cc"])))),
	}, nil
}

// lookupASN 查询并解析 hackertarget 备援 ASN 服务（单行 CSV 回应）。
func lookupASN(client *http.Client, ip string) (string, string, error) {
	resp, err := probeGet(client, "https://api.hackertarget.com/aslookup/?q="+url.QueryEscape(ip))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	return parseASLookup(string(body))
}

// parseASLookup 对齐 Python _parse_as_lookup：第一行 CSV，row[1]=asn、row[3]=运营方。
func parseASLookup(raw string) (string, string, error) {
	rec, err := csv.NewReader(strings.NewReader(strings.TrimSpace(raw))).Read()
	if err != nil || len(rec) < 2 {
		return "", "", errors.New("ASN 查詢服務回應格式無效")
	}
	asn := strings.ToUpper(strings.TrimSpace(rec[1]))
	if asn == "" {
		return "", "", errors.New("ASN 查詢服務未回傳 ASN")
	}
	if !strings.HasPrefix(asn, "AS") {
		asn = "AS" + asn
	}
	operator := ""
	if len(rec) > 3 {
		operator = strings.TrimSpace(rec[3])
	}
	return asn, operator, nil
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
