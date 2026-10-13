package upstream

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupportedAccountContext 表示调用方携带了尚未支持的账号/权益绑定材料，
// 属于请求错误，不能据此把池中账号标成凭据失效。
var ErrUnsupportedAccountContext = errors.New("当前网关尚不支持高速卡或订阅账号上下文")

// ValidateAccountContextHeaders 在选号和构造请求前拒绝账号绑定头。
// 按头名是否存在判断，空值也不放行；仅输出规范头名，绝不回显调用方的凭据值。
// 固定顺序使多头输入的错误稳定；其他归因、协议能力和自定义头不受影响。
func ValidateAccountContextHeaders(headers map[string]string) error {
	for _, field := range [...]string{
		"X-Highspeed-Card-ID",
		"X-Bigmodel-Authorization",
		"Bigmodel-Target-Type",
		"Bigmodel-Organization",
		"Bigmodel-Project",
	} {
		for name := range headers {
			if strings.EqualFold(name, field) {
				return fmt.Errorf("%w：%s；请求未转发，不会降级为普通请求", ErrUnsupportedAccountContext, field)
			}
		}
	}
	return nil
}
