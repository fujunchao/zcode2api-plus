package model

// InitializationView 是持久任务的只读投影，不写入兼容的 accounts.data。
// 任务与路由约束由 Store 发布，后台展示与普通选号使用同一口径。
type InitializationView struct {
	State          string `json:"state"`
	Reason         string `json:"reason"`
	Revision       uint64 `json:"revision"`
	Managed        bool   `json:"managed"`
	Blocked        bool   `json:"blocked"`
	Paused         bool   `json:"paused"`
	DefaultProxyID string `json:"default_proxy_id"`
	CurrentProxyID string `json:"current_proxy_id"`
	Attempts       int    `json:"attempts"`
	MaxAttempts    int    `json:"max_attempts"`
	GrantedAt      int64  `json:"granted_at,omitempty"`
	NextAt         int64  `json:"next_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// InitialQuotaObservation 仅在真实成功查询中产生；Snapshot 只提供待提交的额度字段。
type InitialQuotaObservation struct {
	Kind         string // unknown / missing / allocated / available
	Snapshot     *Account
	CheckedAt    float64
	Error        string
	ProxyFailure bool
	NextAt       int64
}

func (v *InitializationView) Clone() *InitializationView {
	if v == nil {
		return nil
	}
	copy := *v
	return &copy
}

func (a *Account) IsInitializing() bool {
	return a != nil && a.Initialization != nil && a.Initialization.Managed && a.Initialization.State != "completed"
}

func (a *Account) InitializationBlocked() bool {
	return a != nil && a.Initialization != nil && a.Initialization.Managed && a.Initialization.Blocked
}

func (a *Account) UsesFixedProxy() bool {
	return a != nil && a.Initialization != nil && a.Initialization.Managed && a.Initialization.GrantedAt > 0
}
