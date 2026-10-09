// OAuth 登录端点（/admin/api/login/*）：对应 Python 版 admin_api.py 的
// login_start / login_complete 与 _save_oauth_account 收尾链。
package adminapi

import (
	"context"
	"crypto/sha256"
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
// 建立会话时预选出口；确认不可达才删除并换线。新账号以最终可用的登录出口入池，
// 不能只在授权时使用代理，保存账号后又丢失绑定。
type loginSession struct {
	mu       sync.Mutex // 完成请求持有；重复/并发提交不能同时消费一次性授权码。
	flow     *oauth.Flow
	proxyURL string // 已解析的代理地址（空 = 直连）
	proxyID  string // 代理线路 ID（用于写入账号；空表示非线路）
	// auto 表示这条线路是「自動」挑出来的、而非用户显式选定：命中既有账号时
	// 保留它原本的指派，避免重登把老号的线路换掉。
	auto             bool
	direct           bool // 明确选择直连，与未指定线路区分；重新登录也须清除旧指派
	retryProxies     bool
	failedURLs       map[string]bool
	proxySwitches    int
	callbackDigest   [32]byte
	exchanged        *oauth.ExchangeResult // 只保留到账号保存成功，避免保存失败后重复兑换。
	accountID        string
	createdAccountID string // 同一会话先建号后保存失败时，重试仍执行新号初始化。
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
func (h *Handler) resolveLoginProxy(payload map[string]any) (string, string, bool, *apiError) {
	raw := strings.TrimSpace(strOf(firstTruthy(payload["proxy_id"])))
	// 「自動」：按空闲优先、最少绑定挑选线路并固定出口，保证「token 交換 → 兌換 → 刷新 →
	// 領取」整条链路走同一个出口。没有启用线路就直连——不报错，与新增账号同口径。
	if raw == proxyIDAuto {
		if p, ok := h.Store.PickAvailableProxyProfile(); ok {
			return p.URL, p.ID, true, nil
		}
		return "", "", true, nil
	}
	if raw == proxyIDDirect {
		return "", "", false, nil
	}
	if raw != "" {
		for _, p := range h.Store.ListProxyProfiles() {
			if p.ID == raw {
				return p.URL, p.ID, false, nil
			}
		}
		return "", "", false, errBadRequest("代理線路不存在")
	}
	normalized, err := proxy.NormalizeProxyURL(strOf(firstTruthy(payload["proxy_url"])))
	if err != nil {
		return "", "", false, errBadRequest(err.Error())
	}
	if normalized == nil {
		return "", "", false, nil
	}
	return *normalized, "", false, nil
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
	proxyURL, proxyID, proxyAuto, proxyErr := h.resolveLoginProxy(payload)
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
	loginFlows[flowID] = &loginSession{flow: flow, proxyURL: proxyURL, proxyID: proxyID, auto: proxyAuto,
		direct:       strings.TrimSpace(strOf(payload["proxy_id"])) == proxyIDDirect,
		retryProxies: proxyAuto || proxyID != ""}
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
	if !session.mu.TryLock() {
		writeAPIError(w, &apiError{http.StatusConflict, "当前回调正在处理或自动换线，请等待本次登录完成"})
		return
	}
	defer session.mu.Unlock()

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
	digest := sha256.Sum256([]byte(code))
	if (session.exchanged != nil || session.accountID != "") && session.callbackDigest != digest {
		writeAPIError(w, errBadRequest("当前会话已处理另一份回调，请使用原回调或重新开始授权"))
		return
	}
	session.callbackDigest = digest
	if session.accountID != "" {
		h.writeCompletedLogin(w, session)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), session.flow.CreatedAt.Add(loginFlowTTL))
	stop := context.AfterFunc(h.backgroundContext(), cancel)
	defer func() { stop(); cancel() }()
	if session.exchanged == nil {
		result, err := h.exchangeLoginWithFailover(ctx, session, code, state)
		if err != nil {
			if ctx.Err() != nil || proxy.IsEndpointUnreachable(ctx, err) {
				writeAPIError(w, errUpstream(err.Error()))
			} else {
				writeAPIError(w, errBadRequest(err.Error()))
			}
			return
		}
		session.exchanged = result
	}
	if ctx.Err() != nil {
		writeAPIError(w, errUpstream("登录请求已取消；尚在有效期内时可重新提交原回调"))
		return
	}
	// 保存失败后可直接复用已兑换凭据；命名线路若已删除则改派，但不重新改变已成功的直连出口。
	if session.proxyID != "" {
		h.refreshLoginProxy(session)
	}
	account, apiErr := h.saveOAuthAccount(session.exchanged, session)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	session.accountID, session.exchanged = account.ID, nil
	h.writeCompletedLogin(w, session)
}

// 成功会话只保留账号 ID/回调摘要到原 TTL，重复提交不再次兑换、刷新或领取。
func (h *Handler) writeCompletedLogin(w http.ResponseWriter, session *loginSession) {
	account := h.Store.FindAny(session.accountID)
	if account == nil {
		writeAPIError(w, errNotFound("登录对应账号已被删除，请重新开始授权"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "account": account.PublicView(time.Now()),
		"proxy": proxy.MaskURL(session.proxyURL), "proxy_retries": session.proxySwitches})
}

// saveOAuthAccount 落库登录凭证：JWT 入池（邮箱命名）→ 写入选定线路 →
// 兑换 API Key 回填同账号 → 首次额度查询 + 自动领取。真正新号领取后再补查一次。
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
	account, isNew, err := h.Store.SaveOAuthAccount(name, result.Token, email)
	if err != nil {
		return nil, errUpstream(fmt.Sprintf("凭证入池失败: %v", err))
	}
	if session != nil {
		if isNew {
			session.createdAccountID = account.ID
		}
		isNew = isNew || session.createdAccountID == account.ID
	}

	// 线路必须在兑换与刷新之前落到账号上：这三步都要出站。
	// 「自動」挑出的线路命中既有账号时不覆盖原指派——重登不该换掉老号的线路；
	// 此时 account.ProxyURL 保持老号原值，后续出站仍走它。
	if !(session != nil && session.auto && !isNew) {
		if apiErr := h.applyLoginProxy(account, session); apiErr != nil {
			return nil, apiErr
		}
	}
	// 无会话的内部调用保留旧默认；真实登录会话不得在兑换后重新自动换出口。
	if isNew && account.ProxyURL == nil && session == nil {
		_, _ = h.Store.AutoAssignProxies([]string{account.ID})
	}
	// 入口已锁定出口，不能在授权完成后重新自动挑线。后续操作读取最新快照。
	account = h.Store.FindAny(account.ID)
	if account == nil {
		return nil, errNotFound("账号不存在")
	}
	// 出站地址一律以账号上落定的最新值为准。
	accountProxy := ""
	if account.ProxyURL != nil {
		accountProxy = *account.ProxyURL
	}
	if result.AccessToken != "" {
		if apiKey, err := oauth.ExchangeAPIKey(result.AccessToken, accountProxy); err == nil && apiKey != "" {
			_, _ = h.Store.SetAPIKey(account.Provider, account.ID, apiKey)
		} else if err != nil {
			web.Warn("adminapi", fmt.Sprintf("兑换 API Key 失败: %v", err))
		}
	}
	if account.Mode == "jwt" {
		if isNew {
			_, observations := h.Quota.RefreshAccountsObserved([]*model.Account{account})
			h.scheduleNewAccountClaim(account, observations[account.ID])
		} else {
			// 重登只沿用原刷新与领取行为，绝不把存量空额度当作新号代理问题。
			h.Quota.RefreshAccounts([]*model.Account{account})
			h.scheduleAutoClaim(account)
		}
	}
	if session != nil {
		session.createdAccountID = ""
	}
	return account, nil
}

// applyLoginProxy 把登录时选定的线路写到账号上；未指定时保持账号原有指派不动
// （重新登录不该把已有线路清掉）。
func (h *Handler) applyLoginProxy(account *model.Account, session *loginSession) *apiError {
	if session != nil && session.direct {
		if _, err := h.Store.AssignProxyProfile(account.ID, ""); err != nil {
			return errUpstream(fmt.Sprintf("清除代理失败: %v", err))
		}
		return nil
	}
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
	if _, err := h.Store.SetProxyURL(account.Provider, account.ID, &url); err != nil {
		return errUpstream(fmt.Sprintf("写入代理失败: %v", err))
	}
	return nil
}
