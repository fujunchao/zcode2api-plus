package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

func TestAttemptLogNumbersSameAccountRetries(t *testing.T) {
	f := newFixture(t)
	f.respond = func(call int, _ *http.Request) (int, http.Header, string) {
		if call == 1 {
			return 429, http.Header{"Content-Type": {"application/json"}}, `{"error":"rate limited"}`
		}
		return 200, http.Header{"Content-Type": {"application/json"}}, okUpstreamJSON
	}
	a, _ := f.st.AddAccount(model.ProviderZai, "retry", "sk-retry")
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 200 {
		t.Fatalf("重试应成功：%d", status)
	}
	var headers []string
	for _, line := range strings.Split(stripANSI(buf.String()), "\n") {
		if strings.Contains(line, "[attempt]") && strings.Contains(line, "event=headers") {
			headers = append(headers, line)
		}
	}
	if len(headers) != 2 {
		t.Fatalf("实际两次 HTTP 调用应各有响应头日志：%v", headers)
	}
	for i, want := range []string{"attempt=1 selection=1", "attempt=2 selection=1"} {
		if !strings.Contains(headers[i], want) || !strings.Contains(headers[i], `acc_id="`+a.ID+`"`) {
			t.Fatalf("原地重试日志错误：%s", headers[i])
		}
	}
	if !strings.Contains(headers[0], "status=429") || !strings.Contains(headers[1], "status=200") {
		t.Fatal("不得只保留末次 200")
	}
}

func TestAttemptLogRedactsCredentialsAndClassifiesErrors(t *testing.T) {
	s := openStore(t)
	a, _ := s.AddAccount(model.ProviderZai, "account", "sk-token-sentinel")
	raw := "http://user-sentinel:password-sentinel@127.0.0.1:18080/private-sentinel?key=query-sentinel#fragment-sentinel"
	_, _ = s.SetProxyURL(a.Provider, a.ID, &raw)
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	diag := NewReqDiag("abcdef", "model", true, nil, nil)
	diag.Attempts = 1
	finish := diag.BeginUpstreamAttempt(s.FindAny(a.ID), s.ProxyEgress(s.FindAny(a.ID)), nil)
	finish(nil, context.Canceled)
	logs := buf.String()
	for _, secret := range []string{"user-sentinel", "password-sentinel", "private-sentinel", "query-sentinel", "fragment-sentinel", "sk-token-sentinel"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("日志泄漏 %s", secret)
		}
	}
	if !strings.Contains(logs, `endpoint="http://127.0.0.1:18080"`) || !strings.Contains(logs, "event=error") || !strings.Contains(logs, "err_kind=canceled") {
		t.Fatalf("脱敏端点/错误类别缺失：%s", logs)
	}
}

func TestRiskProxyInvalidManualConfigFailsBeforeDispatch(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(405, `{"error":{"message":"blocked due to unusual activity"}}`)
	a, _ := f.st.AddAccount(model.ProviderZai, "invalid-proxy", "sk-invalid-proxy")
	_, _ = f.st.AddProxyProfile("candidate", f.upstream.URL, true)
	bad := "http://user:secret-sentinel@%zz"
	_, _ = f.st.Update(a.Provider, a.ID, func(live *model.Account) { live.ProxyURL = &bad })
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	status, body := f.post(t, msgBody(), "sk-test")
	logs := stripANSI(buf.String())
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "selected_proxy_unavailable") {
		t.Fatalf("手工无效路由必须明确拒绝、零出站且不泄密：status=%d calls=%d body=%s logs=%s", status, f.callCount(), body, logs)
	}
	if f.callCount() != 0 || !strings.Contains(logs, "upstream_attempts=0") {
		t.Fatalf("拒绝派发必须零出站：calls=%d logs=%s", f.callCount(), logs)
	}
	if strings.Contains(logs, "secret-sentinel") || strings.Contains(body, "secret-sentinel") {
		t.Fatal("无效路由诊断不得泄漏代理密码")
	}
	current := f.st.FindAny(a.ID)
	if *current.ProxyURL != bad || current.Status != model.StatusActive || current.FailCount != 0 || current.LastRiskControlAt != nil {
		t.Fatal("未使用的代理不应被风控改派，也不得惩罚账号")
	}
}
