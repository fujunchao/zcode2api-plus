package store

import (
	"errors"
	"testing"
	"time"

	"zcode2api/internal/model"
)

func pinQuota(a *model.Account, remaining float64) {
	a.Quota = map[string]map[string]any{
		"GLM-5.3": {"model": "GLM-5.3", "remaining": remaining},
	}
}

// 钉住的号即使额度最少也必须被选中：这就是「真机复验」的全部意义。
func TestPinOverridesQuotaTier(t *testing.T) {
	s := newTestStore(t)
	rich, _ := s.AddAccount(model.ProviderZai, "rich", "h1.p.s3")
	poor, _ := s.AddAccount(model.ProviderZai, "poor", "h2.p.s3")
	pinQuota(rich, 10_000_000)
	pinQuota(poor, 1)

	if _, err := s.PinAccount(model.ProviderZai, poor.ID, 30, time.Now()); err != nil {
		t.Fatalf("钉住应成功: %v", err)
	}
	for i := range 3 {
		acc := s.Select(model.ProviderZai, nil, "GLM-5.3")
		if acc == nil || acc.ID != poor.ID {
			t.Fatalf("第 %d 次应选中被钉住的账号: %v", i, acc)
		}
	}
}

// 钉住的号不可调度时静默回落正常选号，请求不能因此失败。
func TestPinFallsBackWhenCooling(t *testing.T) {
	s := newTestStore(t)
	pinned, _ := s.AddAccount(model.ProviderZai, "pinned", "h1.p.s3")
	other, _ := s.AddAccount(model.ProviderZai, "other", "h2.p.s3")
	until := float64(time.Now().Add(time.Hour).UnixNano()) / 1e9
	pinned.Status, pinned.CoolingUntil = model.StatusCooling, &until

	if _, err := s.PinAccount(model.ProviderZai, pinned.ID, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != other.ID {
		t.Fatalf("冷却中的钉住号应被跳过: %v", acc)
	}
}

// TTL 到期后钉子自动失效，不推进轮转游标：解除后第一次轮询仍按原顺序。
func TestPinExpiresAndKeepsRotation(t *testing.T) {
	s := newTestStore(t)
	first, _ := s.AddAccount(model.ProviderZai, "first", "h1.p.s3")
	second, _ := s.AddAccount(model.ProviderZai, "second", "h2.p.s3")
	pinQuota(first, 5_000_000)
	pinQuota(second, 5_000_000)

	if _, err := s.PinAccount(model.ProviderZai, second.ID, 5, time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != second.ID {
			t.Fatalf("钉住期间应始终选中 second: %v", acc)
		}
	}
	// 直接把截止时间推到过去，避免测试真等 5 分钟。
	s.mu.Lock()
	s.pinUntil = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got := s.PinnedAccount(time.Now()); got != nil {
		t.Fatalf("过期后状态应为空: %+v", got)
	}
	if acc := s.Select(model.ProviderZai, nil, "GLM-5.3"); acc == nil || acc.ID != first.ID {
		t.Fatalf("解除后应回到原轮询顺序（first）: %v", acc)
	}
}

// 归档账号不可钉住：归档即停止调用，钉它只会让请求全部回落。
func TestPinRejectsArchived(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(model.ProviderZai, "archived", "h1.p.s3")
	now := time.Now()
	ts := float64(now.UnixNano()) / 1e9
	acc.ArchivedAt = &ts

	if _, err := s.PinAccount(model.ProviderZai, acc.ID, 30, now); !errors.Is(err, ErrPinArchived) {
		t.Fatalf("归档账号应被拒绝，实际 %v", err)
	}
}

// 解除钉住是幂等的。
func TestUnpinIdempotent(t *testing.T) {
	s := newTestStore(t)
	s.UnpinAccount()
	s.UnpinAccount()
	if s.PinnedAccount(time.Now()) != nil {
		t.Fatal("未钉住时状态必须为空")
	}
}
