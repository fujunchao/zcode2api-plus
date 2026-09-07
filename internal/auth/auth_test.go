package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

// openStore 在临时路径打开存储（隔离配置变量，测试结束恢复并关闭）。
func openStore(t *testing.T) *store.Store {
	t.Helper()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = filepath.Join(t.TempDir(), "accounts.db")
	config.AdminKeyEnv = ""
	config.GatewayKeyEnv = ""
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})
	s, err := store.New()
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// adminReq 构造后台校验请求（token 为空时不带 Authorization 头）。
func adminReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/api/verify", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestVerifyAdminKeyAcceptsValidKey(t *testing.T) {
	svc := New(openStore(t))
	if e := svc.VerifyAdminKey(adminReq(svc.Store.AdminKey())); e != nil {
		t.Fatalf("有效密钥应通过: %+v", e)
	}
}

func TestVerifyAdminKeyMissingBearer(t *testing.T) {
	svc := New(openStore(t))
	e := svc.VerifyAdminKey(adminReq(""))
	if e == nil || e.Status != http.StatusUnauthorized || e.Message != "缺少鉴权凭证" {
		t.Fatalf("缺凭证应 401: %+v", e)
	}
	// 非 Bearer scheme 同样视为缺失
	r := adminReq(svc.Store.AdminKey())
	r.Header.Set("Authorization", "Basic abc")
	if e := svc.VerifyAdminKey(r); e == nil || e.Status != http.StatusUnauthorized {
		t.Fatalf("非 Bearer 应 401: %+v", e)
	}
}

func TestVerifyAdminKeyLimitsFailures(t *testing.T) {
	svc := New(openStore(t))
	key := svc.Store.AdminKey()

	// 窗口内连续失败 maxFailures 次内均为 401，此后一律 429（含正确密钥）
	for i := 0; i < maxFailures; i++ {
		e := svc.VerifyAdminKey(adminReq("wrong-" + fmt.Sprint(i)))
		if e == nil || e.Status != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401: %+v", i+1, e)
		}
	}
	if e := svc.VerifyAdminKey(adminReq(key)); e == nil || e.Status != http.StatusTooManyRequests {
		t.Fatalf("达到上限后正确密钥也应 429: %+v", e)
	}
	if e := svc.VerifyAdminKey(adminReq("wrong")); e == nil || e.Status != http.StatusTooManyRequests {
		t.Fatalf("达到上限后失败尝试应 429: %+v", e)
	}
}

func TestVerifyAdminKeySuccessResetsFailures(t *testing.T) {
	svc := New(openStore(t))
	key := svc.Store.AdminKey()
	for i := 0; i < maxFailures-1; i++ {
		_ = svc.VerifyAdminKey(adminReq("wrong"))
	}
	if e := svc.VerifyAdminKey(adminReq(key)); e != nil {
		t.Fatalf("成功应通过: %+v", e)
	}
	// 成功清零后可再承受同样次数的失败
	for i := 0; i < maxFailures-1; i++ {
		_ = svc.VerifyAdminKey(adminReq("wrong"))
	}
	if e := svc.VerifyAdminKey(adminReq(key)); e != nil {
		t.Fatalf("清零后不应提前限速: %+v", e)
	}
}

func TestVerifyGatewayKey(t *testing.T) {
	svc := New(openStore(t))
	key := svc.Store.GatewayKey()

	if e := svc.VerifyGatewayKey(httptest.NewRequest(http.MethodPost, "/v1/messages", nil)); e == nil || e.Status != http.StatusUnauthorized {
		t.Fatalf("缺凭证应 401: %+v", e)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	if e := svc.VerifyGatewayKey(r); e == nil || e.Status != http.StatusForbidden {
		t.Fatalf("错误密钥应 403: %+v", e)
	}
	r = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("x-api-key", key)
	if e := svc.VerifyGatewayKey(r); e != nil {
		t.Fatalf("x-api-key 应通过: %+v", e)
	}
}
