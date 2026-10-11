package upstream

import (
	"encoding/json"
	"testing"

	"zcode2api/internal/config"
)

func TestMessageCapabilitiesFollowActualBody(t *testing.T) {
	for _, tc := range []struct {
		name     string
		preserve bool
		body     map[string]any
		incoming string
		want     string
	}{
		{"top_level_only", true, map[string]any{"system": "规则", "messages": []any{map[string]any{"role": "user", "content": "问题"}}}, "caller-feature", "caller-feature"},
		{"inline_system", true, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "后续规则"}}}, "", "mid-conversation-system-2026-04-07"},
		{"merge_features", true, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "规则"}}}, " caller-feature,caller-feature ", "caller-feature, mid-conversation-system-2026-04-07"},
		{"legacy_keeps_explicit_header", false, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "规则"}}}, "caller-feature", "caller-feature"},
		{"legacy_does_not_add_header", false, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "规则"}}}, "", ""},
		{"missing_body", true, nil, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := config.PreserveClientContext
			config.PreserveClientContext = tc.preserve
			t.Cleanup(func() { config.PreserveClientContext = old })
			req := Request{Headers: map[string]string{"Anthropic-Beta": tc.incoming}}
			before, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			ApplyMessageCapabilities(&req, tc.body)
			ApplyMessageCapabilities(&req, tc.body)
			if req.Headers["Anthropic-Beta"] != tc.want {
				t.Fatalf("能力声明或重复应用不符: got=%q want=%q", req.Headers["Anthropic-Beta"], tc.want)
			}
			after, err := json.Marshal(tc.body)
			if err != nil || string(before) != string(after) {
				t.Fatal("声明能力不能改写正文或补造系统消息")
			}
		})
	}
}
