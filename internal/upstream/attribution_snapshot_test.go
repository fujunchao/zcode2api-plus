package upstream

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidatedAttributionSnapshotPreservesOnlyLogicalHeaders(t *testing.T) {
	incoming := map[string]string{
		"x-session-id": "sess_conversation", "X-ZCODE-TRACE-ID": "trace.demo",
		"x-query-id": "query_turn", "X-Zcode-Session-Type": "subagent",
		"x-zcode-query-source": "compact", "Authorization": "Bearer synthetic-secret",
		"Cookie": "session=synthetic-secret", "X-Api-Key": "synthetic-key",
		"X-Request-Id": "old-attempt", "X-Zcode-Unknown": "do-not-save",
	}
	want := map[string]string{
		"X-Session-Id": "sess_conversation", "X-Zcode-Trace-Id": "trace.demo",
		"X-Query-Id": "query_turn", "X-Zcode-Session-Type": "subagent",
		"X-Zcode-Query-Source": "compact",
	}
	got := ValidatedAttributionHeaders(incoming)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("异步快照必须只保留五个逻辑归因字段：got=%v", got)
	}
	got["X-Session-Id"] = "changed"
	if incoming["x-session-id"] != "sess_conversation" {
		t.Fatal("快照不得反向修改入站头")
	}
}

func TestAttributionSnapshotRoundTripKeepsNormalizedLogicalIDs(t *testing.T) {
	snapshot := NewAttributionSnapshot(map[string]string{
		"X-Session-Id": "sess_sess_nested", "X-Query-Id": "query_query_nested",
		"X-Zcode-Trace-Id": "trace.fixed", "X-Zcode-Session-Type": "side_chat",
		"X-Zcode-Query-Source": "compact", "Authorization": "Bearer synthetic-secret",
		"Cookie": "synthetic-cookie", "X-Request-Id": "previous-attempt",
	}, "body-conflict")
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-secret", "synthetic-cookie", "previous-attempt", "request_id", "Authorization"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("快照不应保存 %s", forbidden)
		}
	}
	var restored AttributionSnapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	first, err := restored.Restore()
	if err != nil {
		t.Fatal(err)
	}
	second, err := restored.Restore()
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID != "sess_nested" || first.QueryID != "query_nested" || first.TraceID != "trace.fixed" || first.QuerySource != "compact" || first.SessionType != "side_chat" {
		t.Fatalf("恢复不能再次剥离前缀或改变逻辑身份：%+v", first)
	}
	if first.RequestID == second.RequestID || first.WithFreshRequestID().RequestID == first.RequestID {
		t.Fatal("快照不应复用已结束尝试的 request ID")
	}
	first.RequestID, second.RequestID = "", ""
	if first != second || restored != snapshot {
		t.Fatal("恢复应返回独立值，不改写源快照")
	}
}

func TestAttributionSnapshotUnknownVersionAndInvalidFieldsFailClosed(t *testing.T) {
	base := NewAttributionSnapshot(nil, "body-session")
	for _, mutate := range []func(*AttributionSnapshot){
		func(s *AttributionSnapshot) { s.Version = 0 },
		func(s *AttributionSnapshot) { s.Version++ },
		func(s *AttributionSnapshot) { s.SessionID = "" },
		func(s *AttributionSnapshot) { s.TraceID = "newline\nvalue" },
		func(s *AttributionSnapshot) { s.QueryID = strings.Repeat("q", 1025) },
		func(s *AttributionSnapshot) { s.SessionType = "invented" },
		func(s *AttributionSnapshot) { s.QuerySource = "invalid value" },
	} {
		changed := base
		mutate(&changed)
		if _, err := changed.Restore(); !errors.Is(err, ErrInvalidAttributionSnapshot) {
			t.Fatal("未知版本/损坏快照不得生成新会话继续执行")
		}
	}
}

func TestTransparentAttributionMissingAndBodySourceContract(t *testing.T) {
	first, second := NewAttribution(nil, ""), NewAttribution(nil, "")
	if first.SessionID == second.SessionID || first.TraceID == second.TraceID || first.QueryID == second.QueryID {
		t.Fatal("缺省字段按入口创建，不以共享密钥、正文或连接推断会话")
	}
	header := map[string]string{"X-Session-Id": "sess_subagent_agent_conversation", "X-Query-Id": "query_fixed"}
	first, second = NewAttribution(header, "body-ignored"), NewAttribution(header, "body-ignored")
	if first.SessionID != "conversation" || first.SessionID != second.SessionID || first.QueryID != "fixed" || second.QueryID != "fixed" || first.TraceID == second.TraceID {
		t.Fatal("只给 session 不应推断 trace；调用者固定 query 保持透明")
	}
	for _, value := range []string{"sess_body", "subagent_agent_body", "sess_subagent_agent_body"} {
		if got := NewAttribution(nil, value).SessionID; got != "body" {
			t.Fatalf("正文来源与头应使用相同前缀规则：%q => %q", value, got)
		}
	}
	for _, value := range []string{"bad\nvalue", "a,b", strings.Repeat("s", 1025)} {
		if got := NewAttribution(nil, value).SessionID; got == value || got == "" {
			t.Fatal("非法正文 session 应退回本入口生成值")
		}
	}
	for _, value := range []string{"sess_", "subagent_agent_"} {
		if got := NewAttribution(nil, value).SessionID; got != value {
			t.Fatal("只有前缀而无后缀时沿用官方非空回退语义")
		}
	}
}
