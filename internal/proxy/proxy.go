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
		return nil, fmt.Errorf("代理 URL 无效: %v", err)
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
		return nil, fmt.Errorf("不支持的代理协议: %s，允许: %s", scheme, strings.Join(AllowedSchemes, ", "))
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("代理 URL 缺少主机地址")
	}
	return &trimmed, nil
}
