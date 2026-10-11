package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"zcode2api/internal/web"
)

func TestDiagnosticResponseHeaderAllowlist(t *testing.T) {
	resp := &http.Response{Header: http.Header{
		"X-Request-Id":       {"df9e1260-d75b-4007-bc7c-9d6304ec0bd4"},
		"X-Log-Id":           {"0123456789abcdef0123456789abcdef"},
		"Request-Id":         {"Bearer private-response-token"},
		"Traceparent":        {"00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"},
		"X-Zhipu-Request-Id": {"first-ambiguous", "second-ambiguous"},
		"Set-Cookie":         {"private-cookie-sentinel"},
		"Authorization":      {"private-authorization-sentinel"},
		"X-Debug-Body":       {"private-body-sentinel"},
	}}
	got := responseHeaderSummary(resp)
	for _, want := range []string{
		`response_request_id="df9e1260-d75b-4007-bc7c-9d6304ec0bd4"`,
		`response_log_id="0123456789abcdef0123456789abcdef"`,
		`response_alt_request_id="-"`, `response_alt_request_ref="` + attemptIDRef("Bearer private-response-token") + `"`,
		`response_traceparent_id="00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"`,
		`response_zhipu_request_id="-" response_zhipu_request_ref="-"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("白名单追踪字段缺失 %q：%s", want, got)
		}
	}
	for _, secret := range []string{"private-", "first-ambiguous", "second-ambiguous", "Set-Cookie", "Authorization"} {
		if strings.Contains(got, secret) {
			t.Errorf("响应头泄漏 %q", secret)
		}
	}
	if responseHeaderSummary(nil) != "" {
		t.Fatal("未收到响应不得虚构追踪头")
	}
	for _, value := range []string{"header.payload.signature", "https://private.test/?key=private", "private\nforged=1", strings.Repeat("a", 10000)} {
		id, ref := diagnosticResponseID(value)
		if id != "-" || ref != attemptIDRef(value) {
			t.Fatal("异常形态只应留下引用，不截取原始值")
		}
	}
}

func TestDiagnosticErrorMetadataDoesNotCopyMessages(t *testing.T) {
	raw := []byte(`{"code":3012,"msg":"private-prompt-sentinel","reason_code":"private-reason-sentinel","type":"private-type-sentinel","request_id":"df9e1260-d75b-4007-bc7c-9d6304ec0bd4","logId":"private-log-token","headers":{"Authorization":"private-key-sentinel"},"error":{"type":"rate_limit_error","code":"F001","message":"private-tool-arguments","details":{"code":12345,"message":"private-path-sentinel","retryable":false}},"details":{"reason_code":6789,"payload":"private-extra-sentinel"}}`)
	meta, valid := diagnosticErrorMetadata(raw, false)
	if !valid {
		t.Fatal("合成错误 JSON 应能解析")
	}
	for key, want := range map[string]any{
		"code": "3012", "error.code": "F001", "error.type": "rate_limit_error",
		"error.details.code": "12345", "error.details.retryable": false, "details.reason_code": "6789",
		"message_ref": attemptIDRef("private-prompt-sentinel"), "message_bytes": len("private-prompt-sentinel"),
		"request_id": "df9e1260-d75b-4007-bc7c-9d6304ec0bd4", "logId": "-",
	} {
		if meta[key] != want {
			t.Errorf("%s=%v want %v", key, meta[key], want)
		}
	}
	encoded, _ := json.Marshal(meta)
	if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "Authorization") {
		t.Fatalf("白名单外的字段/消息内容泄漏：%s", encoded)
	}
	for _, tc := range []struct {
		raw       []byte
		truncated bool
	}{
		{[]byte(`{"code":3012`), false}, {[]byte("<html>private-body</html>"), false},
		{[]byte(`{"code":3012}`), true}, {[]byte("null"), false}, {[]byte("[]"), false},
	} {
		if fields, ok := diagnosticErrorMetadata(tc.raw, tc.truncated); ok || len(fields) != 0 {
			t.Fatalf("部分/非对象 JSON 不应猜测业务码：%v", fields)
		}
	}
}

func TestDiagnosticErrorBodyReadLimitsPreserveClassifierInput(t *testing.T) {
	for _, n := range []int{0, maxErrorBodyBytes, maxErrorBodyBytes + 1, maxErrorBodyBytes * 2} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			buf := &syncBuffer{}
			web.SetOut(buf)
			t.Cleanup(func() { web.SetOut(nil) })
			raw := strings.Repeat("x", n)
			resp := &http.Response{StatusCode: 400, ContentLength: int64(n), Body: io.NopCloser(strings.NewReader(raw))}
			diag := NewReqDiag("abcdef", "GLM-5.3", true, nil, nil)
			got, err := ReadErrorResponse(resp, diag, "error_body")
			_ = resp.Body.Close()
			if err != nil || string(got) != raw[:min(n, maxErrorBodyBytes)] {
				t.Fatal("诊断不得改变分类器接收的 64 KiB 前缀或制造错误")
			}
			logs := buf.String()
			for _, want := range []string{
				fmt.Sprintf("body_truncated=%t", n > maxErrorBodyBytes),
				fmt.Sprintf("body_read_bytes=%d", min(n, maxErrorBodyBytes+1)),
				"read_err_kind=-",
			} {
				if !strings.Contains(logs, want) {
					t.Errorf("读取边界观测缺少 %s：%s", want, logs)
				}
			}
		})
	}
}

func TestDiagnosticReadErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "-"}, {fmt.Errorf("wrapped: %w", context.Canceled), "canceled"},
		{context.DeadlineExceeded, "timeout"}, {io.ErrUnexpectedEOF, "unexpected_eof"},
		{io.EOF, "eof"}, {syscall.ECONNRESET, "connection_reset"}, {net.ErrClosed, "connection_closed"},
		{errors.New("https://user:private-secret@private.example/"), "read_failed"},
	} {
		if got := diagnosticReadError(tc.err); got != tc.want {
			t.Errorf("分类=%q want=%q", got, tc.want)
		}
	}
}
