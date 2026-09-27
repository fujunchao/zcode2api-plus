package gateway

import (
	"testing"
	"time"

	"zcode2api/internal/model"
)

// 迟到的成功不得冲掉并发请求刚施加的冷却（PLAN §5.13「冷却到期后成功才归零」）。
//
// 场景：R1 在冷却前选中账号 X、仍在途；R2 在 X 上吃到风控 → cooling + streak=1；
// 随后 R1 的 200 到达。这次成功发生在冷却生效之后、到期之前，不能证明账号已恢复，
// 若照旧复位，风控阶梯对繁忙账号永远升不上去。冷却到期后的成功仍照常归零。
func TestLateSuccessKeepsUnexpiredCooling(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "late", "sk-late")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	secs, _, _ := MarkRiskControl(st, model.ProviderZai, acc.ID, "风控拦截", now)
	cooledUntil := *st.Find(model.ProviderZai, acc.ID).CoolingUntil

	MarkSuccess(st, model.ProviderZai, acc.ID, now.Add(time.Second))
	got := st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusCooling || got.CoolingUntil == nil || *got.CoolingUntil != cooledUntil {
		t.Fatalf("未到期冷却不得被迟到成功解除：status=%s until=%v", got.Status, got.CoolingUntil)
	}
	if got.RiskControlStreak != 1 {
		t.Fatalf("未到期冷却期间的成功不得清零风控连击：streak=%d", got.RiskControlStreak)
	}
	if got.UseCount != 1 {
		t.Fatalf("调用次数仍应累计：use=%d", got.UseCount)
	}

	// 冷却到期后的成功：证明账号确已恢复，照常复位并清零连击。
	MarkSuccess(st, model.ProviderZai, acc.ID, now.Add(time.Duration(secs+1)*time.Second))
	got = st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || got.RiskControlStreak != 0 {
		t.Fatalf("冷却到期后的成功应恢复 active 并清零：status=%s streak=%d", got.Status, got.RiskControlStreak)
	}
}

// 已失效账号上的迟到成功同样不得清零风控连击：失效是人工处置终态。
func TestLateSuccessKeepsInvalidStreak(t *testing.T) {
	st := openStore(t)
	acc, err := st.AddAccount(model.ProviderZai, "late-invalid", "sk-late-invalid")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var streak int
	for invalid := false; !invalid; {
		_, streak, invalid = MarkRiskControl(st, model.ProviderZai, acc.ID, "风控拦截", now)
	}
	MarkSuccess(st, model.ProviderZai, acc.ID, now.Add(time.Second))
	got := st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusInvalid || got.RiskControlStreak != streak {
		t.Fatalf("失效账号不得被迟到成功改写：status=%s streak=%d want=%d", got.Status, got.RiskControlStreak, streak)
	}
}
