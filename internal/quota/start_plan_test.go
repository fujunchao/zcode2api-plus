package quota

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

func TestMissingInitialQuotaEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		missing    bool
	}{
		{"空额度", `{"plans":[],"balances":[]}`, true},
		{"零额度条目", `{"plans":[],"balances":[{"total_units":0,"used_units":0,"remaining_units":0}]}`, true},
		{"字段缺失", `{}`, false},
		{"字段为null", `{"plans":null,"balances":[]}`, false},
		{"已有Start套餐", `{"plans":[{"plan_id":"start-plan"}],"balances":[]}`, false},
		{"已用尽", `{"plans":[],"balances":[{"total_units":100,"used_units":100,"remaining_units":0}]}`, false},
		{"正额度", `{"plans":[],"balances":[{"total_units":0,"remaining_units":100}]}`, false},
		{"余额未知", `{"plans":[],"balances":[{}]}`, false},
		{"条目异常", `{"plans":[],"balances":[null]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data map[string]any
			_ = json.Unmarshal([]byte(tc.body), &data)
			if got := missingInitialQuota(data); got != tc.missing {
				t.Fatalf("missing=%v want=%v", got, tc.missing)
			}
		})
	}
}

func TestLateQuotaResponseDoesNotEstablishEvidenceForNewProxy(t *testing.T) {
	svc, st, _ := setup(t)
	old, _ := st.AddProxyProfile("old", "http://127.0.0.1:18080", true)
	next, _ := st.AddProxyProfile("next", "http://127.0.0.1:18081", true)
	a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.signature")
	_, _ = st.AssignProxyProfile(a.ID, old.ID)
	snap := st.FindAny(a.ID)
	_, _ = st.AssignProxyProfile(a.ID, next.ID)
	svc.handleBillingResponse(snap, 1700000000, &http.Response{StatusCode: 200,
		Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"plans":[],"balances":[]}}`))})
	if st.FindAny(a.ID).MissingStartPlanEvidence() {
		t.Fatal("旧出口的迟到空额度响应不得归因到新代理")
	}
}

func TestQuotaFailureClearsMissingStartPlanEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"网络响应失败", 500, `{}`},
		{"业务错误", 200, `{"code":3012,"msg":"blocked"}`},
		{"非JSON", 200, `invalid`},
		{"幂等405", 405, `already queried`},
		{"缺字段", 200, `{"code":0,"data":{}}`},
		{"缺成功码", 200, `{"data":{"plans":[],"balances":[]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, billing := setup(t)
			a, _ := st.AddAccount(model.ProviderZai, "new", "header.payload.signature")
			billing.status = 200
			billing.body = `{"code":0,"data":{"plans":[],"balances":[]}}`
			svc.FetchQuota(a)
			if !st.FindAny(a.ID).MissingStartPlanEvidence() {
				t.Fatal("成功空快照应建立证据")
			}
			billing.status = tc.status
			billing.body = tc.body
			svc.FetchQuota(st.FindAny(a.ID))
			if st.FindAny(a.ID).MissingStartPlanEvidence() {
				t.Fatal("后续查询失败/未知响应必须废弃旧证据")
			}
		})
	}
}
