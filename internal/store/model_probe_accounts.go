package store

import (
	"math"
	"sort"
	"sync"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

// 业务与探测各保留最近20个结果；探测坏线路的失败不能污染业务账号的可靠性。
type probeAccountOutcome struct {
	At        int64  `json:"at"`
	RequestID string `json:"request,omitempty"`
	Source    string `json:"source,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Success   bool   `json:"ok,omitempty"`
	Risk      bool   `json:"risk,omitempty"`
}

type probeAccountWindow struct {
	Entries    []probeAccountOutcome `json:"entries,omitempty"`
	Credential string                `json:"credential,omitempty"`
	LastPickAt int64                 `json:"last_pick,omitempty"`
	LastAt     int64                 `json:"last_at,omitempty"`
}

type probeAccountScore struct {
	Tier, Success, Total, ProbeSuccess, ProbeTotal, Canceled, Neutral int
	Rate                                                              float64
	LastSuccess                                                       int64
}

func accountOutcomeNeutral(kind string) bool {
	switch kind {
	case "connection_failed", "timeout", "incomplete_response", "upstream_error", "invalid_response", "invalid_target", "invalid_request", "redirect_or_invalid_status":
		return true
	}
	return false
}

func (w *probeAccountWindow) score(now time.Time) probeAccountScore {
	out := probeAccountScore{Tier: 2}
	if w == nil {
		return out
	}
	cutoff := now.Add(-probeAccountWindowTTL).UnixMilli()
	var business, probes []probeAccountOutcome
	for _, e := range w.Entries {
		if e.At < cutoff || e.At > now.UnixMilli() {
			continue
		}
		if e.Kind == "canceled" {
			out.Canceled++
			continue
		}
		if e.Source == "probe" {
			out.ProbeTotal++
			if e.Success {
				out.ProbeSuccess++
			}
			// 对未知线路的风控、传输及平台错误不推定为账号问题。
			if e.Success || (!e.Risk && !accountOutcomeNeutral(e.Kind)) {
				probes = append(probes, e)
			}
		} else {
			out.Total++
			if e.Success {
				out.Success++
			}
			if accountOutcomeNeutral(e.Kind) {
				out.Neutral++
			} else {
				business = append(business, e)
			}
		}
		if e.Success {
			out.LastSuccess = max(out.LastSuccess, e.At)
		}
	}
	entries := business
	if len(entries) == 0 {
		entries = probes
	}
	if len(entries) == 0 {
		return out
	}
	// 六小时半衰期与 Beta(1,1) 平滑：1/1 不再无条件压过19/20。
	good, total := 0.0, 0.0
	var last probeAccountOutcome
	for _, e := range entries {
		weight := math.Exp2(-float64(now.UnixMilli()-e.At) / float64((6 * time.Hour).Milliseconds()))
		total += weight
		if e.Success {
			good += weight
		}
		if e.At >= last.At {
			last = e
		}
	}
	out.Rate = (good + 1) / (total + 2)
	out.Tier = 0
	if good == 0 {
		out.Tier = 3
	} else if last.Risk {
		out.Tier = 1
	}
	return out
}

func (s *Store) probeAccountWindowLocked(id string) *probeAccountWindow {
	if s.modelProbe.AccountWindows == nil {
		s.modelProbe.AccountWindows = map[string]*probeAccountWindow{}
	}
	w := s.modelProbe.AccountWindows[id]
	a := s.findAnyLocked(id)
	if a == nil {
		return nil
	}
	credential := s.strictCredentialRefLocked(a)
	if w == nil || w.Credential != credential {
		w = &probeAccountWindow{Credential: credential}
		s.modelProbe.AccountWindows[id] = w
	}
	return w
}

func (s *Store) recordProbeAccountOutcomeLocked(id string, o ProxyObservation, source, kind string) {
	w := s.probeAccountWindowLocked(id)
	if w == nil {
		return
	}
	requestID := strictDigest(source, o.RequestID)
	for _, e := range w.Entries {
		if e.RequestID == requestID {
			return
		}
	}
	w.Entries = append(w.Entries, probeAccountOutcome{At: o.At.UnixMilli(), RequestID: requestID, Source: source, Kind: kind, Success: probeSuccess(o), Risk: probeRisk(o)})
	count := 0
	for _, e := range w.Entries {
		if e.Source == source {
			count++
		}
	}
	if count > probeAccountWindowEntries {
		for i, e := range w.Entries {
			if e.Source == source {
				w.Entries = append(w.Entries[:i], w.Entries[i+1:]...)
				break
			}
		}
	}
	w.LastAt = max(w.LastAt, o.At.UnixMilli())
	s.modelProbe.accountsDirty = true
	s.pruneProbeAccountWindowsLocked(o.At)
}

// ObserveModelAccountOutcome 只维护选号统计，不向严格退役证据加入新种类错误。
// 调用者提供真实模型尝试的终态；不读取响应正文、日志或运行账号导出文件。
func (s *Store) ObserveModelAccountOutcome(acc *model.Account, o ProxyObservation, kind string) {
	if acc == nil || acc.Mode != "jwt" || o.InvalidRequest || o.RequestID == "" || !probeTargetMatches(config.UpstreamZai, o) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findAnyLocked(acc.ID)
	if s.strictClosed || live == nil || !live.SameCredential(acc) || live.CredentialGeneration != acc.CredentialGeneration {
		return
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	s.recordProbeAccountOutcomeLocked(acc.ID, o, "business", kind)
}

// TrackModelAccountUse 仅观测业务在途数供巡检避让，不改变业务选号和持久路由。
func (s *Store) TrackModelAccountUse(id string) func() {
	s.mu.Lock()
	if s.modelProbe.businessActive == nil {
		s.modelProbe.businessActive = map[string]int{}
	}
	s.modelProbe.businessActive[id]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.modelProbe.businessActive[id]--
			if s.modelProbe.businessActive[id] <= 0 {
				delete(s.modelProbe.businessActive, id)
			}
		})
	}
}

func (s *Store) touchProbeAccountPickLocked(id string, now time.Time) {
	if w := s.probeAccountWindowLocked(id); w != nil {
		w.LastPickAt = now.UnixMilli()
		s.modelProbe.accountsDirty = true
	}
}

func (s *Store) pruneProbeAccountWindowsLocked(now time.Time) {
	cutoff := now.Add(-probeAccountWindowTTL).UnixMilli()
	for id, w := range s.modelProbe.AccountWindows {
		if w == nil || max(w.LastAt, w.LastPickAt) < cutoff || s.findAnyLocked(id) == nil {
			delete(s.modelProbe.AccountWindows, id)
		}
	}
	if len(s.modelProbe.AccountWindows) <= probeAccountWindowLimit {
		return
	}
	ids := make([]string, 0, len(s.modelProbe.AccountWindows))
	for id := range s.modelProbe.AccountWindows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.modelProbe.AccountWindows[ids[i]], s.modelProbe.AccountWindows[ids[j]]
		if max(a.LastAt, a.LastPickAt) != max(b.LastAt, b.LastPickAt) {
			return max(a.LastAt, a.LastPickAt) < max(b.LastAt, b.LastPickAt)
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids[:len(ids)-probeAccountWindowLimit] {
		delete(s.modelProbe.AccountWindows, id)
	}
}

type probeBudget struct {
	Total, Reserved int
	Accounts        map[string]int
	Reservations    map[string]int
	NextAt          int64
	AccountNext     map[string]int64
}

func (s *Store) probeBudgetLocked(now time.Time) (probeBudget, error) {
	b := probeBudget{Accounts: map[string]int{}, Reservations: map[string]int{}, AccountNext: map[string]int64{}}
	rows, err := s.db.Query("SELECT account_id,COUNT(*),MIN(started_at) FROM model_probe_attempts WHERE started_at>? GROUP BY account_id", now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		var first int64
		if err := rows.Scan(&id, &count, &first); err != nil {
			return b, err
		}
		b.Accounts[id] = count
		b.Total += count
		b.AccountNext[id] = first + (24 * time.Hour).Milliseconds() + 1
		if b.NextAt == 0 || b.AccountNext[id] < b.NextAt {
			b.NextAt = b.AccountNext[id]
		}
	}
	for _, l := range s.modelProbe.leases {
		if !l.started {
			b.Reserved++
			b.Reservations[l.account.ID]++
		}
	}
	return b, rows.Err()
}

type probeAccountRow struct {
	view  ModelProbeAccountView
	score probeAccountScore
}

// 管理端解释与实际派发共用这一份资格和排序，避免显示第一名却派发另一个口径。
func (s *Store) probeAccountRowsLocked(p ModelProbeSettings, now time.Time, b probeBudget) []probeAccountRow {
	rows := []probeAccountRow{}
	ids := p.AccountIDs
	if p.AccountMode == "auto" {
		ids = nil
		for _, a := range s.allAccountsLocked() {
			ids = append(ids, a.ID)
		}
	}
	for _, id := range ids {
		a := s.findAnyLocked(id)
		if a == nil {
			continue
		}
		w := s.modelProbe.AccountWindows[id]
		if w != nil && w.Credential != s.strictCredentialRefLocked(a) {
			w = nil
		}
		score := w.score(now)
		v := ModelProbeAccountView{ID: id, Name: a.Name, RecentSuccess: score.Success, RecentTotal: score.Total, ProbeSuccess: score.ProbeSuccess, ProbeTotal: score.ProbeTotal, NeutralFailures: score.Neutral, Canceled: score.Canceled, LastSuccessAt: score.LastSuccess, Score: score.Rate, ActiveRequests: s.modelProbe.businessActive[id], RemainingRequests: max(0, p.AccountDailyLimit-b.Accounts[id]-b.Reservations[id])}
		if w != nil {
			v.LastPickAt = w.LastPickAt
		}
		v.Reason, v.Eligible = s.probeAccountEligibilityLocked(id, now)
		if v.Eligible {
			switch {
			case v.RemainingRequests == 0:
				v.Reason = "account_budget_exhausted"
			case v.ActiveRequests > 0:
				v.Reason = "business_busy"
			default:
				for _, l := range s.modelProbe.leases {
					if l.account.ID == id {
						v.Reason = "probe_busy"
					}
				}
			}
			v.Eligible = v.Reason == ""
		}
		rows = append(rows, probeAccountRow{view: v, score: score})
	}
	if p.AccountMode == "auto" {
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.view.Eligible != b.view.Eligible {
				return a.view.Eligible
			}
			if a.score.Tier != b.score.Tier {
				return a.score.Tier < b.score.Tier
			}
			if a.score.Rate != b.score.Rate {
				return a.score.Rate > b.score.Rate
			}
			if a.view.LastPickAt != b.view.LastPickAt {
				return a.view.LastPickAt < b.view.LastPickAt
			}
			if a.score.LastSuccess != b.score.LastSuccess {
				return a.score.LastSuccess > b.score.LastSuccess
			}
			return a.view.ID < b.view.ID
		})
	}
	rank := 0
	for i := range rows {
		if rows[i].view.Eligible {
			rank++
			rows[i].view.Rank = rank
		}
	}
	return rows
}
