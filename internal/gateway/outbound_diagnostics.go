package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/upstream"
)

// outboundSnapshot 只保存有界摘要，不保留正文、工具 schema、凭据或标识原文。
type outboundSnapshot struct {
	fields, model, metadataSessionRef string
}

func inspectOutboundBody(body map[string]any) outboundSnapshot {
	n := model.NormalizeModelName(body["model"])
	if n != "glm-5.3" && n != "glm-5.3-flash" {
		n = "other"
	}
	thinking, _ := body["thinking"].(map[string]any)
	output, _ := body["output_config"].(map[string]any)
	texts := []string{}
	switch v := body["system"].(type) {
	case string:
		texts = append(texts, v)
	case []any:
		// 最多记录16块；总块数仍由 requestShape 给出，超出的块不抄录。
		for _, raw := range v[:min(len(v), 16)] {
			b, _ := raw.(map[string]any)
			s, _ := b["text"].(string)
			texts = append(texts, s)
		}
	}
	sizes, refs := make([]int, len(texts)), make([]string, len(texts))
	for i, text := range texts {
		sizes[i] = len(text)
		sum := sha256.Sum256([]byte(text))
		refs[i] = hex.EncodeToString(sum[:])
	}
	prefixMatch, identityMatch := false, false
	standard := upstream.ZcodeSystemBlocks()
	if len(texts) > 0 && len(standard) >= 2 {
		prefix := standard[0].(map[string]any)["text"].(string)
		identity := standard[1].(map[string]any)["text"].(string)
		prefixMatch = strings.HasPrefix(texts[0], prefix)
		// 报告明确的逐块匹配，不把任意包含 identity 的用户文本当成官方身份。
		identityMatch = len(texts) > 1 && texts[0] == prefix && texts[1] == identity
	}
	byteJSON, _ := json.Marshal(sizes)
	refJSON, _ := json.Marshal(refs)
	meta, _ := body["metadata"].(map[string]any)
	var metadata map[string]any
	if value, ok := meta["user_id"].(string); ok && len(value) <= 4096 {
		_ = json.Unmarshal([]byte(value), &metadata)
	}
	session, _ := metadata["session_id"].(string)
	return outboundSnapshot{model: n, metadataSessionRef: attemptIDRef(session), fields: fmt.Sprintf(
		" outbound_model=%s outbound_max_tokens=%s outbound_thinking=%s outbound_effort=%s system_text_bytes=%s system_text_sha256=%s system_summary_truncated=%t official_prefix_match=%t official_identity_match=%t",
		n, diagnosticNumber(body["max_tokens"]), diagnosticChoice(thinking["type"], "enabled", "disabled", "adaptive"),
		diagnosticChoice(output["effort"], "low", "high", "max", "medium", "none", "minimal", "xhigh"), byteJSON, refJSON,
		len(texts) == 16 && inspectRequestShape(body).systemBlocks > 16, prefixMatch, identityMatch)}
}

func diagnosticChoice(value any, allowed ...string) string {
	if value == nil {
		return "-"
	}
	s, ok := value.(string)
	if !ok {
		return "other"
	}
	for _, a := range allowed {
		if s == a {
			return a
		}
	}
	return "other"
}

func diagnosticNumber(v any) string {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	case json.Number:
		var err error
		f, err = n.Float64()
		if err != nil {
			return "-"
		}
	default:
		return "-"
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "-"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func diagnosticTimestamp(value *float64) string {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value <= 0 || *value > 253402300799 {
		return "-"
	}
	seconds, frac := math.Modf(*value)
	return time.Unix(int64(seconds), int64(frac*1e9)).UTC().Format(time.RFC3339Nano)
}

func (d *ReqDiag) outboundAttemptSummary(acc, observed *model.Account, request *http.Request, now time.Time) string {
	if observed == nil {
		observed = acc
	}
	window := config.RiskSilentWindow()
	quota, known := observed.UsableQuotaForModel(d.outbound.model)
	available := "-"
	if known {
		available = diagnosticNumber(quota)
	}
	var quotaAt *float64
	if observed.StartPlanObservation.Matches(observed) {
		v := observed.StartPlanObservation.CheckedAt
		quotaAt = &v
	}
	barrier, recheck := observed.LocalSchedulingBarrier(now, window)
	dual, matches, deviceHeader, signingHeader := false, false, false, false
	if request != nil {
		key := request.Header.Get("X-Api-Key")
		dual = key != "" && request.Header.Get("Authorization") == "Bearer "+key
		matches = d.outbound.metadataSessionRef != "-" && d.outbound.metadataSessionRef != "" && d.outbound.metadataSessionRef == attemptIDRef(request.Header.Get("X-Session-Id"))
		deviceHeader = request.Header.Get("X-Device-Mid") != ""
		for _, k := range []string{"X-Client-Sig", "X-Client-Ts", "X-Client-Version", "X-Client-Nonce", "X-Client-Pow", "X-App-Id"} {
			signingHeader = signingHeader || request.Header.Get(k) != ""
		}
	}
	return d.outbound.fields + fmt.Sprintf(" account_status_before=%s account_state_identity_match=%t risk_last_at=%s risk_streak_before=%d risk_extra_window_s=%d risk_extra_active=%t local_barrier=%s local_recheck_at=%s account_checked_at=%s quota_observed_at=%s quota_model_state=%s quota_available=%s auth_dual_equal=%t metadata_session_matches_header=%t device_header_present=%t signing_header_present=%t",
		diagnosticChoice(observed.Status, "active", "cooling", "invalid", "disabled", "exhausted"), observed.Secret() == acc.Secret(),
		diagnosticTimestamp(observed.LastRiskControlAt), observed.RiskControlStreak, int64(max(window, 0)/time.Second), observed.InRiskControlSilentWindow(now, window),
		barrier, diagnosticTimestamp(recheck), diagnosticTimestamp(observed.LastCheckedAt), diagnosticTimestamp(quotaAt), observed.ModelAvailability(d.outbound.model), available,
		dual, matches, deviceHeader, signingHeader)
}
