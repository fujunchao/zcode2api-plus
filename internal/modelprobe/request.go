package modelprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/sse"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
)

const responseLimit = 1 << 20
const errorLimit = 64 << 10

func (m *Manager) send(parent context.Context, l *store.ModelProbeLease) (o store.ProxyObservation, kind string, usage model.Usage) {
	kind = "invalid_request"
	p := m.store.ModelProbeSettings()
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	a := l.Account()
	body := gateway.NormalizeBody(map[string]any{"model": store.ModelProbeModel, "max_tokens": 16, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "只回复 OK"}}}}, "output_config": map[string]any{"effort": "low"}}, true)
	attr := upstream.NewAttribution(nil, l.SessionID())
	upstream.InjectDeviceMetadata(body, a.DeviceMidOr(config.DeviceMid()), attr.SessionID)
	built, err := upstream.BuildRequestWithAttribution(a, "", "", nil, attr)
	if err != nil || built.URL != l.TargetURL() {
		return
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, built.URL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	for k, v := range built.Headers {
		req.Header.Set(k, v)
	}
	o = gateway.ModelRequestObservation(a, req, body, l.EntryID())
	if o.InvalidRequest {
		return
	}
	transport, err := proxy.TransportForTimeout(l.ProxyURL(), 20*time.Second)
	if err != nil {
		return
	}
	client := m.store.GuardModelProbeClient(&http.Client{Transport: transport}, l, m.now)
	resp, err := client.Do(req)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrModelProbeBudget):
			kind = "budget_exhausted"
		case errors.Is(err, store.ErrModelProbeConflict):
			kind = "dispatch_changed"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			kind = "timeout"
		case ctx.Err() != nil:
			kind = "canceled"
		default:
			kind = "connection_failed"
		}
		return
	}
	defer resp.Body.Close()
	o.Status = resp.StatusCode
	if resp.Request == nil || resp.Request.Method != req.Method || resp.Request.URL.String() != req.URL.String() {
		o.InvalidRequest = true
		kind = "invalid_target"
		return
	}
	limit := responseLimit
	if resp.StatusCode >= 400 {
		limit = errorLimit
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if readErr != nil || len(raw) > limit {
		kind = "incomplete_response"
		if ctx.Err() != nil {
			kind = "timeout"
		}
		return
	}
	o.BusinessCode = gateway.UpstreamBusinessCode(string(raw))
	if resp.StatusCode >= 400 {
		switch {
		case resp.StatusCode == 405 && (o.BusinessCode == "3012" || model.IsRiskControlBody(string(raw))):
			kind = "risk_control"
		case gateway.IsCaptchaError(string(raw), resp.StatusCode, resp.Header):
			kind = "captcha_required"
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			kind = "authentication_failed"
		case resp.StatusCode == 402 || gateway.IsQuotaExhaustedCode(string(raw)):
			kind = "quota_exhausted"
		case resp.StatusCode == 429:
			kind = "rate_limited"
		default:
			kind = "upstream_error"
		}
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind = "redirect_or_invalid_status"
		return
	}
	isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	u := gateway.NewUsageCollector(isSSE)
	u.Feed(raw)
	u.Finish()
	usage = u.AsDict()
	valid := u.ResponseComplete()
	if isSSE {
		valid = valid && validSSEMessage(raw)
	}
	if valid && o.BusinessCode == "" && ctx.Err() == nil {
		o.Complete = true
		kind = "success"
	} else {
		kind = "invalid_response"
	}
	return
}

// 复用 SSE 成帧器，不用字符串包含 message_stop 代替协议检查；消息 ID 是必要证据。
func validSSEMessage(raw []byte) bool {
	var decoder sse.Decoder
	started, stopped, bad := false, false, false
	observe := func(frame *sse.Event) {
		if frame == nil {
			return
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(frame.Data), &p); err != nil {
			bad = true
			return
		}
		name := frame.Name
		if name == "" {
			name, _ = p["type"].(string)
		}
		switch name {
		case "message_start":
			msg, _ := p["message"].(map[string]any)
			id, _ := msg["id"].(string)
			if started || strings.TrimSpace(id) == "" || msg["role"] != "assistant" {
				bad = true
			}
			started = true
		case "message_stop":
			if !started {
				bad = true
			}
			stopped = true
		case "error":
			bad = true
		}
	}
	for _, line := range strings.Split(string(raw), "\n") {
		observe(decoder.FeedLine(line))
	}
	observe(decoder.Finish())
	return started && stopped && !bad
}
