package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"time"

	"zcode2api/internal/model"
)

// InitializationAttempt 冻结本次账号、线路及执行权；UI 修订号不代替执行令牌。
type InitializationAttempt struct {
	Task    *InitializationTask
	Account *model.Account
	Egress  ProxyEgress
}

func (s *Store) replaceInitializationAccountLocked(a *model.Account) {
	for i, old := range s.accounts[a.Provider] {
		if old.ID == a.ID {
			s.accounts[a.Provider][i] = a
			return
		}
	}
}

// commitInitializationLocked 在同一事务内保存任务、账号补丁与严格代次。
// a 必须是在锁内从最新账号克隆的副本，提交前不能发布到调用方。
func (s *Store) commitInitializationLocked(t *InitializationTask, a *model.Account, next strictProxyState) error {
	if a != nil {
		trackStrictAccountVersions(&next, a)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a != nil {
		if err = persistAccountOnTx(tx, a); err != nil {
			return err
		}
	}
	if err = persistInitializationOnTx(tx, t); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", strictProxyStateKey, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.strictProxy = next
	s.initializations[t.AccountID] = t
	if a != nil {
		a.CredentialGeneration = next.Credentials[a.ID].Generation
		a.Initialization = s.initializationViewLocked(a)
		s.replaceInitializationAccountLocked(a)
	}
	return nil
}

func (s *Store) initializationCandidatesLocked(t *InitializationTask) []InitializationCandidate {
	var candidates []InitializationCandidate
	seen := map[string]bool{}
	currentDefault := s.initializationDefaultIdentityLocked(t)
	for _, p := range s.listProxyProfilesLocked() {
		identity := strictProxyIdentity(p.URL)
		if p.ID == t.DefaultProxyID || identity == t.DefaultIdentity || identity == currentDefault || seen[identity] ||
			(t.Policy.PoolMode == "selected" && !slices.Contains(t.Policy.ProxyIDs, p.ID)) {
			continue
		}
		if _, ok := s.chooseProxyLocked([]ProxyProfile{p}, nil, "", ""); !ok {
			continue
		}
		seen[identity] = true
		candidates = append(candidates, InitializationCandidate{ID: p.ID, Identity: identity, Generation: p.Generation})
	}
	if len(candidates) > 0 {
		start := s.rotation[initializationIntent] % len(candidates)
		candidates = append(slices.Clone(candidates[start:]), candidates[:start]...)
	}
	if len(candidates) > t.Policy.MaxAttempts {
		candidates = candidates[:t.Policy.MaxAttempts]
	}
	return candidates
}

func (s *Store) initializationDefaultIdentityLocked(t *InitializationTask) string {
	for _, p := range s.listProxyProfilesLocked() {
		if p.ID == t.DefaultProxyID {
			return strictProxyIdentity(p.URL)
		}
	}
	return t.DefaultIdentity
}

func initializationHealthy(a *model.Account, now time.Time) bool {
	return a != nil && a.Mode == "jwt" && a.Enabled && a.ArchivedAt == nil && model.IsManageable(a.Status) &&
		(a.Status != model.StatusCooling || (a.CoolingUntil != nil && *a.CoolingUntil <= float64(now.UnixNano())/1e9))
}

func (s *Store) InitializationQueue() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []*InitializationTask
	for _, t := range s.initializations {
		if !t.Managed || t.Paused || t.State == "completed" || (t.State == "waiting" && t.Reason != "no_candidates") {
			continue
		}
		jobs = append(jobs, t)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].LastAttemptAt != jobs[j].LastAttemptAt {
			return jobs[i].LastAttemptAt < jobs[j].LastAttemptAt
		}
		if jobs[i].CreatedAt != jobs[j].CreatedAt {
			return jobs[i].CreatedAt < jobs[j].CreatedAt
		}
		return jobs[i].AccountID < jobs[j].AccountID
	})
	ids := make([]string, len(jobs))
	for i, t := range jobs {
		ids[i] = t.AccountID
	}
	return ids
}

func (s *Store) BeginInitialization(id string, now time.Time) (*InitializationAttempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.initializations[id]
	if old == nil || !old.Managed || old.Paused || old.State == "completed" || old.GrantedAt > 0 || s.initializationRuns[id] != "" {
		return nil, nil
	}
	p, err := s.initializationPolicyLocked()
	if err != nil {
		return nil, err
	}
	if !p.Enabled || !s.ClaimAutoEnabled() || old.NextAt > now.UnixMilli() {
		return nil, nil
	}
	a := s.findAnyLocked(id)
	if !initializationHealthy(a, now) {
		return nil, nil
	}
	t := old.Clone()
	next := s.cloneStrictProxyLocked()
	wait := func(reason string) (*InitializationAttempt, error) {
		if t.State == "waiting" && t.Reason == reason {
			return nil, nil
		}
		t.State, t.Reason, t.RunID = "waiting", reason, ""
		t.Revision++
		return nil, s.commitInitializationLocked(t, nil, next)
	}
	if a.CredentialGeneration != t.CredentialGeneration {
		return wait("credentials_changed")
	}
	if t.Deadline != 0 && now.UnixMilli() >= t.Deadline {
		return wait("budget_exhausted")
	}
	if t.State == "waiting" && t.Reason != "no_candidates" {
		return nil, nil
	}
	copy := a.Clone()
	resume := t.Uncertain || t.State == "attempting" || t.State == "verifying"
	if resume {
		if derefStr(a.ProxyID) != t.CurrentProxyID || !s.proxyBindingAvailableLocked(a) ||
			s.strictProxy.Generations[t.CurrentProxyID] != t.CurrentProxyGeneration || s.strictProxy.RouteVersions[id].Generation != t.RouteGeneration {
			return wait("route_changed")
		}
		t.State = "verifying"
	} else {
		if t.Attempts >= t.Policy.MaxAttempts {
			return wait("attempts_exhausted")
		}
		if len(t.Candidates) == 0 {
			t.Candidates = s.initializationCandidatesLocked(t)
		}
		if len(t.Candidates) == 0 {
			return wait("no_candidates")
		}
		var selected *ProxyProfile
		currentDefault := s.initializationDefaultIdentityLocked(t)
		for _, candidate := range t.Candidates {
			if slices.Contains(t.Tried, candidate.Identity) || candidate.Identity == currentDefault || candidate.Identity == t.DefaultIdentity {
				continue
			}
			for _, profile := range s.listProxyProfilesLocked() {
				if profile.ID != candidate.ID || profile.Generation != candidate.Generation || strictProxyIdentity(profile.URL) != candidate.Identity {
					continue
				}
				if _, ok := s.chooseProxyLocked([]ProxyProfile{profile}, nil, "", ""); ok {
					p := profile
					selected = &p
					break
				}
			}
			if selected != nil {
				t.Tried = append(t.Tried, candidate.Identity)
				break
			}
		}
		if selected == nil {
			return wait("candidates_exhausted")
		}
		copy.ProxyID, copy.ProxyURL = &selected.ID, &selected.URL
		setStrictIntent(&next, copy, initializationIntent)
		trackStrictAccountVersions(&next, copy)
		t.Attempts++
		t.CurrentProxyID, t.CurrentProxyGeneration = selected.ID, selected.Generation
		t.RouteGeneration = next.RouteVersions[id].Generation
		t.State = "attempting"
		if t.Deadline == 0 {
			t.Deadline = now.Add(time.Duration(t.Policy.RoundBudgetSeconds) * time.Second).UnixMilli()
		}
	}
	t.RunID = randomHex(16)
	t.Revision++
	t.Reason = "running"
	t.LastAttemptAt = now.UnixMilli()
	if err := s.commitInitializationLocked(t, copy, next); err != nil {
		return nil, err
	}
	s.initializationRuns[id] = t.RunID
	if !resume {
		s.rotation[initializationIntent]++
	}
	return &InitializationAttempt{Task: t.Clone(), Account: s.snapshotLocked(copy), Egress: s.proxyEgressLocked(copy)}, nil
}

func (s *Store) initializationAttemptCurrentLocked(a *InitializationAttempt) bool {
	if a == nil || a.Task == nil || a.Account == nil {
		return false
	}
	t := s.initializations[a.Task.AccountID]
	if t == nil || !t.Managed || t.Paused || t.RunID == "" || t.RunID != a.Task.RunID || s.initializationRuns[t.AccountID] != t.RunID {
		return false
	}
	live := s.findAnyLocked(t.AccountID)
	return live != nil && live.AcceptsRequestResult(a.Account) && live.Status != model.StatusInvalid && s.proxyDispatchCurrentLocked(a.Account, a.Egress)
}

func (s *Store) ReleaseInitialization(a *InitializationAttempt) {
	if a == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initializationRuns[a.Task.AccountID] == a.Task.RunID {
		delete(s.initializationRuns, a.Task.AccountID)
	}
}

// RecordInitializationClaim 在发送有副作用的 POST 之前落意图，崩溃恢复只允许先复查。
func (s *Store) RecordInitializationClaim(a *InitializationAttempt, plan string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initializationAttemptCurrentLocked(a) {
		return ErrInitializationConflict
	}
	t := s.initializations[a.Task.AccountID].Clone()
	if !slices.Contains(t.PlanIDs, plan) {
		t.PlanIDs = append(t.PlanIDs, plan)
	}
	t.Uncertain = true
	t.State = "verifying"
	t.Revision++
	return s.commitInitializationLocked(t, nil, s.cloneStrictProxyLocked())
}

func (s *Store) ObserveInitialization(a *InitializationAttempt, observation model.InitialQuotaObservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initializationAttemptCurrentLocked(a) {
		return ErrInitializationConflict
	}
	if observation.Kind == "unknown" || observation.Snapshot == nil || observation.CheckedAt <= 0 {
		return nil
	}
	if !observation.Snapshot.SameCredential(a.Account) {
		return ErrInitializationConflict
	}
	t := s.initializations[a.Task.AccountID].Clone()
	live := s.findAnyLocked(t.AccountID).Clone()
	value := observation.Snapshot.Clone()
	live.Quota, live.Plans, live.Plan = value.Quota, value.Plans, value.Plan
	live.LastCheckedAt = &observation.CheckedAt
	live.SyncExhaustedModels()
	if observation.Kind == "allocated" && live.Status == model.StatusActive {
		live.Status = model.StatusExhausted
	}
	if observation.Kind == "available" {
		t.GrantedAt = time.Now().UnixMilli()
		t.Uncertain = false
		t.State = "awaiting_default"
		t.Reason = "quota_confirmed"
		if live.Status == model.StatusExhausted {
			live.Status = model.StatusActive
		}
		live.LastError = nil
	}
	t.Revision++
	return s.commitInitializationLocked(t, live, s.cloneStrictProxyLocked())
}

func (s *Store) SaveInitializationClaim(a *InitializationAttempt, succeeded bool, nextAt int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initializationAttemptCurrentLocked(a) {
		return ErrInitializationConflict
	}
	t := s.initializations[a.Task.AccountID].Clone()
	live := s.findAnyLocked(t.AccountID).Clone()
	state := live.ClaimView()
	if state == nil {
		state = &model.ClaimState{}
	}
	next := *state
	if succeeded {
		at := float64(time.Now().UnixNano()) / 1e9
		next.ClaimedAt = &at
		next.LastError = nil
	} else {
		next.LastError = &reason
	}
	if nextAt > 0 {
		at := float64(nextAt) / 1000
		next.NextAt = &at
	}
	live.SetClaimState(&next)
	t.Revision++
	return s.commitInitializationLocked(t, live, s.cloneStrictProxyLocked())
}

func (s *Store) EndInitialization(a *InitializationAttempt, reason, message string, nextAt int64, retry bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initializationAttemptCurrentLocked(a) {
		return ErrInitializationConflict
	}
	t := s.initializations[a.Task.AccountID].Clone()
	t.RunID = ""
	t.Revision++
	t.Reason = reason
	t.LastError = message
	t.NextAt = max(t.NextAt, nextAt)
	if t.GrantedAt > 0 {
		t.State = "awaiting_default"
	} else if retry && !t.Uncertain {
		t.State = "queued"
	} else {
		t.State = "waiting"
	}
	if reason == "historical_quota" && !t.Uncertain {
		t.Managed = false
		t.State = "skipped"
	}
	next := s.cloneStrictProxyLocked()
	if !t.Managed {
		setStrictIntent(&next, s.findAnyLocked(t.AccountID), "auto_proxy")
	}
	return s.commitInitializationLocked(t, nil, next)
}

func (s *Store) BindInitialization(id string, revision uint64, currentDefault bool, explicit ...bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.initializations[id]
	if old == nil || !old.Managed || old.Revision != revision {
		return false, ErrInitializationConflict
	}
	if old.GrantedAt == 0 {
		return false, errors.New("尚未确认额度到账，不能绑定业务代理")
	}
	manual := len(explicit) > 0 && explicit[0]
	if old.Paused && !manual {
		return false, ErrInitializationUnavailable
	}
	if old.State == "completed" && !currentDefault {
		return true, nil
	}
	policy, policyErr := s.initializationPolicyLocked()
	if policyErr != nil {
		return false, policyErr
	}
	if !manual && (!policy.Enabled || !s.ClaimAutoEnabled()) {
		return false, ErrInitializationUnavailable
	}
	live := s.findAnyLocked(id)
	if !initializationHealthy(live, time.Now()) || live.CredentialGeneration != old.CredentialGeneration {
		return false, ErrInitializationUnavailable
	}
	t := old.Clone()
	next := s.cloneStrictProxyLocked()
	if currentDefault {
		p, err := s.initializationPolicyLocked()
		if err != nil {
			return false, err
		}
		t.DefaultProxyID = p.DefaultProxyID
	}
	var selected []ProxyProfile
	for _, p := range s.listProxyProfilesLocked() {
		if p.ID == t.DefaultProxyID {
			selected = append(selected, p)
		}
	}
	p, ok := s.chooseProxyLocked(selected, nil, "", "")
	if !ok {
		return false, ErrInitializationUnavailable
	}
	a := live.Clone()
	a.ProxyID, a.ProxyURL = &p.ID, &p.URL
	t.DefaultIdentity = strictProxyIdentity(p.URL)
	t.State, t.Reason, t.RunID = "completed", "ready", ""
	t.Revision++
	t.Paused = false
	t.LastError = ""
	setStrictIntent(&next, a, fixedProxyIntent)
	if err := s.commitInitializationLocked(t, a, next); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ChangeInitialization(id string, revision uint64, action string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.initializations[id]
	if old == nil || !old.Managed || old.Revision != revision {
		return ErrInitializationConflict
	}
	t := old.Clone()
	switch action {
	case "pause":
		if t.State == "completed" {
			return errors.New("已完成账号无需暂停初始化")
		}
		t.Paused = true
	case "retry":
		if t.GrantedAt > 0 {
			return errors.New("已到账账号请重试固定绑定")
		}
		if t.NextAt > time.Now().UnixMilli() || t.Reason == "ineligible" {
			return errors.New("上游资格或等待条件尚未解除")
		}
		a := s.findAnyLocked(id)
		if !initializationHealthy(a, time.Now()) {
			return ErrInitializationUnavailable
		}
		t.CredentialGeneration = a.CredentialGeneration
		t.Paused = false
		t.State = "queued"
		t.Reason = "queued"
		t.LastError = ""
		t.Deadline = 0
		if !t.Uncertain {
			t.Candidates = nil
			t.Tried = nil
			t.Attempts = 0
			t.PlanIDs = nil
		}
	default:
		return errors.New("未知初始化动作")
	}
	t.Revision++
	t.RunID = ""
	return s.commitInitializationLocked(t, nil, s.cloneStrictProxyLocked())
}

func (s *Store) AcquireInitializationDispatch(a *InitializationAttempt) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.initializationPolicyLocked()
	if err != nil {
		return nil, err
	}
	if !p.Enabled || !s.ClaimAutoEnabled() || !s.initializationAttemptCurrentLocked(a) {
		return nil, ErrInitializationConflict
	}
	return s.acquireProxyDispatchLocked(a.Account, a.Egress)
}

func (s *Store) GuardInitializationClient(client *http.Client, attempt *InitializationAttempt) *http.Client {
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &strictProxyTransport{store: s, account: attempt.Account.Clone(), egress: attempt.Egress, base: base, initialization: attempt}
	return &copy
}
