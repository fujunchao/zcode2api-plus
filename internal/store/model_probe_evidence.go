package store

import (
	"sort"
	"time"
)

const modelProbeEvidenceLimit = 4096

// 只让自动处置模式的未解决目标信号影响新增绑定；观察模式绝不改变业务路由。
func (s *Store) modelProbeSuspectsLocked(now time.Time) map[string]bool {
	out := map[string]bool{}
	if !s.modelProbe.Policy.Enabled || s.modelProbe.Policy.Mode != "auto_retire" || s.modelProbe.storageError {
		return out
	}
	bad, good := map[string]time.Time{}, map[string]time.Time{}
	for _, r := range s.modelProbe.runs {
		if r.policy.Revision != s.modelProbe.Policy.Revision {
			continue
		}
		for _, e := range r.proofs {
			if e.identity == "" || !s.probeEvidenceCurrentLocked(e, now) {
				continue
			}
			if e.role != "reference" && probeRisk(e.ProxyObservation) && e.At.After(bad[e.identity]) {
				bad[e.identity] = e.At
			}
			if probeSuccess(e.ProxyObservation) && e.At.After(good[e.identity]) {
				good[e.identity] = e.At
			}
		}
	}
	for id, at := range bad {
		if good[id].IsZero() || !at.Before(good[id]) {
			out[id] = true
		}
	}
	return out
}

// 私有判定缓存与七天展示历史分开限额，丢弃有效证据必须保留十分钟禁止决定窗口。
func (s *Store) boundModelProbeEvidenceLocked(now time.Time) error {
	type entry struct {
		run string
		e   modelProbeEvidence
	}
	var all []entry
	for id, r := range s.modelProbe.runs {
		kept := r.proofs[:0]
		for _, e := range r.proofs {
			if !e.At.Before(now.Add(-10 * time.Minute)) {
				kept = append(kept, e)
				all = append(all, entry{id, e})
			}
		}
		r.proofs = kept
	}
	if len(all) <= modelProbeEvidenceLimit {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].e.At.Before(all[j].e.At) })
	dropped := map[string]bool{}
	for _, item := range all[:len(all)-modelProbeEvidenceLimit] {
		dropped[item.e.RequestID] = true
		s.modelProbe.EvidenceIncompleteUntil = max(s.modelProbe.EvidenceIncompleteUntil, item.e.At.Add(10*time.Minute+time.Millisecond).UnixMilli())
	}
	if err := s.persistProbeStateLocked(); err != nil {
		s.modelProbe.storageError = true
		return err
	}
	for _, r := range s.modelProbe.runs {
		kept := r.proofs[:0]
		for _, e := range r.proofs {
			if !dropped[e.RequestID] {
				kept = append(kept, e)
			}
		}
		r.proofs = kept
	}
	return nil
}
