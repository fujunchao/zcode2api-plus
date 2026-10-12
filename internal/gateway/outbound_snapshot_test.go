package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

// 通过实际HTTP及日志边界检查：恶意扩展值不能进入日志，合法正文仍原样送达。
func TestDiagnosticOutboundValuesAreBoundedAndRedacted(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(200, okUpstreamJSON)
	_, err := f.st.AddAccount(model.ProviderZai, "safe-account", "synthetic-safe-key")
	if err != nil {
		t.Fatal(err)
	}
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	body := msgBody()
	blocks := []any{}
	for range 20 {
		blocks = append(blocks, map[string]any{"type": "text", "text": "private-body-sentinel"})
	}
	body["system"] = blocks
	body["output_config"] = map[string]any{"effort": "private-effort-sentinel"}
	body["thinking"] = map[string]any{"type": "private-thinking-sentinel"}
	body["max_tokens"] = "private-max-sentinel"
	if status, _ := f.post(t, body, "sk-test"); status != http.StatusOK {
		t.Fatal("观测不能擅自改变原生透传行为")
	}
	actual := f.lastCall().Body
	if actual["max_tokens"] != "private-max-sentinel" {
		t.Fatal("观测不能清洗真正发送的参数")
	}
	logs := stripANSI(buf.String())
	if strings.Contains(logs, "private-") || strings.Contains(logs, "synthetic-safe-key") {
		t.Fatal("非法参数或正文泄漏进日志")
	}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "[attempt]") {
			continue
		}
		if !strings.Contains(line, "system_summary_truncated=true") {
			t.Fatal("系统块摘要应有界且明示截断")
		}
		for _, field := range []string{"system_text_bytes", "system_text_sha256"} {
			var values []any
			if err := json.Unmarshal([]byte(diagField(t, line, field)), &values); err != nil || len(values) != 16 {
				t.Fatalf("字段应仅有16项: %s", field)
			}
		}
	}
}

func TestDiagnosticAccountSnapshotFreezesPerAttempt(t *testing.T) {
	f := newFixture(t)
	a, err := f.st.AddAccount(model.ProviderZai, "snapshot-account", "synthetic-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	setQuota := func(amount float64) {
		_, err := f.st.Update(a.Provider, a.ID, func(a *model.Account) {
			a.Quota = map[string]map[string]any{"GLM-5.3": {"remaining": amount, "available": amount}}
		})
		if err != nil {
			t.Error(err)
		}
	}
	setQuota(42)
	f.respond = func(n int, _ *http.Request) (int, http.Header, string) {
		if n == 1 {
			setQuota(77) // 模拟首个请求发送后，旁路账单更新。
			return jsonResp(429, `{"msg":"rate limited"}`)(n, nil)
		}
		return jsonResp(200, okUpstreamJSON)(n, nil)
	}
	buf := &syncBuffer{}
	web.SetOut(buf)
	t.Cleanup(func() { web.SetOut(nil) })
	if status, _ := f.post(t, msgBody(), "sk-test"); status != 200 || f.callCount() != 2 {
		t.Fatal("夹具应产生两次实际出站")
	}
	count := 0
	for _, line := range strings.Split(stripANSI(buf.String()), "\n") {
		if !strings.Contains(line, "[attempt]") || !(strings.Contains(line, "event=start") || strings.Contains(line, "event=headers")) {
			continue
		}
		want := "quota_available=42"
		if strings.Contains(line, "attempt=2 ") {
			want = "quota_available=77"
		}
		if !strings.Contains(line, want) {
			t.Fatalf("响应不能回写首轮快照，后续尝试须读新快照: %s", line)
		}
		count++
	}
	if count != 4 {
		t.Fatalf("两次尝试须各有一对开始/响应快照: %d", count)
	}
}
