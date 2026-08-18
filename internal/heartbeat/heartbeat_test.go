package heartbeat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testConfig(interval, timeout time.Duration, targets ...config.HeartbeatTarget) config.HeartbeatConfig {
	cfg := config.HeartbeatConfig{
		Interval: config.Duration{Duration: interval},
		Timeout:  config.Duration{Duration: timeout},
		Host:     "test-host",
	}
	cfg.Targets = targets
	return cfg
}

// recordingServer records requests and returns the configured status code.
func recordingServer(t *testing.T, status int) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var reqs []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := r.Clone(r.Context())
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		reqs = append(reqs, req)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func decodePayload(t *testing.T, req *http.Request) Payload {
	t.Helper()
	body, _ := io.ReadAll(req.Body)
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode payload: %v\nbody: %s", err, body)
	}
	return p
}

func TestPushMode_PassPostsToURL(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK)
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL, Token: "secret-token", Mode: "push",
	}), newTestStore(t), func() bool { return true }, nil)

	m.ping(context.Background())

	if len(*reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(*reqs))
	}
	req := (*reqs)[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != "/" {
		t.Errorf("path = %q, want / (no /fail on pass)", req.URL.Path)
	}
	if got := req.Header.Get("X-Heartbeat-Token"); got != "secret-token" {
		t.Errorf("token header = %q, want secret-token", got)
	}
	if got := req.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content-type = %q", got)
	}
	p := decodePayload(t, req)
	if p.Status != StatusPass || p.ExitCode != 0 {
		t.Errorf("status = %q exit = %d, want pass/0", p.Status, p.ExitCode)
	}
	if p.Host != "test-host" {
		t.Errorf("host = %q, want test-host", p.Host)
	}
	if p.Stats == nil {
		t.Error("stats payload missing")
	}
	if p.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %d", p.UptimeSeconds)
	}
	if m.LastSuccess() == 0 {
		t.Error("last success not recorded")
	}
}

func TestPushMode_FailPostsToFailEndpoint(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK)
	var ready atomic.Bool
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL + "/ping/abc", Mode: "push",
	}), newTestStore(t), ready.Load, nil)

	ready.Store(true)
	m.ping(context.Background())
	if len(*reqs) != 1 || (*reqs)[0].URL.Path != "/ping/abc" {
		t.Fatalf("pass path wrong: %+v", (*reqs))
	}

	ready.Store(false)
	m.ping(context.Background())
	if len(*reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(*reqs))
	}
	if (*reqs)[1].URL.Path != "/ping/abc/fail" {
		t.Errorf("fail path = %q, want /ping/abc/fail", (*reqs)[1].URL.Path)
	}
	p := decodePayload(t, (*reqs)[1])
	if p.Status != StatusFail || p.ExitCode != 1 {
		t.Errorf("status = %q exit = %d, want fail/1", p.Status, p.ExitCode)
	}
}

func TestJSONMode_CarriesStatusAndHost(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK)
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "cf", URL: srv.URL + "/api/heartbeat", Mode: "json",
	}), newTestStore(t), func() bool { return false }, nil)

	m.ping(context.Background())

	if len(*reqs) != 1 {
		t.Fatalf("got %d requests, want 1", len(*reqs))
	}
	if (*reqs)[0].URL.Path != "/api/heartbeat" {
		t.Errorf("path = %q", (*reqs)[0].URL.Path)
	}
	p := decodePayload(t, (*reqs)[0])
	if p.Status != StatusFail || p.ExitCode != 1 {
		t.Errorf("status = %q exit = %d, want fail/1 (not ready)", p.Status, p.ExitCode)
	}
	if p.Host != "test-host" {
		t.Errorf("host = %q", p.Host)
	}
	if p.Timestamp.IsZero() {
		t.Error("timestamp missing")
	}
}

func TestNoTokenHeaderWhenTokenEmpty(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK)
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL, Mode: "push",
	}), newTestStore(t), func() bool { return true }, nil)

	m.ping(context.Background())

	if got := (*reqs)[0].Header.Get("X-Heartbeat-Token"); got != "" {
		t.Errorf("token header = %q, want empty", got)
	}
}

func TestNon2xxResponseIsFailure(t *testing.T) {
	srv, _ := recordingServer(t, http.StatusInternalServerError)
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL, Mode: "push",
	}), newTestStore(t), func() bool { return true }, nil)

	m.ping(context.Background())

	if m.LastSuccess() != 0 {
		t.Errorf("last success = %d, want 0 after 500", m.LastSuccess())
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var leaked atomic.Bool
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		leaked.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer leak.Close()

	// Redirect target URL contains a fake push token that must never leak.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leak.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()

	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL + "/secret-push-token", Token: "header-token", Mode: "push",
	}), newTestStore(t), func() bool { return true }, nil)

	m.ping(context.Background())

	if leaked.Load() {
		t.Fatal("redirect was followed; token header may have leaked")
	}
	if m.LastSuccess() == 0 {
		t.Error("3xx (unfollowed) should count as success")
	}
}

func TestLoopAndStop(t *testing.T) {
	var pings atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pings.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := New(testConfig(20*time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL, Mode: "push",
	}), newTestStore(t), func() bool { return true }, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for pings.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pings.Load() < 2 {
		t.Fatalf("got %d pings, want >= 2", pings.Load())
	}
	m.Stop()
	before := pings.Load()
	time.Sleep(50 * time.Millisecond)
	if got := pings.Load(); got != before {
		t.Errorf("pings after Stop = %d, want %d (loop must stop)", got, before)
	}
}

func TestDropSummaryIncluded(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK)
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "hc", URL: srv.URL, Mode: "json",
	}), newTestStore(t), func() bool { return true }, func() string { return "service=web:3" })

	m.ping(context.Background())

	if got := decodePayload(t, (*reqs)[0]).DropSummary; got != "service=web:3" {
		t.Errorf("drop_summary = %q", got)
	}
}

func TestAuditURLTargetStripsTokenPath(t *testing.T) {
	got := auditURLTarget("https://hc-ping.com/abcdef1234/my-check?foo=bar")
	if got != "https://hc-ping.com" {
		t.Errorf("audit = %q, want https://hc-ping.com", got)
	}
	if got := auditURLTarget("not a url"); got != "invalid-url" {
		t.Errorf("audit = %q, want invalid-url", got)
	}
}
