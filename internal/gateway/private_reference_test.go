package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticReferenceIsKeyedAndExpires(t *testing.T) {
	referenceKeys.Lock()
	key, expires := referenceKeys.key, referenceKeys.expires
	referenceKeys.expires = time.Time{}
	referenceKeys.Unlock()
	t.Cleanup(func() {
		referenceKeys.Lock()
		referenceKeys.key, referenceKeys.expires = key, expires
		referenceKeys.Unlock()
	})
	now := time.Now()
	first := privateReference("low-entropy-user-1", now)
	sum := sha256.Sum256([]byte("low-entropy-user-1"))
	if len(first) != 64 || first == hex.EncodeToString(sum[:]) || first != privateReference("low-entropy-user-1", now.Add(time.Minute)) {
		t.Fatal("引用必须为稳定的密钥HMAC，不能直接使用可枚举的SHA")
	}
	if first == privateReference("low-entropy-user-1", now.Add(referenceLifetime)) || privateReference("", now) != "" {
		t.Fatal("到期后不得跨周期关联，空值不能制造身份")
	}
}

func TestDiagnosticIngressAttributionUsesOnlyBoundedReferences(t *testing.T) {
	headers := http.Header{"X-Session-Id": {"private-session"}, "X-Zcode-Trace-Id": {"private-trace"}, "X-Query-Id": {"first", "second"}, "Authorization": {"Bearer private-secret"}}
	got := ingressAttributionSummary(headers)
	for _, required := range []string{"incoming_session_present=true", "incoming_trace_ref=\"" + attemptIDRef("private-trace") + "\"", "incoming_query_multiple=true", "incoming_query_ref=\"-\"", "incoming_query_source_present=false"} {
		if !strings.Contains(got, required) {
			t.Fatalf("缺少归因存在性或引用: %s", required)
		}
	}
	if strings.Contains(got, "private-") || strings.Contains(got, "Bearer") {
		t.Fatal("归因诊断不得泄漏来值或鉴权")
	}
}
