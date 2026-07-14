package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// verifyToken 校验静态 token, 仅支持专用请求头 (不再接受 ?token= 查询参数)。
func verifyToken(ac config.AuthConfig, r *http.Request) error {
	if ac.Secret == "" {
		// fail-closed: secret 未配置 (常因环境变量漏注入) 时一律拒绝
		return ErrUnauthorized
	}
	header := ac.Header
	if header == "" {
		header = "X-Webhook-Token"
	}
	got := r.Header.Get(header)
	if subtle.ConstantTimeCompare([]byte(got), []byte(ac.Secret)) != 1 {
		return ErrUnauthorized
	}
	return nil
}
