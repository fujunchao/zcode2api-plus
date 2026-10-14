package upstream

import (
	"encoding/json"
	"regexp"
	"testing"
)

func TestSemanticContentKeyIgnoresAttributionWithoutMutatingBody(t *testing.T) {
	body := map[string]any{
		"model": "GLM-5.3", "max_tokens": 8192,
		"system":   "保留调用者指令",
		"messages": []any{map[string]any{"role": "user", "content": "比较线路"}},
		"metadata": map[string]any{"user_id": `{"device_id":"device-a","account_uuid":"account-a","session_id":"session-a"}`},
	}
	before, _ := json.Marshal(body)
	first := SemanticContentKey(body)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Fatalf("必须使用完整 SHA-256，got=%q", first)
	}
	other := map[string]any{}
	if err := json.Unmarshal(before, &other); err != nil {
		t.Fatal(err)
	}
	other["metadata"].(map[string]any)["user_id"] = `{"session_id":"session-b","account_uuid":"account-b","device_id":"device-b"}`
	if got := SemanticContentKey(other); got != first {
		t.Fatal("只改变已知设备/会话归因不得增加内容多样性")
	}
	other["max_tokens"] = 128000
	if SemanticContentKey(other) == first {
		t.Fatal("实际生成参数必须参与内容摘要")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("摘要计算不得修改原始正文")
	}
}
