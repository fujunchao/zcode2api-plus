// 请求体里的设备身份（官方形态）。
//
// 官方客户端在**模型请求头里不带 X-Device-Mid**（只有额度 / 领取 / 遥测等非模型
// 接口带），设备指纹放在 body 里：
//
//	metadata.user_id = "{\"device_id\":\"<设备指纹>\",\"account_uuid\":\"\",\"session_id\":\"<会话 id>\"}"
//
// golden 抓包实测（docs/analysis-client-golden-diff-20260924.md）。注意两点形态细节：
//   - user_id 是一个 **JSON 字符串**，不是嵌套对象；
//   - session_id 与 X-Session-Id 头**恒等**（不是两个独立的 id）。
//
// 本网关的账号隔离依赖「每账号一份设备指纹」，所以对齐的方向不是删掉指纹，而是把它
// 从头挪到 body —— 两头都去掉等于设备身份彻底消失，反而更像伪造。头那侧的移除见
// config.UpstreamSendDeviceMid 与 request.go 的 dropHeaders。
package upstream

import (
	"encoding/json"
	"strings"
)

// metadata.user_id 里的键名（形态由官方固定，别改名）。
const (
	metadataKeyDeviceID    = "device_id"
	metadataKeyAccountUUID = "account_uuid"
	metadataKeySessionID   = "session_id"
)

// MetadataSessionID 取下游请求体 metadata.user_id 里声明的 session_id；取不到返回空串。
//
// 它只作 X-Session-Id 头缺失时的兜底来源：官方这两个值恒等，头里没有而 body 里有
// 时应当沿用 body 的值，否则会造出「头里一个会话、body 里另一个会话」的畸形请求。
//
// 任何形态异常（metadata 不是对象、user_id 不是 JSON 字符串、session_id 不是字符串）
// 都返回空串——宁可合成一个新会话 id，也不把下游的畸形值带到上游。
func MetadataSessionID(body map[string]any) string {
	raw, ok := userIDString(body)
	if !ok {
		return ""
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return ""
	}
	session, _ := parsed[metadataKeySessionID].(string)
	return strings.TrimSpace(session)
}

// InjectDeviceMetadata 写入官方形态的 metadata.user_id。
//
// deviceID 必须是**本账号**的设备指纹（不是全局值）：同机多账号共用一份会被上游
// 按设备关联，与账号隔离的目的相反。sessionID 应取自同一次请求的 Attribution.SessionID
// ——官方这两个值恒等，分叉即差异。
//
// 只覆盖 user_id 这一个键：metadata 里的其它键（下游可能带的）原样保留，官方形态
// 也只有一个 user_id，不做删改。
func InjectDeviceMetadata(body map[string]any, deviceID, sessionID string) {
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata["user_id"] = deviceUserID(deviceID, sessionID)
	body["metadata"] = metadata
}

// userIDString 取 body 里 metadata.user_id 的字符串形态。
func userIDString(body map[string]any) (string, bool) {
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		return "", false
	}
	raw, ok := metadata["user_id"].(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

// deviceUserID 按官方模板拼 user_id。键序固定为 device_id / account_uuid / session_id，
// 与抓包逐字节一致——业务上服务端解析它，但保持同一形态可省去「顺序也算差异」的疑虑。
func deviceUserID(deviceID, sessionID string) string {
	return "{" + quoteJSON(metadataKeyDeviceID) + ":" + quoteJSON(deviceID) +
		"," + quoteJSON(metadataKeyAccountUUID) + ":" + quoteJSON("") +
		"," + quoteJSON(metadataKeySessionID) + ":" + quoteJSON(sessionID) + "}"
}

// quoteJSON 用 JSON 转义规则（含引号）包一个字符串。
// 刻意不用 strconv.Quote：两者对控制字符的转义形态不同（\x00 vs \u0000）。
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
