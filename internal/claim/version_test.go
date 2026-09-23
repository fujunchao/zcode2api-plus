// 伪装版本号「单一来源」锁定：把 config.ZcodeClientVersion 换成哨兵值后，
// 所有上行点（事件体 / 计费头 / UA / claim 头 / preview query）必须跟着变。
// 任何一处写死字面量，本用例即变红——这是版本号升级的回归护栏。
package claim

import (
	"net/http"
	"net/url"
	"testing"

	"zcode2api/internal/config"
)

// queryRecorder 代理 fakeBilling，额外记录每次请求的完整 query。
type queryRecorder struct {
	inner   *fakeBilling
	queries []url.Values
}

func (q *queryRecorder) Do(r *http.Request) (*http.Response, error) {
	q.queries = append(q.queries, r.URL.Query())
	return q.inner.Do(r)
}

func TestClientVersionSingleSource(t *testing.T) {
	oldVer, oldUA := config.ZcodeClientVersion, config.UserAgent
	const sentinel = "9.9.9-sentinel"
	config.ZcodeClientVersion = sentinel
	config.UserAgent = "ZCode/" + sentinel
	t.Cleanup(func() { config.ZcodeClientVersion, config.UserAgent = oldVer, oldUA })

	acc := newTestAccount(t)

	if got := BuildActivationEventBody("app_launch", "u1", "mid-1")["app_version"]; got != sentinel {
		t.Fatalf("激活事件体 app_version 未取自常量: %v", got)
	}
	if got := authHeaders(acc)["X-ZCode-App-Version"]; got != sentinel {
		t.Fatalf("billing 鉴权头版本未取自常量: %v", got)
	}
	if got := authHeaders(acc)["User-Agent"]; got != "ZCode/"+sentinel {
		t.Fatalf("User-Agent 未随版本常量变化: %v", got)
	}
	if got := claimHeaders(acc, "vp", "sgp")["X-ZCode-App-Version"]; got != sentinel {
		t.Fatalf("claim 版本头未取自常量: %v", got)
	}

	up := &fakeBilling{responses: map[string]func(upstreamCall) (int, string){
		"/billing/preview": func(upstreamCall) (int, string) {
			return http.StatusOK, `{"code":0,"data":{"plans":[]}}`
		},
	}}
	rec := &queryRecorder{inner: up}
	svc := &Service{Client: rec}
	if _, err := svc.PreviewPlans(acc); err != nil {
		t.Fatalf("preview 不应报错: %v", err)
	}
	if len(rec.queries) != 1 {
		t.Fatalf("应恰好 1 次 preview 请求，实得 %d", len(rec.queries))
	}
	if got := rec.queries[0].Get("app_version"); got != sentinel {
		t.Fatalf("preview 的 app_version 未取自常量: %q", got)
	}
	if got := rec.queries[0].Get("platform"); got != config.ZcodeClientPlatform {
		t.Fatalf("preview 的 platform 不符: %q", got)
	}
}
