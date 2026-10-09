package gateway

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
)

// TestAPIKeyQuotaRecoveryAfterUpstreamReset 从真实 HTTP 入口验证恢复闭环：
// 耗尽时不反复冲击上游，等待后重新观测成功，而不是永远返回无可用账号。
func TestAPIKeyQuotaRecoveryAfterUpstreamReset(t *testing.T) {
	f := newFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	f.eng.SetNow(func() time.Time { return time.Unix(0, clock.Load()) })
	f.respond = func(call int, _ *http.Request) (int, http.Header, string) {
		if call == 1 {
			return jsonResp(http.StatusPaymentRequired, `{"error":{"message":"quota exhausted"}}`)(call, nil)
		}
		return jsonResp(http.StatusOK, `{"id":"msg_recovered","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)(call, nil)
	}
	acc, err := f.st.AddAccount(model.ProviderZai, "quota-recovery", "sk-synthetic-recovery")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		status, body := f.post(t, msgBody(), "sk-test")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("耗尽后应暂时排除模型，得到 %d: %s", status, body)
		}
	}
	if got := f.callCount(); got != 1 {
		t.Fatalf("等待窗口内不应重复访问上游，得到 %d 次", got)
	}
	clock.Add(int64(25 * time.Hour))
	status, body := f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK {
		t.Fatalf("上游额度恢复且等待窗口已过，模型应恢复调用，得到 %d: %s", status, body)
	}
	if got := f.st.SnapshotAccount(model.ProviderZai, acc.ID); got.ModelAvailability("GLM-5.3") == "exhausted" {
		t.Fatal("成功探测后仍保留该模型的耗尽标记")
	}
	status, body = f.post(t, msgBody(), "sk-test")
	if status != http.StatusOK || f.callCount() != 3 {
		t.Fatalf("成功后应回到正常调度，得到 %d、调用 %d 次: %s", status, f.callCount(), body)
	}
}
