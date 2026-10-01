package store

import (
	"net/url"
	"strings"

	"zcode2api/internal/model"
)

// ProxyEgress 描述客户端选定的出站配置，不代表探测过公网 IP。
// Endpoint 仅保留协议/主机/端口，绝不携带认证、路径、查询参数或片段。
type ProxyEgress struct {
	ID       string
	Label    string
	Endpoint string
	Mode     string
}

func (s *Store) ProxyEgress(acc *model.Account) ProxyEgress {
	e := ProxyEgress{Label: "direct", Endpoint: "direct", Mode: "direct"}
	if acc == nil || derefStr(acc.ProxyURL) == "" {
		return e
	}
	e.ID, e.Label, e.Endpoint, e.Mode = derefStr(acc.ProxyID), "manual", "invalid", "proxy"
	if e.ID != "" {
		e.Label = s.ProxyLabel(acc)
	}
	if u, err := url.Parse(strings.TrimSpace(*acc.ProxyURL)); err == nil && u.Host != "" {
		e.Endpoint = strings.ToLower(u.Scheme) + "://" + u.Host
	}
	return e
}
