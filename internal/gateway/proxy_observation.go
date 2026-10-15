package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	store            *store.Store
	account          *model.Account
	egress           store.ProxyEgress
	value            store.ProxyObservation
	responseVerified bool
	responseStatus   int
	targetRef        string
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
	return context.WithValue(ctx, proxyObservationKey{}, &proxyAttemptObservation{store: st, account: acc, egress: egress, value: value, targetRef: observationRef(request.URL.String())}), release, nil
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
