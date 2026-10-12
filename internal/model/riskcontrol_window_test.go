// 风控静默期（LastRiskControlAt）的字段语义测试：判定边界、静默期终点换算、
// 升级迁移回填与 Clone 复制。
//
// 背景（规格见 docs/plan-riskcontrol-silent-window-20261012.md）：2026-10-12
// 晚「池卫生」事故——上游对触发风控的账号打服务端标记，标记期内一切请求 405；
// 本地 ladder 冷却（分钟级）远短于标记时长（实测 ≥14h），选号硬闸门与恢复护栏
// 都以本字段为判据。
package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestInRiskControlSilentWindowBoundaries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	window := 24 * time.Hour

	if (&Account{ID: "x"}).InRiskControlSilentWindow(now, window) {
		t.Fatal("无命中记录的账号不在静默期")
	}

	recent := now.Add(-1 * time.Hour)
	stamp := float64(recent.UnixNano()) / 1e9
	hot := &Account{ID: "hot", LastRiskControlAt: &stamp}
	if !hot.InRiskControlSilentWindow(now, window) {
		t.Fatal("1 小时前命中过，24h 静默期内应为 true")
	}

	// 边界：恰好到达窗口终点即放行（「不足 window」的严格语义）。
	edge := now.Add(-window)
	edgeStamp := float64(edge.UnixNano()) / 1e9
	if (&Account{ID: "edge", LastRiskControlAt: &edgeStamp}).InRiskControlSilentWindow(now, window) {
		t.Fatal("恰好在窗口终点应放行")
	}
	justIn := now.Add(-window + time.Second)
	justStamp := float64(justIn.UnixNano()) / 1e9
	if !(&Account{ID: "just", LastRiskControlAt: &justStamp}).InRiskControlSilentWindow(now, window) {
		t.Fatal("窗口内 1 秒仍应拦截")
	}

	old := now.Add(-25 * time.Hour)
	oldStamp := float64(old.UnixNano()) / 1e9
	if (&Account{ID: "old", LastRiskControlAt: &oldStamp}).InRiskControlSilentWindow(now, window) {
		t.Fatal("25 小时前的命中已过静默期")
	}

	// 应急回退：window <= 0 = 关闭闸门与恢复护栏。
	if hot.InRiskControlSilentWindow(now, 0) || hot.InRiskControlSilentWindow(now, -time.Hour) {
		t.Fatal("window <= 0 应关闭静默期判定")
	}

	// nil 接收者安全。
	var nilAcc *Account
	if nilAcc.InRiskControlSilentWindow(now, window) {
		t.Fatal("nil 账号不应命中")
	}
}

func TestRiskControlSilentUntil(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	window := 24 * time.Hour

	if until := (&Account{ID: "x"}).RiskControlSilentUntil(window); until != nil {
		t.Fatalf("无记录应返回 nil: %v", until)
	}
	stampF := float64(now.UnixNano()) / 1e9
	hot := &Account{ID: "hot", LastRiskControlAt: &stampF}
	until := hot.RiskControlSilentUntil(window)
	if until == nil {
		t.Fatal("有记录应给出静默期终点")
	}
	want := float64(now.Add(window).UnixNano()) / 1e9
	if *until != want {
		t.Fatalf("终点应为命中时刻 + window: got %v want %v", *until, want)
	}
	if until := hot.RiskControlSilentUntil(0); until != nil {
		t.Fatalf("window <= 0 应返回 nil: %v", until)
	}
}

// 升级迁移：字段上线前的旧数据没有 last_risk_control_at 键，normalize 用
// last_error_at 回填——仅当错误归类仍是风控；其它成因不得拿来做静默期证据。
func TestNormalizeBackfillsLastRiskControlAt(t *testing.T) {
	base := func(kind string, at float64) string {
		payload := map[string]any{
			"id": "acc-mig", "name": "mig", "provider": "zai", "mode": "jwt",
			"enabled": true, "status": "cooling",
			"last_error_kind": kind, "last_error_at": at,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	at := float64(time.Now().Add(-2*time.Hour).UnixNano()) / 1e9

	// 风控归类 → 回填。
	acc, err := FromJSON([]byte(base(ErrorKindRiskControl, at)))
	if err != nil {
		t.Fatal(err)
	}
	if acc.LastRiskControlAt == nil || *acc.LastRiskControlAt != at {
		t.Fatalf("风控归类的旧数据应回填 last_error_at: %v", acc.LastRiskControlAt)
	}

	// 其它归类 → 不回填（last_error_at 与上游标记无关）。
	acc, err = FromJSON([]byte(base(ErrorKindAuthFailed, at)))
	if err != nil {
		t.Fatal(err)
	}
	if acc.LastRiskControlAt != nil {
		t.Fatalf("非风控归类不得回填: %v", acc.LastRiskControlAt)
	}

	// 已有值 → 原样保留，不被 last_error_at 覆盖。
	payload := map[string]any{
		"id": "acc-keep", "name": "keep", "provider": "zai", "mode": "jwt",
		"enabled": true, "status": "cooling",
		"last_error_kind": ErrorKindRiskControl, "last_error_at": at,
		"last_risk_control_at": at - 60,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	acc, err = FromJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if acc.LastRiskControlAt == nil || *acc.LastRiskControlAt != at-60 {
		t.Fatalf("已有值不得被覆盖: %v", acc.LastRiskControlAt)
	}
}

// Clone 必须复制静默期字段：Select 交给网关的是副本，闸门判定读副本上的字段。
func TestCloneCopiesLastRiskControlAt(t *testing.T) {
	a := Create(ProviderZai, "k", "sk-abc")
	stamp := 1790000000.5
	a.LastRiskControlAt = &stamp

	clone := a.Clone()
	if clone.LastRiskControlAt == nil || *clone.LastRiskControlAt != stamp {
		t.Fatalf("Clone 未复制 LastRiskControlAt: %v", clone.LastRiskControlAt)
	}
	other := 1.0
	clone.LastRiskControlAt = &other
	if *a.LastRiskControlAt != stamp {
		t.Fatal("Clone 后改副本不应影响原件")
	}
}
