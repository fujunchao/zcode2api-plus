// Package proxy 提供账号级出站代理的 URL 校验。
// 对应 Python 版 app/proxy.py（httpx 客户端构造部分在 M6 随网关落地）。
package proxy

import (
	"fmt"
	"net/url"
	"strings"
)

// AllowedSchemes 允许的代理协议（与 Python 版 ALLOWED_SCHEMES 一致）。
var AllowedSchemes = []string{"http", "https", "socks4", "socks5", "socks5h"}

// NormalizeProxyURL 空白视为未配置（返回 nil 对应 Python None）；
// 非法 scheme 或缺少主机则返回错误。
func NormalizeProxyURL(raw string) (*string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// url.Error 包含原始 URL，即使地址非法仍可能带用户名/密码。
		return nil, fmt.Errorf("代理 URL 格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	allowed := false
	for _, s := range AllowedSchemes {
		if scheme == s {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("不支持的代理协议，允许: %s", strings.Join(AllowedSchemes, ", "))
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("代理 URL 缺少主机地址")
	}
	return &trimmed, nil
}

// MaskURL 隐藏代理 URL 里的密码，供错误文案与日志使用。
// 代理凭据不该出现在日志里；解析失败时也可能含密码，必须返回固定占位而不是原文。
//
// 手工拼装而不是改写 u.User 再 u.String()：url.UserPassword 会把 * 转义成 %2A，
// 展示出来是 http://user:%2A%2A%2A@host 这种看不出意图的东西。
func MaskURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "invalid-proxy"
	}
	if parsed.User == nil {
		return raw
	}
	if _, hasPassword := parsed.User.Password(); !hasPassword {
		return parsed.String()
	}
	masked := parsed.Scheme + "://" + parsed.User.Username() + ":***@" + parsed.Host
	if parsed.Path != "" {
		masked += parsed.Path
	}
	if parsed.RawQuery != "" {
		masked += "?" + parsed.RawQuery
	}
	if parsed.Fragment != "" {
		masked += "#" + parsed.Fragment
	}
	return masked
}
