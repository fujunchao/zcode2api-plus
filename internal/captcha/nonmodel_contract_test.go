package captcha

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"zcode2api/internal/config"
)

func TestPublicCaptchaConfigHTTPContract(t *testing.T) {
	var calls atomic.Int32
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Query().Get("app_version") != config.ZcodeClientVersion || r.URL.Query().Get("platform") != config.ZcodeQueryPlatform() {
			t.Error("公开配置请求的参数必须取统一档案")
		}
		for key, value := range config.ZcodeSourceHeaders() {
			if r.Header.Get(key) != value {
				t.Errorf("公开同源配置缺少来源头 %s", key)
			}
		}
		for _, key := range []string{"Authorization", "x-api-key", "X-Coding-Plan-Api-Key", "Cookie", "X-ZCode-Agent", "X-Zcode-Trace-Id"} {
			if r.Header.Get(key) != "" {
				t.Errorf("公开配置不应携带认证/模型归因 %s", key)
			}
		}
		_, _ = io.WriteString(w, `{"data":{"configs":{"captcha":{"enabled":true,"sceneId":"synthetic-scene"}}}}`)
	})
	m := NewManager()
	if cfg := m.FetchConfig(context.Background()); cfg.SceneID != "synthetic-scene" {
		t.Fatal("真实公开配置响应未进入配置缓存")
	}
	_ = m.FetchConfig(context.Background())
	if calls.Load() != 1 {
		t.Fatal("公开配置契约修复不能关闭成功缓存")
	}
}

func TestPublicCaptchaConfigDoesNotFollowRedirect(t *testing.T) {
	var followed atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed.Add(1) }))
	t.Cleanup(other.Close)
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) })
	if _, err := fetchConfigHTTP(context.Background()); err == nil || followed.Load() != 0 {
		t.Fatal("公开配置重定向不能把来源头转发到另一目标，应按原失败回退")
	}
}
