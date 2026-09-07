package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// setup 打开隔离存储并注册完整后台路由；返回 mux 与存储（预置数据用）。
func setup(t *testing.T) (*http.ServeMux, *store.Store) {
	t.Helper()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.AdminKeyEnv = ""
	config.GatewayKeyEnv = ""
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})
	st, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mux := http.NewServeMux()
	New(st, auth.New(st), captcha.NewManager()).Register(mux)
	return mux, st
}

// do 发起带鉴权的 JSON 请求并解码响应体（空响应体返回 nil map）。
func do(t *testing.T, mux *http.ServeMux, st *store.Store, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+st.AdminKey())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(res.Body).Decode(&decoded)
	return res.StatusCode, decoded
}

func str(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("应为字符串: %#v", v)
	}
	return s
}

func num(t *testing.T, v any) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("应为数值: %#v", v)
	}
	return f
}

func TestAdminUnauthorized(t *testing.T) {
	mux, st := setup(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/api/verify", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if res := rec.Result(); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺凭证应 401: %d", res.StatusCode)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/api/verify", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if res := rec.Result(); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误密钥应 401: %d", res.StatusCode)
	}

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/verify", nil)
	if code != http.StatusOK || str(t, body["status"]) != "ok" {
		t.Fatalf("正确密钥应通过: %d %v", code, body)
	}
}

func TestAddAndListAccounts(t *testing.T) {
	mux, st := setup(t)

	// 字符串 tokens 按行拆分 + 去重保序；默认名循环递增
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "tok-aaa\n tok-bbb \n\ntok-aaa",
	})
	if code != http.StatusOK || num(t, body["count"]) != 2 {
		t.Fatalf("添加应去重为 2: %d %v", code, body)
	}
	ids := body["ids"].([]any)
	code, body = do(t, mux, st, http.MethodGet, "/admin/api/accounts", nil)
	if code != http.StatusOK {
		t.Fatalf("列表应 200: %d", code)
	}
	accounts := body["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("应有 2 个账号: %d", len(accounts))
	}
	first := accounts[0].(map[string]any)
	if str(t, first["id"]) != str(t, ids[0]) {
		t.Fatalf("顺序应与 ids 一致: %v vs %v", first["id"], ids[0])
	}
	if str(t, first["name"]) != "zai-1" {
		t.Fatalf("默认名应 zai-1: %v", first["name"])
	}
	stats := body["stats"].(map[string]any)
	if num(t, stats["total"]) != 2 || num(t, stats["calls"]) != 0 {
		t.Fatalf("统计不符: %v", stats)
	}
	if _, ok := body["models"].([]any); !ok {
		t.Fatal("应包含 models 列表")
	}
	if _, ok := body["proxies"].([]any); !ok {
		t.Fatal("应包含 proxies 列表")
	}
}

func TestAddAccountsValidation(t *testing.T) {
	mux, st := setup(t)

	cases := []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{"provider": "foo", "tokens": "tok"}, "不支持的 provider"},
		{map[string]any{"tokens": "  \n "}, "请输入至少一个 Token / API Key"},
		{map[string]any{"tokens": "tok", "proxy_id": "proxy-none"}, "代理配置不存在"},
	}
	for i, c := range cases {
		code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", c.body)
		if code != http.StatusBadRequest || str(t, body["detail"]) != c.msg {
			t.Fatalf("用例 %d 应 400/%s: %d %v", i, c.msg, code, body)
		}
	}
}

func TestEditAccountLifecycle(t *testing.T) {
	mux, st := setup(t)
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "tok-plain",
	})
	accountID := str(t, added["ids"].([]any)[0])

	// 换成 jwt token + 改名
	code, body := do(t, mux, st, http.MethodPut, "/admin/api/accounts/"+accountID, map[string]any{
		"name":  "重命名",
		"token": "a.b.c",
	})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("编辑应成功: %d %v", code, body)
	}
	acc := st.FindAny(accountID)
	if acc.Mode != "jwt" || acc.Name != "重命名" || acc.Status != model.StatusActive {
		t.Fatalf("字段应更新: mode=%s name=%s", acc.Mode, acc.Name)
	}

	// 禁用
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+accountID+"/enabled",
		map[string]any{"enabled": false})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("禁用应成功: %d %v", code, body)
	}
	if acc := st.FindAny(accountID); acc.Enabled || acc.Status != model.StatusDisabled {
		t.Fatalf("应处于禁用状态: %+v", acc)
	}

	// 重置统计
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+accountID+"/reset-stats", nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("重置应成功: %d %v", code, body)
	}

	// 删除（请求体为 ID 数组）
	code, body = do(t, mux, st, http.MethodDelete, "/admin/api/accounts", []string{accountID})
	if code != http.StatusOK || num(t, body["deleted"]) != 1 {
		t.Fatalf("删除应成功: %d %v", code, body)
	}
	if st.FindAny(accountID) != nil {
		t.Fatal("账号应已移除")
	}

	// 不存在账号的各端点均 404
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/admin/api/accounts/none"},
		{http.MethodPost, "/admin/api/accounts/none/enabled"},
		{http.MethodPost, "/admin/api/accounts/none/refresh"},
		{http.MethodPost, "/admin/api/accounts/none/reset-stats"},
	} {
		code, body = do(t, mux, st, c.method, c.path, map[string]any{})
		if code != http.StatusNotFound || str(t, body["detail"]) != "账号不存在" {
			t.Fatalf("%s %s 应 404: %d %v", c.method, c.path, code, body)
		}
	}
}

func TestEditAccountValidation(t *testing.T) {
	mux, st := setup(t)
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{"tokens": "tok-x"})
	accountID := str(t, added["ids"].([]any)[0])

	cases := []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{"proxy_id": "proxy-none"}, "代理配置不存在"},
		{map[string]any{"disabled_models": "glm"}, "停用模型必須是陣列"},
		{map[string]any{"disabled_models": []string{longStr(101)}}, "停用模型格式無效"},
	}
	for i, c := range cases {
		code, body := do(t, mux, st, http.MethodPut, "/admin/api/accounts/"+accountID, c.body)
		if code != http.StatusBadRequest || str(t, body["detail"]) != c.msg {
			t.Fatalf("用例 %d 应 400/%s: %d %v", i, c.msg, code, body)
		}
	}

	// 不支持的代理协议（normalize 错误消息含允许清单后缀，查前缀即可）
	code, body := do(t, mux, st, http.MethodPut, "/admin/api/accounts/"+accountID,
		map[string]any{"proxy_url": "ftp://x"})
	if code != http.StatusBadRequest || !strings.HasPrefix(str(t, body["detail"]), "不支持的代理协议: ftp") {
		t.Fatalf("ftp 代理应 400: %d %v", code, body)
	}

	// 65 项超限
	tooMany := make([]string, 65)
	code, body = do(t, mux, st, http.MethodPut, "/admin/api/accounts/"+accountID,
		map[string]any{"disabled_models": tooMany})
	if code != http.StatusBadRequest || str(t, body["detail"]) != "停用模型數量過多" {
		t.Fatalf("超限应 400: %d %v", code, body)
	}
}

func longStr(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = 'a'
	}
	return string(out)
}

func TestProxiesCRUDAndAssign(t *testing.T) {
	mux, st := setup(t)

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/proxies", map[string]any{
		"name": "线路A", "url": "http://127.0.0.1:8080",
	})
	if code != http.StatusOK {
		t.Fatalf("新增应 200: %d %v", code, body)
	}
	profileID := str(t, body["id"])

	// 重名 / 空 URL / 不支持的协议
	for _, c := range []map[string]any{
		{"name": "线路A", "url": "http://127.0.0.1:1"},
		{"name": "线路B", "url": ""},
		{"name": "线路B", "url": "ftp://x"},
	} {
		code, body = do(t, mux, st, http.MethodPost, "/admin/api/proxies", c)
		if code != http.StatusBadRequest {
			t.Fatalf("非法新增应 400: %d %v", code, body)
		}
	}

	code, body = do(t, mux, st, http.MethodPut, "/admin/api/proxies/"+profileID, map[string]any{
		"name": "线路B", "url": "http://127.0.0.1:9090", "enabled": false,
	})
	if code != http.StatusOK || str(t, body["name"]) != "线路B" || body["enabled"] != false {
		t.Fatalf("更新应成功: %d %v", code, body)
	}
	code, body = do(t, mux, st, http.MethodPut, "/admin/api/proxies/proxy-none", map[string]any{
		"name": "x", "url": "http://127.0.0.1:1",
	})
	if code != http.StatusNotFound || str(t, body["detail"]) != "代理配置不存在" {
		t.Fatalf("更新不存在应 404: %d %v", code, body)
	}

	// 指派：账号 / 代理缺失的各种形态
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{"tokens": "tok-x"})
	accountID := str(t, added["ids"].([]any)[0])
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/proxies/assign", map[string]any{
		"account_id": accountID, "proxy_id": profileID,
	})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("指派应成功: %d %v", code, body)
	}
	if acc := st.FindAny(accountID); acc.ProxyID == nil || *acc.ProxyID != profileID {
		t.Fatalf("指派应生效: %+v", acc.ProxyID)
	}
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/proxies/assign", map[string]any{
		"account_id": accountID, "proxy_id": "proxy-none",
	})
	if code != http.StatusBadRequest || str(t, body["detail"]) != "代理配置不存在" {
		t.Fatalf("指派不存在线路应 400: %d %v", code, body)
	}
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/proxies/assign", map[string]any{
		"account_id": "none", "proxy_id": profileID,
	})
	if code != http.StatusNotFound || str(t, body["detail"]) != "帳號不存在" {
		t.Fatalf("指派不存在账号应 404: %d %v", code, body)
	}
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/proxies/assign", map[string]any{
		"account_id": " ",
	})
	if code != http.StatusBadRequest || str(t, body["detail"]) != "缺少帳號 ID" {
		t.Fatalf("缺账号 ID 应 400: %d %v", code, body)
	}

	code, body = do(t, mux, st, http.MethodDelete, "/admin/api/proxies/"+profileID, nil)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("删除应成功: %d %v", code, body)
	}
	code, body = do(t, mux, st, http.MethodDelete, "/admin/api/proxies/"+profileID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("二次删除应 404: %d %v", code, body)
	}
	if acc := st.FindAny(accountID); acc.ProxyID != nil {
		t.Fatal("删除线路应解除指派")
	}
}

func TestRefreshStubs(t *testing.T) {
	mux, st := setup(t)

	// M3 接入点：批量刷新与 jwt 单账号刷新均为 503
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts/refresh", map[string]any{})
	if code != http.StatusServiceUnavailable || str(t, body["detail"]) != "功能將在後續里程碑啟用" {
		t.Fatalf("批量刷新应 503: %d %v", code, body)
	}

	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "jwt.a.b\nplain-key",
	})
	ids := added["ids"].([]any)
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+str(t, ids[1])+"/refresh", nil)
	if code != http.StatusOK || body["ok"] != false ||
		str(t, body["message"]) != "仅 Coding Plan (JWT) 账号支持额度查询" {
		t.Fatalf("非 jwt 刷新应 200/false: %d %v", code, body)
	}
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+str(t, ids[0])+"/refresh", nil)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("jwt 刷新应 503: %d %v", code, body)
	}

	code, _ = do(t, mux, st, http.MethodPost, "/admin/api/login/start", map[string]any{})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("登录初始化应 503: %d", code)
	}
}

func TestSettingsFlow(t *testing.T) {
	mux, st := setup(t)

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if code != http.StatusOK || str(t, body["admin_key"]) != st.AdminKey() {
		t.Fatalf("读取设置不符: %d %v", code, body)
	}

	cases := []struct {
		body map[string]any
		msg  string
	}{
		{map[string]any{"admin_key": "  "}, "后台密钥不能为空"},
		{map[string]any{"gateway_key": ""}, "网关 API Key 不能为空"},
		{map[string]any{"quota_refresh_interval": "abc"}, "刷新间隔必须是非负整数"},
		{map[string]any{"quota_refresh_interval": []any{1}}, "刷新间隔必须是非负整数"},
	}
	for i, c := range cases {
		code, body := do(t, mux, st, http.MethodPut, "/admin/api/settings", c.body)
		if code != http.StatusBadRequest || str(t, body["detail"]) != c.msg {
			t.Fatalf("用例 %d 应 400/%s: %d %v", i, c.msg, code, body)
		}
	}

	code, body = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"quota_refresh_interval": "30",
	})
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("更新应成功: %d %v", code, body)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if num(t, body["quota_refresh_interval"]) != 30 {
		t.Fatalf("间隔应 30: %v", body["quota_refresh_interval"])
	}

	// float 截断与负数钳零（对齐 Python max(0, int(...))）
	_, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{"quota_refresh_interval": 12.9})
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if num(t, body["quota_refresh_interval"]) != 12 {
		t.Fatalf("12.9 应截断为 12: %v", body["quota_refresh_interval"])
	}
	_, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{"quota_refresh_interval": -5})
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if num(t, body["quota_refresh_interval"]) != 0 {
		t.Fatalf("负数应钳零: %v", body["quota_refresh_interval"])
	}
}

func TestMonitorAndUsage(t *testing.T) {
	mux, st := setup(t)
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "tok-a\njwt.b.c",
	})
	ids := added["ids"].([]any)

	// 预置用量与状态供快照/排行断言
	acc := st.FindAny(str(t, ids[0]))
	acc.UseCount, acc.FailCount, acc.TotalInputTokens, acc.TotalOutputTokens = 8, 2, 100, 40
	if err := st.UpdateAccount(acc); err != nil {
		t.Fatal(err)
	}

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/monitor", nil)
	if code != http.StatusOK {
		t.Fatalf("监控应 200: %d", code)
	}
	acct := body["accounts"].(map[string]any)
	if num(t, acct["total"]) != 2 || num(t, acct["active"]) != 2 {
		t.Fatalf("账号统计不符: %v", acct)
	}
	reqs := body["requests"].(map[string]any)
	if num(t, reqs["total"]) != 8 || num(t, reqs["errors"]) != 2 {
		t.Fatalf("请求统计不符: %v", reqs)
	}
	if fmt.Sprint(reqs["success_rate"]) != "75" {
		t.Fatalf("成功率应 75: %v", reqs["success_rate"])
	}
	if got := body["uptime_sec"].(float64); got < 0 {
		t.Fatalf("uptime 应非负: %v", got)
	}
	services := body["services"].([]any)
	if len(services) != 3 {
		t.Fatalf("应有 3 项服务: %d", len(services))
	}

	code, body = do(t, mux, st, http.MethodGet, "/admin/api/usage", nil)
	if code != http.StatusOK || str(t, body["window"]) != "累計" {
		t.Fatalf("用量应 200: %d %v", code, body)
	}
	ranking := body["ranking"].([]any)
	if len(ranking) != 2 {
		t.Fatalf("排行应 2 项: %d", len(ranking))
	}
	top := ranking[0].(map[string]any)
	if str(t, top["name"]) != "zai-1" || num(t, top["requests"]) != 8 || num(t, top["tokens"]) != 140 {
		t.Fatalf("排行首位不符: %v", top)
	}
	summary := body["summary"].(map[string]any)
	if num(t, summary["tokens_in"]) != 100 || num(t, summary["tokens_out"]) != 40 {
		t.Fatalf("汇总不符: %v", summary)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	mux, st := setup(t)
	_, added := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "tok-a\njwt.b.c",
	})
	ids := added["ids"].([]any)

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/export", nil)
	if code != http.StatusOK || num(t, body["version"]) != 1 {
		t.Fatalf("导出应 200/v1: %d %v", code, body)
	}
	providers := body["providers"].(map[string]any)
	if len(providers["zai"].([]any)) != 2 {
		t.Fatalf("导出应含 2 个账号: %v", providers)
	}

	// 重复导入（幂等：重复 token 返回既有账号，计数仍按条目累计）
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/import", body)
	if code != http.StatusOK || num(t, body["count"]) != 2 {
		t.Fatalf("导入应 200/2: %d %v", code, body)
	}
	if got := st.ListAccounts(""); len(got) != 2 {
		t.Fatalf("导入后应 2 个账号: %d", len(got))
	}
	if st.FindAny(str(t, ids[0])) == nil {
		t.Fatal("导入应保留账号")
	}
}
