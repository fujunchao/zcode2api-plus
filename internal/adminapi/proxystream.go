// 带凭据的短流探测：验证一条线路能否完整承载 SSE 流。
//
// 与 proxies.go 的可达性探测（probeProxy）互补：那只发浅 GET、只回答「能不能连上
// z.ai」；本探测借用该线路绑定的 JWT 账号发一条**最小化的真实流式请求**，回答
// 「响应头能不能到、首个数据行什么时候到、流能不能走到 message_stop」——
// 「能连上、能协商、但流在中途被掐」（2026-09-22 事故的 054 线路那一族）只有
// 这种探测才看得见。
//
// ⚠️ 能力边界（写在这里防止误用）：探测预算 30 秒，**测不出 300s 量级的连接
// 时长上限**——那类墙只有 M1 的线路断流熔断（真实流量计数）能兜住。探测是
// 手动触发的运维工具，不进周期巡检：每次探测都是一次真实上游请求。
package adminapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
)

// streamProbeBudget 整体预算：覆盖「连接 + 响应头 + 首个数据行 + 走完短流」。
// 探测请求 max_tokens=16 且 effort=low，正常应在数秒内完成。
const streamProbeBudget = 30 * time.Second

// handleStreamTestProxy 手动触发一条线路的短流探测（后台「线路」页按钮）。
func (h *Handler) handleStreamTestProxy(w http.ResponseWriter, r *http.Request) {
	profileID := r.PathValue("profile_id")
	var profile *store.ProxyProfile
	found := h.Store.ListProxyProfiles()
	for i := range found {
		if found[i].ID == profileID {
			profile = &found[i]
			break
		}
	}
	if profile == nil {
		writeAPIError(w, errNotFound("代理配置不存在"))
		return
	}
	// 借一条绑定该线路、启用且未归档的 JWT 账号的凭据（副本）。额度消耗可忽略
	//（max_tokens=16）；探测本身不写任何账号状态、不入用量账。
	var probeAcc *model.Account
	for _, a := range h.Store.ListAccounts(model.ProviderZai) {
		if !a.Enabled || a.ArchivedAt != nil || a.Mode != "jwt" || a.JWTToken == nil {
			continue
		}
		if a.ProxyID != nil && *a.ProxyID == profileID {
			probeAcc = a
			break
		}
	}
	if probeAcc == nil {
		writeAPIError(w, errBadRequest("该线路没有绑定启用的 JWT 账号，无法做流式探测"))
		return
	}
	writeJSON(w, http.StatusOK, streamProbeLine(*profile, probeAcc))
}

// streamProbeLine 经指定线路发最小流式请求并给出判定。永不 panic、不落任何状态；
// 全部结论通过返回的 map 交给后台展示。
func streamProbeLine(profile store.ProxyProfile, acc *model.Account) map[string]any {
	start := time.Now()
	result := map[string]any{
		"profile":  profile.Name,
		"account":  acc.Name,
		"budget_s": int(streamProbeBudget.Seconds()),
	}
	fail := func(verdict string, errText string) map[string]any {
		result["ok"] = false
		result["verdict"] = verdict
		if errText != "" {
			result["error"] = errText
		}
		result["total_ms"] = time.Since(start).Milliseconds()
		return result
	}

	// 最小请求：flash + max_tokens=16 + effort=low（压低思考时长，别让探测本身
	// 变成一次长思考）。NormalizeBody 做 JWT 必需的 zcode_system 注入（缺失时
	// 上游一律 405，探测就废了）。
	body := map[string]any{
		"model":      "glm-5.3-flash",
		"max_tokens": 16,
		"stream":     true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "ping"}},
		}},
		"output_config": map[string]any{"effort": "low"},
	}
	body = gateway.NormalizeBody(body, true)
	payload, err := json.Marshal(body)
	if err != nil {
		return fail("构造探测请求失败", err.Error())
	}
	built, err := upstream.BuildRequest(acc, "", "", nil)
	if err != nil {
		return fail("构造探测请求失败", err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), streamProbeBudget)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, built.URL, bytes.NewReader(payload))
	if err != nil {
		return fail("构造探测请求失败", err.Error())
	}
	for k, v := range built.Headers {
		httpReq.Header.Set(k, v)
	}
	// 与网关同源的出站传输（经该线路的代理、响应头 20s 上限）；不能设整体
	// Client.Timeout——那会把流本身掐断，探测就永远「失败」。
	transport, err := proxy.TransportForTimeout(profile.URL, 20*time.Second)
	if err != nil {
		return fail("线路地址无效", err.Error())
	}
	resp, err := (&http.Client{Transport: transport}).Do(httpReq)
	if err != nil {
		return fail("线路不可达或上游无响应", err.Error())
	}
	defer resp.Body.Close()
	result["status"] = resp.StatusCode

	// 任意完整业务响应（401/403/429/3007 验证码挑战、非流式 JSON…）都证明这条
	// 线路能完整承载「请求 + 响应」：连通性层面线路是好的。验证码挑战不算失败，
	// 也不去消耗求解——探测要验证的是线路，不是账号。
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		preview := make([]byte, 256)
		n, _ := resp.Body.Read(preview)
		result["ok"] = true
		result["verdict"] = fmt.Sprintf("连通性通过（上游返回完整业务响应 HTTP %d，未走流式）", resp.StatusCode)
		result["preview"] = string(preview[:n])
		result["total_ms"] = time.Since(start).Milliseconds()
		return result
	}

	// SSE：逐行走到 message_stop。区分三种坏法：一个数据行都没到（缓冲型线路）、
	// 数据到了但流被中途掐断（EOF/中断）、流正常结束却缺 message_stop。
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	var firstDataMs int64
	sawStop := false
	lines := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		lines++
		if firstDataMs == 0 {
			firstDataMs = time.Since(start).Milliseconds()
			result["first_data_ms"] = firstDataMs
		}
		if strings.Contains(line, "message_stop") {
			sawStop = true
			break
		}
	}
	result["data_lines"] = lines
	result["total_ms"] = time.Since(start).Milliseconds()
	if sawStop {
		result["ok"] = true
		result["verdict"] = "通过：完整收到 message_stop"
		return result
	}
	scanErr := scanner.Err()
	if scanErr == nil {
		return fail("流在 message_stop 之前结束", "")
	}
	if ctx.Err() != nil {
		if lines == 0 {
			return fail("预算内未收到任何数据行（疑似缓冲型线路或上游无响应）", scanErr.Error())
		}
		return fail("首个数据行之后停滞，直至预算用尽", scanErr.Error())
	}
	return fail("流被中途掐断", scanErr.Error())
}
