// 领取冷却与串行闸门的单测：这部分只影响本地调度决策，不涉及上游协议。
package adminapi

import (
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// jwtTokenFor 构造 payload 含指定 user_id 的 JWT。
func jwtTokenFor(userID string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"` + userID + `"}`))
	return "header." + enc + ".sig"
}

// newClaimStore 隔离数据目录的存储（DeviceMid 会落盘到 DataDir）。
func newClaimStore(t *testing.T) *store.Store {
	t.Helper()
	oldDB, oldData := config.DBPath, config.DataDir
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.DataDir = t.TempDir()
	t.Cleanup(func() { config.DBPath, config.DataDir = oldDB, oldData })
	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestClaimCooldownUntil(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	oldCaptcha, oldRetry := config.ClaimCaptchaCooldownSeconds, config.ClaimRetryCooldownSeconds
	config.ClaimCaptchaCooldownSeconds = 3600
	config.ClaimRetryCooldownSeconds = 600
	t.Cleanup(func() {
		config.ClaimCaptchaCooldownSeconds, config.ClaimRetryCooldownSeconds = oldCaptcha, oldRetry
	})

	cases := []struct {
		name     string
		outcomes []map[string]any
		want     float64
	}{
		{"上游 ends_at 优先", []map[string]any{{"ok": false, "next_at": 1700007200.0, "code": 1005}}, 1700007200},
		{"验证码失败取长档", []map[string]any{{"ok": false, "captcha": true}}, 1700000000 + 3600},
		{"1005 无时间取长档", []map[string]any{{"ok": false, "code": 1005}}, 1700000000 + 3600},
		{"1003 无时间取长档", []map[string]any{{"ok": false, "code": 1003}}, 1700000000 + 3600},
		{"3007 取长档", []map[string]any{{"ok": false, "code": 3007}}, 1700000000 + 3600},
		{"网络失败取短档", []map[string]any{{"ok": false, "message": "上游網路錯誤"}}, 1700000000 + 600},
		{"无 outcome 取短档", nil, 1700000000 + 600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := claimCooldownUntil(c.outcomes, now)
			if got == nil || *got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestClaimCooldownActive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	acc := &model.Account{}
	if claimCooldownActive(acc, now) {
		t.Fatal("无领取状态不应算冷却中")
	}
	past := float64(now.Unix()) - 1
	acc.Claim = &model.ClaimState{NextAt: &past}
	if claimCooldownActive(acc, now) {
		t.Fatal("已到期不应算冷却中")
	}
	future := float64(now.Unix()) + 60
	acc.Claim = &model.ClaimState{NextAt: &future}
	if !claimCooldownActive(acc, now) {
		t.Fatal("未到期应算冷却中")
	}
}

func TestApplyClaimOutcomePersists(t *testing.T) {
	st := newClaimStore(t)
	h := &Handler{Store: st}
	acc, err := st.AddAccount(model.ProviderZai, "a1", jwtTokenFor("u-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)

	// 成功：写入 claimed_at 与上游的 next_at，并清掉上次错误。
	msg := "旧错误"
	acc.Claim = &model.ClaimState{LastError: &msg}
	h.applyClaimOutcome(acc, []map[string]any{{"ok": true, "next_at": 1700007200.0}}, now)

	reloaded := st.Find(model.ProviderZai, acc.ID)
	if reloaded.Claim == nil || reloaded.Claim.ClaimedAt == nil {
		t.Fatalf("claim.claimed_at 未落盘: %+v", reloaded.Claim)
	}
	if *reloaded.Claim.ClaimedAt != float64(now.Unix()) {
		t.Fatalf("claimed_at 不符: %v", *reloaded.Claim.ClaimedAt)
	}
	if reloaded.Claim.NextAt == nil || *reloaded.Claim.NextAt != 1700007200 {
		t.Fatalf("claim.next_at 未按上游值落盘: %+v", reloaded.Claim)
	}
	if reloaded.Claim.LastError != nil {
		t.Fatalf("成功后应清空 last_error: %v", *reloaded.Claim.LastError)
	}

	// 失败：记录错误并设置冷却，claimed_at 保持不变。
	h.applyClaimOutcome(reloaded, []map[string]any{
		{"ok": false, "code": 1005, "message": "今日領取名額已用完"},
	}, now)
	after := st.Find(model.ProviderZai, acc.ID)
	if after.Claim.LastError == nil || *after.Claim.LastError != "今日領取名額已用完" {
		t.Fatalf("失败原因未落盘: %+v", after.Claim)
	}
	if after.Claim.NextAt == nil || *after.Claim.NextAt <= float64(now.Unix()) {
		t.Fatalf("失败后应设置冷却: %+v", after.Claim)
	}
	if after.Claim.ClaimedAt == nil || *after.Claim.ClaimedAt != float64(now.Unix()) {
		t.Fatalf("失败不应清除 claimed_at: %+v", after.Claim)
	}
}

func TestClaimGateIsExclusive(t *testing.T) {
	// 用独立的闸门实例断言机制本身；全局闸门的状态会被其他用例触发的
	// 入池自动领取占据（fire-and-forget goroutine），直接断言会变得不确定。
	g := newClaimGate()
	if !g.acquire() {
		t.Fatal("闸门空闲时应能抢到")
	}
	if g.acquire() {
		t.Fatal("闸门被占用时不应抢到")
	}
	g.release()
	if !g.acquire() {
		t.Fatal("释放后应能重新抢到")
	}
	g.release()

	// 全局闸门必须是容量 1 的 channel（不断言 len：可能正被后台领取任务占用）。
	if cap(claimSlot) != 1 {
		t.Fatalf("全局闸门容量应为 1: %d", cap(claimSlot))
	}
}
