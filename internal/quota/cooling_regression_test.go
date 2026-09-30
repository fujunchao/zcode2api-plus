package quota

import (
	"net/http"
	"testing"
	"time"
	"zcode2api/internal/model"
)

func TestQuotaZeroThenPositivePreservesCooling(t *testing.T) {
	for _, kind := range []string{model.ErrorKindRiskControl, model.ErrorKindRateLimited, model.ErrorKindUpstreamUnavailable} {
		t.Run(kind, func(t *testing.T) {
			svc, st, billing := setup(t)
			a, _ := st.AddAccount(model.ProviderZai, "cooling", "test.e30.sig")
			now := time.Now()
			svc.SetNow(func() time.Time { return now })
			until := float64(now.Add(time.Hour).Unix())
			reason := "原始冷却原因"
			_, _ = st.Update(a.Provider, a.ID, func(a *model.Account) {
				a.Status = model.StatusCooling
				a.CoolingUntil = &until
				a.LastErrorKind = &kind
				a.LastError = &reason
			})
			for _, n := range []int{0, 100} {
				billing.setStatus(http.StatusOK, balancePayload(n))
				now = now.Add(QuotaCacheTTL + time.Second)
				svc.FetchQuota(a)
				got := st.FindAny(a.ID)
				if got.Status != model.StatusCooling || got.CoolingUntil == nil || *got.CoolingUntil != until || got.LastError == nil || *got.LastError != reason {
					t.Fatalf("额度 %d 覆盖了未到期冷却：%+v", n, got)
				}
			}
		})
	}
}
