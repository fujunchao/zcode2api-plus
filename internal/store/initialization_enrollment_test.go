package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func legacyInitializationCandidate(t *testing.T, s *Store, name string) *model.Account {
	t.Helper()
	a, _, err := s.AddAccountWithIdentity(model.ProviderZai, name, jwtFor(name), "")
	if err != nil {
		t.Fatal(err)
	}
	return s.FindAny(a.ID)
}

func TestNewAccountInitializationEnrollmentValidatesLiveHistory(t *testing.T) {
	for _, kind := range []string{"used", "last-used", "tokens", "plan", "quota", "unknown-quota", "claimed", "claim-wait", "disabled", "archived", "cooling", "invalid", "api-key"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, _ := initializationStoreFixture(t)
			a := legacyInitializationCandidate(t, s, kind)
			_, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
				future := float64(time.Now().Add(time.Hour).Unix())
				past := float64(1)
				switch kind {
				case "used":
					live.UseCount = 1
				case "last-used":
					live.LastUsedAt = &past
				case "tokens":
					live.TotalInputTokens = 1
				case "plan":
					live.Plans = []map[string]any{{"plan_id": "old"}}
				case "quota":
					live.Quota = map[string]map[string]any{"m": {"total": float64(100), "remaining": float64(0)}}
				case "unknown-quota":
					live.Quota = map[string]map[string]any{"m": {"total": "unknown"}}
				case "claimed":
					live.SetClaimState(&model.ClaimState{ClaimedAt: &past})
				case "claim-wait":
					live.SetClaimState(&model.ClaimState{NextAt: &future})
				case "disabled":
					live.Enabled = false
				case "archived":
					live.ArchivedAt = &past
				case "cooling":
					live.CoolingUntil = &future
				case "invalid":
					live.Status = model.StatusInvalid
				case "api-key":
					key := "synthetic-key"
					live.Mode = "apiKey"
					live.APIKey = &key
					live.JWTToken = nil
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			before := s.FindAny(a.ID)
			result, err := s.BatchEnrollInitializations([]string{a.ID}, true)
			if err != nil || result.Succeeded != 0 || result.Failed != 1 || s.Initialization(a.ID) != nil {
				t.Fatalf("不合格账号被补入：%+v %v", result, err)
			}
			if !reflect.DeepEqual(before, s.FindAny(a.ID)) {
				t.Fatal("拒绝补入仍修改了账号")
			}
		})
	}
}

func TestNewAccountInitializationEnrollmentIsAtomicAndInvalidatesOldRequests(t *testing.T) {
	s, _, _, _ := initializationStoreFixture(t)
	a := legacyInitializationCandidate(t, s, "a")
	b := legacyInitializationCandidate(t, s, "b")
	before := s.ListAccounts("")
	if _, err := s.db.Exec(fmt.Sprintf("CREATE TRIGGER reject_enrollment BEFORE INSERT ON account_initializations WHEN NEW.account_id='%s' BEGIN SELECT RAISE(ABORT,'injected'); END", b.ID)); err != nil {
		t.Fatal(err)
	}
	result, err := s.BatchEnrollInitializations([]string{a.ID, b.ID}, true)
	if err == nil || result.Succeeded != 0 || !reflect.DeepEqual(before, s.ListAccounts("")) || s.Initialization(a.ID) != nil || s.Initialization(b.ID) != nil {
		t.Fatal("批量补入失败发布了部分账号或任务")
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_enrollment"); err != nil {
		t.Fatal(err)
	}
	result, err = s.BatchEnrollInitializations([]string{a.ID, b.ID}, true)
	if err != nil || result.Succeeded != 2 {
		t.Fatalf("补入失败：%+v %v", result, err)
	}
	called := false
	if changed, err := s.UpdateForRequest(a, func(*model.Account) { called = true }); err != nil || changed || called {
		t.Fatal("补入前的旧请求仍能回写")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, id := range []string{a.ID, b.ID} {
		if task := reopened.Initialization(id); task == nil || !reopened.FindAny(id).InitializationBlocked() {
			t.Fatal("重启丢失补入任务或保护")
		}
	}
}

func TestNewAccountInitializationEnrollmentRequiresConfirmationAndPolicy(t *testing.T) {
	s, _, _, _ := initializationStoreFixture(t)
	a := legacyInitializationCandidate(t, s, "explicit")
	if _, err := s.BatchEnrollInitializations([]string{a.ID}, false); err == nil {
		t.Fatal("未确认就接管账号")
	}
	policy, err := s.NewAccountProxyPolicy()
	if err != nil {
		t.Fatal(err)
	}
	policy.Enabled = false
	if _, err := s.SaveNewAccountProxyPolicy(policy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BatchEnrollInitializations([]string{a.ID}, true); err == nil || s.Initialization(a.ID) != nil {
		t.Fatal("关闭策略时仍可补入")
	}
}

func TestNewAccountInitializationImportDoesNotInterruptExistingTask(t *testing.T) {
	s, a, _, _ := initializationStoreFixture(t)
	attempt, err := s.BeginInitialization(a.ID, time.Now())
	if err != nil || attempt == nil {
		t.Fatal(err)
	}
	defer s.ReleaseInitialization(attempt)
	before, task := s.FindAny(a.ID), s.Initialization(a.ID)
	count, newIDs, err := s.ImportAccounts(ImportPayload{Providers: map[string][]importItem{"zai": {{Name: "重复", Secret: a.Secret(), DisabledModels: []string{"other-model"}}}}})
	if err != nil || count != 1 || len(newIDs) != 0 || !reflect.DeepEqual(task, s.Initialization(a.ID)) || !reflect.DeepEqual(before, s.FindAny(a.ID)) {
		t.Fatalf("重复导入中断或重置了已有任务：%v", err)
	}
	if release, err := s.AcquireInitializationDispatch(attempt); err != nil {
		t.Fatal("重复导入撤销了在途执行权")
	} else {
		release()
	}
}

func TestNewAccountInitializationImportMetadataAndTaskCommitTogether(t *testing.T) {
	s, _, _, _ := initializationStoreFixture(t)
	count, ids, err := s.ImportAccounts(ImportPayload{Providers: map[string][]importItem{"zai": {{Name: "metadata", Secret: jwtFor("metadata"), DisabledModels: []string{"other-model"}}}}})
	if err != nil || count != 1 || len(ids) != 1 {
		t.Fatal(err)
	}
	a, task := s.FindAny(ids[0]), s.Initialization(ids[0])
	if task == nil || task.Revision != 1 || len(a.DisabledModels) != 1 || a.DisabledModels[0] != "other-model" {
		t.Fatal("导入元数据在登记任务之后进行了二次回写")
	}
}

func TestNewAccountInitializationImportRegistrationFailureRollsBackAccount(t *testing.T) {
	s, _, _, _ := initializationStoreFixture(t)
	before := s.ListAccounts("")
	if _, err := s.db.Exec(`CREATE TRIGGER reject_import_task BEFORE INSERT ON account_initializations WHEN json_extract(NEW.data,'$.source')='import' BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	count, ids, err := s.ImportAccounts(ImportPayload{Providers: map[string][]importItem{"zai": {{Name: "rollback", Secret: jwtFor("rollback"), DisabledModels: []string{"other-model"}}}}})
	if err == nil || count != 0 || len(ids) != 0 || !reflect.DeepEqual(before, s.ListAccounts("")) {
		t.Fatal("初始化登记失败仍留下了新导入账号")
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(reopened.ListAccounts("")) != len(before) {
		t.Fatal("重启出现部分导入数据")
	}
}
