package proxy

import (
	"testing"
	"time"
)

func TestCanonicalRouteAndForgetAllVariants(t *testing.T) {
	base := "http://test-user:test-password@127.0.0.1:18081"
	alias := base + "/not-transmitted?unused=value#fragment"
	first, err := TransportForTimeout(base, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	same, err := TransportForTimeout(alias, time.Second)
	if err != nil || same != first {
		t.Fatalf("同实际路由必须共享缓存：%v", err)
	}
	second, err := TransportForTimeout(base, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ForgetTransport(alias)
	again, err := TransportForTimeout(base, time.Second)
	if err != nil || again == first {
		t.Fatalf("退役别名必须移除原缓存：%v", err)
	}
	other, err := TransportForTimeout(base, 2*time.Second)
	if err != nil || other == second {
		t.Fatalf("退役必须清除全部超时变体：%v", err)
	}
	ForgetTransport(base)
}

func TestCanonicalRouteKeepsActualAuthenticationPartition(t *testing.T) {
	for _, pair := range [][2]string{
		{"http://one:secret@example.test", "http://two:secret@example.test"},
		{"http://one:a@example.test", "http://one:b@example.test"},
		{"socks5://one:a@example.test", "socks5://one:b@example.test"},
	} {
		left, _ := CanonicalRoute(pair[0])
		right, _ := CanonicalRoute(pair[1])
		if left == right {
			t.Fatal("不同认证不能合并")
		}
	}
	for _, pair := range [][2]string{
		{"http://one@example.test", "http://one:@EXAMPLE.TEST.:80/"},
		{"socks4://one:a@example.test", "socks4://one:b@example.test"},
		{"socks5://:unused@example.test", "socks5://example.test:1080"},
	} {
		left, _ := CanonicalRoute(pair[0])
		right, _ := CanonicalRoute(pair[1])
		if left != right {
			t.Fatal("实际未参与握手的认证字段不能绕过身份归一")
		}
	}
}
