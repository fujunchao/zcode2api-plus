// 代理线路自动巡检的轮次级测试：探测目标全部指向本地桩（probeStub），
// 线路要么是活桩（既当代理又当入口）、要么是死端口，整轮不触网。
package adminapi

import (
	"net/http"
	"testing"

	"zcode2api/internal/model"
	"zcode2api/internal/store"
)

// bindAccount 建号并绑到指定线路（直接走 store，避开 HTTP 入池派生的后台领取）。
func bindAccount(t *testing.T, st *store.Store, name, uid, profileID string) string {
	t.Helper()
	acc, err := st.AddAccount(model.ProviderZai, name, jwtTokenFor(uid))
	if err != nil {
		t.Fatalf("入池失败: %v", err)
	}
	if ok, err := st.AssignProxyProfile(acc.ID, profileID); !ok || err != nil {
		t.Fatalf("指派失败: %v %v", ok, err)
	}
	return acc.ID
}

// 部分线路失败：坏线路被移除，其账号改派到幸存的空閒线路。
func TestProxyHealthRoundPurgesFailedLine(t *testing.T) {
	_, st, _ := setup(t)
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	lineOK, err := st.AddProxyProfile("line-ok", srv.URL, true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	lineDead, err := st.AddProxyProfile("line-dead", "http://127.0.0.1:1", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	accID := bindAccount(t, st, "acc-1", "u-health-1", lineDead.ID)

	NewProxyHealthScheduler(st).runOnce(nil)

	profiles := st.ListProxyProfiles()
	if len(profiles) != 1 || profiles[0].ID != lineOK.ID {
		t.Fatalf("应只保留 line-ok: %+v", profiles)
	}
	acc := st.Find(model.ProviderZai, accID)
	if acc.ProxyID == nil || *acc.ProxyID != lineOK.ID || *acc.ProxyURL != lineOK.URL {
		t.Fatalf("账号应改派到 line-ok: %v %v", acc.ProxyID, acc.ProxyURL)
	}
}

// 全部线路失败且直连也不可达：视为本机网络故障，本轮不移除任何线路。
func TestProxyHealthRoundSkipsWhenAllFailAndDirectDead(t *testing.T) {
	_, st, _ := setup(t)
	// 探测目标指向死端口：直连探测同样失败。
	oldTargets := upstreamProbeTargets
	upstreamProbeTargets = []string{"http://127.0.0.1:1/"}
	t.Cleanup(func() { upstreamProbeTargets = oldTargets })
	lineDead, err := st.AddProxyProfile("line-dead", "http://127.0.0.1:1", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	accID := bindAccount(t, st, "acc-1", "u-health-2", lineDead.ID)

	NewProxyHealthScheduler(st).runOnce(nil)

	if profiles := st.ListProxyProfiles(); len(profiles) != 1 {
		t.Fatalf("疑为网络故障时不应移除线路: %+v", profiles)
	}
	acc := st.Find(model.ProviderZai, accID)
	if acc.ProxyID == nil || *acc.ProxyID != lineDead.ID {
		t.Fatalf("账号绑定应原样保留: %+v", acc.ProxyID)
	}
}

// 全部线路失败但直连可达：判定线路自身失效，全部移除、账号退回直连。
func TestProxyHealthRoundPurgesAllWhenDirectWorks(t *testing.T) {
	_, st, _ := setup(t)
	// 目标（直连可达）活着，但所有线路都指向死端口。
	probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	lineDead1, err := st.AddProxyProfile("line-dead-1", "http://127.0.0.1:1", true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	if _, err := st.AddProxyProfile("line-dead-2", "http://127.0.0.1:1", true); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	accID := bindAccount(t, st, "acc-1", "u-health-3", lineDead1.ID)

	NewProxyHealthScheduler(st).runOnce(nil)

	if profiles := st.ListProxyProfiles(); len(profiles) != 0 {
		t.Fatalf("直连可用时应移除全部失效线路: %+v", profiles)
	}
	acc := st.Find(model.ProviderZai, accID)
	if acc.ProxyID != nil || acc.ProxyURL != nil {
		t.Fatalf("账号应退回直连且清掉旧地址: %+v", acc)
	}
}

// 全部线路可用：不动任何线路，也不动账号绑定。
func TestProxyHealthRoundKeepsHealthyLines(t *testing.T) {
	_, st, _ := setup(t)
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	lineA, err := st.AddProxyProfile("line-a", srv.URL, true)
	if err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	if _, err := st.AddProxyProfile("line-b", srv.URL, true); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	accID := bindAccount(t, st, "acc-1", "u-health-4", lineA.ID)

	NewProxyHealthScheduler(st).runOnce(nil)

	if profiles := st.ListProxyProfiles(); len(profiles) != 2 {
		t.Fatalf("健康线路不应被移除: %+v", profiles)
	}
	acc := st.Find(model.ProviderZai, accID)
	if acc.ProxyID == nil || *acc.ProxyID != lineA.ID {
		t.Fatalf("账号绑定不应变化: %+v", acc.ProxyID)
	}
}

// 停用的线路不参与巡检（本来就不会被账号使用），也不被自动移除。
func TestProxyHealthRoundSkipsDisabledLines(t *testing.T) {
	_, st, _ := setup(t)
	probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	// 死端口但停用：既不探测也不移除。
	if _, err := st.AddProxyProfile("line-off", "http://127.0.0.1:1", false); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}

	NewProxyHealthScheduler(st).runOnce(nil)

	if profiles := st.ListProxyProfiles(); len(profiles) != 1 {
		t.Fatalf("停用线路不应被自动移除: %+v", profiles)
	}
}

// Start/Stop 生命周期：Stop 应迅速返回（done 通道关闭）。
func TestProxyHealthSchedulerStartStop(t *testing.T) {
	_, st, _ := setup(t)
	sched := NewProxyHealthScheduler(st)
	sched.Start()
	sched.Stop()
	// Start 内部是 sync.Once，Stop 后再 Start 不会重启循环；这里不进一步断言，
	// 只保证生命周期不挂起、不 panic。
}
