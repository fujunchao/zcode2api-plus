package upstream

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"sync"
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

func TestMetadataIsolationParallelCopies(t *testing.T) {
	input := map[string]any{"metadata": map[string]any{"user_id": "original", "keep": "extension"}}
	const count = 24
	results := make(chan map[string]any, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			body := maps.Clone(input)
			InjectDeviceMetadata(body, fmt.Sprintf("device-%d", i), "shared-session")
			results <- body
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	seen := map[string]bool{}
	for body := range results {
		id := metadataUserID(t, body)
		if seen[id] {
			t.Fatal("并行副本出现相同身份，可能被共享写入覆盖")
		}
		seen[id] = true
		if body["metadata"].(map[string]any)["keep"] != "extension" {
			t.Fatal("扩展字段丢失")
		}
	}
	if len(seen) != count || input["metadata"].(map[string]any)["user_id"] != "original" {
		t.Fatal("并行身份注入不得写入共享入口")
	}
}

func TestMetadataIsolationMissingOrNonObject(t *testing.T) {
	var nilMap map[string]any
	for _, value := range []any{nil, nilMap, "old-metadata", []any{}, false} {
		input := map[string]any{"metadata": value}
		body := maps.Clone(input)
		InjectDeviceMetadata(body, "device", "session")
		if metadataUserID(t, body) != deviceUserID("device", "session") || !reflect.DeepEqual(input["metadata"], value) {
			t.Fatal("缺省/非对象的兼容创建不应回写入口")
		}
	}
}
