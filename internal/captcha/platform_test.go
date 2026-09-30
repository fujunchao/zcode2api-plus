package captcha

import "testing"

func TestBrowserReleasePlatformMatrix(t *testing.T) {
	cases := []struct{ os, arch, tag, version, archive string }{
		{"linux", "amd64", "linux-x64", "146.0.7680.177.5", "cloakbrowser-linux-x64.tar.gz"},
		{"linux", "arm64", "linux-arm64", "146.0.7680.177.3", "cloakbrowser-linux-arm64.tar.gz"},
		{"darwin", "amd64", "darwin-x64", "145.0.7632.109.2", "cloakbrowser-darwin-x64.tar.gz"},
		{"darwin", "arm64", "darwin-arm64", "145.0.7632.109.2", "cloakbrowser-darwin-arm64.tar.gz"},
		{"windows", "amd64", "windows-x64", "146.0.7680.177.5", "cloakbrowser-windows-x64.zip"},
	}
	for _, c := range cases {
		t.Run(c.os+"/"+c.arch, func(t *testing.T) {
			tag, version, archive, err := browserReleaseFor(c.os, c.arch)
			if err != nil || tag != c.tag || version != c.version || archive != c.archive {
				t.Fatalf("下载选择错误：%q %q %q %v", tag, version, archive, err)
			}
		})
	}
	if _, _, _, err := browserReleaseFor("unknown", "amd64"); err == nil {
		t.Fatal("未知平台必须明确拒绝")
	}
}
