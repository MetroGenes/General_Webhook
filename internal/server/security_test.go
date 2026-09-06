package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

func TestUnauthenticatedTrafficDoesNotSpendGlobalQuota(t *testing.T) {
	const secret = "source-secret-for-rate-test"
	initial := newTestServer(t, config.AuthConfig{Type: "token", Secret: secret}, 8)
	initial.cfg.Server.RateLimit.Global = config.RateLimitConfig{Burst: 1, PerSecond: 1}
	s := New(initial.cfg, initial.q, initial.st)
	fixed := time.Now()
	s.now = func() time.Time { return fixed }
	request := func(token, ip string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Webhook-Token", token)
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 10; i++ {
		if code := request("invalid", "192.0.2.1"); code != 401 {
			t.Fatalf("bad auth status=%d", code)
		}
	}
	if code := request(secret, "192.0.2.2"); code != 202 {
		t.Fatalf("unauthenticated traffic spent quota: %d", code)
	}
	if code := request(secret, "192.0.2.2"); code != 429 {
		t.Fatalf("configured global quota not enforced: %d", code)
	}
	if s.rateRejected[1].Load() != 1 {
		t.Fatal("global rejection not counted")
	}
}

func TestPreAuthenticationQuotaIsPerIP(t *testing.T) {
	initial := newTestServer(t, config.AuthConfig{Type: "token", Secret: "source-token-0123456789"}, 8)
	initial.cfg.Server.RateLimit.PreAuthPerIP = config.RateLimitConfig{Burst: 1, PerSecond: 1}
	s := New(initial.cfg, initial.q, initial.st)
	fixed := time.Now()
	s.now = func() time.Time { return fixed }
	for i, ip := range []string{"192.0.2.1", "192.0.2.1", "192.0.2.2"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		want := 401
		if i == 1 {
			want = 429
		}
		if rec.Code != want {
			t.Fatalf("request %d got=%d want=%d", i, rec.Code, want)
		}
	}
	if s.rateRejected[0].Load() != 1 {
		t.Fatal("pre-auth rejection not counted")
	}
}

func TestRejectedValuesCannotEnterMetrics(t *testing.T) {
	s := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	s.cfg.Sources[0].Extract = map[string]string{"service": "$.service"}
	s.cfg.Sources[0].LogPolicy = &config.LogPolicyConfig{Mode: "allowlist", Field: "service", Services: []string{"allowed"}, OnReject: "drop_and_count"}
	value := "FAKE-SECRET\ninjected_metric 123\n#\t"
	body, _ := json.Marshal(map[string]string{"service": value})
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("accept=%d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.q.DropSnapshot()["test"]["allowlist_rejected"] == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	metrics := httptest.NewRecorder()
	s.Handler().ServeHTTP(metrics, adminRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != 200 {
		t.Fatal(metrics.Code)
	}
	text := metrics.Body.String()
	if strings.Contains(text, "FAKE-SECRET") || strings.Contains(text, "injected_metric") || strings.Contains(text, "drops_summary") {
		t.Fatalf("raw rejected value exposed: %s", text)
	}
	if !strings.Contains(text, "reason=\"allowlist_rejected\"} 1") {
		t.Fatalf("missing bounded counter: %s", text)
	}
}

func TestTimestampRangeIncludesOnlyAllowedSkew(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		stamp time.Time
		valid bool
	}{
		{now, true}, {now.Add(-defaultReplaySkew), true}, {now.Add(defaultReplaySkew), true},
		{now.Add(defaultReplaySkew + time.Second), false}, {now.Add(-defaultReplaySkew - time.Second), false},
		{time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC), false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("X-Webhook-Timestamp", strconv.FormatInt(tc.stamp.Unix(), 10))
		err := validateReplayHeaders(req, now, defaultReplaySkew)
		if (err == nil) != tc.valid {
			t.Errorf("stamp=%s valid=%t err=%v", tc.stamp, tc.valid, err)
		}
	}
}

func TestHeartbeatErrorLogAdminAPI(t *testing.T) {
	s := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	err := s.st.SaveHeartbeatError(context.Background(), store.HeartbeatLog{Target: "kuma", Origin: "https://kuma.example", Mode: "kuma", Health: "pass", Error: "request timed out", CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/heartbeat/logs", nil))
	if unauth.Code != 401 {
		t.Fatalf("unprotected logs: %d", unauth.Code)
	}
	for _, tc := range []struct {
		path   string
		status int
	}{{"/heartbeat/logs?limit=1", 200}, {"/heartbeat/logs?limit=0", 400}, {"/heartbeat/logs?limit=101", 400}, {"/heartbeat/logs?limit=bad", 400}} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, adminRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
		if tc.status == 200 && !strings.Contains(rec.Body.String(), "request timed out") {
			t.Fatalf("missing persisted error: %s", rec.Body.String())
		}
	}
}

func TestSourceQuotaRejectionsDoNotSpendGlobalQuota(t *testing.T) {
	initial := newTestServer(t, config.AuthConfig{Type: "none"}, 8)
	initial.cfg.Server.RateLimit.Global = config.RateLimitConfig{Burst: 3, PerSecond: 1}
	initial.cfg.Server.RateLimit.SourcePerIP = config.RateLimitConfig{Burst: 1, PerSecond: 1}
	s := New(initial.cfg, initial.q, initial.st)
	fixed := time.Now()
	s.now = func() time.Time { return fixed }
	for i, ip := range []string{"192.0.2.1", "192.0.2.1", "192.0.2.1", "192.0.2.2"} {
		req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = ip + ":1234"
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		want := 202
		if i == 1 || i == 2 {
			want = 429
		}
		if rec.Code != want {
			t.Fatalf("request %d status=%d want=%d", i, rec.Code, want)
		}
	}
	if s.rateRejected[1].Load() != 0 || s.rateRejected[2].Load() != 2 {
		t.Fatal("unexpected quota accounting")
	}
}
