// Package networkprobe 提供无账号的浅层探测；不决定删除、不访问模型正文或保存凭据。
package networkprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/proxy"
)

const Timeout = 6 * time.Second

// TargetsKey 绑定探测站点集合，配置变更后旧站点的成功不能冒充新配置健康。
func TargetsKey(targets []string) string {
	sum := sha256.Sum256([]byte(strings.Join(targets, "\n")))
	return hex.EncodeToString(sum[:])
}

type Target struct {
	URL     string `json:"url"`
	Host    string `json:"host"`
	OK      bool   `json:"ok"`
	Blocked bool   `json:"blocked"`
	Status  int    `json:"status,omitempty"`
	MS      int64  `json:"ms"`
	Error   string `json:"error,omitempty"`
}

type Report struct {
	OK      bool     `json:"ok"`
	Blocked bool     `json:"blocked"`
	Error   string   `json:"error"`
	MS      int64    `json:"ms"`
	Targets []Target `json:"targets"`
	// Tunnel 是可选的上传能力测量（隧道裸写）。指针 + omitempty：旧记录没有这个字段，
	// 反序列化后为 nil，不影响既有判定。
	Tunnel *TunnelReport `json:"tunnel,omitempty"`
}

// TunnelReport 是一次隧道裸写的结果。Bytes/MS/Kbps 是原始测量值，OK 是分级判定。
type TunnelReport struct {
	OK        bool   `json:"ok"`
	Bytes     int64  `json:"bytes"`
	MS        int64  `json:"ms"`
	Kbps      int64  `json:"kbps"`
	EarlyStop bool   `json:"early_stop,omitempty"`
	Error     string `json:"error,omitempty"`
}

// CheckTunnelWrite 走裸隧道向 target 写 size 字节，用「写了多少 / 用了多久」判断上传能力。
//
// 判据分级（对端早关 ≠ 链路慢）：
//  1. 预算内写满 → OK
//  2. 写错误但已写 ≥ minBytes、用时 < 预算的 2/3 且速率 ≥ minKbps → OK（对端提前关闭是正常现象）
//  3. 其余（含撞上写超时）→ 失败
//
// 刻意**不**用「已写 ≥ minBytes 且平均速率达标」判 OK：内核缓冲会先吞掉几百 KB，
// 之后对端不读、写阻塞到超时，平均速率仍然好看——那正是要抓的「传不动」。
func CheckTunnelWrite(ctx context.Context, proxyURL, target string, size int64, budget time.Duration, minBytes, minKbps int64) TunnelReport {
	rep := TunnelReport{}
	start := time.Now()
	conn, err := proxy.DialTunnel(ctx, proxyURL, target)
	if err != nil {
		rep.MS = time.Since(start).Milliseconds()
		rep.Error = err.Error()
		return rep
	}
	defer conn.Close()

	_ = conn.SetWriteDeadline(start.Add(budget))
	buf := make([]byte, 32<<10)
	for i := range buf {
		buf[i] = 'x'
	}
	var writeErr error
	for rep.Bytes < size {
		chunk := int64(len(buf))
		if left := size - rep.Bytes; left < chunk {
			chunk = left
		}
		n, err := conn.Write(buf[:chunk])
		rep.Bytes += int64(n)
		if err != nil {
			writeErr = err
			rep.EarlyStop = true
			break
		}
	}
	rep.MS = time.Since(start).Milliseconds()
	// 毫秒级完成时 MS 可能为 0：用 1ms 兜底，避免把「极快」算成 0 KB/s。
	elapsedMS := rep.MS
	if elapsedMS < 1 {
		elapsedMS = 1
	}
	rep.Kbps = rep.Bytes * 1000 / elapsedMS / 1024
	budgetMS := budget.Milliseconds()
	switch {
	case rep.Bytes >= size:
		rep.OK = true
	case writeErr != nil && rep.Bytes >= minBytes && rep.MS < budgetMS*2/3 && rep.Kbps >= minKbps:
		rep.OK = true
	}
	if writeErr != nil {
		rep.Error = writeErr.Error()
	}
	return rep
}

func Targets() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range []string{config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		origin := u.Scheme + "://" + u.Host + "/"
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	return out
}

func Client(raw string) (*http.Client, error) {
	t, err := proxy.TransportForTimeout(raw, Timeout)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: t, Timeout: Timeout}, nil
}

func Blocked(resp *http.Response, body string) bool {
	if resp.Header.Get("Cf-Mitigated") != "" {
		return true
	}
	for _, marker := range []string{"Just a moment", "Attention Required", "cf-chl-", "Enable JavaScript and cookies"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return resp.StatusCode == 403 && strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare")
}

func CheckTarget(ctx context.Context, client *http.Client, target string) Target {
	u, _ := url.Parse(target)
	host := ""
	if u != nil {
		host = u.Host
	}
	out := Target{URL: target, Host: host}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		out.Error = "invalid_target"
		return out
	}
	// 无 Authorization、Cookie 或模型 metadata；直连不受环境代理影响。
	req.Header.Set("User-Agent", config.UserAgent)
	resp, err := client.Do(req)
	out.MS = time.Since(started).Milliseconds()
	if err != nil {
		out.Error = "connection_failed"
		if ctx.Err() != nil {
			out.Error = "canceled"
		}
		return out
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		out.Error = "incomplete_response"
		return out
	}
	out.OK, out.Status, out.Blocked = true, resp.StatusCode, Blocked(resp, string(body))
	return out
}

func Check(ctx context.Context, client *http.Client, targets []string) Report {
	r := Report{Targets: []Target{}}
	if len(targets) == 0 {
		r.Error = "no_targets"
		return r
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			r.OK = false
			r.Error = "canceled"
			return r
		}
		e := CheckTarget(ctx, client, target)
		r.Targets = append(r.Targets, e)
		if e.OK && !e.Blocked {
			if !r.OK {
				r.MS = e.MS
			}
			r.OK = true
		}
		if e.Blocked {
			r.Blocked = true
		}
		if e.Error != "" && r.Error == "" {
			r.Error = e.Error
		}
	}
	if r.OK {
		r.Blocked = false
		r.Error = ""
	} else if r.Error == "" && r.Blocked {
		r.Error = "edge_blocked"
	}
	return r
}
