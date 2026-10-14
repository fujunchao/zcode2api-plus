package store

import (
	"fmt"
	"testing"
	"time"
	"zcode2api/internal/model"
)

func TestStrictProxyComparableSuccessDeletesFailedIdentity(t *testing.T) {
	s := newTestStore(t)
	bad, err := s.AddProxyProfile("异常", "http://127.0.0.1:18001", true)
	if err != nil {
		t.Fatal(err)
	}
	good, err := s.AddProxyProfile("对照", "http://127.0.0.1:18002", true)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.AddAccount(model.ProviderZai, "合成账号", "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AssignProxyProfile(acc.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < 2; i++ {
		snapshot := s.FindAny(acc.ID)
		d, err := s.ObserveProxyOutcome(snapshot, s.ProxyEgress(snapshot), ProxyObservation{RequestID: fmt.Sprintf("fail-%d", i), TargetOrigin: "https://model.example.test", Model: "glm-test", ContentRef: "content-a", ProfileRef: "desktop-v1", SessionRef: "session-a", DeviceRef: "device-a", BusinessCode: "3012", Status: 405, At: now.Add(time.Duration(i) * time.Second)})
		if err != nil || len(d.Removed) != 0 {
			t.Fatalf("没有完整成功对照不能删除: %+v, %v", d, err)
		}
	}
	if _, err = s.AssignProxyProfile(acc.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := s.FindAny(acc.ID)
	d, err := s.ObserveProxyOutcome(snapshot, s.ProxyEgress(snapshot), ProxyObservation{RequestID: "success", TargetOrigin: "https://model.example.test", Model: "glm-test", ContentRef: "content-a", ProfileRef: "desktop-v1", SessionRef: "session-a", DeviceRef: "device-a", Status: 200, Complete: true, At: now.Add(2 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Removed) != 1 || d.Removed[0] != bad.ID {
		t.Fatalf("路径 A 完整对照成立后必须立即删除异常线路，得到 %+v", d)
	}
	if _, err := s.AddProxyProfile("改名重新导入", bad.URL, true); err == nil {
		t.Fatal("严格删除后不得通过新 ID/名称重新导入")
	}
}
