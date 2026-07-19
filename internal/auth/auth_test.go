package auth

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/MetroGenes/General_Webhook/internal/config"
)

func TestVerify_EmptyHMACSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := Verify(config.AuthConfig{Type: "hmac", Secret: ""}, req, []byte(`{}`))
	if err != ErrUnauthorized {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyHMAC_BodyAndBoundHeaders(t *testing.T) {
	secret := "0123456789abcdef"
	body := []byte(`{"ok":true}`)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Hub-Signature-256", "sha256="+SignHMACHex(secret, body))
	if err := Verify(config.AuthConfig{Type: "hmac", Secret: secret}, req, body); err != nil {
		t.Fatal(err)
	}
	timestamp := strconv.FormatInt(1_700_000_000, 10)
	req.Header.Set("X-Webhook-Timestamp", timestamp)
	req.Header.Set("X-Delivery-Id", "delivery-1")
	payload := []byte(timestamp + "\n" + "delivery-1" + "\n" + string(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+SignHMACHex(secret, payload))
	bound := config.AuthConfig{Type: "hmac", Secret: secret, SignedHeaders: []string{"timestamp", "delivery_id", "body"}}
	if err := Verify(bound, req, body); err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Delivery-Id", "tampered")
	if err := Verify(bound, req, body); err != ErrUnauthorized {
		t.Fatalf("tampered bound field: %v", err)
	}
}

func TestVerify_EmptyTokenSecret(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := Verify(config.AuthConfig{Type: "token", Secret: ""}, req, nil)
	if err != ErrUnauthorized {
		t.Fatalf("got %v", err)
	}
}

func TestVerify_UnknownType(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := Verify(config.AuthConfig{Type: "basic"}, req, nil)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestVerify_None(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	if err := Verify(config.AuthConfig{Type: "none"}, req, nil); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyToken_HeaderOnly(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?token=secret", nil)
	err := Verify(config.AuthConfig{Type: "token", Secret: "secret"}, req, nil)
	if err != ErrUnauthorized {
		t.Fatalf("query token should be ignored, got %v", err)
	}
	req.Header.Set("X-Webhook-Token", "secret")
	if err := Verify(config.AuthConfig{Type: "token", Secret: "secret"}, req, nil); err != nil {
		t.Fatal(err)
	}
}
