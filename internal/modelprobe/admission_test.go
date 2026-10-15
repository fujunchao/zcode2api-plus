package modelprobe_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

func TestModelProbeSuspectAdmissionDependsOnMode(t *testing.T) {
	for _, mode := range []string{"observe", "auto_retire"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			p := f.st.ModelProbeSettings()
			p.Mode = mode
			if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
				t.Fatal(err)
			}
			f.respond = func(string, string, int) (int, string) { return 405, riskMessage }
			if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.RunDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			next, ok := f.st.PickAvailableProxyProfile()
			if !ok {
				t.Fatal("健康参考不应被排除")
			}
			want := f.target.ID
			if mode == "auto_retire" {
				want = f.reference.ID
			}
			if next.ID != want {
				t.Fatalf("自动新增绑定未按处置模式暂避待核查线路：got=%s want=%s", next.ID, want)
			}
			if len(f.st.ListProxyProfiles()) != 2 {
				t.Fatal("单次待核查不得删除")
			}
		})
	}
}

func TestModelProbeRestartDoesNotForgetContradictorySuccessWindow(t *testing.T) {
	f := newFixture(t)
	f.respond = func(string, string, int) (int, string) { return 200, completeMessage }
	if r := f.drive(t); r.Verdict != "healthy_recent" {
		t.Fatal(r.Verdict)
	}
	reopened, err := store.New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	view, err := reopened.ModelProbeSummary(f.now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	until, ok := data["evidence_incomplete_until"].(float64)
	if !ok || int64(until) <= f.now.UnixMilli() {
		t.Fatalf("重启丢失短窗口私有证据后，应暂停新删除而非宣称没有成功反证：%s", raw)
	}
}

func TestModelProbeResultPersistenceFailureStopsFurtherDispatch(t *testing.T) {
	f := newFixture(t)
	db, err := sql.Open("sqlite", "file:"+config.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f.respond = func(string, string, int) (int, string) {
		if _, err := db.Exec(`CREATE TRIGGER reject_probe_result BEFORE INSERT ON model_probe_runs BEGIN SELECT RAISE(ABORT,'injected result failure'); END`); err != nil {
			t.Error(err)
		}
		return 405, riskMessage
	}
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.RunDue(context.Background()); err == nil {
		t.Fatal("结果持久化失败必须返回错误")
	}
	view, err := f.st.ModelProbeSummary(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !view.StorageError || view.UsedRequests != 1 || view.Active != 0 {
		t.Fatalf("失败时必须保留真实出站预算并停止新派发：%+v", view)
	}
	if _, err := f.st.QueueModelProbe(f.target.ID, "manual", f.now); err == nil {
		t.Fatal("存储故障仍允许新的探测")
	}
	if a := f.st.FindAny(f.accounts[0].ID); a.RiskControlStreak != 0 {
		t.Fatal("失败的事务提前发布了账号状态")
	}
}
