package store

import (
	"io"
	"net/http"
	"sync"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

// AcquireProxyProfileDispatch 只供账号入池前的命名登录路由使用，不能用 nil 账号放行。
// 快照必须仍启用且 ID/URL 身份与当前配置一致；与删除共享同一线性化锁。
func (s *Store) AcquireProxyProfileDispatch(expected ProxyProfile) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expected.ID == "" || !expected.Enabled || s.strictProxyBlockedLocked(expected.URL) {
		return nil, ErrProxyRetired
	}
	for _, live := range s.listProxyProfilesLocked() {
		if live.ID == expected.ID && live.Enabled && live.Generation == expected.Generation && strictProxyIdentity(live.URL) == strictProxyIdentity(expected.URL) {
			return s.acquireStrictLeaseLocked(strictProxyIdentity(live.URL)), nil
		}
	}
	return nil, ErrProxyRetired
}

// AcquireRawProxyDispatch 仅供账号入池前的人工登录 URL，仍受全项目严格墓碑约束。
// 自动登录应使用带代次的命名快照，不能借这个入口跳过配置更新检查。
func (s *Store) AcquireRawProxyDispatch(rawURL string) (func(), error) {
	canonical, err := proxy.CanonicalRoute(rawURL)
	if err != nil {
		return nil, ErrInvalidProxyRoute
	}
	if canonical == "" {
		return func() {}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictProxyBlockedLocked(rawURL) {
		return nil, ErrProxyRetired
	}
	return s.acquireStrictLeaseLocked(strictProxyIdentity(rawURL)), nil
}

func (s *Store) proxyDispatchCurrentLocked(acc *model.Account, egress ProxyEgress) bool {
	if acc == nil || egress.Mode != "proxy" || egress.identity == "" || s.strictProxyBlockedLocked(derefStr(acc.ProxyURL)) ||
		!s.strictCredentialCurrentLocked(acc, egress) || !s.egressVersionCurrentLocked(acc, egress) {
		return false
	}
	live := s.findLocked(acc.Provider, acc.ID)
	return live != nil && derefStr(live.ProxyID) == derefStr(acc.ProxyID) &&
		strictProxyIdentity(derefStr(live.ProxyURL)) == egress.identity &&
		s.strictProxy.RouteVersions[acc.ID].Generation == egress.RouteGeneration
}

// BumpLineTruncateForAttempt/ResetLineTruncateForAttempt 保留独立断流判据，
// 只允许实际代理 HTTP 快照操作同一代线路，不以旧流的结果清理或复位新 URL。
func (s *Store) BumpLineTruncateForAttempt(acc *model.Account, egress ProxyEgress) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if egress.ID == "" || !s.proxyDispatchCurrentLocked(acc, egress) {
		return 0
	}
	s.lineTruncStrikes[egress.ID]++
	s.lineTruncTotals[egress.ID]++
	return s.lineTruncStrikes[egress.ID]
}

func (s *Store) ResetLineTruncateForAttempt(acc *model.Account, egress ProxyEgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if egress.ID != "" && s.proxyDispatchCurrentLocked(acc, egress) {
		delete(s.lineTruncStrikes, egress.ID)
	}
}

func (s *Store) acquireStrictLeaseLocked(identity string) func() {
	if s.strictDispatch == nil {
		s.strictDispatch = map[string]int{}
	}
	s.strictDispatch[identity]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.strictDispatch[identity]--
			if s.strictDispatch[identity] == 0 {
				delete(s.strictDispatch, identity)
			}
		})
	}
}

// GuardProxyClient 复制客户端而不修改共享 Transport，真正 RoundTrip 前才取得许可。
// 有账号的直连同样复核绑定快照，避免管理员改绑后仍从旧直连客户端出站。
// 错误立即释放，正常响应持有到响应体关闭或 EOF。
func (s *Store) GuardProxyClient(client *http.Client, acc *model.Account, egress ProxyEgress) *http.Client {
	if client == nil || egress.Mode == "custom_client" || (acc == nil && egress.Mode != "proxy") {
		return client
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &strictProxyTransport{store: s, account: acc.Clone(), egress: egress, base: base}
	return &copy
}

type strictProxyTransport struct {
	initialization *InitializationAttempt
	store          *Store
	account        *model.Account
	egress         ProxyEgress
	base           http.RoundTripper
}

func (t *strictProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var release func()
	var err error
	if t.initialization != nil {
		release, err = t.store.AcquireInitializationDispatch(t.initialization)
	} else {
		release, err = t.store.AcquireProxyDispatch(t.account, t.egress)
	}
	if err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		return resp, err
	}
	if resp == nil || resp.Body == nil {
		release()
		return resp, nil
	}
	resp.Body = &strictProxyBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

type strictProxyBody struct {
	io.ReadCloser
	release func()
}

func (b *strictProxyBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *strictProxyBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}
