package config

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// P0-B 的缓存 TTL 默认值与 env 覆盖：用子进程重新初始化包变量，避免只改全局变量。
func TestCaptchaConfigCacheTTLDefault(t *testing.T) {
	if CaptchaConfigCacheTTL != 3_600_000 {
		t.Fatalf("CAPTCHA_CONFIG_CACHE_TTL 默认应为 1 小时（3600000ms），实际 %d", CaptchaConfigCacheTTL)
	}
	if CaptchaPoolEnabled != true {
		t.Fatal("ZCODE_CAPTCHA_POOL_ENABLED 默认应开启")
	}
	if CaptchaPoolMin < 1 || CaptchaPoolMax < CaptchaPoolMin {
		t.Fatalf("token 池 min/max 默认值不合法: %d/%d", CaptchaPoolMin, CaptchaPoolMax)
	}
}

func TestCaptchaEnvOverrides(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCaptchaEnvHelper$", "-test.count=1")
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(name) {
		case "CAPTCHA_CONFIG_CACHE_TTL", "ZCODE_CAPTCHA_POOL_TTL_MS", "ZCODE_CAPTCHA_POOL_MIN",
			"ZCODE_CAPTCHA_POOL_MAX", "ZCODE_CAPTCHA_BROWSER_WORKERS", "ZCODE_CAPTCHA_POOL_ENABLED":
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env,
		"ZCODE_TEST_CAPTCHA_ENV_HELPER=1",
		"CAPTCHA_CONFIG_CACHE_TTL=123456",
		"ZCODE_CAPTCHA_POOL_TTL_MS=42000",
		"ZCODE_CAPTCHA_POOL_MIN=3",
		"ZCODE_CAPTCHA_POOL_MAX=9",
		"ZCODE_CAPTCHA_BROWSER_WORKERS=4",
		"ZCODE_CAPTCHA_POOL_ENABLED=false",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("读取独立 captcha 配置快照失败：%v\n%s", err, output)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok && strings.HasPrefix(key, "CAPTCHA_SNAPSHOT_") {
			got[strings.TrimPrefix(key, "CAPTCHA_SNAPSHOT_")] = value
		}
	}
	want := map[string]string{
		"config_cache_ttl": "123456",
		"pool_ttl":         "42000",
		"pool_min":         "3",
		"pool_max":         "9",
		"browser_workers":  "4",
		"pool_enabled":     "false",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s 应为 %s，实际 %q", key, value, got[key])
		}
	}
}

// 只在子进程运行：输出当前包初始化后的 captcha 配置快照。
func TestCaptchaEnvHelper(t *testing.T) {
	if os.Getenv("ZCODE_TEST_CAPTCHA_ENV_HELPER") != "1" {
		return
	}
	values := map[string]string{
		"config_cache_ttl": strconv.FormatInt(CaptchaConfigCacheTTL, 10),
		"pool_ttl":         strconv.FormatInt(CaptchaPoolTTL, 10),
		"pool_min":         strconv.Itoa(CaptchaPoolMin),
		"pool_max":         strconv.Itoa(CaptchaPoolMax),
		"browser_workers":  strconv.Itoa(CaptchaBrowserWorkers),
		"pool_enabled":     strconv.FormatBool(CaptchaPoolEnabled),
	}
	// key 固定排序，避免 map 迭代顺序导致测试输出不稳定。
	for _, key := range []string{"config_cache_ttl", "pool_ttl", "pool_min", "pool_max", "browser_workers", "pool_enabled"} {
		os.Stdout.WriteString("CAPTCHA_SNAPSHOT_" + key + "=" + values[key] + "\n")
	}
}
