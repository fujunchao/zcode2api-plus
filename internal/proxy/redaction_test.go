package proxy

import (
	"strings"
	"testing"
)

func TestProxyInvalidURLNeverEchoesCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://synthetic-user:synthetic-password@%zz",
		"http://synthetic-user:synthetic-password@127.0.0.1:invalid-port",
		"http://synthetic-user:synthetic-password@[bad-ipv6",
	} {
		if got := MaskURL(raw); got != "invalid-proxy" {
			t.Fatalf("非法地址必须使用固定去敏占位，实际 %q", got)
		}
		_, err := NormalizeProxyURL(raw)
		if err == nil || strings.Contains(err.Error(), "synthetic-") {
			t.Fatal("地址验证失败不得回显凭据")
		}
	}
	if _, err := NormalizeProxyURL("synthetic-secret://localhost"); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("未知协议也不得回显任意输入")
	}
}
