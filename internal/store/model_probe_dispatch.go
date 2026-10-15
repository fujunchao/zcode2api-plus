package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

type modelProbeEvidence struct {
	ProxyObservation
	account, route, role, credential, identity string
	generation                                 uint64
	round                                      int
}

// ModelProbeLease 只能由 Store 签发；账号的临时路由不发布到持久绑定。
type ModelProbeLease struct {
	store                                                     *Store
	id, runID, role, routeID, identity, targetURL, credential string
	account                                                   *model.Account
	profile                                                   ProxyProfile
	round                                                     int
	revision                                                  uint64
	started                                                   bool
	requestID                                                 string
	startedAt                                                 time.Time
	bindingGeneration                                         uint64
}

func (l *ModelProbeLease) Account() *model.Account { return l.account.Clone() }
func (l *ModelProbeLease) ProxyURL() string        { return l.profile.URL }
func (l *ModelProbeLease) TargetURL() string       { return l.targetURL }
func (l *ModelProbeLease) RunID() string           { return l.runID }
func (l *ModelProbeLease) SessionID() string {
	return "probe-" + strictDigest(l.runID, l.account.ID)[:32]
}
func (l *ModelProbeLease) EntryID() string {
	return l.runID + ":" + l.role + ":" + l.account.ID + ":" + stringID(l.round)
}
func stringID(i int) string { raw, _ := json.Marshal(i); return string(raw) }

func (s *Store) probeAccountReadyLocked(id string, now time.Time) bool {
	a := s.snapshotLocked(s.findAnyLocked(id))
	if a == nil || !s.isModelProbeAccountLocked(id) || s.initializationManagedLocked(id) || a.Mode != "jwt" || a.Secret() == "" {
		return false
	}
	a.ModelProbeOnly = false
	available, known := a.UsableQuotaForModel(ModelProbeModel)
	return a.IsModelSelectable(ModelProbeModel, now) && !a.InRiskControlSilentWindow(now, config.RiskSilentWindow()) && known && available > 0
}

func (s *Store) probeProfileLocked(id string) (ProxyProfile, bool) {
	for _, p := range s.listProxyProfilesLocked() {
		if p.ID == id {
			return p, p.Enabled && !s.strictProxyBlockedLocked(p.URL)
		}
	}
	return ProxyProfile{}, false
}

func (s *Store) QueueModelProbe(target, source string, now time.Time) (ModelProbeRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queueModelProbeLocked(target, source, now)
}

func (s *Store) queueModelProbeLocked(target, source string, now time.Time) (ModelProbeRun, error) {
	p := s.modelProbe.Policy
	if s.strictClosed || !p.Enabled || !slices.Contains(p.TargetProxyIDs, target) {
		return ModelProbeRun{}, ErrModelProbeUnavailable
	}
	profile, ok := s.probeProfileLocked(target)
	if !ok {
		return ModelProbeRun{}, ErrModelProbeUnavailable
	}
	open := 0
	for _, r := range s.modelProbe.runs {
		if probeRunOpen(r) {
			open++
			if r.TargetID == target && r.policy.Revision == p.Revision {
				return r.view(), nil
			}
		}
	}
	if open >= 1000 {
		return ModelProbeRun{}, errors.New("模型巡检等待队列已满")
	}
	r := &ModelProbeRun{ID: "probe-" + randomHex(12), TargetID: target, Source: source, State: "queued", Verdict: "unknown", Reason: "queued", Mode: p.Mode,
		RuleVersion: ModelProbeRuleVersion, CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(), NextAt: now.UnixMilli(), Deadline: now.Add(20 * time.Minute).UnixMilli(), Samples: []ModelProbeSample{}, policy: p.clone(), target: profile, phase: "screen",
		credentials: map[string]string{}, bindings: map[string]uint64{}, targetURL: config.UpstreamZai}
	for _, id := range p.AccountIDs {
		if s.probeAccountReadyLocked(id, now) {
			r.accounts = append(r.accounts, id)
			r.credentials[id] = s.strictCredentialRefLocked(s.findAnyLocked(id))
			r.bindings[id] = s.strictProxy.RouteVersions[id].Generation
			if len(r.accounts) == 2 {
				break
			}
		}
	}
	if len(r.accounts) == 0 {
		r.State, r.Reason = "skipped", "no_eligible_account"
	}
	if source != "manual" {
		if good, ok := s.modelProbe.business[target]; ok && good.At.After(now.Add(-30*time.Minute)) && !s.modelProbe.hints[target].After(good.At) {
			r.State, r.Verdict, r.Reason = "skipped", "healthy_recent", "skipped_recent_success"
		}
	}
	if s.modelProbe.PausedUntil > now.UnixMilli() {
		r.State, r.Verdict, r.Reason = "skipped", "inconclusive", "reference_unavailable"
	} else if s.modelProbe.PausedUntil > 0 {
		r.referenceID = s.probeReferenceLocked(p, profile)
		if r.referenceID != "" {
			r.phase, r.round = "reference", 1
		} else {
			r.State, r.Reason = "skipped", "reference_unavailable"
		}
	}
	if err := s.pruneModelProbeLocked(now); err != nil {
		return ModelProbeRun{}, err
	}
	if err := s.persistModelProbeRunLocked(r); err != nil {
		return ModelProbeRun{}, err
	}
	s.modelProbe.runs[r.ID] = r
	return r.view(), nil
}

// ScheduleModelProbes 在同一时钟下处理到期目标；间隔修改不需要重启。
func (s *Store) ScheduleModelProbes(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.modelProbe.Policy
	if !p.Enabled || s.strictClosed {
		return nil
	}
	changed := false
	for _, id := range p.TargetProxyIDs {
		due := s.modelProbe.NextDue[id]
		hint := s.modelProbe.hints[id]
		if due > now.UnixMilli() && (hint.IsZero() || now.UnixMilli() < s.modelProbe.LastDiagnosis[id]+int64(time.Hour/time.Millisecond)) {
			continue
		}
		if due == 0 {
			s.modelProbe.NextDue[id] = now.Add(time.Duration(p.IntervalMinutes) * time.Minute).UnixMilli()
			changed = true
			continue
		}
		if _, err := s.queueModelProbeLocked(id, "scheduled", now); err != nil && !errors.Is(err, ErrModelProbeUnavailable) {
			return err
		}
		// 确定性散列错峰，测试无随机时间依赖；区间为标称间隔的 80%–120%。
		seed := strictDigest(id, stringID(int(now.Unix()/60)))
		factor := 80 + int(seed[0])%41
		s.modelProbe.NextDue[id] = now.Add(time.Duration(p.IntervalMinutes) * time.Minute * time.Duration(factor) / 100).UnixMilli()
		delete(s.modelProbe.hints, id)
		changed = true
	}
	if changed {
		return s.persistProbeStateLocked()
	}
	return nil
}

// TakeModelProbe 只预留一个可运行阶段，真实出站前仍由 GuardModelProbeClient 复核。
func (s *Store) TakeModelProbe(now time.Time) (*ModelProbeLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed || !s.modelProbe.Policy.Enabled || len(s.modelProbe.leases) >= s.modelProbe.Policy.Concurrency {
		return nil, nil
	}
	var runs []*ModelProbeRun
	for _, r := range s.modelProbe.runs {
		if probeRunOpen(r) && r.State != "running" && r.NextAt <= now.UnixMilli() {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt < runs[j].CreatedAt })
	for _, r := range runs {
		finish := func(reason string) error {
			copy := *r
			copy.State, copy.Verdict, copy.Reason = "completed", "inconclusive", reason
			copy.UpdatedAt = now.UnixMilli()
			if err := s.persistModelProbeRunLocked(&copy); err != nil {
				return err
			}
			s.modelProbe.runs[r.ID] = &copy
			return nil
		}
		if r.policy.Revision != s.modelProbe.Policy.Revision {
			if err := finish("policy_changed"); err != nil {
				return nil, err
			}
			continue
		}
		if now.UnixMilli() >= r.Deadline || r.Attempts >= 8 {
			if err := finish("diagnosis_budget"); err != nil {
				return nil, err
			}
			continue
		}
		p, ok := s.probeProfileLocked(r.TargetID)
		if !ok || p.Generation != r.target.Generation || strictProxyIdentity(p.URL) != strictProxyIdentity(r.target.URL) {
			if err := finish("route_changed"); err != nil {
				return nil, err
			}
			continue
		}
		if r.accountIndex >= len(r.accounts) {
			if err := finish("no_eligible_account"); err != nil {
				return nil, err
			}
			continue
		}
		id := r.accounts[r.accountIndex]
		aCurrent := s.findAnyLocked(id)
		if aCurrent == nil || s.strictCredentialRefLocked(aCurrent) != r.credentials[id] || s.strictProxy.RouteVersions[id].Generation != r.bindings[id] || r.targetURL != config.UpstreamZai {
			if err := finish("account_or_target_changed"); err != nil {
				return nil, err
			}
			continue
		}
		if !s.probeAccountReadyLocked(id, now) {
			if err := finish("account_not_ready"); err != nil {
				return nil, err
			}
			continue
		}
		routeID, role := r.TargetID, r.phase
		if r.phase == "reference" {
			routeID = r.referenceID
		}
		profile := p
		if routeID == ModelProbeDirect {
			profile = ProxyProfile{}
		} else {
			profile, ok = s.probeProfileLocked(routeID)
			if !ok {
				if err := finish("reference_unavailable"); err != nil {
					return nil, err
				}
				continue
			}
		}
		busy := false
		for _, l := range s.modelProbe.leases {
			if l.account.ID == id || (routeID != ModelProbeDirect && l.identity == strictProxyIdentity(profile.URL)) {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		a := s.snapshotLocked(s.findAnyLocked(id))
		l := &ModelProbeLease{store: s, id: "attempt-" + randomHex(12), runID: r.ID, role: role, routeID: routeID, profile: profile, identity: strictProxyIdentity(profile.URL), account: a,
			credential: s.strictCredentialRefLocked(a), targetURL: r.targetURL, round: r.round, revision: r.policy.Revision, bindingGeneration: r.bindings[id]}
		copy := *r
		copy.State = "running"
		if err := s.persistModelProbeRunLocked(&copy); err != nil {
			return nil, err
		}
		s.modelProbe.runs[r.ID] = &copy
		s.modelProbe.leases[l.id] = l
		return l, nil
	}
	return nil, nil
}

func (s *Store) probeLeaseCurrentLocked(l *ModelProbeLease, now time.Time) bool {
	if l == nil || s.strictClosed || s.modelProbe.leases[l.id] != l || !s.modelProbe.Policy.Enabled || s.modelProbe.Policy.Revision != l.revision || !s.probeAccountReadyLocked(l.account.ID, now) {
		return false
	}
	a := s.findAnyLocked(l.account.ID)
	if a == nil || s.strictCredentialRefLocked(a) != l.credential || config.UpstreamZai != l.targetURL || s.strictProxy.RouteVersions[a.ID].Generation != l.bindingGeneration {
		return false
	}
	if l.routeID == ModelProbeDirect {
		return s.modelProbe.Policy.AllowDirectControl && l.role == "reference"
	}
	p, ok := s.probeProfileLocked(l.routeID)
	return ok && p.Generation == l.profile.Generation && strictProxyIdentity(p.URL) == l.identity
}

type modelProbeTransport struct {
	store *Store
	lease *ModelProbeLease
	base  http.RoundTripper
	now   func() time.Time
}

func (t *modelProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.store.startModelProbeHTTP(t.lease, req, t.now()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (s *Store) GuardModelProbeClient(client *http.Client, l *ModelProbeLease, now func() time.Time) *http.Client {
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &modelProbeTransport{s, l, base, now}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

func (s *Store) startModelProbeHTTP(l *ModelProbeLease, req *http.Request, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.probeLeaseCurrentLocked(l, now) || l.started || req.Method != http.MethodPost || req.URL.String() != l.targetURL || req.Header.Get("X-Request-Id") == "" || req.Header.Get("Authorization") != "Bearer "+l.account.Secret() || req.Header.Get("X-Api-Key") != l.account.Secret() {
		return ErrModelProbeConflict
	}
	r := s.modelProbe.runs[l.runID]
	if r == nil || r.Attempts >= 8 || now.UnixMilli() >= r.Deadline {
		return ErrModelProbeBudget
	}
	var total, account int
	if err := s.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(CASE WHEN account_id=? THEN 1 ELSE 0 END),0) FROM model_probe_attempts WHERE started_at>?", l.account.ID, now.Add(-24*time.Hour).UnixMilli()).Scan(&total, &account); err != nil {
		return err
	}
	if total >= r.policy.GlobalDailyLimit || account >= r.policy.AccountDailyLimit {
		return ErrModelProbeBudget
	}
	copy := *r
	copy.Attempts++
	copy.UpdatedAt = now.UnixMilli()
	raw, err := json.Marshal(&copy)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO model_probe_attempts(id,account_id,started_at) VALUES (?,?,?)", l.id, l.account.ID, now.UnixMilli()); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE model_probe_runs SET data=? WHERE id=?", string(raw), r.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.modelProbe.runs[r.ID] = &copy
	l.started, l.startedAt, l.requestID = true, now, req.Header.Get("X-Request-Id")
	if l.identity != "" {
		if s.strictDispatch == nil {
			s.strictDispatch = map[string]int{}
		}
		s.strictDispatch[l.identity]++
	}
	return nil
}

// CompleteModelProbe 接收可信发送器的结果；同一许可只结算一次，旧策略仅结算真实用量。
func (s *Store) CompleteModelProbe(l *ModelProbeLease, o ProxyObservation, kind string, usage model.Usage, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l == nil || s.modelProbe.leases[l.id] != l {
		return ErrModelProbeConflict
	}
	defer func() {
		delete(s.modelProbe.leases, l.id)
		if l.started && l.identity != "" {
			s.strictDispatch[l.identity]--
			if s.strictDispatch[l.identity] == 0 {
				delete(s.strictDispatch, l.identity)
			}
		}
	}()
	r := s.modelProbe.runs[l.runID]
	if r == nil {
		return ErrModelProbeConflict
	}
	copy := *r
	copy.Samples = slices.Clone(r.Samples)
	copy.proofs = slices.Clone(r.proofs)
	copy.UpdatedAt = now.UnixMilli()
	current := s.probeLeaseCurrentLocked(l, now)
	var account *model.Account
	if live := s.findAnyLocked(l.account.ID); live != nil && l.started {
		account = live.Clone()
		account.ModelProbeOnly = s.isModelProbeAccountLocked(l.account.ID)
		account.AccumulateTokens(usage)
		if current && account.AcceptsRequestResult(l.account) {
			if kind == "risk_control" {
				model.ApplyRiskControl(account, "模型巡检收到风控响应 HTTP 405", now, s.RiskCoolingSteps())
			}
			if kind == "success" && o.Complete {
				account.RiskControlStreak = 0
				if account.Status == model.StatusCooling {
					account.Status = model.StatusActive
				}
			}
		}
	}
	copy.Samples = append(copy.Samples, ModelProbeSample{RequestID: l.requestID, AccountRef: strictDigest(l.account.ID)[:12], RouteID: l.routeID, Role: l.role, Round: l.round, At: now.UnixMilli(), Status: o.Status, BusinessCode: o.BusinessCode, Kind: kind, Complete: o.Complete, Usage: usage, Stale: !current})
	o.At = now
	if current && l.started && !o.InvalidRequest && o.RequestID == l.requestID && validProbeObservation(l, o) {
		if (o.Complete && o.Status >= 200 && o.Status < 300 && o.BusinessCode == "") || (o.Status == 405 && o.BusinessCode == "3012") {
			copy.proofs = append(copy.proofs, modelProbeEvidence{ProxyObservation: o, account: l.account.ID, route: l.routeID, role: l.role, credential: l.credential, identity: l.identity, generation: l.profile.Generation, round: l.round})
		}
	}
	copy.State, copy.Verdict, copy.Reason = "completed", "inconclusive", kind
	if !current {
		copy.Reason = "stale_snapshot"
	} else {
		s.advanceModelProbeLocked(&copy, l, o, kind, account, now)
	}
	if err := s.commitProbeResultLocked(&copy, account); err != nil {
		return err
	}
	if err := s.persistProbeStateLocked(); err != nil {
		return err
	}
	if copy.Verdict == "route_correlated" {
		return s.retireModelProbeLocked(&copy, now)
	}
	return nil
}
