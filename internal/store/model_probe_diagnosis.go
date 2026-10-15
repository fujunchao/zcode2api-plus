package store

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
	"zcode2api/internal/web"
)

func validProbeObservation(l *ModelProbeLease, o ProxyObservation) bool {
	return probeTargetMatches(l.targetURL, o) && o.ContentRef != "" && o.ProfileRef != "" && o.DeviceRef != "" && o.SessionRef != "" && o.EntryRef == l.EntryID()
}

func probeTargetMatches(target string, o ProxyObservation) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return strings.EqualFold(o.TargetOrigin, u.Scheme+"://"+u.Host) && o.TargetPath == path && o.Method == "POST" && o.Model == ModelProbeModel
}

func (s *Store) freshModelProbeReferenceLocked(r *ModelProbeRun, account string, now time.Time) bool {
	for i := len(r.proofs) - 1; i >= 0; i-- {
		e := r.proofs[i]
		if e.role == "reference" && e.round == r.round && e.account == account && probeSuccess(e.ProxyObservation) && s.probeEvidenceCurrentLocked(e, now) && !e.At.Before(now.Add(-2*time.Minute)) {
			for _, other := range s.modelProbe.runs {
				for _, bad := range other.proofs {
					if bad.route == e.route && probeRisk(bad.ProxyObservation) && probeComparable(e.ProxyObservation, bad.ProxyObservation) && s.probeEvidenceCurrentLocked(bad, now) && bad.At.After(e.At) {
						return false
					}
				}
			}
			return true
		}
	}
	return false
}

func probeRisk(o ProxyObservation) bool {
	return o.Status == 405 && o.BusinessCode == "3012" && !o.InvalidRequest
}
func probeSuccess(o ProxyObservation) bool {
	return o.Complete && o.Status >= 200 && o.Status < 300 && o.BusinessCode == "" && !o.InvalidRequest
}
func probeComparable(a, b ProxyObservation) bool {
	return a.TargetOrigin == b.TargetOrigin && a.TargetPath == b.TargetPath && a.Method == b.Method && a.Model == b.Model && a.ProfileRef == b.ProfileRef
}
func probeSameInput(a, b modelProbeEvidence) bool {
	return probeComparable(a.ProxyObservation, b.ProxyObservation) && a.credential == b.credential && a.DeviceRef == b.DeviceRef && a.SessionRef == b.SessionRef && a.ContentRef == b.ContentRef
}

func probeRecheckAt(a *model.Account, now time.Time) int64 {
	at := now.Add(5 * time.Minute).UnixMilli()
	if a != nil {
		if a.CoolingUntil != nil {
			at = max(at, int64(*a.CoolingUntil*1000)+1)
		}
		if until := a.RiskControlSilentUntil(config.RiskSilentWindow()); until != nil {
			at = max(at, int64(*until*1000)+1)
		}
	}
	return at
}

func (s *Store) advanceModelProbeLocked(r *ModelProbeRun, l *ModelProbeLease, o ProxyObservation, kind string, a *model.Account, now time.Time) {
	wait := func(phase string, round int, at int64) {
		r.phase, r.round, r.State, r.Reason, r.NextAt = phase, round, "waiting", "awaiting_control", at
		if at >= r.Deadline {
			r.State, r.Reason = "completed", "account_cooldown_exceeds_budget"
		}
	}
	if kind == "canceled" || kind == "dispatch_changed" || kind == "budget_exhausted" {
		return
	}
	switch l.role {
	case "screen":
		if probeSuccess(o) {
			r.Verdict = "healthy_recent"
			return
		}
		if !probeRisk(o) {
			if kind == "risk_control" {
				r.Verdict = "suspect"
			}
			return
		}
		r.Verdict = "suspect"
		if now.UnixMilli() < s.modelProbe.LastDiagnosis[r.TargetID]+int64(time.Hour/time.Millisecond) {
			r.Reason = "diagnosis_rate_limit"
			return
		}
		r.referenceID = s.probeReferenceLocked(r.policy, r.target)
		if r.referenceID == "" {
			r.Reason = "no_reference"
			return
		}
		s.modelProbe.LastDiagnosis[r.TargetID] = now.UnixMilli()
		wait("reference", 1, probeRecheckAt(a, now))
	case "reference":
		if probeSuccess(o) {
			s.modelProbe.PausedUntil = 0
			wait("target", r.round, now.UnixMilli())
			return
		}
		r.accountIndex++
		if r.accountIndex < len(r.accounts) {
			wait("reference", 1, now.UnixMilli())
			return
		}
		r.Verdict, r.Reason = "inconclusive", "reference_unavailable"
		s.modelProbe.PausedUntil = now.Add(30 * time.Minute).UnixMilli()
	case "target":
		if probeSuccess(o) {
			r.Verdict = "mixed"
			if s.probeAccountCorrelatedLocked(r, l.account.ID, now) {
				r.Verdict = "account_correlated"
			}
			r.Reason = r.Verdict
			return
		}
		if !probeRisk(o) {
			return
		}
		r.Verdict = "suspect"
		r.ConfirmedRounds, _ = s.probeConfirmedRoundsLocked(r, now)
		if r.ConfirmedRounds >= 2 {
			r.Verdict, r.Reason = "route_correlated", "qualified"
			return
		}
		if r.round >= 2 {
			r.Verdict, r.Reason = "inconclusive", "evidence_incomplete_or_contradicted"
			return
		}
		wait("reference", r.round+1, probeRecheckAt(a, now))
	}
}

func (s *Store) probeEvidenceCurrentLocked(e modelProbeEvidence, now time.Time) bool {
	if e.At.Before(now.Add(-10*time.Minute)) || e.At.After(now) || e.RequestID == "" {
		return false
	}
	a := s.findAnyLocked(e.account)
	if a == nil || s.strictCredentialRefLocked(a) != e.credential {
		return false
	}
	if e.route == ModelProbeDirect {
		return true
	}
	p, ok := s.probeProfileLocked(e.route)
	return ok && p.Generation == e.generation && strictProxyIdentity(p.URL) == e.identity
}

// 成对轮次是唯一正证据；其他账号或其他任务在同目标上的成功只提供反证。
func (s *Store) probeConfirmedRoundsLocked(r *ModelProbeRun, now time.Time) (int, []string) {
	var failures []modelProbeEvidence
	for _, e := range r.proofs {
		if e.role == "target" && probeRisk(e.ProxyObservation) && s.probeEvidenceCurrentLocked(e, now) {
			failures = append(failures, e)
		}
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].At.Before(failures[j].At) })
	if len(failures) == 0 {
		return 0, nil
	}
	seed := failures[len(failures)-1]
	for _, other := range s.modelProbe.runs {
		for _, e := range other.proofs {
			if e.identity == strictProxyIdentity(r.target.URL) && probeSuccess(e.ProxyObservation) && probeComparable(seed.ProxyObservation, e.ProxyObservation) && s.probeEvidenceCurrentLocked(e, now) {
				return 0, nil
			}
		}
	}
	if good, ok := s.recentProbeBusinessLocked(r.target, now, 10*time.Minute); ok && probeComparable(seed.ProxyObservation, good) {
		return 0, nil
	}
	seen := map[string]bool{}
	rounds := map[int]bool{}
	refs := []string{}
	var previous time.Time
	for _, bad := range failures {
		if !probeSameInput(seed, bad) || seen[bad.RequestID] || rounds[bad.round] || (!previous.IsZero() && bad.At.Sub(previous) < 5*time.Minute) {
			continue
		}
		for _, good := range r.proofs {
			if good.role != "reference" || good.round != bad.round || good.route == r.TargetID || !probeSuccess(good.ProxyObservation) || !probeSameInput(bad, good) || !s.probeEvidenceCurrentLocked(good, now) || good.At.After(bad.At) || good.At.Before(bad.At.Add(-2*time.Minute)) {
				continue
			}
			valid := true
			for _, other := range s.modelProbe.runs {
				for _, e := range other.proofs {
					if e.route == good.route && probeRisk(e.ProxyObservation) && probeComparable(good.ProxyObservation, e.ProxyObservation) && s.probeEvidenceCurrentLocked(e, now) && e.At.After(good.At) {
						valid = false
					}
				}
			}
			if !valid {
				continue
			}
			seen[bad.RequestID] = true
			rounds[bad.round] = true
			previous = bad.At
			refs = append(refs, good.RequestID, bad.RequestID)
			break
		}
	}
	return len(rounds), refs
}

func (s *Store) probeAccountCorrelatedLocked(r *ModelProbeRun, goodAccount string, now time.Time) bool {
	goodRoutes := map[string]bool{}
	for _, e := range r.proofs {
		if e.account == goodAccount && probeSuccess(e.ProxyObservation) && s.probeEvidenceCurrentLocked(e, now) {
			goodRoutes[e.route] = true
		}
	}
	if !goodRoutes[r.TargetID] || !goodRoutes[r.referenceID] {
		return false
	}
	for _, id := range r.accounts {
		if id == goodAccount {
			continue
		}
		badRoutes := map[string]bool{}
		for _, e := range r.proofs {
			if e.account == id && probeRisk(e.ProxyObservation) && s.probeEvidenceCurrentLocked(e, now) {
				badRoutes[e.route] = true
			}
		}
		if badRoutes[r.TargetID] && badRoutes[r.referenceID] {
			return true
		}
	}
	return false
}

func (s *Store) retireModelProbeLocked(r *ModelProbeRun, now time.Time) error {
	if r.policy.Mode != "auto_retire" {
		r.Reason = "qualified_observe"
		return s.persistModelProbeRunLocked(r)
	}
	if !s.StrictProxyEnabled() || s.modelProbe.PausedUntil > now.UnixMilli() {
		r.Reason = "decision_disabled"
		return s.persistModelProbeRunLocked(r)
	}
	if now.UnixMilli() < s.strictProxy.EvidenceIncompleteUntil {
		r.Reason = "evidence_incomplete"
		return s.persistModelProbeRunLocked(r)
	}
	count, refs := s.probeConfirmedRoundsLocked(r, now)
	if count < 2 {
		r.Reason = "evidence_incomplete_or_contradicted"
		return s.persistModelProbeRunLocked(r)
	}
	target := ""
	for _, e := range r.proofs {
		if e.role == "target" {
			target = e.TargetOrigin
			break
		}
	}
	d, err := s.startStrictRetirementLocked(strictProxyIdentity(r.target.URL), r.target.URL, "model_probe_route_control", target, ModelProbeRuleVersion, refs, now)
	if err != nil {
		r.Reason = "persist_failed"
		_ = s.persistModelProbeRunLocked(r)
		return err
	}
	r.DecisionID, r.Reason = d.DecisionID, "retired"
	if err := s.persistModelProbeRunLocked(r); err != nil {
		return err
	}
	web.Warn("model-probe", fmt.Sprintf("rule=%s decision=%s reason=%s removed=%d reassigned=%d direct_fallback=%d", d.RuleVersion, d.DecisionID, d.Reason, len(d.Removed), d.Assigned, d.Direct))
	return nil
}
