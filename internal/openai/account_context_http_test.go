package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 首条回归从真实 HTTP 入口进入；不支持的卡上下文不能被当作普通请求成功执行。
func TestAccountContextHeadersRejectedBeforeUpstream(t *testing.T) {
	f := newFixture(t)
	f.setResponder(replyText)
	req, err := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages",
		strings.NewReader(`{"model":"GLM-5.3","max_tokens":64,"messages":[{"role":"user","content":"合成卡上下文回归"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Highspeed-Card-ID", "synthetic-private-card")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("未支持的卡上下文必须在选号前返回 400，实际 %d: %s", resp.StatusCode, raw)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("错误响应必须是 JSON: %v", err)
	}
	if body.Error.Type != "unsupported_account_context" {
		t.Errorf("应给出明确的账号上下文错误，实际 %q", body.Error.Type)
	}
	if strings.Contains(string(raw), "synthetic-private-card") {
		t.Error("错误响应不得回显调用者的卡 ID")
	}
	f.mu.Lock()
	calls := len(f.calls)
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("拒绝的请求不得消耗普通账号，上游实际收到 %d 次请求", calls)
	}
}
