package gateway

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 在真实 HTTP 边界复现“已有 405 响应头，错误体却没读完”的日志盲区。
func TestDiagnosticErrorBodyReadFailureVisible(t *testing.T) {
	oldData := config.DataDir
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DataDir = oldData })
	f := newDiagFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "500")
		w.Header().Set("X-Log-Id", "0123456789abcdef0123456789abcdef")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = io.WriteString(w, `{"code":3012,"msg":`)
		w.(http.Flusher).Flush()
		// 少于 Content-Length 的 EOF 会由客户端报告为 unexpected EOF。
	}))
	if _, err := f.st.AddAccount(model.ProviderZai, "read-error", "sk-synthetic"); err != nil {
		t.Fatal(err)
	}
	_, _ = f.post(t, msgBody())
	logs := stripANSI(f.buf.String())
	for _, want := range []string{
		"event=response_body", "status=405", "read_err_kind=unexpected_eof",
		`response_log_id="0123456789abcdef0123456789abcdef"`, "gateway_request_id=", "ts=",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("错误体读取盲区仍存在，缺少 %q：\n%s", want, logs)
		}
	}
}

// 重放早退也属于独立入站调用，必须有收尾；不得再次访问上游或续期窗口。
func TestDiagnosticReplayEarlyReturnHasSummary(t *testing.T) {
	oldData := config.DataDir
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DataDir = oldData })
	f := newDiagFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("重放防护不应访问上游")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	body := msgBody()
	NormalizeBody(body, false)
	f.eng.replay.Record(ContentKey("GLM-5.3", body))
	status, response := f.post(t, body)
	if status != http.StatusServiceUnavailable || !strings.Contains(response, "risk_control_cooldown") {
		t.Fatalf("重放交付语义不应改变：status=%d body=%s", status, response)
	}
	lines := f.buf.diagLines(t)
	if len(lines) != 1 {
		t.Fatalf("每个入站请求恰好一条汇总，实际 %d 条", len(lines))
	}
	line := lines[0]
	for _, want := range []string{"upstream_attempts=0", "replay_blocked=1", "gateway_request_id=", "input_ref="} {
		if !strings.Contains(line, want) {
			t.Errorf("重放汇总缺少 %q：%s", want, line)
		}
	}
	ms, err := strconv.Atoi(diagField(t, line, "replay_wait_ms"))
	if err != nil || ms <= 0 || ms > 60_000 {
		t.Fatalf("应报告真实剩余窗口：%d %v", ms, err)
	}
	if strings.Contains(f.buf.String(), "[attempt]") {
		t.Fatal("零出站重放不得生成上游尝试日志")
	}
}
