package gateway

import (
	"strings"
	"testing"
)

// 会话引用必须是稳定哈希：同一会话跨请求一致（可按会话分组），不同会话不碰撞，
// 空值不落占位符。用按时间轮换的 HMAC 会让跨小时的同一会话裂成多个组，故不采用。
func TestSessionRefStableAndDistinct(t *testing.T) {
	a := SessionRef("session-a")
	b := SessionRef("session-a")
	c := SessionRef("session-b")
	if a != b {
		t.Fatalf("同一会话应得到相同引用: %q vs %q", a, b)
	}
	if len(a) != 12 {
		t.Fatalf("引用应为 12 位十六进制: %q", a)
	}
	if a == c {
		t.Fatalf("不同会话不应碰撞: %q", a)
	}
	if got := SessionRef(""); got != "" {
		t.Fatalf("空会话应返回空串，实际 %q", got)
	}
	if strings.Contains(a, "session-a") {
		t.Fatal("引用里不得出现会话原值")
	}
}

// 诊断行必须带 session= 字段（粘性实验的取数口径）。
func TestDiagFormatIncludesSession(t *testing.T) {
	d := NewReqDiag("req-session", "GLM-5.3", false, msgBody(), nil)
	d.Session = SessionRef("session-a")
	line := d.Format()
	if got := diagField(t, line, "session"); got != d.Session {
		t.Fatalf("session 字段不符: %q\n行=%s", got, line)
	}
}
