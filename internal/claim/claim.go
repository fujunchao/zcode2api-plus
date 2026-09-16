// 套餐领取（Z.AI billing/preview + billing/claim），对应 Python 版 app/claim.py。
// 链路：GET /billing/preview → data.plans[]；POST /billing/claim（body
// {"plan_id"}）需验证码头 X-Aliyun-Captcha-Verify-Param（+可选 Region 头）。
// 业务码沿用 zcode-switch 映射；3007 换码重试一次。请求经 clientFor 走账号代理。
package claim

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/web"
)

// ClaimError 业务失败（含上游 code 语义），消息面向使用者。
// nextAt 是上游在响应里已经算好的「下次可领时间」（data.plan.ends_at）——
// 此前只取了错误文案把时间丢掉，导致自动路径无法按上游节奏冷却、后台也无法倒计时。
type ClaimError struct {
	msg    string
	code   int
	nextAt *float64
	// captcha 标记失败源于本机验证码不可用（浏览器求解失败或人工参数缺失），
	// 而非上游判定——这类失败等的是人工回填，冷却应取最长档。
	captcha bool
}

func (e *ClaimError) Error() string { return e.msg }

// Code 上游业务码；非业务失败（网络/解码/本机验证码）为 0。
func (e *ClaimError) Code() int { return e.code }

// NextAt 下次可领时间（unix 秒）；上游未提供时为 nil。
func (e *ClaimError) NextAt() *float64 { return e.nextAt }

// IsCaptchaFailure 失败是否源于本机验证码不可用。
func (e *ClaimError) IsCaptchaFailure() bool { return e.captcha }

// businessError 构造带业务码与可领时间的失败；失败文案沿用 claimFail 映射。
func businessError(code int, body map[string]any, nextAt *float64) *ClaimError {
	return &ClaimError{msg: failMessage(code, body), code: code, nextAt: nextAt}
}

var claimFail = map[int]string{
	1001: "套餐不存在",
	1002: "活動已結束或套餐暫不可領取",
	1003: "該套餐已經領取過",
	1004: "不符合領取條件",
	1005: "今日領取名額已用完",
	3001: "領取參數錯誤，請重新整理後重試",
	3007: "驗證碼校驗失敗，請重試",
	401:  "請先登入後再領取",
}

const (
	captchaHeader       = "X-Aliyun-Captcha-Verify-Param"
	captchaRegionHeader = "X-Aliyun-Captcha-Verify-Region"
)

// Service 领取服务：captcha 求解 + billing HTTP（账号代理生效）。
type Service struct {
	Captcha *captcha.Manager
	// Client 测试注入；nil 时按账号代理构造（25s 超时，短请求语义）。
	Client HTTPClient
}

// HTTPClient 计费端点客户端抽象。
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewService 创建领取服务。
func NewService(cm *captcha.Manager) *Service {
	return &Service{Captcha: cm}
}

// clientFor 账号出站客户端：有代理走代理（25s 超时），否则默认直连。
func (s *Service) clientFor(acc *model.Account) HTTPClient {
	if s.Client != nil {
		return s.Client
	}
	if acc != nil && acc.ProxyURL != nil && *acc.ProxyURL != "" {
		if c, err := proxy.ClientFor(*acc.ProxyURL, 25*time.Second); err == nil {
			return c
		}
		web.Warn("claim", fmt.Sprintf("账号 %s 代理无效，回退直连", acc.Name))
	}
	return &http.Client{Timeout: 25 * time.Second}
}

// failMessage 业务码 → 使用者文案（带上游 msg 补充）。
func failMessage(code int, body map[string]any) string {
	base, ok := claimFail[code]
	if !ok {
		base = "領取失敗"
	}
	server := ""
	for _, key := range []string{"msg", "message"} {
		if s, isStr := body[key].(string); isStr && s != "" {
			server = s
			break
		}
	}
	if server != "" {
		return fmt.Sprintf("%s（%s）", base, server)
	}
	return base
}

// ParsePlan 提取可领取套餐（plan_id/name/描述/优先级 + model_usage token 授权项）。
func ParsePlan(raw map[string]any) map[string]any {
	planID := orFirst(raw, "plan_id", "planId")
	if planID == "" {
		return nil
	}
	var grants []any
	if entitlements, ok := raw["entitlements"].([]any); ok {
		for _, item := range entitlements {
			ent, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if meter, _ := ent["meter"].(string); meter != "model_usage" {
				continue
			}
			if unit, _ := ent["unit_type"].(string); unit != "token" {
				continue
			}
			name := orFirst(ent, "show_name", "showName")
			if name == "" {
				continue
			}
			grants = append(grants, map[string]any{
				"name":   name,
				"units":  asFloat(firstNonNil(ent["grant_units"], ent["grantUnits"])),
				"period": orDefault(orFirst(ent, "period"), "one_time"),
			})
		}
	}
	return map[string]any{
		"plan_id":     planID,
		"name":        orFirst(raw, "name"),
		"description": orFirst(raw, "description"),
		"priority":    asFloat(firstNonNil(raw["priority"])),
		"grants":      grants,
	}
}

// JWTUserID JWT payload 的 user_id（官方客户端事件上报以 user_id 标识用户）。
// 实现已下沉到 model——账号身份判据（跨 token 刷新识别同一账号）也在用它，
// 而 claim 依赖 model，不能反向引用。此处保留导出 API，调用点无需变动。
func JWTUserID(jwtToken string) string { return model.JWTUserID(jwtToken) }

// parsePlanWindow 取响应里 data.plan 的起止时间（unix 秒）。
// 上游在成功响应与 1005（当日名额用完）里都会给出 ends_at 作为下次可领时间——
// 这是它自己算好的节奏，比在本地拍一个冷却时长更准。
func parsePlanWindow(body map[string]any) (startsAt, endsAt *float64) {
	data, ok := body["data"].(map[string]any)
	if !ok {
		return nil, nil
	}
	plan, ok := data["plan"].(map[string]any)
	if !ok {
		return nil, nil
	}
	return unixSeconds(plan["starts_at"]), unixSeconds(plan["ends_at"])
}

// unixSeconds 宽松取时间戳（float64 / int / json.Number / 数字字符串），缺失返回 nil。
func unixSeconds(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return normalizeEpoch(n)
	case int:
		return normalizeEpoch(float64(n))
	case int64:
		return normalizeEpoch(float64(n))
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return normalizeEpoch(f)
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return normalizeEpoch(f)
		}
	}
	return nil
}

// normalizeEpoch 毫秒时间戳归一为秒；非正值视为缺失。
func normalizeEpoch(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	if v > 1e12 {
		v /= 1000
	}
	return &v
}

// billingRequest 统一计费请求：网络错误/鉴权失败统一转 ClaimError。
func (s *Service) billingRequest(acc *model.Account, method, path string, headers map[string]string, payload []byte) (map[string]any, error) {
	req, err := http.NewRequest(method, config.ZcodeBillingBase+path, bytes.NewReader(payload))
	if err != nil {
		return nil, &ClaimError{msg: fmt.Sprintf("上游網路錯誤: %v", err)}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := s.clientFor(acc).Do(req)
	if err != nil {
		return nil, &ClaimError{msg: fmt.Sprintf("上游網路錯誤: %v", err)}
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(res.Body)
	if res.StatusCode == 401 || res.StatusCode == 403 {
		text := lower(raw)
		if !containsAny(text, "captcha", "verify") {
			return nil, &ClaimError{msg: fmt.Sprintf("鑑權失敗 HTTP %d", res.StatusCode)}
		}
	}
	if readErr != nil {
		return nil, &ClaimError{msg: fmt.Sprintf("上游回應讀取失敗: %v", readErr)}
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, &ClaimError{msg: fmt.Sprintf("上游回應非 JSON HTTP %d", res.StatusCode)}
	}
	return body, nil
}

// authHeaders 计费端点鉴权头（与 quota._auth_headers 同源形态）。
// X-Device-Mid 取账号自己的指纹（缺失回退全局值）：同机多账号共用一份会被上游关联。
func authHeaders(acc *model.Account) map[string]string {
	headers := map[string]string{
		"Content-Type":        "application/json",
		"User-Agent":          config.UserAgent,
		"X-ZCode-App-Version": config.ZcodeClientVersion,
		"X-Platform":          config.ZcodeClientPlatform,
		"X-Device-Mid":        acc.DeviceMidOr(config.DeviceMid()),
		"HTTP-Referer":        "https://zcode.z.ai/",
	}
	if acc.Mode == "jwt" && acc.JWTToken != nil {
		headers["Authorization"] = "Bearer " + *acc.JWTToken
	} else if acc.APIKey != nil {
		headers["x-api-key"] = *acc.APIKey
	}
	return headers
}

// ReportActivationEvents 上报激活事件（app_launch + app_daily_active），
// 返回错误文案或空串；首个失败即中止（日活键上游按 device_mid+日期去重）。
func ReportActivationEvents(acc *model.Account) string {
	userID := JWTUserID(acc.Secret())
	if userID == "" {
		return "JWT 無 user_id，跳過激活上報"
	}
	deviceMid := acc.DeviceMidOr(config.DeviceMid())
	for _, element := range ActivationElements {
		if err := PostActivationEvent(userID, element, deviceMid); err != nil {
			return fmt.Sprintf("激活事件 %s 上報失敗: %v", element, err)
		}
	}
	return ""
}

// PreviewPlans 拉取账号当前可领取套餐，按优先级降序。
func (s *Service) PreviewPlans(acc *model.Account) ([]map[string]any, error) {
	headers := authHeaders(acc)
	body, err := s.billingRequest(acc, http.MethodGet,
		"/billing/preview?app_version="+config.ZcodeClientVersion+"&platform="+config.ZcodeClientPlatform,
		headers, nil)
	if err != nil {
		return nil, err
	}
	if code := BusinessCode(body); code != 0 {
		_, endsAt := parsePlanWindow(body)
		return nil, businessError(code, body, endsAt)
	}
	var rawPlans []any
	if data, ok := body["data"].(map[string]any); ok {
		rawPlans, _ = data["plans"].([]any)
	}
	plans := make([]map[string]any, 0, len(rawPlans))
	for _, raw := range rawPlans {
		if obj, ok := raw.(map[string]any); ok {
			if parsed := ParsePlan(obj); parsed != nil {
				plans = append(plans, parsed)
			}
		}
	}
	sort.Slice(plans, func(i, j int) bool {
		pi, pj := asFloat(plans[i]["priority"]), asFloat(plans[j]["priority"])
		if pi != pj {
			return pi > pj
		}
		return plans[i]["plan_id"].(string) < plans[j]["plan_id"].(string)
	})
	return plans, nil
}

// Claim 领取套餐。planID 为空时自动选优先级最高的可领套餐。
// 返回 {plan_id, plan_name, grants}；3007（验证码失败）自动换码重试一次。
func (s *Service) Claim(acc *model.Account, planID string) (map[string]any, error) {
	if acc.Mode != "jwt" || acc.JWTToken == nil || *acc.JWTToken == "" {
		return nil, &ClaimError{msg: "僅 Coding Plan (JWT) 賬號支持領取"}
	}

	planName := ""
	grants := []any{}
	if planID == "" {
		plans, err := s.PreviewPlans(acc)
		if err != nil {
			return nil, err
		}
		if len(plans) == 0 {
			return nil, &ClaimError{msg: "沒有待領取的套餐"}
		}
		best := plans[0]
		planID, _ = best["plan_id"].(string)
		planName, _ = best["name"].(string)
		if planName == "" {
			planName = planID
		}
		if g, ok := best["grants"].([]any); ok {
			grants = g
		}
	}

	var lastErr error
	for attempt := 1; attempt <= 2; attempt++ {
		token, err := s.Captcha.GetVerifyParam(nil)
		if err != nil || token == nil {
			return nil, &ClaimError{
				msg:     "驗證碼求解失敗或已停用，請到後台驗證碼頁面回填參數",
				captcha: true,
			}
		}
		headers := claimHeaders(acc, token.VerifyParam, token.Region)
		payload, _ := json.Marshal(map[string]string{"plan_id": planID})
		body, err := s.billingRequest(acc, http.MethodPost, "/billing/claim", headers, payload)
		if err != nil {
			return nil, err
		}
		code := BusinessCode(body)
		_, endsAt := parsePlanWindow(body)
		if code == 0 {
			out := map[string]any{"plan_id": planID, "plan_name": planName, "grants": grants}
			if endsAt != nil {
				// 上游自己算好的下次可领时间，供调用方落盘与倒计时展示。
				// 缺省时不写该键，避免 *float64(nil) 这类形态混进 outcome。
				out["next_at"] = *endsAt
			}
			return out, nil
		}
		if code == 3007 && attempt == 1 {
			web.Warn("claim", fmt.Sprintf("账号 %s 验证码被拒，换码重试", acc.Name))
			s.Captcha.Invalidate()
			lastErr = businessError(code, body, endsAt)
			continue
		}
		return nil, businessError(code, body, endsAt)
	}
	if lastErr == nil {
		lastErr = errors.New("領取失敗")
	}
	return nil, lastErr
}

// claimHeaders billing/claim 客户端请求头形态（asar claimManualPlan）。
// 实测缺版本/平台头时即使验证码有效也 3007；X-Device-Mid 由 authHeaders 提供。
func claimHeaders(acc *model.Account, verifyParam, region string) map[string]string {
	headers := authHeaders(acc)
	headers[captchaHeader] = verifyParam
	if region != "" {
		headers[captchaRegionHeader] = region
	}
	headers["X-ZCode-App-Version"] = config.ZcodeClientVersion
	headers["X-Platform"] = config.ZcodeClientPlatform
	return headers
}

// AutoClaimAllPlans 新账号入池自动领取：激活上报 + 逐个领取全部可领套餐。
// 入池链路的 fire-and-forget 收尾：任何失败只记日志/返回 outcome，绝不 panic。
// 重复执行安全（上游 1003 已领取过幂等）。
func (s *Service) AutoClaimAllPlans(acc *model.Account) []map[string]any {
	if acc.Mode != "jwt" || acc.JWTToken == nil || *acc.JWTToken == "" {
		return nil
	}
	outcomes := []map[string]any{}

	if err := ReportActivationEvents(acc); err != "" {
		web.Warn("claim", fmt.Sprintf("账号 %s 激活上报失败: %s", acc.Name, err))
	}

	plans, err := s.PreviewPlans(acc)
	if err != nil {
		outcomes = append(outcomes, FailureOutcome(acc, "", err))
		var ce *ClaimError
		if errors.As(err, &ce) {
			web.Ok("claim", fmt.Sprintf("账号 %s 无可领套餐（%s）", acc.Name, err))
		} else {
			web.Warn("claim", fmt.Sprintf("账号 %s preview 异常: %v", acc.Name, err))
		}
		return outcomes
	}
	if len(plans) == 0 {
		web.Ok("claim", fmt.Sprintf("账号 %s 上游无投放套餐，跳过领取", acc.Name))
		return outcomes
	}

	for _, plan := range plans {
		planID, _ := plan["plan_id"].(string)
		result, err := s.Claim(acc, planID)
		if err != nil {
			outcomes = append(outcomes, FailureOutcome(acc, planID, err))
			web.Warn("claim", fmt.Sprintf("账号 %s 自动领取 %s 失败: %v", acc.Name, planID, err))
			continue
		}
		outcome := map[string]any{
			"account_id": acc.ID, "account_name": acc.Name, "ok": true,
		}
		for k, v := range result {
			outcome[k] = v
		}
		outcomes = append(outcomes, outcome)
		planName, _ := result["plan_name"].(string)
		if planName == "" {
			planName = planID
		}
		web.Ok("claim", fmt.Sprintf("账号 %s 自动领取成功: %s", acc.Name, planName))
	}
	return outcomes
}

// FailureOutcome 领取失败的 outcome。带上业务码与上游给出的下次可领时间，
// 调用方（adminapi）据此决定冷却档位并落盘，不必再解析错误文案。
func FailureOutcome(acc *model.Account, planID string, err error) map[string]any {
	out := map[string]any{
		"account_id": acc.ID, "account_name": acc.Name,
		"ok": false, "message": err.Error(),
	}
	if planID != "" {
		out["plan_id"] = planID
	}
	var ce *ClaimError
	if errors.As(err, &ce) {
		out["code"] = ce.Code()
		if next := ce.NextAt(); next != nil {
			out["next_at"] = *next
		}
		out["captcha"] = ce.IsCaptchaFailure()
	}
	return out
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// orFirst 依次取字符串字段（含 camelCase 别名），全空返回空串。
func orFirst(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// firstNonNil 取首个非 nil 值。
func firstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// asFloat 宽松取 float（float64/int；JSON 解析一律 float64）。
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

// lower 小写化原始体（鉴权失败文本检测）。
func lower(raw []byte) string {
	out := make([]byte, len(raw))
	for i, b := range raw {
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		out[i] = b
	}
	return string(out)
}

// containsAny 文本包含任一子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// newUUID 随机 UUID v4（事件体 event_id）。
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
