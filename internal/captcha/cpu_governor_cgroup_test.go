// cgroup v2 CPU 采样的用例。
//
// 背景：旧实现只读 /proc/stat（整机口径），容器里跑 109% CPU 在 4 核宿主上
// 只算 27%，永远碰不到 70%/85% 阈值，governor 形同虚设。
package captcha

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// withCgroupFiles 用临时文件替换 cgroup 采样路径与起始时刻。
func withCgroupFiles(t *testing.T, statContent, maxContent string) {
	t.Helper()
	dir := t.TempDir()
	statPath := filepath.Join(dir, "cpu.stat")
	maxPath := filepath.Join(dir, "cpu.max")
	if statContent != "" {
		if err := os.WriteFile(statPath, []byte(statContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if maxContent != "" {
		if err := os.WriteFile(maxPath, []byte(maxContent), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	oldStat, oldMax, oldStart := cgroupCPUStatPath, cgroupCPUMaxPath, cgroupCPUStart
	t.Cleanup(func() {
		cgroupCPUStatPath, cgroupCPUMaxPath, cgroupCPUStart = oldStat, oldMax, oldStart
	})
	cgroupCPUStatPath = statPath
	cgroupCPUMaxPath = maxPath
	cgroupCPUStart = time.Now().Add(-10 * time.Second)
}

// TestCgroupCPUSampleUsesQuota 分母必须是 cgroup 配额，不是整机核数。
func TestCgroupCPUSampleUsesQuota(t *testing.T) {
	// 配额 2 核（200000/100000）；10 秒内用了 20 CPU 秒 = 2 核满载。
	withCgroupFiles(t, "usage_usec 20000000\nuser_usec 10000000\n", "200000 100000\n")

	sample, ok := readCgroupCPUSample()
	if !ok {
		t.Fatal("应能读到 cgroup 采样")
	}
	if sample.Busy != 20000000 {
		t.Fatalf("busy 应为 20000000，实际 %d", sample.Busy)
	}
	// total = 墙钟 10s × 2 核 = 20e6 微秒；允许采样期间的少量漂移。
	if sample.Total < 19000000 || sample.Total > 21500000 {
		t.Fatalf("total 应约为 20e6（10s × 2 核），实际 %d", sample.Total)
	}
	// 折算出来的比例应接近 100%（满载）。
	percent := 100 * float64(sample.Busy) / float64(sample.Total)
	if percent < 90 || percent > 110 {
		t.Fatalf("满载占比应约为 100%%，实际 %.1f%%", percent)
	}
}

// TestCgroupCPUQuotaFallback cpu.max 为 max 或缺失时退回 NumCPU。
func TestCgroupCPUQuotaFallback(t *testing.T) {
	want := int64(runtime.NumCPU()) * 1000

	withCgroupFiles(t, "usage_usec 1\n", "max 100000\n")
	if got := cgroupCPUQuotaMilliCores(); got != want {
		t.Fatalf("max 配额应退回 NumCPU*1000=%d，实际 %d", want, got)
	}

	withCgroupFiles(t, "usage_usec 1\n", "")
	if got := cgroupCPUQuotaMilliCores(); got != want {
		t.Fatalf("缺失 cpu.max 应退回 NumCPU*1000=%d，实际 %d", want, got)
	}
}

// TestCgroupCPUSampleMissingFile 读不到 cpu.stat 时返回 false，交由上层退回。
func TestCgroupCPUSampleMissingFile(t *testing.T) {
	withCgroupFiles(t, "", "100000 100000\n")
	if _, ok := readCgroupCPUSample(); ok {
		t.Fatal("缺少 cpu.stat 时应返回 false")
	}
}
