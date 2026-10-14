package store

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/proxy"
)

type dispatchRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dispatchRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGuardProxyClientRevalidatesAtRoundTripAndReleasesBody(t *testing.T) {
	s := newTestStore(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "complete") }))
	defer server.Close()
	p, err := s.AddProxyProfile("本地合成代理", server.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	a := addSelectionAccount(t, s, "账号", p.ID)
	base, err := proxy.ClientFor(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	guarded := s.GuardProxyClient(base, a, s.ProxyEgress(a))
	if guarded == base || guarded.Transport == base.Transport {
		t.Fatal("代理客户端应复制而不是修改共享传输")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://model.example.test/api", nil)
	resp, err := guarded.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.strictDispatch) != 1 {
		t.Fatal("响应体关闭前需保持在途许可")
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if len(s.strictDispatch) != 0 {
		t.Fatal("EOF/重复 Close 应幂等释放许可")
	}
	if _, _, err := s.DeleteProxyProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := guarded.Do(req); !errors.Is(err, ErrProxyRetired) {
		t.Fatalf("旧客户端必须在发送前拒绝: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("删除后的旧客户端不应产生新出站")
	}
	direct := &http.Client{}
	if s.GuardProxyClient(direct, nil, ProxyEgress{Mode: "direct"}) != direct {
		t.Fatal("直连不包装为代理许可")
	}
}

func TestGuardProxyClientRoundTripFailureReleasesLease(t *testing.T) {
	s, _, _, a := strictComparisonSetup(t)
	client := &http.Client{Transport: dispatchRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("合成网络错误") })}
	guarded := s.GuardProxyClient(client, a, s.ProxyEgress(a))
	req, _ := http.NewRequest(http.MethodGet, "http://model.example.test", nil)
	if _, err := guarded.Do(req); err == nil {
		t.Fatal("需要返回网络错误")
	}
	if len(s.strictDispatch) != 0 {
		t.Fatal("发送失败必须立即释放")
	}
}

func TestLoginProfileDispatchRejectsDisabledChangedAndRemoved(t *testing.T) {
	for _, change := range []string{"disabled", "changed", "removed"} {
		t.Run(change, func(t *testing.T) {
			s, p, _, _ := strictComparisonSetup(t)
			release, err := s.AcquireProxyProfileDispatch(p)
			if err != nil {
				t.Fatal(err)
			}
			release()
			release()
			switch change {
			case "disabled":
				_, err = s.UpdateProxyProfile(p.ID, p.Name, p.URL, false)
			case "changed":
				_, err = s.UpdateProxyProfile(p.ID, p.Name, strings.Replace(p.URL, "18101", "18103", 1), true)
			case "removed":
				_, _, err = s.DeleteProxyProfile(p.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AcquireProxyProfileDispatch(p); !errors.Is(err, ErrProxyRetired) {
				t.Fatalf("旧登录快照应被拒绝: %v", err)
			}
		})
	}
}
