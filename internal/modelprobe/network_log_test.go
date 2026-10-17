package modelprobe

import (
	"fmt"
	"strings"
	"testing"
)

// 删除明细是事后复盘误删的唯一证据：只有 removed=14 计数无法核对删了哪条线。
func TestRemovedIDsFieldIsBounded(t *testing.T) {
	if got := removedIDsField(nil); got != "" {
		t.Fatalf("无删除时不应输出字段: %q", got)
	}
	if got := removedIDsField([]string{"p1"}); got != "removed_ids=p1" {
		t.Fatalf("单条 id 输出不符: %q", got)
	}
	many := make([]string, 25)
	for i := range many {
		many[i] = fmt.Sprintf("p%02d", i)
	}
	got := removedIDsField(many)
	if !strings.HasPrefix(got, "removed_ids=p00,p01,") || !strings.HasSuffix(got, "+5") {
		t.Fatalf("超过 20 条应折叠为 +N: %q", got)
	}
	if len(got) > 512 {
		t.Fatalf("单行字段应受长度限制: %d", len(got))
	}
}
