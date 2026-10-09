package adminapi

import (
	"context"
	"fmt"

	"zcode2api/internal/oauth"
	"zcode2api/internal/proxy"
	"zcode2api/internal/web"
)

// refreshLoginProxy 由持有会话锁的完成请求调用。保持有效指派，只修复已删除/停用的线路；
// 上次耗尽后若补充了新代理，可以不重建会话直接继续使用原回调。
func (h *Handler) refreshLoginProxy(session *loginSession) {
	if !session.retryProxies || session.direct {
		return
	}
	if session.proxyID != "" {
		for _, profile := range h.Store.ListProxyProfiles() {
			if profile.ID == session.proxyID && profile.Enabled && !session.failedURLs[profile.URL] {
				session.proxyURL = profile.URL
				return
			}
		}
	}
	if next, ok := h.Store.PickAvailableProxyProfileExcluding(session.failedURLs); ok {
		session.proxyID, session.proxyURL = next.ID, next.URL
	} else {
		session.proxyID, session.proxyURL = "", ""
	}
}

// exchangeLoginWithFailover 在同一回调和原会话时限内尝试未失败出口。
// 只在明确代理传输故障后换线，普通 OAuth 错误和取消原样返回；直连最多尝试一次。
func (h *Handler) exchangeLoginWithFailover(ctx context.Context, session *loginSession, code, state string) (*oauth.ExchangeResult, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		h.refreshLoginProxy(session)
		result, err := session.flow.ExchangeCodeContext(ctx, code, state, session.proxyURL)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil || session.proxyID == "" || !proxy.IsEndpointUnreachable(ctx, err) {
			if ctx.Err() == nil && session.retryProxies && session.proxyID == "" && proxy.IsEndpointUnreachable(ctx, err) {
				return nil, fmt.Errorf("可用代理已尝试完毕，直连仍不可达；会话已保留，补充代理后可重新提交原回调：%w", err)
			}
			return nil, err
		}
		failedID, failedURL := session.proxyID, session.proxyURL
		replacement, replaceErr := h.Store.ReplaceFailedLoginProxy(failedID, failedURL, session.failedURLs)
		if replaceErr != nil {
			return nil, fmt.Errorf("故障代理删除失败，原绑定与登录会话已保留，请稍后重新提交回调：%w", replaceErr)
		}
		if session.failedURLs == nil {
			session.failedURLs = map[string]bool{}
		}
		session.failedURLs[failedURL] = true
		session.retryProxies = true
		session.proxyID, session.proxyURL = "", ""
		nextLabel := "直连"
		if replacement.Next != nil {
			session.proxyID, session.proxyURL = replacement.Next.ID, replacement.Next.URL
			nextLabel = replacement.Next.ID
		}
		session.proxySwitches++
		web.Warn("login-proxy", fmt.Sprintf("登录代理 %s 不可达，删除=%t；改派 %d 个已有账号、%d 个退直连；改用 %s 继续原回调（第 %d 次换线）",
			failedID, replacement.Removed, len(replacement.Reassign.Assigned), len(replacement.Reassign.Direct), nextLabel, session.proxySwitches))
	}
}
