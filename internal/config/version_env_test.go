package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 用已编译的测试进程重新初始化配置，检验真实环境覆盖，而不是只改包级变量。
func TestClientVersionEnvironmentOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, version, userAgent, wantVersion, wantUserAgent string
	}{
		{"defaults", "", "", "3.15.3", "ZCode/3.15.3"},
		{"version_override", " 3.14.5 ", "", "3.14.5", "ZCode/3.14.5"},
		{"user_agent_override", "", "synthetic-custom-ua", "3.15.3", "synthetic-custom-ua"},
		{"both_overrides", "3.14.5", "synthetic-custom-ua", "3.14.5", "synthetic-custom-ua"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClientVersionEnvironmentHelper$", "-test.count=1")
			for _, value := range os.Environ() {
				name, _, _ := strings.Cut(value, "=")
				switch strings.ToUpper(name) {
				case "ZCODE_CLIENT_VERSION", "UPSTREAM_USER_AGENT", "ZCODE_CLIENT_AISDK_VERSION",
					"ZCODE_CLIENT_NODE_MAJOR", "ZCODE_DATA_DIR", "ZCODE_TEST_CLIENT_VERSION_HELPER":
					continue
				}
				cmd.Env = append(cmd.Env, value)
			}
			cmd.Env = append(cmd.Env,
				"ZCODE_TEST_CLIENT_VERSION_HELPER=1",
				"ZCODE_DATA_DIR="+t.TempDir(),
				"ZCODE_CLIENT_VERSION="+tc.version,
				"UPSTREAM_USER_AGENT="+tc.userAgent)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("读取独立配置快照失败：%v\n%s", err, output)
			}
			var snapshot map[string]string
			for _, line := range strings.Split(string(output), "\n") {
				if encoded, ok := strings.CutPrefix(line, "CLIENT_VERSION_SNAPSHOT="); ok {
					if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := map[string]string{
				"version":           tc.wantVersion,
				"source_version":    tc.wantVersion,
				"user_agent":        tc.wantUserAgent,
				"source_user_agent": tc.wantUserAgent,
				"model_user_agent":  "ZCode/" + tc.wantVersion + " ai-sdk/provider-utils/4.0.27 runtime/node.js/24",
			}
			for name, value := range want {
				if snapshot[name] != value {
					t.Errorf("%s 应为 %q，实际 %q", name, value, snapshot[name])
				}
			}
		})
	}
}

// 只输出无凭据的版本信息；子进程数据路径始终由父用例显式隔离。
func TestClientVersionEnvironmentHelper(t *testing.T) {
	if os.Getenv("ZCODE_TEST_CLIENT_VERSION_HELPER") != "1" {
		return
	}
	headers := ZcodeSourceHeaders()
	output, err := json.Marshal(map[string]string{
		"version":           ZcodeClientVersion,
		"source_version":    headers["X-ZCode-App-Version"],
		"user_agent":        UserAgent,
		"source_user_agent": headers["User-Agent"],
		"model_user_agent":  ZcodeClientProfile().ModelUserAgent(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("CLIENT_VERSION_SNAPSHOT=%s\n", output)
}
