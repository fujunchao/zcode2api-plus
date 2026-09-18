// 流式重编码：上游 Anthropic SSE 事件流 → OpenAI chat.completion.chunk 流。
// 不是透传：逐事件解析后按 §5.7 重新编码（ping 丢弃、[DONE] 收尾）。
package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// reencodeSSE 读取上游 SSE 流并写出 OpenAI chunk 流；includeUsage 为 true 时
// 终止前附 usage chunk。write 只接收 `data: ...\n\n` 形态的完整事件。
// 返回 write 或读取的错误（客户端中断由调用方经 write 错误感知）。
func reencodeSSE(body io.Reader, includeUsage bool, write func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	enc := &sseEncoder{write: write, includeUsage: includeUsage}
	event := ""
	var data strings.Builder

	flush := func() error { return enc.dispatch(event, data.String()) }
	reset := func() { event, data = "", strings.Builder{} }

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case line == "":
			if err := flush(); err != nil {
				return enc.fail(err)
			}
			reset()
		}
		// 注释行（: keepalive）与其他行忽略
	}
	if err := scanner.Err(); err != nil {
		return enc.fail(err)
	}
	// 流以事件收尾而非空行时同样分发
	if event != "" || data.Len() > 0 {
		if err := flush(); err != nil {
			return enc.fail(err)
		}
	}
	if !enc.finished {
		return enc.fail(fmt.Errorf("上游串流在 message_stop 之前结束: %w", io.ErrUnexpectedEOF))
	}
	return nil
}

// sseEncoder 持有跨事件的流状态（id/model/tool 序号/usage）。
type sseEncoder struct {
	write func(string) error

	id           string
	model        string
	tools        map[int]*chatStreamTool // Anthropic block index → 独立工具状态
	toolOrder    []*chatStreamTool
	finished     bool
	inputUsage   map[string]any // message_start 的 usage（input 系）
	outputUsage  map[string]any // message_delta 的 usage（output）
	includeUsage bool
}

type chatStreamTool struct {
	index   float64
	initial string
	hasArgs bool
}

// dispatch 分发一个已解析的上游事件。
func (e *sseEncoder) dispatch(event, data string) error {
	if data == "" || data == "[DONE]" || e.finished {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return fmt.Errorf("上游串流事件不是合法 JSON: %w", err)
	}
	if event == "" {
		event, _ = payload["type"].(string)
	}

	switch event {
	case "message_start":
		return e.onMessageStart(payload)
	case "content_block_start":
		return e.onContentBlockStart(payload)
	case "content_block_delta":
		return e.onContentBlockDelta(payload)
	case "content_block_stop":
		index, _ := payload["index"].(float64)
		return e.flushToolInput(e.tools[int(index)])
	case "message_delta":
		return e.onMessageDelta(payload)
	case "message_stop":
		return e.onMessageStop()
	case "error":
		upstreamError, _ := payload["error"].(map[string]any)
		return fmt.Errorf("上游串流错误: %s", stringOr(upstreamError["message"], "上游返回错误事件"))
	default:
		// ping 等无内容事件丢弃
		return nil
	}
}

func (e *sseEncoder) onMessageStart(payload map[string]any) error {
	message, _ := payload["message"].(map[string]any)
	if message == nil {
		return nil
	}
	e.id = newChunkID(stringOr(message["id"], "unknown"))
	e.model = stringOr(message["model"], "")
	if u, ok := message["usage"].(map[string]any); ok {
		e.inputUsage = u
	}
	// 首 chunk：delta 带 role（OpenAI 惯例 content 以空串开场）
	return e.emit(chunk(e.id, e.model, map[string]any{"role": "assistant", "content": ""}, nil))
}

func (e *sseEncoder) onContentBlockStart(payload map[string]any) error {
	block, _ := payload["content_block"].(map[string]any)
	if block == nil || block["type"] != "tool_use" {
		return nil
	}
	index, _ := payload["index"].(float64)
	toolIndex := float64(len(e.toolOrder))
	if e.tools == nil {
		e.tools = map[int]*chatStreamTool{}
	}
	input := block["input"]
	if input == nil {
		input = map[string]any{}
	}
	initial, err := marshalCompact(input)
	if err != nil {
		return err
	}
	tool := &chatStreamTool{index: toolIndex, initial: initial}
	e.tools[int(index)] = tool
	e.toolOrder = append(e.toolOrder, tool)
	delta := map[string]any{"tool_calls": []any{map[string]any{
		"index": toolIndex,
		"id":    block["id"],
		"type":  "function",
		"function": map[string]any{
			"name":      block["name"],
			"arguments": "",
		},
	}}}
	return e.emit(chunk(e.id, e.model, delta, nil))
}

func (e *sseEncoder) onContentBlockDelta(payload map[string]any) error {
	deltaObj, _ := payload["delta"].(map[string]any)
	if deltaObj == nil {
		return nil
	}
	switch deltaObj["type"] {
	case "text_delta":
		text, _ := deltaObj["text"].(string)
		if text == "" {
			return nil
		}
		return e.emit(chunk(e.id, e.model, map[string]any{"content": text}, nil))
	case "input_json_delta":
		index, _ := payload["index"].(float64)
		tool := e.tools[int(index)]
		if tool == nil {
			return fmt.Errorf("上游工具参数引用了未声明的内容块 %d", int(index))
		}
		partial, _ := deltaObj["partial_json"].(string)
		if partial == "" {
			return nil
		}
		return e.emitToolArguments(tool, partial)
	case "thinking_delta":
		// 扩展思考增量 → reasoning_content（DeepSeek / GLM 系 OpenAI 兼容端点的
		// 惯例字段：增量只带 reasoning_content，不带 content，客户端据此渲染思考过程）
		text, _ := deltaObj["thinking"].(string)
		if text == "" {
			return nil
		}
		return e.emit(chunk(e.id, e.model, map[string]any{"reasoning_content": text}, nil))
	case "signature_delta":
		// 思考签名仅供上游校验，属内部凭据，不向客户端暴露
		return nil
	default:
		// 其余未知增量忽略
		return nil
	}
}

func (e *sseEncoder) onMessageDelta(payload map[string]any) error {
	if u, ok := payload["usage"].(map[string]any); ok {
		e.outputUsage = u
	}
	deltaObj, _ := payload["delta"].(map[string]any)
	if stringOf(deltaObj["stop_reason"]) == "" {
		return nil // 纯 usage 更新不是终止信号
	}
	stopReason := mapStopReason(deltaObj["stop_reason"])
	for _, tool := range e.toolOrder {
		if err := e.flushToolInput(tool); err != nil {
			return err
		}
	}
	return e.emit(chunk(e.id, e.model, map[string]any{}, stopReason))
}

func (e *sseEncoder) onMessageStop() error {
	for _, tool := range e.toolOrder {
		if err := e.flushToolInput(tool); err != nil {
			return err
		}
	}
	if e.includeUsage {
		merged := mergeUsage(e.inputUsage, e.outputUsage)
		data, err := marshalCompact(usageChunk(e.id, e.model, merged))
		if err != nil {
			return err
		}
		if err := e.write("data: " + data + "\n\n"); err != nil {
			return err
		}
	}
	if err := e.write("data: [DONE]\n\n"); err != nil {
		return err
	}
	e.finished = true
	return nil
}

func (e *sseEncoder) emitToolArguments(tool *chatStreamTool, arguments string) error {
	tool.hasArgs = true
	delta := map[string]any{"tool_calls": []any{map[string]any{
		"index": tool.index, "function": map[string]any{"arguments": arguments},
	}}}
	return e.emit(chunk(e.id, e.model, delta, nil))
}

func (e *sseEncoder) flushToolInput(tool *chatStreamTool) error {
	if tool == nil || tool.hasArgs {
		return nil
	}
	return e.emitToolArguments(tool, tool.initial)
}

func (e *sseEncoder) fail(cause error) error {
	if !e.finished {
		e.finished = true
		if err := e.emit(map[string]any{"error": map[string]any{
			"message": cause.Error(), "type": "upstream_stream_error", "code": "server_error",
		}}); err != nil {
			return err
		}
	}
	return cause
}

// emit 序列化并写出一个 chunk 事件。
func (e *sseEncoder) emit(payload map[string]any) error {
	data, err := marshalCompact(payload)
	if err != nil {
		return err
	}
	return e.write("data: " + data + "\n\n")
}

// mergeUsage 合并 message_start（input 系）与 message_delta（output）的 usage。
func mergeUsage(input, output map[string]any) map[string]any {
	return mapUsage(mergeRawUsage(input, output))
}

// mergeRawUsage 逐键合并两段原始 usage，**数值键取较大者**而非后者覆盖前者。
//
// Anthropic 的流式 usage 分两处到达：message_start 带完整 input（含缓存两系），
// message_delta 只带 output——但它常把 input_tokens 一并补发为 0。按后者覆盖会把
// 已有用量清零，客户端据此算出的计费与上下文占用都会错。
func mergeRawUsage(input, output map[string]any) map[string]any {
	merged := make(map[string]any, len(input)+len(output))
	for k, v := range input {
		merged[k] = v
	}
	for k, v := range output {
		if current, ok := merged[k]; ok {
			if a, aok := usageNumber(current); aok {
				if b, bok := usageNumber(v); bok && b < a {
					continue // 已有更大的数值，保留它
				}
			}
		}
		merged[k] = v
	}
	return merged
}

// usageNumber 把 usage 里的数值归一成 float64；非数值返回 false。
func usageNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// stringOr 取字符串值，nil 或空时回退。
func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}
