package proxysub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/proxy"
)

func readLimited(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, errors.New("响应读取失败")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("响应超过大小限制")
	}
	return data, nil
}

func noRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func fetch(ctx context.Context, endpoint string) ([]byte, error) {
	transport, err := proxy.NewTransport("", 10*time.Second)
	if err != nil {
		return nil, errors.New("订阅客户端构造失败")
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: noRedirect}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("订阅地址无效")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("订阅连接失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("订阅未返回 HTTP 200，请检查地址与访问权限")
	}
	return readLimited(resp.Body, MaxResponse)
}

// Parse 保留逐条拒绝计数，但结构/大小越界拒绝整批，绝不退回 TXT 导入。
func Parse(data []byte, _ Config, now time.Time) ([]Candidate, Run, error) {
	report := Run{}
	if len(data) > MaxResponse {
		return nil, report, errors.New("订阅超过大小限制")
	}
	var rows []json.RawMessage
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "[") || json.Unmarshal(data, &rows) != nil {
		return nil, report, errors.New("订阅必须是完整 JSON 数组")
	}
	if len(rows) > MaxItems {
		return nil, report, errors.New("订阅条目超过 1000 条")
	}
	report.Received = len(rows)
	out := []Candidate{}
	seen := map[string]bool{}
	for _, row := range rows {
		var item struct {
			Proxy    string          `json:"proxy"`
			Target   string          `json:"target_profile"`
			Usable   bool            `json:"usable"`
			Aux      map[string]bool `json:"aux_reachable"`
			Streak   json.RawMessage `json:"pass_streak"`
			Failures int             `json:"fail_count"`
			LastOK   json.RawMessage `json:"last_ok"`
			IP       string          `json:"ip"`
			Shared   bool            `json:"ip_shared"`
			Latency  float64         `json:"latency"`
		}
		if json.Unmarshal(row, &item) != nil || item.Target != "zai" || !item.Usable || !item.Aux["zcode"] || item.Failures != 0 {
			report.Rejected++
			continue
		}
		endpoint, err := PublicEndpoint(item.Proxy)
		if err != nil {
			report.Rejected++
			continue
		}
		key := Fingerprint(endpoint)
		if seen[key] {
			report.Existing++
			continue
		}
		seen[key] = true
		ip := ""
		if parsed, err := netip.ParseAddr(item.IP); err == nil {
			ip = parsed.Unmap().String()
		}
		// 外部历史只影响排序：缺失、异常或旧记录不能阻止当次本机复检。
		lastOK := historyNumber(item.LastOK)
		if lastOK < 946684800000 || lastOK > now.Add(5*time.Minute).UnixMilli() {
			// 秒单位/明显异常/未来时间降为未知，不猜测转换，也不给排序优势。
			lastOK = 0
		}
		out = append(out, Candidate{URL: endpoint, Key: key, LastOK: lastOK, Streak: int(historyNumber(item.Streak)), IP: ip, Shared: item.Shared, Latency: int64(item.Latency)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LastOK != out[j].LastOK {
			return out[i].LastOK > out[j].LastOK
		}
		if out[i].Streak != out[j].Streak {
			return out[i].Streak > out[j].Streak
		}
		if out[i].Latency != out[j].Latency {
			return out[i].Latency > 0 && (out[j].Latency <= 0 || out[i].Latency < out[j].Latency)
		}
		return out[i].Key < out[j].Key
	})
	return out, report, nil
}

func historyNumber(raw json.RawMessage) int64 {
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0
	}
	return value
}

func TargetSupported() bool {
	u, err := url.Parse(config.UpstreamZai)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), "zcode.z.ai") && (u.Port() == "" || u.Port() == "443") && u.User == nil
}

func probe(ctx context.Context, candidate Candidate) Detail {
	start := time.Now()
	result := Detail{Key: candidate.Key, Endpoint: candidate.URL}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	transport, err := proxy.NewTransport(candidate.URL, 6*time.Second)
	if err != nil {
		result.Reason = "代理传输构造失败"
		return result
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: noRedirect}
	result.OK, result.Reason = probeConfig(ctx, client, ProbeTarget)
	result.MS = time.Since(start).Milliseconds()
	return result
}

// 单独保留实际 HTTP 接缝，测试用本地 TLS/代理链路，不替换响应判定。
func probeConfig(ctx context.Context, client *http.Client, target string) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, "探针地址无效"
	}
	req.Header.Set("User-Agent", config.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, "探针连接失败或超时"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "配置入口未返回 HTTP 200"
	}
	data, err := readLimited(resp.Body, 64<<10)
	if err != nil {
		return false, err.Error()
	}
	var payload struct {
		Code *int `json:"code"`
		Data struct {
			Providers []struct {
				ID string `json:"id"`
			} `json:"providers"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Code == nil || *payload.Code != 0 {
		return false, "配置响应不符合契约"
	}
	for _, p := range payload.Data.Providers {
		if strings.TrimSpace(p.ID) != "" {
			return true, "配置入口复检通过，未验证模型业务"
		}
	}
	return false, "配置响应缺少 providers"
}
