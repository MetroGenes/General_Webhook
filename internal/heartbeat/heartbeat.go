// Package heartbeat pushes a deadman heartbeat from the webhook process to
// external monitoring endpoints.
//
// The standard deployment publishes no host port and has no public reverse
// proxy, so external probes cannot reach /healthz or /readyz. Instead the
// process itself periodically POSTs a liveness/readiness signal outward to
// deadman monitors such as Healthchecks.io, Uptime Kuma push URLs or the
// Cloudflare Pages receiver used by the Runtime project. If the process, the
// queue workers or the store stop being healthy, the pushed status flips to
// "fail" (and push-mode targets receive url + "/fail"); if the whole host is
// down, the monitor stops hearing from us at all and alerts by timeout.
package heartbeat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

// Status values match the payload convention of the Runtime
// cloudflare-heartbeat receiver ("pass" / "fail").
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
)

// Payload is the structured body sent to every target. json-mode targets
// interpret payload.Status directly; push-mode targets instead signal failure
// by POSTing to url + "/fail" (Healthchecks.io convention) while carrying the
// same body for context.
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

// Monitor periodically pushes heartbeats to all configured targets.
type Monitor struct {
	cfg         config.HeartbeatConfig
	st          *store.Store
	ready       func() bool
	dropSummary func() string
	client      *http.Client
	now         func() time.Time
	startedAt   time.Time
	revision    string

	lastSuccess atomic.Int64 // unix seconds of last ping accepted by >=1 target; 0 = never

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup
}

// New builds a Monitor. ready reports whether the process is currently
// healthy (queue workers alive and the HTTP server not stopping); it is
// called once per ping, so it must not perform expensive work.
func New(cfg config.HeartbeatConfig, st *store.Store, ready func() bool, dropSummary func() string) *Monitor {
	if ready == nil {
		ready = func() bool { return true }
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Heartbeat pushes are trusted egress: never inherit HTTP_PROXY, and
	// never follow redirects (a cross-origin redirect would re-send the
	// X-Heartbeat-Token header to the redirect target).
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
	}
}

// Start launches the periodic push loop. The first ping fires after one
// interval, giving the process time to become ready; monitor grace periods
// should therefore exceed interval + expected restart time.
func (m *Monitor) Start(ctx context.Context) {
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.startedAt = m.now()
	m.done = make(chan struct{})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(m.done)
		m.loop(m.ctx)
	}()
}

// Stop cancels the loop and waits for any in-flight ping (bounded by the
// per-target timeout) to settle. No "fail" ping is sent on shutdown: deadman
// grace periods absorb the restart, and a false /fail would alert on every
// deploy.
func (m *Monitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.done != nil {
		<-m.done
		m.wg.Wait()
	}
}

// LastSuccess returns the unix seconds of the last ping accepted by at least
// one target, or 0 if no ping has succeeded yet.
func (m *Monitor) LastSuccess() int64 { return m.lastSuccess.Load() }

func (m *Monitor) loop(ctx context.Context) {
	interval := m.cfg.Interval.Duration
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.ping(ctx)
		}
	}
}

func (m *Monitor) ping(ctx context.Context) {
	now := m.now()
	stats, statsErr := m.st.Stats(ctx, now)
	pass := statsErr == nil && m.ready()
	status, exitCode := StatusPass, 0
	if !pass {
		status, exitCode = StatusFail, 1
	}
	payload := Payload{
		Host:          m.cfg.Host,
		Status:        status,
		ExitCode:      exitCode,
		Timestamp:     now.UTC(),
		UptimeSeconds: int64(now.Sub(m.startedAt).Seconds()),
		Revision:      m.revision,
	}
	if statsErr == nil {
		payload.Stats = stats
	}
	if m.dropSummary != nil {
		payload.DropSummary = m.dropSummary()
	}

	ok := 0
	for _, target := range m.cfg.Targets {
		if err := m.pushTarget(ctx, target, payload); err != nil {
			slog.Error("heartbeat push failed", "target", target.Name, "status", status, "err", err.Error())
			continue
		}
		ok++
	}
	if ok > 0 {
		m.lastSuccess.Store(now.Unix())
		slog.Info("heartbeat pushed", "status", status, "targets_ok", ok, "targets_total", len(m.cfg.Targets))
		return
	}
	slog.Error("heartbeat push failed", "status", status, "targets_ok", 0, "targets_total", len(m.cfg.Targets))
}

func (m *Monitor) pushTarget(parent context.Context, target config.HeartbeatTarget, payload Payload) error {
	ctx, cancel := context.WithTimeout(parent, m.cfg.Timeout.Duration)
	defer cancel()

	endpoint := target.URL
	if target.Mode == "push" && payload.Status != StatusPass {
		endpoint = strings.TrimRight(endpoint, "/") + "/fail"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if target.Token != "" {
		req.Header.Set("X-Heartbeat-Token", target.Token)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		// auditURLTarget keeps only scheme://host[:port] so a push token in
		// the URL path never reaches the logs.
		return fmt.Errorf("request %s: %w", auditURLTarget(endpoint), err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s responded %d", auditURLTarget(endpoint), resp.StatusCode)
	}
	return nil
}

// auditURLTarget deliberately excludes path, query and fragment: deadman push
// URLs (Healthchecks, Uptime Kuma) carry a per-check token in the path.
func auditURLTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "invalid-url"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host
}
