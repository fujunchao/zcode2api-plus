// HTTP 层：/v1/messages 与 /v1/models。同时导出供 OpenAI 兼容层复用的
// JSON 写出、鉴权错误、请求头汇合工具（M4）。
// 对应 Python 版 routes/gateway.py 的路由与流式透传部分。
package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"zcode2api/internal/auth"
	"zcode2api/internal/config"
)

// Handler 网关 HTTP 层。
type Handler struct {
	Engine *Engine
	Auth   *auth.Service
}

// Register 在 mux 上注册网关路由（鉴权内建于每个端点）。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", h.Engine.Store.Requests.Wrap(ObserveRequests("messages", h.handleMessages)))
	mux.HandleFunc("GET /v1/models", h.handleModels)
}

func (h *Handler) handleMessages(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyGatewayKey(r); e != nil {
		WriteAuthError(w, e)
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{"message": "请求体不是合法 JSON", "type": "invalid_request"},
		})
		return
	}

	// 演练通道：只在 ZCODE_DRILL_KEY 非空且密钥命中时生效，不接触账号池与上游。
	// 用途是验证「new-api → 客户端」真实链路上的终态字节形态（见方案 §4）。
	if mode, ok := drillMode(r, body); ok {
		serveDrill(w, mode)
		return
	}

	// 入口整形一次（不含 system 注入；每账号副本内注入，见 engine.tryAccount）
	NormalizeBody(body, false)

	if !ModelAllowed(body["model"]) {
		modelName := AnyToString(body["model"])
		WriteJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("模型 %s 不在可用清單內，僅支持 %s", modelName, strings.Join(AvailableModels, ", ")),
				"type":    "model_not_allowed",
			},
		})
		return
	}

	result := h.Engine.RunMessages(r.Context(), body, IncomingHeaders(r), func(d Delivery) error {
		return passthroughDeliver(w, d)
	})
	if result.Delivered {
		// abort 策略：不补帧、也不让 chunked 正常收尾，直接断开连接。客户端看到的是
		// 传输层中断（可重试），而不是「服务端业务错误」（不可重试）。
		if result.AbortConnection {
			panic(http.ErrAbortHandler)
		}
		return
	}
	WriteRunResult(w, result)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if e := h.Auth.VerifyGatewayKey(r); e != nil {
		WriteAuthError(w, e)
		return
	}
	// 双兼容超集（M4）：每项同时带 Anthropic 侧 id/display_name/type 与
	// OpenAI 侧 object/created/owned_by，顶层 object:list
	data := make([]map[string]any, 0, len(AvailableModels))
	for _, id := range AvailableModels {
		data = append(data, map[string]any{
			"id":           id,
			"type":         "model",
			"display_name": id,
			"created_at":   "2025-01-01T00:00:00Z",
			"object":       "model",
			"created":      float64(1735689600), // 2025-01-01T00:00:00Z
			"owned_by":     "zcode2api",
		})
	}
	WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// passthroughDeliver 字节级透传上游响应；SSE 每 chunk flush。
//
// 分两段：
//   - 预缓冲期（仅 SSE 且预算 > 0）：首个内容增量之前不提交响应头，这段期间的失败
//     可以换号重打，客户端一个字节都没收到（NothingDeliveredError）；
//   - 提交之后：逐块透传，失败按 config.TruncateTerminal 补终态（可重试错误 /
//     优雅收尾 / 直接断连），而不是一律写不可重试的 api_error。
func passthroughDeliver(w http.ResponseWriter, d Delivery) error {
	isSSE := strings.Contains(d.ContentType, "text/event-stream")
	gate := newPrebufferGate(w, d.StatusCode, d.ContentType, config.TruncatePrebufferBytes)
	defer gate.close()

	prebuffer := isSSE && prebufferBudget() > 0
	if !prebuffer {
		if err := gate.commit(); err != nil {
			return err
		}
	}
	gate.startTimer(prebufferBudget())

	tail := &sseTail{}
	buf := make([]byte, 32*1024)
	for {
		n, err := d.Body.Read(buf)
		if n > 0 {
			if isSSE {
				tail.feed(buf[:n])
			}
			if prebuffer {
				shouldCommit, perr := gate.push(buf[:n])
				if perr != nil {
					return perr
				}
				// 出现真正的内容就立刻提交：预缓冲不能拖慢首字。
				if shouldCommit || tail.contentSeen() {
					if cerr := gate.commit(); cerr != nil {
						return cerr
					}
				}
			} else if werr := gate.write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			if isSSE {
				tail.finish()
			}
			return gate.commit()
		}
		if err != nil {
			if !isSSE {
				return err
			}
			tail.finish()
			// 上游业务错误可能仍在预缓冲中：先原样交付，不追加传输中断帧。
			if IsUpstreamErrorEvent(err) {
				if cerr := gate.commit(); cerr != nil {
					return cerr
				}
				return err
			}
			if gate.claimUndelivered() {
				// 客户端一个字节都没收到：交给引擎换号重打。
				return &NothingDeliveredError{Err: err}
			}
			return writeTruncateTerminal(func(frame string) error {
				return gate.write([]byte(frame))
			}, tail, err)
		}
	}
}

// ── 演练通道 ────────────────────────────────────────────────────────────────

// drillMode 解析演练请求；返回模式与是否命中。
//
// 只有 config.DrillKey 非空时才可能命中，密钥比对走常量时间比较。支持两种传递方式：
// 请求头 X-Zcode-Drill（可配 X-Zcode-Drill-Mode 选 ok/truncate），或请求体
// metadata.zcode_drill（new-api 只透传部分请求头时用体兜底）。
func drillMode(r *http.Request, body map[string]any) (string, bool) {
	key := strings.TrimSpace(config.DrillKey)
	if key == "" {
		return "", false
	}
	token := strings.TrimSpace(r.Header.Get("X-Zcode-Drill"))
	mode := strings.TrimSpace(r.Header.Get("X-Zcode-Drill-Mode"))
	if token == "" && body != nil {
		if meta, ok := body["metadata"].(map[string]any); ok {
			token, _ = meta["zcode_drill"].(string)
			if m, _ := meta["zcode_drill_mode"].(string); m != "" {
				mode = m
			}
		}
	}
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
		return "", false
	}
	if mode != "ok" {
		mode = "truncate"
	}
	return mode, true
}

// serveDrill 产出一段可控的 SSE：3 个文本增量后按当前策略收尾（ok 模式正常结束）。
//
// 刻意走与真实流量完全相同的终态函数，才能验证「客户端到底会看到什么」。
func serveDrill(w http.ResponseWriter, mode string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(frame string) error {
		if _, err := io.WriteString(w, frame); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	tail := &sseTail{}
	frames := []string{
		sseFrame("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_drill", "type": "message", "role": "assistant", "model": "drill",
				"content": []any{}, "stop_reason": nil,
				"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
			},
		}),
		sseFrame("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		}),
		sseFrame("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "演练：这一段来自网关演练通道。"},
		}),
	}
	for _, frame := range frames {
		if err := write(frame); err != nil {
			return
		}
		tail.feed([]byte(frame))
	}

	if mode == "ok" {
		for _, frame := range []string{
			sseFrame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
			sseFrame("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 12},
			}),
			sseFrame("message_stop", map[string]any{"type": "message_stop"}),
		} {
			if err := write(frame); err != nil {
				return
			}
		}
		return
	}

	if err := writeTruncateTerminal(write, tail, errors.New("drill: unexpected EOF")); err != nil {
		var aborted *AbortedDeliveryError
		if errors.As(err, &aborted) {
			panic(http.ErrAbortHandler)
		}
	}
}

// IncomingHeaders 汇合客户端请求头（多值逗号连接，对齐 Python request.headers）。
func IncomingHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for k, vs := range r.Header {
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	// 与 Python JSONResponse 对齐：紧凑序列化、不转义 HTML、无尾部换行
	data, err := marshalJSON(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"响应序列化失败","type":"internal_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// WriteAuthError 鉴权错误返回 FastAPI 的 {"detail": ...} 形态（与 Python 版一致）。
func WriteAuthError(w http.ResponseWriter, e *auth.AuthError) {
	WriteJSON(w, e.Status, map[string]any{"detail": e.Message})
}

// AnyToString 对应 Python str(v or "")：nil → 空串。
func AnyToString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
