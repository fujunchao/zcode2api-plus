// browserdl 单测：httptest 假下载源 + 临时生成的 Ed25519 密钥签发 SHA256SUMS，
// 全程离线验证下载 → 验签 → 哈希 → 解包链路；篡改用例确保校验不被静默绕过。
package captcha

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCloakSource 假下载源：内置私钥签发的 SHA256SUMS 与压缩包。
type fakeCloakSource struct {
	srv      *httptest.Server
	sums     []byte
	sig      []byte
	archive  []byte
	archiveN string
}

func newFakeSource(t *testing.T, files map[string]string) *fakeCloakSource {
	t.Helper()
	// 打包假 tar.gz（根目录放 files）。
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()

	// 临时 Ed25519 密钥签发 SHA256SUMS（覆盖包级公钥变量）。
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fakePubKey := base64.StdEncoding.EncodeToString(pub)
	oldPub := cloakSigningPubkey
	cloakSigningPubkey = &fakePubKey
	t.Cleanup(func() { cloakSigningPubkey = oldPub })

	sumLines := hex.EncodeToString(func() []byte { s := sha256.Sum256(archive); return s[:] }()) + "  " + fakeArchiveName + "\n"
	sums := []byte(sumLines)
	// 真实 .sig 形态：base64 编码的签名文本。
	sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums)) + "\n")

	s := &fakeCloakSource{sums: sums, sig: sig, archive: archive, archiveN: fakeArchiveName}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write(s.sums)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.sig"):
			_, _ = w.Write(s.sig)
		case strings.HasSuffix(r.URL.Path, "/"+s.archiveN):
			_, _ = w.Write(s.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

const (
	fakeArchiveName = "cloakbrowser-fake.tar.gz"
	fakeVersion     = "9.9.9.9.9"
)

func setupFakeEnv(t *testing.T, src *fakeCloakSource) {
	t.Helper()
	oldBase, oldCache := os.Getenv("CLOAKBROWSER_DOWNLOAD_URL"), os.Getenv("CLOAKBROWSER_CACHE_DIR")
	if err := os.Setenv("CLOAKBROWSER_DOWNLOAD_URL", src.srv.URL); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	if err := os.Setenv("CLOAKBROWSER_CACHE_DIR", cache); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Setenv("CLOAKBROWSER_DOWNLOAD_URL", oldBase)
		_ = os.Setenv("CLOAKBROWSER_CACHE_DIR", oldCache)
	})
}

func forcePlatform(t *testing.T) {
	// 单测只验证下载链路本身，不依赖真实平台 tag 与版本常量。
	t.Helper()
}

func TestEnsureBrowserBinaryDownloadsAndExtracts(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{executableName(): "#!/bin/sh\necho fake\n", "lib.so": "lib"})
	setupFakeEnv(t, src)

	// 直接驱动内部链路（与平台解耦：手工指定版本与包名）。
	dir := cloakBinaryDir(fakeVersion)
	version, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName)
	if err != nil {
		t.Fatalf("下载链路应成功: %v", err)
	}
	if version != fakeVersion {
		t.Fatalf("版本不符: %s", version)
	}
	bin := filepath.Join(dir, executableName())
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() {
		t.Fatalf("chrome 应已安装: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatal("chrome 应有可执行位")
	}
	data, _ := os.ReadFile(bin)
	if !strings.Contains(string(data), "echo fake") {
		t.Fatalf("内容不符: %q", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib.so")); err != nil {
		t.Fatalf("附属文件应解包: %v", err)
	}

	// 幂等：第二次直接命中本地，不发请求。
	if _, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName); err != nil {
		t.Fatalf("二次调用应直接返回: %v", err)
	}
}

func TestEnsureRejectsTamperedArchive(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{"chrome": "clean"})
	setupFakeEnv(t, src)
	// 篡改压缩包内容（SHA256SUMS 不再匹配）。
	src.archive = append(src.archive, byte(0x00))

	if _, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName); err == nil {
		t.Fatal("哈希不符应失败")
	} else if !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("应报哈希错误: %v", err)
	}
}

func TestEnsureRejectsBadSignature(t *testing.T) {
	forcePlatform(t)
	src := newFakeSource(t, map[string]string{"chrome": "clean"})
	setupFakeEnv(t, src)
	// 伪造签名（另一把密钥）。
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	src.sig = ed25519.Sign(other, src.sums)

	if _, err := ensureVersion(context.Background(), fakeVersion, fakeArchiveName); err == nil {
		t.Fatal("签名校验失败应报错")
	} else if !strings.Contains(err.Error(), "签名") {
		t.Fatalf("应报签名错误: %v", err)
	}
	// 校验失败不得落盘半成品。
	if _, err := os.Stat(cloakBinaryDir(fakeVersion)); !os.IsNotExist(err) {
		t.Fatal("失败时不应安装目录")
	}
}

// macOS 的 Chromium.app bundle 以符号链接组织 Framework：
// 解包必须保留 symlink，否则解出的安装不可用。
func TestExtractTarGzPreservesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 未开启开发者模式时，普通进程没有创建符号链接的权限。
		// 先独立探测环境能力；实际解包失败仍由下方断言捕获，Linux CI 不跳过。
		probe := filepath.Join(t.TempDir(), "symlink-probe")
		if err := os.Symlink("target", probe); err != nil {
			t.Skipf("当前 Windows 环境不能创建符号链接: %v", err)
		}
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries := []struct {
		hdr  *tar.Header
		body string
	}{
		{&tar.Header{Name: "Chromium.app/Contents/MacOS/", Typeflag: tar.TypeDir, Mode: 0o755}, ""},
		{&tar.Header{Name: "Chromium.app/Contents/MacOS/Chromium", Mode: 0o755, Size: 4}, "bin\n"},
		{&tar.Header{Name: "Chromium.app/Contents/MacOS/Current", Typeflag: tar.TypeSymlink, Linkname: "Chromium", Mode: 0o777}, ""},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTarGz(buf.Bytes(), dest); err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	link := filepath.Join(dest, "Chromium.app", "Contents", "MacOS", "Current")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("符号链接应存在: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("应保留为符号链接而非普通文件: %v", info.Mode())
	}
	if target, _ := os.Readlink(link); target != "Chromium" {
		t.Fatalf("链接目标不符: %q", target)
	}
}

// 逃逸解包目录的符号链接必须被拒绝（压缩包投毒防护）。
func TestExtractTarGzRejectsEscapingSymlink(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd", Mode: 0o777}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("逃逸目录的符号链接应被拒绝")
	}
}

// safeArchiveName 的边界：只有「清理后仍落在解包目录内」的相对路径才放行。
// 单独测这个纯函数，是为了覆盖那些「不以 .. 开头、清理后才逃逸」的形态。
func TestSafeArchiveName(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		raw    string
		escape bool
	}{
		{"lib.so", false},
		{"sub/lib.so", false},
		{"./lib.so", false},
		{"a/b/../c", false}, // 清理后是 a/c，仍在目录内
		{"..", true},
		{"../secret", true},
		{"../../etc/passwd", true},
		{"a/../../secret", true}, // 不以 .. 开头，但清理后逃逸
		{"a/b/../../../x", true},
		{"/etc/passwd", true},
		{".." + sep + "secret", true},
	}
	for _, c := range cases {
		err := safeArchiveName(c.raw)
		if c.escape && err == nil {
			t.Errorf("name=%q 应被拒绝", c.raw)
		}
		if !c.escape && err != nil {
			t.Errorf("name=%q 应被接受: %v", c.raw, err)
		}
	}
}

// tarGzWithHardlink 打包一个「lib.so + 指向 linkTarget 的硬链接」压缩包。
func tarGzWithHardlink(t *testing.T, linkTarget string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "lib.so", Mode: 0o644, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("lib")); err != nil {
		t.Fatal(err)
	}
	hdr := &tar.Header{Name: "linked", Typeflag: tar.TypeLink, Linkname: linkTarget, Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 硬链接的源路径同样必须校验：本分支会读取 dest 之外的文件并复制进来，
// 未校验的 Linkname 等于把容器内任意文件的内容搬进解包目录。
func TestExtractTarGzRejectsEscapingHardlink(t *testing.T) {
	for _, target := range []string{"../../etc/passwd", "a/../../secret", "/etc/passwd"} {
		if err := extractTarGz(tarGzWithHardlink(t, target), t.TempDir()); err == nil {
			t.Fatalf("逃逸硬链接 %q 应被拒绝", target)
		}
	}
}

// 合法的包内硬链接仍应照常解出（校验不能把正常安装包也挡掉）。
func TestExtractTarGzCopiesLegitHardlink(t *testing.T) {
	dest := t.TempDir()
	if err := extractTarGz(tarGzWithHardlink(t, "lib.so"), dest); err != nil {
		t.Fatalf("包内硬链接应可解包: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "linked"))
	if err != nil {
		t.Fatalf("硬链接应已复制为普通文件: %v", err)
	}
	if string(data) != "lib" {
		t.Fatalf("内容不符: %q", data)
	}
}
