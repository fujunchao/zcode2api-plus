// CPU governor：验证码预求解的负载自适应。
//
// 预求解会在空闲时也持续拉起浏览器求解，若无约束会长期占用 CPU。这里按进程可见的
// CPU 累计 tick 估算占用率：Linux 优先读 /proc/stat，其余平台（或读取失败）退回
// runtime/metrics 的进程 CPU 时间。只使用标准库，不引入新依赖。
//
// 采样是差分的：第一次只记录基线，第二次起用 (busy 增量 / total 增量) 计算占用率，
// 避免把开机以来的累计值当成瞬时值。采样失败时保持上一次比例，不影响池继续补货。
package captcha

import (
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"
)

// cpuSample 一次 CPU 累计计数（单位：平台 tick，只用于求增量比例）。
type cpuSample struct {
	Busy  uint64
	Total uint64
}

// cpuGovernor 维护最近一次采样得到的 CPU 占用率。
type cpuGovernor struct {
	sample  func() (cpuSample, bool)
	last    cpuSample
	hasLast bool
	percent float64
}

func newCPUGovernor(sample func() (cpuSample, bool)) *cpuGovernor {
	if sample == nil {
		sample = readCPUSample
	}
	return &cpuGovernor{sample: sample}
}

// Update 采样一次；只有拿到有效的两次样本才更新占用率。
func (g *cpuGovernor) Update() {
	if g == nil {
		return
	}
	cur, ok := g.sample()
	if !ok {
		return
	}
	if g.hasLast && cur.Total > g.last.Total {
		total := cur.Total - g.last.Total
		busy := cur.Busy
		if busy >= g.last.Busy {
			busy -= g.last.Busy
		} else {
			busy = 0
		}
		if busy > total {
			busy = total
		}
		g.percent = 100 * float64(busy) / float64(total)
	}
	g.last = cur
	g.hasLast = true
}

// Percent 最近一次采样得到的 CPU 占用率（0–100）。
func (g *cpuGovernor) Percent() float64 {
	if g == nil {
		return 0
	}
	return g.percent
}

// Factor 把 CPU 占用映射为 refill 目标/并发的缩放系数。
// 阈值留出余量：70% 开始减半，85% 降到四分之一；任何情况下至少保留 1 路。
func (g *cpuGovernor) Factor() float64 {
	if g == nil {
		return 1
	}
	switch {
	case g.percent >= 85:
		return 0.25
	case g.percent >= 70:
		return 0.5
	default:
		return 1
	}
}

// readCPUSample 读取 CPU 累计计数：Linux /proc/stat 优先，其余平台用
// runtime/metrics 的进程 CPU 时间折算为「进程 CPU / (墙钟 × NumCPU)」。
func readCPUSample() (cpuSample, bool) {
	if runtime.GOOS == "linux" {
		if sample, ok := readProcStatSample(); ok {
			return sample, true
		}
	}
	return readProcessCPUSample()
}

// readProcStatSample 解析 /proc/stat 第一行（聚合 cpu 行）。
//
// 字段：user nice system idle iowait irq softirq steal ...
// idle + iowait 视为空闲，其余视为忙；guest/guest_nice 已包含在 user/nice 中，
// 不再重复计入。
func readProcStatSample() (cpuSample, bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var values [8]uint64
		for i := 0; i < 8 && i+1 < len(fields); i++ {
			v, err := strconv.ParseUint(fields[i+1], 10, 64)
			if err != nil {
				return cpuSample{}, false
			}
			values[i] = v
		}
		idle := values[3] + values[4]
		total := values[0] + values[1] + values[2] + values[3] + values[4] + values[5] + values[6] + values[7]
		if total == 0 || idle > total {
			return cpuSample{}, false
		}
		return cpuSample{Busy: total - idle, Total: total}, true
	}
	return cpuSample{}, false
}

// processCPUStart 进程启动时刻，供非 Linux 平台把进程 CPU 时间换算成占用率。
var processCPUStart = time.Now()

// readProcessCPUSample 用 runtime/metrics 的进程 CPU 累计时间作为 busy，
// 以「墙钟 × NumCPU」作为 total：这样跨平台可得 0–100% 的进程 CPU 占用，
// 不依赖 /proc 或第三方库。首次采样时 total 可能接近 0，返回 false 跳过。
func readProcessCPUSample() (cpuSample, bool) {
	samples := []metrics.Sample{{Name: "/cpu/classes/total:cpu-seconds"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindFloat64 {
		return cpuSample{}, false
	}
	busySeconds := samples[0].Value.Float64()
	totalSeconds := time.Since(processCPUStart).Seconds() * float64(runtime.NumCPU())
	if totalSeconds <= 0 || busySeconds < 0 {
		return cpuSample{}, false
	}
	return cpuSample{
		Busy:  uint64(busySeconds * 1e9),
		Total: uint64(totalSeconds * 1e9),
	}, true
}
