package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

type failingCaptchaSolver struct{ calls atomic.Int32 }

func (s *failingCaptchaSolver) Solve(context.Context, captcha.Config) (string, error) {
	s.calls.Add(1)
	return "", fmt.Errorf("synthetic solver failure")
}

func (s *failingCaptchaSolver) Close() error { return nil }

type uniqueCaptchaSolver struct{ n atomic.Int32 }

func (s *uniqueCaptchaSolver) Solve(context.Context, captcha.Config) (string, error) {
	id := s.n.Add(1)
	payload, _ := json.Marshal(map[string]string{"certifyId": fmt.Sprintf("cert-%d", id), "sceneId": "sc"})
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func (s *uniqueCaptchaSolver) Close() error { return nil }

// 池空且求解失败时必须 503，绝不能裸发（上游 0 次调用）。
func TestJWTCaptchaPoolEmptyDoesNotSendBare(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })

	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil
	})
	solver := &failingCaptchaSolver{}
	f.cm.SetSolver(solver)
	f.cm.EnableTokenPool(true)
	t.Cleanup(func() { _ = f.cm.Close() })

	f.respond = func(int, *http.Request) (int, http.Header, string) {
		t.Fatal("拿不到 token 时不应触达上游")
		return 200, nil, ""
	}
	if _, err := f.st.AddAccount(model.ProviderZai, "jwt-pool-empty", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("池空应 503，实际 %d %s", status, raw)
	}
	if f.callCount() != 0 {
		t.Fatalf("上游不应被调用，实际 %d", f.callCount())
	}
	if solver.calls.Load() == 0 {
		t.Fatal("应至少尝试一次求解")
	}
}

// 并发 JWT：每个上游请求必须携带不同 token（certifyId 不重复），且都成功。
func TestConcurrentJWTCaptchaTokensAreUnique(t *testing.T) {
	f := newFixture(t)
	oldBrowser := config.CaptchaBrowserEnabled
	config.CaptchaBrowserEnabled = true
	t.Cleanup(func() { config.CaptchaBrowserEnabled = oldBrowser })

	f.cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.DefaultConfig, nil
	})
	solver := &uniqueCaptchaSolver{}
	f.cm.SetSolver(solver)
	f.cm.EnableTokenPool(true)
	t.Cleanup(func() { _ = f.cm.Close() })

	f.respond = jsonResp(200, okUpstreamJSON)
	if _, err := f.st.AddAccount(model.ProviderZai, "jwt-concurrent", "header.payload.sig"); err != nil {
		t.Fatal(err)
	}

	const total = 10
	var wg sync.WaitGroup
	statusMu := sync.Mutex{}
	statuses := make([]int, 0, total)
	errs := make(chan error, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, err := postPoolRequest(f)
			if err != nil {
				errs <- err
				return
			}
			statusMu.Lock()
			statuses = append(statuses, status)
			statusMu.Unlock()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发请求失败: %v", err)
	}
	for _, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("并发请求应全部 200，实际 %v", statuses)
		}
	}

	f.mu.Lock()
	calls := append([]upstreamCall(nil), f.calls...)
	f.mu.Unlock()
	if len(calls) != total {
		t.Fatalf("上游调用数应为 %d，实际 %d", total, len(calls))
	}
	seen := map[string]bool{}
	for _, call := range calls {
		token := call.Header.Get("X-Aliyun-Captcha-Verify-Param")
		if token == "" {
			t.Fatal("存在裸发：上游请求缺少验证码头")
		}
		if seen[token] {
			t.Fatalf("并发请求复用了同一 token: %s", token)
		}
		seen[token] = true
	}
}

func postPoolRequest(f *fixture) (int, string, error) {
	body, err := json.Marshal(msgBody())
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), err
}
