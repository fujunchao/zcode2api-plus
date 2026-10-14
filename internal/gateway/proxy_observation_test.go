package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/upstream"
	"zcode2api/internal/web"
)

func syntheticProxyObservation(t *testing.T) (context.Context, *http.Request) {
	t.Helper()
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "observation", "sk-synthetic-observation")
	if err != nil {
		t.Fatal(err)
	}
	body := msgBody()
	attr := upstream.NewAttribution(nil, "synthetic-session")
	upstream.InjectDeviceMetadata(body, acc.DeviceMidOr("synthetic-device"), attr.SessionID)
	prepared, err := upstream.BuildRequestWithAttribution(acc, "", "", nil, attr)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, prepared.URL, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range prepared.Headers {
		req.Header.Set(k, v)
	}
	ctx, release, err := BeginProxyAttempt(context.Background(), f.st, acc, f.st.ProxyEgress(acc), req, body, "synthetic-entry")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return ctx, req
}

func TestStrictProxyInjectedClientDoesNotClaimDirectEvidence(t *testing.T) {
	f := newFixture(t)
	acc, err := f.st.AddAccount(model.ProviderZai, "custom-outlet", "sk-synthetic-custom")
	if err != nil {
		t.Fatal(err)
	}
	f.respond = jsonResp(200, strictCompleteJSON)
	f.eng.Client = f.upstream.Client()
	_, egress := f.eng.clientFor(acc)
	if egress.Mode != "custom_client" || egress.Endpoint == "direct" {
		t.Fatal("注入客户端的实际出口未确定，不能标为直连")
	}
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	if status, _ := f.post(t, msgBody(), "sk-test"); status != http.StatusOK {
		t.Fatal("诚实记录出口不能破坏注入客户端的正常交付")
	}
	logs := stripANSI(buf.String())
	if !strings.Contains(logs, "mode=custom_client") || strings.Contains(logs, "mode=direct ") {
		t.Fatal("实际HTTP诊断不得把自定义出口当直连")
	}
}

func TestStrictProxyObservationSurvivesDiagnosticKeyRotation(t *testing.T) {
	ctx, req := syntheticProxyObservation(t)
	p := ctx.Value(proxyObservationKey{}).(*proxyAttemptObservation)
	if p.value.InvalidRequest {
		t.Fatal("夹具应构造合法模型请求")
	}
	before := observationRef("synthetic-session")
	referenceKeys.Lock()
	key, expires := referenceKeys.key, referenceKeys.expires
	referenceKeys.expires = time.Time{}
	referenceKeys.Unlock()
	t.Cleanup(func() {
		referenceKeys.Lock()
		referenceKeys.key, referenceKeys.expires = key, expires
		referenceKeys.Unlock()
	})
	_ = privateReference("rotate-log-key", time.Now())
	ConfirmProxyAttemptResponse(ctx, &http.Response{StatusCode: 200, Request: req})
	if !p.responseVerified || observationRef("synthetic-session") != before {
		t.Fatal("日志密钥轮换不能破坏实际目标匹配和私有证据对照")
	}
}

func TestStrictProxyObservationRejectsChangedTargetAndRealRedirectStatus(t *testing.T) {
	for _, status := range []int{200, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			ctx, req := syntheticProxyObservation(t)
			if status == 200 {
				copied := *req.URL
				copied.Path += "/wrong-target"
				req.URL = &copied
			}
			ConfirmProxyAttemptResponse(ctx, &http.Response{StatusCode: status, Request: req})
			ObserveProxyAttempt(ctx, 200, "", true, time.Now())
			p := ctx.Value(proxyObservationKey{}).(*proxyAttemptObservation)
			unused := false
			p.once.Do(func() { unused = true })
			if !unused {
				t.Fatal("错误目标或真实302不得因交付层写200而记作完整成功")
			}
		})
	}
}
