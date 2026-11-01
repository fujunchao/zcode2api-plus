package config

import "testing"

func TestZcodeSystemProfileFor(t *testing.T) {
	cases := []struct {
		version string
		want    ZcodeSystemProfile
	}{
		{"3.17.1", ZcodeSystemProfile3171},
		{" 3.17.1 ", ZcodeSystemProfile3171},
		{"3.16.3", ZcodeSystemProfileLegacy},
		{"3.15.3", ZcodeSystemProfileLegacy},
		{"3.17.2", ZcodeSystemProfileLegacy},
		{"", ZcodeSystemProfileLegacy},
	}
	for _, tc := range cases {
		if got := ZcodeSystemProfileFor(tc.version); got != tc.want {
			t.Fatalf("ZcodeSystemProfileFor(%q) = %q, 期望 %q", tc.version, got, tc.want)
		}
	}
}
