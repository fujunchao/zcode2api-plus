package store

import (
	"fmt"
	"testing"
	"time"
)

func TestStrictProxyEvidenceOverflowCannotForgetContradictorySuccess(t *testing.T) {
	s, bad, good, a := strictComparisonSetup(t)
	now := time.Now().Truncate(time.Second)
	s.strictNow = func() time.Time { return now }
	observe := func(id string, status int, model string) ProxyDecision {
		t.Helper()
		o := strictObservation(id, status)
		o.At = now
		if model != "" {
			o.Model = model
		}
		return strictObserve(t, s, s.FindAny(a.ID), o)
	}
	observe("target-had-complete-success", 200, "")
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	// 真实走公开观察入口填满预算；不能把被挤掉的目标成功当作从未发生。
	for i := 0; i < strictProxyEvidenceLimit; i++ {
		observe(fmt.Sprintf("filler-%d", i), 200, "unrelated-model")
	}
	if len(s.strictEvidence) > strictProxyEvidenceLimit {
		t.Fatal("证据保护不能通过无限扩充缓存实现")
	}
	status, err := s.StrictProxySummary()
	if err != nil || !status.EvidenceLimited || status.EvidenceIncompleteUntil < now.Add(strictProxyWindow).UnixMilli() {
		t.Fatalf("必须暴露证据窗口不完整状态：%+v %v", status, err)
	}
	deadline := time.UnixMilli(status.EvidenceIncompleteUntil)
	now = now.Add(time.Minute)
	if _, err := s.AssignProxyProfile(a.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		observe(fmt.Sprintf("failure-%d", i), 405, "")
	}
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	if d := observe("control", 200, ""); len(d.Removed) != 0 || d.Reason != "evidence_incomplete" {
		t.Fatalf("预算挤掉矛盾成功之后即使表面满足 A 也不能删除：%+v", d)
	}
	if len(s.ListProxyProfiles()) != 2 {
		t.Fatal("不完整窗口不能产生删除副作用")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = reopened
	s.strictNow = func() time.Time { return now }
	if status, _ := s.StrictProxySummary(); !status.EvidenceLimited {
		t.Fatal("重启清空证据缓存不能绕过尚未结束的不完整窗口")
	}
	if _, err := s.AssignProxyProfile(a.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		observe(fmt.Sprintf("restart-failure-%d", i), 405, "")
	}
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	if d := observe("restart-control", 200, ""); len(d.Removed) != 0 {
		t.Fatalf("重启后仍不能用不完整窗口判删：%+v", d)
	}
	// 再跨过所有旧样本的窗口，只有新的两次失败及完整成功对照能够成立。
	now = deadline.Add(strictProxyWindow + time.Second)
	if status, _ := s.StrictProxySummary(); status.EvidenceLimited {
		t.Fatal("不完整窗口到期后应恢复采纳新决定，不得永久关闭")
	}
	if _, err := s.AssignProxyProfile(a.ID, bad.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if d := observe(fmt.Sprintf("fresh-failure-%d", i), 405, ""); len(d.Removed) != 0 {
			t.Fatal("窗口恢复后仍须先取得新的完整成功对照")
		}
	}
	if _, err := s.AssignProxyProfile(a.ID, good.ID); err != nil {
		t.Fatal(err)
	}
	d := observe("fresh-control", 200, "")
	if len(d.Removed) != 1 || d.Removed[0] != bad.ID {
		t.Fatalf("完整新窗口满足 A 后应恢复正常严格删除：%+v", d)
	}
}
