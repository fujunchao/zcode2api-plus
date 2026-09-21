// 上游 503 递进冷却与 no_available_account 池状态分解的测试。
// 背景：2026-09-20 线上事故——503 固定冷却 300s 把整池清空、静态文案误导排查
// （docs/analysis-503-no-available-account-20260920.md）。
package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 递进冷却阶梯：连续收到 503 才逐级加重，任意一次成功调用后回到最低档。
// 与 TestTransientRateLimitCoolingEscalates 同构，但超阶梯**不升 invalid**（503 是
// 上游健康信号，封顶冷却即可，与风控的升级语义不同）。
func TestUpstream503CoolingEscalates(t *testing.T) {
	st := openStore(t)
	acc, _ := st.AddAccount(model.ProviderZai, "a", "sk-1")
	now := time.Now()

	want := []int{30, 60, 120, config.CoolingSeconds}
	for i, secs := range want {
		got, streak := MarkUpstreamUnavailable(st, model.ProviderZai, acc.ID, "上游服務不可用 HTTP 503", now)
		if got != secs {
			t.Fatalf("第 %d 次 503 冷却应为 %ds，实得 %ds", i+1, secs, got)
		}
		if streak != i+1 {
			t.Fatalf("连续计数应递增到 %d: %d", i+1, streak)
		}
	}
	// 超出阶梯长度后封顶在 CoolingSeconds，账号不得被判失效
	for range 3 {
		got, _ := MarkUpstreamUnavailable(st, model.ProviderZai, acc.ID, "上游服務不可用 HTTP 503", now)
		if got != config.CoolingSeconds {
			t.Fatalf("超出阶梯应封顶在 %ds: %d", config.CoolingSeconds, got)
		}
	}
	if accLive := st.Find(model.ProviderZai, acc.ID); accLive.Status != model.StatusCooling {
		t.Fatalf("503 阶梯封顶后应保持 cooling 而非 invalid: %s", accLive.Status)
	}
	// 成功调用后计数清零，下次从最低档重新起算
	if _, err := st.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		ResetUpstream503Streak(a)
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := MarkUpstreamUnavailable(st, model.ProviderZai, acc.ID, "上游服務不可用 HTTP 503", now); got != 30 {
		t.Fatalf("成功调用后应回到最低档 30s: %d", got)
	}
}

// 503 全链路：首档 30s 冷却（而非固定 300s）、last_error 带 body 预览、
// FailCount 与 streak 在同一次落库里完成。
func Test503CoolingUsesLadderAndLogsBodyPreview(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(http.StatusServiceUnavailable, `{"error":{"message":"upstream maintenance"}}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, _ := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("503 换号后应 503: %d", status)
	}
	if f.callCount() != 1 {
		t.Fatalf("单账号只应尝试一次: %d", f.callCount())
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusCooling || got.CoolingUntil == nil {
		t.Fatalf("503 应 cooling: %+v", got)
	}
	// 首档 30s，不是旧的固定 CoolingSeconds(300s)
	want := float64(time.Now().Add(30*time.Second).UnixNano()) / 1e9
	if diff := *got.CoolingUntil - want; diff > 5 || diff < -5 {
		t.Fatalf("首次 503 冷却应约 30s: cooling_until=%v", *got.CoolingUntil)
	}
	if got.Upstream503Streak != 1 {
		t.Fatalf("连续 503 计数应为 1: %d", got.Upstream503Streak)
	}
	if got.FailCount != 1 {
		t.Fatalf("FailCount 应 +1（折进 MarkUpstreamUnavailable 的同一次落库）: %d", got.FailCount)
	}
	if got.LastErrorKind == nil || *got.LastErrorKind != model.ErrorKindUpstreamUnavailable {
		t.Fatalf("503 应归类为 upstream_unavailable: %v", got.LastErrorKind)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "upstream maintenance") {
		t.Fatalf("last_error 应带 body 预览: %v", got.LastError)
	}
}

// no_available_account 错误体必须携带池状态分解：message 内联繁体文案 +
// error.details 结构化字段。此前静态文案把整池冷却误读成「未提供此模型/额度用完」。
func TestNoAvailableAccountCarriesPoolDetails(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(http.StatusServiceUnavailable, `{"error":{"message":"boom"}}`)
	_, _ = f.st.AddAccount(model.ProviderZai, "a", "sk-1")
	_, _ = f.st.AddAccount(model.ProviderZai, "b", "sk-2")

	status, raw := f.post(t, msgBody(), "sk-test")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("应 503: %d", status)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("错误体应为 JSON: %v", err)
	}
	errObj, _ := payload["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("错误体缺少 error 对象: %s", raw)
	}
	if errObj["type"] != "no_available_account" {
		t.Fatalf("type 不符: %v", errObj["type"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "冷卻中") || !strings.Contains(msg, "上游503") || !strings.Contains(msg, "恢復") {
		t.Fatalf("message 应内联池状态分解（冷却/成因/最早恢复）: %s", msg)
	}
	details, _ := errObj["details"].(map[string]any)
	if details == nil {
		t.Fatalf("错误体缺少 details: %s", raw)
	}
	if details["cooling"] != float64(2) || details["total_accounts"] != float64(2) {
		t.Fatalf("details 冷却计数不符: %v", details)
	}
	byKind, _ := details["cooling_by_kind"].(map[string]any)
	if byKind["upstream_unavailable"] != float64(2) {
		t.Fatalf("cooling_by_kind 不符: %v", byKind)
	}
	if _, ok := details["cooling_earliest"]; !ok {
		t.Fatalf("details 应含 cooling_earliest: %v", details)
	}
	// 文案与结构来自同一份快照：两账号冷却成因一致，不得出现「各说各话」
	if !strings.Contains(msg, "2 帳號冷卻中") {
		t.Fatalf("message 冷却账号数应与 details 一致: %s", msg)
	}
}

// 模型级隔离回归测试：单一模型额度耗尽只标记该模型，账号保持 active 且
// 其他模型选号不受影响（MarkModelExhausted / Select 的既有语义，事故排查时
// 曾被误认为「单模型耗尽会冷却整号」而提议改造——本测试钉死正确行为）。
func TestModelExhaustionDoesNotAffectOtherModels(t *testing.T) {
	f := newFixture(t)
	f.respond = jsonResp(http.StatusOK, `{"code":1005,"msg":"exceed quota limit"}`)
	acc, _ := f.st.AddAccount(model.ProviderZai, "a", "sk-1")

	status, _ := f.post(t, msgBody(), "sk-test") // model=GLM-5.3
	if status != http.StatusServiceUnavailable {
		t.Fatalf("1005 应换号并 503: %d", status)
	}
	got := f.st.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive {
		t.Fatalf("单模型耗尽时账号应保持 active: %s", got.Status)
	}
	if len(got.ExhaustedModels) != 1 || got.ExhaustedModels[0] != "glm-5.3" {
		t.Fatalf("应只标记请求模型: %v", got.ExhaustedModels)
	}
	// glm-5.3 已耗尽 → 选不出；glm-5.3-flash 未受影响 → 照常可选
	if sel := f.st.Select(model.ProviderZai, map[string]bool{}, "glm-5.3"); sel != nil {
		t.Fatalf("耗尽模型不应再被选中: %+v", sel)
	}
	if sel := f.st.Select(model.ProviderZai, map[string]bool{}, "glm-5.3-flash"); sel == nil || sel.ID != acc.ID {
		t.Fatalf("其他模型选号不应受影响: %+v", sel)
	}
}
