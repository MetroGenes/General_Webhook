package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

// verifyHMAC 校验 HMAC-SHA256 签名。
// 默认（GitHub 兼容）仅签名 body。
// 若 auth.signed_headers 非空，则按顺序拼接 timestamp / delivery_id / body（以 \n 分隔）。
func verifyHMAC(ac config.AuthConfig, r *http.Request, body []byte) error {
	if ac.Secret == "" {
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

	payload := buildHMACPayload(ac.SignedHeaders, r, body)
	mac := hmac.New(sha256.New, []byte(ac.Secret))
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(got), []byte(want)) {
		return ErrUnauthorized
	}
	return nil
}

func buildHMACPayload(signed []string, r *http.Request, body []byte) []byte {
	if len(signed) == 0 {
		return body
	}
	parts := make([]string, 0, len(signed))
	for _, h := range signed {
		switch strings.ToLower(strings.TrimSpace(h)) {
		case "timestamp":
			parts = append(parts, strings.TrimSpace(r.Header.Get("X-Webhook-Timestamp")))
		case "delivery_id":
			parts = append(parts, deliveryIDFromRequest(r))
		case "body":
			parts = append(parts, string(body))
		}
	}
	return []byte(strings.Join(parts, "\n"))
}

func deliveryIDFromRequest(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if id == "" {
		id = strings.TrimSpace(r.Header.Get("X-Delivery-Id"))
	}
	return id
}

// SignHMACHex computes sha256 hex for tests/helpers.
func SignHMACHex(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
