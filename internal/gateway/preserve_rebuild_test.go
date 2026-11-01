package gateway

import (
	"strings"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/upstream"
)

// 调用者的 system 首段撞上官方前缀、但身份段不是本版本档案时，必须重建 canonical，
// 不能原样转发——否则上游 3012（2026-11-01 生产事故复现路径）。
func TestNormalizeBodyRebuildsCallerSystemWithMismatchedIdentity(t *testing.T) {
	oldPreserve := config.PreserveClientContext
	config.PreserveClientContext = true
	t.Cleanup(func() { config.PreserveClientContext = oldPreserve })

	standard := upstream.ZcodeSystemBlocks()
	prefix := standard[0].(map[string]any)["text"].(string)
	identity := standard[1].(map[string]any)["text"].(string)
	const foreignIdentity = "身份段来自旧版客户端，且与本版本档案不一致"

	build := func(system []any) map[string]any {
		return map[string]any{
			"model":    "GLM-5.3",
			"system":   system,
			"messages": []any{map[string]any{"role": "user", "content": "你好"}},
		}
	}

	// 1) 身份段不匹配 → 重建为当前档案。
	body := build([]any{
		map[string]any{"type": "text", "text": prefix},
		map[string]any{"type": "text", "text": foreignIdentity},
	})
	NormalizeBody(body, true)
	blocks, _ := body["system"].([]any)
	if len(blocks) < 2 {
		t.Fatalf("重建后 system 块数 = %d, 期望 ≥2", len(blocks))
	}
	if got := blocks[1].(map[string]any)["text"].(string); got != identity {
		t.Fatalf("身份段未重建为本版本档案: %q", got[:min(60, len(got))])
	}
	dynamic := blocks[len(blocks)-1].(map[string]any)["text"].(string)
	if !strings.Contains(dynamic, foreignIdentity) {
		t.Fatal("调用者原文必须保留在 dynamic 块")
	}

	// 2) 身份段完全一致 → 原样保留（不重建、不改写）。
	body = build([]any{
		map[string]any{"type": "text", "text": prefix},
		map[string]any{"type": "text", "text": identity},
	})
	NormalizeBody(body, true)
	kept := body["system"].([]any)
	if len(kept) != 2 {
		t.Fatalf("身份段一致时不应重建，块数 = %d", len(kept))
	}
	if got := kept[1].(map[string]any)["text"].(string); got != identity {
		t.Fatal("身份段一致时应原样保留")
	}
}
