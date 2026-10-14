package adminapi

import (
	"testing"
	"time"

	"zcode2api/internal/model"
)

func TestClaimLateOutcomeCannotOverwriteNewCredential(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(map[bool]string{false: "旧失败", true: "旧成功"}[success], func(t *testing.T) {
			st := newClaimStore(t)
			a, err := st.AddAccount(model.ProviderZai, "合成账号", jwtTokenFor("synthetic-old"))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := st.FindAny(a.ID)
			newToken := jwtTokenFor("synthetic-new")
			nextAt := float64(1800000100)
			marker := "new-credential-marker"
			_, _ = st.Update(a.Provider, a.ID, func(live *model.Account) {
				live.JWTToken = &newToken
				live.SetClaimState(&model.ClaimState{NextAt: &nextAt, LastError: &marker})
			})
			h := &Handler{Store: st}
			h.applyClaimOutcome(snapshot, []map[string]any{{"ok": success, "message": "stale-error"}}, time.Unix(1800000000, 0))
			got := st.FindAny(a.ID).ClaimView()
			if got == nil || got.NextAt == nil || *got.NextAt != nextAt || got.LastError == nil || *got.LastError != marker || got.ClaimedAt != nil {
				t.Fatal("旧凭据的领取结果覆盖了新登录状态")
			}
		})
	}
}
