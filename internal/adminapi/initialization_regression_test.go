package adminapi

import (
	"net/http"
	"reflect"
	"testing"

	"zcode2api/internal/model"
)

// 通过新增端点验证共享线路保护；上游和凭据均来自隔离夹具。
func TestNewAccountEmptyQuotaNeverDeletesSharedProxy(t *testing.T) {
	f := newAccountQuotaFixture(t, newAccountEmptyBalance, newAccountEmptyBalance)
	old, _, err := f.h.Store.SaveOAuthAccount("已有账号", jwtTokenFor("existing-shared"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.AssignProxyProfile(old.ID, f.bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Store.Update(old.Provider, old.ID, func(a *model.Account) { a.UseCount = 8 }); err != nil {
		t.Fatal(err)
	}
	before := f.h.Store.FindAny(old.ID)
	mux := http.NewServeMux()
	f.h.Register(mux)
	status, result := do(t, mux, f.h.Store, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": []string{jwtTokenFor("new-empty-shared")}, "proxy_id": f.bad.ID,
	})
	if status != http.StatusOK {
		t.Fatalf("新增失败：%d %v", status, result)
	}
	f.wait(t)
	if len(f.h.Store.ListProxyProfiles()) != 2 || !reflect.DeepEqual(before, f.h.Store.FindAny(old.ID)) {
		t.Fatal("新号空额度不得删除共享代理或改派已有账号")
	}
}
