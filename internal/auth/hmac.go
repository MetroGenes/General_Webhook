package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// verifyHMAC 校验 GitHub 风格的 HMAC-SHA256 签名:
// 头部形如 "X-Hub-Signature-256: sha256=<hex>"。
func verifyHMAC(ac config.AuthConfig, r *http.Request, body []byte) error {
	if ac.Secret == "" {
		// fail-closed: secret 未配置 (常因环境变量漏注入) 时一律拒绝
		return ErrUnauthorized
	}
	header := ac.Header
	if header == "" {
		header = "X-Hub-Signature-256"
	}
	got := strings.TrimPrefix(r.Header.Get(header), "sha256=")
	if got == "" {
		return ErrUnauthorized
	}
	mac := hmac.New(sha256.New, []byte(ac.Secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	// 常量时间比较, 防时序攻击
	if !hmac.Equal([]byte(got), []byte(want)) {
		return ErrUnauthorized
	}
	return nil
}
