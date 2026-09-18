// 新增账号时的代理自动分配，以及代理线路的一键批量检测。
//
// 批量检测的用例用本地 httptest 冒充两种角色：既当「z.ai 侧入口」（被覆写的
// upstreamProbeTargets 指向它），又当「代理」（Go 对 http 目标会发绝对 URI 请求，
// 普通 handler 照常应答），因此整条链路不触网。
package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"zcode2api/internal/model"
)

func TestAddAccountsAutoAssignsProxy(t *testing.T) {
	mux, st, _ := setup(t)
	line, err := st.AddProxyProfile("line-1", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	t.Run("缺省即自动分配空閒线路", func(t *testing.T) {
		code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts",
			map[string]any{"tokens": []any{jwtTokenFor("u-auto-1")}})
		if code != http.StatusOK {
			t.Fatalf("应 200: %d %v", code, body)
		}
		if n, _ := body["assigned"].(float64); n != 1 {
			t.Fatalf("应分配 1 条线路: %v", body)
		}
		ids, _ := body["ids"].([]any)
		if len(ids) != 1 {
			t.Fatalf("应返回 1 个账号 ID: %v", body)
		}
		acc := st.Find(model.ProviderZai, ids[0].(string))
		if acc == nil || acc.ProxyID == nil || *acc.ProxyID != line.ID {
			t.Fatalf("账号应落到唯一的空閒线路: %+v", acc)
		}
		if acc.ProxyURL == nil || *acc.ProxyURL != line.URL {
			t.Fatalf("出站地址应同步写入: %+v", acc.ProxyURL)
		}
	})

	t.Run("显式直连不分配", func(t *testing.T) {
		code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts",
			map[string]any{"tokens": []any{jwtTokenFor("u-direct")}, "proxy_id": proxyIDDirect})
		if code != http.StatusOK {
			t.Fatalf("应 200: %d %v", code, body)
		}
		if n, _ := body["assigned"].(float64); n != 0 {
			t.Fatalf("显式直连不应分配: %v", body)
		}
		ids, _ := body["ids"].([]any)
		acc := st.Find(model.ProviderZai, ids[0].(string))
		if acc == nil || acc.ProxyID != nil || acc.ProxyURL != nil {
			t.Fatalf("显式直连的账号不应带代理: %+v", acc)
		}
	})

	t.Run("线路用尽时回退直连并计数", func(t *testing.T) {
		// line-1 已被上面的子测试占用，这两个账号都只能回退。
		code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts",
			map[string]any{"tokens": []any{jwtTokenFor("u-fb-1"), jwtTokenFor("u-fb-2")}})
		if code != http.StatusOK {
			t.Fatalf("应 200: %d %v", code, body)
		}
		if n, _ := body["assigned"].(float64); n != 0 {
			t.Fatalf("已无空閒线路，不应分配: %v", body)
		}
		if n, _ := body["direct_fallback"].(float64); n != 2 {
			t.Fatalf("应回报 2 个回退直连: %v", body)
		}
	})
}

// probeStub 启动一个既当代理、又当 z.ai 侧入口的本地服务器（测试结束时自动
// 还原包级配置）。覆写 upstreamProbeTargets 是必需的——漏掉就会让探测真的去打
// z.ai，测试便不再是离线的。
func probeStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldTargets := upstreamProbeTargets
	upstreamProbeTargets = []string{srv.URL + "/"}
	t.Cleanup(func() {
		upstreamProbeTargets = oldTargets
		srv.Close()
	})
	return srv
}

// 删除线路时，原绑定账号应被自动改派到空閒线路，接口回报两种处置的计数。
func TestDeleteProxyReassignsAccounts(t *testing.T) {
	mux, st, _ := setup(t)
	lineA, err := st.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	lineB, err := st.AddProxyProfile("line-b", "socks5://2.2.2.2:1080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	// 新增账号时自动分配，按线路顺序拿到 line-a。
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts",
		map[string]any{"tokens": []any{jwtTokenFor("u-del-1")}})
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	ids, _ := body["ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("应新增 1 个账号: %v", body)
	}
	accID := ids[0].(string)
	if acc := st.Find(model.ProviderZai, accID); acc == nil || acc.ProxyID == nil || *acc.ProxyID != lineA.ID {
		t.Fatalf("账号应落在 line-a: %+v", acc)
	}

	code, body = do(t, mux, st, http.MethodDelete, "/admin/api/proxies/"+lineA.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	if n, _ := body["reassigned"].(float64); n != 1 {
		t.Fatalf("应回报改派 1 个账号: %v", body)
	}
	if n, _ := body["direct_fallback"].(float64); n != 0 {
		t.Fatalf("有空閒线路时不应回退直连: %v", body)
	}
	acc := st.Find(model.ProviderZai, accID)
	if acc.ProxyID == nil || *acc.ProxyID != lineB.ID {
		t.Fatalf("账号应已改派到 line-b: %+v", acc.ProxyID)
	}
	if acc.ProxyURL == nil || *acc.ProxyURL != lineB.URL {
		t.Fatalf("出站地址应同步为 line-b: %v", acc.ProxyURL)
	}

	// 再删 line-b：已无空閒线路，应退回直连。
	code, body = do(t, mux, st, http.MethodDelete, "/admin/api/proxies/"+lineB.ID, nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	if n, _ := body["direct_fallback"].(float64); n != 1 {
		t.Fatalf("应回报 1 个账号回退直连: %v", body)
	}
	acc = st.Find(model.ProviderZai, accID)
	if acc.ProxyID != nil || acc.ProxyURL != nil {
		t.Fatalf("回退直连后不应残留代理: %+v", acc)
	}
}

func TestTestAllProxies(t *testing.T) {
	mux, st, _ := setup(t)
	// 探测只关心「能不能拿到响应」，返回什么都不重要。
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	// httptest 服务器直接充当代理地址。
	line, err := st.AddProxyProfile("line-1", srv.URL, true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	// 停用的线路不应参与批量检测。
	if _, err := st.AddProxyProfile("line-off", srv.URL, false); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/proxies/test-all", nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("只应检测启用的线路: %v", results)
	}
	first, _ := results[0].(map[string]any)
	if first["id"] != line.ID {
		t.Fatalf("线路 ID 不符: %v", first)
	}
	if first["ok"] != true {
		t.Fatalf("应探测成功: %v", first)
	}
	upstream, _ := first["upstream"].(map[string]any)
	if upstream == nil || upstream["ok"] != true {
		t.Fatalf("应带上 z.ai 可达性明细: %v", first)
	}
	summary, _ := body["summary"].(map[string]any)
	if summary["total"] != float64(1) || summary["ok"] != float64(1) || summary["fail"] != float64(0) {
		t.Fatalf("汇总不符: %v", summary)
	}
}

func TestTestAllProxiesReportsFailure(t *testing.T) {
	mux, st, _ := setup(t)
	// 代理地址指向不可达端口：探测必然失败，但接口本身仍应 200 并逐条回报。
	line, err := st.AddProxyProfile("line-dead", "http://127.0.0.1:1", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ip":"1.2.3.4"}`))
	})

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/proxies/test-all", nil)
	if code != http.StatusOK {
		t.Fatalf("单条失败不应让整个接口失败: %d %v", code, body)
	}
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应有 1 条结果: %v", results)
	}
	first, _ := results[0].(map[string]any)
	if first["id"] != line.ID || first["ok"] != false {
		t.Fatalf("应逐条回报失败: %v", first)
	}
	if msg, _ := first["error"].(string); msg == "" {
		t.Fatalf("失败条目应带 error 文案: %v", first)
	}
	summary, _ := body["summary"].(map[string]any)
	if summary["ok"] != float64(0) || summary["fail"] != float64(1) {
		t.Fatalf("汇总应记 1 条失败: %v", summary)
	}
}

// firstResult 取批量检测结果里的唯一一条。
func firstResult(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	results, _ := body["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("应有 1 条结果: %v", body)
	}
	first, _ := results[0].(map[string]any)
	if first == nil {
		t.Fatalf("结果条目格式不符: %v", results)
	}
	return first
}

// 线路能连上 z.ai 时判为可用，并带上逐入口明细。
func TestProxyProbeReportsUpstreamReachable(t *testing.T) {
	mux, st, _ := setup(t)
	// 探测只关心「能不能拿到响应」，返回什么都不重要。
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	line, err := st.AddProxyProfile("line-ok", srv.URL, true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/proxies/test-all", nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	first := firstResult(t, body)
	if first["id"] != line.ID {
		t.Fatalf("线路 ID 不符: %v", first)
	}
	upstream, _ := first["upstream"].(map[string]any)
	if upstream == nil {
		t.Fatalf("结果应带 upstream 明细: %v", first)
	}
	if upstream["ok"] != true || upstream["blocked"] == true {
		t.Fatalf("z.ai 侧应可达: %v", upstream)
	}
	if targets, _ := upstream["targets"].([]any); len(targets) != 1 {
		t.Fatalf("应带逐入口明细: %v", upstream)
	}
	if first["ok"] != true {
		t.Fatalf("线路应判为可用: %v", first)
	}
	if _, hasErr := first["error"]; hasErr {
		t.Fatalf("可用时不应带 error: %v", first)
	}
}

// z.ai 边缘返回拦截页时，线路必须判为不可用，并给出可读原因。
func TestProxyProbeDetectsUpstreamBlock(t *testing.T) {
	mux, st, _ := setup(t)
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<html><title>Just a moment...</title></html>`))
	})
	if _, err := st.AddProxyProfile("line-blocked", srv.URL, true); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/proxies/test-all", nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	first := firstResult(t, body)
	upstream, _ := first["upstream"].(map[string]any)
	if upstream["blocked"] != true || upstream["ok"] != false {
		t.Fatalf("应识别为被边缘拦截: %v", upstream)
	}
	if first["ok"] != false {
		t.Fatalf("被拦截的线路不应判为可用: %v", first)
	}
	if msg, _ := first["error"].(string); msg != "z.ai 返回边缘拦截页" {
		t.Fatalf("应给出「被拦截」的原因: %v", first)
	}
	summary, _ := body["summary"].(map[string]any)
	if summary["fail"] != float64(1) {
		t.Fatalf("汇总应记为不可用: %v", summary)
	}
}

// z.ai 侧完全拿不到响应（连接被掐）时，应报「不通」而不是「被拦截」。
func TestProxyProbeReportsUpstreamUnreachable(t *testing.T) {
	mux, st, _ := setup(t)
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	if _, err := st.AddProxyProfile("line-half", srv.URL, true); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/proxies/test-all", nil)
	if code != http.StatusOK {
		t.Fatalf("应 200: %d %v", code, body)
	}
	first := firstResult(t, body)
	upstream, _ := first["upstream"].(map[string]any)
	if upstream["ok"] != false || upstream["blocked"] != false {
		t.Fatalf("应报「不通」而不是「被拦截」: %v", upstream)
	}
	if msg, _ := upstream["error"].(string); msg == "" {
		t.Fatalf("不通时应带 error: %v", upstream)
	}
	if first["ok"] != false {
		t.Fatalf("z.ai 不通则线路不可用: %v", first)
	}
	if msg, _ := first["error"].(string); msg == "" {
		t.Fatalf("原因应冒泡到条目上供前端展示: %v", first)
	}
}
