package store

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/networkprobe"
)

type NetworkHealthRecord struct {
	ProfileID      string               `json:"profile_id"`
	Generation     uint64               `json:"generation"`
	PolicyRevision uint64               `json:"policy_revision"`
	State          string               `json:"state"`
	At             int64                `json:"at"`
	RecheckAt      int64                `json:"recheck_at,omitempty"`
	RoundID        string               `json:"round_id"`
	TargetsKey     string               `json:"targets_key"`
	First          networkprobe.Report  `json:"first"`
	Second         *networkprobe.Report `json:"second,omitempty"`
	Control        *networkprobe.Report `json:"control,omitempty"`
	ControlID      string               `json:"control_id,omitempty"`
	Removed        bool                 `json:"removed"`
}

func (s *Store) initNetworkHealth() error {
	_, err := s.db.Exec("CREATE TABLE IF NOT EXISTS proxy_network_health (profile_id TEXT PRIMARY KEY, data TEXT NOT NULL)")
	return err
}

func (s *Store) NetworkHealth() ([]NetworkHealthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query("SELECT data FROM proxy_network_health")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NetworkHealthRecord{}
	for rows.Next() {
		var raw string
		var r NetworkHealthRecord
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		if r.PolicyRevision != s.modelProbe.Policy.Revision && r.State == "waiting" {
			r.State = "interrupted"
			r.RecheckAt = 0
		}
		if p, ok := s.probeProfileLocked(r.ProfileID); ok && p.Generation != r.Generation {
			r.State = "changed"
			r.RecheckAt = 0
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At > out[j].At })
	return out, rows.Err()
}

func (s *Store) SaveNetworkHealth(policy ModelProbeSettings, records []NetworkHealthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelProbe.Policy.Revision != policy.Revision || !s.modelProbe.Policy.NetworkEnabled {
		return ErrModelProbeConflict
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range records {
		if p, ok := s.probeProfileLocked(r.ProfileID); ok && p.Generation != r.Generation {
			r.State = "changed"
			r.RecheckAt = 0
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT OR REPLACE INTO proxy_network_health(profile_id,data) VALUES (?,?)", r.ProfileID, string(raw)); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM proxy_network_health WHERE rowid NOT IN (SELECT rowid FROM proxy_network_health ORDER BY rowid DESC LIMIT 10000)"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ClearInterruptedNetworkHealth() error {
	records, err := s.NetworkHealth()
	if err != nil {
		return err
	}
	for i := range records {
		if records[i].State == "waiting" {
			records[i].State = "interrupted"
			records[i].RecheckAt = 0
		}
	}
	// 启动恢复只改展示状态，不重建旧网络删除证据。
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err = s.db.Exec("UPDATE proxy_network_health SET data=? WHERE profile_id=?", string(raw), r.ProfileID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ApplyNetworkHealth(ctx context.Context, policy ModelProbeSettings, expected []ProxyProfile) ([]string, ProxyReassign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ProxyReassign{}, ctx.Err()
	}
	if s.modelProbe.Policy.Revision != policy.Revision || !s.modelProbe.Policy.NetworkEnabled {
		return nil, ProxyReassign{}, ErrModelProbeConflict
	}
	if s.modelProbe.Policy.Mode != "auto_retire" {
		return nil, ProxyReassign{}, nil
	}
	return s.purgeProxyProfilesIfCurrentLocked(expected)
}

type networkHealthTransport struct {
	store   *Store
	policy  uint64
	profile ProxyProfile
	base    http.RoundTripper
}

func (t *networkHealthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	release, err := t.store.acquireNetworkHealth(t.policy, t.profile)
	if err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		return resp, err
	}
	if resp == nil || resp.Body == nil {
		release()
		return resp, err
	}
	resp.Body = &strictProxyBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

func (s *Store) acquireNetworkHealth(revision uint64, expected ProxyProfile) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed || s.modelProbe.Policy.Revision != revision || !s.modelProbe.Policy.NetworkEnabled {
		return nil, ErrModelProbeConflict
	}
	if expected.ID == "" {
		return func() {}, nil
	}
	p, ok := s.probeProfileLocked(expected.ID)
	if !ok || p.Generation != expected.Generation || strictProxyIdentity(p.URL) != strictProxyIdentity(expected.URL) {
		return nil, ErrProxyRetired
	}
	return s.acquireStrictLeaseLocked(strictProxyIdentity(p.URL)), nil
}

func (s *Store) GuardNetworkHealthClient(client *http.Client, policy ModelProbeSettings, profile ProxyProfile) *http.Client {
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = &networkHealthTransport{s, policy.Revision, profile, base}
	return &copy
}

// 旧字段仍可读写，但真实调度只读取统一策略。非法旧值沿用原环境变量回退语义。
func (s *Store) networkPolicyFromLegacy(values map[string]string) (modelProbePersistent, bool) {
	_, a := values["proxy_health_enabled"]
	_, b := values["proxy_health_interval"]
	next := s.modelProbe.modelProbePersistent
	if !a && !b {
		return next, false
	}
	if a {
		v, err := strconv.ParseBool(values["proxy_health_enabled"])
		if err != nil {
			v = config.ProxyHealthEnabled
		}
		next.Policy.NetworkEnabled = v
	}
	if b {
		v, err := strconv.Atoi(values["proxy_health_interval"])
		if err != nil || v < 1 {
			v = config.ProxyHealthIntervalMinutes
		}
		next.Policy.NetworkIntervalMinutes = v
	}
	next.Policy.Revision++
	next.Policy.HealthVersion = 1
	return next, true
}

func (s *Store) migrateNetworkHealthPolicy() {
	p := &s.modelProbe.Policy
	if p.HealthVersion > 0 {
		return
	}
	p.NetworkEnabled = s.ProxyHealthEnabled()
	p.NetworkIntervalMinutes = s.ProxyHealthIntervalMinutes()
	p.NetworkConfirmSeconds = 10
	p.HealthVersion = 1
	if !p.Enabled && p.NetworkEnabled {
		p.Mode = "auto_retire"
	} else if p.Enabled && p.Mode == "observe" && p.NetworkEnabled {
		p.MigrationNotice = "网络自动清理与模型观察模式已保守合并为仅观察，请确认统一策略"
	}
}

func NetworkHealthyAt(r NetworkHealthRecord, p ProxyProfile, policy ModelProbeSettings, key string, now time.Time) bool {
	return r.ProfileID == p.ID && r.Generation == p.Generation && r.PolicyRevision == policy.Revision && r.TargetsKey == key && (r.State == "healthy" || r.State == "recovered") && r.At <= now.UnixMilli() && r.At > now.Add(-5*time.Minute).UnixMilli()
}
