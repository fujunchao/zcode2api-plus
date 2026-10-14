package oauth

import (
	"context"
	"io"
	"net/http"
	"sync"
)

type dispatchGuardKey struct{}
type dispatchAcquire func() (func(), error)

// WithDispatchGuard 将当前路由快照的许可边界附到登录请求上下文。
// 不保存凭据/请求头，每次真实 RoundTrip（含重定向）重新准入；不得复用过期快照。
func WithDispatchGuard(ctx context.Context, acquire func() (func(), error)) context.Context {
	return context.WithValue(ctx, dispatchGuardKey{}, dispatchAcquire(acquire))
}

func guardClient(ctx context.Context, client *http.Client) *http.Client {
	acquire, _ := ctx.Value(dispatchGuardKey{}).(dispatchAcquire)
	if acquire == nil {
		return client
	}
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &dispatchTransport{base: base, acquire: acquire}
	return &copy
}

type dispatchTransport struct {
	base    http.RoundTripper
	acquire dispatchAcquire
}

func (t *dispatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	release, err := t.acquire()
	if err != nil {
		return nil, err
	}
	var once sync.Once
	done := func() { once.Do(release) }
	response, err := t.base.RoundTrip(req)
	if err != nil || response == nil || response.Body == nil {
		done()
		return response, err
	}
	response.Body = &dispatchBody{ReadCloser: response.Body, done: done}
	return response, nil
}

type dispatchBody struct {
	io.ReadCloser
	done func()
}

func (b *dispatchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.done()
	}
	return n, err
}

func (b *dispatchBody) Close() error {
	defer b.done()
	return b.ReadCloser.Close()
}
