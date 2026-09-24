// body 里设备身份（metadata.user_id）的形态守卫。
//
// 这一项的失败模式与请求头同源：形态与官方不同、或与会话头分叉，都会成为上游可交叉
// 比对的差异。见 docs/analysis-client-golden-diff-20260924.md 的 golden 实测。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// metadataUserID 从 body 里取出 metadata.user_id 的**字符串**形态（官方的 wire 形态）。
func metadataUserID(t *testing.T, body map[string]any) string {
	t.Helper()
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("body 缺少 metadata 对象: %v", body)
	}
	raw, ok := metadata["user_id"].(string)
	if !ok {
		t.Fatalf("metadata.user_id 应为 JSON **字符串**（不是嵌套对象）: %#v", metadata["user_id"])
	}
	return raw
}

// TestInjectDeviceMetadataOfficialShape user_id 是 JSON 字符串，键序与取值同官方模板。
func TestInjectDeviceMetadataOfficialShape(t *testing.T) {
	body := map[string]any{}
	InjectDeviceMetadata(body, "dev-abc", "sess-xyz")

	raw := metadataUserID(t, body)
	want := `{"device_id":"dev-abc","account_uuid":"","session_id":"sess-xyz"}`
	if raw != want {
		t.Fatalf("metadata.user_id=%q，期望 %q", raw, want)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("user_id 应是可解析的 JSON: %v", err)
	}
	if len(parsed) != 3 {
		t.Fatalf("user_id 应恰好 3 个键，实际 %v", parsed)
	}
	if parsed["device_id"] != "dev-abc" || parsed["session_id"] != "sess-xyz" {
		t.Fatalf("键值不符: %v", parsed)
	}
	// account_uuid 官方恒为空串（订阅账号没有这个维度），别自作主张填值。
	if v, ok := parsed["account_uuid"].(string); !ok || v != "" {
		t.Fatalf("account_uuid 应为空串，实际 %#v", parsed["account_uuid"])
	}
}

// TestInjectDeviceMetadataPreservesOtherMetadata 只覆盖 user_id 一个键，
// metadata 里的其它键原样保留（下游可能带的自定义元数据不该被网关吃掉）。
func TestInjectDeviceMetadataPreservesOtherMetadata(t *testing.T) {
	body := map[string]any{
		"metadata": map[string]any{"user_id": `{"device_id":"old"}`, "trace_hint": "keep"},
	}
	InjectDeviceMetadata(body, "dev-new", "sess-new")

	metadata := body["metadata"].(map[string]any)
	if metadata["trace_hint"] != "keep" {
		t.Fatalf("其它 metadata 键应保留: %v", metadata)
	}
	if got := metadataUserID(t, body); !strings.Contains(got, `"dev-new"`) {
		t.Fatalf("user_id 应被覆盖为本账号指纹，实际 %q", got)
	}
}

// TestInjectDeviceMetadataEscapes 指纹/会话若含引号或反斜杠，必须转义成合法 JSON
// （否则整份请求体在 JSON 层就坏了，且是我们自己拼字符串拼出来的 bug）。
func TestInjectDeviceMetadataEscapes(t *testing.T) {
	body := map[string]any{}
	InjectDeviceMetadata(body, `dev"quote\`, "sess\nnew")

	raw := metadataUserID(t, body)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("转义后应是合法 JSON: %v（原文 %q）", err, raw)
	}
	if parsed["device_id"] != `dev"quote\` || parsed["session_id"] != "sess\nnew" {
		t.Fatalf("转义后取值应还原: %v", parsed)
	}
}

// TestMetadataSessionID 兜底来源的取值规则：只有「JSON 字符串里 session_id 是非空
// 字符串」才算数，其余一律返回空串（宁可合成新会话，也不把畸形值带上游）。
func TestMetadataSessionID(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"正常", map[string]any{"metadata": map[string]any{
			"user_id": `{"device_id":"d","account_uuid":"","session_id":"s-1"}`}}, "s-1"},
		{"空白被裁掉", map[string]any{"metadata": map[string]any{
			"user_id": `{"session_id":"  s-2  "}`}}, "s-2"},
		{"没有 metadata", map[string]any{}, ""},
		{"metadata 不是对象", map[string]any{"metadata": "x"}, ""},
		{"user_id 是对象而非字符串", map[string]any{"metadata": map[string]any{
			"user_id": map[string]any{"session_id": "s-3"}}}, ""},
		{"user_id 不是 JSON", map[string]any{"metadata": map[string]any{
			"user_id": "not-json"}}, ""},
		{"user_id 是 JSON 但不是对象", map[string]any{"metadata": map[string]any{
			"user_id": `"s-4"`}}, ""},
		{"session_id 不是字符串", map[string]any{"metadata": map[string]any{
			"user_id": `{"session_id":123}`}}, ""},
		{"session_id 缺失", map[string]any{"metadata": map[string]any{
			"user_id": `{"device_id":"d"}`}}, ""},
		{"session_id 空串", map[string]any{"metadata": map[string]any{
			"user_id": `{"session_id":""}`}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MetadataSessionID(tc.body); got != tc.want {
				t.Fatalf("MetadataSessionID=%q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestAttributionSessionSharedBetweenHeaderAndBody 归因标识是头与 body 的**唯一**
// 会话来源：X-Session-Id 头与 metadata.user_id.session_id 必须同值。
//
// 官方这两个值恒等（golden 实测），分叉即成差异；旧实现两处各算一次、下游没送会话
// 时还会各自新生成 UUID —— 本用例钉住的正是这条。
func TestAttributionSessionSharedBetweenHeaderAndBody(t *testing.T) {
	attr := NewAttribution(map[string]string{"x-session-id": "sess_shared-1"}, "")
	if attr.SessionID != "shared-1" {
		t.Fatalf("sess_ 前缀应被剥离，实际 %q", attr.SessionID)
	}
	if got := attr.Headers()["X-Session-Id"]; got != attr.SessionID {
		t.Fatalf("头里的会话 id 与归因标识不一致: %q vs %q", got, attr.SessionID)
	}

	body := map[string]any{}
	InjectDeviceMetadata(body, "dev-1", attr.SessionID)
	parsed := map[string]any{}
	if err := json.Unmarshal([]byte(metadataUserID(t, body)), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["session_id"] != attr.Headers()["X-Session-Id"] {
		t.Fatalf("body 的 metadata 会话 %q 与头的会话 %q 不一致",
			parsed["session_id"], attr.Headers()["X-Session-Id"])
	}
}

// TestAttributionFallsBackToBodySession 下游只把会话写在 body 的 metadata 里
// （没有 x-session-id 头）时，头也必须用同一个会话，而不是另造一个 UUID。
func TestAttributionFallsBackToBodySession(t *testing.T) {
	body := map[string]any{"metadata": map[string]any{
		"user_id": `{"device_id":"d","account_uuid":"","session_id":"from-body"}`}}

	attr := NewAttribution(nil, MetadataSessionID(body))
	if attr.SessionID != "from-body" {
		t.Fatalf("应沿用 body 声明的会话，实际 %q", attr.SessionID)
	}
	if got := attr.Headers()["X-Session-Id"]; got != "from-body" {
		t.Fatalf("X-Session-Id 应与 body 声明同值，实际 %q", got)
	}
}

// TestAttributionHeaderWinsOverBody 头与 body 都给了会话时以**头**为准：
// 头是显式声明的传输层事实，body 里的 metadata 可能来自更早的一轮。
func TestAttributionHeaderWinsOverBody(t *testing.T) {
	attr := NewAttribution(map[string]string{"X-Session-Id": "from-header"}, "from-body")
	if attr.SessionID != "from-header" {
		t.Fatalf("头里的会话应优先，实际 %q", attr.SessionID)
	}
}
