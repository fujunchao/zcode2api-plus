package proxy

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalRoute 只按 Transport 实际使用的字段确定路由身份。
// 返回值含认证信息，只供内部缓存/摘要，不得直接写日志或返回诊断接口。
// URL 的路径、查询和片段不会参与 HTTP/SOCKS 代理握手，不能借它们绕过退役。
func CanonicalRoute(raw string) (string, error) {
	normalized, err := NormalizeProxyURL(raw)
	if err != nil || normalized == nil {
		return "", err
	}
	u, err := url.Parse(*normalized)
	if err != nil {
		return "", err
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", fmt.Errorf("代理 URL 缺少主机地址")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.Unmap().String()
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			port = "1080"
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("代理端口无效")
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(number))
	u.Path, u.RawPath, u.RawQuery, u.Fragment, u.RawFragment = "", "", "", "", ""
	u.ForceQuery = false
	if u.User != nil {
		user := u.User.Username()
		password, _ := u.User.Password()
		switch u.Scheme {
		case "socks4":
			if user == "" {
				u.User = nil
			} else {
				u.User = url.User(user)
			}
		case "socks5", "socks5h":
			if user == "" {
				u.User = nil
			} else {
				u.User = url.UserPassword(user, password)
			}
		default:
			u.User = url.UserPassword(user, password)
		}
	}
	return u.String(), nil
}
