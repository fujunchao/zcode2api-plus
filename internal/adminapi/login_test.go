// 登录时选定出口线路的解析、回显与落盘。
// 代理必须在建立会话时就定：登录、API Key 兑换、额度刷新、活动领取是同一条出站链路。
package adminapi

import (
	"net/http"
	"testing"
)

func TestResolveLoginProxy(t *testing.T) {
	st := newClaimStore(t)
	h := &Handler{Store: st}
	profile, err := st.AddProxyProfile("hk", "http://1.2.3.4:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	t.Run("未指定即直连", func(t *testing.T) {
		url, id, apiErr := h.resolveLoginProxy(map[string]any{})
		if url != "" || id != "" || apiErr != nil {
			t.Fatalf("got (%q, %q, %v)", url, id, apiErr)
		}
	})
	t.Run("线路 ID 解析为地址", func(t *testing.T) {
		url, id, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": profile.ID})
		if apiErr != nil {
			t.Fatalf("不应报错: %v", apiErr)
		}
		if url != "http://1.2.3.4:8080" || id != profile.ID {
			t.Fatalf("got (%q, %q)", url, id)
		}
	})
	t.Run("未知线路 400", func(t *testing.T) {
		_, _, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": "no-such-line"})
		if apiErr == nil || apiErr.status != http.StatusBadRequest {
			t.Fatalf("应 400: %v", apiErr)
		}
	})
	t.Run("直接给地址", func(t *testing.T) {
		url, id, apiErr := h.resolveLoginProxy(map[string]any{"proxy_url": "  socks5://u:p@1.2.3.4:1080  "})
		if apiErr != nil {
			t.Fatalf("不应报错: %v", apiErr)
		}
		if url != "socks5://u:p@1.2.3.4:1080" || id != "" {
			t.Fatalf("got (%q, %q)", url, id)
		}
	})
	t.Run("非法协议 400", func(t *testing.T) {
		_, _, apiErr := h.resolveLoginProxy(map[string]any{"proxy_url": "ftp://1.2.3.4"})
		if apiErr == nil || apiErr.status != http.StatusBadRequest {
			t.Fatalf("应 400: %v", apiErr)
		}
	})
}

func TestLoginStartAcceptsProxy(t *testing.T) {
	mux, st, _ := setup(t)
	profile, err := st.AddProxyProfile("hk", "http://user:pw@1.2.3.4:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/login/start",
		map[string]any{"proxy_id": profile.ID})
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	if str(t, body["authorize_url"]) == "" {
		t.Fatalf("应返回授权链接: %v", body)
	}
	// 回显必须脱敏：代理密码不能进响应体。
	if shown := str(t, body["proxy"]); shown != "http://user:***@1.2.3.4:8080" {
		t.Fatalf("应回显脱敏后的出口: %q", shown)
	}

	// 未知线路提前 400：不要把坏代理带进会话，等兑换时才炸。
	code, _ = do(t, mux, st, http.MethodPost, "/admin/api/login/start",
		map[string]any{"proxy_id": "no-such-line"})
	if code != http.StatusBadRequest {
		t.Fatalf("未知线路应 400: %d", code)
	}

	// 不选代理（空请求体）仍应可用。
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/login/start", map[string]any{})
	if code != http.StatusOK || str(t, body["proxy"]) != "" {
		t.Fatalf("不选代理应 200 且 proxy 为空: %d %v", code, body)
	}
}
