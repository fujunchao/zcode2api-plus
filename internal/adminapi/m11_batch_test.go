// M11 批次的回归守卫（上游 6da8df6 的伴生项）。
//
// 这一批与 async 入口归一化同属一个 6 合 1 提交，逐项核实过本地同样缺失：
//   - 批量/单个额度刷新不得包含已归档与已停用账号（刷新会把归档账号写回 active）；
//   - 领取与「刷新資格」的冷却闸门必须用同一判据（否则 preview 可查、claim 被拒）；
//   - 编辑账号必须先指派线路再套用其余字段（避免「回 500 但字段已生效」的半套用）；
//   - new 的 async 强制直连开关可读可写、非法值 400。
package adminapi

import (
	"net/http"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// TestRefreshSkipsArchivedAndDisabledAccounts 额度刷新必须跳过已归档与已停用账号。
//
// store.SetArchived 的契约是「调度、领取、刷新全部跳过」，而刷新会经
// handleBillingResponse 把归档账号的状态写回 active——那等于归档被一次刷新撤销。
// 本用例是缺口 6da8df6（M11 段）的回归守卫：退回「仅按 Mode==jwt 过滤」即红。
func TestRefreshSkipsArchivedAndDisabledAccounts(t *testing.T) {
	mux, st, _ := setup(t)

	// 三个 jwt 账号：入池后会各触发一次额度刷新，故先记下基线计数
	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "jwt.a.b\njwt.c.d\njwt.e.f",
	})
	if code != http.StatusOK || num(t, body["count"]) != 3 {
		t.Fatalf("添加三个 jwt 账号应成功: %d %v", code, body)
	}
	ids := body["ids"].([]any)
	archivedID, disabledID, okID := str(t, ids[0]), str(t, ids[1]), str(t, ids[2])

	// 归档第一个、停用第二个
	if code, _ := do(t, mux, st, http.MethodPost,
		"/admin/api/accounts/"+archivedID+"/archived", map[string]any{"archived": true}); code != http.StatusOK {
		t.Fatalf("归档应 200: %d", code)
	}
	if code, _ := do(t, mux, st, http.MethodPost,
		"/admin/api/accounts/"+disabledID+"/enabled", map[string]any{"enabled": false}); code != http.StatusOK {
		t.Fatalf("停用应 200: %d", code)
	}

	// 前置：两个账号确实满足了排除条件（否则用例可能空过）
	if acc := st.FindAny(archivedID); acc == nil || acc.ArchivedAt == nil {
		t.Fatal("前置条件不成立：账号应已归档")
	}
	if acc := st.FindAny(disabledID); acc == nil || acc.Status != model.StatusDisabled {
		t.Fatalf("前置条件不成立：账号应为停用状态，实际 %v", acc)
	}

	// 批量刷新 all=true：只剩未归档未停用的那一个
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/refresh", map[string]any{"all": true})
	if code != http.StatusOK {
		t.Fatalf("批量刷新应 200: %d", code)
	}
	if got := num(t, body["count"]); got != 1 {
		t.Fatalf("批量刷新应只覆盖 1 个账号（跳过已归档/已停用），实际 %v", got)
	}

	// 按 ids 显式刷新同样要过滤
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/refresh",
		map[string]any{"ids": []any{archivedID, disabledID, okID}})
	if code != http.StatusOK {
		t.Fatalf("按 ids 刷新应 200: %d", code)
	}
	if got := num(t, body["count"]); got != 1 {
		t.Fatalf("按 ids 刷新应只覆盖 1 个账号，实际 %v", got)
	}

	// 单个刷新归档账号：如实拒绝，且不得把它写回 active
	code, body = do(t, mux, st, http.MethodPost, "/admin/api/accounts/"+archivedID+"/refresh", nil)
	if code != http.StatusOK || body["ok"] != false {
		t.Fatalf("归档账号单个刷新应 200/false: %d %v", code, body)
	}
	if msg := str(t, body["message"]); msg != "账号已归档，不参与额度刷新" {
		t.Fatalf("归档账号应给出明确文案，实际 %q", msg)
	}
	if acc := st.FindAny(archivedID); acc.Status == model.StatusActive {
		t.Fatal("刷新不得把归档账号写回 active（会撤销归档）")
	}
}

// TestClaimBlockedByCooling 领取与「刷新資格」共用的冷却闸门判据。
//
// 只看原始 Status 会把「冷却已到期」的账号判成不可领，而同一时刻 preview 用
// IsSelectable 判成可查——用户看到 preview 有结果、点领取却说冷却中。
// 本用例是缺口 6da8df6（M11 段）的回归守卫：把判据退回 `acc.Status == cooling`
// 后「冷却已到期」那格即红。
func TestClaimBlockedByCooling(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	past := float64(now.Unix() - 60)
	future := float64(now.Unix() + 60)

	cases := []struct {
		name string
		acc  *model.Account
		want bool
	}{
		{
			name: "冷却已到期：应放行（与 preview 一致）",
			acc:  &model.Account{Enabled: true, Status: model.StatusCooling, CoolingUntil: &past},
			want: false,
		},
		{
			name: "冷却未到期：应拦住",
			acc:  &model.Account{Enabled: true, Status: model.StatusCooling, CoolingUntil: &future},
			want: true,
		},
		{
			name: "正常账号：不在本闸门管辖内",
			acc:  &model.Account{Enabled: true, Status: model.StatusActive},
			want: false,
		},
		{
			name: "失效账号：状态不是 cooling，交其他分支处理",
			acc:  &model.Account{Enabled: true, Status: model.StatusInvalid},
			want: false,
		},
		{
			name: "已归档账号：状态不是 cooling，交其他分支处理",
			acc: func() *model.Account {
				a := &model.Account{Enabled: true, Status: model.StatusActive}
				at := float64(now.Unix())
				a.ArchivedAt = &at
				return a
			}(),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claimBlockedByCooling(c.acc, now); got != c.want {
				t.Fatalf("claimBlockedByCooling=%v，期望 %v", got, c.want)
			}
		})
	}
}

// TestEditAccountRejectsUnknownProxyWithoutHalfApply 指派不存在的线路时必须整体失败，
// 不能出现「返回错误但 name 已生效」的半套用。
//
// ⚠️ 覆盖度说明：本用例钉住的是「拒绝语义」这一不变量。改动本身（把
// AssignProxyProfile 提到 EditAccount 之前）针对的是并发窗口——profile 在
// profileExists 预检与指派之间被删掉。那个窗口无法在单测里确定性复现（需要注入
// 钩子），所以反向验证不适用于本项：把顺序改回去、同时删掉预检，本用例才会红。
func TestEditAccountRejectsUnknownProxyWithoutHalfApply(t *testing.T) {
	mux, st, _ := setup(t)

	code, body := do(t, mux, st, http.MethodPost, "/admin/api/accounts", map[string]any{
		"tokens": "jwt.a.b",
	})
	if code != http.StatusOK {
		t.Fatalf("添加账号应成功: %d %v", code, body)
	}
	id := str(t, body["ids"].([]any)[0])
	before := st.FindAny(id)
	if before == nil {
		t.Fatal("账号应存在")
	}

	// 同时改名 + 指派一个不存在的线路：整个请求必须失败，且名字不得被改
	code, _ = do(t, mux, st, http.MethodPut, "/admin/api/accounts/"+id, map[string]any{
		"name":     "should-not-apply",
		"proxy_id": "proxy-does-not-exist",
	})
	if code == http.StatusOK {
		t.Fatalf("指派不存在的线路不该成功: %d", code)
	}
	after := st.FindAny(id)
	if after.Name != before.Name {
		t.Fatalf("整体失败时 name 不得被写入：%q → %q", before.Name, after.Name)
	}
	if after.ProxyID != nil || after.ProxyURL != nil {
		t.Fatalf("线路不得被写入: proxyID=%v proxyURL=%v", after.ProxyID, after.ProxyURL)
	}
}

// TestAsyncForceDirectSetting 新增的 async 强制直连开关：可读可写、非法值 400。
func TestAsyncForceDirectSetting(t *testing.T) {
	mux, st, _ := setup(t)

	code, body := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if code != http.StatusOK {
		t.Fatalf("读取设置应 200: %d", code)
	}
	if got, ok := body["async_force_direct"].(bool); !ok || got {
		t.Fatalf("默认应为 false: %#v", body["async_force_direct"])
	}

	if code, _ := do(t, mux, st, http.MethodPut, "/admin/api/settings",
		map[string]any{"async_force_direct": true}); code != http.StatusOK {
		t.Fatalf("写入 true 应 200: %d", code)
	}
	_, body = do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
	if got, _ := body["async_force_direct"].(bool); !got {
		t.Fatalf("写入后应回读 true: %#v", body["async_force_direct"])
	}
	if !st.AsyncForceDirect() {
		t.Fatal("store 访问器应读到 true")
	}

	// 非法值一律 400，且不得改动已存值
	for _, bad := range []any{"maybe", []any{1}, map[string]any{}} {
		code, _ = do(t, mux, st, http.MethodPut, "/admin/api/settings",
			map[string]any{"async_force_direct": bad})
		if code != http.StatusBadRequest {
			t.Fatalf("非法值 %#v 应 400: %d", bad, code)
		}
	}
	if !st.AsyncForceDirect() {
		t.Fatal("非法值不该改动已存值")
	}

	// 关回去
	if code, _ := do(t, mux, st, http.MethodPut, "/admin/api/settings",
		map[string]any{"async_force_direct": false}); code != http.StatusOK {
		t.Fatalf("写入 false 应 200: %d", code)
	}
	if st.AsyncForceDirect() {
		t.Fatal("应能关回 false")
	}
	_ = store.AsyncForceDirectKey // 键名常量在两处共用，改一处即编译失败
}
