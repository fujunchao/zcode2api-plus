package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"zcode2api/internal/model"
)

const initializationPolicyKey = "new_account_proxy_policy"
const initializationIntent = "initialization"
const fixedProxyIntent = "fixed_proxy"

var ErrInitializationConflict = errors.New("初始化状态已变化，请刷新后重试")
var ErrInitializationUnavailable = errors.New("账号初始化或固定出口尚未就绪")

// NewAccountProxyPolicy 只配置后续任务，不追溯迁移已有账号。
type NewAccountProxyPolicy struct {
	Enabled               bool     `json:"enabled"`
	DefaultProxyID        string   `json:"default_proxy_id"`
	PoolMode              string   `json:"pool_mode"`
	ProxyIDs              []string `json:"proxy_ids"`
	MaxAttempts           int      `json:"max_attempts"`
	AttemptTimeoutSeconds int      `json:"attempt_timeout_seconds"`
	RoundBudgetSeconds    int      `json:"round_budget_seconds"`
	Revision              uint64   `json:"revision"`
}

func DefaultNewAccountProxyPolicy() NewAccountProxyPolicy {
	return NewAccountProxyPolicy{PoolMode: "all_enabled", ProxyIDs: []string{}, MaxAttempts: 32, AttemptTimeoutSeconds: 120, RoundBudgetSeconds: 900}
}

func (s *Store) initializationPolicyLocked() (NewAccountProxyPolicy, error) {
	p := DefaultNewAccountProxyPolicy()
	if raw := s.settings[initializationPolicyKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return p, fmt.Errorf("读取新号代理策略失败: %w", err)
		}
	}
	p.ProxyIDs = slices.Clone(p.ProxyIDs)
	return p, nil
}

func (s *Store) NewAccountProxyPolicy() (NewAccountProxyPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initializationPolicyLocked()
}

func (s *Store) SaveNewAccountProxyPolicy(p NewAccountProxyPolicy) (NewAccountProxyPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.initializationPolicyLocked()
	if err != nil {
		return old, err
	}
	if p.Revision != old.Revision {
		return old, ErrInitializationConflict
	}
	if (p.PoolMode != "all_enabled" && p.PoolMode != "selected") || p.MaxAttempts < 1 || p.MaxAttempts > 1000 ||
		p.AttemptTimeoutSeconds < 10 || p.AttemptTimeoutSeconds > 300 || p.RoundBudgetSeconds < 60 || p.RoundBudgetSeconds > 3600 || p.RoundBudgetSeconds < p.AttemptTimeoutSeconds {
		return old, errors.New("代理池模式或初始化预算无效")
	}
	p.ProxyIDs = dedupeIDs(p.ProxyIDs)
	if len(p.ProxyIDs) > 1000 {
		return old, errors.New("候选代理数量超过上限")
	}
	profiles := s.listProxyProfilesLocked()
	if p.Enabled {
		var target []ProxyProfile
		for _, v := range profiles {
			if v.ID == p.DefaultProxyID {
				target = append(target, v)
			}
		}
		if _, ok := s.chooseProxyLocked(target, nil, "", ""); !ok {
			return old, errors.New("请选择启用且可准入的默认业务代理")
		}
		if p.PoolMode == "selected" && len(p.ProxyIDs) == 0 {
			return old, errors.New("自选取额池不能为空")
		}
	}
	for _, id := range p.ProxyIDs {
		found := false
		for _, v := range profiles {
			if v.ID == id {
				found = true
				break
			}
		}
		if !found || id == p.DefaultProxyID {
			return old, errors.New("取额候选不存在或与默认业务代理冲突")
		}
	}
	p.Revision++
	raw, err := json.Marshal(p)
	if err != nil {
		return old, err
	}
	if err := s.setMeta(initializationPolicyKey, string(raw)); err != nil {
		return old, err
	}
	s.settings[initializationPolicyKey] = string(raw)
	s.publishSettings()
	return p, nil
}

// AccountCreationOptions 只能由受控新增入口提供，是否新号仍由 Store 判重决定。
type AccountCreationOptions struct {
	InitializeProxy bool
	Source          string
	LoginProxy      *ProxyProfile
	DisabledModels  []string // 导入的新号元数据必须与账号、任务一起提交。
}

type InitializationCandidate struct {
	ID         string `json:"id"`
	Identity   string `json:"identity"`
	Generation uint64 `json:"generation"`
}

// InitializationTask 不包含凭据、代理认证或验证码；账号数据和此表共用事务。
type InitializationTask struct {
	Version                int                       `json:"version"`
	AccountID              string                    `json:"account_id"`
	Source                 string                    `json:"source"`
	Revision               uint64                    `json:"revision"`
	Managed                bool                      `json:"managed"`
	Paused                 bool                      `json:"paused"`
	State                  string                    `json:"state"`
	Reason                 string                    `json:"reason"`
	Policy                 NewAccountProxyPolicy     `json:"policy"`
	DefaultProxyID         string                    `json:"default_proxy_id"`
	DefaultIdentity        string                    `json:"default_identity"`
	CredentialGeneration   uint64                    `json:"credential_generation"`
	RouteGeneration        uint64                    `json:"route_generation"`
	CurrentProxyID         string                    `json:"current_proxy_id"`
	CurrentProxyGeneration uint64                    `json:"current_proxy_generation"`
	RunID                  string                    `json:"run_id"`
	Attempts               int                       `json:"attempts"`
	Candidates             []InitializationCandidate `json:"candidates"`
	Tried                  []string                  `json:"tried"`
	PlanIDs                []string                  `json:"plan_ids"`
	Uncertain              bool                      `json:"uncertain"`
	CreatedAt              int64                     `json:"created_at"`
	LastAttemptAt          int64                     `json:"last_attempt_at"`
	Deadline               int64                     `json:"deadline"`
	GrantedAt              int64                     `json:"granted_at"`
	NextAt                 int64                     `json:"next_at"`
	LastError              string                    `json:"last_error"`
}

func (t *InitializationTask) Clone() *InitializationTask {
	if t == nil {
		return nil
	}
	v := *t
	v.Policy.ProxyIDs = slices.Clone(t.Policy.ProxyIDs)
	v.Candidates = slices.Clone(t.Candidates)
	v.Tried = slices.Clone(t.Tried)
	v.PlanIDs = slices.Clone(t.PlanIDs)
	return &v
}

func (s *Store) newInitializationLocked(a *model.Account, options []AccountCreationOptions) (*InitializationTask, error) {
	if len(options) == 0 || !options[0].InitializeProxy || a.Provider != model.ProviderZai || a.Mode != "jwt" {
		return nil, nil
	}
	p, err := s.initializationPolicyLocked()
	if err != nil || !p.Enabled {
		return nil, err
	}
	t := &InitializationTask{Version: 1, AccountID: a.ID, Source: options[0].Source, Revision: 1, Managed: true, State: "queued", Reason: "queued", Policy: p, DefaultProxyID: p.DefaultProxyID, CreatedAt: time.Now().UnixMilli()}
	for _, profile := range s.listProxyProfilesLocked() {
		if profile.ID == p.DefaultProxyID {
			t.DefaultIdentity = strictProxyIdentity(profile.URL)
		}
	}
	if selected := options[0].LoginProxy; selected != nil {
		for _, current := range s.listProxyProfilesLocked() {
			if current.ID == selected.ID && current.Generation == selected.Generation && current.URL == selected.URL {
				if _, ok := s.chooseProxyLocked([]ProxyProfile{current}, nil, "", ""); ok {
					a.ProxyID, a.ProxyURL = &current.ID, &current.URL
				}
			}
		}
	}
	return t, nil
}

func persistInitializationOnTx(tx *sql.Tx, task *InitializationTask) error {
	if task == nil {
		return nil
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO account_initializations (account_id,data) VALUES (?,?)", task.AccountID, string(raw))
	return err
}

func (s *Store) persistCreatedAccountLocked(a *model.Account, options []AccountCreationOptions) error {
	if len(options) > 0 && options[0].DisabledModels != nil {
		a.SetDisabledModels(options[0].DisabledModels)
	}
	task, err := s.newInitializationLocked(a, options)
	if err != nil {
		return err
	}
	if task == nil {
		return s.persistAccountLocked(a)
	}
	next := s.cloneStrictProxyLocked()
	trackStrictAccountVersions(&next, a)
	task.CredentialGeneration = next.Credentials[a.ID].Generation
	setStrictIntent(&next, a, initializationIntent)
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = persistAccountOnTx(tx, a); err != nil {
		return err
	}
	if err = persistInitializationOnTx(tx, task); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", strictProxyStateKey, string(raw)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.strictProxy = next
	a.CredentialGeneration = task.CredentialGeneration
	s.initializations[a.ID] = task
	a.Initialization = s.initializationViewLocked(a)
	return nil
}

func (s *Store) loadInitializations() error {
	rows, err := s.db.Query("SELECT account_id,data FROM account_initializations")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		var task InitializationTask
		if err = json.Unmarshal([]byte(raw), &task); err != nil || task.Version != 1 || task.AccountID != id {
			return fmt.Errorf("初始化任务 %s 损坏或版本不支持", id)
		}
		s.initializations[id] = &task
	}
	return rows.Err()
}

func (s *Store) initializationViewLocked(a *model.Account) *model.InitializationView {
	if a == nil {
		return nil
	}
	t := s.initializations[a.ID]
	if t == nil {
		return nil
	}
	blocked, reason := t.Managed && t.State != "completed", t.Reason
	if t.Managed && t.State == "completed" && (derefStr(a.ProxyID) != t.DefaultProxyID || !s.proxyBindingAvailableLocked(a)) {
		blocked, reason = true, "default_unavailable"
	}
	return &model.InitializationView{State: t.State, Reason: reason, Revision: t.Revision, Managed: t.Managed, Blocked: blocked, Paused: t.Paused,
		DefaultProxyID: t.DefaultProxyID, CurrentProxyID: derefStr(a.ProxyID), Attempts: t.Attempts, MaxAttempts: t.Policy.MaxAttempts, GrantedAt: t.GrantedAt, NextAt: t.NextAt, LastError: t.LastError}
}

func (s *Store) initializationManagedLocked(id string) bool {
	t := s.initializations[id]
	return t != nil && t.Managed
}

func (s *Store) snapshotLocked(a *model.Account) *model.Account {
	copy := a.Clone()
	if copy != nil {
		copy.Initialization = s.initializationViewLocked(a)
	}
	return copy
}

func (s *Store) Initialization(id string) *InitializationTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initializations[id].Clone()
}
