package model

import "testing"

// TestErrorKindAllCovered 钉住枚举表的自洽性：取值唯一非空、故障表里的键都已登记。
// 新增一个 ErrorKind 却忘记加进 accountFaultKinds（或反过来拼错字）都会在这里暴露。
func TestErrorKindAllCovered(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range ErrorKindAll {
		if k == "" {
			t.Fatal("ErrorKindAll 含空取值")
		}
		if seen[k] {
			t.Fatalf("ErrorKindAll 取值重复: %s", k)
		}
		seen[k] = true
		if !ErrorKindValid(k) {
			t.Fatalf("ErrorKindValid 不认已登记的取值: %s", k)
		}
	}
	for k := range accountFaultKinds {
		if !seen[k] {
			t.Fatalf("故障表含未登记的取值: %s", k)
		}
	}
}

// TestErrorKindAccountFault 钉住归因判定：只有「换个账号可能成功」的类型才算账号故障。
// 最容易写错的是 1305 平台过载与 1302 账户限流——官方文档明确前者与单一账户无关，
// 因此它必须落在「非故障」一侧，否则前端的「账号故障」视图会被平台过载刷屏。
func TestErrorKindAccountFault(t *testing.T) {
	fault := map[string]bool{
		ErrorKindRateLimited:         true,
		ErrorKindAuthFailed:          true,
		ErrorKindConnectionFailed:    true,
		ErrorKindUpstreamUnavailable: true,
		ErrorKindInvalidResponse:     true,
		ErrorKindUpstreamError:       true,
		ErrorKindRiskControl:         true,

		ErrorKindUpstreamOverload: false,
		ErrorKindQuotaExhausted:   false,
		ErrorKindModelBusy:        false,
		ErrorKindCaptchaFailed:    false,
		ErrorKindClientCanceled:   false,
		ErrorKindQuotaQueryFailed: false,
	}
	if len(fault) != len(ErrorKindAll) {
		t.Fatalf("本用例必须覆盖全部取值: 表 %d 项 vs 枚举 %d 项", len(fault), len(ErrorKindAll))
	}
	for _, k := range ErrorKindAll {
		if got := ErrorKindAccountFault(k); got != fault[k] {
			t.Fatalf("%s 的故障归因应为 %v，实际 %v", k, fault[k], got)
		}
	}

	// 未登记取值一律不认，避免旧数据或将来新增的类型混进「账号故障」视图。
	for _, k := range []string{"", "unknown_kind", "UPSTREAM_OVERLOAD"} {
		if ErrorKindAccountFault(k) {
			t.Fatalf("未登记取值 %q 不应算作账号故障", k)
		}
		if ErrorKindValid(k) {
			t.Fatalf("ErrorKindValid 不应认未登记取值 %q", k)
		}
	}
}
