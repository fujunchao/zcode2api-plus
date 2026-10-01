package store

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
)

const RiskProxyHistoryTTL = 24 * time.Hour
const riskProxyHistoryKey = "risk_proxy_history"
const riskProxyHistoryLimit = 64

type riskProxyFailure struct {
	ProxyID  string `json:"proxy_id"`
	URLHash  string `json:"url_hash"`
	FailedAt int64  `json:"failed_at"`
}

type riskProxyHistory map[string][]riskProxyFailure

// ProxyRotation 只报告已提交的改派；Reason 同时用于未改派日志，不把跳过误报为成功。
type ProxyRotation struct {
	Next           ProxyProfile
	Reason         string
	HistoryBlocked int
}

func proxyURLHash(raw string) string {
	if raw == "" {
		return ""
	}
	if normalized, err := proxy.NormalizeProxyURL(raw); err == nil && normalized != nil {
		raw = *normalized
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// recentProxyHistoryLocked 建立独立副本，顺便清理过期及已删除账号；不得原地修改旧快照。
func (s *Store) recentProxyHistoryLocked(now time.Time) riskProxyHistory {
	result := riskProxyHistory{}
	for _, acc := range s.allAccountsLocked() {
		for _, failure := range s.riskProxyHistory[acc.ID] {
			if failure.FailedAt > now.Add(-RiskProxyHistoryTTL).Unix() {
				result[acc.ID] = append(result[acc.ID], failure)
			}
		}
	}
	return result
}

// RotateAccountProxyAt 按本次风控时刻记录失败出口，再选择该账号近期未失败的线路。
// 保留显式时钟以便和网关冷却共用同一时间，并可确定性验证 TTL，不需要真实等待。
func (s *Store) RotateAccountProxyAt(expected *model.Account, now time.Time) (ProxyRotation, error) {
	result := ProxyRotation{Reason: "stale_snapshot"}
	if expected == nil {
		return result, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(expected.Provider, expected.ID)
	if live == nil || derefStr(live.ProxyID) != derefStr(expected.ProxyID) ||
		derefStr(live.ProxyURL) != derefStr(expected.ProxyURL) {
		return result, nil
	}
	history := s.recentProxyHistoryLocked(now)
	oldID, oldHash := derefStr(live.ProxyID), proxyURLHash(derefStr(live.ProxyURL))
	if oldID != "" || oldHash != "" {
		entries := []riskProxyFailure{}
		for _, entry := range history[live.ID] {
			if entry.ProxyID != oldID || entry.URLHash != oldHash {
				entries = append(entries, entry)
			}
		}
		entries = append(entries, riskProxyFailure{ProxyID: oldID, URLHash: oldHash, FailedAt: now.Unix()})
		if len(entries) > riskProxyHistoryLimit {
			entries = entries[len(entries)-riskProxyHistoryLimit:]
		}
		history[live.ID] = entries
	}
	var candidates []ProxyProfile
	for _, p := range s.listProxyProfilesLocked() {
		if !p.Enabled || p.ID == oldID || proxyURLHash(p.URL) == oldHash {
			continue
		}
		blocked := false
		hash := proxyURLHash(p.URL)
		for _, failure := range history[live.ID] {
			if failure.ProxyID == p.ID || (failure.URLHash != "" && failure.URLHash == hash) {
				blocked = true
				break
			}
		}
		if blocked {
			result.HistoryBlocked++
			continue
		}
		candidates = append(candidates, p)
	}
	p, ok := leastLoadedProxy(candidates, s.proxyOccupancyLocked(), "", "")
	result.Reason = "no_alternative"
	var pending []*model.Account
	if ok {
		copy := live.Clone()
		copy.ProxyID, copy.ProxyURL = &p.ID, &p.URL
		pending = append(pending, copy)
	} else if result.HistoryBlocked > 0 {
		result.Reason = "recent_routes_exhausted"
	}
	// 即使没有候选，也必须记录当前失败出口，避免新增候选后丢失这次证据。
	if err := s.commitProxyStateWithHistoryLocked(nil, pending, history); err != nil {
		result.Reason = "persist_failed"
		return result, err
	}
	if ok {
		result.Next, result.Reason = p, "changed"
	}
	return result, nil
}
