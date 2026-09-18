// Responses 流式重编码：按上游内容块维护稳定的输出项目索引，补全项目与内容生命周期。
// 工具参数按 Anthropic block index 路由，不能把交错增量一律写入最后一个工具。
package openai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

func reencodeResponsesSSE(body io.Reader, write func(string) error, parallelTools bool) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	enc := &responsesEncoder{
		write: write, startedAt: time.Now(), parallelTools: parallelTools,
		blocks: map[int]*responsesBlock{}, counts: map[string]int{},
	}
	event := ""
	var data strings.Builder
	flush := func() error { return enc.dispatch(event, data.String()) }
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
			event, data = "", strings.Builder{}
		}
	}
	if err := scanner.Err(); err != nil {
		return enc.fail(err)
	}
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

// responsesBlock 的 outputIndex 创建后不再改变；items 保存首次声明顺序。
type responsesBlock struct {
	kind        string
	outputIndex int
	item        map[string]any
	part        map[string]any
	text        strings.Builder
	initialArgs string
	hasArgs     bool
}

type responsesEncoder struct {
	write         func(string) error
	startedAt     time.Time
	respID        string
	model         string
	created       float64
	finished      bool
	stopReason    string
	sequence      int
	parallelTools bool
	inputUsage    map[string]any
	outputUsage   map[string]any
	blocks        map[int]*responsesBlock
	items         []*responsesBlock
	counts        map[string]int
}

func (e *responsesEncoder) emit(name string, payload map[string]any) error {
	payload["type"] = name
	payload["sequence_number"] = e.sequence
	e.sequence++
	raw, err := marshalCompact(payload)
	if err != nil {
		return err
	}
	return e.write("event: " + name + "\ndata: " + raw + "\n\n")
}

func (e *responsesEncoder) dispatch(event, data string) error {
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
		block, _ := payload["content_block"].(map[string]any)
		kind, _ := block["type"].(string)
		if kind != "text" && kind != "thinking" && kind != "tool_use" {
			return nil
		}
		index, _ := payload["index"].(float64)
		b, err := e.openBlock(int(index), kind, block)
		if err != nil {
			return err
		}
		if kind == "text" {
			return e.appendDelta(b, stringOf(block["text"]))
		}
		if kind == "thinking" {
			return e.appendDelta(b, stringOf(block["thinking"]))
		}
	case "content_block_delta":
		return e.onContentBlockDelta(payload)
	case "message_delta":
		if usage, ok := payload["usage"].(map[string]any); ok {
			e.outputUsage = usage
		}
		if delta, ok := payload["delta"].(map[string]any); ok {
			if reason := stringOf(delta["stop_reason"]); reason != "" {
				e.stopReason = reason
			}
		}
	case "message_stop":
		return e.onMessageStop()
	case "error":
		upstreamError, _ := payload["error"].(map[string]any)
		return fmt.Errorf("上游串流错误: %s", stringOr(upstreamError["message"], "上游返回错误事件"))
	}
	// content_block_stop 暂不输出 done：待 message_stop 时按项目顺序统一完成，
	// 让所有 done 事件都处于最终 response 之前，并保留交错工具参数的独立缓冲。
	return nil
}

func (e *responsesEncoder) onMessageStart(payload map[string]any) error {
	message, _ := payload["message"].(map[string]any)
	e.respID = stringOr(message["id"], "unknown")
	e.model = stringOf(message["model"])
	e.created = float64(e.startedAt.Unix())
	e.inputUsage, _ = message["usage"].(map[string]any)
	if err := e.emit("response.created", map[string]any{"response": e.responseEnvelope("in_progress")}); err != nil {
		return err
	}
	return e.emit("response.in_progress", map[string]any{"response": e.responseEnvelope("in_progress")})
}

func (e *responsesEncoder) itemID(prefix, kind string) string {
	id := prefix + e.respID
	if n := e.counts[kind]; n > 0 {
		id = fmt.Sprintf("%s_%d", id, n)
	}
	e.counts[kind]++
	return id
}

func (e *responsesEncoder) openBlock(sourceIndex int, kind string, raw map[string]any) (*responsesBlock, error) {
	b := &responsesBlock{kind: kind, outputIndex: len(e.items)}
	switch kind {
	case "text":
		b.item = map[string]any{
			"type": "message", "id": e.itemID("msg_", kind), "role": "assistant",
			"status": "in_progress", "content": []any{},
		}
		b.part = map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
	case "thinking":
		b.item = map[string]any{
			"type": "reasoning", "id": e.itemID("rs_", kind),
			"status": "in_progress", "summary": []any{},
		}
		b.part = map[string]any{"type": "summary_text", "text": ""}
	case "tool_use":
		id, name := stringOf(raw["id"]), stringOf(raw["name"])
		if id == "" || name == "" {
			return nil, fmt.Errorf("上游工具块缺少 id 或 name")
		}
		b.item = map[string]any{
			"type": "function_call", "id": "fc_" + id, "call_id": id,
			"name": name, "arguments": "", "status": "in_progress",
		}
		input := raw["input"]
		if input == nil {
			input = map[string]any{}
		}
		b.initialArgs, _ = marshalCompact(input)
	}
	e.blocks[sourceIndex] = b
	e.items = append(e.items, b)
	if err := e.emit("response.output_item.added", map[string]any{
		"output_index": b.outputIndex, "item": b.item,
	}); err != nil {
		return nil, err
	}
	switch kind {
	case "text":
		b.item["content"] = []any{b.part}
		if err := e.emit("response.content_part.added", b.partEvent()); err != nil {
			return nil, err
		}
	case "thinking":
		b.item["summary"] = []any{b.part}
		if err := e.emit("response.reasoning_summary_part.added", b.partEvent()); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *responsesBlock) location() map[string]any {
	out := map[string]any{"item_id": b.item["id"], "output_index": b.outputIndex}
	switch b.kind {
	case "text":
		out["content_index"] = 0
	case "thinking":
		out["summary_index"] = 0
	}
	return out
}

func (b *responsesBlock) partEvent() map[string]any {
	out := b.location()
	out["part"] = b.part
	return out
}

func (e *responsesEncoder) onContentBlockDelta(payload map[string]any) error {
	delta, _ := payload["delta"].(map[string]any)
	kind, text := "", ""
	switch delta["type"] {
	case "text_delta":
		kind, text = "text", stringOf(delta["text"])
	case "thinking_delta":
		kind, text = "thinking", stringOf(delta["thinking"])
	case "input_json_delta":
		kind, text = "tool_use", stringOf(delta["partial_json"])
	default:
		return nil // signature_delta / redacted_thinking 不对客户端暴露
	}
	if text == "" {
		return nil
	}
	index, _ := payload["index"].(float64)
	b := e.blocks[int(index)]
	if b == nil || b.kind != kind {
		if kind == "tool_use" {
			return fmt.Errorf("上游工具参数引用了未声明的内容块 %d", int(index))
		}
		// 容忍兼容上游省略 text/thinking 的 start 事件：先声明项目再投递增量。
		var err error
		b, err = e.openBlock(int(index), kind, nil)
		if err != nil {
			return err
		}
	}
	return e.appendDelta(b, text)
}

func (e *responsesEncoder) appendDelta(b *responsesBlock, text string) error {
	if text == "" {
		return nil
	}
	b.text.WriteString(text)
	payload := b.location()
	payload["delta"] = text
	switch b.kind {
	case "text":
		b.part["text"] = b.text.String()
		payload["logprobs"] = []any{}
		return e.emit("response.output_text.delta", payload)
	case "thinking":
		b.part["text"] = b.text.String()
		return e.emit("response.reasoning_summary_text.delta", payload)
	case "tool_use":
		b.hasArgs = true
		b.item["arguments"] = b.text.String()
		return e.emit("response.function_call_arguments.delta", payload)
	}
	return nil
}

func (e *responsesEncoder) finishBlock(b *responsesBlock, status string) error {
	b.item["status"] = status
	payload := b.location()
	switch b.kind {
	case "text":
		payload["text"], payload["logprobs"] = b.text.String(), []any{}
		if err := e.emit("response.output_text.done", payload); err != nil {
			return err
		}
		if err := e.emit("response.content_part.done", b.partEvent()); err != nil {
			return err
		}
	case "thinking":
		payload["text"] = b.text.String()
		if err := e.emit("response.reasoning_summary_text.done", payload); err != nil {
			return err
		}
		if err := e.emit("response.reasoning_summary_part.done", b.partEvent()); err != nil {
			return err
		}
	case "tool_use":
		if !b.hasArgs {
			if err := e.appendDelta(b, b.initialArgs); err != nil {
				return err
			}
		}
		payload["arguments"], payload["name"] = b.item["arguments"], b.item["name"]
		if err := e.emit("response.function_call_arguments.done", payload); err != nil {
			return err
		}
	}
	return e.emit("response.output_item.done", map[string]any{"output_index": b.outputIndex, "item": b.item})
}

func (e *responsesEncoder) onMessageStop() error {
	if e.finished {
		return nil
	}
	status, _ := responsesCompletion(e.stopReason)
	for i, b := range e.items {
		itemStatus := "completed"
		if status == "incomplete" && i == len(e.items)-1 {
			itemStatus = "incomplete"
		}
		if err := e.finishBlock(b, itemStatus); err != nil {
			return err
		}
	}
	e.finished = true
	return e.emit("response."+status, map[string]any{"response": e.responseEnvelope(status)})
}

// fail 通过终止事件交付失败，同时返回错误，避免引擎把中断串流累计为完整交付。
func (e *responsesEncoder) fail(cause error) error {
	if e.finished {
		return cause
	}
	if e.respID == "" {
		if err := e.onMessageStart(nil); err != nil {
			return err
		}
	}
	for _, b := range e.items {
		b.item["status"] = "incomplete"
	}
	e.finished = true
	response := e.responseEnvelope("failed")
	response["error"] = map[string]any{"code": "server_error", "message": cause.Error()}
	if err := e.emit("response.failed", map[string]any{"response": response}); err != nil {
		return err
	}
	return cause
}

func (e *responsesEncoder) responseEnvelope(status string) map[string]any {
	output := make([]any, 0, len(e.items))
	for _, b := range e.items {
		output = append(output, b.item)
	}
	var usage any
	if status != "in_progress" {
		usage = responsesUsage(mergeRawUsage(e.inputUsage, e.outputUsage))
	}
	var incompleteDetails any
	if status == "incomplete" {
		_, incompleteDetails = responsesCompletion(e.stopReason)
	}
	return map[string]any{
		"id": "resp_" + e.respID, "object": "response", "created_at": e.created,
		"model": e.model, "status": status, "output": output, "usage": usage,
		"parallel_tool_calls": e.parallelTools, "error": nil, "incomplete_details": incompleteDetails,
	}
}

// 合并 message_start（input 系）与 message_delta（output）的 Anthropic usage。
// mergeRawUsage 见 stream.go：两段 usage 的数值键取 max，避免 message_delta
// 补发的 input_tokens: 0 把已统计的用量清零。
