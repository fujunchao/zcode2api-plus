package upstream

import "testing"

// 通过实际构造入口锁定官方可选归因字段，不能放开整组未知 x-zcode 头。
func TestQuerySourcePreservesValidatedPurpose(t *testing.T) {
	req, err := BuildRequest(jwtAccount(), "", "", map[string]string{
		"x-zcode-query-source": "compact",
		"x-zcode-unknown":      "must-not-leak",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Headers["X-Zcode-Query-Source"]; got != "compact" {
		t.Fatalf("合法用途应保留，got=%q", got)
	}
	if _, ok := req.Headers["X-Zcode-Unknown"]; ok {
		t.Fatal("未知上下文不能因为保留用途而透传")
	}
}
