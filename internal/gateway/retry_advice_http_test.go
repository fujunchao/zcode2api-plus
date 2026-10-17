package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"

	"zcode2api/internal/model"
)

// 初次已用完风险尝试预算的405和随后本地503都必须阻止同范围立即重投。
//
// 阈值临时置 1：让首次 405 直接落进「请求级」分支（这是现在唯一会原样返回
// 上游 405 的路径）；默认阈值 3 时同线多账号会继续换号，不再产生 405。
func TestRetryAdviceCoversRiskExhaustionAndReplay(t *testing.T) {
	old := RiskControlRequestLevelThreshold
	RiskControlRequestLevelThreshold = 1
	t.Cleanup(func() { RiskControlRequestLevelThreshold = old })

	f := newFixture(t)
	f.respond = jsonResp(405, `{"code":"3012","message":"Request has been blocked due to unusual activity."}`)
	for _, name := range []string{"one", "two"} {
		if _, err := f.st.AddAccount(model.ProviderZai, name, "sk-synthetic-"+name); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(msgBody())
	for _, wantStatus := range []int{405, 503} {
		req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/v1/messages", bytes.NewReader(body))
		req.Header.Set("X-Api-Key", "sk-test")
		resp, err := f.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Fatalf("状态不应被等待建议改写: got=%d want=%d", resp.StatusCode, wantStatus)
		}
		seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
		if err != nil || seconds <= 0 || resp.Header.Get("X-Zcode2api-Retry-Policy") != "wait_same_gateway" {
			t.Fatalf("需要明确等待建议: Retry-After=%q policy=%q", resp.Header.Get("Retry-After"), resp.Header.Get("X-Zcode2api-Retry-Policy"))
		}
	}
	// 阈值 1 时首个账号即触发请求级：整轮只出站一次，第二次入站零出站。
	if f.callCount() != 1 {
		t.Fatalf("第二次入站必须零出站，实际总数=%d", f.callCount())
	}
}
