package upstream

import (
	"strings"
	"testing"
)

// 通过实际构造入口锁定官方可选归因字段，不能放开整组未知 x-zcode 头。
func TestQuerySourcePreservesValidatedPurpose(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "", "", map[string]string{
		"x-zcode-query-source": "compact",
		"x-zcode-unknown":      "must-not-leak",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Headers["X-Zcode-Query-Source"]; got != "compact" {
		t.Fatalf("合法用途应保留，got=%q", got)
	}
	if _, ok := req.Headers["X-Zcode-Unknown"]; ok {
		t.Fatal("未知上下文不能因为保留用途而透传")
	}
}

func TestQuerySourceValidationAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
	}{
		{"missing", "", ""}, {"main", "main_turn", "main_turn"},
		{"non_main", "subagent:review.v1-2", "subagent:review.v1-2"},
		{"trim", " compact ", "compact"},
		{"limit", strings.Repeat("a", 128), strings.Repeat("a", 128)},
		{"too_long", strings.Repeat("a", 129), ""},
		{"space", "main turn", ""}, {"slash", "main/turn", ""},
		{"multivalue", "compact, main_turn", ""}, {"unicode", "主任务", ""},
		{"control", "compact\r\nsecret", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			incoming := map[string]string{"x-ZCODE-QUERY-source": tc.value}
			a := NewAttribution(incoming, "")
			retry := a.WithFreshRequestID()
			for _, attr := range []Attribution{a, retry} {
				req, err := BuildRequestWithAttribution(jwtAccount(), "", "", incoming, attr)
				if err != nil || req.Headers["X-Zcode-Query-Source"] != tc.want {
					t.Fatalf("用途验证不符：value=%q got=%q err=%v", tc.value, req.Headers["X-Zcode-Query-Source"], err)
				}
				if tc.want == "" {
					if _, present := req.Headers["X-Zcode-Query-Source"]; present {
						t.Fatal("缺失/非法用途必须省略头，不能发送空值或伪造 main_turn")
					}
				}
			}
			if a.RequestID == retry.RequestID || a.SessionID != retry.SessionID || a.TraceID != retry.TraceID || a.QueryID != retry.QueryID {
				t.Fatal("同轮重试只更新 request ID")
			}
		})
	}
}

func TestAttributionRejectsCaseAliasAmbiguity(t *testing.T) {
	for _, duplicate := range []string{"compact", "main_turn"} {
		incoming := map[string]string{"X-Zcode-Query-Source": "compact", "x-zcode-query-source": duplicate}
		for range 32 {
			if got := NewAttribution(incoming, "").QuerySource; got != "" {
				t.Fatalf("大小写别名多值不能依赖 map 迭代顺序取值：%q", got)
			}
		}
	}
}
