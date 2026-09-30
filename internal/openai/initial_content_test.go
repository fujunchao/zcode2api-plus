package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatSSEPreservesInitialContent(t *testing.T) {
	for _, kind := range []string{"text", "thinking"} {
		for _, delta := range []string{"", "后续"} {
			t.Run(kind+"/"+delta, func(t *testing.T) {
				events := []map[string]any{
					{"type": "message_start", "message": map[string]any{"id": "test", "model": "GLM-5.3"}},
					{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": kind, kind: "初始"}},
				}
				if delta != "" {
					events = append(events, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": kind + "_delta", kind: delta}})
				}
				events = append(events, map[string]any{"type": "message_stop"})
				var input, output strings.Builder
				for _, e := range events {
					b, _ := json.Marshal(e)
					input.WriteString("data: " + string(b) + "\n\n")
				}
				if err := reencodeSSE(strings.NewReader(input.String()), false, func(s string) error { output.WriteString(s); return nil }); err != nil {
					t.Fatal(err)
				}
				key := "content"
				if kind == "thinking" {
					key = "reasoning_content"
				}
				text := ""
				for _, line := range strings.Split(output.String(), "\n") {
					if !strings.HasPrefix(line, "data: {") {
						continue
					}
					var event map[string]any
					_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
					choices, _ := event["choices"].([]any)
					if len(choices) > 0 {
						d := choices[0].(map[string]any)["delta"].(map[string]any)
						text += stringOf(d[key])
					}
				}
				if text != "初始"+delta {
					t.Fatalf("正文丢失：%q", text)
				}
			})
		}
	}
}
