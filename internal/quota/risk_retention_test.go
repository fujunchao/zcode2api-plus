package quota

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// 不能只测试「风控失效后立即查额度成功」：先失败/幂等/耗尽，再成功才会暴露
// 最近错误覆盖封禁原因的问题；重开数据库保证保护不依赖运行期 streak。
func TestQuotaObservationsPreserveRiskInvalid(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
	}{
		{"查询500", http.StatusInternalServerError, "temporary failure"},
		{"查询401", http.StatusUnauthorized, "unauthorized"},
		{"查询405幂等", http.StatusMethodNotAllowed, "duplicate query"},
		{"查询405风控", http.StatusMethodNotAllowed, "blocked due to unusual activity"},
		{"无效JSON", http.StatusOK, "not-json"},
		{"业务错误", http.StatusOK, `{"code":1005,"msg":"failed"}`},
		{"空额度", http.StatusOK, `{"code":0,"data":{"plans":[],"balances":[]}}`},
		{"额度耗尽", http.StatusOK, balancePayload(0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, st, billing := setup(t)
			acc, err := st.AddAccount(model.ProviderZai, "risk-invalid", "header.payload.signature")
			if err != nil {
				t.Fatal(err)
			}
			kind, reason, stamp := model.ErrorKindRiskControl, "风控失效，等待人工处理", float64(123)
			if _, err := st.Update(acc.Provider, acc.ID, func(a *model.Account) {
				a.Status = model.StatusInvalid
				a.LastErrorKind, a.LastError, a.LastErrorAt = &kind, &reason, &stamp
				a.RiskControlStreak = 4
				a.Quota = map[string]map[string]any{"GLM-5.3-Flash": {"remaining": float64(2500000)}}
			}); err != nil {
				t.Fatal(err)
			}
			billing.setStatus(tt.status, tt.body)
			svc.FetchQuota(acc)
			assertRetained := func(s *store.Store) {
				t.Helper()
				got := s.FindAny(acc.ID)
				if got.Status != model.StatusInvalid || got.LastErrorKind == nil || *got.LastErrorKind != kind ||
					got.LastError == nil || *got.LastError != reason || got.LastErrorAt == nil || *got.LastErrorAt != stamp {
					t.Errorf("额度观测覆盖了风控失效证据：status=%s kind=%v reason=%v at=%v",
						got.Status, got.LastErrorKind, got.LastError, got.LastErrorAt)
				}
				if got.LastCheckedAt == nil {
					t.Error("保留封禁不能阻止更新额度检查时间")
				}
				if got.IsSelectable(time.Now()) {
					t.Error("风控失效账号不得重新进入调度")
				}
			}
			assertRetained(st)

			reopened, err := store.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			svc = NewService(reopened)
			svc.Client = billing
			billing.setStatus(http.StatusOK, balancePayload(2500000))
			result := svc.FetchQuota(reopened.FindAny(acc.ID))
			if _, failed := result["error"]; failed {
				t.Fatalf("本次额度查询应成功：%v", result)
			}
			assertRetained(reopened)
		})
	}
}
