package adminapi

import (
	"net/http"
	"sync/atomic"
	"testing"
)

// 首轮失败不等于持续故障：本地入口第二次已恢复时必须保留线路。
func TestProxyHealthRecoveryBeforeConfirmationKeepsLine(t *testing.T) {
	_, st, _ := setup(t)
	var calls atomic.Int32
	srv := probeStub(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(403)
			_, _ = w.Write([]byte("Just a moment"))
			return
		}
		_, _ = w.Write([]byte("reachable"))
	})
	p, err := st.AddProxyProfile("瞬时异常", srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	NewProxyHealthScheduler(st).runOnce(nil)
	profiles := st.ListProxyProfiles()
	if len(profiles) != 1 || profiles[0].ID != p.ID {
		t.Fatal("未复核同一线路就删除了已经恢复的代理")
	}
}
