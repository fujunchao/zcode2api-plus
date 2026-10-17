package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxysub"
)

func (s *Store) subscriptionLocked() (proxysub.State, error) {
	raw := s.settings[proxysub.StateKey]
	state := proxysub.NewState()
	if raw != "" && (json.Unmarshal([]byte(raw), &state) != nil || state.Version != 1 || state.Config.Validate() != nil || state.Registry == nil || state.Blocks == nil) {
		return state, proxysub.ErrState
	}
	// 旧版本回退后可能更改过线路；只保留 ID 和地址仍匹配的来源记录。
	live := map[string]string{}
	for _, p := range s.listProxyProfilesLocked() {
		live[p.ID] = proxysub.Fingerprint(p.URL)
	}
	for id, o := range state.Registry {
		if live[id] != o.Key {
			delete(state.Registry, id)
		}
	}
	// 开始拉取/探测之前就与独立严格墓碑合并，不只在最终导入时才发现退役。
	for _, b := range s.strictProxy.Blocks {
		if b.SubscriptionKey != "" {
			state.Blocks[b.SubscriptionKey] = proxysub.Block{Key: b.SubscriptionKey, Reason: "strict_3012:" + b.Reason, CreatedAt: b.CreatedAt, Until: 0}
		}
	}
	state.ImportPaused = state.ImportPaused || s.strictProxy.Paused || s.strictProxy.ImportPaused
	return state, nil
}

func (s *Store) SubscriptionSnapshot() (proxysub.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.subscriptionLocked()
	out := proxysub.Snapshot{State: state, Profiles: []proxysub.Existing{}}
	for _, p := range s.listProxyProfilesLocked() {
		out.Profiles = append(out.Profiles, proxysub.Existing{ID: p.ID, URL: p.URL, Enabled: p.Enabled})
	}
	return out, err
}

func (s *Store) SaveSubscriptionConfig(c proxysub.Config) (proxysub.Config, error) {
	if err := c.Validate(); err != nil {
		return c, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.subscriptionLocked()
	if err != nil {
		return c, err
	}
	if state.Config.Revision != c.Revision {
		return c, proxysub.ErrConflict
	}
	c.Revision++
	state.Config = c
	err = s.commitProxyBundleLocked(context.Background(), nil, nil, nil, &state)
	return c, err
}

func putSubscriptionRun(state *proxysub.State, r proxysub.Run) {
	runs := []proxysub.Run{r}
	for _, old := range state.Runs {
		if old.ID != r.ID && len(runs) < proxysub.MaxHistory {
			runs = append(runs, old)
		}
	}
	state.Runs = runs
}

func (s *Store) BeginSubscriptionRun(r proxysub.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.subscriptionLocked()
	if err != nil {
		return err
	}
	if state.Config.Revision != r.Revision {
		return proxysub.ErrConflict
	}
	putSubscriptionRun(&state, r)
	return s.commitProxyBundleLocked(context.Background(), nil, nil, nil, &state)
}

func (s *Store) RecoverSubscriptionRuns() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.subscriptionLocked()
	if err != nil {
		return err
	}
	changed := false
	for i := range state.Runs {
		if !state.Runs[i].Terminal() {
			state.Runs[i].Status = "interrupted"
			state.Runs[i].FinishedAt = time.Now().UnixMilli()
			state.Runs[i].Error = "进程重启，未恢复旧任务"
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.commitProxyBundleLocked(context.Background(), nil, nil, nil, &state)
}

func (s *Store) UnblockSubscription(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.strictProxy.Blocks {
		if b.SubscriptionKey == key {
			return errors.New("严格淘汰记录只能通过严格代理解除入口恢复")
		}
	}
	state, err := s.subscriptionLocked()
	if err != nil {
		return err
	}
	if _, ok := state.Blocks[key]; !ok {
		return ErrNotFound
	}
	delete(state.Blocks, key)
	return s.commitProxyBundleLocked(context.Background(), nil, nil, nil, &state)
}

func (s *Store) FinishSubscriptionRun(ctx context.Context, r proxysub.Run, passed []proxysub.Candidate) (proxysub.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.subscriptionLocked()
	if err != nil {
		return r, err
	}
	// 相同终态重复提交不重复创建线路。
	for _, old := range state.Runs {
		if old.ID == r.ID && old.Terminal() {
			return old, nil
		}
	}
	if r.Status == "completed" && (r.Revision != state.Config.Revision || ctx.Err() != nil) {
		return r, proxysub.ErrConflict
	}
	if r.Mode != "import" || r.Status != "completed" {
		passed = nil
	}
	c := state.Config
	now := time.Now()
	profiles := s.listProxyProfilesLocked()
	keys, names := map[string]bool{}, map[string]bool{}
	for _, p := range profiles {
		keys[proxysub.Fingerprint(p.URL)] = true
		names[p.Name] = true
	}
	owned := len(state.Registry)
	if state.ImportPaused && len(passed) > 0 {
		return r, errors.New("订阅禁止记录达到预算")
	}
	// 预览只写任务摘要，不能顺带清理禁止记录。
	if r.Mode == "import" && r.Status == "completed" {
		for key, b := range state.Blocks {
			if !b.Active(now) {
				delete(state.Blocks, key)
			}
		}
	}
	r.Added = 0
	var added []ProxyProfile
	for _, a := range passed {
		endpoint, e := proxysub.PublicEndpoint(a.URL)
		b, blocked := state.Blocks[a.Key]
		if e != nil || proxysub.Fingerprint(endpoint) != a.Key || keys[a.Key] ||
			s.strictProxyBlockedLocked(endpoint) ||
			(blocked && b.Active(now)) || owned >= c.Capacity || r.Added >= c.ImportLimit ||
			!proxysub.FreshLocalProbe(a.ProbedAt, now) ||
			len(state.Blocks)+owned >= proxysub.MaxBlocks {
			r.CommitSkipped++
			continue
		}
		id := "proxy-" + randomHex(8)
		name := "订阅-" + id[6:]
		for names[name] {
			id = "proxy-" + randomHex(8)
			name = "订阅-" + id[6:]
		}
		profiles = append(profiles, ProxyProfile{ID: id, Name: name, URL: endpoint, Enabled: c.EnableImported})
		added = append(added, ProxyProfile{ID: id, Name: name, URL: endpoint, Enabled: c.EnableImported})
		state.Registry[id] = proxysub.Origin{Source: proxysub.SourceID, Key: a.Key, ImportedAt: now.UnixMilli(), LastOK: a.LastOK, ProbedAt: a.ProbedAt, IP: a.IP}
		keys[a.Key], names[name] = true, true
		owned++
		r.Added++
	}
	putSubscriptionRun(&state, r)
	if r.Added == 0 {
		profiles = nil
	}
	err = s.commitProxyBundleLocked(ctx, profiles, nil, nil, &state)
	if err != nil {
		r.Added = 0
		return r, err
	}
	// 新导入线路必须体检后才参与自动选线：这里登记一次冷启动检查，
	// 巡检下一 tick 取走，结果落库前 chooseProxyLocked 会跳过它。
	s.queueFreshProxyChecksLocked(added, now)
	return r, nil
}

// subscriptionMutationLocked 与线路修改处在同一把 Store 锁/同一事务中。
// reason=manual 只用于人工删除；停用与改 URL 从前后差异识别。
func (s *Store) subscriptionMutationLocked(profiles []ProxyProfile, reason string) (*proxysub.State, error) {
	if s.settings[proxysub.StateKey] == "" && len(s.strictProxy.Blocks) == 0 {
		return nil, nil
	}
	state, err := s.subscriptionLocked()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for key, b := range state.Blocks {
		if !b.Active(now) {
			delete(state.Blocks, key)
		}
	}
	next := map[string]ProxyProfile{}
	for _, p := range profiles {
		next[p.ID] = p
	}
	block := func(key, why string, manual bool) {
		if old, ok := state.Blocks[key]; ok && old.Until == 0 {
			return
		}
		if len(state.Blocks) >= proxysub.MaxBlocks {
			state.ImportPaused = true
			return
		}
		until := now.Add(24 * time.Hour).UnixMilli()
		if manual {
			until = 0
		}
		state.Blocks[key] = proxysub.Block{Key: key, Reason: why, CreatedAt: now.UnixMilli(), Until: until}
	}
	for _, old := range s.listProxyProfilesLocked() {
		key := proxysub.Fingerprint(old.URL)
		_, owned := state.Registry[old.ID]
		p, exists := next[old.ID]
		switch {
		case !exists:
			if reason == "manual" {
				block(key, "人工删除", true)
			} else if owned {
				block(key, "自动淘汰", false)
			}
			delete(state.Registry, old.ID)
		case proxysub.Fingerprint(p.URL) != key:
			block(key, "人工更改地址", true)
			delete(state.Registry, old.ID)
		case old.Enabled && !p.Enabled:
			block(key, "人工停用", true)
		case !old.Enabled && p.Enabled:
			if !s.strictProxyBlockedLocked(p.URL) {
				delete(state.Blocks, key)
			}
		}
	}
	for _, b := range s.strictProxy.Blocks {
		if b.SubscriptionKey != "" {
			state.Blocks[b.SubscriptionKey] = proxysub.Block{Key: b.SubscriptionKey, Reason: "strict_3012:" + b.Reason, CreatedAt: b.CreatedAt, Until: 0}
		}
	}
	if s.strictProxy.Paused || s.strictProxy.ImportPaused {
		state.ImportPaused = true
	}
	return &state, nil
}

// 断流/自动巡检保持同一自动淘汰语义；账号风控换线不经过此处。
func (s *Store) commitProxyBundleLocked(ctx context.Context, profiles []ProxyProfile, pending []*model.Account, history riskProxyHistory, sub *proxysub.State) error {
	return s.commitProxyBundleStrictLocked(ctx, profiles, pending, history, sub, nil)
}

func (s *Store) commitProxyBundleStrictLocked(ctx context.Context, profiles []ProxyProfile, pending []*model.Account, history riskProxyHistory, sub *proxysub.State, strict *strictProxyState) error {
	if profiles == nil && len(pending) == 0 && history == nil && sub == nil && strict == nil {
		return nil
	}
	if strict == nil {
		next := s.cloneStrictProxyLocked()
		strict = &next
	}
	for _, a := range pending {
		trackStrictAccountVersions(strict, a)
	}
	if profiles != nil {
		old := map[string]ProxyProfile{}
		newEligible := false
		for _, p := range s.listProxyProfilesLocked() {
			old[p.ID] = p
		}
		for _, p := range profiles {
			previous, ok := old[p.ID]
			if p.Enabled && (!ok || !previous.Enabled || strictProxyIdentity(previous.URL) != strictProxyIdentity(p.URL)) {
				newEligible = true
			}
			if !ok || strictProxyIdentity(previous.URL) != strictProxyIdentity(p.URL) || previous.Enabled != p.Enabled || previous.AutoDeleteProtected != p.AutoDeleteProtected {
				strict.Generations[p.ID]++
			}
		}
		if newEligible {
			pending = s.rebindStrictPendingLocked(profiles, pending, strict)
		}
	}
	for _, a := range pending {
		trackStrictAccountVersions(strict, a)
	}
	values := map[string]string{}
	initializations := s.initializationProxyMutationsLocked(profiles, pending, strict)
	strictRaw, err := marshalJSON(strict)
	if err != nil {
		return err
	}
	values[strictProxyStateKey] = string(strictRaw)
	if profiles != nil {
		raw, err := marshalJSON(profiles)
		if err != nil {
			return err
		}
		values["proxy_profiles"] = string(raw)
	}
	if history != nil {
		raw, err := marshalJSON(history)
		if err != nil {
			return err
		}
		values[riskProxyHistoryKey] = string(raw)
	}
	if sub != nil {
		raw, err := marshalJSON(sub)
		if err != nil {
			return err
		}
		values[proxysub.StateKey] = string(raw)
	}
	var oldProfiles []ProxyProfile
	if profiles != nil {
		oldProfiles = s.listProxyProfilesLocked()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for key, value := range values {
		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", key, value); err != nil {
			return err
		}
	}
	for _, a := range pending {
		if err = persistAccountOnTx(tx, a); err != nil {
			return err
		}
	}
	for _, task := range initializations {
		if err = persistInitializationOnTx(tx, task); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if history != nil {
		s.riskProxyHistory = history
	}
	s.strictProxy = *strict
	for id, task := range initializations {
		s.initializations[id] = task
	}
	s.publishCredentialGenerationsLocked()
	for key, value := range values {
		if key == strictProxyStateKey || key == riskProxyHistoryKey || key == proxySoftFailureKey {
			continue
		}
		s.settings[key] = value
	}
	if len(values) > 0 {
		s.publishSettings()
	}
	for _, a := range pending {
		live := s.findLocked(a.Provider, a.ID)
		live.ProxyID, live.ProxyURL = a.ProxyID, a.ProxyURL
	}
	if profiles != nil {
		s.retireProxyTransportsLocked(oldProfiles, profiles)
	}
	return nil
}

func (s *Store) SubscriptionOrigins() map[string]proxysub.Origin {
	snap, err := s.SubscriptionSnapshot()
	if err != nil {
		return map[string]proxysub.Origin{}
	}
	return snap.State.Registry
}

// 编译期锁定 Store 与同步模块的持久化契约。
var _ proxysub.Repository = (*Store)(nil)
