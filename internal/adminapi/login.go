// OAuth 登录端点（/admin/api/login/*）：对应 Python 版 admin_api.py 的
// login_start / login_complete 与 _save_oauth_account 收尾链。
package adminapi

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/oauth"
	"zcode2api/internal/proxy"
	"zcode2api/internal/web"
)

// loginFlowTTL 登录会话有效期（对齐 Python _LOGIN_TTL_SECONDS = 600）。
const loginFlowTTL = 600 * time.Second

// loginSession 一次登录会话：OAuth flow + 本次登录选定的出口线路。
//
// 代理必须在**建立会话时**定下来：token 交换、API Key 兑换、随后的额度刷新与
// 自动领取属于同一条出站链路，中途换出口没有意义。只在登录本身走代理、后续直连，
// 等于拿真实 IP 去打上游——那正是需要代理的人最不想要的。
type loginSession struct {
	flow     *oauth.Flow
	proxyURL string // 已解析的代理地址（空 = 直连）
	proxyID  string // 代理线路 ID（用于写入账号；空表示非线路）
}

var (
	loginFlowsMu sync.Mutex
	loginFlows   = map[string]*loginSession{}
)

// cleanupLoginFlows 清扫过期会话。
func cleanupLoginFlows() {
	loginFlowsMu.Lock()
	defer loginFlowsMu.Unlock()
	cutoff := time.Now().Add(-loginFlowTTL)
	for id, session := range loginFlows {
		if session.flow.CreatedAt.Before(cutoff) {
			delete(loginFlows, id)
		}
	}
}

// getLoginFlow 取会话；并发安全（过期判定交给 cleanupLoginFlows）。
func getLoginFlow(flowID string) *loginSession {
	loginFlowsMu.Lock()
	defer loginFlowsMu.Unlock()
	return loginFlows[flowID]
}

// errUpstream 对齐 Python HTTPException(502)（上游初始化/兑换失败）。
func errUpstream(msg string) *apiError { return &apiError{http.StatusBadGateway, msg} }

// resolveLoginProxy 解析登录时选定的出口线路，返回 (代理地址, 线路 ID)。
// 支持线路 ID（proxy_id）与直接给出的地址（proxy_url，与编辑账号同口径）；
// 两者都为空即直连。
//
// 线路不存在或地址非法一律提前 400——不要把坏代理带进会话，等兑换时才炸。
func (h *Handler) resolveLoginProxy(payload map[string]any) (string, string, *apiError) {
	profileID := strings.TrimSpace(strOf(firstTruthy(payload["proxy_id"])))
	if profileID != "" {
		for _, p := range h.Store.ListProxyProfiles() {
			if p.ID == profileID {
				return p.URL, p.ID, nil
			}
		}
		return "", "", errBadRequest("代理線路不存在")
	}
	normalized, err := proxy.NormalizeProxyURL(strOf(firstTruthy(payload["proxy_url"])))
	if err != nil {
		return "", "", errBadRequest(err.Error())
	}
	if normalized == nil {
		return "", "", nil
	}
	return *normalized, "", nil
}

// handleLoginStart POST /admin/api/login/start（body 可选 proxy_id / proxy_url）
// 返回授权链接；本次登录的所有出站请求都会走选定线路。
func (h *Handler) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	cleanupLoginFlows()
	// 请求体可省略（不选代理时前端不发 body）。
	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		payload = map[string]any{}
	}
	proxyURL, proxyID, proxyErr := h.resolveLoginProxy(payload)
	if proxyErr != nil {
		writeAPIError(w, proxyErr)
		return
	}
	flow := oauth.NewFlow()
	flowID, authorizeURL, err := flow.Init()
	if err != nil {
		writeAPIError(w, errUpstream(fmt.Sprintf("登录初始化失败: %v", err)))
		return
	}
	loginFlowsMu.Lock()
	loginFlows[flowID] = &loginSession{flow: flow, proxyURL: proxyURL, proxyID: proxyID}
	loginFlowsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"flow_id":       flowID,
		"authorize_url": authorizeURL,
		// 回显脱敏后的出口（空串=直连），前端据此提示本次登录经哪条线路。
		"proxy": proxy.MaskURL(proxyURL),
	})
}

func (h *Handler) handleLoginComplete(w http.ResponseWriter, r *http.Request) {
	cleanupLoginFlows()
	flowID := r.PathValue("flow_id")
	session := getLoginFlow(flowID)
	if session == nil {
		writeAPIError(w, errNotFound("登录会话不存在或已过期"))
		return
	}

	payload, apiErr := decodeBody(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	callbackURL := strings.TrimSpace(strOf(payload["callback_url"]))
	code, state, oauthErr, err := oauth.ParseCallbackURL(callbackURL)
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	if err == nil && oauthErr != "" {
		writeAPIError(w, errBadRequest(fmt.Sprintf("Z.AI 拒绝授权: %s", oauthErr)))
		return
	}
	if !session.flow.MatchesState(state) {
		writeAPIError(w, errBadRequest("回调地址与当前登录会话不匹配"))
		return
	}

	result, err := session.flow.ExchangeCode(code, state, session.proxyURL)
	if err != nil {
		writeAPIError(w, errBadRequest(err.Error()))
		return
	}
	account, apiErr := h.saveOAuthAccount(result, session)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	loginFlowsMu.Lock()
	delete(loginFlows, flowID)
	loginFlowsMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "account": account.PublicView(time.Now())})
}

// saveOAuthAccount 落库登录凭证：JWT 入池（邮箱命名）→ 写入选定线路 →
// 兑换 API Key 回填同账号 → 刷新额度 + 自动领取。
// 兑换/刷新失败不影响 JWT 已入池（对齐 Python _save_oauth_account）。
func (h *Handler) saveOAuthAccount(result *oauth.ExchangeResult, session *loginSession) (*model.Account, *apiError) {
	email := ""
	if result.Email != nil {
		email = strings.TrimSpace(*result.Email)
	}
	name := email
	if name == "" {
		name = "oauth-login"
	}
	// 邮箱必须在入池时就传入：每次登录的 token 都不同，只比凭据字节会把同一个号
	// 建成两条记录（见 store.AddAccountWithIdentity 的三级判重）。
	account, err := h.Store.AddAccountWithIdentity(model.ProviderZai, name, result.Token, email)
	if err != nil {
		return nil, errUpstream(fmt.Sprintf("凭证入池失败: %v", err))
	}
	if email != "" {
		dirty := false
		if account.Email == nil || *account.Email == "" {
			// 命中的既有账号可能早于本次改造入库，尚无邮箱记录。
			account.Email = &email
			dirty = true
		}
		if account.Name == "oauth-login" {
			account.Name = email
			dirty = true
		}
		if dirty {
			_ = h.Store.UpdateAccount(account)
		}
	}
	// 线路必须在兑换与刷新之前落到账号上：这三步都要出站。
	if apiErr := h.applyLoginProxy(account, session); apiErr != nil {
		return nil, apiErr
	}
	if result.AccessToken != "" {
		if apiKey, err := oauth.ExchangeAPIKey(result.AccessToken, session.proxyURL); err == nil && apiKey != "" {
			account.APIKey = &apiKey
			_ = h.Store.UpdateAccount(account)
		} else if err != nil {
			web.Warn("adminapi", fmt.Sprintf("兑换 API Key 失败: %v", err))
		}
	}
	if account.Mode == "jwt" {
		h.Quota.RefreshAccounts([]*model.Account{account})
		// 授权完成即激活 + 自动领取（入池即吃满活动；对齐 Python _save_oauth_account）
		h.scheduleAutoClaim(account)
	}
	return account, nil
}

// applyLoginProxy 把登录时选定的线路写到账号上；未指定时保持账号原有指派不动
// （重新登录不该把已有线路清掉）。
func (h *Handler) applyLoginProxy(account *model.Account, session *loginSession) *apiError {
	if session == nil || (session.proxyID == "" && session.proxyURL == "") {
		return nil
	}
	if session.proxyID != "" {
		ok, err := h.Store.AssignProxyProfile(account.ID, session.proxyID)
		if err != nil {
			return errBadRequest(err.Error())
		}
		if !ok {
			return errNotFound("账号不存在")
		}
		return nil
	}
	// 直接给地址：与编辑账号同语义，解除线路指派。
	url := session.proxyURL
	account.ProxyID = nil
	account.ProxyURL = &url
	if err := h.Store.UpdateAccount(account); err != nil {
		return errUpstream(fmt.Sprintf("写入代理失败: %v", err))
	}
	return nil
}
