package upstream

import (
	"reflect"
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
