// probe 子命令：只读账号库、挑一个账号、发**一条**真实模型请求，输出结果摘要。
//
// 为什么需要它：上游从 system 第 0 字节校验到身份段末尾，文案必须与 app_version
// 成对。换版本前必须先验证，但不能拿生产流量去试——所以做成「一个账号、一条请求、
// 打完即退」的探针（见 docs/analysis-zcode-3.17.1-405-incident-20261101.md）。
//
// 硬边界：
//   - 账号库以 SQLite **只读**模式打开（mode=ro，失败退回 immutable=1），不建表、
//     不迁移、不写任何字段；
//   - 不启动存储 / 调度 / 验证码池，不发第二条请求；
//   - 输出只含状态码、业务码、响应前 4KB 与 system 指纹，不含凭证与会话内容。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"zcode2api/internal/config"
	"zcode2api/internal/gateway"
	"zcode2api/internal/model"
	"zcode2api/internal/proxy"
	"zcode2api/internal/upstream"
)

const probeResponseLimit = 4096

type probeOptions struct {
	Model      string
	Message    string
	MaxTokens  int
	Timeout    time.Duration
	SystemFile string
}

type probeCandidate struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	LastUsed *float64 `json:"last_used_at"`
	acc      *model.Account
}

type probeReport struct {
	OK                bool   `json:"ok"`
	ClientVersion     string `json:"client_version"`
	SystemProfile     string `json:"system_profile"`
	SystemFirst       string `json:"system_first_text"`
	SystemFirstSHA    string `json:"system_first_sha16"`
	SystemIdentitySHA string `json:"system_identity_sha16"`
	SystemBlocks      int    `json:"system_blocks"`
	SystemSource      string `json:"system_source"`
	Account           string `json:"account"`
	AccountName       string `json:"account_name"`
	DeviceMidSrc      string `json:"device_mid_source"`
	Egress            string `json:"egress"`
	Model             string `json:"model"`
	Upstream          string `json:"upstream"`
	HTTPStatus        int    `json:"http_status"`
	BusinessCode      string `json:"business_code,omitempty"`
	Message           string `json:"message,omitempty"`
	BodyPrefix        string `json:"body_prefix,omitempty"`
	ElapsedMS         int64  `json:"elapsed_ms"`
	Error             string `json:"error,omitempty"`
}

func runProbe(args []string) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	var (
		dbPath     = fs.String("db", config.DBPath, "账号库路径（默认 ZCODE_DATA_DIR/accounts.db）")
		account    = fs.String("account", "", "账号 id 或 name；留空则挑最久未使用的一个")
		listOnly   = fs.Bool("list", false, "只列出候选账号，不发请求")
		clientVer  = fs.String("client-version", "", "覆盖伪装版本（默认取 ZCODE_CLIENT_VERSION）")
		modelName  = fs.String("model", "glm-5.3", "探针模型")
		message    = fs.String("message", "只回复 OK", "探针消息")
		maxTokens  = fs.Int("max-tokens", 16, "max_tokens")
		timeout    = fs.Duration("timeout", 90*time.Second, "单请求超时")
		dryRun     = fs.Bool("dry-run", false, "只打印将要发送的指纹，不发请求")
		systemFile = fs.String("system-file", "", "用自定义 JSON（{blocks:[…]}）覆盖 system，用于逐字节还原实验")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if v := strings.TrimSpace(*clientVer); v != "" {
		applyProbeClientVersion(v)
	}
	summary := upstream.ZcodeSystemSummary()
	if file := strings.TrimSpace(*systemFile); file != "" {
		blocks, loadErr := loadProbeSystemFile(file)
		if loadErr != nil {
			fmt.Fprintln(os.Stderr, "读取 system 文件失败: "+loadErr.Error())
			return 1
		}
		summary = upstream.ZcodeSystemSummaryFromBlocks(blocks, "custom")
	}

	candidates, err := loadProbeCandidates(*dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取账号库失败: "+err.Error())
		return 1
	}
	if *listOnly {
		return printProbeJSON(map[string]any{
			"client_version": config.ZcodeClientVersion,
			"system":         summary,
			"accounts":       candidates,
		}, 0)
	}
	picked, err := pickProbeCandidate(candidates, *account)
	if err != nil {
		fmt.Fprintln(os.Stderr, "挑选账号失败: "+err.Error())
		return 1
	}
	if *dryRun {
		return printProbeJSON(map[string]any{
			"dry_run":        true,
			"client_version": config.ZcodeClientVersion,
			"system":         summary,
			"account":        picked,
			"model":          *modelName,
			"message":        *message,
		}, 0)
	}
	report := probeSend(picked, summary, probeOptions{
		Model: *modelName, Message: *message, MaxTokens: *maxTokens, Timeout: *timeout,
		SystemFile: strings.TrimSpace(*systemFile),
	})
	exitCode := 1
	if report.OK {
		exitCode = 0
	}
	return printProbeJSON(report, exitCode)
}

// applyProbeClientVersion 把伪装版本切到探针指定的值。
//
// UserAgent 是包级变量、启动时按默认版本算过一次；只改 ZcodeClientVersion 会发出
// 「UA 与 X-ZCode-App-Version 各说各话」的请求，把结论搞脏，所以这里必须一起改。
func applyProbeClientVersion(version string) {
	config.ZcodeClientVersion = version
	if os.Getenv("UPSTREAM_USER_AGENT") == "" {
		config.UserAgent = "ZCode/" + version
	}
}

// loadProbeCandidates 以只读方式读出可用 JWT 账号。
//
// 先用 mode=ro 打开（生产实例同时持有 WAL 时也能读）；若环境不允许（例如只读卷上
// 无法创建 -shm），退回 immutable=1——它忽略未 checkpoint 的 WAL，对「挑一个账号」
// 这个用途足够，且绝不会写库。
func loadProbeCandidates(path string) ([]probeCandidate, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("账号库不存在（%s）: %w", path, err)
	}
	escaped := filepath.ToSlash(path)
	attempts := []string{
		"file:" + escaped + "?mode=ro&_pragma=busy_timeout(5000)",
		"file:" + escaped + "?immutable=1&_pragma=busy_timeout(5000)",
	}
	var lastErr error
	for _, dsn := range attempts {
		candidates, err := queryProbeAccounts(dsn)
		if err == nil {
			return candidates, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func queryProbeAccounts(dsn string) ([]probeCandidate, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	rows, err := db.Query("SELECT id, name, data FROM accounts")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]probeCandidate, 0, 8)
	for rows.Next() {
		var id, name string
		var data []byte
		if err := rows.Scan(&id, &name, &data); err != nil {
			return nil, err
		}
		// 每行独立分配：model.Account 内嵌 sync.Mutex，按值复制会被 go vet 的
		// copylocks 拦下（也确实不安全），所以直接反序列化到新指针。
		acc := new(model.Account)
		if err := json.Unmarshal(data, acc); err != nil {
			continue
		}
		if acc.ID == "" {
			acc.ID = id
		}
		if acc.Name == "" {
			acc.Name = name
		}
		if !probeEligible(acc) {
			continue
		}
		out = append(out, probeCandidate{
			ID: acc.ID, Name: acc.Name, Status: acc.Status, LastUsed: acc.LastUsedAt, acc: acc,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 最久未使用的排前面：探针优先碰「近期没在跑量」的账号，降低对生产的影响。
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].LastUsed, out[j].LastUsed
		switch {
		case a == nil && b == nil:
			return out[i].ID < out[j].ID
		case a == nil:
			return true
		case b == nil:
			return false
		case *a != *b:
			return *a < *b
		default:
			return out[i].ID < out[j].ID
		}
	})
	return out, nil
}

// loadProbeSystemFile 读取自定义 system 文案文件（{"blocks":[…]}）。
//
// 用途：拿到官方客户端的真实抓包后，不必重新发版就能逐字节复现它的 system；
// 也方便做「只改一处」的对照实验。block 0 的 cache_control 与内嵌档案一样被剥掉，
// 保证「文件模式」与「内建档案模式」的出站形态一致。
func loadProbeSystemFile(path string) ([]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Blocks []any `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if len(payload.Blocks) == 0 {
		return nil, fmt.Errorf("system 文件缺少 blocks（%s）", path)
	}
	blocks := make([]any, 0, len(payload.Blocks))
	for i, item := range payload.Blocks {
		block, ok := item.(map[string]any)
		if !ok {
			blocks = append(blocks, item)
			continue
		}
		if i == 0 {
			delete(block, "cache_control")
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// probeEligible 只留「启用的 JWT 账号且当前可正常使用」的候选。
func probeEligible(acc *model.Account) bool {
	if acc == nil || !acc.Enabled || acc.ArchivedAt != nil || acc.ModelProbeOnly {
		return false
	}
	if acc.Mode != "jwt" || acc.JWTToken == nil || strings.TrimSpace(*acc.JWTToken) == "" {
		return false
	}
	if acc.Status != "" && acc.Status != model.StatusActive {
		return false
	}
	return true
}

func pickProbeCandidate(candidates []probeCandidate, selector string) (*probeCandidate, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("没有可用的 JWT 账号（需 enabled 且 status=active）")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return &candidates[0], nil
	}
	for i := range candidates {
		if candidates[i].ID == selector || candidates[i].Name == selector {
			return &candidates[i], nil
		}
	}
	return nil, fmt.Errorf("账号 %q 不在可用候选里（先用 --list 看候选）", selector)
}

func probeSend(picked *probeCandidate, summary upstream.ZcodeSystemProfileSummary, opt probeOptions) probeReport {
	report := probeReport{
		ClientVersion:     config.ZcodeClientVersion,
		SystemProfile:     summary.Profile,
		SystemFirst:       summary.FirstText,
		SystemFirstSHA:    summary.FirstSHA16,
		SystemIdentitySHA: summary.IdentitySHA16,
		SystemBlocks:      summary.Blocks,
		SystemSource:      "builtin",
		Account:           picked.ID,
		AccountName:       picked.Name,
		Model:             opt.Model,
	}
	if picked.acc == nil {
		report.Error = "账号数据为空"
		return report
	}
	body := gateway.NormalizeBody(map[string]any{
		"model":      opt.Model,
		"max_tokens": opt.MaxTokens,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": opt.Message},
			}},
		},
	}, true)
	if opt.SystemFile != "" {
		// 自定义文案：整块替换 system，其余（metadata、头、端点）与线上一致，
		// 这样一次实验只改一个变量。
		blocks, loadErr := loadProbeSystemFile(opt.SystemFile)
		if loadErr != nil {
			report.Error = "读取 system 文件失败: " + loadErr.Error()
			return report
		}
		body["system"] = blocks
		report.SystemSource = "file:" + opt.SystemFile
	}
	attr := upstream.NewAttribution(nil, "")
	deviceMid, deviceSrc := probeDeviceMid(picked.acc)
	report.DeviceMidSrc = deviceSrc
	upstream.InjectDeviceMetadata(body, deviceMid, attr.SessionID)
	built, err := upstream.BuildRequestWithAttribution(picked.acc, "", "", nil, attr)
	if err != nil {
		report.Error = "构造请求失败: " + err.Error()
		return report
	}
	report.Upstream = built.URL
	payload, err := json.Marshal(body)
	if err != nil {
		report.Error = "序列化请求体失败: " + err.Error()
		return report
	}
	transport, egress := probeTransport(picked.acc, opt.Timeout)
	report.Egress = egress
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, built.URL, bytes.NewReader(payload))
	if err != nil {
		report.Error = "构造 HTTP 请求失败: " + err.Error()
		return report
	}
	for key, value := range built.Headers {
		req.Header.Set(key, value)
	}
	client := &http.Client{Transport: transport, Timeout: opt.Timeout}
	started := time.Now()
	resp, err := client.Do(req)
	report.ElapsedMS = time.Since(started).Milliseconds()
	if err != nil {
		report.Error = "发送失败: " + err.Error()
		return report
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, probeResponseLimit))
	report.HTTPStatus = resp.StatusCode
	report.BodyPrefix = strings.TrimSpace(string(raw))
	report.BusinessCode, report.Message = probeBusinessCode(raw)
	report.OK = resp.StatusCode == http.StatusOK && report.BusinessCode == ""
	return report
}

// probeDeviceMid 取账号应使用的设备指纹，全程只读。
func probeDeviceMid(acc *model.Account) (string, string) {
	if acc.VirtualDeviceMid != nil {
		if mid := strings.TrimSpace(*acc.VirtualDeviceMid); mid != "" {
			return mid, "account_virtual"
		}
	}
	path := filepath.Join(config.DataDir, "device_mid.txt")
	if b, err := os.ReadFile(path); err == nil {
		if mid := strings.TrimSpace(string(b)); mid != "" {
			return mid, "data_dir_file"
		}
	}
	// 最后兜底才用临时值：它会让上游视作新设备，报告里标出来，别让人误判结论。
	return config.NewUUIDv4(), "ephemeral"
}

// probeTransport 复用账号绑定的出口；没有绑定就走直连。
func probeTransport(acc *model.Account, timeout time.Duration) (http.RoundTripper, string) {
	if acc.ProxyURL == nil || strings.TrimSpace(*acc.ProxyURL) == "" {
		return http.DefaultTransport, "direct"
	}
	raw := strings.TrimSpace(*acc.ProxyURL)
	transport, err := proxy.TransportForTimeout(raw, timeout)
	if err != nil {
		return http.DefaultTransport, "direct(proxy-invalid)"
	}
	return transport, "proxy:" + probeEgressLabel(raw)
}

func probeEgressLabel(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "unknown"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// probeBusinessCode 从响应体里提取业务码与文案（外层 code/message 或 error 嵌套）。
func probeBusinessCode(raw []byte) (string, string) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", ""
	}
	code := ""
	for _, key := range []string{"code", "error_code", "biz_code"} {
		if value, ok := payload[key]; ok {
			code = probeStringify(value)
			break
		}
	}
	message := ""
	for _, key := range []string{"message", "msg", "error_msg"} {
		if value, ok := payload[key]; ok {
			message = probeStringify(value)
			break
		}
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if code == "" {
			if value, ok := nested["code"]; ok {
				code = probeStringify(value)
			}
		}
		if message == "" {
			if value, ok := nested["message"]; ok {
				message = probeStringify(value)
			}
		}
	}
	return code, message
}

func probeStringify(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func printProbeJSON(payload any, exitCode int) int {
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "序列化输出失败: "+err.Error())
		return 1
	}
	fmt.Println(string(buf))
	return exitCode
}
