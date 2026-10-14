package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func TestRequestCredentialGenerationRejectsABAHealthAndClaimWrites(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionAccount(t, s, "同一身份", "")
	expected := s.FindAny(a.ID)
	for _, token := range []string{jwtFor("temporary-identity"), expected.Secret()} {
		if _, err := s.RenewJWT(a.Provider, a.ID, token); err != nil {
			t.Fatal(err)
		}
	}
	before := s.FindAny(a.ID)
	if expected.CredentialGeneration == 0 || before.CredentialGeneration <= expected.CredentialGeneration {
		t.Fatal("持久凭据代次应跨 A→B→A 递增")
	}
	called := false
	changed, err := s.UpdateForRequest(expected, func(live *model.Account) {
		called = true
		live.Status = model.StatusInvalid
		key := "stale-exchanged-key"
		live.APIKey = &key
		at := float64(123)
		live.SetClaimState(&model.ClaimState{ClaimedAt: &at})
	})
	if err != nil || changed || called || !reflect.DeepEqual(before, s.FindAny(a.ID)) {
		t.Fatalf("旧请求不得写新身份的健康、领取或兑换结果：changed=%v called=%v err=%v", changed, called, err)
	}
	if err := s.MarkQuotaExhausted(expected, recoveryModel, "迟到耗尽", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, s.FindAny(a.ID)) {
		t.Fatal("旧身份耗尽结果不得写新代次")
	}
	changed, err = s.UpdateForRequest(before, func(live *model.Account) { live.LastError = nil })
	if err != nil || !changed {
		t.Fatalf("新代快照仍须正常接收结果：%v %v", changed, err)
	}
}

func TestCredentialGenerationSurvivesRestartAndCannotComeFromAccountJSON(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionAccount(t, s, "持久代次", "")
	if _, err := s.RenewJWT(a.Provider, a.ID, jwtFor("replacement")); err != nil {
		t.Fatal(err)
	}
	latest := s.FindAny(a.ID)
	raw, err := json.Marshal(latest)
	if err != nil {
		t.Fatal(err)
	}
	var copy model.Account
	if err := json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateForRequest(&copy, func(live *model.Account) { t.Error("未冻结 JSON 应拒绝") }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, snapshot := range []*model.Account{
		reopened.Find(a.Provider, a.ID), reopened.FindAny(a.ID), reopened.SnapshotAccount(a.Provider, a.ID),
		reopened.ListAccounts(a.Provider)[0], reopened.Select(a.Provider, nil, ""),
	} {
		if snapshot == nil || snapshot.CredentialGeneration != latest.CredentialGeneration || !snapshot.SameCredential(latest) {
			t.Fatal("全部请求读取入口必须携带持久代次")
		}
	}
}

func TestCredentialGenerationEditFailureDoesNotPublishNewVersion(t *testing.T) {
	s := newTestStore(t)
	a := addSelectionAccount(t, s, "原子编辑", "")
	if _, err := s.db.Exec("CREATE TRIGGER reject_credential_version BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetSecret: true, SecretMode: "jwt", Secret: jwtFor("replacement")}); err == nil {
		t.Fatal("失败编辑必须报告错误")
	}
	if current := s.FindAny(a.ID); !current.SameCredential(a) || current.CredentialGeneration != a.CredentialGeneration {
		t.Fatal("事务失败不能发布凭据或新代次")
	}
}

func TestQuotaRecoveryOldLeaseCannotRestoreAfterCredentialABA(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1_790_000_000, 0)
	a := quotaTestAccount(t, s, base)
	now := base.Add(5 * time.Minute)
	probe := requireQuotaProbe(t, s, recoveryModel, now, true)
	for _, secret := range []string{"sk-temporary-generation", a.Secret()} {
		if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetSecret: true, SecretMode: "apiKey", Secret: secret}); err != nil {
			t.Fatal(err)
		}
	}
	if restored, err := s.FinishQuotaProbe(probe, true, now); err != nil || restored {
		t.Fatalf("相同字节的新凭据代次不能被旧恢复租约清除耗尽：%v %v", restored, err)
	}
	if len(s.FindAny(a.ID).ExhaustedModels) != 1 {
		t.Fatal("旧租约不应清除模型耗尽")
	}
	requireQuotaProbe(t, s, recoveryModel, now, false)
	fresh := requireQuotaProbe(t, s, recoveryModel, now.Add(5*time.Minute), true)
	if restored, err := s.FinishQuotaProbe(fresh, true, now.Add(5*time.Minute)); err != nil || !restored {
		t.Fatalf("新代次的有效租约仍应能恢复：%v %v", restored, err)
	}
}
