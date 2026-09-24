// 账号批量操作的测试：事务一致性（失败零副作用）、逐条明细、严格/宽松语义、
// 条件查询过滤器。行为规格见 docs/plan-account-batch-management.md §2。
package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"zcode2api/internal/model"
)

// countAccountsInDB 重开一个 Store 数 DB 里的账号行数（验证落库与内存一致）。
func countAccountsInDB(t *testing.T, dbPath string) int {
	t.Helper()
	reopened := openAt(t, dbPath)
	defer func() { _ = reopened.Close() }()
	return len(reopened.ListAccounts(model.ProviderZai))
}

func TestBatchAddMixedResults(t *testing.T) {
	s := newTestStore(t)
	// 预置一个既有账号，供重复项判定（同凭据）。
	existing, err := s.AddAccount(model.ProviderZai, "existing", "dup-secret-1")
	if err != nil {
		t.Fatalf("预置账号失败: %v", err)
	}

	res, err := s.BatchAddAccounts(model.ProviderZai, []BatchAddItem{
		{Name: "new-a", Secret: "fresh-key-a"},
		{Name: "dup", Secret: "dup-secret-1"}, // 与 existing 凭据相同 → duplicate
	})
	if err != nil {
		t.Fatalf("批量新增失败: %v", err)
	}
	if res.Total != 2 || res.Succeeded != 1 || res.Duplicated != 1 || res.Failed != 0 {
		t.Fatalf("汇总计数不符: %+v", res)
	}
	if len(res.IDs) != 1 || res.IDs[0] == existing.ID {
		t.Fatalf("IDs 应只含新建账号: %v", res.IDs)
	}
	if res.Items[0].Status != BatchStatusOK || res.Items[1].Status != BatchStatusDuplicate {
		t.Fatalf("逐条状态不符: %+v", res.Items)
	}
	if res.Items[1].ID != existing.ID {
		t.Fatalf("重复项应回传既有 ID: %+v", res.Items[1])
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 2 {
		t.Fatalf("池内应有 2 个账号，实际 %d", got)
	}

	// 新账号应有独立设备指纹（与单账号入池同一规则）。
	fresh := s.FindAny(res.IDs[0])
	if fresh == nil || fresh.VirtualDeviceMid == nil || *fresh.VirtualDeviceMid == "" {
		t.Fatalf("新账号未分配设备指纹: %+v", fresh)
	}
}

func TestBatchAddInvalidSecretZeroSideEffect(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddAccount(model.ProviderZai, "keep", "keep-key"); err != nil {
		t.Fatalf("预置失败: %v", err)
	}

	res, err := s.BatchAddAccounts(model.ProviderZai, []BatchAddItem{
		{Secret: ""},                        // 空凭据
		{Secret: strings.Repeat("x", 9000)}, // 超长
		{Secret: "good-key"},
	})
	if err == nil {
		t.Fatal("存在非法项应整体报错")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("错误应说明坏项数量: %v", err)
	}
	if res.Failed != 2 || len(res.Items) != 2 {
		t.Fatalf("应逐条标注全部坏项: %+v", res)
	}
	// 零副作用：内存里不得出现 good-key。
	if got := len(s.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("校验失败不得写入，池内应仍为 1 个，实际 %d", got)
	}
	for _, a := range s.ListAccounts(model.ProviderZai) {
		if a.Secret() == "good-key" {
			t.Fatal("非法项存在时新项不得入池")
		}
	}
}

func TestBatchAddRollbackWhenDBUnavailable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, dbPath)
	if _, err := s.AddAccount(model.ProviderZai, "pre", "pre-key"); err != nil {
		t.Fatalf("预置失败: %v", err)
	}
	// 关闭底层 DB 注入持久化失败（BeginTx 即失败）。
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	res, err := s.BatchAddAccounts(model.ProviderZai, []BatchAddItem{
		{Secret: "a"}, {Secret: "b"}, {Secret: "c"},
	})
	if err == nil {
		t.Fatal("DB 不可用应报错")
	}
	if res.Succeeded != 0 || res.Failed != 3 {
		t.Fatalf("整批应标记失败且零成功: %+v", res)
	}
	for _, item := range res.Items {
		if item.Status != BatchStatusError {
			t.Fatalf("全部条目应标 error: %+v", res.Items)
		}
		if item.ID != "" {
			t.Fatalf("回滚后不得留下假 ID: %+v", item)
		}
	}
	// 内存零变化（DB 已关，只能查内存）。
	if got := len(s.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("回滚后内存应仍为 1 个账号，实际 %d", got)
	}
}

func TestBatchAddRejectsOversizeAndUnknownProvider(t *testing.T) {
	s := newTestStore(t)
	items := make([]BatchAddItem, MaxBatchSize+1)
	for i := range items {
		items[i].Secret = "k"
	}
	if _, err := s.BatchAddAccounts(model.ProviderZai, items); err == nil {
		t.Fatal("超过上限应报错")
	}
	if _, err := s.BatchAddAccounts("nope", []BatchAddItem{{Secret: "k"}}); err == nil {
		t.Fatal("未知 provider 应报错")
	}
	if _, err := s.BatchAddAccounts(model.ProviderZai, nil); err == nil {
		t.Fatal("空列表应报错")
	}
}

func TestBatchRemoveStrictVsMissingOK(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, dbPath)
	var ids []string
	for _, name := range []string{"d1", "d2", "d3"} {
		acc, err := s.AddAccount(model.ProviderZai, name, "key-"+name)
		if err != nil {
			t.Fatalf("预置失败: %v", err)
		}
		ids = append(ids, acc.ID)
	}

	// 严格模式：一个缺失 → 整体拒绝、零删除。
	res, err := s.BatchRemoveAccounts([]string{ids[0], "ghost", ids[1]}, false)
	if !errors.Is(err, ErrBatchMissingIDs) {
		t.Fatalf("严格模式应返回 ErrBatchMissingIDs: %v", err)
	}
	if res.NotFound != 1 || res.Succeeded != 2 {
		t.Fatalf("明细应标出缺失项: %+v", res)
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 3 {
		t.Fatalf("严格模式一个都不删，实际剩 %d", got)
	}
	if got := countAccountsInDB(t, dbPath); got != 3 {
		t.Fatalf("DB 应仍为 3 行，实际 %d", got)
	}

	// 宽松模式：跳过缺失、删掉存在项。
	res, err = s.BatchRemoveAccounts([]string{ids[0], "ghost", ids[1]}, true)
	if err != nil {
		t.Fatalf("宽松模式应成功: %v", err)
	}
	if res.Succeeded != 2 || res.NotFound != 1 {
		t.Fatalf("宽松模式计数不符: %+v", res)
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("应剩 1 个账号，实际 %d", got)
	}
	if got := countAccountsInDB(t, dbPath); got != 1 {
		t.Fatalf("DB 应剩 1 行，实际 %d", got)
	}
	if remain := s.ListAccounts(model.ProviderZai)[0]; remain.ID != ids[2] {
		t.Fatalf("剩下的应是 d3: %+v", remain)
	}
}

func TestBatchRemoveRollbackWhenDBUnavailable(t *testing.T) {
	s := newTestStore(t)
	var ids []string
	for _, name := range []string{"r1", "r2"} {
		acc, err := s.AddAccount(model.ProviderZai, name, "key-"+name)
		if err != nil {
			t.Fatalf("预置失败: %v", err)
		}
		ids = append(ids, acc.ID)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	res, err := s.BatchRemoveAccounts(ids, true)
	if err == nil {
		t.Fatal("DB 不可用应报错")
	}
	if res.Succeeded != 0 {
		t.Fatalf("回滚后不得报告成功: %+v", res)
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 2 {
		t.Fatalf("内存应保持 2 个账号，实际 %d", got)
	}
}

func TestBatchSetEnabledTransitions(t *testing.T) {
	s := newTestStore(t)
	mk := func(name, secret string) *model.Account {
		acc, err := s.AddAccount(model.ProviderZai, name, secret)
		if err != nil {
			t.Fatalf("预置失败: %v", err)
		}
		return acc
	}
	active := mk("e-active", "k1")
	// invalid 账号：直接改状态字段（经 Update 走锁内合法路径），且**不**参与
	// 前面的禁用步骤——禁用会把 invalid 覆写成 disabled，启用转移就无从验证了。
	invalid := mk("e-invalid", "k2")
	if _, err := s.Update(model.ProviderZai, invalid.ID, func(a *model.Account) {
		a.Status = model.StatusInvalid
	}); err != nil {
		t.Fatalf("置 invalid 失败: %v", err)
	}

	// 禁用 active 账号。
	res, err := s.BatchSetEnabled([]string{active.ID}, false, false)
	if err != nil {
		t.Fatalf("批量禁用失败: %v", err)
	}
	if res.Succeeded != 1 {
		t.Fatalf("禁用计数不符: %+v", res)
	}
	if acc := s.FindAny(active.ID); acc.Enabled || acc.Status != model.StatusDisabled {
		t.Fatalf("账号 %s 应为 disabled: enabled=%v status=%s", active.ID, acc.Enabled, acc.Status)
	}

	// 启用：disabled → active；invalid 只翻 Enabled 标志、状态保持 invalid
	// （与单账号 SetEnabled 同一转移逻辑，归档/失效语义不被越权改写）。
	res, err = s.BatchSetEnabled([]string{active.ID, invalid.ID}, true, false)
	if err != nil {
		t.Fatalf("批量启用失败: %v", err)
	}
	if a := s.FindAny(active.ID); !a.Enabled || a.Status != model.StatusActive {
		t.Fatalf("disabled 启用后应为 active: %+v", a)
	}
	if a := s.FindAny(invalid.ID); !a.Enabled || a.Status != model.StatusInvalid {
		t.Fatalf("invalid 启用后状态应保持 invalid: enabled=%v status=%s", a.Enabled, a.Status)
	}

	// 严格模式：缺一个 → 整体拒绝、零改动。
	if _, err := s.BatchSetEnabled([]string{active.ID, "ghost"}, false, false); !errors.Is(err, ErrBatchMissingIDs) {
		t.Fatalf("严格模式应返回 ErrBatchMissingIDs: %v", err)
	}
	if a := s.FindAny(active.ID); !a.Enabled {
		t.Fatal("严格拒绝后不得改动任何账号")
	}
	// 宽松模式：缺失跳过、存在项照常。
	if _, err := s.BatchSetEnabled([]string{active.ID, "ghost"}, false, true); err != nil {
		t.Fatalf("宽松模式应成功: %v", err)
	}
	if a := s.FindAny(active.ID); a.Enabled || a.Status != model.StatusDisabled {
		t.Fatalf("宽松模式应禁用成功: enabled=%v status=%s", a.Enabled, a.Status)
	}
}

func TestQueryAccountsFiltersAndPaging(t *testing.T) {
	s := newTestStore(t)
	// 四个账号：a1 jwt active、a2 jwt disabled、a3 apiKey（带 email）、a4 jwt exhausted(flash)。
	a1, _, _ := s.AddAccountWithIdentity(model.ProviderZai, "alpha", jwtFor("u-a"), "alpha@example.com")
	a2, _, _ := s.AddAccountWithIdentity(model.ProviderZai, "beta", jwtFor("u-b"), "")
	if _, err := s.BatchSetEnabled([]string{a2.ID}, false, false); err != nil {
		t.Fatalf("禁用 a2 失败: %v", err)
	}
	a3, _, _ := s.AddAccountWithIdentity(model.ProviderZai, "gamma", "plain-key-3", "gamma@example.com")
	a4, _, _ := s.AddAccountWithIdentity(model.ProviderZai, "delta", jwtFor("u-d"), "")
	// 模型可用性：无快照的账号是 unknown 不算 available，先给 a1/a2/a3 预置
	// 剩余额度（available），a4 置零（exhausted）。
	for _, acc := range []*model.Account{a1, a2, a3} {
		if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
			a.Quota["glm-5.3-flash"] = map[string]any{"remaining": 50, "total": 100, "used": 50}
		}); err != nil {
			t.Fatalf("预置额度失败: %v", err)
		}
	}
	if _, err := s.Update(model.ProviderZai, a4.ID, func(a *model.Account) {
		a.Quota["glm-5.3-flash"] = map[string]any{"remaining": 0, "total": 100, "used": 100}
		a.SyncExhaustedModels()
	}); err != nil {
		t.Fatalf("置 exhausted 失败: %v", err)
	}

	// 零过滤 = 全量，顺序 created_at 升序（与 DB 载入一致）。
	all, total := s.QueryAccounts(AccountQuery{})
	if total != 4 || len(all) != 4 {
		t.Fatalf("零过滤应返回全部 4 个: total=%d len=%d", total, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].CreatedAt > all[i].CreatedAt {
			t.Fatal("默认排序应为 created_at 升序")
		}
	}

	// keyword：email 命中。
	got, total := s.QueryAccounts(AccountQuery{Keyword: "gamma@example.com"})
	if total != 1 || got[0].ID != a3.ID {
		t.Fatalf("keyword 应命中 gamma: total=%d", total)
	}
	// keyword：user_id 命中。
	got, _ = s.QueryAccounts(AccountQuery{Keyword: "u-a"})
	if len(got) != 1 || got[0].ID != a1.ID {
		t.Fatalf("keyword 应按 user_id 命中 alpha: %+v", got)
	}

	// status 过滤。
	got, _ = s.QueryAccounts(AccountQuery{Statuses: []string{model.StatusDisabled}})
	if len(got) != 1 || got[0].ID != a2.ID {
		t.Fatalf("status=disabled 应只命中 a2: %+v", got)
	}
	// enabled 过滤。
	got, total = s.QueryAccounts(AccountQuery{Enabled: boolPtr(false)})
	if total != 1 || got[0].ID != a2.ID {
		t.Fatalf("enabled=false 应只命中 a2: %+v", got)
	}
	// mode 过滤。
	got, _ = s.QueryAccounts(AccountQuery{Mode: "apiKey"})
	if len(got) != 1 || got[0].ID != a3.ID {
		t.Fatalf("mode=apiKey 应只命中 a3: %+v", got)
	}

	// 模型可用性：flash 已耗尽的账号。
	got, _ = s.QueryAccounts(AccountQuery{Model: "GLM-5.3-Flash", ModelStatus: "exhausted"})
	if len(got) != 1 || got[0].ID != a4.ID {
		t.Fatalf("model exhausted 应命中 a4: %+v", got)
	}
	// flash 仍可用的账号（a4 之外）。
	_, total = s.QueryAccounts(AccountQuery{Model: "glm-5.3-flash", ModelStatus: "available"})
	if total != 3 {
		t.Fatalf("flash available 应命中 3 个，实际 %d", total)
	}

	// 分页：total 不随 limit/offset 变化。
	got, total = s.QueryAccounts(AccountQuery{Limit: 2, Offset: 1})
	if total != 4 || len(got) != 2 {
		t.Fatalf("分页应 total=4 len=2: total=%d len=%d", total, len(got))
	}
	if got, total := s.QueryAccounts(AccountQuery{Offset: 99}); total != 4 || len(got) != 0 {
		t.Fatalf("offset 超界应返回空列表 + total: total=%d len=%d", total, len(got))
	}

	// 排序。
	byName, _ := s.QueryAccounts(AccountQuery{Sort: "name"})
	if byName[0].Name != "alpha" || byName[3].Name != "gamma" {
		t.Fatalf("按名排序不符: %s..%s", byName[0].Name, byName[3].Name)
	}
}

func TestQueryAccountsArchivedAndProxy(t *testing.T) {
	s := newTestStore(t)
	a1, _, _ := s.AddAccountWithIdentity(model.ProviderZai, "arch-me", jwtFor("u-x"), "")
	a2, _, _ := s.AddAccountWithIdentity(model.ProviderZai, "stay", "plain-2", "")
	if _, err := s.SetArchived(model.ProviderZai, a1.ID, true); err != nil {
		t.Fatalf("归档失败: %v", err)
	}

	got, total := s.QueryAccounts(AccountQuery{Archived: "true"})
	if total != 1 || got[0].ID != a1.ID {
		t.Fatalf("archived=true 应只命中已归档: %+v", got)
	}
	got, total = s.QueryAccounts(AccountQuery{Archived: "false"})
	if total != 1 || got[0].ID != a2.ID {
		t.Fatalf("archived=false 应排除已归档: %+v", got)
	}
	if _, total = s.QueryAccounts(AccountQuery{Archived: "all"}); total != 2 {
		t.Fatalf("archived=all 应返回全部: %d", total)
	}
	if _, total = s.QueryAccounts(AccountQuery{}); total != 2 {
		t.Fatalf("缺省 archived 应与 all 相同（保持现状）: %d", total)
	}

	// proxy_id：a1（归档）与 a2 都未指派任何线路，均命中 __direct__。
	if _, err := s.SetProxyURL(model.ProviderZai, a2.ID, nil); err != nil {
		t.Fatalf("置直连失败: %v", err)
	}
	got, _ = s.QueryAccounts(AccountQuery{ProxyID: proxyFilterDirect})
	if len(got) != 2 {
		t.Fatalf("a1/a2 均未指派线路，__direct__ 应命中 2 个: %d", len(got))
	}
	found := false
	for _, a := range got {
		if a.ID == a2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("__direct__ 结果应包含 a2")
	}
}

func boolPtr(b bool) *bool { return &b }
