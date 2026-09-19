// zcode2api 服务入口：API 网关 + 后台管理 API + SPA 托管。
// 对应 Python 版 app/main.py 的启动流程与横幅。
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zcode2api" // 嵌入的前端构建产物（仓库根包，受 go:embed 目录约束）

	"zcode2api/internal/adminapi"
	"zcode2api/internal/asyncpool"
	"zcode2api/internal/auth"
	"zcode2api/internal/captcha"
	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/openai"
	"zcode2api/internal/quota"
	"zcode2api/internal/store"
	"zcode2api/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		os.Exit(runCLI(os.Args[1], os.Args[2:], serve))
	}
	// serve 内部靠 defer 收尾，所以等它返回之后再决定退出码：
	// 在其内部 os.Exit 会跳过全部 defer（存储、验证码池、额度监控、领取调度器）。
	if code := serve(); code != 0 {
		os.Exit(code)
	}
}

// 服务端超时与体积预算。
const (
	// readHeaderTimeout 读取请求头的上限（防御慢速头攻击）。
	readHeaderTimeout = 30 * time.Second
	// idleTimeout 空闲 keep-alive 连接的最长保持时间。
	idleTimeout = 120 * time.Second
	// gracefulShutdownTimeout 收到停机信号后等待在途请求收尾的上限。
	gracefulShutdownTimeout = 10 * time.Second
	// maxBodyBytes 请求体大小上限。
	//
	// 八个对外入口都把 r.Body 直接交给 json.NewDecoder 解进 map[string]any，
	// 解出来的内存远大于线上字节数；网关还会为每个候选账号再序列化一次。
	// 不设上限时一个超大 JSON 就能把进程撑爆，连带杀掉所有在途 SSE 串流。
	// 16 MiB：足以容纳任何合理请求（含长上下文的多模态输入），同时把
	// 「单个请求吃光内存」变成一次明确的拒绝。
	maxBodyBytes = 16 << 20
	// maxHeaderBytes 请求头体积上限：默认 1 MiB 对 Cookie/Authorization 足够，
	// 显式写小以缩小单连接可占用的内存。
	maxHeaderBytes = 64 << 10
)

// limitBody 给请求体套上大小上限。
//
// 放在服务端而非各 handler：入口有八个，逐个加容易漏，且新入口默认没有防护；
// 包在 mux 外层则新增端点自动受保护。超限时响应体不会被整段读进内存——读取
// 直接失败，handler 的解码随之报错并回 4xx。
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// newServer 构造 HTTP 服务端。
//
// 刻意不设 WriteTimeout：SSE 是长连接，写超时会把正常的长流掐断。
// 零值 http.Server 则连读头超时与空闲超时都没有，既不防御慢速攻击，
// 也让空闲连接一直占着不放。
func newServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           limitBody(handler),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

// serve 启动网关 + 后台管理 + SPA（对应 Python 版 main.py serve）。
// 返回进程退出码：0 为正常退出（含收到停机信号），1 为启动或运行失败。
func serve() int {
	st, err := store.New()
	if err != nil {
		web.Err("main", "存储初始化失败: "+err.Error())
		return 1
	}
	defer func() { _ = st.Close() }()

	mux := http.NewServeMux()
	authSvc := auth.New(st)
	cm := captcha.NewManager()
	// 浏览器池求解器（M5）：启用时注入 rod 求解，失败冷却后回退人工回填
	if config.CaptchaBrowserEnabled {
		cm.SetSolver(captcha.NewBrowserSolver())
	}
	defer func() { _ = cm.Close() }()

	// 额度查询：网关成功/耗尽路径触发刷新，后台管理端点与周期监控共用
	qs := quota.NewService(st)

	// 网关 + 后台管理 API（各自端点内建鉴权）
	engine := gateway.NewEngine(st, cm, nil)
	engine.OnQuotaRefresh = func(acc *model.Account) { _ = qs.FetchQuota(acc) }
	gw := gateway.Handler{Engine: engine, Auth: authSvc}
	gw.Register(mux)
	admin := adminapi.New(st, authSvc, cm, qs)
	admin.Register(mux)

	// OpenAI 兼容层：/v1/chat/completions 复用同一引擎（M4）
	openai.New(engine, authSvc).Register(mux)

	// Async 空闲池：与 Python 版一致按设置条件挂载
	if config.AsyncEnabled {
		asyncpool.NewPool(st, authSvc, cm).Register(mux)
	}

	// SPA 托管（/ → /admin、/assets 静态、/admin/{path...} 回落 index.html、/meta）
	web.NewSPA(distSub()).Register(mux)

	// 后台额度监控：随服务启动、退出时等待循环收尾（对齐 lifespan）
	mon := qs.NewMonitor()
	mon.Start()
	defer mon.Stop()

	// 每日定时领取调度器：实时读后台设置，开关关闭时为空转（每 30s 看一次）
	sched := adminapi.NewClaimScheduler(admin)
	sched.Start()
	defer sched.Stop()

	// 代理线路自动巡检：周期检测可用性，自动移除不可用线路并改派绑定账号
	//（全部线路不可用且直连也不可达时跳过该轮，防本机网络故障清空线路池）
	proxyHealth := adminapi.NewProxyHealthScheduler(st)
	proxyHealth.Start()
	defer proxyHealth.Stop()

	printBanner(st)

	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
	srv := newServer(addr, mux)

	// 停机信号：容器里对应 docker stop 发的 SIGTERM。收到后停止接受新连接、
	// 等待在途请求收尾，ListenAndServe 随之返回 ErrServerClosed，defer 依次执行。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		<-stop
		web.Ok("main", "收到退出信号，开始优雅停机…")
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			web.Warn("main", "优雅停机未在限时内完成: "+err.Error())
		}
	}()

	web.Ok("main", "服务运行中 "+addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		web.Err("main", "服务退出: "+err.Error())
		return 1
	}
	return 0
}

// distSub 从嵌入根提取 frontend/dist 子树；缺失时返回 nil（页面路由 404 提示）。
func distSub() fs.FS {
	sub, err := fs.Sub(zcode2api.DistFS, "frontend/dist")
	if err != nil {
		return nil
	}
	return sub
}

// printBanner 打印启动横幅；密钥由引导逻辑生成时必须在此交付给管理者，
// 否则无法登录／调用（对齐 Python main.py，原文措辞保留）。
func printBanner(st *store.Store) {
	base := fmt.Sprintf("http://%s:%d", displayHost(), config.Port)
	lines := []string{
		fmt.Sprintf("%szcode2api-plus%s %sv%s · Go%s",
			web.Bold+web.Magenta, web.Reset, web.Dim, config.AppVersion, web.Reset),
		fmt.Sprintf("%s后台管理%s  %s%s/admin/login%s", web.Dim, web.Reset, web.Cyan, base, web.Reset),
		fmt.Sprintf("%s对话端点%s  %s%s/v1/messages%s", web.Dim, web.Reset, web.Cyan, base, web.Reset),
	}
	if st.GeneratedAdminKey != "" {
		lines = append(lines, fmt.Sprintf(
			"%s初始后台密码%s  %s%s%s %s（请登录后尽快在「设置」页修改）%s",
			web.Dim, web.Reset, web.Yellow, st.GeneratedAdminKey, web.Reset, web.Dim, web.Reset))
	}
	if st.GeneratedGatewayKey != "" {
		lines = append(lines, fmt.Sprintf(
			"%s网关 API Key%s  %s%s%s %s（调用 /v1/messages 需携带，可在「设置」页修改）%s",
			web.Dim, web.Reset, web.Yellow, st.GeneratedGatewayKey, web.Reset, web.Dim, web.Reset))
	}
	web.Banner(lines...)
}

// displayHost 横幅展示用主机：通配地址在浏览器中不可直接访问，显示 127.0.0.1
// （对齐 Python _display_host）。
func displayHost() string {
	switch config.Host {
	case "", "0.0.0.0", "::":
		return "127.0.0.1"
	}
	return config.Host
}
