package store

import (
	"testing"
	"zcode2api/internal/model"
)

func TestOAuthSaveSharesIdentityAndPreservesFailureAtomicity(t *testing.T) {
	st := newTestStore(t)
	oldToken := jwtFor("oauth-user")
	first, isNew, err := st.SaveOAuthAccount("oauth-login", oldToken, "test@example.invalid")
	if err != nil || !isNew || first.Name != "test@example.invalid" {
		t.Fatalf("新授权身份未保存：%v %v", first, err)
	}
	_, _ = st.Update(first.Provider, first.ID, func(a *model.Account) { a.Status = model.StatusInvalid })
	_ = st.db.Close()
	if _, _, err := st.SaveOAuthAccount("oauth-login", oldToken+"new", "test@example.invalid"); err == nil {
		t.Fatal("写入失败必须返回错误")
	}
	current := st.FindAny(first.ID)
	if current.Secret() != oldToken || current.Status != model.StatusInvalid {
		t.Fatal("失败的重新授权改变了运行中凭据")
	}
}
