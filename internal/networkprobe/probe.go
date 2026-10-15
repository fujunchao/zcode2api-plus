// Package networkprobe 提供无账号的浅层探测；不决定删除、不访问模型正文或保存凭据。
package networkprobe

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/proxy"
)

const Timeout = 6 * time.Second

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
