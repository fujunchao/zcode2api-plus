package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
	"zcode2api/internal/upstream"
	"zcode2api/internal/web"
)

type proxyObservationKey struct{}

// ModelAttemptClient 禁止一次许可下自动跟随重定向；第二个目标必须是新的显式派发，
// 不能靠 http.Client 的隐式第二跳继续使用退役代理或把账号凭据带到其它目标。
func ModelAttemptClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

type proxyAttemptObservation struct {
	once             sync.Once
	healthOnce       sync.Once
	store            *store.Store
	account          *model.Account
	egress           store.ProxyEgress
	value            store.ProxyObservation
	responseVerified bool
	responseStatus   int
	targetRef        string
	beganAt          time.Time
	firstByteAt      time.Time
}

// 判定引用只存在于本进程的十分钟证据窗口，不输出、不持久化。其密钥须稳定到进程结束，
// 不能借用会到期轮换的日志 HMAC，否则跨日志周期的同一次 HTTP 都可能被误判为其它目标。
var observationKey = func() [32]byte {
	var key [32]byte
	_, _ = rand.Read(key[:])
	return key
}()

func observationRef(value string) string {
	if value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, observationKey[:])
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// BeginProxyAttempt 获取本次派发许可并冻结去敏证据；不让日志容器承担调度职责。
func BeginProxyAttempt(ctx context.Context, st *store.Store, acc *model.Account, egress store.ProxyEgress, request *http.Request, body map[string]any, entry string) (context.Context, func(), error) {
	release, err := st.AcquireModelDispatch(acc, egress)
	if err != nil {
		return ctx, nil, err
	}
	if acc == nil || request == nil || request.URL == nil {
		return ctx, release, nil
	}
	acc = acc.Clone()
	value := ModelRequestObservation(acc, request, body, entry)
	p := &proxyAttemptObservation{store: st, account: acc, egress: egress, value: value, targetRef: observationRef(request.URL.String()), beganAt: time.Now()}
	attemptCtx := context.WithValue(ctx, proxyObservationKey{}, p)
	idle := st.TrackModelAccountUse(acc.ID)
	var once sync.Once
	finish := func() {
		once.Do(func() {
			// 没有走完整模型交付的路径也有终态；不把响应头或usage当成成功。
			kind := "incomplete_response"
			if p.responseStatus == 0 {
				kind = "connection_failed"
			} else if !p.responseVerified {
				kind = "invalid_target"
			} else if p.responseStatus >= 300 && p.responseStatus < 400 {
				kind = "redirect_or_invalid_status"
			}
			observeModelAttemptHealth(attemptCtx, "", false, kind, time.Now())
			idle()
			release()
		})
	}
	return attemptCtx, finish, nil
}

func modelAccountOutcomeKind(status int, code string, complete bool) string {
	switch {
	case complete && status >= 200 && status < 300 && code == "":
		return "success"
	case status == 405 && code == "3012":
		return "risk_control"
	case code == "3007" || code == "F001":
		return "captcha_required"
	case status == 401 || status == 403:
		return "authentication_failed"
	case status == 402 || code == "1005":
		return "quota_exhausted"
	case status == 429 || code == "3008" || code == "3009" || code == "3010":
		return "rate_limited"
	case status >= 500 || code != "":
		return "upstream_error"
	default:
		return "invalid_response"
	}
}

func observeModelAttemptHealth(ctx context.Context, code string, complete bool, kind string, now time.Time) {
	p, _ := ctx.Value(proxyObservationKey{}).(*proxyAttemptObservation)
	if p == nil {
		return
	}
	p.healthOnce.Do(func() {
		o := p.value
		o.Status, o.BusinessCode, o.Complete, o.At = p.responseStatus, code, complete, now
		if ctx.Err() != nil {
			kind, o.Complete = "canceled", false
		}
		if kind == "invalid_target" || kind == "dispatch_changed" {
			return
		}
		p.store.ObserveModelAccountOutcome(p.account, o, kind)
		// 软失败统计与账号结果分开：它只用来判断「这条线路还能不能用」，
		// 既不参与账号状态，也不构成删除证据（删除仍只由网络复核决定）。
		if signal, ok := softOutcomeSignal(kind, o.Status); ok && !o.InvalidRequest {
			p.store.RecordProxySoftOutcome(p.egress, signal, o.Status, kind == "success", now)
		}
		// 线路 405 画像的成功面：一次完整成功即清零连续 405 并解除隔离。
		// 只记模型请求的完整成功，不把"响应头 200"当成功（口径与软失败一致）。
		if kind == "success" {
			p.store.RecordLineOK(p.egress, now)
		}
		// 慢样本：成功后仍记录首字节时长，用于后续「同负载优先快线」的排序输入。
		// 它**不计入失败率**——30–90 秒出首字节但成功的线路是可用的，记成失败会把
		// 唯一还活着的线路隔离掉。
		if kind == "success" && !p.beganAt.IsZero() && !p.firstByteAt.IsZero() {
			p.store.RecordProxySlowSample(p.egress, p.firstByteAt.Sub(p.beganAt), now)
		}
	})
}

// ObserveModelAttemptTimeout 记录「等不到首字节」。它与客户端取消、普通连接失败
// 都不同：账号未必有问题，但这条线路在预算内交不出首字节，必须记一次软失败。
func ObserveModelAttemptTimeout(ctx context.Context, now time.Time) {
	observeModelAttemptHealth(ctx, "", false, "timeout", now)
}

// ObserveModelAttemptUplinkSlow 记录「上行传不完」：按实测速率外推，请求体在本次
// 尝试预算内无法写完。与首字节超时分开统计——它只能是线路的上行带宽问题。
func ObserveModelAttemptUplinkSlow(ctx context.Context, now time.Time) {
	observeModelAttemptHealth(ctx, "", false, "uplink_slow", now)
}

// softOutcomeSignal 只把与线路相关的结果交给软失败统计：成功计入分母，
// 超时/连接重置/408/504/524 计入分子。405/402/429 等业务语义不参与，
// 否则失败率会被与线路无关的上游语义稀释。
func softOutcomeSignal(kind string, status int) (string, bool) {
	switch kind {
	case "success":
		return "", true
	case "timeout":
		return store.ProxySignalTransportTimeout, true
	case "connection_failed":
		return store.ProxySignalTransportReset, true
	case "uplink_slow":
		return store.ProxySignalUplinkSlow, true
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout, 524:
		return fmt.Sprintf("status_%d", status), true
	}
	return "", false
}

// ObserveModelAttemptError 区分真实连接失败与未派发；严格退役仍不接收这些错误。
func ObserveModelAttemptError(ctx context.Context, err error, now time.Time) {
	kind := "connection_failed"
	if errors.Is(err, store.ErrProxyRetired) || errors.Is(err, store.ErrInvalidProxyRoute) || errors.Is(err, store.ErrStrictProxyPaused) || errors.Is(err, store.ErrInitializationUnavailable) {
		kind = "dispatch_changed"
	} else if errors.Is(err, context.DeadlineExceeded) {
		kind = "timeout"
	}
	observeModelAttemptHealth(ctx, "", false, kind, now)
}

// ObserveModelAttemptJSONError 记录HTTP 200内的业务错误，不混入完整成功或退役证据。
func ObserveModelAttemptJSONError(ctx context.Context, code string, now time.Time) {
	if code != "" && code != "0" {
		observeModelAttemptHealth(ctx, code, false, modelAccountOutcomeKind(200, code, false), now)
	}
}

// ModelRequestObservation 共用真实模型请求的去敏校验；不获取许可，也不产生调度副作用。
func ModelRequestObservation(acc *model.Account, request *http.Request, body map[string]any, entry string) store.ProxyObservation {
	if acc == nil || request == nil || request.URL == nil {
		return store.ProxyObservation{InvalidRequest: true}
	}
	profile := map[string]string{}
	for _, key := range []string{"User-Agent", "X-Zcode-App-Version", "X-Title", "X-Zcode-Agent", "X-Release-Channel", "X-Client-Language", "X-Client-Timezone", "X-Platform", "X-Os-Category", "X-Os-Version", "HTTP-Referer", "Content-Type", "Accept", "Accept-Language", "Anthropic-Version", "Anthropic-Beta", "X-Zcode-Session-Type", "X-Zcode-Query-Source"} {
		profile[key] = request.Header.Get(key)
	}
	profileJSON, _ := json.Marshal(profile)
	metadata, _ := body["metadata"].(map[string]any)
	userID, _ := metadata["user_id"].(string)
	var identity map[string]any
	_ = json.Unmarshal([]byte(userID), &identity)
	device, _ := identity["device_id"].(string)
	session, _ := identity["session_id"].(string)
	key := request.Header.Get("X-Api-Key")
	value := store.ProxyObservation{
		RequestID: request.Header.Get("X-Request-Id"), EntryRef: entry,
		TargetOrigin: request.URL.Scheme + "://" + request.URL.Host,
		TargetPath:   request.URL.EscapedPath(), Method: request.Method,
		Model: model.NormalizeModelName(body["model"]), ContentRef: upstream.SemanticContentKey(body),
		ProfileRef: observationRef(string(profileJSON)), SessionRef: observationRef(session), DeviceRef: observationRef(device),
		InvalidRequest: !validObservationTarget(acc, request) || device == "" || session == "" || session != request.Header.Get("X-Session-Id") ||
			key == "" || key != acc.Secret() || request.Header.Get("Authorization") != "Bearer "+key ||
			len(request.Header.Values("Authorization")) != 1 || len(request.Header.Values("X-Api-Key")) != 1,
	}
	if value.TargetPath == "" {
		value.TargetPath = "/"
	}
	return value
}

func validObservationTarget(acc *model.Account, request *http.Request) bool {
	if acc.Provider != model.ProviderZai || request.Method != http.MethodPost || request.URL.User != nil {
		return false
	}
	target := config.UpstreamZaiFallback
	if acc.Mode == "jwt" {
		target = config.UpstreamZai
	}
	want, err := url.Parse(target)
	if err != nil || want.Host == "" || (want.Scheme != "http" && want.Scheme != "https") {
		return false
	}
	return strings.EqualFold(request.URL.Scheme, want.Scheme) && strings.EqualFold(request.URL.Host, want.Host) &&
		request.URL.EscapedPath() == want.EscapedPath() && request.URL.RawQuery == want.RawQuery
}

// ConfirmProxyAttemptResponse 排除重定向后的其它目标；这里只确认归属，HTTP 200 仍不算成功。
func ConfirmProxyAttemptResponse(ctx context.Context, response *http.Response) {
	p, _ := ctx.Value(proxyObservationKey{}).(*proxyAttemptObservation)
	if p == nil || response == nil || response.Request == nil || response.Request.URL == nil {
		return
	}
	p.responseVerified = response.Request.Method == p.value.Method && observationRef(response.Request.URL.String()) == p.targetRef
	p.responseStatus = response.StatusCode
	// 首字节到达时刻：只用于慢样本统计（>30s 的成功请求），不参与任何判定。
	if p.firstByteAt.IsZero() {
		p.firstByteAt = time.Now()
	}
}

// ProxyAttemptEgress 把本次真实路由交给断流/成功处理，不能重新读取账号后来修改的绑定。
func ProxyAttemptEgress(ctx context.Context) (store.ProxyEgress, bool) {
	p, ok := ctx.Value(proxyObservationKey{}).(*proxyAttemptObservation)
	if !ok {
		return store.ProxyEgress{}, false
	}
	return p.egress, true
}

// ObserveProxyAttempt 只收完整成功或已读完的3012，不读取/保留任何响应正文。
func ObserveProxyAttempt(ctx context.Context, status int, code string, complete bool, now time.Time) {
	p, _ := ctx.Value(proxyObservationKey{}).(*proxyAttemptObservation)
	if p == nil || !p.responseVerified || ctx.Err() != nil {
		return
	}
	// 交付层可能统一写 200；严格证据必须使用实际上游状态，不把 3xx 或异常响应算成功。
	status = p.responseStatus
	observeModelAttemptHealth(ctx, code, complete, modelAccountOutcomeKind(status, code, complete), now)
	if !((status == 405 && code == "3012") || (complete && status >= 200 && status < 300)) {
		return
	}
	p.once.Do(func() {
		value := p.value
		value.Status, value.BusinessCode, value.Complete, value.At = status, code, complete, now
		decision, err := p.store.ObserveProxyOutcome(p.account, p.egress, value)
		if err != nil {
			web.Warn("strict-proxy", "严格代理观察/事务失败，未报告删除成功")
			return
		}
		if decision.DecisionID != "" {
			web.Warn("strict-proxy", fmt.Sprintf("rule=%s decision=%s reason=%s removed=%d reassigned=%d direct_fallback=%d", decision.RuleVersion, decision.DecisionID, decision.Reason, len(decision.Removed), decision.Assigned, decision.Direct))
		}
	})
}
