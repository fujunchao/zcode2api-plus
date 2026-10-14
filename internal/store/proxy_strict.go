package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxysub"
)

const strictProxyStateKey = "strict_proxy_v3"
const StrictProxyRuleVersion = "proxy-3012-v3.1"
const strictProxyWindow = 10 * time.Minute
const strictProxyEvidenceLimit = 4096

var ErrProxyRetired = errors.New("选定代理已淘汰或路由快照已过期")
var ErrStrictProxyPaused = errors.New("严格代理恢复尚未完成，暂停代理准入")

// ProxyObservation 仅接收真实模型 HTTP 尝试的去敏归因；Complete 表示完整成功。
type ProxyObservation struct {
	RequestID, TargetOrigin, Model, ContentRef, ProfileRef, SessionRef, DeviceRef, BusinessCode string
	Status                                                                                      int
	Complete                                                                                    bool
	At                                                                                          time.Time
	// EntryRef 关联同一网关入口的多号重试；未提供时仅用于兼容旧调用。
	EntryRef string
	// InvalidRequest 是构造/目标/鉴权检查已明确失败的排除信号。
	InvalidRequest bool
}

// ProxyDecision 描述严格规则的已提交删除结果，而不是供应商因果确认。
type ProxyDecision struct {
	Removed     []string
	Reason      string
	DecisionID  string
	RuleVersion string
	Assigned    int
	Direct      int
}

type strictProxyBlock struct {
	Identity, DecisionID, State, Reason, TargetOrigin, SubscriptionKey string
	CreatedAt                                                          int64
	Evidence                                                           []string
}

type strictProxyState struct {
	Version int
	Blocks  map[string]strictProxyBlock
	// 意图不迁移旧空绑定；只有明确自动操作产生 pending。
	Intents     map[string]string
	Pending     map[string]string
	Generations map[string]uint64
	Audit       []string
	Paused      bool
}

type strictProxyEvidence struct {
	ProxyObservation
	Identity, Credential, Provider string
}

// StrictProxyStatus 只提供维护所需的去敏摘要，不输出账号或输入引用。
type StrictProxyStatus struct {
	RuleVersion  string                    `json:"rule_version"`
	Blocks       []StrictProxyBlockSummary `json:"blocks"`
	Pending      int                       `json:"pending"`
	Retiring     int                       `json:"retiring"`
	ImportPaused bool                      `json:"import_paused"`
}

type StrictProxyBlockSummary struct {
	Identity      string `json:"identity"`
	DecisionID    string `json:"decision_id"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	TargetOrigin  string `json:"target_origin"`
	CreatedAt     int64  `json:"created_at"`
	EvidenceCount int    `json:"evidence_count"`
}

func newStrictProxyState() strictProxyState {
	return strictProxyState{Version: 1, Blocks: map[string]strictProxyBlock{}, Intents: map[string]string{}, Pending: map[string]string{}, Generations: map[string]uint64{}}
}

func (s *Store) cloneStrictProxyLocked() strictProxyState {
	raw, _ := json.Marshal(s.strictProxy)
	var next strictProxyState
	_ = json.Unmarshal(raw, &next)
	return next
}

func strictDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func strictCredential(a *model.Account) string {
	return strictDigest(a.Provider, a.ID, a.Mode, a.Secret())
}

// 地址身份保留认证分区，合并大小写、缺省端口及纯尾斜杠等路由别名。
// 无认证 IP 端点直接复用订阅 Fingerprint，禁止状态在两个入口一致。
func strictProxyIdentity(raw string) string {
	if raw == "" {
		return ""
	}
	if canonical, err := proxysub.Canonical(raw); err == nil {
		return proxysub.Fingerprint(canonical)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strictDigest(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		case "socks4", "socks5", "socks5h":
			port = "1080"
		}
	}
	u.Host = net.JoinHostPort(host, port)
	if u.Path == "/" {
		u.Path = ""
	}
	u.Fragment = ""
	return strictDigest(u.String())
}

func (s *Store) loadStrictProxy(raw string) error {
	s.strictProxy = newStrictProxyState()
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.strictProxy); err != nil {
			return fmt.Errorf("读取严格代理状态失败: %w", err)
		}
		if s.strictProxy.Version != 1 || s.strictProxy.Blocks == nil || s.strictProxy.Intents == nil || s.strictProxy.Pending == nil || s.strictProxy.Generations == nil {
			return errors.New("严格代理状态版本或结构无效")
		}
	}
	journal, err := os.ReadFile(s.strictJournalPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取严格删除意图失败: %w", err)
	}
	if len(journal) > 0 {
		var blocks map[string]strictProxyBlock
		if err := json.Unmarshal(journal, &blocks); err != nil {
			return fmt.Errorf("严格删除意图损坏，拒绝恢复代理调度: %w", err)
		}
		for key, b := range blocks {
			if key == "" || b.Identity != key {
				return errors.New("严格删除意图身份无效")
			}
			if _, ok := s.strictProxy.Blocks[key]; !ok {
				s.strictProxy.Blocks[key] = b
			}
		}
	}
	return nil
}

// 先 fsync 独立意图再改 SQL；即使数据库完全不可写，重启也不会重新准入旧线路。
func (s *Store) writeStrictJournalLocked(blocks map[string]strictProxyBlock) error {
	raw, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.strictJournalPath), ".strict-proxy-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.strictJournalPath)
}

func (s *Store) strictProxyBlockedLocked(raw string) bool {
	_, blocked := s.strictProxy.Blocks[strictProxyIdentity(raw)]
	return blocked || s.strictProxy.Paused
}

func (s *Store) strictSuspectLocked(identity string, now time.Time) bool {
	for _, e := range s.strictEvidence {
		if e.Identity == identity && e.Status == 405 && e.BusinessCode == "3012" && !e.At.Before(now.Add(-strictProxyWindow)) {
			return true
		}
	}
	return false
}

// chooseProxyLocked 是全部自动选线的唯一准入：待核查不新增绑定，禁止记录不按用途放行。
func (s *Store) chooseProxyLocked(profiles []ProxyProfile, occupancy map[string]int, excludeID, excludeURL string) (ProxyProfile, bool) {
	allowed := make([]ProxyProfile, 0, len(profiles))
	now := time.Now()
	for _, p := range profiles {
		if !p.Enabled || p.ID == excludeID || (excludeURL != "" && strictProxyIdentity(p.URL) == strictProxyIdentity(excludeURL)) || s.strictProxyBlockedLocked(p.URL) || s.strictSuspectLocked(strictProxyIdentity(p.URL), now) {
			continue
		}
		allowed = append(allowed, p)
	}
	return leastLoadedProxy(allowed, occupancy, "", "")
}

func comparable(a, b strictProxyEvidence) bool {
	return a.Provider == b.Provider && a.TargetOrigin == b.TargetOrigin && a.Model == b.Model && a.ProfileRef == b.ProfileRef
}

func strictFailure(e strictProxyEvidence) bool { return e.Status == 405 && e.BusinessCode == "3012" }
func strictSuccess(e strictProxyEvidence) bool {
	return e.Complete && e.Status >= 200 && e.Status < 300
}

func strictDecisionFor(identity string, evidence []strictProxyEvidence) (string, []string, string) {
	// 每个目标/模型/协议档案独立评价，禁止拼接不同用途的失败数。
	for _, seed := range evidence {
		if seed.Identity != identity || !strictFailure(seed) {
			continue
		}
		var failures, successes []strictProxyEvidence
		contradiction := false
		for _, e := range evidence {
			if !comparable(seed, e) {
				continue
			}
			if e.Identity == identity {
				if strictSuccess(e) {
					contradiction = true
				}
				if strictFailure(e) {
					failures = append(failures, e)
				}
			} else if strictSuccess(e) {
				successes = append(successes, e)
			}
		}
		// 混合成功也不自动按路径 A 删除，保守保留给人工复核。
		if contradiction {
			continue
		}
		for _, good := range successes {
			refs := []string{good.RequestID}
			entries := map[string]bool{}
			for _, bad := range failures {
				if bad.Credential == good.Credential && bad.DeviceRef != "" && bad.DeviceRef == good.DeviceRef && bad.SessionRef != "" && bad.SessionRef == good.SessionRef && bad.ContentRef == good.ContentRef {
					entry := bad.EntryRef
					if entry == "" {
						entry = bad.RequestID
					}
					if !entries[entry] {
						entries[entry] = true
						refs = append(refs, bad.RequestID)
					}
				}
			}
			if len(entries) >= 2 {
				return "comparable_route_control", refs, seed.TargetOrigin
			}
		}
		credentials, entries, contents := map[string]bool{}, map[string]bool{}, map[string]bool{}
		refs := []string{}
		for _, bad := range failures {
			credentials[bad.Credential] = true
			contents[bad.ContentRef] = true
			entry := bad.EntryRef
			if entry == "" {
				entry = bad.RequestID
			}
			entries[entry] = true
			refs = append(refs, bad.RequestID)
		}
		controlled := map[string]bool{}
		contentControl := false
		for _, good := range successes {
			if credentials[good.Credential] {
				controlled[good.Credential] = true
				refs = append(refs, good.RequestID)
				if contents[good.ContentRef] {
					contentControl = true
				}
			}
		}
		if len(credentials) >= 3 && len(entries) >= 3 && len(contents) >= 2 && len(controlled) >= 2 && contentControl {
			return "shared_route_control", refs, seed.TargetOrigin
		}
	}
	return "", nil, ""
}

func (s *Store) ObserveProxyOutcome(acc *model.Account, egress ProxyEgress, o ProxyObservation) (ProxyDecision, error) {
	result := ProxyDecision{Reason: "ignored", RuleVersion: StrictProxyRuleVersion}
	if acc == nil || egress.Mode != "proxy" || o.InvalidRequest || o.RequestID == "" || o.TargetOrigin == "" || o.Model == "" || o.ContentRef == "" || o.ProfileRef == "" {
		return result, nil
	}
	u, err := url.Parse(o.TargetOrigin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return result, nil
	}
	o.TargetOrigin = strings.ToLower(u.Scheme + "://" + u.Host)
	if !((o.Status == 405 && o.BusinessCode == "3012") || (o.Complete && o.Status >= 200 && o.Status < 300)) {
		return result, nil
	}
	if o.At.IsZero() {
		o.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	live := s.findLocked(acc.Provider, acc.ID)
	if !live.SameCredential(acc) || egress.identity == "" || egress.identity != strictProxyIdentity(derefStr(acc.ProxyURL)) || !s.egressVersionCurrentLocked(acc, egress) {
		result.Reason = "stale_snapshot"
		return result, nil
	}
	if _, blocked := s.strictProxy.Blocks[egress.identity]; blocked {
		result.Reason = "already_retired"
		return result, nil
	}
	filtered := make([]strictProxyEvidence, 0, len(s.strictEvidence)+1)
	for _, e := range s.strictEvidence {
		if e.At.Before(o.At.Add(-strictProxyWindow)) || e.At.After(o.At.Add(strictProxyWindow)) {
			continue
		}
		if e.RequestID == o.RequestID {
			result.Reason = "duplicate"
			return result, nil
		}
		filtered = append(filtered, e)
	}
	filtered = append(filtered, strictProxyEvidence{ProxyObservation: o, Identity: egress.identity, Credential: strictCredential(acc), Provider: acc.Provider})
	if len(filtered) > strictProxyEvidenceLimit {
		filtered = filtered[len(filtered)-strictProxyEvidenceLimit:]
	}
	s.strictEvidence = filtered
	result.Reason = "suspect"
	if o.Complete {
		result.Reason = "observed_success"
	}
	identities := map[string]bool{}
	for _, e := range filtered {
		if strictFailure(e) {
			identities[e.Identity] = true
		}
	}
	for identity := range identities {
		if _, blocked := s.strictProxy.Blocks[identity]; blocked {
			continue
		}
		reason, refs, target := strictDecisionFor(identity, filtered)
		if reason == "" {
			continue
		}
		b := strictProxyBlock{Identity: identity, DecisionID: "decision-" + randomHex(12), State: "retiring", Reason: reason, TargetOrigin: target, CreatedAt: o.At.UnixMilli(), Evidence: refs}
		for _, p := range s.listProxyProfilesLocked() {
			if strictProxyIdentity(p.URL) == identity {
				if _, err := proxysub.Canonical(p.URL); err == nil {
					b.SubscriptionKey = proxysub.Fingerprint(p.URL)
				}
				break
			}
		}
		s.strictProxy.Blocks[identity] = b // 与新许可同锁线性化，即使持久化失败也不准入。
		if len(s.strictProxy.Blocks) >= proxysub.MaxBlocks {
			s.strictProxy.Paused = true
		}
		if err := s.writeStrictJournalLocked(s.strictProxy.Blocks); err != nil {
			s.strictProxy.Paused = true
			result.Reason = "persist_failed"
			return result, fmt.Errorf("持久化严格删除意图失败: %w", err)
		}
		d, err := s.retireStrictIdentityLocked(identity)
		if err != nil {
			result.Reason = "persist_failed"
			return result, err
		}
		result.Removed = append(result.Removed, d.Removed...)
		result.Reason = d.Reason
		result.DecisionID = d.DecisionID
		result.Assigned += d.Assigned
		result.Direct += d.Direct
	}
	return result, nil
}

func (s *Store) egressVersionCurrentLocked(acc *model.Account, e ProxyEgress) bool {
	if e.ID != "" {
		for _, p := range s.listProxyProfilesLocked() {
			if p.ID == e.ID {
				return p.Enabled && strictProxyIdentity(p.URL) == e.identity && s.strictProxy.Generations[p.ID] == e.generation
			}
		}
		return false
	}
	return e.identity == strictProxyIdentity(derefStr(acc.ProxyURL))
}

// AcquireProxyDispatch 与严格删除共享线性化点；已获许可流不被删除动作取消。
func (s *Store) AcquireProxyDispatch(acc *model.Account, egress ProxyEgress) (func(), error) {
	if egress.Mode != "proxy" {
		return func() {}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc == nil || egress.identity == "" || s.strictProxyBlockedLocked(derefStr(acc.ProxyURL)) || !s.egressVersionCurrentLocked(acc, egress) {
		return nil, ErrProxyRetired
	}
	live := s.findLocked(acc.Provider, acc.ID)
	if live != nil && (!live.SameCredential(acc) || derefStr(live.ProxyID) != derefStr(acc.ProxyID) || strictProxyIdentity(derefStr(live.ProxyURL)) != egress.identity) {
		return nil, ErrProxyRetired
	}
	if s.strictDispatch == nil {
		s.strictDispatch = map[string]int{}
	}
	s.strictDispatch[egress.identity]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.strictDispatch[egress.identity]--
			if s.strictDispatch[egress.identity] == 0 {
				delete(s.strictDispatch, egress.identity)
			}
		})
	}, nil
}

func (s *Store) StrictProxySummary() (StrictProxyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := StrictProxyStatus{RuleVersion: StrictProxyRuleVersion, Blocks: []StrictProxyBlockSummary{}, Pending: len(s.strictProxy.Pending), ImportPaused: s.strictProxy.Paused}
	for _, b := range s.strictProxy.Blocks {
		out.Blocks = append(out.Blocks, StrictProxyBlockSummary{Identity: b.Identity, DecisionID: b.DecisionID, State: b.State, Reason: b.Reason, TargetOrigin: b.TargetOrigin, CreatedAt: b.CreatedAt, EvidenceCount: len(b.Evidence)})
		if b.State == "retiring" {
			out.Retiring++
		}
	}
	sort.Slice(out.Blocks, func(i, j int) bool { return out.Blocks[i].Identity < out.Blocks[j].Identity })
	return out, nil
}

func (s *Store) UnblockStrictProxy(identity string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.strictProxy.Blocks[identity]
	if !ok {
		return ErrNotFound
	}
	if b.State != "removed" {
		return errors.New("严格删除事务未完成，请先恢复删除")
	}
	next := s.cloneStrictProxyLocked()
	delete(next.Blocks, identity)
	next.Audit = append(next.Audit, "manual_unblock:"+b.DecisionID)
	if len(next.Audit) > 256 {
		next.Audit = next.Audit[len(next.Audit)-256:]
	}
	if len(next.Blocks) < proxysub.MaxBlocks {
		next.Paused = false
	}
	sub, err := s.subscriptionLocked()
	if err != nil {
		return err
	}
	if b.SubscriptionKey != "" {
		delete(sub.Blocks, b.SubscriptionKey)
	}
	// 先写新意图日志，SQL 失败时恢复旧日志，绝不把未提交解除发布到内存。
	if err := s.writeStrictJournalLocked(next.Blocks); err != nil {
		return err
	}
	if err := s.commitProxyBundleStrictLocked(context.Background(), nil, nil, nil, &sub, &next); err != nil {
		_ = s.writeStrictJournalLocked(s.strictProxy.Blocks)
		return err
	}
	kept := s.strictEvidence[:0]
	for _, e := range s.strictEvidence {
		if e.Identity != identity {
			kept = append(kept, e)
		}
	}
	s.strictEvidence = kept
	return nil
}

func (s *Store) RecoverStrictProxyRetirements() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for identity, b := range s.strictProxy.Blocks {
		if b.State == "retiring" {
			if _, err := s.retireStrictIdentityLocked(identity); err != nil {
				return err
			}
		}
	}
	return nil
}
