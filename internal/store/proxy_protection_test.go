package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func configureProxyProtection(t *testing.T, s *Store, p, other ProxyProfile, kind string) {
	t.Helper()
	switch kind {
	case "manual":
		if _, err := s.UpdateProxyProfile(p.ID, p.Name, p.URL, true, true); err != nil {
			t.Fatal(err)
		}
	case "default", "managed_default":
		policy := DefaultNewAccountProxyPolicy()
		policy.Enabled, policy.DefaultProxyID = true, p.ID
		var err error
		policy, err = s.SaveNewAccountProxyPolicy(policy)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "managed_default" {
			if _, _, err := s.AddAccountWithIdentity(model.ProviderZai, "托管账号", "managed.synthetic.token", "", AccountCreationOptions{InitializeProxy: true, Source: "test"}); err != nil {
				t.Fatal(err)
			}
			policy.DefaultProxyID = other.ID
			if _, err := s.SaveNewAccountProxyPolicy(policy); err != nil {
				t.Fatal(err)
			}
		}
	case "observe":
		policy := s.ModelProbeSettings()
		policy.Mode = "observe"
		if _, err := s.SaveModelProbeSettings(policy, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProxyEndpointProtectionGuardsAutomaticRemoval(t *testing.T) {
	for _, protection := range []string{"manual", "default", "managed_default", "observe"} {
		for _, entry := range []string{"account_failure", "network", "batch", "login"} {
			t.Run(protection+"/"+entry, func(t *testing.T) {
				s := newTestStore(t)
				p := addSelectionProxy(t, s, "家庭出口", 18080, true)
				alias := addSelectionProxy(t, s, "同路由别名", 18080, true)
				other := addSelectionProxy(t, s, "备用出口", 18081, true)
				a := addSelectionAccount(t, s, "业务账号", p.ID)
				configureProxyProtection(t, s, p, other, protection)
				a = s.FindAny(a.ID)
				beforeAccounts, beforeProfiles := s.ListAccounts(""), s.ListProxyProfiles()
				switch entry {
				case "account_failure":
					if removed, _, err := s.PurgeFailedAccountProxy(a, s.ProxyEgress(a)); err != nil || removed {
						t.Fatalf("自动故障处置必须遵守保护：%v %v", removed, err)
					}
				case "network":
					if removed, _, err := s.ApplyNetworkHealth(t.Context(), s.ModelProbeSettings(), beforeProfiles[:2]); err != nil || len(removed) != 0 {
						t.Fatalf("网络删除必须遵守保护：%v %v", removed, err)
					}
				case "batch":
					if removed, _, err := s.PurgeProxyProfiles([]string{p.ID, alias.ID}); err != nil || len(removed) != 0 {
						t.Fatalf("批量自动删除不得绕过同路由保护：%v %v", removed, err)
					}
				case "login":
					if result, err := s.ReplaceFailedLoginProxy(p.ID, p.URL, nil); err != nil || result.Removed || len(result.Reassign.Assigned) != 0 {
						t.Fatalf("登录失败不得删除受保护的共享出口：%v %v", result, err)
					}
				}
				if !reflect.DeepEqual(beforeAccounts, s.ListAccounts("")) || !reflect.DeepEqual(beforeProfiles, s.ListProxyProfiles()) {
					t.Fatal("保护必须覆盖全部绑定和节点，不能只阻止删除日志")
				}
				if protection != "observe" {
					protections := s.ProxyAutoDeleteProtections()
					if protections[p.ID] == "" || protections[alias.ID] == "" {
						t.Fatal("管理界面应可解释原节点及别名的有效保护")
					}
				}
				if removed, _, err := s.DeleteProxyProfile(p.ID); err != nil || !removed {
					t.Fatalf("保护不能剥夺显式手动删除能力：%v %v", removed, err)
				}
			})
		}
	}
}

func TestProxyEndpointProtectionPersistsAndInvalidatesOldProof(t *testing.T) {
	s := newTestStore(t)
	p, err := s.AddProxyProfile("人工出口", "http://127.0.0.1:18080", true, true)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateProxyProfile(p.ID, "旧客户端改名", p.URL, true)
	if err != nil || !updated.AutoDeleteProtected || updated.Generation != p.Generation {
		t.Fatal("旧客户端省略字段不能解除保护或制造路由变化")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if profiles := reopened.ListProxyProfiles(); len(profiles) != 1 || !profiles[0].AutoDeleteProtected || reopened.ProxyAutoDeleteProtections()[p.ID] != "manual" {
		t.Fatal("手工保护必须跨重启保留")
	}
	unprotected, err := s.UpdateProxyProfile(p.ID, p.Name, p.URL, true, false)
	if err != nil || unprotected.AutoDeleteProtected || unprotected.Generation <= p.Generation {
		t.Fatal("明确修改保护必须使旧故障证据失效")
	}
	if removed, _, err := s.PurgeProxyProfilesIfCurrent([]ProxyProfile{p}); err != nil || len(removed) != 0 {
		t.Fatal("解除保护后不能拿保护前的旧代次证据删除")
	}
}

func TestProxyEndpointProtectionStopsNewStrictRetirement(t *testing.T) {
	for _, protection := range []string{"manual", "default", "managed_default", "observe"} {
		t.Run(protection, func(t *testing.T) {
			s := newTestStore(t)
			p := addSelectionProxy(t, s, "家庭出口", 18080, true)
			other := addSelectionProxy(t, s, "参考出口", 18081, true)
			a := addSelectionAccount(t, s, "业务账号", p.ID)
			configureProxyProtection(t, s, p, other, protection)
			now := time.Now()
			for i := 0; i < 3; i++ {
				if i == 2 {
					if _, err := s.AssignProxyProfile(a.ID, other.ID); err != nil {
						t.Fatal(err)
					}
				}
				snapshot := s.FindAny(a.ID)
				observation := ProxyObservation{RequestID: fmt.Sprintf("proof-%d", i), EntryRef: fmt.Sprintf("entry-%d", i), TargetOrigin: "https://model.example.test", TargetPath: "/api/anthropic/v1/messages", Method: "POST", Model: "glm-test", ContentRef: "same-content", ProfileRef: "desktop-v1", SessionRef: "same-session", DeviceRef: "same-device", BusinessCode: "3012", Status: 405, At: now.Add(time.Duration(i) * time.Second)}
				if i == 2 {
					observation.Status, observation.Complete, observation.BusinessCode = 200, true, ""
				}
				decision, err := s.ObserveProxyOutcome(snapshot, s.ProxyEgress(snapshot), observation)
				if err != nil || len(decision.Removed) != 0 || decision.DecisionID != "" {
					t.Fatalf("严格模型对照也不得绕过节点保护或观察模式：%+v %v", decision, err)
				}
			}
			if len(s.ListProxyProfiles()) != 2 {
				t.Fatal("达到模型归因门槛也不能删除受保护配置")
			}
			if _, err := s.AddProxyProfile("验证没有新增永久禁止", p.URL, true); err != nil {
				t.Fatalf("不得借保护之名保留配置却悄悄写永久禁止：%v", err)
			}
		})
	}
}
