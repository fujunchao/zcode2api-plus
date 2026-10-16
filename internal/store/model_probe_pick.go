package store

import "time"

// 仅在尚未开始真实请求时允许重选；成对对照开始后仍冻结主体与代次。
func (s *Store) prepareAutomaticProbeLocked(r *ModelProbeRun, b probeBudget, candidates []probeAccountRow, now time.Time) string {
	if s.modelProbe.PausedUntil > now.UnixMilli() {
		return "reference_unavailable"
	}
	if b.Total+b.Reserved >= r.policy.GlobalDailyLimit {
		return "budget_exhausted"
	}
	r.accounts, r.accountIndex = nil, 0
	r.credentials, r.bindings = map[string]string{}, map[string]uint64{}
	reason := "no_eligible_account"
	for _, row := range candidates {
		if !row.view.Eligible {
			switch row.view.Reason {
			case "account_budget_exhausted":
				reason = "account_budget_exhausted"
			case "business_busy", "probe_busy":
				if reason == "no_eligible_account" {
					reason = "accounts_busy"
				}
			}
			continue
		}
		id := row.view.ID
		r.accounts = append(r.accounts, id)
		r.credentials[id] = s.strictCredentialRefLocked(s.findAnyLocked(id))
		r.bindings[id] = s.strictProxy.RouteVersions[id].Generation
		if len(r.accounts) == 2 {
			break
		}
	}
	if len(r.accounts) == 0 {
		return reason
	}
	if s.modelProbe.PausedUntil > 0 {
		r.referenceID = s.probeReferenceLocked(r.policy, r.target, now)
		if r.referenceID == "" {
			return "reference_unavailable"
		}
		r.phase, r.round = "reference", 1
	}
	r.Deadline = now.Add(20 * time.Minute).UnixMilli()
	r.Reason = "queued"
	return ""
}
