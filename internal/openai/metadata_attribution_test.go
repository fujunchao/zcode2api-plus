package openai

import (
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"zcode2api/internal/upstream"
)

func TestCompatibleMetadataCopiesOnlyValidatedSession(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			body := compatRequest(api)
			body["user"] = "not-a-session-or-caller-scope"
			body["metadata"] = map[string]any{
				"user_id":    `{"session_id":"sess_logical","device_id":"caller-device","account_uuid":"caller-account","secret":"private-extra"}`,
				"session_id": "not-an-approved-source", "trace_id": "not-a-header", "private": "not-forwarded",
			}
			before, _ := json.Marshal(body)
			convert := ConvertRequest
			if api == "responses" {
				convert = ConvertResponsesRequest
			}
			first, err := convert(body)
			if err != nil {
				t.Fatal(err)
			}
			second, err := convert(body)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"user_id": `{"session_id":"sess_logical"}`}
			if !reflect.DeepEqual(first["metadata"], want) || !reflect.DeepEqual(second["metadata"], want) {
				t.Fatalf("协议转换只能保存明确的 session 来源：%v", first["metadata"])
			}
			first["metadata"].(map[string]any)["user_id"] = "changed"
			if !reflect.DeepEqual(second["metadata"], want) {
				t.Fatal("两个转换结果不能共享可写 metadata 容器")
			}
			after, _ := json.Marshal(body)
			if string(before) != string(after) {
				t.Fatal("转换不得改写输入 metadata")
			}
		})
	}
}

func TestCompatibleMetadataInvalidAndConcurrentInputs(t *testing.T) {
	for _, metadata := range []any{
		nil, "invalid", map[string]any{"session_id": "wrong-source"},
		map[string]any{"user_id": map[string]any{"session_id": "wrong-shape"}},
		map[string]any{"user_id": `{"session_id":"unsafe\nheader"}`},
	} {
		for _, convert := range []func(map[string]any) (map[string]any, error){ConvertRequest, ConvertResponsesRequest} {
			body := map[string]any{"model": "GLM-5.3", "input": "input", "messages": []any{}, "metadata": metadata}
			got, err := convert(body)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := got["metadata"]; exists {
				t.Fatal("非法或未声明的 metadata 来源不能伪装为 session")
			}
		}
	}
	input := map[string]any{
		"model": "GLM-5.3", "input": "input", "messages": []any{},
		"metadata": map[string]any{"user_id": `{"session_id":"shared-logical-session"}`},
	}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			convert := ConvertRequest
			if i%2 == 0 {
				convert = ConvertResponsesRequest
			}
			out, err := convert(input)
			if err != nil {
				t.Error(err)
				return
			}
			upstream.InjectDeviceMetadata(out, "local-device", "shared-logical-session")
			if upstream.MetadataSessionID(out) != "shared-logical-session" {
				t.Error("并发转换后归因不一致")
			}
		}()
	}
	wg.Wait()
	if input["metadata"].(map[string]any)["user_id"] != `{"session_id":"shared-logical-session"}` {
		t.Fatal("并发身份注入污染了协议输入")
	}
}
