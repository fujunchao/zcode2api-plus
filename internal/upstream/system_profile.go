package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"zcode2api/internal/config"
)

// zcodeIdentityEndMarker 身份段的结束边界：桌面上下文段从这里开始。
// 上游的校验范围是「system 第 0 字节到身份段末尾」，所以指纹也只算到这里。
const zcodeIdentityEndMarker = "# ZCode Desktop Context"

// ZcodeSystemProfileSummary 描述「当前伪装版本实际会发出的 system」概况。
// 用于 probe 子命令与线上排障对账：只看指纹就能判断档案有没有生效。
type ZcodeSystemProfileSummary struct {
	Profile       string `json:"profile"`
	Blocks        int    `json:"blocks"`
	FirstText     string `json:"first_text"`
	FirstChars    int    `json:"first_chars"`
	FirstSHA16    string `json:"first_sha16"`
	IdentityChars int    `json:"identity_chars"`
	IdentitySHA16 string `json:"identity_sha16"`
	// DynamicChars / DynamicSHA16 第三块（3.18.0 的 Dynamic Behavior +
	// Context Management + Total Tokens）的指纹；旧档案默认只有两块时为空。
	DynamicChars int    `json:"dynamic_chars,omitempty"`
	DynamicSHA16 string `json:"dynamic_sha16,omitempty"`
}

// ZcodeSystemSummary 取当前档案的 system 指纹（含 block 0 原文，便于人工核对）。
func ZcodeSystemSummary() ZcodeSystemProfileSummary {
	return ZcodeSystemSummaryFromBlocks(ZcodeSystemBlocks(), string(config.ZcodeSystemProfileKey()))
}

// ZcodeSystemSummaryFromBlocks 按给定 blocks 计算指纹，供 probe --system-file 的自定义
// 文案复用同一套算法——人工还原时看到的指纹，必须与线上出站请求口径一致。
func ZcodeSystemSummaryFromBlocks(blocks []any, profile string) ZcodeSystemProfileSummary {
	summary := ZcodeSystemProfileSummary{Profile: profile}
	summary.Blocks = len(blocks)
	if len(blocks) == 0 {
		return summary
	}
	first, _ := blocks[0].(map[string]any)
	firstText, _ := first["text"].(string)
	summary.FirstText = firstText
	summary.FirstChars = len(firstText)
	summary.FirstSHA16 = shortSHA16(firstText)
	if len(blocks) > 1 {
		second, _ := blocks[1].(map[string]any)
		body, _ := second["text"].(string)
		identity := body
		if i := strings.Index(body, zcodeIdentityEndMarker); i >= 0 {
			identity = body[:i]
		}
		summary.IdentityChars = len(identity)
		summary.IdentitySHA16 = shortSHA16(identity)
	}
	if len(blocks) > 2 {
		third, _ := blocks[2].(map[string]any)
		dynamic, _ := third["text"].(string)
		summary.DynamicChars = len(dynamic)
		summary.DynamicSHA16 = shortSHA16(dynamic)
	}
	return summary
}

func shortSHA16(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:16]
}
