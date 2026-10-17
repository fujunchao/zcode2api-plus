package gateway

import (
	"testing"

	"zcode2api/internal/store"
)

func TestSoftOutcomeSignal(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		status int
		want   string
		wantOK bool
	}{
		{name: "成功计入分母", kind: "success", status: 200, want: "", wantOK: true},
		{name: "传输层超时", kind: "timeout", status: 0, want: store.ProxySignalTransportTimeout, wantOK: true},
		{name: "连接重置", kind: "connection_failed", status: 0, want: store.ProxySignalTransportReset, wantOK: true},
		{name: "408 代理超时", kind: "invalid_response", status: 408, want: "status_408", wantOK: true},
		{name: "504 网关超时", kind: "upstream_error", status: 504, want: "status_504", wantOK: true},
		{name: "524 边缘超时", kind: "upstream_error", status: 524, want: "status_524", wantOK: true},
		{name: "405 走风控链", kind: "risk_control", status: 405},
		{name: "429 有独立语义", kind: "rate_limited", status: 429},
		{name: "402 额度耗尽", kind: "quota_exhausted", status: 402},
		{name: "503 有冷却阶梯", kind: "upstream_error", status: 503},
		{name: "取消不算", kind: "canceled", status: 200},
		{name: "断流不算", kind: "incomplete_response", status: 200},
		{name: "非法目标不算", kind: "invalid_target", status: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := softOutcomeSignal(tc.kind, tc.status)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("softOutcomeSignal(%q,%d) = (%q,%v)，期望 (%q,%v)", tc.kind, tc.status, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
