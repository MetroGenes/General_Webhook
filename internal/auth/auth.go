package auth

import (
	"errors"
	"net/http"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

var (
	// ErrUnauthorized 表示签名/token 校验失败。
	ErrUnauthorized = errors.New("unauthorized")
	// ErrUnsupported 表示配置了未知的 auth 类型。
	ErrUnsupported = errors.New("unsupported auth type")
)

// Verify 按 source 的 auth 配置校验请求, 通过返回 nil。
func Verify(ac config.AuthConfig, r *http.Request, body []byte) error {
	switch ac.Type {
	case "none":
		return nil
	case "hmac":
		return verifyHMAC(ac, r, body)
	case "token":
		return verifyToken(ac, r)
	case "":
		// 空类型应该在配置加载时被拦住, 这里防御性拒绝
		return ErrUnsupported
	default:
		return ErrUnsupported
	}
}
