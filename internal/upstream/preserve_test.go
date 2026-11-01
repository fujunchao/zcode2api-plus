package upstream

import "testing"

// 保留模式的判据必须是「首段 + 身份段」都对：3.16.x 与 3.17.1 的块 0 只差几个字，
// 只比对前缀会把身份段不匹配的请求原样送出，被上游判 3012
// （2026-11-01 生产事故：切默认版本后 preserve 误判导致全量拦截）。
func TestShouldPreserveCallerSystemRequiresIdentityMatch(t *testing.T) {
	standard := ZcodeSystemBlocks()
	prefix := standard[0].(map[string]any)["text"].(string)
	identity := standard[1].(map[string]any)["text"].(string)

	canonical := []any{
		map[string]any{"type": "text", "text": prefix},
		map[string]any{"type": "text", "text": identity},
	}
	mismatched := []any{
		map[string]any{"type": "text", "text": prefix},
		map[string]any{"type": "text", "text": "You are an agent working with the user toward their goals. (另一个客户端的身份段)"},
	}
	withExtra := []any{
		map[string]any{"type": "text", "text": prefix},
		map[string]any{"type": "text", "text": identity},
		map[string]any{"type": "text", "text": "# Environment\n调用者追加的环境段"},
	}

	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"官方两块原样", map[string]any{"system": canonical}, true},
		{"官方两块 + 调用者追加块", map[string]any{"system": withExtra}, true},
		{"前缀对但身份段不同", map[string]any{"system": mismatched}, false},
		{"首段是第三方文本", map[string]any{"system": []any{map[string]any{"type": "text", "text": "You are Claude Code"}}}, false},
		{"字符串形态仍按前缀", map[string]any{"system": prefix + " 调用者补充"}, true},
		{"空 system 且无消息内 system", map[string]any{"system": ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldPreserveCallerSystem(tc.body); got != tc.want {
				t.Fatalf("ShouldPreserveCallerSystem = %t, 期望 %t", got, tc.want)
			}
		})
	}
}
