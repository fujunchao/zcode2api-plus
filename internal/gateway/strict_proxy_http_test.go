package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

const strictCompleteJSON = `{"id":"msg_control","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
const strictRiskJSON = `{"code":"3012","message":"Request has been blocked due to unusual activity."}`

// 真实发送和完整交付必须接入严格判定，不能只在 Store 直接调用时有效。
func TestStrictProxyHTTPRecordsControlAndRetires(t *testing.T) {
	strictHTTPControlCase(t, strictRiskJSON, "application/json", strictCompleteJSON, true, false)
}

func TestStrictProxyHTTPRequiresCompleteComparableEvidence(t *testing.T) {
	completeSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_control\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, bad, contentType, good string
		retire                       bool
	}{
		{"完整SSE可作对照", strictRiskJSON, "text/event-stream", completeSSE, true},
		{"仅usage不是完整JSON", strictRiskJSON, "application/json", okUpstreamJSON, false},
		{"无终帧不作成功", strictRiskJSON, "text/event-stream", strings.TrimSuffix(completeSSE, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), false},
		{"普通405不淘汰", `{"code":"9999","message":"method not allowed"}`, "application/json", strictCompleteJSON, false},
		{"截断错误前缀不淘汰", strictRiskJSON + strings.Repeat(" ", maxErrorBodyBytes), "application/json", strictCompleteJSON, false},
		{"业务错误200不是成功", strictRiskJSON, "application/json", `{"code":"3012","message":"blocked"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) { strictHTTPControlCase(t, tc.bad, tc.contentType, tc.good, tc.retire, false) })
	}
}

func TestStrictProxyHTTPDirectSuccessIsControl(t *testing.T) {
	strictHTTPControlCase(t, strictRiskJSON, "application/json", strictCompleteJSON, true, true)
}

func TestStrictProxyHTTPRedirectCannotSupplySuccessEvidence(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			strictHTTPControlCase(t, strictRiskJSON, "application/json", strictCompleteJSON, false, false, status)
		})
	}
}

func strictHTTPControlCase(t *testing.T, badBody, goodType, goodBody string, wantRetired, direct bool, responseStatus ...int) {
	t.Helper()
	f := newFixture(t)
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(405)
		_, _ = w.Write([]byte(badBody))
	}))
	defer badServer.Close()
	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", goodType)
		if len(responseStatus) > 0 {
			w.Header().Set("Location", f.upstream.URL+"/redirected")
			w.WriteHeader(responseStatus[0])
		}
		_, _ = w.Write([]byte(goodBody))
	}))
	defer goodServer.Close()
	bad, err := f.st.AddProxyProfile("bad-control", badServer.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	good, err := f.st.AddProxyProfile("good-control", goodServer.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.st.AddAccount(model.ProviderZai, "synthetic", "sk-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	goodID := good.ID
	if direct {
		goodID = ""
		f.respond = func(int, *http.Request) (int, http.Header, string) {
			return http.StatusOK, http.Header{"Content-Type": {goodType}}, goodBody
		}
	}
	for _, p := range []string{bad.ID, bad.ID, goodID} {
		// 仅在隔离夹具中重置调度时间，构造正常业务在窗口内的可比尝试，不修改生产保护预算。
		_, _ = f.st.Update(a.Provider, a.ID, func(live *model.Account) {
			live.Status = model.StatusActive
			live.CoolingUntil = nil
			live.LastRiskControlAt = nil
			live.RiskControlStreak = 0
		})
		if _, err := f.st.AssignProxyProfile(a.ID, p); err != nil {
			t.Fatal(err)
		}
		f.postWithHeaders(t, msgBody(), map[string]string{"X-Session-Id": "sess-synthetic-control", "X-Zcode-Trace-Id": "trace-synthetic"})
	}
	status, err := f.st.StrictProxySummary()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(status.Blocks) == 1; got != wantRetired {
		t.Fatalf("严格HTTP对照是否成立: got=%t want=%t blocks=%+v", got, wantRetired, status.Blocks)
	}
	_, err = f.st.AddProxyProfile("bad-reimport", bad.URL, true)
	if (err != nil) != wantRetired {
		t.Fatalf("只在严格HTTP对照成立后拒绝重导: retired=%t err=%v", wantRetired, err)
	}
	if !direct && f.callCount() != 0 {
		t.Fatal("模型请求不得在一次许可下自动跟随重定向")
	}
}
