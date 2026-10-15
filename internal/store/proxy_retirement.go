package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"zcode2api/internal/proxy"
	"zcode2api/internal/proxysub"
)

// 所有模型归因决定共用唯一的严格退役事务，不以普通巡检删除替代持久禁止。
func (s *Store) startStrictRetirementLocked(identity, rawURL, reason, target, rule string, refs []string, at time.Time) (ProxyDecision, error) {
	result := ProxyDecision{RuleVersion: rule, Reason: "ignored"}
	if identity == "" {
		return result, ErrInvalidProxyRoute
	}
	if b, ok := s.strictProxy.Blocks[identity]; ok {
		return ProxyDecision{RuleVersion: b.RuleVersion, Reason: "already_retired", DecisionID: b.DecisionID}, nil
	}
	b := strictProxyBlock{Identity: identity, DecisionID: "decision-" + randomHex(12), State: "retiring", Reason: reason, TargetOrigin: target, CreatedAt: at.UnixMilli(), Evidence: refs, RuleVersion: rule}
	for _, p := range s.listProxyProfilesLocked() {
		if strictProxyIdentity(p.URL) == identity {
			rawURL = p.URL
			break
		}
	}
	if strictProxyIdentity(rawURL) == identity {
		if canonical, err := proxy.CanonicalRoute(rawURL); err == nil {
			if _, err := proxysub.Canonical(canonical); err == nil {
				b.SubscriptionKey = identity
			}
		}
	}
	s.strictProxy.Blocks[identity] = b
	if len(s.strictProxy.Blocks) >= proxysub.MaxBlocks {
		s.strictProxy.ImportPaused = true
	}
	if err := s.writeStrictJournalLocked(s.strictProxy.Blocks); err != nil {
		s.strictProxy.Paused = true
		result.Reason = "persist_failed"
		next := s.cloneStrictProxyLocked()
		dbErr := s.commitProxyBundleStrictLocked(context.Background(), nil, nil, nil, nil, &next)
		s.scheduleStrictProxyRecoveryLocked()
		return result, fmt.Errorf("持久化严格删除日志失败，代理准入已暂停: %w", errors.Join(err, dbErr))
	}
	d, err := s.retireStrictIdentityLocked(identity)
	if err != nil {
		s.scheduleStrictProxyRecoveryLocked()
	}
	return d, err
}
