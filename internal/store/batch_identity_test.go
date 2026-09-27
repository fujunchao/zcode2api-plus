package store

import (
	"testing"

	"zcode2api/internal/model"
)

func TestBatchAddDeduplicatesWithinRequest(t *testing.T) {
	for _, tt := range []struct {
		name  string
		items []BatchAddItem
	}{
		{"凭据", []BatchAddItem{{Name: "first", Secret: "same-key"}, {Name: "second", Secret: "same-key"}}},
		{"用户身份", []BatchAddItem{{Name: "first", Secret: jwtFor("same-user")}, {Name: "second", Secret: jwtFor("same-user") + "new"}}},
		{"邮箱", []BatchAddItem{{Name: "first", Secret: "key-one", Email: "same@example.invalid"}, {Name: "second", Secret: "key-two", Email: "same@example.invalid"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(t)
			res, err := s.BatchAddAccounts(model.ProviderZai, tt.items)
			if err != nil {
				t.Fatal(err)
			}
			if res.Succeeded != 1 || res.Duplicated != 1 || len(res.IDs) != 1 || res.Items[1].ID != res.Items[0].ID {
				t.Fatalf("同批重复项应指向同一个账号：%+v", res)
			}
			if len(s.ListAccounts(model.ProviderZai)) != 1 {
				t.Fatal("内存中存在重复账号")
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if got := reopened.ListAccounts(model.ProviderZai); len(got) != 1 || got[0].Secret() != tt.items[0].Secret {
				t.Fatal("数据库应只保留第一条凭据，不得让重复项覆盖它")
			}
		})
	}
}

func TestBatchIdentityPriorityIncludesPendingAccounts(t *testing.T) {
	s := newTestStore(t)
	old, _, err := s.AddAccountWithIdentity(model.ProviderZai, "old-email", "old-key", "shared@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.BatchAddAccounts(model.ProviderZai, []BatchAddItem{
		{Name: "pending-user", Secret: jwtFor("pending-user")},
		{Name: "duplicate", Secret: jwtFor("pending-user") + "new", Email: "shared@example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Succeeded != 1 || res.Duplicated != 1 || res.Items[1].ID != res.Items[0].ID || res.Items[1].ID == old.ID {
		t.Fatalf("同批用户身份必须优先于已有账号的邮箱：%+v", res)
	}
}

func TestBatchRollbackClearsPendingDuplicateIDs(t *testing.T) {
	s := newTestStore(t)
	old, err := s.AddAccount(model.ProviderZai, "existing", "existing-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_batch BEFORE INSERT ON accounts
		WHEN NEW.name = 'reject' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := s.BatchAddAccounts(model.ProviderZai, []BatchAddItem{
		{Secret: "existing-key"}, {Name: "fresh", Secret: "fresh-key"},
		{Name: "duplicate", Secret: "fresh-key"}, {Name: "reject", Secret: "reject-key"},
	})
	if err == nil || res.Succeeded != 0 || res.Duplicated != 1 || res.Failed != 3 || len(res.IDs) != 0 {
		t.Fatalf("回滚时仅已有账号仍可报告重复：result=%+v err=%v", res, err)
	}
	if res.Items[0].Status != BatchStatusDuplicate || res.Items[0].ID != old.ID {
		t.Fatal("已有账号的重复引用应保留")
	}
	for _, item := range res.Items[1:] {
		if item.Status != BatchStatusError || item.ID != "" {
			t.Fatalf("回滚后不得返回指向不存在账号的重复 ID：%+v", item)
		}
	}
	if len(s.ListAccounts(model.ProviderZai)) != 1 {
		t.Fatal("回滚后内存不应存在新账号")
	}
}
