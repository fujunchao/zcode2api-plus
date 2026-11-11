package captcha

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"zcode2api/internal/config"
)

// setCaptchaServer 将配置端点指向测试服务器，避免测试触网。
func setCaptchaServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	old := configURL
	configURL = srv.URL
	t.Cleanup(func() {
		configURL = old
		srv.Close()
	})
}

func TestFetchConfigFromUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, configuredPlatform, queryPlatform string
	}{
		{"windows-x64", "win32-x64", "windows-x86_64"},
		{"windows-arm64", "win32-arm64", "windows-aarch64"},
		{"macos-override", "darwin-arm64", "darwin-aarch64"},
		{"already-normalized", "windows-x86_64", "windows-x86_64"},
		{"custom-override", "custom-platform", "custom-platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldPlatform := config.ZcodeClientPlatform
			config.ZcodeClientPlatform = tc.configuredPlatform
			t.Cleanup(func() { config.ZcodeClientPlatform = oldPlatform })
			setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
				// 查询协议不是 X-Platform 的表示；期望值来自已核实的客户端协议。
				if r.URL.Query().Get("app_version") != config.ZcodeClientVersion || r.URL.Query().Get("platform") != tc.queryPlatform {
					t.Errorf("配置查询 platform 应为 %q，实际参数: %v", tc.queryPlatform, r.URL.Query())
				}
				_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`))
			})
			m := NewManager()
			cfg := m.FetchConfig(context.Background())
			if !cfg.Enabled || cfg.Prefix != "px" || cfg.Region != "cn" || cfg.SceneID != "sc" {
				t.Fatalf("配置解析不符: %+v", cfg)
			}

			// 二次读取保留原有的配置缓存语义。
			if cfg2 := m.FetchConfig(context.Background()); cfg2 != cfg {
				t.Fatalf("应命中配置缓存: %+v", cfg2)
			}
		})
	}
}

func TestFetchConfigModelSkipFlag(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc","skip_model_request":true}}}}`))
	})
	cfg := NewManager().FetchConfig(context.Background())
	if !cfg.Enabled || !cfg.SkipModelRequest || cfg.SceneID != "sc" {
		t.Fatalf("模型跳过标记应保留，且不能关闭全局验证码: %+v", cfg)
	}
}

func TestFetchConfigMissingCaptchaFallsBack(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{}}}`))
	})
	m := NewManager()
	if cfg := m.FetchConfig(context.Background()); cfg != DefaultConfig {
		t.Fatalf("缺少 captcha 对象应回退默认: %+v", cfg)
	}
}

func TestFetchConfigServerErrorFallsBack(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	if cfg := m.FetchConfig(context.Background()); cfg != DefaultConfig {
		t.Fatalf("上游失败应回退默认: %+v", cfg)
	}
}

func TestGetVerifyParamDisabledConfig(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"configs":{"captcha":{"enabled":false}}}}`))
	})
	m := NewManager()
	tok, err := m.GetVerifyParam(context.Background())
	if tok != nil || err != nil {
		t.Fatalf("禁用配置应返回 (nil, nil): %v %v", tok, err)
	}
}

func TestNoSolverUnavailable(t *testing.T) {
	// 上游 500 → 默认配置（enabled），浏览器默认关闭 → 无求解器可用
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	if _, err := m.GetVerifyParam(context.Background()); err != ErrUnavailable {
		t.Fatalf("应返回 ErrUnavailable: %v", err)
	}
}

func TestManualTokenCacheAndExpiry(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })

	if err := m.SetManualParam(" manual-token ", ""); err != nil {
		t.Fatal(err)
	}
	tok, err := m.GetVerifyParam(context.Background())
	if err != nil || tok == nil || tok.VerifyParam != "manual-token" {
		t.Fatalf("人工令牌应命中（含去空白）: %+v %v", tok, err)
	}
	if tok.Region != DefaultConfig.Region {
		t.Fatalf("region 缺省应取默认配置: %q", tok.Region)
	}

	// TTL 过期后走求解路径 → 浏览器关闭 → ErrUnavailable
	now = now.Add(time.Duration(config.CaptchaManualCacheTTL+1000) * time.Millisecond)
	if _, err := m.GetVerifyParam(context.Background()); err != ErrUnavailable {
		t.Fatalf("过期后应重新求解并返回 ErrUnavailable: %v", err)
	}
}

func TestInvalidateClearsManualToken(t *testing.T) {
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })
	if err := m.SetManualParam("t", "cn"); err != nil {
		t.Fatal(err)
	}
	m.Invalidate()
	if _, err := m.GetVerifyParam(context.Background()); err != ErrUnavailable {
		t.Fatalf("失效后应重新求解: %v", err)
	}
}

func TestSetManualParamEmptyRejected(t *testing.T) {
	m := NewManager()
	if err := m.SetManualParam("  ", ""); err == nil {
		t.Fatal("空令牌应报错")
	}
}

func TestFetchConfigFailureKeepsLastGood(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	body := atomic.Value{}
	body.Store(`{"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-a","region":"cn","sceneId":"sc-a"}}}}`)
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if status.Load() != http.StatusOK {
			w.WriteHeader(int(status.Load()))
			return
		}
		_, _ = w.Write([]byte(body.Load().(string)))
	})
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })

	first := m.FetchConfig(context.Background())
	if first.Prefix != "px-a" || first.SceneID != "sc-a" {
		t.Fatalf("首次成功配置不符: %+v", first)
	}

	// 缓存过期后配置接口故障：必须继续返回 last-good，不能回退 DefaultConfig。
	status.Store(http.StatusInternalServerError)
	now = now.Add(time.Duration(config.CaptchaConfigCacheTTL+1000) * time.Millisecond)
	got := m.FetchConfig(context.Background())
	if got != first {
		t.Fatalf("配置故障时应保留 last-good: %+v", got)
	}
	if got == DefaultConfig {
		t.Fatal("不得用 DefaultConfig 覆盖 last-good")
	}

	// 退避窗口内不应反复打故障接口；窗口过后成功配置可替换 last-good。
	body.Store(`{"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-b","region":"sg","sceneId":"sc-b"}}}}`)
	status.Store(http.StatusOK)
	now = now.Add(31 * time.Second)
	next := m.FetchConfig(context.Background())
	if next.Prefix != "px-b" || next.Region != "sg" {
		t.Fatalf("恢复后应更新 last-good: %+v", next)
	}
}

func TestModelCaptchaPolicyContinuesWithLastGood(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
		if status.Load() != http.StatusOK {
			w.WriteHeader(int(status.Load()))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc","skip_model_request":true}}}}`))
	})
	setBrowserEnabled(t, false)
	m := NewManager()
	now := time.Unix(1700000000, 0)
	m.SetNow(func() time.Time { return now })

	if token, err := m.GetModelVerifyParam(nil); err != nil || token != nil {
		t.Fatalf("首次应识别模型跳过: %+v %v", token, err)
	}
	status.Store(http.StatusInternalServerError)
	now = now.Add(time.Duration(config.CaptchaConfigCacheTTL+1000) * time.Millisecond)
	if token, err := m.GetModelVerifyParam(nil); err != nil || token != nil {
		t.Fatalf("配置故障时应沿用 last-good 的 skip_model_request: %+v %v", token, err)
	}
}

// TestFetchConfigRejectsInvalidPayload 覆盖 3.18.0 readValidatedRequestVerificationConfig
// 的严格语义：业务 envelope 失败、captcha 缺失/null/非对象、显式 null 与非布尔
// enabled、启用态缺失或纯空白的 prefix/region/sceneId、非布尔 skip_model_request
// 都必须返回错误；无 last-good 时 FetchConfig 仍回退 DefaultConfig。
func TestFetchConfigRejectsInvalidPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			"业务 code 非零",
			`{"code":40001,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"业务 code 显式 null",
			`{"code":null,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"业务 code 非数字",
			`{"code":"0","data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"业务 success=false",
			`{"success":false,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"缺少 captcha 对象",
			`{"code":0,"data":{"configs":{}}}`,
		},
		{
			"captcha 显式 null",
			`{"code":0,"data":{"configs":{"captcha":null}}}`,
		},
		{
			"captcha 非对象",
			`{"code":0,"data":{"configs":{"captcha":"on"}}}`,
		},
		{
			"缺少 data",
			`{"code":0}`,
		},
		{
			"enabled 显式 null",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":null,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"enabled 字符串",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":"true","prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"enabled 数字",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":1,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"prefix 缺失",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"prefix 显式 null",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":null,"region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"prefix 纯空白",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"   ","region":"cn","sceneId":"sc"}}}}`,
		},
		{
			"region 纯空白",
			// JSON 转义 \t 与 \n 解码后即为制表符与换行，属于纯空白。
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"\t\n ","sceneId":"sc"}}}}`,
		},
		{
			"sceneId 数值",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":11}}}}`,
		},
		{
			"skip_model_request 显式 null",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc","skip_model_request":null}}}}`,
		},
		{
			"skip_model_request 字符串",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc","skip_model_request":"true"}}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			if _, err := fetchConfigHTTP(context.Background()); err == nil {
				t.Fatal("残缺或失败的业务响应必须返回错误")
			}
			if cfg := NewManager().FetchConfig(context.Background()); cfg != DefaultConfig {
				t.Fatalf("无 last-good 时应回退 DefaultConfig: %+v", cfg)
			}
		})
	}
}

// TestFetchConfigAcceptsValidPayload 确认严格校验不误伤合法配置：
// 完整启用配置、成功态新字段（success=true）、合法的 disabled 最小配置，
// 以及缺失/显式 false 的 skip_model_request。
func TestFetchConfigAcceptsValidPayload(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Config
	}{
		{
			"完整启用配置",
			`{"code":0,"success":true,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
			Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc"},
		},
		{
			"字段去首尾空白",
			`{"data":{"configs":{"captcha":{"enabled":true,"prefix":" px ","region":"\tcn\n","sceneId":" sc "}}}}`,
			Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc"},
		},
		{
			"合法 disabled 最小配置",
			`{"code":0,"data":{"configs":{"captcha":{"enabled":false}}}}`,
			Config{},
		},
		{
			"disabled 允许空字段",
			`{"data":{"configs":{"captcha":{"enabled":false,"prefix":"","region":"  ","sceneId":null,"skip_model_request":false}}}}`,
			Config{},
		},
		{
			"enabled 缺失按启用处理",
			`{"data":{"configs":{"captcha":{"prefix":"px","region":"cn","sceneId":"sc"}}}}`,
			Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc"},
		},
		{
			"skip=true 允许无三字段",
			`{"data":{"configs":{"captcha":{"skip_model_request":true}}}}`,
			Config{Enabled: true, SkipModelRequest: true},
		},
		{
			"skip_model_request 为 true",
			`{"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc","skip_model_request":true}}}}`,
			Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc", SkipModelRequest: true},
		},
		{
			"skip_model_request 为 false",
			`{"data":{"configs":{"captcha":{"enabled":true,"prefix":"px","region":"cn","sceneId":"sc","skip_model_request":false}}}}`,
			Config{Enabled: true, Prefix: "px", Region: "cn", SceneID: "sc"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			cfg, err := fetchConfigHTTP(context.Background())
			if err != nil {
				t.Fatalf("合法配置不应报错: %v", err)
			}
			if cfg != tc.want {
				t.Fatalf("配置解析不符: %+v，期望 %+v", cfg, tc.want)
			}
			// 合法配置必须作为成功值写入 last-good，而不是回退默认值。
			if got := NewManager().FetchConfig(context.Background()); got != tc.want {
				t.Fatalf("FetchConfig 应采用上游配置: %+v，期望 %+v", got, tc.want)
			}
		})
	}
}

// TestFetchConfigInvalidPayloadKeepsLastGood 确认残缺配置不会污染 last-good：
// 首次成功配置之后，业务失败/显式 null/空白字段等残缺响应只能走 last-good，
// 30 秒退避窗口仍然生效；退避结束且上游恢复后，有效配置才能替换 last-good。
func TestFetchConfigInvalidPayloadKeepsLastGood(t *testing.T) {
	const validFirst = `{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-a","region":"cn","sceneId":"sc-a"}}}}`
	const validNext = `{"code":0,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-b","region":"sg","sceneId":"sc-b"}}}}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"业务 code 非零", `{"code":40001,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-x","region":"cn","sceneId":"sc-x"}}}}`},
		{"业务 success=false", `{"success":false,"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-x","region":"cn","sceneId":"sc-x"}}}}`},
		{"enabled 显式 null", `{"data":{"configs":{"captcha":{"enabled":null,"prefix":"px-x","region":"cn","sceneId":"sc-x"}}}}`},
		{"sceneId 纯空白", `{"data":{"configs":{"captcha":{"enabled":true,"prefix":"px-x","region":"cn","sceneId":"  "}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			body.Store(validFirst)
			var fetches atomic.Int32
			setCaptchaServer(t, func(w http.ResponseWriter, r *http.Request) {
				fetches.Add(1)
				_, _ = w.Write([]byte(body.Load().(string)))
			})
			m := NewManager()
			now := time.Unix(1700000000, 0)
			m.SetNow(func() time.Time { return now })

			first := m.FetchConfig(context.Background())
			if first.Prefix != "px-a" || first.SceneID != "sc-a" {
				t.Fatalf("首次成功配置不符: %+v", first)
			}
			body.Store(tc.body)
			now = now.Add(time.Duration(config.CaptchaConfigCacheTTL+1000) * time.Millisecond)
			if got := m.FetchConfig(context.Background()); got != first {
				t.Fatalf("残缺配置不得覆盖 last-good: %+v", got)
			}
			if fetches.Load() != 2 {
				t.Fatalf("缓存过期后应重试一次配置: %d", fetches.Load())
			}
			// 失败后仍有 30 秒退避：窗口内继续 last-good，不重复打故障接口。
			now = now.Add(5 * time.Second)
			if got := m.FetchConfig(context.Background()); got != first {
				t.Fatalf("退避窗口内应继续使用 last-good: %+v", got)
			}
			if fetches.Load() != 2 {
				t.Fatalf("退避窗口内不应重试配置: %d", fetches.Load())
			}
			// 退避结束后有效配置仍可正常替换 last-good。
			body.Store(validNext)
			now = now.Add(26 * time.Second)
			next := m.FetchConfig(context.Background())
			if next.Prefix != "px-b" || next.Region != "sg" || next.SceneID != "sc-b" {
				t.Fatalf("恢复后应替换 last-good: %+v", next)
			}
			if fetches.Load() != 3 {
				t.Fatalf("退避结束后应重试一次配置: %d", fetches.Load())
			}
		})
	}
}
