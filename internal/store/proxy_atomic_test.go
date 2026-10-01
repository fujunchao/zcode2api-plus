package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"zcode2api/internal/model"
)

func TestProxyMutationFailureIsAtomic(t *testing.T) {
	for _, op := range []string{"update", "delete", "purge", "assign", "custom", "auto", "rotate"} {
		t.Run(op, func(t *testing.T) {
			s := newTestStore(t)
			p, _ := s.AddProxyProfile("before", "http://127.0.0.1:11001", true)
			other, _ := s.AddProxyProfile("other", "http://127.0.0.1:11002", true)
			a, _ := s.AddAccount(model.ProviderZai, "account", "test-token")
			_, _ = s.AssignProxyProfile(a.ID, p.ID)
			if op == "auto" {
				_, _ = s.AssignProxyProfile(a.ID, "")
			}
			profiles := s.ListProxyProfiles()
			accounts, _ := json.Marshal(s.ListAccounts(""))
			// 在账号写入处失败：必须连已写入的 meta 一起回滚。
			_, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_proxy_write BEFORE INSERT ON accounts WHEN NEW.id='%s' BEGIN SELECT RAISE(ABORT,'test failure'); END", a.ID))
			if err != nil {
				t.Fatal(err)
			}
			switch op {
			case "update":
				_, err = s.UpdateProxyProfile(p.ID, "changed", "http://127.0.0.1:11003", true)
			case "delete":
				_, _, err = s.DeleteProxyProfile(p.ID)
			case "purge":
				_, _, err = s.PurgeProxyProfiles([]string{p.ID})
			case "assign":
				_, err = s.AssignProxyProfile(a.ID, other.ID)
			case "custom":
				url := "http://127.0.0.1:11004"
				_, err = s.SetProxyURL(a.Provider, a.ID, &url)
			case "rotate":
				_, _, err = s.RotateAccountProxy(s.FindAny(a.ID))
			case "auto":
				assigned, _ := s.AutoAssignProxies([]string{a.ID})
				if len(assigned) != 0 {
					t.Fatal("失败指派不能报告成功")
				}
			}
			if op != "auto" && err == nil {
				t.Fatal("必须报告事务失败")
			}
			after, _ := json.Marshal(s.ListAccounts(""))
			if !reflect.DeepEqual(profiles, s.ListProxyProfiles()) || string(after) != string(accounts) {
				t.Fatal("失败后内存状态被改变")
			}
			reopened, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			disk, _ := json.Marshal(reopened.ListAccounts(""))
			if !reflect.DeepEqual(profiles, reopened.ListProxyProfiles()) || string(disk) != string(accounts) {
				t.Fatal("事务失败后数据库出现部分修改")
			}
		})
	}
}

func TestAddProxyFailureDoesNotPublish(t *testing.T) {
	s := newTestStore(t)
	_ = s.db.Close()
	if _, err := s.AddProxyProfile("test", "http://127.0.0.1:11001", true); err == nil {
		t.Fatal("应报告写库失败")
	}
	if len(s.ListProxyProfiles()) != 0 {
		t.Fatal("失败新增不应发布到内存")
	}
}
