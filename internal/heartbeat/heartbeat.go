// Package heartbeat actively pushes readiness signals to external deadman
// monitors. Uptime Kuma is the default protocol; Healthchecks and JSON
// receivers are also supported. A stopped process is detected by the remote
// monitor's missing-heartbeat deadline.
package heartbeat

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

// Status is the readiness value in the JSON payload. Protocol adapters map it
// to Kuma's up/down query or Healthchecks' success/fail path.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"

	maxResponseBytes = 4 << 10
)

// Storage is the subset of the persistent store needed by heartbeat. ReadyCheck
// must test writes, and all methods must honor their context deadlines.
type Storage interface {
	ReadyCheck(context.Context) error
	Stats(context.Context, time.Time) (*store.QueueStats, error)
	SaveHeartbeatError(context.Context, store.HeartbeatLog) error
}

// Payload is sent as JSON to every target. Kuma uses query.status for readiness;
// Healthchecks uses the URL path; JSON receivers inspect Status directly.
type Payload struct {
	Host          string            `json:"host"`
	Status        Status            `json:"status"`
	ExitCode      int               `json:"exit_code"`
	Timestamp     time.Time         `json:"timestamp"`
	UptimeSeconds int64             `json:"uptime_seconds"`
	Revision      string            `json:"revision,omitempty"`
	Stats         *store.QueueStats `json:"stats,omitempty"`
	DropSummary   string            `json:"drop_summary,omitempty"`
}

// TargetMetrics describes delivery to one configured target. LastSuccess is
// receipt of either a healthy or an unhealthy signal, not proof of health.
// Times are Unix seconds; zero means that the corresponding event has not
// occurred. Counters last for the lifetime of this Monitor.
type TargetMetrics struct {
	Name           string
	Mode           string
	Attempts       uint64
	Failures       uint64
	LogFailures    uint64
	LastAttempt    int64
	LastSuccess    int64
	LastFailure    int64
	LastDurationMS int64
	LastHTTPStatus int
	LastHealth     Status
}

// Monitor gives each target its own ticker and worker. Configuration validation
// bounds the target count to 16; each target has at most one in-flight push.
type Monitor struct {
	cfg         config.HeartbeatConfig
	st          Storage
	ready       func() bool
	dropSummary func() string
	client      *http.Client
	now         func() time.Time
	startedAt   time.Time
	revision    string

	lastSuccess atomic.Int64
	metricsMu   sync.RWMutex
	metrics     []TargetMetrics

	lifecycleMu sync.Mutex
	started     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

// New builds a Monitor. ready and dropSummary must be inexpensive,
// concurrency-safe callbacks. Database readiness also requires a successful
// write probe and queue-statistics query within the sampling deadline.
func New(cfg config.HeartbeatConfig, st Storage, ready func() bool, dropSummary func() string) *Monitor {
	if ready == nil {
		ready = func() bool { return true }
	}
	if cfg.Interval.Duration <= 0 {
		cfg.Interval.Duration = time.Minute
	}
	if cfg.Timeout.Duration <= 0 {
		cfg.Timeout.Duration = 10 * time.Second
	}
	cfg.Targets = append([]config.HeartbeatTarget(nil), cfg.Targets...)
	metrics := make([]TargetMetrics, len(cfg.Targets))
	for i := range cfg.Targets {
		if cfg.Targets[i].Mode == "" {
			cfg.Targets[i].Mode = "kuma"
		}
		metrics[i] = TargetMetrics{Name: cfg.Targets[i].Name, Mode: cfg.Targets[i].Mode}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Trusted, administrator-controlled egress must not inherit HTTP_PROXY or
	// forward URL/header credentials to a redirect destination.
	transport.Proxy = nil
	client := &http.Client{
		Timeout:   cfg.Timeout.Duration,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Monitor{
		cfg:         cfg,
		st:          st,
		ready:       ready,
		dropSummary: dropSummary,
		client:      client,
		now:         time.Now,
		startedAt:   time.Now(),
		revision:    os.Getenv("GENERAL_WEBHOOK_GIT_COMMIT"),
		metrics:     metrics,
	}
}

// Start launches independent target loops once. The first push is after one
// interval, so a monitor's grace period must include startup time and interval.
func (m *Monitor) Start(ctx context.Context) {
	m.lifecycleMu.Lock()
	if m.started {
		m.lifecycleMu.Unlock()
		return
	}
	m.started = true
	ctx, m.cancel = context.WithCancel(ctx)
	m.startedAt = m.now()
	m.done = make(chan struct{})
	done := m.done
	m.lifecycleMu.Unlock()
	go func() {
		var wg sync.WaitGroup
		for i := range m.cfg.Targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.targetLoop(ctx, i)
			}()
		}
		wg.Wait()
		m.client.CloseIdleConnections()
		close(done)
	}()
}

// Stop cancels requests and sampling, then waits for every target worker. It is
// safe to call repeatedly. Intentional shutdown cancellation is not logged as
// a push failure and sends no extra down signal during routine deployments.
func (m *Monitor) Stop() {
	m.lifecycleMu.Lock()
	cancel, done := m.cancel, m.done
	m.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// LastSuccess returns the last accepted push to any target, including down
// signals. Per-target delivery failures are available from MetricsSnapshot.
func (m *Monitor) LastSuccess() int64 { return m.lastSuccess.Load() }

// MetricsSnapshot returns copies in configuration order without URL credentials.
func (m *Monitor) MetricsSnapshot() []TargetMetrics {
	m.metricsMu.RLock()
	defer m.metricsMu.RUnlock()
	return append([]TargetMetrics(nil), m.metrics...)
}

func (m *Monitor) targetLoop(ctx context.Context, index int) {
	t := time.NewTicker(m.cfg.Interval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tickTarget(ctx, index)
		}
	}
}

func (m *Monitor) tickTarget(ctx context.Context, index int) {
	// A sample and delivery may each use one timeout. A timed-out sample still
	// sends down using a live delivery context. Error persistence has its own
	// short budget, so the entire target tick is bounded by 2*timeout plus
	// min(timeout, 1s), even if sampling and delivery both exhaust their budget.
	roundCtx, cancel := context.WithTimeout(ctx, 2*m.cfg.Timeout.Duration)
	defer cancel()
	payload := m.sample(roundCtx)
	m.deliver(ctx, roundCtx, index, payload)
}

func (m *Monitor) sample(parent context.Context) Payload {
	ctx, cancel := context.WithTimeout(parent, m.cfg.Timeout.Duration)
	defer cancel()
	now := m.now()
	var stats *store.QueueStats
	pass := false
	if m.st != nil {
		writeErr := m.st.ReadyCheck(ctx)
		var statsErr error
		stats, statsErr = m.st.Stats(ctx, now)
		pass = writeErr == nil && statsErr == nil && ctx.Err() == nil && m.ready()
		if statsErr != nil {
			stats = nil
		}
	}
	status, exitCode := StatusPass, 0
	if !pass {
		status, exitCode = StatusFail, 1
	}
	m.lifecycleMu.Lock()
	startedAt := m.startedAt
	m.lifecycleMu.Unlock()
	uptime := max(int64(now.Sub(startedAt).Seconds()), 0)
	payload := Payload{
		Host:          m.cfg.Host,
		Status:        status,
		ExitCode:      exitCode,
		Timestamp:     now.UTC(),
		UptimeSeconds: uptime,
		Revision:      m.revision,
		Stats:         stats,
	}
	if m.dropSummary != nil {
		payload.DropSummary = m.dropSummary()
	}
	return payload
}

func (m *Monitor) deliver(monitorCtx, roundCtx context.Context, index int, payload Payload) {
	if monitorCtx.Err() != nil {
		return
	}
	target := m.cfg.Targets[index]
	attemptedAt := m.now()
	m.metricsMu.Lock()
	m.metrics[index].Attempts++
	m.metrics[index].LastAttempt = attemptedAt.Unix()
	m.metrics[index].LastHealth = payload.Status
	m.metricsMu.Unlock()

	start := time.Now()
	httpStatus, err := m.pushTarget(roundCtx, target, payload)
	durationMS := time.Since(start).Milliseconds()
	completedAt := m.now()
	m.metricsMu.Lock()
	m.metrics[index].LastDurationMS = durationMS
	m.metrics[index].LastHTTPStatus = httpStatus
	if err == nil {
		m.metrics[index].LastSuccess = completedAt.Unix()
	} else if monitorCtx.Err() == nil {
		m.metrics[index].Failures++
		m.metrics[index].LastFailure = completedAt.Unix()
	}
	m.metricsMu.Unlock()
	if err == nil {
		// Concurrent target completions must not move the aggregate backwards.
		for previous := m.lastSuccess.Load(); completedAt.Unix() > previous; previous = m.lastSuccess.Load() {
			if m.lastSuccess.CompareAndSwap(previous, completedAt.Unix()) {
				break
			}
		}
		slog.Info("heartbeat pushed", "target", target.Name, "mode", target.Mode, "health", payload.Status)
		return
	}
	if monitorCtx.Err() != nil {
		return
	}
	origin := auditURLTarget(target.URL)
	// pushTarget only returns classified messages. Never log raw transport
	// errors, request URLs, response bodies or token header values here.
	slog.Error("heartbeat push failed", "target", target.Name, "origin", origin,
		"mode", target.Mode, "health", payload.Status, "http_status", httpStatus, "err", err.Error())
	entry := store.HeartbeatLog{
		Target: target.Name, Origin: origin, Mode: target.Mode, Health: string(payload.Status),
		Error: err.Error(), HTTPStatus: httpStatus, DurationMS: durationMS, CreatedAt: completedAt.UTC(),
	}
	// Do not inherit the exhausted sampling/delivery budget: the database may
	// have recovered while the HTTP request timed out. Shutdown still cancels
	// this independent write through the monitor's lifecycle context.
	logCtx, cancel := context.WithTimeout(monitorCtx, min(m.cfg.Timeout.Duration, time.Second))
	defer cancel()
	logErr := errors.New("storage unavailable")
	if m.st != nil {
		logErr = m.st.SaveHeartbeatError(logCtx, entry)
	}
	if logErr != nil {
		m.metricsMu.Lock()
		m.metrics[index].LogFailures++
		m.metricsMu.Unlock()
		slog.Error("heartbeat error persistence failed", "target", target.Name, "origin", origin,
			"err", safeErrorClass(logErr))
	}
}

func (m *Monitor) pushTarget(parent context.Context, target config.HeartbeatTarget, payload Payload) (int, error) {
	ctx, cancel := context.WithTimeout(parent, m.cfg.Timeout.Duration)
	defer cancel()
	endpoint, err := targetEndpoint(target, payload.Status)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, errors.New("invalid heartbeat payload")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("invalid heartbeat request")
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Token != "" {
		req.Header.Set("X-Heartbeat-Token", target.Token)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		// In particular, *url.Error.Error includes the complete URL. Even its
		// nested cause can contain credentials, so do not wrap or print it.
		return 0, errors.New(safeErrorClass(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp.StatusCode, fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	if target.Mode == "kuma" || target.Mode == "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil {
			return resp.StatusCode, errors.New(safeErrorClass(err))
		}
		if len(body) > maxResponseBytes {
			return resp.StatusCode, errors.New("Kuma response exceeds size limit")
		}
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return resp.StatusCode, errors.New("invalid Kuma response")
		}
		if !result.OK {
			return resp.StatusCode, errors.New("Kuma rejected heartbeat")
		}
	} else if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes)); err != nil {
		return resp.StatusCode, errors.New(safeErrorClass(err))
	}
	return resp.StatusCode, nil
}

func targetEndpoint(target config.HeartbeatTarget, status Status) (string, error) {
	u, err := url.Parse(target.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid heartbeat target URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("invalid heartbeat target URL scheme")
	}
	switch target.Mode {
	case "", "kuma":
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return "", errors.New("invalid heartbeat target query")
		}
		if status == StatusPass {
			query.Set("status", "up")
			query.Set("msg", "OK")
		} else {
			query.Set("status", "down")
			query.Set("msg", "not ready")
		}
		u.RawQuery = query.Encode()
	case "push", "healthchecks":
		if status != StatusPass {
			// Modify the escaped path, preserving encoded token bytes and query
			// parameters; appending to the original URL corrupts query values.
			u.RawPath = strings.TrimRight(u.EscapedPath(), "/") + "/fail"
			u.Path, err = url.PathUnescape(u.RawPath)
			if err != nil {
				return "", errors.New("invalid heartbeat target path")
			}
		}
	case "json":
	default:
		return "", errors.New("invalid heartbeat target mode")
	}
	return u.String(), nil
}

// safeErrorClass inspects types, never error strings: DNS/TLS/network errors
// can contain URL paths, query values or other credential-bearing text.
func safeErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS lookup failed"
	}
	var tlsErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certificateErr x509.CertificateInvalidError
	if errors.As(err, &tlsErr) || errors.As(err, &authorityErr) || errors.As(err, &hostnameErr) || errors.As(err, &certificateErr) {
		return "TLS verification failed"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	return "operation failed"
}

// auditURLTarget excludes userinfo, path, query and fragment. Push URL tokens
// are secrets and may never be included in either SQLite or application logs.
func auditURLTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" {
		return "invalid-url"
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "invalid-url"
	}
	return scheme + "://" + u.Host
}
