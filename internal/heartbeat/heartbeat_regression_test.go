package heartbeat

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

// ping executes one synchronous round for protocol and persistence tests.
// The production scheduler is covered separately by the independent-loop test.
func (m *Monitor) ping(ctx context.Context) {
	roundCtx, cancel := context.WithTimeout(ctx, 2*m.cfg.Timeout.Duration)
	defer cancel()
	payload := m.sample(roundCtx)
	var wg sync.WaitGroup
	for i := range m.cfg.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.deliver(ctx, roundCtx, i, payload)
		}()
	}
	wg.Wait()
}

type storageStub struct {
	ready func(context.Context) error
	stats func(context.Context, time.Time) (*store.QueueStats, error)
	save  func(context.Context, store.HeartbeatLog) error
}

func (s storageStub) ReadyCheck(ctx context.Context) error {
	if s.ready != nil {
		return s.ready(ctx)
	}
	return ctx.Err()
}

func (s storageStub) Stats(ctx context.Context, now time.Time) (*store.QueueStats, error) {
	if s.stats != nil {
		return s.stats(ctx, now)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &store.QueueStats{}, nil
}

func (s storageStub) SaveHeartbeatError(ctx context.Context, entry store.HeartbeatLog) error {
	if s.save != nil {
		return s.save(ctx, entry)
	}
	return ctx.Err()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func responseFor(r *http.Request, code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r,
	}
}

func captureHeartbeatLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestKumaMode_ChangesQueryStatusAndPreservesTokenPath(t *testing.T) {
	for _, mode := range []string{"kuma", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			srv, reqs := recordingServer(t, http.StatusOK)
			var ready atomic.Bool
			m := New(testConfig(time.Second, time.Second, config.HeartbeatTarget{
				Name: "kuma", URL: srv.URL + "/api/push/token%2Fvalue?status=down&status=up&msg=old&ping=&extra=a%2Fb", Mode: mode,
			}), storageStub{}, ready.Load, nil)
			for _, pass := range []bool{true, false} {
				ready.Store(pass)
				m.ping(context.Background())
			}
			if len(*reqs) != 2 {
				t.Fatalf("requests = %d, want 2", len(*reqs))
			}
			for i, want := range []string{"up", "down"} {
				req := (*reqs)[i]
				if req.Method != http.MethodPost || req.URL.EscapedPath() != "/api/push/token%2Fvalue" {
					t.Errorf("request = %s %s", req.Method, req.URL.EscapedPath())
				}
				query := req.URL.Query()
				if len(query["status"]) != 1 || query.Get("status") != want {
					t.Errorf("query status = %v, want one %s", query["status"], want)
				}
				if query.Get("extra") != "a/b" || !query.Has("ping") {
					t.Errorf("unrelated parameters changed: %v", query)
				}
				if i == 1 && query.Get("msg") != "not ready" {
					t.Errorf("down message = %q", query.Get("msg"))
				}
				payload := decodePayload(t, req)
				if (payload.Status == StatusPass) != (want == "up") {
					t.Errorf("JSON and query disagree: %s / %s", payload.Status, want)
				}
			}
			metrics := m.MetricsSnapshot()[0]
			if metrics.Mode != "kuma" || metrics.Attempts != 2 || metrics.Failures != 0 || metrics.LastHealth != StatusFail || metrics.LastSuccess == 0 {
				t.Fatalf("Kuma metrics = %+v", metrics)
			}
		})
	}
}

func TestHealthchecksFailurePreservesQueryAndEscapedPath(t *testing.T) {
	for _, mode := range []string{"push", "healthchecks"} {
		t.Run(mode, func(t *testing.T) {
			srv, reqs := recordingServer(t, http.StatusOK)
			m := New(testConfig(time.Second, time.Second, config.HeartbeatTarget{
				Name: "hc", URL: srv.URL + "/ping/token%2F/?token=query-value&other=a%2Fb", Mode: mode,
			}), storageStub{}, func() bool { return false }, nil)
			m.ping(context.Background())
			if len(*reqs) != 1 {
				t.Fatalf("requests = %d, want 1", len(*reqs))
			}
			req := (*reqs)[0]
			if req.URL.EscapedPath() != "/ping/token%2F/fail" {
				t.Errorf("escaped path = %q", req.URL.EscapedPath())
			}
			if req.URL.RawQuery != "token=query-value&other=a%2Fb" {
				t.Errorf("query was modified: %s", req.URL.RawQuery)
			}
			if m.LastSuccess() == 0 {
				t.Error("accepted failure signal must count as a successful delivery")
			}
		})
	}
}

func TestKumaRequiresSuccessfulJSONAcknowledgement(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		ok   bool
	}{
		{"accepted", 200, `{"ok":true}`, true},
		{"accepted_created", 201, `{"ok":true,"msg":"received"}`, true},
		{"unknown_token", 200, `{"ok":false,"msg":"unknown secret token"}`, false},
		{"missing_ok", 200, `{"msg":"received"}`, false},
		{"non_boolean_ok", 200, `{"ok":"true"}`, false},
		{"missing_body", 204, "", false},
		{"html", 200, "<html>login</html>", false},
		{"trailing_document", 200, `{"ok":true}{"error":"second document"}`, false},
		{"oversized", 200, `{"ok":true,"msg":"` + strings.Repeat("x", maxResponseBytes) + `"}`, false},
		{"inactive", 404, `{"ok":false,"msg":"inactive monitor"}`, false},
		{"server_error", 500, `{"ok":true}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(testConfig(time.Second, time.Second, config.HeartbeatTarget{
				Name: "kuma", URL: "http://kuma.test/api/push/test-token", Mode: "kuma",
			}), storageStub{}, nil, nil)
			m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return responseFor(r, tc.code, tc.body), nil
			})
			m.ping(context.Background())
			metrics := m.MetricsSnapshot()[0]
			if (metrics.LastSuccess > 0) != tc.ok || (m.LastSuccess() > 0) != tc.ok {
				t.Errorf("acknowledgement accepted = %v, want %v", metrics.LastSuccess > 0, tc.ok)
			}
			if metrics.LastHTTPStatus != tc.code || (metrics.Failures == 0) != tc.ok {
				t.Errorf("metrics = %+v", metrics)
			}
		})
	}
}

func TestRedirectsNeverCountAsDelivery(t *testing.T) {
	var downstream atomic.Int64
	leak := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstream.Add(1)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer leak.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, leak.URL+"/steal", code)
			}))
			defer srv.Close()
			m := New(testConfig(time.Second, time.Second, config.HeartbeatTarget{
				Name: "redirect", URL: srv.URL + "/path-secret?token=query-secret", Token: "header-secret", Mode: "kuma",
			}), storageStub{}, nil, nil)
			m.ping(context.Background())
			if m.LastSuccess() != 0 || m.MetricsSnapshot()[0].LastHTTPStatus != code || m.MetricsSnapshot()[0].Failures != 1 {
				t.Errorf("redirect accepted or status lost: %+v", m.MetricsSnapshot()[0])
			}
		})
	}
	if downstream.Load() != 0 {
		t.Fatalf("redirect destination received %d requests", downstream.Load())
	}
}

func TestReadOnlyStorageSendsKumaDownWithReadableStats(t *testing.T) {
	srv, reqs := recordingServer(t, http.StatusOK)
	var writeProbes atomic.Int64
	st := storageStub{
		ready: func(context.Context) error {
			writeProbes.Add(1)
			return errors.New("attempt to write a readonly database")
		},
		stats: func(context.Context, time.Time) (*store.QueueStats, error) {
			return &store.QueueStats{Pending: 3}, nil
		},
	}
	m := New(testConfig(time.Second, time.Second, config.HeartbeatTarget{
		Name: "kuma", URL: srv.URL + "/api/push/token?status=up", Mode: "kuma",
	}), st, func() bool { return true }, nil)
	m.ping(context.Background())
	if writeProbes.Load() != 1 || len(*reqs) != 1 {
		t.Fatalf("write probes = %d, requests = %d", writeProbes.Load(), len(*reqs))
	}
	req := (*reqs)[0]
	p := decodePayload(t, req)
	if req.URL.Query().Get("status") != "down" || p.Status != StatusFail || p.ExitCode != 1 || p.Stats == nil || p.Stats.Pending != 3 {
		t.Errorf("read-only store reported healthy or lost readable stats: query=%v payload=%+v", req.URL.Query(), p)
	}
}

func TestSamplingDeadlineStillAllowsKumaDown(t *testing.T) {
	for _, stage := range []string{"write", "stats"} {
		t.Run(stage, func(t *testing.T) {
			const timeout = 60 * time.Millisecond
			var observedDeadline atomic.Bool
			block := func(ctx context.Context) error {
				if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= timeout {
					observedDeadline.Store(true)
				}
				<-ctx.Done()
				return ctx.Err()
			}
			st := storageStub{}
			if stage == "write" {
				st.ready = block
			} else {
				st.stats = func(ctx context.Context, _ time.Time) (*store.QueueStats, error) {
					return nil, block(ctx)
				}
			}
			srv, reqs := recordingServer(t, http.StatusOK)
			m := New(testConfig(time.Second, timeout, config.HeartbeatTarget{
				Name: "kuma", URL: srv.URL + "/api/push/token?status=up", Mode: "kuma",
			}), st, nil, nil)
			m.ping(context.Background())
			if !observedDeadline.Load() || len(*reqs) != 1 {
				t.Fatalf("bounded sample = %v, requests = %d", observedDeadline.Load(), len(*reqs))
			}
			if (*reqs)[0].URL.Query().Get("status") != "down" || m.LastSuccess() == 0 {
				t.Errorf("sample timeout did not deliver down: query=%v metrics=%+v", (*reqs)[0].URL.Query(), m.MetricsSnapshot()[0])
			}
		})
	}
}

func TestIndependentTargetLoopsKeepDeliveringWhilePeerIsBlocked(t *testing.T) {
	slowStarted := make(chan struct{}, 1)
	fastReceived := make(chan struct{}, 16)
	var slowAttempts atomic.Int64
	var slowCompleted atomic.Int64
	m := New(testConfig(10*time.Millisecond, 2*time.Second,
		config.HeartbeatTarget{Name: "slow", URL: "http://slow.test/api/push/token", Mode: "kuma"},
		config.HeartbeatTarget{Name: "fast", URL: "http://fast.test/api/push/token", Mode: "kuma"},
	), storageStub{}, nil, nil)
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "slow.test" {
			slowAttempts.Add(1)
			select {
			case slowStarted <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			slowCompleted.Add(1)
			return nil, r.Context().Err()
		}
		select {
		case fastReceived <- struct{}{}:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return responseFor(r, http.StatusOK, `{"ok":true}`), nil
	})
	m.Start(context.Background())
	defer m.Stop()
	m.Start(context.Background()) // Repeated Start must not create extra workers.
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow target did not start")
	}
	for range 4 {
		select {
		case <-fastReceived:
		case <-time.After(time.Second):
			t.Fatal("fast target was held up by slow target")
		}
	}
	if slowAttempts.Load() != 1 || slowCompleted.Load() != 0 {
		t.Errorf("fast delivery waited for timeout, or slow target overlapped: attempts=%d completed=%d", slowAttempts.Load(), slowCompleted.Load())
	}
	m.Stop()
	if slowCompleted.Load() != 1 {
		t.Errorf("Stop did not await slow request cancellation: completed=%d", slowCompleted.Load())
	}
}

func TestPerTargetMetricsAndPersistentPushErrors(t *testing.T) {
	st := newTestStore(t)
	var recoverTarget atomic.Bool
	m := New(testConfig(time.Second, time.Second,
		config.HeartbeatTarget{Name: "healthy", URL: "http://monitor.test/api/push/success-secret", Mode: "kuma"},
		config.HeartbeatTarget{Name: "failed", URL: "http://monitor.test/api/push/failure-secret?token=query-secret", Mode: "kuma"},
	), st, func() bool { return false }, nil)
	clock := time.Unix(1_800_000_000, 0)
	m.now = func() time.Time { return clock }
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "failure") && !recoverTarget.Load() {
			return responseFor(r, http.StatusOK, `{"ok":false,"msg":"failure-secret query-secret"}`), nil
		}
		return responseFor(r, http.StatusOK, `{"ok":true}`), nil
	})
	m.ping(context.Background())
	metrics := m.MetricsSnapshot()
	if len(metrics) != 2 || metrics[0].Attempts != 1 || metrics[0].Failures != 0 || metrics[0].LastSuccess != clock.Unix() {
		t.Fatalf("healthy target metrics = %+v", metrics)
	}
	failed := metrics[1]
	if failed.Name != "failed" || failed.Attempts != 1 || failed.Failures != 1 || failed.LastSuccess != 0 ||
		failed.LastAttempt != clock.Unix() || failed.LastFailure != clock.Unix() || failed.LastHTTPStatus != 200 || failed.LastHealth != StatusFail || failed.LogFailures != 0 {
		t.Errorf("failed target metrics = %+v", failed)
	}
	if m.LastSuccess() != clock.Unix() {
		t.Errorf("global last success = %d, want accepted down delivery", m.LastSuccess())
	}
	entries, err := st.ListHeartbeatErrors(context.Background(), 100)
	if err != nil || len(entries) != 1 {
		t.Fatalf("persisted failures = %+v, err=%v", entries, err)
	}
	entry := entries[0]
	if entry.ID == 0 || entry.Target != "failed" || entry.Origin != "http://monitor.test" || entry.Mode != "kuma" ||
		entry.Health != "fail" || entry.HTTPStatus != 200 || entry.Error != "Kuma rejected heartbeat" ||
		entry.DurationMS < 0 || !entry.CreatedAt.Equal(clock) {
		t.Errorf("unexpected persisted heartbeat error: %+v", entry)
	}
	metrics[0].Name = "mutated snapshot"
	if m.MetricsSnapshot()[0].Name != "healthy" {
		t.Fatal("metrics snapshot exposes mutable internal state")
	}
	recoverTarget.Store(true)
	clock = clock.Add(time.Second)
	m.ping(context.Background())
	failed = m.MetricsSnapshot()[1]
	if failed.Attempts != 2 || failed.Failures != 1 || failed.LastSuccess != clock.Unix() || failed.LastFailure != clock.Add(-time.Second).Unix() {
		t.Errorf("recovery lost failure history or did not record success: %+v", failed)
	}
	entries, err = st.ListHeartbeatErrors(context.Background(), 100)
	if err != nil || len(entries) != 1 {
		t.Errorf("successful pushes should not add error rows: entries=%d err=%v", len(entries), err)
	}
}

func TestFinalLogsAndSQLiteNeverContainPushCredentials(t *testing.T) {
	output := captureHeartbeatLogs(t)
	st := newTestStore(t)
	const endpoint = "https://monitor.test/PATH_SECRET?token=QUERY_SECRET"
	const nestedSecrets = "PATH_SECRET QUERY_SECRET HEADER_SECRET https://monitor.test/PATH_SECRET?token=QUERY_SECRET"
	cases := []struct {
		name string
		url  string
		err  error
		body string
	}{
		{"nested_url", endpoint, &url.Error{Op: "Post", URL: endpoint, Err: errors.New(nestedSecrets)}, ""},
		{"dns", endpoint, &net.DNSError{Name: nestedSecrets, Err: nestedSecrets}, ""},
		{"tls", endpoint, &tls.CertificateVerificationError{Err: errors.New(nestedSecrets)}, ""},
		{"timeout", endpoint, &url.Error{Op: nestedSecrets, URL: endpoint, Err: context.DeadlineExceeded}, ""},
		{"refused", endpoint, &net.OpError{Op: nestedSecrets, Net: "tcp", Err: syscall.ECONNREFUSED}, ""},
		{"malformed", "https://monitor.test/PATH_SECRET%zz?token=QUERY_SECRET", nil, ""},
		{"fragment", endpoint + "#HEADER_SECRET", nil, ""},
		{"body_rejection", endpoint, nil, `{"ok":false,"msg":"` + nestedSecrets + `"}`},
		{"body_invalid", endpoint, nil, nestedSecrets},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New(testConfig(time.Second, time.Second, config.HeartbeatTarget{
				Name: tc.name, URL: tc.url, Token: "HEADER_SECRET", Mode: "kuma",
			}), st, nil, nil)
			m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return responseFor(r, http.StatusOK, tc.body), nil
			})
			m.ping(context.Background())
			metrics := m.MetricsSnapshot()[0]
			if metrics.Failures != 1 || metrics.LogFailures != 0 {
				t.Fatalf("failure not captured in SQL: %+v", metrics)
			}
		})
	}
	entries, err := st.ListHeartbeatErrors(context.Background(), 100)
	if err != nil || len(entries) != len(cases) {
		t.Fatalf("SQL entries=%d want=%d err=%v", len(entries), len(cases), err)
	}
	persisted, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PATH_SECRET", "QUERY_SECRET", "HEADER_SECRET"} {
		if strings.Contains(output.String(), secret) || bytes.Contains(persisted, []byte(secret)) {
			t.Errorf("credential %s leaked to application or SQLite logs", secret)
		}
	}
	if !strings.Contains(output.String(), "https://monitor.test") || !strings.Contains(output.String(), "TLS verification failed") {
		t.Error("safe origin and actionable error class were not retained")
	}
}

func TestErrorPersistenceHasDeadlineAndDoesNotRecursivelyLog(t *testing.T) {
	output := captureHeartbeatLogs(t)
	var writes atomic.Int64
	var deadlineObserved atomic.Bool
	st := storageStub{save: func(ctx context.Context, entry store.HeartbeatLog) error {
		writes.Add(1)
		if _, ok := ctx.Deadline(); ok {
			deadlineObserved.Store(true)
		}
		<-ctx.Done()
		return errors.New("SQL_DRIVER_SECRET")
	}}
	m := New(testConfig(time.Second, 40*time.Millisecond, config.HeartbeatTarget{
		Name: "kuma", URL: "http://monitor.test/api/push/token", Mode: "kuma",
	}), st, nil, nil)
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return responseFor(r, http.StatusBadGateway, ""), nil
	})
	done := make(chan struct{})
	go func() {
		m.ping(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("error persistence exceeded its bounded write deadline")
	}
	metrics := m.MetricsSnapshot()[0]
	if writes.Load() != 1 || !deadlineObserved.Load() || metrics.Failures != 1 || metrics.LogFailures != 1 {
		t.Errorf("unbounded or recursive error handling: writes=%d deadline=%v metrics=%+v", writes.Load(), deadlineObserved.Load(), metrics)
	}
	if strings.Contains(output.String(), "SQL_DRIVER_SECRET") || !strings.Contains(output.String(), "heartbeat error persistence failed") {
		t.Errorf("unsafe or missing fallback log: %s", output.String())
	}
}

func TestSamplingAndPushTimeoutStillPersistAfterStorageRecovery(t *testing.T) {
	st := newTestStore(t)
	var recovered atomic.Bool
	var attemptedWrite atomic.Bool
	wrapped := storageStub{
		ready: func(ctx context.Context) error {
			// Model storage becoming available after the readiness wait has
			// already used the complete sampling budget.
			<-ctx.Done()
			recovered.Store(true)
			return ctx.Err()
		},
		save: func(ctx context.Context, entry store.HeartbeatLog) error {
			attemptedWrite.Store(true)
			if !recovered.Load() {
				return errors.New("storage has not recovered")
			}
			// This is an actual SQLite insert; a canceled round context would
			// lose the record despite the now-available database.
			return st.SaveHeartbeatError(ctx, entry)
		},
	}
	m := New(testConfig(time.Second, 40*time.Millisecond, config.HeartbeatTarget{
		Name: "kuma", URL: "http://monitor.test/api/push/token?status=up", Mode: "kuma",
	}), wrapped, nil, nil)
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	m.ping(context.Background())
	metrics := m.MetricsSnapshot()[0]
	if !attemptedWrite.Load() || metrics.Failures != 1 || metrics.LogFailures != 0 {
		t.Fatalf("delivery timeout prevented error persistence after recovery: %+v", metrics)
	}
	entries, err := st.ListHeartbeatErrors(context.Background(), 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("timeout error not persisted after storage recovery: entries=%d err=%v", len(entries), err)
	}
	if entries[0].Error != "timeout" || entries[0].Health != "fail" || entries[0].Target != "kuma" {
		t.Errorf("unexpected timeout audit entry: %+v", entries[0])
	}
}

func TestStopCancelsIndependentErrorPersistence(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan error, 1)
	st := storageStub{save: func(ctx context.Context, _ store.HeartbeatLog) error {
		close(entered)
		<-ctx.Done()
		finished <- ctx.Err()
		return ctx.Err()
	}}
	m := New(testConfig(time.Millisecond, 5*time.Second, config.HeartbeatTarget{
		Name: "kuma", URL: "http://monitor.test/api/push/token", Mode: "kuma",
	}), st, nil, nil)
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return responseFor(r, http.StatusBadGateway, ""), nil
	})
	m.Start(context.Background())
	defer m.Stop()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("error persistence did not start")
	}
	m.Stop()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("Stop did not cancel the independent log context: %v", err)
	}
}

func TestStopWaitsForCanceledRequestToFinish(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRequest := func() { releaseOnce.Do(func() { close(release) }) }
	m := New(testConfig(time.Millisecond, time.Second, config.HeartbeatTarget{
		Name: "kuma", URL: "http://monitor.test/api/push/token", Mode: "kuma",
	}), storageStub{}, nil, nil)
	m.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		return nil, r.Context().Err()
	})
	m.Stop() // Stop before Start is harmless.
	m.Start(context.Background())
	defer func() {
		releaseRequest()
		m.Stop()
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not start")
	}
	stopped := make(chan struct{})
	go func() {
		m.Stop()
		close(stopped)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel request")
	}
	select {
	case <-stopped:
		t.Error("Stop returned before request cleanup completed")
	case <-time.After(20 * time.Millisecond):
	}
	releaseRequest()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not wait for worker completion")
	}
	m.Stop()
	metrics := m.MetricsSnapshot()[0]
	if metrics.Attempts != 1 || metrics.Failures != 0 || metrics.LogFailures != 0 {
		t.Errorf("intentional shutdown counted as a delivery outage: %+v", metrics)
	}
}
