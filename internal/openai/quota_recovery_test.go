package openai

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func TestAPIKeyQuotaRecoveryThroughOpenAIAdapters(t *testing.T) {
	for _, route := range []string{"Chat", "Responses"} {
		t.Run(route, func(t *testing.T) {
			f := newFixture(t)
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			f.eng.SetNow(func() time.Time { return time.Unix(0, clock.Load()) })
			a := f.st.Select(model.ProviderZai, nil, "GLM-5.3")
			f.setResponder(func(call int) (int, string, string) {
				if call == 1 {
					return 402, "application/json", `{"error":{"message":"quota exhausted"}}`
				}
				return 200, "application/json", `{"id":"msg_recovered","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
			})
			request := func() (int, string) {
				if route == "Chat" {
					return f.postChat(t, "sk-test", `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}`)
				}
				return post(f, t, "sk-test", `{"model":"GLM-5.3","input":"hi"}`)
			}
			if status, body := request(); status != http.StatusServiceUnavailable {
				t.Fatalf("首次模型耗尽应返回不可用: %d %s", status, body)
			}
			clock.Add(int64(5 * time.Minute))
			if status, body := request(); status != http.StatusOK {
				t.Fatalf("适配层应复用网关的恢复探测: %d %s", status, body)
			}
			if f.st.SnapshotAccount(a.Provider, a.ID).ModelAvailability("GLM-5.3") == "exhausted" {
				t.Fatal("适配成功后未解除目标模型标记")
			}
		})
	}
}
