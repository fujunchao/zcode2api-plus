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

// readCPUSample 读取 CPU 累计计数，口径按优先级：
//
//  1. cgroup v2 的 cpu.stat —— 容器部署的正确口径：只统计本容器用掉的 CPU，
//     分母是 cgroup 的 CPU 配额（cpu.max），不是整机核心数。
//  2. /proc/stat —— 非容器 Linux 的整机口径。
//  3. runtime/metrics 的进程 CPU 时间 —— 其余平台。
//
// 为什么必须改：旧实现只读 /proc/stat，那是整机口径。容器里跑 109% CPU 时，
// 在 4 核宿主上只算 27%，永远碰不到 70%/85% 阈值，governor 形同虚设
// （真机实测见 findings/browser-lifecycle-audit-20261106.md §5.3）。
func readCPUSample() (cpuSample, bool) {
	if runtime.GOOS == "linux" {
		if sample, ok := readCgroupCPUSample(); ok {
			return sample, true
		}
		if sample, ok := readProcStatSample(); ok {
			return sample, true
		}
	}
	return readProcessCPUSample()
}

// cgroup v2 采样路径；测试可覆写为临时文件。
var (
	cgroupCPUStatPath = "/sys/fs/cgroup/cpu.stat"
	cgroupCPUMaxPath  = "/sys/fs/cgroup/cpu.max"
)

// cgroupCPUStart 进程启动时刻，用于把累计用量折算成「墙钟 × 配额」。
var cgroupCPUStart = time.Now()

// readCgroupCPUSample 用 cgroup v2 的累计 CPU 用量作 busy，以
// 「墙钟 × CPU 配额」作 total，得到容器口径的 0–100% 占用率。
//
// cgroup v1（无 cpu.stat）或读不到时返回 false，交由上层退回 /proc/stat。
func readCgroupCPUSample() (cpuSample, bool) {
	usage, ok := readCgroupUsageUsec()
	if !ok {
		return cpuSample{}, false
	}
	milliCores := cgroupCPUQuotaMilliCores()
	elapsed := time.Since(cgroupCPUStart).Microseconds()
	if milliCores <= 0 || elapsed <= 0 {
		return cpuSample{}, false
	}
	// total 与 busy 同为微秒量级；total = 墙钟微秒 × 配额（千分之一核）。
	total := uint64(elapsed) * uint64(milliCores) / 1000
	if total == 0 {
		return cpuSample{}, false
	}
	return cpuSample{Busy: usage, Total: total}, true
}

// readCgroupUsageUsec 读 cpu.stat 的 usage_usec（cgroup v2 累计 CPU 微秒）。
func readCgroupUsageUsec() (uint64, bool) {
	data, err := os.ReadFile(cgroupCPUStatPath)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// cgroupCPUQuotaMilliCores 读 cpu.max 的配额，返回「千分之一核」。
//
// 格式："<quota> <period>" 或 "max <period>"。max 表示不限，此时用
// runtime.NumCPU() 作为分母（等价于整机可用核数）。
func cgroupCPUQuotaMilliCores() int64 {
	fallback := int64(runtime.NumCPU()) * 1000
	data, err := os.ReadFile(cgroupCPUMaxPath)
	if err != nil {
		return fallback
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 || fields[0] == "max" {
		return fallback
	}
	quota, err1 := strconv.ParseInt(fields[0], 10, 64)
	period, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		return fallback
	}
	if cores := quota * 1000 / period; cores > 0 {
		return cores
	}
	return fallback
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
