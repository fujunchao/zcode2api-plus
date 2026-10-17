package store

import (
	"net/url"
	"strings"

	"zcode2api/internal/model"
)

// ProxyEgress 描述客户端选定的出站配置，不代表探测过公网 IP。
// Endpoint 仅保留协议/主机/端口，绝不携带认证、路径、查询参数或片段。
type ProxyEgress struct {
	ID                   string
	Label                string
	Endpoint             string
	Mode                 string
	Reason               string
	Generation           uint64
	RouteGeneration      uint64
	identity             string
	credentialRef        string
	credentialGeneration uint64
}

// Identity 返回出站线路的规范化身份（不含认证）；可用于按线路去重或回避。
func (e ProxyEgress) Identity() string { return e.identity }

// LineKey 返回「线路级」聚合去重键：代理线路用 proxy:+identity，直连统一记 direct。
// 空串表示不参与聚合（无效路由 / 自定义客户端）。
//
// 与 boundLineIdentityLocked 的口径刻意分开：前者用于请求级风控判定（需要把直连
// 也当成一条线），后者用于匹配 line405 画像（只认真实代理线路）。
func (e ProxyEgress) LineKey() string {
	if e.Mode == "proxy" && e.identity != "" {
		return "proxy:" + e.identity
	}
	switch e.Mode {
	case "direct", "direct_fallback", "forced_direct":
		return "direct"
	}
	return ""
}

func (s *Store) ProxyEgress(acc *model.Account) ProxyEgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyEgressLocked(acc)
}

func (s *Store) proxyEgressLocked(acc *model.Account) ProxyEgress {
	e := ProxyEgress{Label: "direct", Endpoint: "direct", Mode: "direct"}
	if acc != nil {
		e.credentialRef = s.strictCredentialRefLocked(acc)
		e.credentialGeneration = s.strictProxy.Credentials[acc.ID].Generation
		e.RouteGeneration = s.strictProxy.RouteVersions[acc.ID].Generation
	}
	if acc == nil || derefStr(acc.ProxyURL) == "" {
		if acc != nil {
			if s.strictProxy.Intents[acc.ID] == "auto_pending_proxy" {
				e.Mode = "direct_fallback"
				e.Reason = "awaiting_automatic_proxy"
			} else if s.strictProxy.Intents[acc.ID] == "explicit_direct" {
				e.Reason = "explicit_direct"
			}
		}
		return e
	}
	e.ID, e.Label, e.Endpoint, e.Mode = derefStr(acc.ProxyID), "manual", "invalid", "proxy"
	if e.ID != "" {
		e.Label = "proxy-id:" + e.ID
		for _, p := range s.listProxyProfilesLocked() {
			if p.ID == e.ID {
				e.Label = p.Name
				break
			}
		}
	}
	if u, err := url.Parse(strings.TrimSpace(*acc.ProxyURL)); err == nil && u.Host != "" {
		e.Endpoint = strings.ToLower(u.Scheme) + "://" + u.Host
	}
	e.identity = strictProxyIdentity(derefStr(acc.ProxyURL))
	e.Generation = s.strictProxy.Generations[e.ID]
	return e
}

// DirectEgress 用于显式强制直连或自动回退，仍冻结凭据代次以安全接收成功对照。
// 不改账号绑定；direct_fallback 是否有资格替代选定 URL 在派发锁内再校验。
func (s *Store) DirectEgress(acc *model.Account, mode string) ProxyEgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := ProxyEgress{Label: "direct", Endpoint: "direct", Mode: mode}
	if acc != nil {
		e.credentialRef = s.strictCredentialRefLocked(acc)
		e.credentialGeneration = s.strictProxy.Credentials[acc.ID].Generation
		e.RouteGeneration = s.strictProxy.RouteVersions[acc.ID].Generation
	}
	switch mode {
	case "forced_direct":
		e.Reason = "explicit_force_direct"
	case "direct_fallback":
		e.Reason = "awaiting_automatic_proxy"
	case "direct":
		e.Reason = "direct_binding"
	default:
		e.Mode = "invalid_route"
	}
	return e
}

func (e ProxyEgress) ConfigurationGeneration() uint64 { return e.Generation }
func (e ProxyEgress) BindingGeneration() uint64       { return e.RouteGeneration }
func (e ProxyEgress) CredentialGeneration() uint64    { return e.credentialGeneration }
