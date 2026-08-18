package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/handler"
	"github.com/MetroGenes/General_Webhook/internal/heartbeat"
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
	q := queue.New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 1}, handler.NewRegistry(), st, cfg, "owner-1")
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
	if count != 1 || (status != "pending" && status != "processing" && status != "retrying" && status != "dead" && status != "done" && status != "skipped" && status != "partial" && status != "error") {
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
	secret := "s3cret-sixteen!!"
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

func TestWebhook_RejectsDuplicateHMACBodyDifferentDelivery(t *testing.T) {
	secret := "s3cret-sixteen!!"
	srv := newTestServer(t, config.AuthConfig{
		Type:   "hmac",
		Header: "X-Hub-Signature-256",
		Secret: secret,
	}, 8)
	fixed := time.Unix(1_700_000_000, 0)
	srv.now = func() time.Time { return fixed }

	body := []byte(`{"ok":true}`)
	sign := "sha256=" + hmacHex(secret, body)

	for i, id := range []string{"deliv-a", "deliv-b"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hub-Signature-256", sign)
		req.Header.Set("X-GitHub-Delivery", id)
		req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(fixed.Unix(), 10))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if i == 0 {
			if rec.Code != http.StatusAccepted {
				t.Fatalf("first delivery = %d, want accepted", rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusConflict {
			t.Fatalf("same body different delivery = %d, want conflict", rec.Code)
		}
	}
}

func TestWebhook_BoundHMAC(t *testing.T) {
	secret := "s3cret-sixteen!!"
	srv := newTestServer(t, config.AuthConfig{
		Type:          "hmac",
		Header:        "X-Hub-Signature-256",
		Secret:        secret,
		SignedHeaders: []string{"timestamp", "delivery_id", "body"},
	}, 8)
	fixed := time.Unix(1_700_000_000, 0)
	srv.now = func() time.Time { return fixed }

	body := []byte(`{"ok":true}`)
	ts := strconv.FormatInt(fixed.Unix(), 10)
	payload := ts + "\n" + "deliv-bound" + "\n" + string(body)
	sign := "sha256=" + hmacHex(secret, []byte(payload))

	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", sign)
	req.Header.Set("X-Delivery-Id", "deliv-bound")
	req.Header.Set("X-Webhook-Timestamp", ts)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebhook_RejectsStaleTimestamp(t *testing.T) {
	secret := "s3cret-sixteen!!"
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
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "10.0.0.99")
	req.RemoteAddr = "192.0.2.10:12345"
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

func TestOpsEndpoints(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader([]byte(`{"x":1}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("accept = %d", rec.Code)
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	id := resp["event_id"]

	recStats := httptest.NewRecorder()
	unauthorized := httptest.NewRequest(http.MethodGet, "/queue/stats", nil)
	srv.Handler().ServeHTTP(recStats, unauthorized)
	if recStats.Code != http.StatusUnauthorized {
		t.Fatalf("stats without admin token = %d", recStats.Code)
	}
	recStats = httptest.NewRecorder()
	statsReq := adminRequest(http.MethodGet, "/queue/stats", nil)
	srv.Handler().ServeHTTP(recStats, statsReq)
	if recStats.Code != http.StatusOK {
		t.Fatalf("stats = %d", recStats.Code)
	}
	recEv := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recEv, adminRequest(http.MethodGet, "/events/"+id, nil))
	if recEv.Code != http.StatusOK {
		t.Fatalf("get event = %d body=%s", recEv.Code, recEv.Body.String())
	}
	recMetrics := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recMetrics, adminRequest(http.MethodGet, "/metrics", nil))
	if recMetrics.Code != http.StatusOK || !strings.Contains(recMetrics.Body.String(), "general_webhook_events") {
		t.Fatalf("metrics = %d %q", recMetrics.Code, recMetrics.Body.String())
	}
	if !strings.Contains(recMetrics.Body.String(), "general_webhook_action_failure_ratio") {
		t.Fatalf("metrics missing action failure ratio: %q", recMetrics.Body.String())
	}
}

func TestMetrics_HeartbeatGauge(t *testing.T) {
	srv := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	// Attach a monitor with no targets; the metric must report 0 (never).
	hb := heartbeat.New(config.HeartbeatConfig{Host: "test"}, srv.st, nil, nil)
	srv.SetHeartbeat(hb)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "general_webhook_heartbeat_last_success_timestamp 0") {
		t.Fatalf("metrics missing heartbeat gauge: %q", rec.Body.String())
	}
}

func TestRetryEvent_ReplaysOnlyDeadActionsAndRecordsReason(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	src := config.Source{
		Name: "test", Auth: config.AuthConfig{Type: "none"},
		Actions: []config.ActionConfig{{Type: "exec", Command: "/bin/true"}, {Type: "exec", Command: "/bin/false"}},
	}
	cfg := &config.Config{
		Admin:   config.AdminConfig{Header: "Authorization", Token: "admin-token-123456789"},
		Sources: []config.Source{src},
	}
	snapshot, err := config.SnapshotSource(&src)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	if err := st.SavePendingEvent(context.Background(), "dead-event", src.Name, "", []byte(`{}`), now, "", "", "hash", snapshot); err != nil {
		t.Fatal(err)
	}
	if ev, err := st.ClaimPending(context.Background(), time.Minute, now, "owner"); err != nil || ev == nil {
		t.Fatalf("claim=%+v err=%v", ev, err)
	}
	if err := st.PrepareEvent(context.Background(), "dead-event", `{}`, 2, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartAction(context.Background(), "dead-event", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteAction(context.Background(), "dead-event", 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartAction(context.Background(), "dead-event", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := st.DeadAction(context.Background(), "dead-event", 1, "downstream unavailable", now); err != nil {
		t.Fatal(err)
	}
	if status, err := st.RefreshEventStatus(context.Background(), "dead-event", now); err != nil || status != "dead" {
		t.Fatalf("status=%q err=%v", status, err)
	}

	q := queue.New(config.QueueConfig{Workers: 1, Buffer: 1}, handler.NewRegistry(), st, cfg, "owner")
	srv := New(cfg, q, st)
	body := bytes.NewReader([]byte(`{"reason":"downstream recovered"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, adminRequest(http.MethodPost, "/events/dead-event/retry", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response["generation"] != float64(1) {
		t.Fatalf("response=%v err=%v", response, err)
	}
	states, err := st.ListActionStates(context.Background(), "dead-event")
	if err != nil || len(states) != 2 || states[0].Status != "done" || states[1].Status != "pending" {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	meta, err := st.GetEvent(context.Background(), "dead-event")
	if err != nil || meta.ReplayReason != "downstream recovered" || meta.ReplayGeneration != 1 {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
}

func TestOpsRoutesAbsentWithoutAdminToken(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{}
	q := queue.New(config.QueueConfig{Workers: 1, Buffer: 1}, handler.NewRegistry(), st, cfg, "owner")
	srv := New(cfg, q, st)
	for _, path := range []string{"/events/id", "/queue/stats", "/metrics"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", path, rec.Code)
		}
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
		Admin: config.AdminConfig{Header: "Authorization", Token: "admin-token-123456789"},
		Sources: []config.Source{{
			Name:    "test",
			Auth:    authCfg,
			Actions: []config.ActionConfig{{Type: "exec", Command: "/bin/true"}},
		}},
	}
	retain := true
	cfg.Store.RetainPayload = &retain
	cfg.Server.MaxInFlight = 16
	cfg.Server.MaxInFlightBytes = 32 << 20
	q := queue.New(config.QueueConfig{Workers: 1, Buffer: buffer, MaxRetries: 1}, handler.NewRegistry(), st, cfg, "owner-1")
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	q.Start(workerCtx)
	t.Cleanup(func() {
		cancelWorker()
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = q.Stop(stopCtx)
	})
	return New(cfg, q, st)
}

func adminRequest(method, target string, body *bytes.Reader) *http.Request {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Authorization", "Bearer admin-token-123456789")
	return req
}

func hmacHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
