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
		url, id, auto, apiErr := h.resolveLoginProxy(map[string]any{})
		if url != "" || id != "" || auto || apiErr != nil {
			t.Fatalf("got (%q, %q, %v, %v)", url, id, auto, apiErr)
		}
	})
	t.Run("线路 ID 解析为地址", func(t *testing.T) {
		url, id, auto, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": profile.ID})
		if apiErr != nil {
			t.Fatalf("不应报错: %v", apiErr)
		}
		if auto {
			t.Fatal("显式指定线路不应标记为自动")
		}
		if url != "http://1.2.3.4:8080" || id != profile.ID {
			t.Fatalf("got (%q, %q)", url, id)
		}
	})
	t.Run("未知线路 400", func(t *testing.T) {
		_, _, _, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": "no-such-line"})
		if apiErr == nil || apiErr.status != http.StatusBadRequest {
			t.Fatalf("应 400: %v", apiErr)
		}
	})
	t.Run("直接给地址", func(t *testing.T) {
		url, id, auto, apiErr := h.resolveLoginProxy(map[string]any{"proxy_url": "  socks5://u:p@1.2.3.4:1080  "})
		if apiErr != nil {
			t.Fatalf("不应报错: %v", apiErr)
		}
		if auto {
			t.Fatal("直接给地址不应标记为自动")
		}
		if url != "socks5://u:p@1.2.3.4:1080" || id != "" {
			t.Fatalf("got (%q, %q)", url, id)
		}
	})
	t.Run("非法协议 400", func(t *testing.T) {
		_, _, _, apiErr := h.resolveLoginProxy(map[string]any{"proxy_url": "ftp://1.2.3.4"})
		if apiErr == nil || apiErr.status != http.StatusBadRequest {
			t.Fatalf("应 400: %v", apiErr)
		}
	})
	// 「自動」要在会话建立时就定下出口，否则整条登录链路的出口不一致。
	t.Run("自动挑中空闲线路", func(t *testing.T) {
		url, id, auto, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": proxyIDAuto})
		if apiErr != nil {
			t.Fatalf("不应报错: %v", apiErr)
		}
		if !auto {
			t.Fatal("应标记为自动挑出")
		}
		if url != "http://1.2.3.4:8080" || id != profile.ID {
			t.Fatalf("应挑中唯一空闲线路，got (%q, %q)", url, id)
		}
	})
	t.Run("显式直连不分配", func(t *testing.T) {
		url, id, auto, apiErr := h.resolveLoginProxy(map[string]any{"proxy_id": proxyIDDirect})
		if apiErr != nil || url != "" || id != "" || auto {
			t.Fatalf("got (%q, %q, %v, %v)", url, id, auto, apiErr)
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
