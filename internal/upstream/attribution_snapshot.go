package upstream

import (
	"errors"
	"strings"
)

// AttributionSnapshotVersion 只描述透明归因的生命周期，不代表托管身份或调用者范围。
const AttributionSnapshotVersion = 1

// AttributionSnapshot 保存一轮已解析的逻辑身份；从不保存鉴权、整组请求头或尝试 ID。
// 这里只提供可编码的值类型。当前异步队列仍为进程内队列，不承诺跨重启恢复票据。
type AttributionSnapshot struct {
	Version     int    `json:"version"`
	SessionID   string `json:"session_id"`
	TraceID     string `json:"trace_id"`
	SessionType string `json:"session_type"`
	QueryID     string `json:"query_id"`
	QuerySource string `json:"query_source,omitempty"`
}

var ErrInvalidAttributionSnapshot = errors.New("异步归因快照版本或字段无效")

// NewAttributionSnapshot 入队前冻结逻辑身份，缺值也只在此生成一次。
func NewAttributionSnapshot(incoming map[string]string, bodySessionID string) AttributionSnapshot {
	a := NewAttribution(incoming, bodySessionID)
	return AttributionSnapshot{
		Version: AttributionSnapshotVersion, SessionID: a.SessionID, TraceID: a.TraceID,
		SessionType: a.SessionType, QueryID: a.QueryID, QuerySource: a.QuerySource,
	}
}

// Restore 恢复同一轮归因，尝试 ID 每次生成；已经规范化的前缀不能被再次剥离。
// 未知版本/损坏快照必须明确失败，不能静默生成另一份会话后继续派发。
func (s AttributionSnapshot) Restore() (Attribution, error) {
	if s.Version != AttributionSnapshotVersion || !validSessionType(s.SessionType) {
		return Attribution{}, ErrInvalidAttributionSnapshot
	}
	for _, value := range []string{s.SessionID, s.TraceID, s.QueryID} {
		if !validAttributionValue(value) || value != strings.TrimSpace(value) {
			return Attribution{}, ErrInvalidAttributionSnapshot
		}
	}
	if s.QuerySource != "" && !querySourcePattern.MatchString(s.QuerySource) {
		return Attribution{}, ErrInvalidAttributionSnapshot
	}
	return (Attribution{
		SessionID: s.SessionID, TraceID: s.TraceID, SessionType: s.SessionType,
		QueryID: s.QueryID, QuerySource: s.QuerySource,
	}).WithFreshRequestID(), nil
}

// ValidatedAttributionHeaders 返回可持久化的逻辑归因白名单，不包含凭据或请求 ID。
// 保留合法来值，不在入队时生成 ID；前缀处理和缺值生成由 NewAttribution 统一完成。
// ID 的 1024 字节上限是本地快照预算，不声称是官方 UUID 格式要求。
func ValidatedAttributionHeaders(incoming map[string]string) map[string]string {
	out := make(map[string]string, 5)
	for _, name := range []string{
		"X-Session-Id", "X-Zcode-Trace-Id", "X-Query-Id",
		"X-Zcode-Session-Type", "X-Zcode-Query-Source",
	} {
		var raw string
		count := 0
		for key, value := range incoming {
			if strings.EqualFold(key, name) {
				raw = value
				count++
			}
		}
		// 大小写别名冲突和逗号合并的多值都不任意挑一个，避免 map 迭代决定身份。
		if count != 1 || !validAttributionValue(raw) {
			continue
		}
		value := strings.TrimSpace(raw)
		switch name {
		case "X-Zcode-Query-Source":
			if !querySourcePattern.MatchString(value) {
				continue
			}
		case "X-Zcode-Session-Type":
			if !validSessionType(value) {
				continue
			}
		}
		out[name] = value
	}
	return out
}

func validSessionType(value string) bool {
	switch value {
	case "main", "subagent", "side_chat", "other":
		return true
	default:
		return false
	}
}

func validAttributionValue(value string) bool {
	if len(value) > 1024 || strings.TrimSpace(value) == "" {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e || value[i] == ',' {
			return false
		}
	}
	return true
}
