// Package networkprobe 提供无账号的浅层探测；不决定删除、不访问模型正文或保存凭据。
package networkprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/proxy"
)

const Timeout = 6 * time.Second

// TargetsKey 绑定探测站点集合，配置变更后旧站点的成功不能冒充新配置健康。
func TargetsKey(targets []string) string {
	sum := sha256.Sum256([]byte(strings.Join(targets, "\n")))
	return hex.EncodeToString(sum[:])
}

type Target struct {
	URL     string `json:"url"`
	Host    string `json:"host"`
	OK      bool   `json:"ok"`
	Blocked bool   `json:"blocked"`
	Status  int    `json:"status,omitempty"`
	MS      int64  `json:"ms"`
	Error   string `json:"error,omitempty"`
}

type Report struct {
	OK      bool     `json:"ok"`
	Blocked bool     `json:"blocked"`
	Error   string   `json:"error"`
	MS      int64    `json:"ms"`
	Targets []Target `json:"targets"`
	// Tunnel 是可选的上传能力测量（隧道裸写）。指针 + omitempty：旧记录没有这个字段，
	// 反序列化后为 nil，不影响既有判定。
	Tunnel *TunnelReport `json:"tunnel,omitempty"`
}

// TunnelReport 是一次隧道裸写的结果。Bytes/MS/Kbps 是原始测量值，OK 是分级判定。
type TunnelReport struct {
	OK        bool   `json:"ok"`
	Bytes     int64  `json:"bytes"`
	MS        int64  `json:"ms"`
	Kbps      int64  `json:"kbps"`
	// SteadyKBps 是**跳过前 skipBytes 之后**的稳态速率；采样不足时为 0。
	//
	// 为什么必须分开：所有线路前 ~128KB 会瞬间写进内核缓冲（0.2-0.3s），之后才
	// 断崖跌到真实链路速率。Kbps（全程平均）因此系统性偏高——实测同一条线
	// 报 165KB/s 而稳态只有 32KB/s（高估 5.2x）、报 130 而稳态 12（高估 10.8x）。
	// 判定必须用稳态值，否则「突发后卡死」的线路会被判成健康。
	SteadyKBps int64 `json:"steady_kbps,omitempty"`
	EarlyStop  bool  `json:"early_stop,omitempty"`
	Error      string `json:"error,omitempty"`
}

// tunnelJudgement 是判定所需的全部输入；抽成结构体是为了让分级判定可以被单测覆盖
// （CheckTunnelWrite 需要真服务器，判定本身不需要）。
type tunnelJudgement struct {
	bytes    int64
	size     int64
	ms       int64
	budgetMS int64
	minBytes int64
	minKbps  int64
	kbps     int64
	writeErr error
}

// judgeTunnelOK 按「写满 / 早停」两种成功形态给出判定。kbps 必须是稳态速率
// （由调用方算好传进来），用全程平均会把「突发很快、持续很差」的线路判成健康。
func judgeTunnelOK(j tunnelJudgement) bool {
	if j.kbps < j.minKbps {
		return false
	}
	if j.bytes >= j.size {
		return true
	}
	// 对端提前关闭不是链路故障，但只有在「关得够晚、传得够快」时才算通过。
	return j.writeErr != nil && j.bytes >= j.minBytes && j.ms < j.budgetMS*2/3
}

// tunnelMark 是写入过程中的一个里程碑，用于事后算稳态速率。
type tunnelMark struct {
	bytes int64
	ms    int64
}

// steadyKbps 计算跳过 skipBytes 之后区间的速率（KB/s）。
//
// 采样不足或区间无效时返回 false——调用方应退回全程平均，而不是当成 0KB/s，
// 否则会把「还没测出来」误判成「完全传不动」。
func steadyKbps(marks []tunnelMark, skipBytes int64) (int64, bool) {
	if len(marks) < 2 {
		return 0, false
	}
	base := -1
	for i, m := range marks {
		if m.bytes >= skipBytes {
			base = i
			break
		}
	}
	if base < 0 || base >= len(marks)-1 {
		return 0, false
	}
	end := marks[len(marks)-1]
	dw := end.bytes - marks[base].bytes
	dt := end.ms - marks[base].ms
	if dw <= 0 || dt <= 0 {
		return 0, false
	}
	return dw * 1000 / dt / 1024, true
}

// CheckTunnelWrite 走裸隧道向 target 写 size 字节，用「写了多少 / 用了多久」判断上传能力。
//
// 判据分级（对端早关 ≠ 链路慢）：
//  1. 预算内写满，且稳态速率 ≥ minKbps → OK
//  2. 写错误但已写 ≥ minBytes、用时 < 预算的 2/3 且稳态速率 ≥ minKbps → OK
//     （对端提前关闭是正常现象，只要关得足够晚、传得足够快）
//  3. 其余（含撞上写超时、写满但很慢）→ 失败
//
// 「写满但很慢」也判失败，是相对旧行为的一处收紧：旧判据「写满即 OK」把
// minKbps 变成了隐式条件（size/budget 恰好等于速率底线），一旦有人下调 size
// 就会失守。现在速率是显式判据，size 可以独立调整。
//
// skipBytes 是稳态速率的统计起点，必须覆盖内核缓冲的吸收窗口（实测约 128KB）。
// 传 0 表示不做稳态统计，退回全程平均（旧行为）。
//
// 刻意**不**用「已写 ≥ minBytes 且平均速率达标」判 OK：内核缓冲会先吞掉几百 KB，
// 之后对端不读、写阻塞到超时，平均速率仍然好看——那正是要抓的「传不动」。
func CheckTunnelWrite(ctx context.Context, proxyURL, target string, size, skipBytes int64, budget time.Duration, minBytes, minKbps int64) TunnelReport {
	rep := TunnelReport{}
	start := time.Now()
	conn, err := proxy.DialTunnel(ctx, proxyURL, target)
	if err != nil {
		rep.MS = time.Since(start).Milliseconds()
		rep.Error = err.Error()
		return rep
	}
	defer conn.Close()

	_ = conn.SetWriteDeadline(start.Add(budget))
	buf := make([]byte, 32<<10)
	for i := range buf {
		buf[i] = 'x'
	}
	var writeErr error
	var marks []tunnelMark
	for rep.Bytes < size {
		chunk := int64(len(buf))
		if left := size - rep.Bytes; left < chunk {
			chunk = left
		}
		n, err := conn.Write(buf[:chunk])
		rep.Bytes += int64(n)
		if n > 0 {
			// 每个分块记一个里程碑，事后用来算跳过缓冲窗口的稳态速率。
			marks = append(marks, tunnelMark{bytes: rep.Bytes, ms: time.Since(start).Milliseconds()})
		}
		if err != nil {
			writeErr = err
			rep.EarlyStop = true
			break
		}
	}
	rep.MS = time.Since(start).Milliseconds()
	// 毫秒级完成时 MS 可能为 0：用 1ms 兜底，避免把「极快」算成 0 KB/s。
	elapsedMS := rep.MS
	if elapsedMS < 1 {
		elapsedMS = 1
	}
	rep.Kbps = rep.Bytes * 1000 / elapsedMS / 1024
	if skipBytes > 0 {
		if kbps, ok := steadyKbps(marks, skipBytes); ok {
			rep.SteadyKBps = kbps
		}
	}
	// 判定用稳态值：全程平均会被前 128KB 的缓冲吸收抬高（实测高估 5-11 倍），
	// 用它会放行「突发后卡死」的线路。稳态采样不足时退回全程平均。
	judgeKbps := rep.Kbps
	if rep.SteadyKBps > 0 {
		judgeKbps = rep.SteadyKBps
	}
	rep.OK = judgeTunnelOK(tunnelJudgement{
		bytes:    rep.Bytes,
		size:     size,
		ms:       rep.MS,
		budgetMS: budget.Milliseconds(),
		minBytes: minBytes,
		minKbps:  minKbps,
		kbps:     judgeKbps,
		writeErr: writeErr,
	})
	if writeErr != nil {
		rep.Error = writeErr.Error()
	}
	return rep
}

func Targets() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range []string{config.UpstreamZai, config.UpstreamZaiFallback, config.ZcodeBillingBase} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		origin := u.Scheme + "://" + u.Host + "/"
		if !seen[origin] {
			seen[origin] = true
			out = append(out, origin)
		}
	}
	return out
}

func Client(raw string) (*http.Client, error) {
	t, err := proxy.TransportForTimeout(raw, Timeout)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: t, Timeout: Timeout}, nil
}

func Blocked(resp *http.Response, body string) bool {
	if resp.Header.Get("Cf-Mitigated") != "" {
		return true
	}
	for _, marker := range []string{"Just a moment", "Attention Required", "cf-chl-", "Enable JavaScript and cookies"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return resp.StatusCode == 403 && strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudflare")
}

func CheckTarget(ctx context.Context, client *http.Client, target string) Target {
	u, _ := url.Parse(target)
	host := ""
	if u != nil {
		host = u.Host
	}
	out := Target{URL: target, Host: host}
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		out.Error = "invalid_target"
		return out
	}
	// 无 Authorization、Cookie 或模型 metadata；直连不受环境代理影响。
	req.Header.Set("User-Agent", config.UserAgent)
	resp, err := client.Do(req)
	out.MS = time.Since(started).Milliseconds()
	if err != nil {
		out.Error = "connection_failed"
		if ctx.Err() != nil {
			out.Error = "canceled"
		}
		return out
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		out.Error = "incomplete_response"
		return out
	}
	out.OK, out.Status, out.Blocked = true, resp.StatusCode, Blocked(resp, string(body))
	return out
}

func Check(ctx context.Context, client *http.Client, targets []string) Report {
	r := Report{Targets: []Target{}}
	if len(targets) == 0 {
		r.Error = "no_targets"
		return r
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			r.OK = false
			r.Error = "canceled"
			return r
		}
		e := CheckTarget(ctx, client, target)
		r.Targets = append(r.Targets, e)
		if e.OK && !e.Blocked {
			if !r.OK {
				r.MS = e.MS
			}
			r.OK = true
		}
		if e.Blocked {
			r.Blocked = true
		}
		if e.Error != "" && r.Error == "" {
			r.Error = e.Error
		}
	}
	if r.OK {
		r.Blocked = false
		r.Error = ""
	} else if r.Error == "" && r.Blocked {
		r.Error = "edge_blocked"
	}
	return r
}
