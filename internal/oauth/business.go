package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// ErrBusinessUnauthorized 只描述 API Key 兑换链的业务认证失败，不触发主 JWT 登出。
var ErrBusinessUnauthorized = errors.New("Z.AI 业务认证已失效（code=401），请重新登录后再兑换 API Key")

// decodeBusinessResponse 先检查业务结果，再允许调用方使用 data。
// 不回显上游 msg 或任意 code 字符串，避免错误响应夹带令牌、代理口令或换行。
func decodeBusinessResponse(body []byte) (map[string]any, error) {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
		return nil, errors.New("上游响应无效 JSON")
	}
	if codeIsZero(parsed["code"]) {
		return parsed, nil
	}
	code := businessErrorCode(parsed["code"])
	if code == "401" {
		return nil, ErrBusinessUnauthorized
	}
	if code == "" || code == "0" {
		return nil, errors.New("Z.AI 业务请求失败（无效业务码）")
	}
	return nil, fmt.Errorf("Z.AI 业务请求失败（code=%s）", code)
}

func businessErrorCode(value any) string {
	var candidate string
	switch code := value.(type) {
	case float64:
		candidate = strconv.FormatFloat(code, 'f', -1, 64)
	case string:
		candidate = code
	default:
		return ""
	}
	if len(candidate) > 32 {
		return ""
	}
	code, err := strconv.ParseInt(candidate, 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(code, 10)
}
