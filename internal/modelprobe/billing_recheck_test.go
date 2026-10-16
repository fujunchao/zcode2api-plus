package modelprobe_test

import (
	"context"
	"testing"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/modelprobe"
	"zcode2api/internal/proxy"
	"zcode2api/internal/store"
)

func TestUnifiedBillingFailureRequiresFreshNetworkConfirmation(t *testing.T) {
	for _, scenario := range []string{"automatic", "observe", "protected", "default", "recovered", "reference_failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNetworkFixture(t)
			switch scenario {
			case "observe":
				p := f.st.ModelProbeSettings()
				p.Mode = "observe"
				if _, err := f.st.SaveModelProbeSettings(p, f.now); err != nil {
					t.Fatal(err)
				}
			case "protected":
				if _, err := f.st.UpdateProxyProfile(f.bad.ID, f.bad.Name, f.bad.URL, true, true); err != nil {
					t.Fatal(err)
				}
			case "default":
				p := store.DefaultNewAccountProxyPolicy()
				p.Enabled, p.DefaultProxyID = true, f.bad.ID
				if _, err := f.st.SaveNewAccountProxyPolicy(p); err != nil {
					t.Fatal(err)
				}
			case "reference_failure":
				if _, err := f.st.UpdateProxyProfile(f.good.ID, f.good.Name, f.bad.URL, true); err != nil {
					t.Fatal(err)
				}
				f.m.Close()
				f.m = modelprobe.NewWithNetworkTargets(context.Background(), f.st, func() []string { return []string{f.bad.URL} }, func() time.Time { return f.now })
				t.Cleanup(f.m.Close)
			}
			a, err := f.st.AddAccount(model.ProviderZai, "计费观测账号", "synthetic.token.sig")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.st.AssignProxyProfile(a.ID, f.bad.ID); err != nil {
				t.Fatal(err)
			}
			snapshot := f.st.FindAny(a.ID)
			if result := f.st.ObserveProxyEndpointFailure(context.Background(), snapshot, proxy.FailureEndpointUnreachable, f.now, f.st.ProxyEgress(snapshot)); result != "recheck_pending" {
				t.Fatalf("计费异常应先等待独立复核：%s", result)
			}
			if len(f.st.ListProxyProfiles()) != 2 || f.calls.Load() != 0 {
				t.Fatal("计费通知自身不能删除或直接发起网络请求")
			}
			if worked, err := f.m.RunNetworkDue(context.Background()); err != nil || !worked {
				t.Fatalf("统一巡检没有消费计费通知：%v %v", worked, err)
			}
			if f.calls.Load() != 1 || len(f.st.ListProxyProfiles()) != 2 {
				t.Fatal("必须重新执行真实首轮检查，不能把计费失败当成首轮")
			}
			if scenario == "recovered" {
				f.blocked.Store(false)
			}
			f.second(t)
			wantProfiles := 2
			if scenario == "automatic" {
				wantProfiles = 1
			}
			if len(f.st.ListProxyProfiles()) != wantProfiles {
				t.Fatal("只有未保护的自动模式节点经两次失败且参考正常后才可删除")
			}
			records, err := f.st.NetworkHealth()
			if err != nil || len(records) != 1 {
				t.Fatalf("缺少真实复核记录：%v %v", records, err)
			}
			record := records[0]
			if record.Second == nil || record.Removed != (scenario == "automatic") {
				t.Fatalf("复核结果或删除记录不符：%+v", record)
			}
			if (scenario == "protected" || scenario == "default") && record.State != "protected_failure" {
				t.Fatal("保护不能把异常显示成健康，必须明确受保护未删除")
			}
			if scenario == "reference_failure" && record.State != "unconfirmed" {
				t.Fatal("目标和参考共同异常不能归因到单条代理")
			}
			if scenario != "automatic" && (f.st.FindAny(a.ID).ProxyID == nil || *f.st.FindAny(a.ID).ProxyID != f.bad.ID) {
				t.Fatal("没有确认删除时不能改派原业务绑定")
			}
			if worked, err := f.m.RunDue(context.Background()); err != nil || worked {
				t.Fatalf("计费异常触发的网络复核不得自动发送模型请求：%v %v", worked, err)
			}
		})
	}
}
