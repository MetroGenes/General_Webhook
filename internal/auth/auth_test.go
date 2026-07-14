package auth

import (
	"net/http"
	"net/http/httptest"
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
