package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"zcode2api/internal/web"
)

// 代理上行带宽状态：让「这条线路有多快」跨请求活下来。
//
// 为什么需要它：网关对「慢」原本只有一条判据——上行闸门「按实测速率算，剩余字节
// 在本次尝试预算内传不完」。对 324KB 的中位请求体、25s 的首字节预算，这等价于
// 「速率 < 13KB/s 才中止」，实测 32 个采样触发 0 次。于是 8KB/s 的线路在语义上
// 是**完全合格**的线路，只是每个请求都要用户干等 40 秒。
//
// 为什么不能复用 proxy_soft_failure 的 Slow：Slow 记的是「首字节 >30s 但成功」，
// 它混了两类完全不同的原因——①线路带宽不足（换线可解）②上游排队慢（换线无用）。
// 把两者当成一个数，选路会朝错误方向优化。这里直接量带宽本身。
//
// 为什么只收真实请求的采样、不收探针 kbps：探针（networkprobe.CheckTunnelWrite）
// 写的是 TLS 端口，边缘 10ms 内 400 并重置，bytes/ms 变成「缓冲容量 + 对端关闭
// 时间」。实测同一条线高估 5.2x/10.8x、也低估 5.4x，**方向都不确定**。
// 真实请求走 TLS + 324KB body，远超 128KB 的缓冲吸收窗口，数据可信。
const (
	proxyUplinkKey   = "proxy_uplink_state"
	proxyUplinkLimit = 4096
	// proxyUplinkTTL 免费代理寿命以分钟计，6h 没有新样本的条目等同于重新建档。
	proxyUplinkTTL = 6 * time.Hour
	// proxyUplinkAlpha EWMA 平滑系数。偏小以抗单次抖动：一次网络抖动不该把
	// 一条 1500KB/s 的线路判成慢线（那会让它在接下来几十分钟都不被选中）。
	proxyUplinkAlpha = 0.3
	// proxyUplinkMinSample 少于该样本数不参与选路门槛判定：单次采样可能撞上
	// TLS 冷启动或对端限速，不足以定性。但仍会记账，等凑够样本。
	proxyUplinkMinSample = 2
	// proxyUplinkCommitInterval 落盘节流。带宽是每请求更新，比软失败（只在出错时写）
	// 高两个数量级；每次都写 meta 会把这个热路径变成 DB 写路径。
	proxyUplinkCommitInterval = 60 * time.Second
)

// 带宽分档门槛（KB/s）。分档而不是逐值比较：EWMA 会小幅浮动，逐值比较会让
// 「最优组」每小时换一次人，反而引入抖动。分档把档内差异吸收掉。
//
// 600 的来历：324KB 请求体在 600KB/s 上只需 0.54s，加上 TLS 握手与上游排队
// 仍能落在 2-3s 量级；150KB/s 需 2.2s，已进入用户可感知的慢区。
const (
	UplinkBandFast = 600
	UplinkBandMid  = 150
)

// ProxyUplink 是一条线路的上行带宽观测；只保存去敏身份，不含凭据。
type ProxyUplink struct {
	Identity   string `json:"identity"`
	ProfileID  string `json:"profile_id"`
	Generation uint64 `json:"generation"`
	// KBps 是上行速率的指数加权移动平均（KB/s）。
	KBps float64 `json:"kbps"`
	// Sample 累计样本数；小于 proxyUplinkMinSample 时只记账、不参与选路。
	Sample int   `json:"sample"`
	LastAt int64 `json:"last_at"`
	// 同一巡检轮会多次保存；阶段位分别记录首次探测与复核，随带宽一起持久化。
	ProbeRound  string `json:"probe_round,omitempty"`
	ProbeStages uint8  `json:"probe_stages,omitempty"`
}

type proxyUplinkState struct {
	Entries map[string]ProxyUplink `json:"entries"`
}

// UplinkBand 把带宽值映射到档位：3=快 2=中 1=慢 0=无样本/不可信。
func UplinkBand(kbps float64, sample int) int {
	if sample < proxyUplinkMinSample || kbps <= 0 {
		return 0
	}
	switch {
	case kbps >= UplinkBandFast:
		return 3
	case kbps >= UplinkBandMid:
		return 2
	default:
		return 1
	}
}

// RecordProxyUplink 记录一次真实请求的实测上行速率。
//
// 只接受代理模式的采样：直连不经线路，记进去会污染线路画像。
// kbps<=0 表示本次未采到样本（请求体低于上行闸门门槛，或上传太快不足以计时），
// 直接忽略——**不能当成 0KB/s 记账**，那会把快线误判成慢线。
func (s *Store) RecordProxyUplink(e ProxyEgress, kbps float64, now time.Time) {
	if e.Mode != "proxy" || e.identity == "" || e.ID == "" || kbps <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strictClosed {
		return
	}
	if s.proxyUplink.Entries == nil {
		s.proxyUplink.Entries = map[string]ProxyUplink{}
	}
	ent, exists := s.proxyUplink.Entries[e.identity]
	if !exists {
		ent = ProxyUplink{Identity: e.identity}
	}
	ent.ProfileID, ent.Generation = e.ID, e.Generation
	if ent.Sample <= 0 {
		ent.KBps = kbps
	} else {
		ent.KBps = ent.KBps*(1-proxyUplinkAlpha) + kbps*proxyUplinkAlpha
	}
	ent.Sample++
	ent.LastAt = now.UnixMilli()
	s.proxyUplink.Entries[e.identity] = ent
	if now.UnixMilli()-s.proxyUplinkCommitAt >= proxyUplinkCommitInterval.Milliseconds() {
		s.proxyUplinkCommitAt = now.UnixMilli()
		if err := s.commitProxyUplinkLocked(); err != nil {
			// 落盘失败不影响选路：内存里的样本仍然有效，下一次节流窗口会重试。
			web.Warn("proxy-uplink", "上行带宽状态落库失败: "+err.Error())
		}
	}
}

// ProxyUplinkSnapshot 返回 identity → 带宽 的只读快照；样本不足的线路不出现在结果里。
func (s *Store) ProxyUplinkSnapshot() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proxyUplinkSnapshotLocked()
}

// proxyUplinkSnapshotLocked 供已持锁的调用方（selectAccount）使用。
func (s *Store) proxyUplinkSnapshotLocked() map[string]float64 {
	out := make(map[string]float64, len(s.proxyUplink.Entries))
	for identity, ent := range s.proxyUplink.Entries {
		if ent.Sample < proxyUplinkMinSample || ent.KBps <= 0 {
			continue
		}
		out[identity] = ent.KBps
	}
	return out
}

// ProxyUplinkKBps 返回单条线路的带宽；样本不足或不存在时返回 (0,false)。
func (s *Store) ProxyUplinkKBps(identity string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.proxyUplink.Entries[identity]
	if !ok || ent.Sample < proxyUplinkMinSample || ent.KBps <= 0 {
		return 0, false
	}
	return ent.KBps, true
}

// ProxyUplinks 供后台展示；按最近活跃倒序返回深拷贝。
func (s *Store) ProxyUplinks() []ProxyUplink {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ProxyUplink, 0, len(s.proxyUplink.Entries))
	for _, ent := range s.proxyUplink.Entries {
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastAt != out[j].LastAt {
			return out[i].LastAt > out[j].LastAt
		}
		return out[i].Identity < out[j].Identity
	})
	return out
}

func (s *Store) commitProxyUplinkLocked() error {
	s.pruneProxyUplinkLocked(time.Now())
	raw, err := json.Marshal(s.proxyUplink)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO meta (key,value) VALUES (?,?)", proxyUplinkKey, string(raw))
	return err
}

// pruneProxyUplinkLocked 清理过期条目并按最近活跃截断；调用方须持有 s.mu。
func (s *Store) pruneProxyUplinkLocked(now time.Time) {
	if s.proxyUplink.Entries == nil {
		s.proxyUplink.Entries = map[string]ProxyUplink{}
		return
	}
	cut := now.Add(-proxyUplinkTTL).UnixMilli()
	for identity, ent := range s.proxyUplink.Entries {
		if ent.LastAt < cut {
			delete(s.proxyUplink.Entries, identity)
		}
	}
	if len(s.proxyUplink.Entries) <= proxyUplinkLimit {
		return
	}
	type entry struct {
		identity string
		at       int64
	}
	list := make([]entry, 0, len(s.proxyUplink.Entries))
	for identity, ent := range s.proxyUplink.Entries {
		list = append(list, entry{identity, ent.LastAt})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at < list[j].at })
	for i := 0; i < len(list)-proxyUplinkLimit; i++ {
		delete(s.proxyUplink.Entries, list[i].identity)
	}
}

// uplinkRankOfIdentity 只用于选路排序：快(4) > 中(3) > 未知(2) > 慢(1)。
// 快照已过滤不足两个样本的线路；未知线路不能被已知慢线永久排除。
func uplinkRankOfIdentity(snapshot map[string]float64, identity string) int {
	band := UplinkBand(snapshot[identity], proxyUplinkMinSample)
	if band == 0 {
		return 2
	}
	if band == 1 {
		return 1
	}
	return band + 1
}
