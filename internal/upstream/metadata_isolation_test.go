package upstream

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"
)

// 对应真实重试：顶层副本共享已有 metadata，注入不得反向改写入口或先前副本。
func TestMetadataIsolationAcrossAttempts(t *testing.T) {
	meta := map[string]any{
		"user_id":    `{"device_id":"client-device","account_uuid":"client-account","session_id":"client-session"}`,
		"trace_hint": "keep",
		"extension":  map[string]any{"flag": true},
	}
	input := map[string]any{"metadata": meta, "messages": []any{"synthetic-content"}}
	before, _ := json.Marshal(input)
	first, second := maps.Clone(input), maps.Clone(input)
	InjectDeviceMetadata(first, "device-a", "round-session")
	firstValue := metadataUserID(t, first)
	InjectDeviceMetadata(second, "device-b", "round-session")

	after, _ := json.Marshal(input)
	if string(after) != string(before) {
		t.Errorf("身份注入改写了入口 metadata：before=%s after=%s", before, after)
	}
	if metadataUserID(t, first) != firstValue || firstValue != deviceUserID("device-a", "round-session") {
		t.Error("后续账号的身份覆盖了先前副本")
	}
	if metadataUserID(t, second) != deviceUserID("device-b", "round-session") {
		t.Error("本次副本仍须写入当次身份")
	}
	for _, body := range []map[string]any{first, second} {
		got := body["metadata"].(map[string]any)
		if got["trace_hint"] != "keep" || !reflect.DeepEqual(got["extension"], meta["extension"]) {
			t.Error("非身份扩展字段必须保留")
		}
	}
	first["metadata"].(map[string]any)["trace_hint"] = "first-only"
	if meta["trace_hint"] != "keep" || second["metadata"].(map[string]any)["trace_hint"] != "keep" {
		t.Error("每个 metadata 容器必须独立")
	}
}

func TestMetadataIsolationRepeatedInjectionKeepsPriorSnapshot(t *testing.T) {
	body := map[string]any{"metadata": map[string]any{"user_id": "client", "keep": 7}}
	InjectDeviceMetadata(body, "first", "session")
	previous := body["metadata"].(map[string]any)
	InjectDeviceMetadata(body, "second", "session")
	if previous["user_id"] != deviceUserID("first", "session") {
		t.Fatal("再次注入不得修改调用者已保存的 metadata 快照")
	}
	if metadataUserID(t, body) != deviceUserID("second", "session") {
		t.Fatal("当前正文必须具有最新身份")
	}
}
