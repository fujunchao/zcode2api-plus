package proxysub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixture(raw string, now time.Time) map[string]any {
	return map[string]any{"proxy": raw, "target_profile": "zai", "usable": true, "aux_reachable": map[string]bool{"zcode": true}, "pass_streak": 2, "last_ok": now.UnixMilli()}
}

func TestSubscriptionContract(t *testing.T) {
	now := time.Unix(1791892313, 0)
	for _, tc := range []struct {
		name              string
		patch             func(map[string]any)
		accepted, expired int
	}{
		{"有效", func(map[string]any) {}, 1, 0},
		{"不同档位", func(x map[string]any) { x["target_profile"] = "generic" }, 0, 0},
		{"错误布尔", func(x map[string]any) { x["usable"] = "true" }, 0, 0},
		{"未达标", func(x map[string]any) { x["aux_reachable"] = map[string]bool{"zcode": false} }, 0, 0},
		{"一次成功", func(x map[string]any) { x["pass_streak"] = 1 }, 0, 0},
		{"失败记录", func(x map[string]any) { x["fail_count"] = 1 }, 0, 0},
		{"秒时间戳", func(x map[string]any) { x["last_ok"] = now.Unix() }, 0, 1},
		{"过期", func(x map[string]any) { x["last_ok"] = now.Add(-3 * time.Hour).UnixMilli() }, 0, 1},
		{"未来", func(x map[string]any) { x["last_ok"] = now.Add(6 * time.Minute).UnixMilli() }, 0, 1},
		{"缺时间", func(x map[string]any) { delete(x, "last_ok") }, 0, 1},
		{"未知字段", func(x map[string]any) { x["future_field"] = true }, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := fixture("http://8.8.8.8:8080", now)
			tc.patch(item)
			data, _ := json.Marshal([]any{item})
			got, report, err := Parse(data, DefaultConfig(), now)
			if err != nil || len(got) != tc.accepted || report.Expired != tc.expired {
				t.Fatalf("got=%v report=%+v err=%v", got, report, err)
			}
		})
	}
	for _, raw := range []string{`null`, `{}`, `[{}`, `[] []`, `<html>login</html>`, strings.Repeat(" ", MaxResponse+1), "[" + strings.Repeat("{},", MaxItems) + "{}]"} {
		if _, _, err := Parse([]byte(raw), DefaultConfig(), now); err == nil {
			t.Fatalf("应拒绝异常订阅，长度=%d", len(raw))
		}
	}
	items := []any{fixture("HTTP://8.8.8.8:08080/", now), fixture("http://8.8.8.8:8080", now), fixture("socks5://8.8.8.8:8080", now), fixture("socks5h://8.8.8.8:8080", now)}
	raw, _ := json.Marshal(items)
	got, report, err := Parse(raw, DefaultConfig(), now)
	if err != nil || len(got) != 3 || report.Existing != 1 {
		t.Fatalf("去重不能合并不同协议: %v %+v %v", got, report, err)
	}
}

func TestSubscriptionPublicEndpoints(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:80", "http://10.0.0.1:80", "http://169.254.169.254:80", "http://100.64.0.1:80", "http://192.0.2.1:80",
		"http://224.1.1.1:80", "http://[::ffff:127.0.0.1]:80", "http://[::1]:80", "http://[fc00::1]:80", "http://[2001:db8::1]:80", "http://[2002:0808:0808::1]:80",
		"http://example.com:80", "http://user:secret@8.8.8.8:80", "http://8.8.8.8:80/path", "http://8.8.8.8:80?a=b", "http://8.8.8.8:80#fragment", "http://8.8.8.8:0", "ftp://8.8.8.8:80"} {
		if _, err := PublicEndpoint(raw); err == nil {
			t.Fatalf("应拒绝 %s", raw)
		}
	}
	for _, raw := range []string{"http://8.8.8.8:80", "https://1.1.1.1:443", "socks5h://[2606:4700:4700::1111]:1080"} {
		if _, err := PublicEndpoint(raw); err != nil {
			t.Fatalf("合法地址 %s: %v", raw, err)
		}
	}
}

func TestSubscriptionConfigProbe(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
	}{
		{200, `{"code":0,"data":{"providers":[{"id":"bigmodel"}]}}`, true},
		{200, `{"data":{"providers":[{"id":"bigmodel"}]}}`, false},
		{200, `{"code":0,"data":{"providers":[]}}`, false},
		{200, `<html>providers</html>`, false},
		{405, `{"code":3012,"msg":"unusual activity"}`, false},
		{401, `{"code":0}`, false},
		{500, `{"code":0,"data":{"providers":[{"id":"bigmodel"}]}}`, false},
		{200, strings.Repeat("x", 65537), false},
	} {
		t.Run(fmt.Sprint(tc.status, "-", len(tc.body)), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Method != "GET" {
					t.Error("探测不应携带凭据")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			ok, reason := probeConfig(context.Background(), server.Client(), server.URL)
			if ok != tc.ok {
				t.Fatalf("ok=%v reason=%s", ok, reason)
			}
			// 系统信任链不认识测试证书，不能为探测绕过 TLS 校验。
			if tc.ok {
				if ok, _ := probeConfig(context.Background(), http.DefaultClient, server.URL); ok {
					t.Fatal("不受信证书被接受")
				}
			}
		})
	}
}

func TestSubscriptionFetchBoundsAndPrivacy(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++; _, _ = w.Write([]byte("[]")) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/secret":
			http.Redirect(w, r, target.URL, 302)
		case "/huge":
			_, _ = w.Write([]byte(strings.Repeat("x", MaxResponse+1)))
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	if data, err := fetch(context.Background(), server.URL); err != nil || string(data) != "[]" {
		t.Fatalf("显式直连: %s %v", data, err)
	}
	for _, path := range []string{"/secret", "/huge"} {
		_, err := fetch(context.Background(), server.URL+path+"?token=secret-value")
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Fatal("应失败且不泄露 token")
		}
	}
	if targetHits != 0 {
		t.Fatal("不应跟随订阅重定向")
	}
}
