package adminapi

import (
	"database/sql"
	"net/http"
	"reflect"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

func TestSettingsValidationHasNoPartialWrites(t *testing.T) {
	for _, bad := range []map[string]any{
		{"quota_refresh_interval": "invalid"},
		{"claim_schedule_time": "99:99"},
		{"risk_cooling_steps": "bad"},
		{"line_truncate_avoid_seconds": 3601},
	} {
		mux, st, _ := setup(t)
		_, before := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
		payload := map[string]any{"admin_key": "new-admin", "gateway_key": "new-gateway", "claim_auto_enabled": false}
		for k, v := range bad {
			payload[k] = v
		}
		code, _ := do(t, mux, st, http.MethodPut, "/admin/api/settings", payload)
		_, after := do(t, mux, st, http.MethodGet, "/admin/api/settings", nil)
		if code != http.StatusBadRequest || !reflect.DeepEqual(before, after) {
			t.Fatalf("任一字段非法时整次更新必须零副作用：status=%d invalid=%v changed=%v", code, bad, !reflect.DeepEqual(before, after))
		}
	}
}

func TestSettingsDatabaseFailureRollsBackAllKeys(t *testing.T) {
	mux, st, _ := setup(t)
	beforeAdmin, beforeGateway := st.AdminKey(), st.GatewayKey()
	db, err := sql.Open("sqlite", config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_gateway BEFORE INSERT ON meta
		WHEN NEW.key = 'gateway_key' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	code, _ := do(t, mux, st, http.MethodPut, "/admin/api/settings", map[string]any{
		"admin_key": "new-admin", "gateway_key": "new-gateway", "quota_refresh_interval": 123,
	})
	if code != http.StatusInternalServerError || st.AdminKey() != beforeAdmin || st.GatewayKey() != beforeGateway {
		t.Errorf("数据库中途失败时不得发布部分设置：status=%d", code)
	}
	for key, want := range map[string]string{"admin_key": beforeAdmin, "gateway_key": beforeGateway} {
		var got string
		if err := db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("数据库没有回滚 %s", key)
		}
	}
	if _, err := db.Exec("DROP TRIGGER reject_gateway"); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.AdminKey() != beforeAdmin || reopened.GatewayKey() != beforeGateway {
		t.Fatal("重开数据库后仍应是更新前的设置")
	}
}
