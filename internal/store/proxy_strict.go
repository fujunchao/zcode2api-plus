package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/proxysub"
)

const strictProxyStateKey = "strict_proxy_v3"
const StrictProxyRuleVersion = "proxy-3012-v3.2"
const StrictProxyEnabledKey = "strict_proxy_enabled"
const strictProxyWindow = 10 * time.Minute
const strictProxyEvidenceLimit = 4096

var ErrProxyRetired = errors.New("选定代理已淘汰或路由快照已过期")
var ErrStrictProxyPaused = errors.New("严格代理恢复尚未完成，暂停代理准入")
var ErrInvalidProxyRoute = errors.New("选定代理路由不可用，请选择自动分配或显式直连")
var ErrStrictDecisionConflict = errors.New("严格代理决定已更新，请重新查看并复核")

// ProxyObservation 仅接收真实模型 HTTP 尝试的去敏归因；Complete 表示完整成功。
type ProxyObservation struct {
	RequestID, TargetOrigin, Model, ContentRef, ProfileRef, SessionRef, DeviceRef, BusinessCode string
	Status                                                                                      int
	Complete                                                                                    bool
	At                                                                                          time.Time
	// EntryRef 关联同一网关入口的多号重试；缺失时不能作为独立输入证据。
	EntryRef string
	// 与 Origin 分开保存真实模型端点和方法，不把同域的登录/额度响应混作对照。
	TargetPath, Method string
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
	Pending     int
}

type strictProxyBlock struct {
	Identity, DecisionID, State, Reason, TargetOrigin, SubscriptionKey, RuleVersion string
	CreatedAt                                                                       int64
	Evidence                                                                        []string
}

type strictProxyState struct {
	Version int
	Blocks  map[string]strictProxyBlock
	// 意图不迁移旧空绑定；只有明确自动操作产生 pending。
	Intents       map[string]string
	Pending       map[string]string
	PendingAvoid  map[string][]string
	Generations   map[string]uint64
	Credentials   map[string]strictCredentialVersion
	RouteVersions map[string]strictRouteVersion
	Audit         []string
	Paused        bool
	// 容量保护只暂停导入，不能把仍然健康的存量线路也停用。
	ImportPaused bool
	// 有效样本因容量被裁剪后，不能再从残存样本断言“窗口内没有矛盾成功”。
	// 持久化该截止时间，进程重启也不能提前恢复删除判定；不影响既有退役记录。
	EvidenceIncompleteUntil int64
}

type strictCredentialVersion struct {
	Digest     string
	Generation uint64
}

type strictRouteVersion struct {
	Digest     string
	Generation uint64
}

type strictProxyEvidence struct {
	ProxyObservation
	Identity, Credential, Provider, AccountID, ProxyID string
	CredentialGeneration, ProxyGeneration              uint64
}

// StrictProxyStatus 只提供维护所需的去敏摘要，不输出账号或输入引用。
type StrictProxyStatus struct {
	RuleVersion             string                    `json:"rule_version"`
	Blocks                  []StrictProxyBlockSummary `json:"blocks"`
	Pending                 int                       `json:"pending"`
	Retiring                int                       `json:"retiring"`
	ImportPaused            bool                      `json:"import_paused"`
	DispatchPaused          bool                      `json:"dispatch_paused"`
	DecisionEnabled         bool                      `json:"decision_enabled"`
	ActiveDispatch          int                       `json:"active_dispatch"`
	Audit                   []string                  `json:"audit"`
	EvidenceLimited         bool                      `json:"evidence_limited"`
	EvidenceIncompleteUntil int64                     `json:"evidence_incomplete_until,omitempty"`
}

type StrictProxyBlockSummary struct {
	Identity      string `json:"identity"`
	DecisionID    string `json:"decision_id"`
	State         string `json:"state"`
	Reason        string `json:"reason"`
	TargetOrigin  string `json:"target_origin"`
	CreatedAt     int64  `json:"created_at"`
	EvidenceCount int    `json:"evidence_count"`
	RuleVersion   string `json:"rule_version"`
}

func newStrictProxyState() strictProxyState {
	return strictProxyState{Version: 1, Blocks: map[string]strictProxyBlock{}, Intents: map[string]string{}, Pending: map[string]string{}, PendingAvoid: map[string][]string{}, Generations: map[string]uint64{}, Credentials: map[string]strictCredentialVersion{}, RouteVersions: map[string]strictRouteVersion{}}
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
	canonical, err := proxy.CanonicalRoute(raw)
	if err != nil {
		return strictDigest(raw)
	}
	if subscriptionURL, err := proxysub.Canonical(canonical); err == nil {
		return proxysub.Fingerprint(subscriptionURL)
	}
	return strictDigest(canonical)
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
	if s.strictProxy.PendingAvoid == nil {
		s.strictProxy.PendingAvoid = map[string][]string{}
	}
	if s.strictProxy.Credentials == nil {
		s.strictProxy.Credentials = map[string]strictCredentialVersion{}
	}
	if s.strictProxy.RouteVersions == nil {
		s.strictProxy.RouteVersions = map[string]strictRouteVersion{}
	}
	journal, err := os.ReadFile(s.strictJournalPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取严格删除意图失败: %w", err)
	}
	if err == nil && len(journal) == 0 {
		return errors.New("严格删除意图为空，拒绝恢复代理调度")
	}
	if len(journal) > 0 {
		var blocks map[string]strictProxyBlock
		if err := json.Unmarshal(journal, &blocks); err != nil {
			return fmt.Errorf("严格删除意图损坏，拒绝恢复代理调度: %w", err)
		}
		if blocks == nil {
			return errors.New("严格删除意图结构无效，拒绝恢复代理调度")
		}
		for key, b := range blocks {
			if key == "" || b.Identity != key || (b.State != "retiring" && b.State != "removed") || b.DecisionID == "" || b.RuleVersion == "" {
				return errors.New("严格删除意图身份无效")
			}
			if _, ok := s.strictProxy.Blocks[key]; !ok {
				s.strictProxy.Blocks[key] = b
			}
		}
	}
	return nil
}

// 先 fsync 独立意图再改 SQL；数据库不可写时仍可由可写的恢复日志阻止回流。
// 两种持久介质同时不可写时只能在当前进程闭锁，并明确返回恢复失败。
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
	return replaceStrictJournal(name, s.strictJournalPath)
}

func (s *Store) strictProxyBlockedLocked(raw string) bool {
	_, blocked := s.strictProxy.Blocks[strictProxyIdentity(raw)]
	return blocked || s.strictProxy.Paused
}

func (s *Store) strictObservationNow() time.Time {
	if s.strictNow != nil {
		return s.strictNow()
	}
	return time.Now()
}

func (s *Store) strictSuspectLocked(identity string, now time.Time) bool {
	s.pruneStrictEvidenceLocked(now)
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
	now := s.strictObservationNow()
	s.pruneStrictEvidenceLocked(now)
	suspects := map[string]bool{}
	for identity := range s.modelProbeSuspectsLocked(now) {
		suspects[identity] = true
	}
	for _, e := range s.strictEvidence {
		if strictFailure(e) {
			suspects[e.Identity] = true
		}
	}
	for _, p := range profiles {
		if !p.Enabled || p.ID == excludeID || (excludeURL != "" && strictProxyIdentity(p.URL) == strictProxyIdentity(excludeURL)) || s.strictProxyBlockedLocked(p.URL) || suspects[strictProxyIdentity(p.URL)] {
			continue
		}
		allowed = append(allowed, p)
	}
	return leastLoadedProxy(allowed, occupancy, "", "")
}

func comparable(a, b strictProxyEvidence) bool {
	return a.Provider == b.Provider && a.TargetOrigin == b.TargetOrigin && a.TargetPath == b.TargetPath && a.Method == b.Method && a.Model == b.Model && a.ProfileRef == b.ProfileRef
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
		latest := seed.At
		lastFailure := map[string]time.Time{}
		for _, e := range evidence {
			if !comparable(seed, e) {
				continue
			}
			if e.At.After(latest) {
				latest = e.At
			}
			if strictFailure(e) && e.At.After(lastFailure[e.Identity]) {
				lastFailure[e.Identity] = e.At
			}
		}
		for _, e := range evidence {
			if !comparable(seed, e) || e.At.Before(latest.Add(-strictProxyWindow)) {
				continue
			}
			if e.Identity == identity {
				if strictSuccess(e) {
					contradiction = true
				}
				if strictFailure(e) {
					failures = append(failures, e)
				}
			} else if strictSuccess(e) && !e.At.Before(lastFailure[e.Identity]) {
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
		credentialContents := map[string]map[string]bool{}
		refs := []string{}
		for _, bad := range failures {
			credentials[bad.Credential] = true
			contents[bad.ContentRef] = true
			if credentialContents[bad.Credential] == nil {
				credentialContents[bad.Credential] = map[string]bool{}
			}
			credentialContents[bad.Credential][bad.ContentRef] = true
			entry := bad.EntryRef
			entries[entry] = true
			refs = append(refs, bad.RequestID)
		}
		controlled := map[string]bool{}
		contentControl := false
		for _, good := range successes {
			if credentials[good.Credential] {
				controlled[good.Credential] = true
				refs = append(refs, good.RequestID)
				if credentialContents[good.Credential][good.ContentRef] {
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
	isProxy := egress.Mode == "proxy"
	isDirect := egress.Mode == "direct" || egress.Mode == "direct_fallback" || egress.Mode == "forced_direct"
	if acc == nil || (!isProxy && !isDirect) || o.InvalidRequest || o.RequestID == "" || o.EntryRef == "" ||
		o.TargetOrigin == "" || o.TargetPath == "" || o.Method != "POST" || o.Model == "" || o.ContentRef == "" || o.ProfileRef == "" {
		return result, nil
	}
	u, err := url.Parse(o.TargetOrigin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return result, nil
	}
	o.TargetOrigin = strings.ToLower(u.Scheme + "://" + u.Host)
	path, err := url.ParseRequestURI(o.TargetPath)
	if err != nil || path.IsAbs() || path.Host != "" || path.RawQuery != "" || path.Fragment != "" || !strings.HasPrefix(o.TargetPath, "/") {
		return result, nil
	}
	if !((isProxy && o.Status == 405 && o.BusinessCode == "3012") || (o.Complete && o.Status >= 200 && o.Status < 300 && o.BusinessCode == "")) {
		return result, nil
	}
	now := s.strictObservationNow()
	if o.At.IsZero() {
		o.At = now
	}
	if o.At.Before(now.Add(-strictProxyWindow)) || o.At.After(now.Add(time.Minute)) {
		return result, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.strictCredentialCurrentLocked(acc, egress) || (isProxy && (egress.identity == "" || egress.identity != strictProxyIdentity(derefStr(acc.ProxyURL)) || !s.egressVersionCurrentLocked(acc, egress))) {
		result.Reason = "stale_snapshot"
		return result, nil
	}
	if _, blocked := s.strictProxy.Blocks[egress.identity]; isProxy && blocked {
		result.Reason = "already_retired"
		return result, nil
	}
	s.pruneStrictEvidenceLocked(now)
	for _, e := range s.strictEvidence {
		if e.RequestID == o.RequestID {
			result.Reason = "duplicate"
			return result, nil
		}
	}
	s.observeModelProbeBusinessLocked(acc, egress, o)
	identity := egress.identity
	if isDirect {
		identity = "direct"
	}
	filtered := append(s.strictEvidence, strictProxyEvidence{ProxyObservation: o, Identity: identity, Credential: s.strictCredentialRefLocked(acc), Provider: acc.Provider,
		AccountID: acc.ID, ProxyID: egress.ID, ProxyGeneration: egress.Generation, CredentialGeneration: s.strictProxy.Credentials[acc.ID].Generation})
	if len(filtered) > strictProxyEvidenceLimit {
		cut := len(filtered) - strictProxyEvidenceLimit
		until := s.strictProxy.EvidenceIncompleteUntil
		for _, dropped := range filtered[:cut] {
			// 向上取毫秒，不能在仍有效的最后一个亚毫秒提前解除限制。
			deadline := dropped.At.Add(strictProxyWindow + time.Millisecond).UnixMilli()
			if deadline > until {
				until = deadline
			}
		}
		filtered = filtered[cut:]
		if until > s.strictProxy.EvidenceIncompleteUntil {
			// 先在内存闭锁；SQL 失败也不能从截断后的缓存产生删除决定。
			s.strictProxy.EvidenceIncompleteUntil = until
			next := s.cloneStrictProxyLocked()
			if err := s.commitProxyBundleStrictLocked(context.Background(), nil, nil, nil, nil, &next); err != nil {
				s.strictEvidence = filtered
				result.Reason = "evidence_incomplete"
				return result, fmt.Errorf("持久化证据不完整窗口失败，已暂停新删除决定: %w", err)
			}
		}
	}
	s.strictEvidence = filtered
	result.Reason = "suspect"
	if o.Complete {
		result.Reason = "observed_success"
	}
	// 关闭仅停止产生新决定；墓碑、在途许可和恢复事务不受回滚开关影响。
	if !s.StrictProxyEnabled() {
		result.Reason = "decision_disabled"
		return result, nil
	}
	if now.UnixMilli() < s.strictProxy.EvidenceIncompleteUntil {
		result.Reason = "evidence_incomplete"
		return result, nil
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
		s.pruneStrictEvidenceLocked(now)
		reason, refs, target := strictDecisionFor(identity, s.strictEvidence)
		if reason == "" {
			continue
		}
		d, err := s.startStrictRetirementLocked(identity, derefStr(acc.ProxyURL), reason, target, StrictProxyRuleVersion, refs, o.At)
		result.Removed = append(result.Removed, d.Removed...)
		result.Reason = d.Reason
		result.DecisionID = d.DecisionID
		result.Assigned += d.Assigned
		result.Direct += d.Direct
		result.Pending += d.Pending
		if err != nil {
			result.Reason = "persist_failed"
			s.scheduleStrictProxyRecoveryLocked()
			return result, err
		}
	}
	return result, nil
}

func (s *Store) StrictProxyEnabled() bool {
	return s.claimBool(StrictProxyEnabledKey, true)
}

func (s *Store) egressVersionCurrentLocked(acc *model.Account, e ProxyEgress) bool {
	if e.ID != "" {
		for _, p := range s.listProxyProfilesLocked() {
			if p.ID == e.ID {
				return p.Enabled && strictProxyIdentity(p.URL) == e.identity && s.strictProxy.Generations[p.ID] == e.Generation
			}
		}
		return false
	}
	return e.identity == strictProxyIdentity(derefStr(acc.ProxyURL))
}

// AcquireProxyDispatch 与严格删除共享线性化点；已获许可流不被删除动作取消。
func (s *Store) AcquireProxyDispatch(acc *model.Account, egress ProxyEgress) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquireProxyDispatchLocked(acc, egress)
}

func (s *Store) acquireProxyDispatchLocked(acc *model.Account, egress ProxyEgress) (func(), error) {
	if egress.Mode == "custom_client" {
		return func() {}, nil
	}
	if egress.Mode != "proxy" {
		if egress.Mode != "direct" && egress.Mode != "direct_fallback" && egress.Mode != "forced_direct" {
			return nil, ErrInvalidProxyRoute
		}
		if egress.Mode != "forced_direct" && acc != nil && derefStr(acc.ProxyURL) != "" {
			return nil, ErrInvalidProxyRoute
		}
		if acc != nil && !s.strictCredentialCurrentLocked(acc, egress) {
			return nil, ErrProxyRetired
		}
		if acc != nil && egress.Mode != "forced_direct" {
			live := s.findLocked(acc.Provider, acc.ID)
			if derefStr(live.ProxyID) != "" || derefStr(live.ProxyURL) != "" ||
				s.strictProxy.RouteVersions[acc.ID].Generation != egress.RouteGeneration {
				return nil, ErrProxyRetired
			}
		}
		return func() {}, nil
	}
	if !s.proxyDispatchCurrentLocked(acc, egress) {
		return nil, ErrProxyRetired
	}
	return s.acquireStrictLeaseLocked(egress.identity), nil
}

func (s *Store) StrictProxySummary() (StrictProxyStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := StrictProxyStatus{RuleVersion: StrictProxyRuleVersion, Blocks: []StrictProxyBlockSummary{}, Pending: len(s.strictProxy.Pending),
		ImportPaused: s.strictProxy.Paused || s.strictProxy.ImportPaused, DispatchPaused: s.strictProxy.Paused,
		DecisionEnabled: s.StrictProxyEnabled(), Audit: append([]string(nil), s.strictProxy.Audit...)}
	if s.strictObservationNow().UnixMilli() < s.strictProxy.EvidenceIncompleteUntil {
		out.EvidenceLimited = true
		out.EvidenceIncompleteUntil = s.strictProxy.EvidenceIncompleteUntil
	}
	for _, count := range s.strictDispatch {
		out.ActiveDispatch += count
	}
	for _, b := range s.strictProxy.Blocks {
		out.Blocks = append(out.Blocks, StrictProxyBlockSummary{Identity: b.Identity, DecisionID: b.DecisionID, State: b.State, Reason: b.Reason, TargetOrigin: b.TargetOrigin, CreatedAt: b.CreatedAt, EvidenceCount: len(b.Evidence), RuleVersion: b.RuleVersion})
		if b.State == "retiring" {
			out.Retiring++
		}
	}
	sort.Slice(out.Blocks, func(i, j int) bool { return out.Blocks[i].Identity < out.Blocks[j].Identity })
	return out, nil
}

func (s *Store) UnblockStrictProxy(identity string, expectedDecision ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.strictProxy.Blocks[identity]
	if !ok {
		return ErrNotFound
	}
	if len(expectedDecision) > 0 && expectedDecision[0] != b.DecisionID {
		return ErrStrictDecisionConflict
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
		next.ImportPaused = false
	}
	sub, err := s.subscriptionLocked()
	if err != nil {
		return err
	}
	if b.SubscriptionKey != "" {
		delete(sub.Blocks, b.SubscriptionKey)
	}
	if !next.Paused && !next.ImportPaused && len(sub.Blocks)+len(sub.Registry) < proxysub.MaxBlocks {
		sub.ImportPaused = false
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
	if s.strictProxy.Paused {
		// 只有恢复介质重新可写且内部暂停状态已落库，才解除调度闭锁。
		if err := s.writeStrictJournalLocked(s.strictProxy.Blocks); err != nil {
			return err
		}
		next := s.cloneStrictProxyLocked()
		next.Paused = false
		if err := s.commitProxyBundleStrictLocked(context.Background(), nil, nil, nil, nil, &next); err != nil {
			return err
		}
	}
	for identity, b := range s.strictProxy.Blocks {
		if b.State == "retiring" {
			if _, err := s.retireStrictIdentityLocked(identity); err != nil {
				return err
			}
		}
	}
	return nil
}
