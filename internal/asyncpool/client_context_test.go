package asyncpool

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
)

func TestAsyncClientContextReachesActualUpstream(t *testing.T) {
	p, st, cm, _ := newTestPool(t)
	t.Cleanup(p.Close)
	addJWTAccount(t, st, "async-client-context")
	if err := st.SetSetting("gateway_key", "sk-test"); err != nil {
		t.Fatal(err)
	}
	cm.SetConfigProvider(func(context.Context) (captcha.Config, error) {
		return captcha.Config{Enabled: true, SkipModelRequest: true}, nil
	})
	oldEnabled := config.AsyncEnabled
	config.AsyncEnabled = true
	t.Cleanup(func() { config.AsyncEnabled = oldEnabled })
	type captured struct {
		body   map[string]any
		header http.Header
	}
	calls := make(chan captured, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		select {
		case calls <- captured{body: body, header: r.Header.Clone()}:
		default:
			t.Error("同一请求发生了非预期的重复调用")
			http.Error(w, "unexpected repeated request", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_async_context\",\"usage\":{\"input_tokens\":1}}}\n\n"+
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"+
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
			"data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(up.Close)
	config.UpstreamZai = up.URL
	srv := newTestMux(t, p)
	body := map[string]any{
		"model": "GLM-5.3", "max_tokens": float64(64),
		"system": "本地客户端规则，不提供服务器工具。",
		"messages": []any{
			map[string]any{"role": "user", "content": "第一轮"},
			map[string]any{"role": "system", "content": "第二轮只读"},
			map[string]any{"role": "user", "content": "第二轮"},
		},
		"tools": []any{map[string]any{"name": "client_read", "input_schema": map[string]any{"type": "object"}}},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/async/v1/messages", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "hello") || !strings.Contains(string(raw), "message_stop") {
		t.Fatalf("异步公开接口应交付完整流: %d %s %v", resp.StatusCode, raw, err)
	}
	select {
	case actual := <-calls:
		if actual.body["system"] != body["system"] || !reflect.DeepEqual(actual.body["tools"], body["tools"]) || actual.body["max_tokens"] != float64(64) {
			t.Fatalf("后台处理不能改写本地客户端上下文: %v", actual.body)
		}
		messages := actual.body["messages"].([]any)
		if len(messages) != 3 || messages[1].(map[string]any)["content"] != "第二轮只读" {
			t.Fatalf("异步 MCS 应与同步保持相同消息形态: %v", messages)
		}
		if actual.header.Get("Anthropic-Beta") != "mid-conversation-system-2026-04-07" {
			t.Fatal("不能只在同步路径添加 MCS 能力声明")
		}
	case <-ctx.Done():
		t.Fatal("未捕获实际出站请求")
	}
}
