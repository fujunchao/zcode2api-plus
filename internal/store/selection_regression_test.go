package store

import (
	"testing"
	"time"
	"zcode2api/internal/model"
)

func TestSelectAvoidKeepsOnlyUsableModel(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.AddAccount(model.ProviderZai, "flash", "test-flash")
	b, _ := s.AddAccount(model.ProviderZai, "other", "test-other")
	_, _ = s.Update(a.Provider, a.ID, func(a *model.Account) {
		a.TruncateAvoidUntil = float64(time.Now().Add(time.Minute).Unix())
		a.Quota = map[string]map[string]any{"glm-5.3-flash": {"remaining": 100.0}}
	})
	for _, state := range []string{"disabled", "absent", "exhausted"} {
		t.Run(state, func(t *testing.T) {
			_, _ = s.Update(b.Provider, b.ID, func(b *model.Account) {
				b.DisabledModels = nil
				b.Quota = map[string]map[string]any{}
				switch state {
				case "disabled":
					b.SetDisabledModels([]string{"glm-5.3-flash"})
				case "absent":
					b.Quota["GLM-5.3"] = map[string]any{"remaining": 100.0}
				case "exhausted":
					b.Quota["glm-5.3-flash"] = map[string]any{"remaining": 0.0}
				}
			})
			got := s.Select(model.ProviderZai, nil, "glm-5.3-flash")
			if got == nil || got.ID != a.ID {
				t.Fatal("短回避不能排除唯一可用模型账号")
			}
		})
	}
}
