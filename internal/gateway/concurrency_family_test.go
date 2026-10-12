package gateway

import (
	"fmt"
	"strings"
	"testing"

	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

func TestModelConcurrencyFamilyKeepsAccountUsable(t *testing.T) {
	for _, code := range []int{3008, 3009, 3010} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newFixture(t)
			body := fmt.Sprintf(`{"code":%d,"msg":"model concurrency limit exceeded"}`, code)
			f.respond = jsonResp(429, body)
			a, err := f.st.AddAccount(model.ProviderZai, "busy", "synthetic-busy-secret")
			if err != nil {
				t.Fatal(err)
			}
			buf := &syncBuffer{}
			web.SetOut(buf)
			t.Cleanup(func() { web.SetOut(nil) })
			status, response := f.post(t, msgBody(), "sk-test")
			if status != 429 || !strings.Contains(response, fmt.Sprint(code)) {
				t.Fatalf("并发重试用尽应保留上游码而非无账号503: %d %s", status, response)
			}
			got := f.st.FindAny(a.ID)
			if got.Status != model.StatusActive || got.CoolingUntil != nil || got.LastRiskControlAt != nil || got.RateLimitStreak != 0 {
				t.Fatal("并发限制不能变成整号冷却、风控或额度耗尽")
			}
			if f.callCount() != 3 || got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindModelBusy {
				t.Fatal("应使用独立的有限并发重试预算和错误分类")
			}
			if got.LastError == nil || !strings.Contains(*got.LastError, fmt.Sprint(code)) {
				t.Fatal("错误证据不能把3008/3009硬写成3010")
			}
		})
	}
}
