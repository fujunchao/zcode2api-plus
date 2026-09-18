// Package upstream 构造对 ZCode 上游的请求。
// 对应 Python 版 app/agent.py：按账号凭证选端点、组装请求头。
// 实际发送与流式透传在 internal/gateway。
package upstream

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/textproto"
	"strings"
	"sync"

	"zcode2api/internal/config"
	"zcode2api/internal/model"
)

//go:embed zcode_system.json
var zcodeSystemJSON []byte

var (
	zcodeOnce   sync.Once
	zcodeBlocks []any
)

// ZcodeSystemBlocks 返回 ZCode 官方系统提示词块（JSON 解析失败时为空，对齐 Python 版）。
// JWT 账号请求必须注入到顶层 system，否则上游返回 405。
func ZcodeSystemBlocks() []any {
	zcodeOnce.Do(func() {
		var blocks []any
		if err := json.Unmarshal(zcodeSystemJSON, &blocks); err != nil {
			blocks = []any{}
		}
		zcodeBlocks = blocks
	})
	return zcodeBlocks
}

// dropHeaders 透传客户端 header 时需要剔除的字段（对齐 agent.py _DROP_HEADERS）。
var dropHeaders = map[string]bool{
	"host":                           true,
	"content-length":                 true,
	"x-api-key":                      true,
	"authorization":                  true,
	"user-agent":                     true,
	"http-referer":                   true,
	"accept-encoding":                true,
	"connection":                     true,
	"cookie":                         true,
	"x-aliyun-captcha-verify-param":  true,
	"x-aliyun-captcha-verify-region": true,
}

// Request 构造结果：目标 URL 与请求头。
type Request struct {
	URL     string
	Headers map[string]string
}

// BuildRequest 对应 Python 版 build_request：
// JWT 账号走 zcode.z.ai 主端点（Bearer），API Key 账号走 api.z.ai 回退端点（x-api-key）。
// verifyParam/verifyRegion 为验证码令牌（仅 JWT 账号）；incomingHeaders 为客户端透传头。
//
// 头合并的优先级是固定的：**网关固定头恒胜**。客户端透传头先落，固定头最后覆写，
// 因此客户端既不能改掉鉴权与网关标识，也不能改掉每账号的设备指纹（X-Device-Mid）。
func BuildRequest(acc *model.Account, verifyParam, verifyRegion string, incomingHeaders map[string]string) (Request, error) {
	var targetURL, authHeader, authValue string
	if acc.Provider == model.ProviderZai {
		if acc.Mode == "jwt" && acc.JWTToken != nil {
			targetURL = config.UpstreamZai
			authHeader, authValue = "Authorization", "Bearer "+*acc.JWTToken
		} else if acc.APIKey != nil {
			targetURL = config.UpstreamZaiFallback
			authHeader, authValue = "X-Api-Key", *acc.APIKey
		} else {
			return Request{}, errors.New("账号缺少有效凭证")
		}
	} else {
		return Request{}, fmt.Errorf("未知提供商: %s", acc.Provider)
	}

	// 网关固定头：客户端透传头一律不得改写这些取值，尤其是每账号的设备指纹
	//（伪造它等于让上游把多个账号看成同一台设备，与账号隔离的目的相反）。
	fixed := map[string]string{
		"Content-Type":        "application/json",
		authHeader:            authValue,
		"Anthropic-Version":   "2023-06-01",
		"User-Agent":          config.UserAgent,
		"X-ZCode-App-Version": config.ZcodeClientVersion,
		"X-ZCode-Agent":       "glm",
		"HTTP-Referer":        "https://zcode.z.ai/",
		"X-Device-Mid":        acc.DeviceMidOr(config.DeviceMid()),
	}
	if verifyParam != "" {
		fixed["X-Aliyun-Captcha-Verify-Param"] = verifyParam
		if verifyRegion != "" {
			fixed["X-Aliyun-Captcha-Verify-Region"] = verifyRegion
		}
	}

	// 透传头先合并，并把键统一为规范形式。不规范化的话，固定头 `X-Device-Mid`
	// 与客户端送来的 `x-device-mid` 会在 map 里各占一个键；下游 `Header.Set`
	// 又把两者归一到同一名字，最终取值便取决于 map 的迭代顺序——同名头的结果
	// 成了随机的，这正是客户端能间接影响指纹的成因。
	headers := make(map[string]string, len(fixed)+len(incomingHeaders))
	for key, value := range incomingHeaders {
		lower := strings.ToLower(key)
		if dropHeaders[lower] || strings.HasPrefix(lower, "x-zcode") {
			continue
		}
		headers[textproto.CanonicalMIMEHeaderKey(key)] = value
	}
	// 固定头最后写回，同名透传头被覆盖——固定头必胜。
	for key, value := range fixed {
		headers[key] = value
	}

	return Request{URL: targetURL, Headers: headers}, nil
}
