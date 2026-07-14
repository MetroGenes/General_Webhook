package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/handler"
	"github.com/MetroGenes/General_Webhook/internal/queue"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

func TestWebhook_OutboxAcceptsWhenWakeBufferFull(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "webhook.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		Sources: []config.Source{{
			Name:    "test",
			Auth:    config.AuthConfig{Type: "none"},
			Actions: []config.ActionConfig{{Type: "exec", Command: "/bin/true"}},
		}},
	}
	retain := true
	cfg.Store.RetainPayload = &retain
	q := queue.New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 1}, handler.NewRegistry(), st, cfg)
	srv := New(cfg, q, st)

	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	var count int
	var status string
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(status), '') FROM events`).Scan(&count, &status); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if count != 1 || (status != "pending" && status != "processing" && status != "done" && status != "skipped" && status != "partial" && status != "error") {
		t.Fatalf("persisted event count/status = %d/%q", count, status)
	}
}

func TestWebhook_RejectsNonJSONContentType(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
}

func TestWebhook_RejectsInvalidJSON(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestWebhook_RejectsDuplicateDelivery(t *testing.T) {
	secret := "s3cret"
	srv := newTestServer(t, config.AuthConfig{
		Type:   "hmac",
		Header: "X-Hub-Signature-256",
		Secret: secret,
	}, 8)
	fixed := time.Unix(1_700_000_000, 0)
	srv.now = func() time.Time { return fixed }

	body := []byte(`{"ok":true}`)
	sign := "sha256=" + hmacHex(secret, body)

	makeReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", sign)
		req.Header.Set("X-GitHub-Delivery", "deliv-1")
		req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(fixed.Unix(), 10))
		return req
	}

	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, makeReq())
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d body=%s", rec1.Code, http.StatusAccepted, rec1.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec1.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid json response: %v body=%s", err, rec1.Body.String())
	}
	if resp["event_id"] == "" || resp["event_id"] == "deliv-1" {
		t.Fatalf("event_id should be internal, got %q", resp["event_id"])
	}

	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, makeReq())
	if rec2.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want %d", rec2.Code, http.StatusConflict)
	}
}

func TestWebhook_AllowsDistinctDeliverySameHMACBody(t *testing.T) {
	secret := "s3cret"
	srv := newTestServer(t, config.AuthConfig{
		Type:   "hmac",
		Header: "X-Hub-Signature-256",
		Secret: secret,
	}, 8)
	fixed := time.Unix(1_700_000_000, 0)
	srv.now = func() time.Time { return fixed }

	body := []byte(`{"ok":true}`)
	sign := "sha256=" + hmacHex(secret, body)

	for _, id := range []string{"deliv-a", "deliv-b"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", sign)
		req.Header.Set("X-GitHub-Delivery", id)
		req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(fixed.Unix(), 10))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("delivery %s = %d, want accepted", id, rec.Code)
		}
	}
}

func TestWebhook_RejectsStaleTimestamp(t *testing.T) {
	secret := "s3cret"
	srv := newTestServer(t, config.AuthConfig{
		Type:   "hmac",
		Header: "X-Hub-Signature-256",
		Secret: secret,
	}, 8)
	now := time.Unix(1_700_000_000, 0)
	srv.now = func() time.Time { return now }

	body := []byte(`{"ok":true}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hmacHex(secret, body))
	req.Header.Set("X-GitHub-Delivery", "deliv-stale")
	req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(now.Add(-20*time.Minute).Unix(), 10))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestWebhook_IgnoresSpoofedXFFWithoutTrustedProxy(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "10.0.0."+strconv.Itoa(i))
		req.RemoteAddr = "192.0.2.10:12345"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if i < 30 && rec.Code == http.StatusTooManyRequests {
			t.Fatalf("unexpected rate limit at %d", i)
		}
	}
	// same remote should eventually hit per-source limiter after auth
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "10.0.0.99")
	req.RemoteAddr = "192.0.2.10:12345"
	// burn remaining tokens quickly by reusing same source|ip
	limited := false
	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("expected rate limit on RemoteAddr, not forged XFF")
	}
	if srv.limiter.lenBuckets() > 5 {
		t.Fatalf("limiter buckets = %d, XFF should not create many keys", srv.limiter.lenBuckets())
	}
}

func TestWebhook_RejectsInvalidDeliveryID(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Delivery-Id", `bad"id`)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHealthzAndReadyz(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	for _, path := range []string{"/healthz", "/webhook/healthz", "/readyz", "/webhook/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("%s => %d %q", path, rec.Code, rec.Body.String())
		}
	}
	srv.SetStopping()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while stopping = %d", rec.Code)
	}
}

func TestClientIP_TrustedProxy(t *testing.T) {
	trusted := parseTrustedProxies([]string{"10.0.0.0/8"})
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = "10.1.2.3:9999"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.1.2.3")
	if got := clientIP(req, trusted); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want 203.0.113.9", got)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.RemoteAddr = "198.51.100.1:1"
	req2.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := clientIP(req2, trusted); got != "198.51.100.1" {
		t.Fatalf("untrusted remote should ignore XFF, got %q", got)
	}
}

func newTestServer(t *testing.T, authCfg config.AuthConfig, buffer int) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "webhook.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{
		Sources: []config.Source{{
			Name:    "test",
			Auth:    authCfg,
			Actions: []config.ActionConfig{{Type: "exec", Command: "/bin/true"}},
		}},
	}
	retain := true
	cfg.Store.RetainPayload = &retain
	q := queue.New(config.QueueConfig{Workers: 1, Buffer: buffer, MaxRetries: 1}, handler.NewRegistry(), st, cfg)
	return New(cfg, q, st)
}

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
