package store

import (
	"context"
	"strconv"
	"time"

	"zcode2api/internal/model"
)

// 代次与凭据/路由一同提交；即使 A→B→A 回到相同字节，旧 HTTP 快照也已失效。
// 账号 JSON 的兼容键集不变，内部版本只存入独立 meta。
func trackStrictAccountVersions(next *strictProxyState, a *model.Account) bool {
	changed := false
	credential := strictCredential(a)
	current := next.Credentials[a.ID]
	if current.Digest != credential {
		current.Digest = credential
		current.Generation++
		next.Credentials[a.ID] = current
		changed = true
		if _, waiting := next.Pending[a.ID]; waiting {
			next.Pending[a.ID] = credential
		}
	}
	route := strictDigest(derefStr(a.ProxyID), strictProxyIdentity(derefStr(a.ProxyURL)))
	previous := next.RouteVersions[a.ID]
	if previous.Digest != route {
		previous.Digest = route
		previous.Generation++
		next.RouteVersions[a.ID] = previous
		changed = true
	}
	return changed
}

func clearStrictAccountState(next *strictProxyState, id string) {
	delete(next.Intents, id)
	delete(next.Pending, id)
	delete(next.PendingAvoid, id)
	// 保留递增的撤销代次，但擦除已删除凭据的摘要；重复 ID 也不能恢复旧许可。
	credential := next.Credentials[id]
	credential.Digest = ""
	credential.Generation++
	next.Credentials[id] = credential
	route := next.RouteVersions[id]
	route.Digest = ""
	route.Generation++
	next.RouteVersions[id] = route
}

func (s *Store) migrateStrictProxyVersions() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cloneStrictProxyLocked()
	changed := false
	liveIDs := map[string]bool{}
	for _, a := range s.allAccountsLocked() {
		liveIDs[a.ID] = true
		if trackStrictAccountVersions(&next, a) {
			changed = true
		}
	}
	for id := range next.Pending {
		if !liveIDs[id] {
			clearStrictAccountState(&next, id)
			changed = true
		}
	}
	for _, p := range s.listProxyProfilesLocked() {
		if next.Generations[p.ID] == 0 {
			next.Generations[p.ID] = 1
			changed = true
		}
	}
	if !changed {
		s.publishCredentialGenerationsLocked()
		return nil
	}
	return s.commitProxyBundleStrictLocked(context.Background(), nil, nil, nil, nil, &next)
}

// 在发布 Store 状态的同一把锁内更新 live；外部请求只能从 Clone 取得冻结的代次。
func (s *Store) publishCredentialGenerationsLocked() {
	for _, a := range s.allAccountsLocked() {
		a.CredentialGeneration = s.strictProxy.Credentials[a.ID].Generation
	}
}

func (s *Store) persistAccountVersionLocked(a *model.Account, next strictProxyState) error {
	raw, err := marshalJSON(next)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := persistAccountOnTx(tx, a); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", strictProxyStateKey, string(raw)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.strictProxy = next
	a.CredentialGeneration = next.Credentials[a.ID].Generation
	return nil
}

func (s *Store) strictCredentialRefLocked(a *model.Account) string {
	return strictDigest(strictCredential(a), strconv.FormatUint(s.strictProxy.Credentials[a.ID].Generation, 10))
}

func (s *Store) strictCredentialCurrentLocked(a *model.Account, e ProxyEgress) bool {
	if a == nil || e.credentialRef == "" {
		return false
	}
	live := s.findLocked(a.Provider, a.ID)
	return live.SameCredential(a) && e.credentialRef == s.strictCredentialRefLocked(live)
}

func (s *Store) pruneStrictEvidenceLocked(now time.Time) {
	credentials := make(map[string]string)
	for _, a := range s.allAccountsLocked() {
		credentials[a.ID] = s.strictCredentialRefLocked(a)
	}
	profiles := make(map[string]ProxyProfile)
	identities := make(map[string]string)
	for _, p := range s.listProxyProfilesLocked() {
		profiles[p.ID] = p
		identities[p.ID] = strictProxyIdentity(p.URL)
	}
	kept := s.strictEvidence[:0]
	for _, e := range s.strictEvidence {
		if e.At.Before(now.Add(-strictProxyWindow)) || e.At.After(now.Add(time.Minute)) || credentials[e.AccountID] != e.Credential {
			continue
		}
		if e.ProxyID != "" {
			p, ok := profiles[e.ProxyID]
			if !ok || p.Generation != e.ProxyGeneration || identities[p.ID] != e.Identity {
				continue
			}
		}
		kept = append(kept, e)
	}
	s.strictEvidence = kept
}
