package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/auth"
	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/logger"
	"github.com/MetroGenes/General_Webhook/internal/queue"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

const maxBodyBytes = 1 << 20

const (
	defaultReplaySkew   = 5 * time.Minute
	defaultReplayTTL    = 10 * time.Minute
	defaultRateBurst    = 30
	defaultGlobalBurst  = 120
	maxDeliveryIDLength = 128
)

var deliveryIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

type Server struct {
	cfg      *config.Config
	q        *queue.Queue
	st       *store.Store
	limiter  *rateLimiter
	global   *rateLimiter
	ops      *rateLimiter
	budget   *inFlightBudget
	trusted  *trustedNets
	now      func() time.Time
	stopping atomic.Bool
}

func New(cfg *config.Config, q *queue.Queue, st *store.Store) *Server {
	maxInFlight := 16
	maxBytes := 32 << 20
	var proxies []string
	if cfg != nil {
		if cfg.Server.MaxInFlight > 0 {
			maxInFlight = cfg.Server.MaxInFlight
		}
		if cfg.Server.MaxInFlightBytes > 0 {
			maxBytes = cfg.Server.MaxInFlightBytes
		}
		proxies = cfg.Server.TrustedProxies
		if cfg.Admin.Header == "" {
			cfg.Admin.Header = "Authorization"
		}
	}
	return &Server{
		cfg:     cfg,
		q:       q,
		st:      st,
		limiter: newRateLimiter(defaultRateBurst, float64(defaultRateBurst)/60),
		global:  newRateLimiter(defaultGlobalBurst, float64(defaultGlobalBurst)/60),
		ops:     newRateLimiter(60, 1),
		budget:  newInFlightBudget(maxInFlight, maxBytes),
		trusted: parseTrustedProxies(proxies),
		now:     time.Now,
	}
}

func (s *Server) SetStopping() { s.stopping.Store(true) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /webhook/healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /webhook/readyz", s.ready)
	mux.HandleFunc("POST /webhook/{source}", s.webhook)
	// Fail closed: without an admin token these routes do not exist.
	if s.cfg != nil && s.cfg.Admin.Token != "" {
		mux.HandleFunc("GET /events/{id}", s.requireAdmin(s.getEvent))
		mux.HandleFunc("POST /events/{id}/retry", s.requireAdmin(s.retryEvent))
		mux.HandleFunc("GET /queue/stats", s.requireAdmin(s.queueStats))
		mux.HandleFunc("GET /metrics", s.requireAdmin(s.metrics))
	}
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.stopping.Load() {
		http.Error(w, "stopping", http.StatusServiceUnavailable)
		return
	}
	if s.q == nil || !s.q.Ready() {
		http.Error(w, "workers not ready", http.StatusServiceUnavailable)
		return
	}
	if err := s.st.ReadyCheck(r.Context()); err != nil {
		http.Error(w, "store not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("source")
	src := s.cfg.SourceByName(name)
	if src == nil {
		http.Error(w, "unknown source", http.StatusNotFound)
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	now := s.now()
	if !s.global.allow("global", now) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if r.ContentLength > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	reserve := int64(maxBodyBytes)
	if r.ContentLength > 0 {
		reserve = r.ContentLength
	}
	if !s.budget.acquire(reserve) {
		http.Error(w, "server busy", http.StatusServiceUnavailable)
		return
	}
	defer s.budget.release(reserve)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	id, err := newID()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	ctx := logger.WithTrace(r.Context(), id)
	log := logger.From(ctx)
	if err := auth.Verify(src.Auth, r, body); err != nil {
		log.Warn("auth failed", "source", name, "reason", err.Error())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ip := clientIP(r, s.trusted)
	if !s.limiter.allow(name+"|"+ip, now) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if !json.Valid(body) {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if deliveryID == "" {
		deliveryID = strings.TrimSpace(r.Header.Get("X-Delivery-Id"))
	}
	if deliveryID != "" {
		if err := validateDeliveryID(deliveryID); err != nil {
			http.Error(w, "invalid delivery id", http.StatusBadRequest)
			return
		}
	}
	tsHeader := strings.TrimSpace(r.Header.Get("X-Webhook-Timestamp"))
	if src.Auth.Type == "hmac" && (deliveryID == "" || tsHeader == "") {
		log.Warn("missing delivery id or timestamp", "source", name)
		http.Error(w, "missing delivery id or timestamp", http.StatusUnauthorized)
		return
	}
	if tsHeader != "" {
		if err := validateReplayHeaders(r, now, defaultReplaySkew); err != nil {
			log.Warn("replay rejected", "source", name, "reason", err.Error())
			http.Error(w, "replay rejected", http.StatusUnauthorized)
			return
		}
	}
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	actionsJSON, err := config.SnapshotSource(src)
	if err != nil {
		log.Error("snapshot source", "err", err)
		http.Error(w, "save event", http.StatusInternalServerError)
		return
	}
	configHash := config.ConfigHash(s.cfg)
	var saveErr error
	if src.Auth.Type == "hmac" {
		saveErr = s.st.SavePendingEventGuarded(ctx, id, name, ip, body, now, deliveryID, bodyHash,
			configHash, actionsJSON, now.Add(defaultReplayTTL))
	} else {
		saveErr = s.st.SavePendingEvent(ctx, id, name, ip, body, now, deliveryID, bodyHash, configHash, actionsJSON)
	}
	if saveErr != nil {
		switch {
		case errors.Is(saveErr, store.ErrDuplicateBody):
			log.Warn("duplicate body hash", "source", name)
			http.Error(w, "duplicate body", http.StatusConflict)
		case errors.Is(saveErr, store.ErrDuplicateEvent):
			log.Warn("duplicate delivery", "source", name, "delivery_id", deliveryID)
			http.Error(w, "duplicate delivery", http.StatusConflict)
		default:
			log.Error("save event", "err", saveErr)
			http.Error(w, "save event", http.StatusInternalServerError)
		}
		return
	}
	s.q.Wake()
	log.Info("webhook received", "source", name, "remote_ip", ip, "bytes", len(body))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"event_id": id, "status": "accepted"})
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ev, err := s.st.GetEvent(r.Context(), id)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if ev == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	logs, err := s.st.ListActionLogs(r.Context(), id, 20)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	states, err := s.st.ListActionStates(r.Context(), id)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	type logView struct {
		Action           string `json:"action"`
		ActionIndex      int    `json:"action_index"`
		ReplayGeneration int    `json:"replay_generation"`
		Target           string `json:"target"`
		Attempt          int    `json:"attempt"`
		Success          bool   `json:"success"`
		Detail           string `json:"detail"`
		DurationMS       int64  `json:"duration_ms"`
		CreatedAt        string `json:"created_at"`
	}
	outLogs := make([]logView, 0, len(logs))
	for _, entry := range logs {
		outLogs = append(outLogs, logView{
			Action: entry.Action, ActionIndex: entry.ActionIndex, ReplayGeneration: entry.ReplayGeneration,
			Target: entry.Target, Attempt: entry.Attempt, Success: entry.Success, Detail: entry.Detail,
			DurationMS: entry.DurationMS, CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	resp := map[string]any{
		"event_id": ev.ID, "source": ev.Source, "remote_ip": ev.RemoteIP, "status": ev.Status,
		"received_at": ev.ReceivedAt.UTC().Format(time.RFC3339Nano), "external_delivery_id": ev.ExternalDeliveryID,
		"config_hash": ev.ConfigHash, "replay_generation": ev.ReplayGeneration, "actions": states, "action_logs": outLogs,
	}
	if ev.ProcessedAt != nil {
		resp["processed_at"] = ev.ProcessedAt.UTC().Format(time.RFC3339Nano)
	}
	if ev.ReplayReason != "" {
		resp["replay_reason"] = ev.ReplayReason
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) retryEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason, err := readReplayReason(r)
	if err != nil {
		http.Error(w, "invalid replay request", http.StatusBadRequest)
		return
	}
	reason = logger.RedactSensitive(reason)
	generation, err := s.st.ReplayDeadEvent(r.Context(), id, reason, s.now())
	if err != nil {
		if errors.Is(err, store.ErrReplayUnavailable) {
			http.Error(w, "event replay input unavailable", http.StatusConflict)
		} else {
			http.Error(w, "event not retryable", http.StatusConflict)
		}
		return
	}
	s.q.Wake()
	logger.From(r.Context()).Warn("event manually replayed", "event_id", id, "generation", generation,
		"remote_ip", clientIP(r, s.trusted), "reason", reason)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"event_id": id, "status": "pending", "generation": generation})
}

func (s *Server) queueStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.st.Stats(r.Context(), s.now())
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	stats, err := s.st.Stats(r.Context(), s.now())
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, "# HELP general_webhook_events Current events by durable state.\n")
	_, _ = io.WriteString(w, "# TYPE general_webhook_events gauge\n")
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"pending\"} %d\n", stats.Pending)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"processing\"} %d\n", stats.Processing)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"retrying\"} %d\n", stats.Retrying)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"dead\"} %d\n", stats.Dead)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"partial\"} %d\n", stats.Partial)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"error\"} %d\n", stats.Error)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"done\"} %d\n", stats.Done)
	_, _ = fmt.Fprintf(w, "general_webhook_events{status=\"skipped\"} %d\n", stats.Skipped)
	_, _ = io.WriteString(w, "# HELP general_webhook_actions Current actions by durable retry state.\n")
	_, _ = io.WriteString(w, "# TYPE general_webhook_actions gauge\n")
	_, _ = fmt.Fprintf(w, "general_webhook_actions{status=\"retrying\"} %d\n", stats.ActionsRetrying)
	_, _ = fmt.Fprintf(w, "general_webhook_actions{status=\"dead\"} %d\n", stats.ActionsDead)
	_, _ = io.WriteString(w, "# HELP general_webhook_action_attempts Action attempts in a rolling window.\n")
	_, _ = io.WriteString(w, "# TYPE general_webhook_action_attempts gauge\n")
	_, _ = fmt.Fprintf(w, "general_webhook_action_attempts{window=\"5m\",result=\"all\"} %d\n", stats.ActionAttempts5M)
	_, _ = fmt.Fprintf(w, "general_webhook_action_attempts{window=\"5m\",result=\"failure\"} %d\n", stats.ActionFailures5M)
	_, _ = fmt.Fprintf(w, "general_webhook_action_attempts{window=\"1h\",result=\"all\"} %d\n", stats.ActionAttempts1H)
	_, _ = fmt.Fprintf(w, "general_webhook_action_attempts{window=\"1h\",result=\"failure\"} %d\n", stats.ActionFailures1H)
	_, _ = io.WriteString(w, "# HELP general_webhook_action_failure_ratio Failed attempts divided by all attempts in a rolling window.\n")
	_, _ = io.WriteString(w, "# TYPE general_webhook_action_failure_ratio gauge\n")
	_, _ = fmt.Fprintf(w, "general_webhook_action_failure_ratio{window=\"5m\"} %.9g\n", stats.ActionFailureRate5M)
	_, _ = fmt.Fprintf(w, "general_webhook_action_failure_ratio{window=\"1h\"} %.9g\n", stats.ActionFailureRate1H)
	_, _ = io.WriteString(w, "# HELP general_webhook_oldest_pending_age_milliseconds Age of the oldest unfinished event.\n")
	_, _ = io.WriteString(w, "# TYPE general_webhook_oldest_pending_age_milliseconds gauge\n")
	_, _ = fmt.Fprintf(w, "general_webhook_oldest_pending_age_milliseconds %d\n", stats.OldestPendingAgeMS)
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.trusted)
		if !s.ops.allow(ip, s.now()) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		got := strings.TrimSpace(r.Header.Get(s.cfg.Admin.Header))
		if strings.EqualFold(s.cfg.Admin.Header, "Authorization") {
			parts := strings.Fields(got)
			if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
				got = parts[1]
			} else {
				got = ""
			}
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Admin.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func readReplayReason(r *http.Request) (string, error) {
	if r.Body == nil || r.ContentLength == 0 {
		return "manual replay", nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(body) > 1024 {
		return "", errString("replay request too large")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return "manual replay", nil
	}
	var input struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		return "", err
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if input.Reason == "" {
		input.Reason = "manual replay"
	}
	if len(input.Reason) > 256 {
		return "", errString("replay reason too long")
	}
	return input.Reason, nil
}

func validateDeliveryID(id string) error {
	if len(id) > maxDeliveryIDLength || !deliveryIDRe.MatchString(id) {
		return errInvalidDeliveryID
	}
	return nil
}

func isJSONContentType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return false
	}
	media := strings.TrimSpace(strings.Split(value, ";")[0])
	return media == "application/json" || strings.HasSuffix(media, "+json")
}

func validateReplayHeaders(r *http.Request, now time.Time, skew time.Duration) error {
	raw := strings.TrimSpace(r.Header.Get("X-Webhook-Timestamp"))
	if raw == "" {
		return errInvalidTimestamp
	}
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errInvalidTimestamp
	}
	delta := now.Sub(time.Unix(seconds, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > skew {
		return errStaleTimestamp
	}
	return nil
}

var (
	errInvalidTimestamp  = errString("invalid timestamp")
	errStaleTimestamp    = errString("stale timestamp")
	errInvalidDeliveryID = errString("invalid delivery id")
)

type errString string

func (e errString) Error() string { return string(e) }

func newID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(bytes), nil
}
