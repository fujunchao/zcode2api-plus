package store

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// openAt 在指定路径打开存储（隔离环境变量，测试结束恢复并关闭）。
func openAt(t *testing.T, path string) *Store {
	t.Helper()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = path
	config.AdminKeyEnv = ""
	config.GatewayKeyEnv = ""
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})
	s, err := New()
	if err != nil {
		t.Fatalf("打开 %s 失败: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestStore(t *testing.T) *Store {
	return openAt(t, filepath.Join(t.TempDir(), "accounts.db"))
}

// jwtFor 构造 payload 含指定 user_id 的 JWT（形态与 model.JWTUserID 的解析一致）。
func jwtFor(userID string) string {
	enc := base64.RawURLEncoding.EncodeToString([]byte(`{"user_id":"` + userID + `"}`))
	return "header." + enc + ".sig"
}

func TestAddAccountDedupByUserIDAcrossTokenRefresh(t *testing.T) {
	// 同一个号重新登录时 token 字节会变，只比凭据会把一条记录变成两条。
	s := newTestStore(t)
	first, err := s.AddAccount(model.ProviderZai, "a1", jwtFor("u-1"))
	if err != nil {
		t.Fatalf("首次入池失败: %v", err)
	}
	if first.UserID == nil || *first.UserID != "u-1" {
		t.Fatalf("user_id 未从 secret 派生: %v", first.UserID)
	}

	// 同一 user_id、不同 token 字节。
	second, err := s.AddAccount(model.ProviderZai, "a1-again", jwtFor("u-1")+"x")
	if err != nil {
		t.Fatalf("重复入池失败: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("应命中既有账号: got %s want %s", second.ID, first.ID)
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("不应新建记录，实际 %d 条", got)
	}
}

func TestAddAccountDedupPrefersUserIDOverEmail(t *testing.T) {
	// 单遍遍历会按列表顺序命中靠前的低优先级判据（邮箱），把"同一账号换了 token"
	// 误判成另一个账号。三轮遍历必须让 user_id 先胜出。
	s := newTestStore(t)
	byEmail, _, err := s.AddAccountWithIdentity(model.ProviderZai, "by-email", "plain-key-1", "shared@example.com")
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if byEmail.UserID != nil {
		t.Fatalf("apiKey 模式不应有 user_id: %v", byEmail.UserID)
	}
	byUserID, err := s.AddAccount(model.ProviderZai, "by-user-id", jwtFor("u-2"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}

	// 同时匹配「靠前账号的邮箱」与「靠后账号的 user_id」。
	got, _, err := s.AddAccountWithIdentity(model.ProviderZai, "new", jwtFor("u-2"), "shared@example.com")
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if got.ID != byUserID.ID {
		t.Fatalf("user_id 应优先于 email: got %s want %s", got.ID, byUserID.ID)
	}
	if n := len(s.ListAccounts(model.ProviderZai)); n != 2 {
		t.Fatalf("不应新建记录，实际 %d 条", n)
	}
}

func TestAddAccountAssignsDistinctDeviceMid(t *testing.T) {
	s := newTestStore(t)
	a1, err := s.AddAccount(model.ProviderZai, "a1", jwtFor("u-a"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	a2, err := s.AddAccount(model.ProviderZai, "a2", jwtFor("u-b"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if a1.VirtualDeviceMid == nil || *a1.VirtualDeviceMid == "" {
		t.Fatal("新账号应分配设备指纹")
	}
	if a2.VirtualDeviceMid == nil || *a2.VirtualDeviceMid == "" {
		t.Fatal("新账号应分配设备指纹")
	}
	if *a1.VirtualDeviceMid == *a2.VirtualDeviceMid {
		t.Fatalf("不同账号不应共用设备指纹: %s", *a1.VirtualDeviceMid)
	}
}

func TestMigrationBackfillsIdentityAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)

	// 模拟改造前的旧数据：只有凭据，没有 user_id / virtual_device_mid 键。
	legacy := `{"id":"legacy-1","name":"old","provider":"zai","mode":"jwt",` +
		`"jwt_token":"` + jwtFor("u-legacy") + `","api_key":null,"email":null,` +
		`"enabled":true,"status":"active","created_at":1}`
	if _, err := s.db.Exec(
		`INSERT OR REPLACE INTO accounts (id, provider, name, mode, status, enabled, created_at, data)
		 VALUES (?,?,?,?,?,?,?,?)`,
		"legacy-1", model.ProviderZai, "old", "jwt", model.StatusActive, 1, 1.0, legacy); err != nil {
		t.Fatalf("写入旧数据失败: %v", err)
	}
	_ = s.Close()

	// 重开：迁移应回填两个字段。
	s2 := openAt(t, path)
	acc := s2.Find(model.ProviderZai, "legacy-1")
	if acc == nil {
		t.Fatal("旧账号在迁移后丢失")
	}
	if acc.UserID == nil || *acc.UserID != "u-legacy" {
		t.Fatalf("user_id 未回填: %v", acc.UserID)
	}
	if acc.VirtualDeviceMid == nil || *acc.VirtualDeviceMid == "" {
		t.Fatal("virtual_device_mid 未回填")
	}
	mid := *acc.VirtualDeviceMid
	_ = s2.Close()

	// 再开一次：迁移必须幂等，不能重新分配指纹。
	s3 := openAt(t, path)
	again := s3.Find(model.ProviderZai, "legacy-1")
	if again == nil || again.VirtualDeviceMid == nil {
		t.Fatal("账号在二次迁移后丢失")
	}
	if *again.VirtualDeviceMid != mid {
		t.Fatalf("迁移非幂等: %s -> %s", mid, *again.VirtualDeviceMid)
	}
}

func TestClaimSettingAccessors(t *testing.T) {
	s := newTestStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("SetSetting 失败: %v", err)
		}
	}

	// 键缺失 → 回退默认：入池默认开启，定时默认关闭（升级不该自发产生每日上游流量）。
	if !s.ClaimAutoEnabled() {
		t.Fatal("入池自动领取默认应开启")
	}
	if s.ClaimScheduleEnabled() {
		t.Fatal("定时领取默认应关闭")
	}
	if got := s.ClaimScheduleTime(); got != "23:00" {
		t.Fatalf("定时点默认应 23:00: %q", got)
	}
	captcha, retry, preview := s.ClaimCooldowns()
	if captcha != config.ClaimCaptchaCooldownSeconds || retry != config.ClaimRetryCooldownSeconds ||
		preview != config.ClaimPreviewCooldownSeconds {
		t.Fatalf("默认冷却应取 config: %d/%d/%d", captcha, retry, preview)
	}

	// 合法值生效。
	must(s.SetSetting("claim_auto_enabled", "false"))
	must(s.SetSetting("claim_schedule_enabled", "true"))
	must(s.SetSetting("claim_schedule_time", "07:30"))
	must(s.SetSetting("claim_captcha_cooldown", "120"))
	must(s.SetSetting("claim_preview_cooldown", "0"))
	if s.ClaimAutoEnabled() {
		t.Fatal("开关应已关闭")
	}
	if !s.ClaimScheduleEnabled() {
		t.Fatal("定时应已开启")
	}
	if got := s.ClaimScheduleTime(); got != "07:30" {
		t.Fatalf("定时点应 07:30: %q", got)
	}
	captcha, _, preview = s.ClaimCooldowns()
	if captcha != 120 || preview != 0 {
		t.Fatalf("冷却应生效: %d/%d/%d", captcha, retry, preview)
	}
	// 低于下限的值钳到下限（retry ≥ 30）。
	must(s.SetSetting("claim_retry_cooldown", "5"))
	if _, retry, _ = s.ClaimCooldowns(); retry != 30 {
		t.Fatalf("retry 应钳到下限 30: %d", retry)
	}

	// 非法值回退默认，而不是把配置锁死在坏值上。
	must(s.SetSetting("claim_schedule_time", "25:00"))
	if got := s.ClaimScheduleTime(); got != "23:00" {
		t.Fatalf("非法时间应回退 23:00: %q", got)
	}
	must(s.SetSetting("claim_auto_enabled", "maybe"))
	if !s.ClaimAutoEnabled() {
		t.Fatal("非法布尔应回退默认 true")
	}
	must(s.SetSetting("claim_captcha_cooldown", "abc"))
	if c, _, _ := s.ClaimCooldowns(); c != config.ClaimCaptchaCooldownSeconds {
		t.Fatalf("非法数字应回退 config 默认: %d", c)
	}
	// on/off 这类写法也要能认。
	must(s.SetSetting("claim_auto_enabled", "off"))
	if s.ClaimAutoEnabled() {
		t.Fatal("off 应视为关闭")
	}
}

func TestBootstrapGeneratesKeys(t *testing.T) {
	s := newTestStore(t)
	if s.AdminKey() == "" || s.AdminKey() == "zcode" {
		t.Fatalf("应随机生成后台密码: %q", s.AdminKey())
	}
	if s.GeneratedAdminKey != s.AdminKey() {
		t.Fatalf("生成标记应记录: %q vs %q", s.GeneratedAdminKey, s.AdminKey())
	}
	if !strings.HasPrefix(s.GatewayKey(), "sk-") {
		t.Fatalf("网关密钥应 sk- 前缀: %q", s.GatewayKey())
	}
	if s.GeneratedGatewayKey != s.GatewayKey() {
		t.Fatalf("网关生成标记应记录")
	}
}

func TestLegacyAdminKeyRotated(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("admin_key", "zcode"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("gateway_key", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := New()
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if s2.AdminKey() == "" || s2.AdminKey() == "zcode" {
		t.Fatalf("历史默认密码应强制轮换: %q", s2.AdminKey())
	}
	if s2.GeneratedAdminKey != s2.AdminKey() {
		t.Fatal("轮换应标记为随机生成")
	}
	if s2.GatewayKey() == "" || s2.GeneratedGatewayKey != s2.GatewayKey() {
		t.Fatal("空网关密钥应重新生成")
	}
}

func TestEnvKeysRespected(t *testing.T) {
	dir := t.TempDir()
	oldDB, oldAdmin, oldGW := config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv
	config.DBPath = filepath.Join(dir, "accounts.db")
	config.AdminKeyEnv = "env-admin-key"
	config.GatewayKeyEnv = "sk-env-gateway"
	t.Cleanup(func() {
		config.DBPath, config.AdminKeyEnv, config.GatewayKeyEnv = oldDB, oldAdmin, oldGW
	})

	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if s.AdminKey() != "env-admin-key" || s.GeneratedAdminKey != "" {
		t.Fatalf("环境变量应生效且不标记生成: %q %q", s.AdminKey(), s.GeneratedAdminKey)
	}
	if s.GatewayKey() != "sk-env-gateway" || s.GeneratedGatewayKey != "" {
		t.Fatalf("网关环境变量应生效: %q %q", s.GatewayKey(), s.GeneratedGatewayKey)
	}
}

func TestCustomKeysPreservedAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("admin_key", "my-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting("gateway_key", "sk-my-gw"); err != nil {
		t.Fatal(err)
	}
	path := config.DBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 在同一路径重开：自定义密钥不受影响，也不触发轮换
	oldDB := config.DBPath
	config.DBPath = path
	t.Cleanup(func() { config.DBPath = oldDB })
	s2, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if s2.AdminKey() != "my-secret" || s2.GatewayKey() != "sk-my-gw" {
		t.Fatalf("自定义密钥应保留: %q %q", s2.AdminKey(), s2.GatewayKey())
	}
	if s2.GeneratedAdminKey != "" || s2.GeneratedGatewayKey != "" {
		t.Fatal("不应重新生成")
	}
}

func TestAccountCRUDAndDedup(t *testing.T) {
	s := newTestStore(t)
	acc1, err := s.AddAccount(model.ProviderZai, "main", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	acc2, err := s.AddAccount(model.ProviderZai, "dup", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	if acc1.ID != acc2.ID {
		t.Fatal("重复 token 应返回既有账号")
	}
	if got := len(s.ListAccounts(model.ProviderZai)); got != 1 {
		t.Fatalf("应只有 1 个账号: %d", got)
	}
	if _, err := s.AddAccount("other", "x", "k"); err == nil {
		t.Fatal("未知 provider 应报错")
	}

	if ok, err := s.RemoveAccount(model.ProviderZai, "main"); !ok || err != nil {
		t.Fatalf("删除失败: %v %v", ok, err)
	}
	if ok, _ := s.RemoveAccount(model.ProviderZai, "main"); ok {
		t.Fatal("重复删除应返回 false")
	}
}

func TestSetEnabled(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "acc", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetEnabled(model.ProviderZai, acc.ID, false); !ok || err != nil {
		t.Fatalf("禁用失败: %v %v", ok, err)
	}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("禁用后状态错误: %+v", got)
	}
	if ok, err := s.SetEnabled(model.ProviderZai, acc.ID, true); !ok || err != nil {
		t.Fatalf("启用失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.Status != model.StatusActive || !got.Enabled {
		t.Fatalf("启用后状态错误: %+v", got)
	}
}

// 字段级写入：只替换补丁点名的字段、其余保持不动，并且一定落库（用 Find 复查）。
// 这些方法是「锁外改字段 → UpdateAccount」的替代形态，语义必须逐项对齐。
func TestFieldLevelMutators(t *testing.T) {
	s := newTestStore(t)
	lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	acc, err := s.AddAccount(model.ProviderZai, "acc-1", jwtFor("uid-mut-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if ok, err := s.AssignProxyProfile(acc.ID, lineA.ID); !ok || err != nil {
		t.Fatalf("指派失败: %v %v", ok, err)
	}

	t.Run("EditAccount 换名称与凭据并恢复可用", func(t *testing.T) {
		if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
			a.Status = model.StatusInvalid
			msg := "旧错误"
			a.LastError = &msg
		}); err != nil {
			t.Fatalf("准备现场失败: %v", err)
		}
		name := "改过的名字"
		ok, err := s.EditAccount(model.ProviderZai, acc.ID, AccountEdit{
			Name: &name, SetSecret: true, Secret: "x.y.z", SecretMode: "jwt",
		})
		if err != nil || !ok {
			t.Fatalf("编辑失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.Name != name || got.Mode != "jwt" || got.JWTToken == nil || *got.JWTToken != "x.y.z" {
			t.Fatalf("名称/凭据未替换: %+v", got)
		}
		if got.APIKey != nil {
			t.Fatalf("切到 jwt 后应清掉 APIKey: %v", got.APIKey)
		}
		if got.Status != model.StatusActive || got.LastError != nil {
			t.Fatalf("换凭据应恢复 active 并清错误: %s %v", got.Status, got.LastError)
		}
		// 补丁未点名的字段必须原样保留。
		if got.ProxyID == nil || *got.ProxyID != lineA.ID {
			t.Fatalf("未提及的线路指派不应被动到: %v", got.ProxyID)
		}
	})

	t.Run("EditAccount 空补丁不动任何字段", func(t *testing.T) {
		before := s.Find(model.ProviderZai, acc.ID)
		beforeName, beforeProxy, beforeMode := before.Name, before.ProxyURL, before.Mode
		ok, err := s.EditAccount(model.ProviderZai, acc.ID, AccountEdit{})
		if err != nil || !ok {
			t.Fatalf("编辑失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.Name != beforeName || got.Mode != beforeMode || !sameStringPtr(got.ProxyURL, beforeProxy) {
			t.Fatalf("空补丁不应改动任何字段: %+v", got)
		}
	})

	t.Run("EditAccount 代理地址没变则保留线路指派", func(t *testing.T) {
		ok, err := s.EditAccount(model.ProviderZai, acc.ID, AccountEdit{SetProxyURL: true, ProxyURL: &lineA.URL})
		if err != nil || !ok {
			t.Fatalf("编辑失败: %v %v", ok, err)
		}
		if got := s.Find(model.ProviderZai, acc.ID); got.ProxyID == nil {
			t.Fatal("地址没变时不应解除线路指派")
		}
	})

	t.Run("EditAccount 改地址即解除线路指派", func(t *testing.T) {
		manual := "http://9.9.9.9:9999"
		ok, err := s.EditAccount(model.ProviderZai, acc.ID, AccountEdit{SetProxyURL: true, ProxyURL: &manual})
		if err != nil || !ok {
			t.Fatalf("编辑失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.ProxyID != nil {
			t.Fatalf("改为手工代理应解除指派: %v", got.ProxyID)
		}
		if got.ProxyURL == nil || *got.ProxyURL != manual {
			t.Fatalf("出站地址未替换: %v", got.ProxyURL)
		}
	})

	t.Run("SetProxyURL 置地址并解除指派", func(t *testing.T) {
		if ok, err := s.AssignProxyProfile(acc.ID, lineA.ID); !ok || err != nil {
			t.Fatalf("重新指派失败: %v %v", ok, err)
		}
		manual := "http://8.8.8.8:8080"
		if ok, err := s.SetProxyURL(model.ProviderZai, acc.ID, &manual); !ok || err != nil {
			t.Fatalf("写入失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.ProxyID != nil || got.ProxyURL == nil || *got.ProxyURL != manual {
			t.Fatalf("应置地址并解除指派: id=%v url=%v", got.ProxyID, got.ProxyURL)
		}
		// nil 表示改为直连。
		if ok, err := s.SetProxyURL(model.ProviderZai, acc.ID, nil); !ok || err != nil {
			t.Fatalf("清空失败: %v %v", ok, err)
		}
		if got := s.Find(model.ProviderZai, acc.ID); got.ProxyURL != nil {
			t.Fatalf("nil 应表示直连: %v", got.ProxyURL)
		}
	})

	t.Run("SetIdentity 只写传入的字段", func(t *testing.T) {
		email := "someone@example.com"
		if ok, err := s.SetIdentity(model.ProviderZai, acc.ID, &email, nil); !ok || err != nil {
			t.Fatalf("写入失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.Email == nil || *got.Email != email {
			t.Fatalf("邮箱未写入: %v", got.Email)
		}
		if got.Name != "改过的名字" {
			t.Fatalf("未传 name 时不应改名字: %q", got.Name)
		}
		name := "oauth-login"
		if ok, err := s.SetIdentity(model.ProviderZai, acc.ID, nil, &name); !ok || err != nil {
			t.Fatalf("写入失败: %v %v", ok, err)
		}
		got = s.Find(model.ProviderZai, acc.ID)
		if got.Name != name || got.Email == nil || *got.Email != email {
			t.Fatalf("未传 email 时不应动邮箱: %q %v", got.Name, got.Email)
		}
	})

	t.Run("SetAPIKey 与 SetDisabledModels", func(t *testing.T) {
		if ok, err := s.SetAPIKey(model.ProviderZai, acc.ID, "sk-secret"); !ok || err != nil {
			t.Fatalf("写入失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.APIKey == nil || *got.APIKey != "sk-secret" {
			t.Fatalf("APIKey 未写入: %v", got.APIKey)
		}
		if ok, err := s.SetDisabledModels(model.ProviderZai, acc.ID, []string{"GLM-5.3", "glm-5.3", ""}); !ok || err != nil {
			t.Fatalf("写入失败: %v %v", ok, err)
		}
		got = s.Find(model.ProviderZai, acc.ID)
		if len(got.DisabledModels) != 1 || got.DisabledModels[0] != "glm-5.3" {
			t.Fatalf("应归一化并去重: %v", got.DisabledModels)
		}
	})

	t.Run("ResetTokenStats 清零累计用量", func(t *testing.T) {
		if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
			a.TotalInputTokens, a.TotalOutputTokens = 11, 22
			a.TotalCacheCreationTokens, a.TotalCacheReadTokens = 33, 44
		}); err != nil {
			t.Fatalf("准备现场失败: %v", err)
		}
		if ok, err := s.ResetTokenStats(model.ProviderZai, acc.ID); !ok || err != nil {
			t.Fatalf("重置失败: %v %v", ok, err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.TotalInputTokens != 0 || got.TotalOutputTokens != 0 ||
			got.TotalCacheCreationTokens != 0 || got.TotalCacheReadTokens != 0 {
			t.Fatalf("累计用量未清零: %+v", got)
		}
	})

	t.Run("账号不存在时返回 false 且不报错", func(t *testing.T) {
		edit := AccountEdit{}
		for name, call := range map[string]func() (bool, error){
			"EditAccount":       func() (bool, error) { return s.EditAccount(model.ProviderZai, "no-such", edit) },
			"SetProxyURL":       func() (bool, error) { return s.SetProxyURL(model.ProviderZai, "no-such", nil) },
			"SetIdentity":       func() (bool, error) { return s.SetIdentity(model.ProviderZai, "no-such", nil, nil) },
			"SetAPIKey":         func() (bool, error) { return s.SetAPIKey(model.ProviderZai, "no-such", "k") },
			"SetDisabledModels": func() (bool, error) { return s.SetDisabledModels(model.ProviderZai, "no-such", nil) },
			"ResetTokenStats":   func() (bool, error) { return s.ResetTokenStats(model.ProviderZai, "no-such") },
		} {
			ok, err := call()
			if ok || err != nil {
				t.Fatalf("%s 对不存在的账号应返回 false,nil: %v %v", name, ok, err)
			}
		}
	})
}

// 读 API 必须交出**副本**：账号对象不再被多个 goroutine 无锁共享，改副本
// （含内层 map）都不得影响 store。这是「无锁读 vs 加锁写仍是竞态」的正面防线。
func TestReadAPIsReturnDetachedCopies(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "acc-1", jwtFor("uid-copy-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if _, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.Quota["glm-5.3"] = map[string]any{"remaining": 10.0}
	}); err != nil {
		t.Fatalf("准备现场失败: %v", err)
	}

	t.Run("Find 返回副本", func(t *testing.T) {
		got := s.Find(model.ProviderZai, acc.ID)
		got.Name = "改过的名字"
		got.Status = model.StatusInvalid
		got.Quota["glm-5.3"]["remaining"] = 0.0
		got.Quota["新模型"] = map[string]any{"remaining": 1.0}

		again := s.Find(model.ProviderZai, acc.ID)
		if again.Name != "acc-1" || again.Status != model.StatusActive {
			t.Fatalf("改副本不应影响 store: %+v", again)
		}
		if v := again.Quota["glm-5.3"]["remaining"]; v != 10.0 {
			t.Fatalf("内层 map 应深拷贝: %v", v)
		}
		if _, ok := again.Quota["新模型"]; ok {
			t.Fatal("给副本加键不应影响 store")
		}
	})

	t.Run("ListAccounts 返回副本", func(t *testing.T) {
		list := s.ListAccounts(model.ProviderZai)
		if len(list) != 1 {
			t.Fatalf("应有 1 个账号: %d", len(list))
		}
		list[0].Name = "列表改的名字"
		if got := s.Find(model.ProviderZai, acc.ID); got.Name != "acc-1" {
			t.Fatalf("改列表元素不应影响 store: %q", got.Name)
		}
	})

	t.Run("Select 返回副本", func(t *testing.T) {
		sel := s.Select(model.ProviderZai, map[string]bool{}, "")
		if sel == nil {
			t.Fatal("应选中账号")
		}
		sel.Name = "选择改的名字"
		sel.FailCount = 99
		got := s.Find(model.ProviderZai, acc.ID)
		if got.Name != "acc-1" || got.FailCount != 0 {
			t.Fatalf("改 Select 结果不应影响 store: %q %d", got.Name, got.FailCount)
		}
	})

	// AddAccount* 刻意仍返回内部对象：登录链路依赖「AutoAssignProxies 改的就是
	// 同一个对象，读 account.ProxyURL 立刻看到新线路」。这条不对称必须钉住，
	// 否则将来有人"顺手统一成副本"会静默破坏登录选线路。
	t.Run("AddAccount 仍返回内部对象", func(t *testing.T) {
		fresh, err := s.AddAccount(model.ProviderZai, "acc-2", jwtFor("uid-copy-2"))
		if err != nil {
			t.Fatalf("入池失败: %v", err)
		}
		manual := "http://7.7.7.7:8080"
		if _, err := s.SetProxyURL(model.ProviderZai, fresh.ID, &manual); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		if fresh.ProxyURL == nil || *fresh.ProxyURL != manual {
			t.Fatalf("AddAccount 返回的应是 store 内部对象: %v", fresh.ProxyURL)
		}
	})
}

func TestSelectRotationAndModelFilter(t *testing.T) {
	s := newTestStore(t)
	a1, _ := s.AddAccount(model.ProviderZai, "a1", "h1.p.s3")
	a2, _ := s.AddAccount(model.ProviderZai, "a2", "h2.p.s3")
	for _, a := range []*model.Account{a1, a2} {
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"remaining": float64(10), "model": "GLM-5.3"},
		}
	}

	first := s.Select("zai", nil, "GLM-5.3")
	second := s.Select("zai", nil, "GLM-5.3")
	if first == nil || second == nil || first.ID == second.ID {
		t.Fatalf("两个可用账号应轮询交替: %v %v", first, second)
	}

	// skip 已试过的账号
	third := s.Select("zai", map[string]bool{first.ID: true, second.ID: true}, "GLM-5.3")
	if third != nil {
		t.Fatalf("全部跳过后应无可用账号: %v", third)
	}

	// 无快照的账号作为 unknown 后备
	a3, _ := s.AddAccount(model.ProviderZai, "a3", "h3.p.s3")
	got := s.Select("zai", map[string]bool{first.ID: true, second.ID: true}, "GLM-5.3")
	if got == nil || got.ID != a3.ID {
		t.Fatalf("available 空时应回退 unknown: %v", got)
	}

	// 模型额度耗尽的账号被排除
	a2.Quota["GLM-5.3"]["remaining"] = float64(0)
	got = s.Select("zai", nil, "GLM-5.3")
	if got == nil || got.ID == a2.ID {
		t.Fatalf("耗尽账号不应被选中: %v", got)
	}
	// 且耗尽账号不在 available 池 → available=[a1]；skip a1 后回退 unknown [a3]
	got = s.Select("zai", map[string]bool{a1.ID: true}, "GLM-5.3")
	if got == nil || got.ID != a3.ID {
		t.Fatalf("skip available 后应回退 unknown: %v", got)
	}
}

func TestSelectPromoAccountsFirst(t *testing.T) {
	s := newTestStore(t)
	promo1, _ := s.AddAccount(model.ProviderZai, "promo1", "h1.p.s3")
	promo2, _ := s.AddAccount(model.ProviderZai, "promo2", "h2.p.s3")
	plain, _ := s.AddAccount(model.ProviderZai, "plain", "h3.p.s3")
	// promo1/2 持有未耗尽的一次性优惠；plain 只有每日额度。
	for _, a := range []*model.Account{promo1, promo2} {
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"remaining": float64(500), "period": "one_time", "model": "GLM-5.3"},
		}
	}
	plain.Quota = map[string]map[string]any{
		"GLM-5.3": {"remaining": float64(10), "period": "daily", "model": "GLM-5.3"},
	}

	// 优惠组内轮询，绝不落到 plain。
	seen := map[string]bool{}
	for range 6 {
		acc := s.Select("zai", nil, "GLM-5.3")
		if acc == nil {
			t.Fatal("应选到账号")
		}
		if acc.ID == plain.ID {
			t.Fatal("优惠组未耗尽时不应选中普通账号")
		}
		seen[acc.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("优惠组内应两个账号轮询: %v", seen)
	}

	// 优惠全部耗尽 → 回落普通账号。
	promo1.Quota["GLM-5.3"]["remaining"] = float64(0)
	promo2.Quota["GLM-5.3"]["remaining"] = float64(0)
	if acc := s.Select("zai", nil, "GLM-5.3"); acc == nil || acc.ID != plain.ID {
		t.Fatalf("优惠耗尽后应选中普通账号: %v", acc)
	}
}

// 额度优先调度：剩余可用额度最多的账号先服务，额度并列时才轮询。
func TestSelectPrefersMostRemainingQuota(t *testing.T) {
	s := newTestStore(t)
	small, _ := s.AddAccount(model.ProviderZai, "small", "h1.p.s3")
	mid, _ := s.AddAccount(model.ProviderZai, "mid", "h2.p.s3")
	big, _ := s.AddAccount(model.ProviderZai, "big", "h3.p.s3")
	setRemaining := func(a *model.Account, v float64) {
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"model": "GLM-5.3", "remaining": v},
		}
	}
	setRemaining(small, 100)
	setRemaining(mid, 5_000)
	setRemaining(big, 1_000_000)

	// big 的额度严格最多 → 一直选它，不轮询到额度更少的账号。
	for i := range 5 {
		acc := s.Select("zai", nil, "GLM-5.3")
		if acc == nil || acc.ID != big.ID {
			t.Fatalf("第 %d 次应优先选中额度最多的账号: %v", i, acc)
		}
	}

	// big 降到与 mid 并列 → 两者轮询交替；small 仍靠后。
	setRemaining(big, 5_000)
	seen := map[string]bool{}
	for range 6 {
		acc := s.Select("zai", nil, "GLM-5.3")
		if acc == nil {
			t.Fatal("应选到账号")
		}
		if acc.ID == small.ID {
			t.Fatal("并列组未耗尽时不应选中额度更少的账号")
		}
		seen[acc.ID] = true
	}
	if !seen[big.ID] || !seen[mid.ID] {
		t.Fatalf("额度并列时应轮询交替: %v", seen)
	}

	// big 继续降到低于 mid → 最大值转移到 mid。
	setRemaining(big, 4_000)
	if acc := s.Select("zai", nil, "GLM-5.3"); acc == nil || acc.ID != mid.ID {
		t.Fatalf("应跟随额度排序转移到 mid: %v", acc)
	}
}

// 同规格账号（额度完全相同）是最常见的生产形态，行为必须与原来的纯轮询一致：
// 并列组就是全池，游标逐个轮转。
func TestSelectEqualQuotaStillRotates(t *testing.T) {
	s := newTestStore(t)
	ids := map[string]bool{}
	for _, name := range []string{"a1", "a2", "a3"} {
		a, _ := s.AddAccount(model.ProviderZai, name, "h"+name+".p.s3")
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"model": "GLM-5.3", "remaining": float64(5_000_000)},
		}
		ids[a.ID] = true
	}
	seen := map[string]bool{}
	for range 3 {
		acc := s.Select("zai", nil, "GLM-5.3")
		if acc == nil || !ids[acc.ID] {
			t.Fatalf("应选中池内账号: %v", acc)
		}
		seen[acc.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("额度完全相同的三个账号应各被选中一次: %v", seen)
	}
}

// 有额度数值的账号优先于「尚无快照」的账号；后者彼此之间仍轮询。
// 「没有数值」不能被当成「额度 0」——0 会被 ModelAvailability 判成耗尽而排除，
// 两者在排序里必须落在不同位置。
func TestSelectQuotaNumbersRankAboveUnknown(t *testing.T) {
	s := newTestStore(t)
	unknownIDs := []string{}
	for _, name := range []string{"u1", "u2"} {
		a, _ := s.AddAccount(model.ProviderZai, name, "h"+name+".p.s3")
		unknownIDs = append(unknownIDs, a.ID)
	}
	known, _ := s.AddAccount(model.ProviderZai, "known", "hk.p.s3")
	known.Quota = map[string]map[string]any{
		"GLM-5.3": {"model": "GLM-5.3", "remaining": float64(7)},
	}

	for range 3 {
		if acc := s.Select("zai", nil, "GLM-5.3"); acc == nil || acc.ID != known.ID {
			t.Fatalf("有额度数值的账号应优先于无数值者: %v", acc)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		acc := s.Select("zai", map[string]bool{known.ID: true}, "GLM-5.3")
		if acc == nil {
			t.Fatal("有数值者被跳过后应回退到无数值账号")
		}
		seen[acc.ID] = true
	}
	for _, id := range unknownIDs {
		if !seen[id] {
			t.Fatalf("无数值账号之间应轮询: %v", seen)
		}
	}
}

// 不指定模型时没有额度可比较，全池并列 → 退化为纯轮询（"*" 游标键的既有语义）。
func TestSelectWithoutModelRotates(t *testing.T) {
	s := newTestStore(t)
	ids := map[string]bool{}
	for _, name := range []string{"a1", "a2"} {
		a, _ := s.AddAccount(model.ProviderZai, name, "h"+name+".p.s3")
		a.Quota = map[string]map[string]any{
			"GLM-5.3": {"model": "GLM-5.3", "remaining": float64(100)},
		}
		ids[a.ID] = true
	}
	seen := map[string]bool{}
	for range 2 {
		acc := s.Select("zai", nil, "")
		if acc == nil || !ids[acc.ID] {
			t.Fatalf("不指定模型时应轮询全池: %v", acc)
		}
		seen[acc.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("应轮询到两个账号: %v", seen)
	}
}

func TestUpdateDeletedAccountRejected(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(model.ProviderZai, "ghost", "h.p.s3")
	if ok, _ := s.RemoveAccount(model.ProviderZai, acc.ID); !ok {
		t.Fatal("删除失败")
	}
	// 模拟后台流长期持有旧对象、删除后回写：必须被拒绝（不命中）而非复活
	if ok, err := s.Update(model.ProviderZai, acc.ID, func(a *model.Account) {
		a.UseCount = 999
	}); ok || err != nil {
		t.Fatalf("已删除账号的写入应不命中且不报错: %v %v", ok, err)
	}
	if s.Find(model.ProviderZai, acc.ID) != nil {
		t.Fatal("已删除账号不得复活")
	}
}

func TestArchiveForcesDisabled(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(model.ProviderZai, "arc", "h.p.s3")
	if ok, err := s.SetArchived(model.ProviderZai, acc.ID, true); !ok || err != nil {
		t.Fatalf("归档失败: %v %v", ok, err)
	}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.ArchivedAt == nil || got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("归档应强制停用: archived=%v status=%s enabled=%v", got.ArchivedAt, got.Status, got.Enabled)
	}
	if got.IsSelectable(time.Now()) {
		t.Fatal("归档账号不可被调度")
	}

	// 恢复后仍是停用状态，需手动启用
	if ok, err := s.SetArchived(model.ProviderZai, acc.ID, false); !ok || err != nil {
		t.Fatalf("恢复失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ArchivedAt != nil || got.Status != model.StatusDisabled || got.Enabled {
		t.Fatalf("恢复后应保持停用: archived=%v status=%s enabled=%v", got.ArchivedAt, got.Status, got.Enabled)
	}
	if ok, err := s.SetEnabled(model.ProviderZai, acc.ID, true); !ok || err != nil {
		t.Fatalf("启用失败: %v %v", ok, err)
	}
	if got = s.Find(model.ProviderZai, acc.ID); got.Status != model.StatusActive {
		t.Fatalf("手动启用后应恢复 active: %s", got.Status)
	}
}

func TestExportImportRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s1 := openAt(t, filepath.Join(dir, "a.db"))
	acc, err := s1.AddAccount(model.ProviderZai, "exp", "header.payload.sig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.SetDisabledModels(model.ProviderZai, acc.ID, []string{"glm-4.7"}); err != nil {
		t.Fatal(err)
	}
	payload := s1.Export()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Version != 1 {
		t.Fatalf("导出版本应为 1")
	}

	s2 := openAt(t, filepath.Join(dir, "b.db"))
	var parsed ImportPayload
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	count, newIDs, err := s2.ImportAccounts(parsed)
	if err != nil || count != 1 {
		t.Fatalf("导入失败: count=%d err=%v", count, err)
	}
	if len(newIDs) != 1 {
		t.Fatalf("应返回 1 个新建账号 ID，实际 %d", len(newIDs))
	}
	got := s2.Find(model.ProviderZai, "exp")
	if got == nil || got.Secret() != "header.payload.sig" {
		t.Fatalf("导入账号凭证不符: %v", got)
	}
	if len(got.DisabledModels) != 1 || got.DisabledModels[0] != "glm-4.7" {
		t.Fatalf("停用模型应随导入保留: %v", got.DisabledModels)
	}
}

func TestProxyProfiles(t *testing.T) {
	s := newTestStore(t)
	p, err := s.AddProxyProfile("", "socks5://127.0.0.1:1080", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "代理-1" || p.URL != "socks5://127.0.0.1:1080" {
		t.Fatalf("默认命名/URL 不符: %+v", p)
	}
	if _, err := s.AddProxyProfile("代理-1", "http://x:1", true); err == nil {
		t.Fatal("重名应报错")
	}
	if _, err := s.AddProxyProfile("bad", "ftp://x:1", true); err == nil {
		t.Fatal("非法协议应报错")
	}

	up, err := s.UpdateProxyProfile(p.ID, "line-2", "http://1.2.3.4:8080", true)
	if err != nil || up.Name != "line-2" || up.URL != "http://1.2.3.4:8080" {
		t.Fatalf("更新失败: %+v %v", up, err)
	}

	acc, _ := s.AddAccount(model.ProviderZai, "acc", "header.payload.sig")
	if ok, err := s.AssignProxyProfile(acc.ID, p.ID); !ok || err != nil {
		t.Fatalf("指派失败: %v %v", ok, err)
	}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.ProxyID == nil || *got.ProxyID != p.ID || got.ProxyURL == nil || *got.ProxyURL != "http://1.2.3.4:8080" {
		t.Fatalf("账号代理指派不符: %+v", got)
	}

	// 更新线路 URL 应同步到已指派账号
	if _, err := s.UpdateProxyProfile(p.ID, "", "http://5.6.7.8:9090", true); err != nil {
		t.Fatal(err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ProxyURL == nil || *got.ProxyURL != "http://5.6.7.8:9090" {
		t.Fatalf("线路更新应同步账号: %v", got.ProxyURL)
	}

	// 此时没有别的空閒线路，删掉唯一一条线后账号只能退回直连。
	if ok, _, err := s.DeleteProxyProfile(p.ID); !ok || err != nil {
		t.Fatalf("删除失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ProxyID != nil || got.ProxyURL != nil {
		t.Fatalf("删除线路后账号应恢复直连: %+v", got)
	}

	if ok, err := s.AssignProxyProfile("nonexistent", ""); ok || err != nil {
		t.Fatalf("账号不存在应返回 false,nil: %v %v", ok, err)
	}
	if _, err := s.AssignProxyProfile(acc.ID, "proxy-nope"); err == nil {
		t.Fatal("指派不存在的线路应报错")
	}
}

// 删除线路时，原绑定账号应自动改派到其它空閒线路；确实没有空閒线路才退回直连。
func TestDeleteProxyReassigns(t *testing.T) {
	s := newTestStore(t)
	lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	lineB, err := s.AddProxyProfile("line-b", "socks5://2.2.2.2:1080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	// 停用的线路不能作为补位候选。
	if _, err := s.AddProxyProfile("line-off", "http://3.3.3.3:8080", false); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	acc, err := s.AddAccount(model.ProviderZai, "acc-1", jwtFor("uid-del-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if ok, err := s.AssignProxyProfile(acc.ID, lineA.ID); !ok || err != nil {
		t.Fatalf("指派失败: %v %v", ok, err)
	}

	t.Run("有空閒线路则改派", func(t *testing.T) {
		ok, reassign, err := s.DeleteProxyProfile(lineA.ID)
		if err != nil || !ok {
			t.Fatalf("删除失败: %v %v", ok, err)
		}
		if reassign.Assigned[acc.ID] != lineB.ID || len(reassign.Direct) != 0 {
			t.Fatalf("应改派到空閒的 line-b: %+v", reassign)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.ProxyID == nil || *got.ProxyID != lineB.ID {
			t.Fatalf("账号应指向新线路: %v", got.ProxyID)
		}
		if got.ProxyURL == nil || *got.ProxyURL != lineB.URL {
			t.Fatalf("出站地址应同步为新线路: %v", got.ProxyURL)
		}
		// 被删线路不应残留，否则下次还能被分配。
		for _, p := range s.ListProxyProfiles() {
			if p.ID == lineA.ID {
				t.Fatalf("被删线路仍在线路表里: %+v", p)
			}
		}
	})

	t.Run("无空閒线路则退回直连且清掉旧地址", func(t *testing.T) {
		// 此刻只剩停用的 line-off，没有可补的线路。
		ok, reassign, err := s.DeleteProxyProfile(lineB.ID)
		if err != nil || !ok {
			t.Fatalf("删除失败: %v %v", ok, err)
		}
		if len(reassign.Assigned) != 0 || len(reassign.Direct) != 1 || reassign.Direct[0] != acc.ID {
			t.Fatalf("应记为退回直连: %+v", reassign)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.ProxyID != nil || got.ProxyURL != nil {
			t.Fatalf("退回直连后不应残留线路与地址: %+v", got)
		}
	})

	t.Run("线路不存在返回 false", func(t *testing.T) {
		if ok, _, err := s.DeleteProxyProfile("no-such-line"); ok || err != nil {
			t.Fatalf("应返回 false,nil: %v %v", ok, err)
		}
	})
}

// 批量清理：先把整批线路摘除、再统一改派；改派优先空閒线路，没有空閒则选
// 绑定账号数最少的（并列取线路表顺序），连候选都没有才退回直连。
func TestPurgeProxyProfiles(t *testing.T) {
	// mustAssign 建号并绑到指定线路（store 顺序即入池顺序，改派结果依赖它）。
	mustAssign := func(s *Store, name, uid, profileID string) string {
		t.Helper()
		acc, err := s.AddAccount(model.ProviderZai, name, jwtFor(uid))
		if err != nil {
			t.Fatalf("入池失败: %v", err)
		}
		if ok, err := s.AssignProxyProfile(acc.ID, profileID); !ok || err != nil {
			t.Fatalf("指派失败: %v %v", ok, err)
		}
		return acc.ID
	}

	t.Run("同批待删线路不会被补位、优先空閒线路", func(t *testing.T) {
		s := newTestStore(t)
		lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		// line-b 空閒但同批待删——如果实现是「逐条删除」，line-a 的账号会被
		// 补位到这条马上也要消失的线路上。
		lineB, err := s.AddProxyProfile("line-b", "http://2.2.2.2:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		lineC, err := s.AddProxyProfile("line-c", "http://3.3.3.3:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		accID := mustAssign(s, "acc-1", "uid-purge-1", lineA.ID)

		purged, reassign, err := s.PurgeProxyProfiles([]string{lineA.ID, lineB.ID})
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(purged) != 2 {
			t.Fatalf("应移除 2 条: %v", purged)
		}
		if reassign.Assigned[accID] != lineC.ID || len(reassign.Direct) != 0 {
			t.Fatalf("应改派到幸存的 line-c（而不是同批待删的 line-b）: %+v", reassign)
		}
		got := s.Find(model.ProviderZai, accID)
		if got.ProxyID == nil || *got.ProxyID != lineC.ID || *got.ProxyURL != lineC.URL {
			t.Fatalf("账号应指向 line-c: %v %v", got.ProxyID, got.ProxyURL)
		}
		if profiles := s.ListProxyProfiles(); len(profiles) != 1 || profiles[0].ID != lineC.ID {
			t.Fatalf("线路表应只剩 line-c: %+v", profiles)
		}
	})

	t.Run("无空閒时摊薄到绑定数最少的线路", func(t *testing.T) {
		s := newTestStore(t)
		lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		lineB, err := s.AddProxyProfile("line-b", "http://2.2.2.2:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		lineC, err := s.AddProxyProfile("line-c", "http://3.3.3.3:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		// 预置负载：line-a 2 个、line-b 1 个；line-c 上 3 个账号待改派。
		mustAssign(s, "acc-a1", "uid-pa1", lineA.ID)
		mustAssign(s, "acc-a2", "uid-pa2", lineA.ID)
		mustAssign(s, "acc-b1", "uid-pb1", lineB.ID)
		c0 := mustAssign(s, "acc-c0", "uid-pc0", lineC.ID)
		c1 := mustAssign(s, "acc-c1", "uid-pc1", lineC.ID)
		c2 := mustAssign(s, "acc-c2", "uid-pc2", lineC.ID)

		purged, reassign, err := s.PurgeProxyProfiles([]string{lineC.ID})
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(purged) != 1 || len(reassign.Direct) != 0 {
			t.Fatalf("应只移除 line-c 且无人退直连: %v %+v", purged, reassign)
		}
		// 摊薄过程（贪心、逐个更新计数）：a=2,b=1 → c0→b(2)；
		// c1→并列(2,2)取表序→a(3)；c2→b(3)。最终 a=3、b=3。
		expect := map[string]string{c0: lineB.ID, c1: lineA.ID, c2: lineB.ID}
		for id, want := range expect {
			if reassign.Assigned[id] != want {
				t.Fatalf("改派结果与摊薄预期不符: %s → %v（期望 %s）全量: %+v", id, reassign.Assigned[id], want, reassign.Assigned)
			}
		}
	})

	t.Run("无可用候选则退回直连且清掉旧地址", func(t *testing.T) {
		s := newTestStore(t)
		lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		// 幸存线路停用 → 不作候选。
		if _, err := s.AddProxyProfile("line-off", "http://2.2.2.2:8080", false); err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		accID := mustAssign(s, "acc-1", "uid-purge-direct", lineA.ID)

		purged, reassign, err := s.PurgeProxyProfiles([]string{lineA.ID})
		if err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		if len(purged) != 1 || len(reassign.Direct) != 1 || reassign.Direct[0] != accID {
			t.Fatalf("应移除 line-a 并把账号退回直连: %v %+v", purged, reassign)
		}
		got := s.Find(model.ProviderZai, accID)
		if got.ProxyID != nil || got.ProxyURL != nil {
			t.Fatalf("退回直连后不应残留线路与地址: %+v", got)
		}
	})

	t.Run("不存在的 ID 静默跳过", func(t *testing.T) {
		s := newTestStore(t)
		purged, reassign, err := s.PurgeProxyProfiles([]string{"no-such-line", ""})
		if err != nil || purged != nil || len(reassign.Assigned) != 0 || len(reassign.Direct) != 0 {
			t.Fatalf("应空手而归: %v %+v %v", purged, reassign, err)
		}
	})

	t.Run("手工填地址的账号不受影响", func(t *testing.T) {
		s := newTestStore(t)
		lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		acc, err := s.AddAccount(model.ProviderZai, "acc-manual", jwtFor("uid-purge-manual"))
		if err != nil {
			t.Fatalf("入池失败: %v", err)
		}
		manual := "http://9.9.9.9:8080"
		if ok, err := s.SetProxyURL(model.ProviderZai, acc.ID, &manual); !ok || err != nil {
			t.Fatalf("手工填地址失败: %v %v", ok, err)
		}

		if _, _, err := s.PurgeProxyProfiles([]string{lineA.ID}); err != nil {
			t.Fatalf("清理失败: %v", err)
		}
		got := s.Find(model.ProviderZai, acc.ID)
		if got.ProxyID != nil || got.ProxyURL == nil || *got.ProxyURL != manual {
			t.Fatalf("手工出站地址不应被清理触碰: %+v", got)
		}
	})
}

// 后台任务拿到的必须是「副本」，且领取结果回写只改领取状态——
// 否则用副本整体回写会把派生之后发生的改动（例如删除线路改派的 ProxyURL）覆盖回去。
func TestSnapshotAccountIsDetachedAndClaimWriteBackKeepsChanges(t *testing.T) {
	s := newTestStore(t)
	lineA, err := s.AddProxyProfile("line-a", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	lineB, err := s.AddProxyProfile("line-b", "socks5://2.2.2.2:1080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	acc, err := s.AddAccount(model.ProviderZai, "acc-1", jwtFor("uid-snap-1"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if ok, err := s.AssignProxyProfile(acc.ID, lineA.ID); !ok || err != nil {
		t.Fatalf("指派失败: %v %v", ok, err)
	}

	// 长活后台任务在「派生那一刻」取副本。
	snap := s.SnapshotAccount(model.ProviderZai, acc.ID)
	if snap == nil {
		t.Fatal("应取到副本")
	}
	if snap.ProxyURL == nil || *snap.ProxyURL != lineA.URL {
		t.Fatalf("副本应带上当前出站地址: %v", snap.ProxyURL)
	}
	if snap.Quota == nil {
		t.Fatal("副本的 Quota 应为可用容器")
	}

	// 副本与 store 解耦：改副本不落回 store。
	other := "http://9.9.9.9:9999"
	snap.ProxyURL = &other
	snap.Status = model.StatusInvalid
	snap.Quota["glm-5.3"] = map[string]any{"remaining": 1.0}
	got := s.Find(model.ProviderZai, acc.ID)
	if got.ProxyURL == nil || *got.ProxyURL != lineA.URL {
		t.Fatalf("改副本不应影响 store 的出站地址: %v", got.ProxyURL)
	}
	if got.Status != model.StatusActive {
		t.Fatalf("改副本不应影响 store 的状态: %v", got.Status)
	}
	if len(got.Quota) != 0 {
		t.Fatalf("Quota 应为深拷贝: %v", got.Quota)
	}

	// 派生之后 store 改了该账号：删掉 line-a → 自动改派到 line-b。
	if _, _, err := s.DeleteProxyProfile(lineA.ID); err != nil {
		t.Fatalf("删除线路失败: %v", err)
	}

	// 领取结果回写：只落领取状态，改派结果必须保住。
	claimed := 1700000000.0
	ok, err := s.UpdateClaimState(model.ProviderZai, acc.ID, func(cur *model.ClaimState) *model.ClaimState {
		next := *cur
		next.ClaimedAt = &claimed
		return &next
	})
	if err != nil || !ok {
		t.Fatalf("回写领取状态失败: %v %v", ok, err)
	}
	got = s.Find(model.ProviderZai, acc.ID)
	if got.ProxyID == nil || *got.ProxyID != lineB.ID {
		t.Fatalf("回写不该覆盖改派结果: %v", got.ProxyID)
	}
	if got.ProxyURL == nil || *got.ProxyURL != lineB.URL {
		t.Fatalf("回写不该覆盖改派后的出站地址: %v", got.ProxyURL)
	}
	if v := got.ClaimView(); v == nil || v.ClaimedAt == nil || *v.ClaimedAt != claimed {
		t.Fatalf("领取状态应已写入: %+v", v)
	}
}

// 账号不存在时快照返回 nil、回写返回 false 且不报错——领取任务与删除并发属正常情况。
func TestSnapshotAndClaimWriteBackOnMissingAccount(t *testing.T) {
	s := newTestStore(t)

	if snap := s.SnapshotAccount(model.ProviderZai, "no-such-account"); snap != nil {
		t.Fatalf("账号不存在应返回 nil: %+v", snap)
	}
	ok, err := s.UpdateClaimState(model.ProviderZai, "no-such-account",
		func(cur *model.ClaimState) *model.ClaimState { return cur })
	if ok || err != nil {
		t.Fatalf("账号不存在应返回 false,nil: %v %v", ok, err)
	}
}

func TestAutoAssignProxies(t *testing.T) {
	// 建 n 个账号并返回它们的 ID（每个用不同 user_id，避免被三级判重合并）。
	newAccounts := func(t *testing.T, s *Store, names ...string) []string {
		t.Helper()
		ids := []string{}
		for _, n := range names {
			acc, err := s.AddAccount(model.ProviderZai, n, jwtFor("uid-"+n))
			if err != nil {
				t.Fatalf("入池 %s 失败: %v", n, err)
			}
			ids = append(ids, acc.ID)
		}
		return ids
	}

	t.Run("无线路时全部回退直连", func(t *testing.T) {
		s := newTestStore(t)
		ids := newAccounts(t, s, "a1", "a2")
		assigned, fallback := s.AutoAssignProxies(ids)
		if len(assigned) != 0 || len(fallback) != 2 {
			t.Fatalf("应全部回退直连: assigned=%v fallback=%v", assigned, fallback)
		}
		if got := s.Find(model.ProviderZai, ids[0]); got.ProxyID != nil || got.ProxyURL != nil {
			t.Fatalf("回退后不应带代理: %v / %v", got.ProxyID, got.ProxyURL)
		}
	})

	t.Run("按顺序分配且不足的回退直连", func(t *testing.T) {
		s := newTestStore(t)
		p1, err := s.AddProxyProfile("line-1", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		p2, err := s.AddProxyProfile("line-2", "socks5://2.2.2.2:1080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		ids := newAccounts(t, s, "a1", "a2", "a3", "a4", "a5")

		assigned, fallback := s.AutoAssignProxies(ids)
		if len(assigned) != 2 || len(fallback) != 3 {
			t.Fatalf("应 2 条分配、3 条直连: assigned=%d fallback=%d", len(assigned), len(fallback))
		}
		if assigned[ids[0]] != p1.ID || assigned[ids[1]] != p2.ID {
			t.Fatalf("应按传入顺序分配: %v", assigned)
		}
		// 必须落库：重新读出来的账号同样带线路。
		got := s.Find(model.ProviderZai, ids[0])
		if got.ProxyID == nil || *got.ProxyID != p1.ID ||
			got.ProxyURL == nil || *got.ProxyURL != p1.URL {
			t.Fatalf("线路未落库: %+v", got)
		}
	})

	t.Run("跳过已被占用的线路", func(t *testing.T) {
		s := newTestStore(t)
		p1, err := s.AddProxyProfile("line-1", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		p2, err := s.AddProxyProfile("line-2", "http://2.2.2.2:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		old := newAccounts(t, s, "old")[0]
		if ok, err := s.AssignProxyProfile(old, p1.ID); !ok || err != nil {
			t.Fatalf("指派失败: %v %v", ok, err)
		}
		fresh := newAccounts(t, s, "fresh")[0]

		assigned, fallback := s.AutoAssignProxies([]string{fresh})
		if len(fallback) != 0 {
			t.Fatalf("还有空閒线路，不应回退: %v", fallback)
		}
		if assigned[fresh] != p2.ID {
			t.Fatalf("应跳过已占用的 line-1 拿到 line-2: %v", assigned)
		}
	})

	t.Run("停用的线路不参与分配", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.AddProxyProfile("off", "http://1.1.1.1:8080", false); err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		id := newAccounts(t, s, "a1")[0]
		assigned, fallback := s.AutoAssignProxies([]string{id})
		if len(assigned) != 0 || len(fallback) != 1 {
			t.Fatalf("停用线路不应被分配: assigned=%v fallback=%v", assigned, fallback)
		}
	})

	t.Run("已被占用的线路不会被重复分配", func(t *testing.T) {
		s := newTestStore(t)
		p1, err := s.AddProxyProfile("line-1", "http://1.1.1.1:8080", true)
		if err != nil {
			t.Fatalf("建线路失败: %v", err)
		}
		ids := newAccounts(t, s, "a1", "a2")

		if _, fallback := s.AutoAssignProxies([]string{ids[0]}); len(fallback) != 0 {
			t.Fatalf("首个账号应拿到线路: %v", fallback)
		}
		assigned, fallback := s.AutoAssignProxies([]string{ids[1]})
		if len(assigned) != 0 || len(fallback) != 1 {
			t.Fatalf("线路已被占用，第二个账号应回退直连: assigned=%v fallback=%v", assigned, fallback)
		}
		// 前一个账号的指派不能被后续调用改动。
		if got := s.Find(model.ProviderZai, ids[0]); got.ProxyID == nil || *got.ProxyID != p1.ID {
			t.Fatalf("已有指派不应被改动: %+v", got.ProxyID)
		}
	})
}

func TestPickFreeProxyProfile(t *testing.T) {
	s := newTestStore(t)
	if _, ok := s.PickFreeProxyProfile(); ok {
		t.Fatal("没有任何线路时应返回 false")
	}
	p, err := s.AddProxyProfile("line-1", "http://1.1.1.1:8080", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	got, ok := s.PickFreeProxyProfile()
	if !ok || got.ID != p.ID {
		t.Fatalf("应挑中唯一空閒线路: %+v ok=%v", got, ok)
	}
	// 「只挑不写」：调用本身不占用线路、也不改账号——登录会话需要在账号建立前定出口。
	acc, err := s.AddAccount(model.ProviderZai, "a1", jwtFor("uid-pick"))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if _, ok := s.PickFreeProxyProfile(); !ok {
		t.Fatal("Pick 不应写入占用（账号尚未指派时线路应仍为空閒）")
	}
	if acc.ProxyID != nil || acc.ProxyURL != nil {
		t.Fatalf("Pick 不应改动账号: %v / %v", acc.ProxyID, acc.ProxyURL)
	}
}

// 删除必须「先落库再改内存」。反过来时落库失败会让内存与 DB 分叉——本次进程里账号
// 已消失、重启后又从 DB 载入回来。删除常用来撤销可疑或外泄的凭证，这种「显示已删除、
// 实际还在」属于安全相关的静默失败。这里用「关掉底层 DB 让落库必失败」来复现。
func TestRemoveAccountKeepsMemoryWhenPersistFails(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(model.ProviderZai, "victim", "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭存储失败: %v", err)
	}

	if ok, err := s.RemoveAccount(model.ProviderZai, acc.ID); err == nil || ok {
		t.Fatalf("落库失败应报错且不算删除成功: ok=%v err=%v", ok, err)
	}
	if s.Find(model.ProviderZai, acc.ID) == nil {
		t.Fatal("落库失败时内存里的账号不得被移除（否则重启后它会复活，而调用方以为已删除）")
	}
}

// 与删除同理：新建也必须先落库再改内存。反过来时落库失败会让账号在本次进程里可用、
// 重启后消失，而调用方收到错误以为没建成。
func TestAddAccountKeepsMemoryWhenPersistFails(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭存储失败: %v", err)
	}

	acc, isNew, err := s.AddAccountWithIdentity(model.ProviderZai, "late", "sk-1", "")
	if err == nil {
		t.Fatal("落库失败应报错")
	}
	if acc != nil || isNew {
		t.Fatalf("落库失败不应返回账号: acc=%v isNew=%v", acc, isNew)
	}
	if got := s.ListAccounts(model.ProviderZai); len(got) != 0 {
		t.Fatalf("落库失败时内存里不得留下账号: %d 个", len(got))
	}
}

// 设置读路径走原子快照：写入后必须立刻可见——发布点漏一个就会读到旧值，比不加
// 快照更糟。读不再依赖 store 锁，并发正确性由 CI 的 -race 覆盖。
func TestGetSettingSeesWritesImmediately(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("admin_key", "k1"); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.GetSetting("admin_key"); !ok || got != "k1" {
		t.Fatalf("写入后应立刻可见: %q ok=%v", got, ok)
	}
	if err := s.SetSetting("admin_key", "k2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSetting("admin_key"); got != "k2" {
		t.Fatalf("覆盖写后应看到新值: %q", got)
	}
	// 线路落库走另一条发布路径（saveProxyProfilesLocked），同样要立刻可见
	if _, err := s.AddProxyProfile("prx", "http://1.2.3.4:8080", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetSetting("proxy_profiles"); !ok {
		t.Fatal("线路写入后 proxy_profiles 应立刻可见")
	}
}

// 设置也必须先落库再改内存：落库失败时内存里不能留下未持久化的值，否则本次进程按
// 新值运行、重启后回滚，而调用方收到错误以为没生效（改密码场景会让人拿旧密码重试）。
func TestSetSettingKeepsMemoryWhenPersistFails(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetSetting("admin_key", "before"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("关闭存储失败: %v", err)
	}

	if err := s.SetSetting("admin_key", "after"); err == nil {
		t.Fatal("落库失败应报错")
	}
	if got, _ := s.GetSetting("admin_key"); got != "before" {
		t.Fatalf("落库失败时内存值不应改变: %q", got)
	}
}

// 巡检设置：默认跟随环境变量（开、30 分钟），落库后以设置为准，非法值回退默认。
func TestProxyHealthSettings(t *testing.T) {
	s := newTestStore(t)

	if !s.ProxyHealthEnabled() {
		t.Fatal("默认应开（环境变量默认 true）")
	}
	if got := s.ProxyHealthIntervalMinutes(); got != 30 {
		t.Fatalf("默认间隔应 30 分钟: %d", got)
	}

	// 落库覆盖。
	if err := s.SetSetting("proxy_health_enabled", "false"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if err := s.SetSetting("proxy_health_interval", "15"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if s.ProxyHealthEnabled() {
		t.Fatal("落库 false 后应关")
	}
	if got := s.ProxyHealthIntervalMinutes(); got != 15 {
		t.Fatalf("落库后间隔应 15: %d", got)
	}

	// 非法值回退默认；越界钳到下限 1。
	if err := s.SetSetting("proxy_health_enabled", "maybe"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if !s.ProxyHealthEnabled() {
		t.Fatal("非法布尔应回退默认（开）")
	}
	if err := s.SetSetting("proxy_health_interval", "abc"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if got := s.ProxyHealthIntervalMinutes(); got != 30 {
		t.Fatalf("非法间隔应回退默认 30: %d", got)
	}
	if err := s.SetSetting("proxy_health_interval", "0"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if got := s.ProxyHealthIntervalMinutes(); got != 1 {
		t.Fatalf("间隔 0 应钳到下限 1: %d", got)
	}
}
