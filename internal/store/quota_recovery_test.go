package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"zcode2api/internal/model"
)

const recoveryModel = "GLM-5.3"

func quotaTestAccount(t *testing.T, s *Store, now time.Time) *model.Account {
	t.Helper()
	a, err := s.AddAccount(model.ProviderZai, "quota-probe", "sk-synthetic-quota-probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQuotaExhausted(a, recoveryModel, "合成额度耗尽", now, 0); err != nil {
		t.Fatal(err)
	}
	return a
}

func requireQuotaProbe(t *testing.T, s *Store, name string, now time.Time, want bool) *QuotaProbe {
	t.Helper()
	probe, err := s.AcquireQuotaProbe(model.ProviderZai, nil, name, now)
	if err != nil || (probe != nil) != want {
		t.Fatalf("探测存在=%v，应为 %v，错误=%v", probe != nil, want, err)
	}
	return probe
}

func TestQuotaProbeSingleFlightAndModelIsolation(t *testing.T) {
	s := newTestStore(t)
	now := time.Unix(1_790_000_000, 0)
	a := quotaTestAccount(t, s, now)
	other := "glm-5.3-flash"
	if _, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
		live.Quota = map[string]map[string]any{
			"current": {"model": recoveryModel, "remaining": 0, "available": 0},
			"other":   {"model": other, "remaining": 0, "available": 0},
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQuotaExhausted(a, other, "另一模型也耗尽", now, 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	probe := requireQuotaProbe(t, s, "glm-5.3", now, true)
	if s.Select(a.Provider, nil, recoveryModel) != nil {
		t.Fatal("探测期间不能提前解除耗尽标记")
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.AcquireQuotaProbe(a.Provider, nil, recoveryModel, now.Add(24*time.Hour))
			if p != nil || err != nil {
				t.Errorf("即使时间推进，在途探测也必须独占: %v, %v", p != nil, err)
			}
		}()
	}
	wg.Wait()
	if ok, err := s.FinishQuotaProbe(probe, true, now); err != nil || !ok {
		t.Fatalf("完整成功应恢复: %v, %v", ok, err)
	}
	got := s.SnapshotAccount(a.Provider, a.ID)
	if got.ModelAvailability(recoveryModel) != "unknown" || got.ModelAvailability(other) != "exhausted" {
		t.Fatalf("只能恢复目标模型，且不得编造余额: %v", got.Quota)
	}
	if s.Select(a.Provider, nil, recoveryModel) == nil || s.Select(a.Provider, nil, other) != nil {
		t.Fatal("恢复后的调度没有保持模型隔离")
	}
	if ok, err := s.FinishQuotaProbe(probe, true, now); ok || err != nil {
		t.Fatalf("重复完成不得重复应用: %v, %v", ok, err)
	}
}

func TestQuotaProbeBackoffSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)
	base := time.Unix(1_790_000_000, 0)
	quotaTestAccount(t, s, base)
	requireQuotaProbe(t, s, recoveryModel, base.Add(5*time.Minute-time.Second), false)
	probe := requireQuotaProbe(t, s, recoveryModel, base.Add(5*time.Minute), true)
	if _, err := s.FinishQuotaProbe(probe, false, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	if got, exists := s.GetSetting(quotaRecoveryKey); exists || got != "" {
		t.Fatal("内部恢复元数据不得成为公开设置")
	}
	// 首次等待 5 分钟，探测后分别等待 15 分钟、1 小时、6 小时，之后封顶。
	now := base.Add(5 * time.Minute)
	for _, wait := range []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 6 * time.Hour} {
		requireQuotaProbe(t, s, recoveryModel, now.Add(wait-time.Second), false)
		now = now.Add(wait)
		probe = requireQuotaProbe(t, s, recoveryModel, now, true)
		if _, err := s.FinishQuotaProbe(probe, false, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestQuotaProbeLegacyRecords(t *testing.T) {
	for _, knownTime := range []bool{false, true} {
		t.Run(map[bool]string{false: "缺失旧失败时间", true: "有旧额度失败时间"}[knownTime], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.db")
			s := openAt(t, path)
			a, err := s.AddAccount(model.ProviderZai, "legacy", "sk-synthetic-legacy")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1_790_000_000, 0)
			_, err = s.Update(a.Provider, a.ID, func(live *model.Account) {
				live.MarkModelExhausted(recoveryModel)
				if knownTime {
					kind, when := model.ErrorKindQuotaExhausted, float64(now.Add(-time.Hour).Unix())
					live.LastErrorKind, live.LastErrorAt = &kind, &when
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			probe := requireQuotaProbe(t, s, recoveryModel, now, knownTime)
			if knownTime {
				_, err = s.FinishQuotaProbe(probe, true, now)
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openAt(t, path)
			requireQuotaProbe(t, s, recoveryModel, now.Add(4*time.Minute), false)
			probe = requireQuotaProbe(t, s, recoveryModel, now.Add(5*time.Minute), true)
			_, _ = s.FinishQuotaProbe(probe, false, now.Add(5*time.Minute))
		})
	}
}

func TestQuotaProbeProtectsAccountConstraints(t *testing.T) {
	base := time.Unix(1_790_000_000, 0)
	future := float64(base.Add(time.Hour).Unix())
	cases := map[string]func(*model.Account){
		"风控失效": func(a *model.Account) { a.Status = model.StatusInvalid },
		"停用":   func(a *model.Account) { a.Enabled = false },
		"停用状态": func(a *model.Account) { a.Status = model.StatusDisabled },
		"归档":   func(a *model.Account) { a.ArchivedAt = &future },
		"冷却": func(a *model.Account) {
			a.Status, a.CoolingUntil = model.StatusCooling, &future
		},
		"无截止的冷却": func(a *model.Account) { a.Status = model.StatusCooling },
		"人工停用模型": func(a *model.Account) { a.SetDisabledModels([]string{recoveryModel}) },
	}
	for name, mutate := range cases {
		for _, afterAcquire := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "取得前", true: "取得后"}[afterAcquire], func(t *testing.T) {
				s := newTestStore(t)
				a := quotaTestAccount(t, s, base)
				now := base.Add(5 * time.Minute)
				var probe *QuotaProbe
				if afterAcquire {
					probe = requireQuotaProbe(t, s, recoveryModel, now, true)
				}
				if _, err := s.Update(a.Provider, a.ID, mutate); err != nil {
					t.Fatal(err)
				}
				if !afterAcquire {
					requireQuotaProbe(t, s, recoveryModel, now, false)
					return
				}
				if ok, err := s.FinishQuotaProbe(probe, true, now); ok || err != nil {
					t.Fatalf("迟到成功不得绕过账号约束: %v, %v", ok, err)
				}
				if len(s.SnapshotAccount(a.Provider, a.ID).ExhaustedModels) != 1 {
					t.Fatal("强保护生效时错误移除了模型标记")
				}
			})
		}
	}
}

func TestQuotaProbeProtectsNewIdentityAndFailure(t *testing.T) {
	for _, change := range []string{"凭据", "模式", "代理", "设备", "同一时刻新失败", "删除"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			base := time.Unix(1_790_000_000, 0)
			a := quotaTestAccount(t, s, base)
			now := base.Add(5 * time.Minute)
			probe := requireQuotaProbe(t, s, recoveryModel, now, true)
			if change == "同一时刻新失败" {
				if err := s.MarkQuotaExhausted(a, recoveryModel, "新的耗尽事实", base, 0); err != nil {
					t.Fatal(err)
				}
			} else if change == "删除" {
				if _, err := s.RemoveAccount(a.Provider, a.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
					value := "synthetic-new-value"
					switch change {
					case "凭据":
						live.APIKey = &value
					case "模式":
						live.Mode, live.JWTToken = "jwt", &value
					case "代理":
						live.ProxyURL = &value
					case "设备":
						live.VirtualDeviceMid = &value
					}
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := s.FinishQuotaProbe(probe, true, now); ok || err != nil {
				t.Fatalf("旧租约不得恢复已变化的账号: %v, %v", ok, err)
			}
		})
	}
}

func TestQuotaProbePersistenceFailureIsAtomic(t *testing.T) {
	t.Run("记录耗尽失败", func(t *testing.T) {
		s := newTestStore(t)
		a, err := s.AddAccount(model.ProviderZai, "persist", "sk-synthetic-persist")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkQuotaExhausted(a, recoveryModel, "失败", time.Now(), 0); err == nil {
			t.Fatal("数据库失败应返回错误")
		}
		if len(s.SnapshotAccount(a.Provider, a.ID).ExhaustedModels) != 0 {
			t.Fatal("失败的事务不应发布内存耗尽状态")
		}
	})
	t.Run("恢复落盘失败", func(t *testing.T) {
		s := newTestStore(t)
		now := time.Unix(1_790_000_000, 0)
		a := quotaTestAccount(t, s, now)
		probe := requireQuotaProbe(t, s, recoveryModel, now.Add(5*time.Minute), true)
		if err := s.db.Close(); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.FinishQuotaProbe(probe, true, now.Add(5*time.Minute)); ok || err == nil {
			t.Fatalf("恢复落盘失败不得发布成功: %v, %v", ok, err)
		}
		if s.SnapshotAccount(a.Provider, a.ID).ModelAvailability(recoveryModel) != "exhausted" {
			t.Fatal("持久化失败不应乐观恢复模型")
		}
		if probe, err := s.AcquireQuotaProbe(a.Provider, nil, recoveryModel, now.Add(20*time.Minute)); probe != nil || err == nil {
			t.Fatalf("预留落盘失败不应发放租约: %v, %v", probe != nil, err)
		}
	})
}

func TestQuotaProbeHandlesLegacyNullQuotaEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.db")
	s := openAt(t, path)
	now := time.Unix(1_790_000_000, 0)
	a := quotaTestAccount(t, s, now)
	if _, err := s.Update(a.Provider, a.ID, func(live *model.Account) {
		live.Quota = map[string]map[string]any{"glm-5.3": nil}
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAt(t, path)
	probe := requireQuotaProbe(t, s, recoveryModel, now.Add(5*time.Minute), true)
	if ok, err := s.FinishQuotaProbe(probe, true, now.Add(5*time.Minute)); !ok || err != nil {
		t.Fatalf("旧空额度列应可恢复成未知值: %v, %v", ok, err)
	}
	if s.Select(a.Provider, nil, recoveryModel) == nil {
		t.Fatal("旧空列成功恢复后仍不可调度")
	}
}

func TestQuotaProbeRejectsRebuiltGeneration(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1_790_000_000, 0)
	a := quotaTestAccount(t, s, base)
	now := base.Add(5 * time.Minute)
	probe := requireQuotaProbe(t, s, recoveryModel, now, true)
	if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetSecret: true, SecretMode: "jwt", Secret: "synthetic.payload.signature"}); err != nil {
		t.Fatal(err)
	}
	b, err := s.AddAccount(a.Provider, "other", "sk-synthetic-other")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQuotaExhausted(b, recoveryModel, "触发过期元数据清理", now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EditAccount(a.Provider, a.ID, AccountEdit{SetSecret: true, SecretMode: "apiKey", Secret: a.Secret()}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQuotaExhausted(a, recoveryModel, "原凭据上的新失败", now, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishQuotaProbe(probe, true, now); ok || err != nil {
		t.Fatalf("元数据重建不能令旧租约代次碰撞: %v, %v", ok, err)
	}
	if got := s.SnapshotAccount(a.Provider, a.ID); got.ModelAvailability(recoveryModel) != "exhausted" {
		t.Fatal("旧成功清除了新的额度耗尽事实")
	}
	if p, err := s.AcquireQuotaProbe(a.Provider, map[string]bool{b.ID: true}, recoveryModel, now); p != nil || err != nil {
		t.Fatalf("新失败的等待窗口应保留: %v %v", p != nil, err)
	}
}
