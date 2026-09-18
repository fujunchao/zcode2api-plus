// 领取冷却与串行闸门的单测：这部分只影响本地调度决策，不涉及上游协议。
package adminapi

import (
	"encoding/base64"
	"net/http"
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
	// 入池自动领取是 fire-and-forget：等它们收尾再关库（cleanup 是 LIFO）。
	t.Cleanup(autoClaimTasks.Wait)
	return st
}

func TestClaimCooldownUntil(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	cases := []struct {
		name     string
		outcomes []map[string]any
		captcha  int
		retry    int
		want     float64
	}{
		{"上游 ends_at 优先", []map[string]any{{"ok": false, "next_at": 1700007200.0, "code": 1005}}, 3600, 600, 1700007200},
		{"验证码失败取长档", []map[string]any{{"ok": false, "captcha": true}}, 3600, 600, 1700000000 + 3600},
		{"1005 无时间取长档", []map[string]any{{"ok": false, "code": 1005}}, 3600, 600, 1700000000 + 3600},
		{"1003 无时间取长档", []map[string]any{{"ok": false, "code": 1003}}, 3600, 600, 1700000000 + 3600},
		{"3007 取长档", []map[string]any{{"ok": false, "code": 3007}}, 3600, 600, 1700000000 + 3600},
		{"网络失败取短档", []map[string]any{{"ok": false, "message": "上游網路錯誤"}}, 3600, 600, 1700000000 + 600},
		{"无 outcome 取短档", nil, 3600, 600, 1700000000 + 600},
		{"时长来自后台设置", nil, 90, 45, 1700000000 + 45},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := claimCooldownUntil(c.outcomes, now, c.captcha, c.retry)
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

// TestApplyClaimOutcomeUsesStoreCooldown 冷却时长取自后台设置而非 config 包级变量。
func TestApplyClaimOutcomeUsesStoreCooldown(t *testing.T) {
	st := newClaimStore(t)
	h := &Handler{Store: st}
	if err := st.SetSetting("claim_retry_cooldown", "90"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	acc, err := st.AddAccount(model.ProviderZai, "a1", jwtTokenFor("u-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	h.applyClaimOutcome(acc, []map[string]any{{"ok": false, "message": "上游網路錯誤"}}, now)
	reloaded := st.Find(model.ProviderZai, acc.ID)
	if reloaded.Claim == nil || reloaded.Claim.NextAt == nil {
		t.Fatalf("next_at 未落盘: %+v", reloaded.Claim)
	}
	if want := float64(now.Unix() + 90); *reloaded.Claim.NextAt != want {
		t.Fatalf("冷却应取后台设置的 90s: got %v want %v", *reloaded.Claim.NextAt, want)
	}
}

func TestShouldFireClaim(t *testing.T) {
	// 本地时间 2026-09-16 23:00:30
	at := time.Date(2026, 9, 16, 23, 0, 30, 0, time.Local)
	if !shouldFireClaim(at, "", "23:00", true) {
		t.Fatal("到点且当天未触发过应触发")
	}
	if shouldFireClaim(at, "2026-09-16", "23:00", true) {
		t.Fatal("同一天已触发过不应重复")
	}
	if shouldFireClaim(at, "", "23:00", false) {
		t.Fatal("开关关闭不应触发")
	}
	if shouldFireClaim(at, "", "25:00", true) {
		t.Fatal("目标时间非法不应触发")
	}
	if shouldFireClaim(at.Add(-time.Minute), "", "23:00", true) {
		t.Fatal("未到点不应触发")
	}
}

// TestScheduleAutoClaimRespectsToggle 开关关闭时自动路径必须完全不动作。
// 正向路径需要真上游，由 claim 包与在线验收覆盖，这里只验证「关得住」。
func TestScheduleAutoClaimRespectsToggle(t *testing.T) {
	st := newClaimStore(t)
	h := &Handler{Store: st}
	acc, err := st.AddAccount(model.ProviderZai, "a1", jwtTokenFor("u-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if err := st.SetSetting("claim_auto_enabled", "false"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	h.scheduleAutoClaim(acc)
	autoClaimTasks.Wait()
	if got := st.Find(model.ProviderZai, acc.ID); got.Claim != nil {
		t.Fatalf("开关关闭时不应产生领取状态: %+v", got.Claim)
	}
}

// TestRunScheduledClaimsSkipsCooling 定时批量必须尊重 claim.next_at：
// 刚领过的账号（next_at 在未来）直接跳过，不产生任何上游调用。
func TestRunScheduledClaimsSkipsCooling(t *testing.T) {
	st := newClaimStore(t)
	h := &Handler{Store: st}
	acc, err := st.AddAccount(model.ProviderZai, "a1", jwtTokenFor("u-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	future := float64(time.Now().Add(time.Hour).Unix())
	if _, err := st.UpdateClaimState(model.ProviderZai, acc.ID, func(*model.ClaimState) *model.ClaimState {
		return &model.ClaimState{NextAt: &future}
	}); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	h.runScheduledClaims(time.Now())

	got := st.Find(model.ProviderZai, acc.ID)
	if got.Claim == nil || got.Claim.NextAt == nil || *got.Claim.NextAt != future {
		t.Fatalf("冷却中的账号不应被改动: %+v", got.Claim)
	}
}

// TestClaimSettingsFlow 后台设置页领取项的读写与校验（改完即生效，无需重启）。
func TestClaimSettingsFlow(t *testing.T) {
	mux, st, _ := setup(t)

	// 默认值：入池开、定时关、时间 23:00。
	code, body := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("读取设置应 200: %d", code)
	}
	if b, _ := body["claim_auto_enabled"].(bool); !b {
		t.Fatalf("入池自动领取默认应开: %v", body)
	}
	if b, _ := body["claim_schedule_enabled"].(bool); b {
		t.Fatalf("定时领取默认应关: %v", body)
	}
	if str(t, body["claim_schedule_time"]) != "23:00" {
		t.Fatalf("定时点默认应 23:00: %v", body)
	}

	// 写入并回读。
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"claim_auto_enabled":     false,
		"claim_schedule_enabled": true,
		"claim_schedule_time":    "08:15",
		"claim_captcha_cooldown": 120,
		"claim_preview_cooldown": 0,
	})
	if code != http.StatusOK {
		t.Fatalf("保存设置应 200: %d", code)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if b, _ := body["claim_auto_enabled"].(bool); b {
		t.Fatalf("入池开关应已关: %v", body)
	}
	if b, _ := body["claim_schedule_enabled"].(bool); !b {
		t.Fatalf("定时开关应已开: %v", body)
	}
	if str(t, body["claim_schedule_time"]) != "08:15" {
		t.Fatalf("定时点应 08:15: %v", body)
	}
	if num(t, body["claim_captcha_cooldown"]) != 120 || num(t, body["claim_preview_cooldown"]) != 0 {
		t.Fatalf("冷却应生效: %v", body)
	}

	// 校验：非法时间 400；负数钳到下限；非数字 400。
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{"claim_schedule_time": "25:00"})
	if code != http.StatusBadRequest {
		t.Fatalf("非法时间应 400: %d", code)
	}
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{"claim_retry_cooldown": -5})
	if code != http.StatusOK {
		t.Fatalf("负数应钳到下限而非报错: %d", code)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if num(t, body["claim_retry_cooldown"]) != 30 {
		t.Fatalf("负数应钳到下限 30: %v", body)
	}
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{"claim_retry_cooldown": "abc"})
	if code != http.StatusBadRequest {
		t.Fatalf("非数字应 400: %d", code)
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
