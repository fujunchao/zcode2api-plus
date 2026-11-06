// 定时领取前置预热的用例。
package adminapi

import (
	"testing"
	"time"
)

func TestClaimPreWarmMinute(t *testing.T) {
	cases := []struct {
		target string
		lead   time.Duration
		want   string
	}{
		{"23:23", 2 * time.Minute, "23:21"},
		{"12:00", 2 * time.Minute, "11:58"},
		// 跨日回绕：00:01 提前 2 分钟是前一天 23:59。
		{"00:01", 2 * time.Minute, "23:59"},
		{"00:00", 2 * time.Minute, "23:58"},
	}
	for _, tc := range cases {
		got, ok := claimPreWarmMinute(tc.target, tc.lead)
		if !ok || got != tc.want {
			t.Fatalf("claimPreWarmMinute(%q, %s) = %q, %v；期望 %q",
				tc.target, tc.lead, got, ok, tc.want)
		}
	}

	for _, bad := range []string{"", "23", "25:00", "12:60", "aa:bb", "12:00:00"} {
		if _, ok := claimPreWarmMinute(bad, time.Minute); ok {
			t.Fatalf("非法时间 %q 应返回 false", bad)
		}
	}
}

// TestMaybePreWarmOnlyAtLeadMinute 只在预热时刻那一分钟触发一次。
func TestMaybePreWarmOnlyAtLeadMinute(t *testing.T) {
	_, st, _ := setup(t)
	if err := st.SetSetting("claim_schedule_enabled", "true"); err != nil {
		t.Fatalf("写入开关失败: %v", err)
	}
	// 目标 23:23 → 预热时刻 23:21。
	if err := st.SetSetting("claim_schedule_time", "23:23"); err != nil {
		t.Fatalf("写入时间失败: %v", err)
	}
	h := &Handler{Store: st}
	s := NewClaimScheduler(h)
	base := time.Date(2026, 11, 6, 23, 21, 0, 0, time.Local)

	// 预热时刻：记录 lastWarmed。
	s.maybePreWarm(base)
	if s.lastWarmed == "" {
		t.Fatal("预热时刻应记录 lastWarmed")
	}
	stamp := s.lastWarmed

	// 同一分钟重复调用：不重复预热。
	s.maybePreWarm(base.Add(20 * time.Second))
	if s.lastWarmed != stamp {
		t.Fatalf("同一分钟不应重复预热: %q -> %q", stamp, s.lastWarmed)
	}

	// 非预热时刻：不记录。
	s2 := NewClaimScheduler(h)
	s2.maybePreWarm(base.Add(5 * time.Minute))
	if s2.lastWarmed != "" {
		t.Fatalf("非预热时刻不应触发: %q", s2.lastWarmed)
	}

	// 开关关闭：不预热。
	if err := st.SetSetting("claim_schedule_enabled", "false"); err != nil {
		t.Fatalf("写入开关失败: %v", err)
	}
	s3 := NewClaimScheduler(h)
	s3.maybePreWarm(base)
	if s3.lastWarmed != "" {
		t.Fatalf("开关关闭不应预热: %q", s3.lastWarmed)
	}
}
