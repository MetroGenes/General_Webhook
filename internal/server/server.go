package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
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

const maxBodyBytes = 5 << 20 // 5MB

const (
	defaultReplaySkew   = 5 * time.Minute
	defaultReplayTTL    = 10 * time.Minute
	defaultRateBurst    = 30
	defaultGlobalBurst  = 120
	maxDeliveryIDLength = 128
)

var deliveryIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

// Server 是 HTTP 接入层。
type Server struct {
	cfg      *config.Config
	q        *queue.Queue
	st       *store.Store
	limiter  *rateLimiter
	global   *rateLimiter
	delivery *deliveryCache
	trusted  *trustedNets
	now      func() time.Time
	stopping atomic.Bool
}

func New(cfg *config.Config, q *queue.Queue, st *store.Store) *Server {
	return &Server{
		cfg:      cfg,
		q:        q,
		st:       st,
		limiter:  newRateLimiter(defaultRateBurst, float64(defaultRateBurst)/60),
		global:   newRateLimiter(defaultGlobalBurst, float64(defaultGlobalBurst)/60),
		delivery: newDeliveryCache(defaultReplayTTL),
		trusted:  parseTrustedProxies(cfg.Server.TrustedProxies),
		now:      time.Now,
	}
}

// SetStopping marks the server as not ready (used during shutdown).
func (s *Server) SetStopping() {
	s.stopping.Store(true)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /webhook/healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /webhook/readyz", s.ready)
	mux.HandleFunc("POST /webhook/{source}", s.webhook)
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

	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
	now := s.now()
	if !s.global.allow("global|"+remoteHost, now) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	if r.ContentLength > maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

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

	if err := validateReplayHeaders(r, now, defaultReplaySkew); err != nil {
		log.Warn("replay rejected", "source", name, "reason", err.Error())
		http.Error(w, "replay rejected", http.StatusUnauthorized)
		return
	}

	if src.Auth.Type == "hmac" && deliveryID == "" && strings.TrimSpace(r.Header.Get("X-Webhook-Timestamp")) == "" {
		log.Warn("missing delivery id or timestamp", "source", name)
		http.Error(w, "missing delivery id or timestamp", http.StatusUnauthorized)
		return
	}

	if deliveryID != "" && !s.delivery.claim(name+"|"+deliveryID, now) {
		log.Warn("duplicate delivery", "source", name, "delivery_id", deliveryID)
		http.Error(w, "duplicate delivery", http.StatusConflict)
		return
	}

	// Always persist payload until processing finishes; retain_payload=false clears it after.
	if err := s.st.SavePendingEvent(ctx, id, name, ip, body, now, deliveryID, ""); err != nil {
		if errors.Is(err, store.ErrDuplicateEvent) {
			log.Warn("duplicate delivery", "source", name, "delivery_id", deliveryID)
			http.Error(w, "duplicate delivery", http.StatusConflict)
			return
		}
		log.Error("save event", "err", err)
		http.Error(w, "save event", http.StatusInternalServerError)
		return
	}

	s.q.Wake()

	log.Info("webhook received", "source", name, "remote_ip", ip, "bytes", len(body))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"event_id": id,
		"status":   "accepted",
	})
}

func validateDeliveryID(id string) error {
	if len(id) > maxDeliveryIDLength {
		return errInvalidDeliveryID
	}
	if !deliveryIDRe.MatchString(id) {
		return errInvalidDeliveryID
	}
	return nil
}

func isJSONContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if ct == "" {
		return false
	}
	media := strings.TrimSpace(strings.Split(ct, ";")[0])
	return media == "application/json" || strings.HasSuffix(media, "+json")
}

func validateReplayHeaders(r *http.Request, now time.Time, skew time.Duration) error {
	raw := strings.TrimSpace(r.Header.Get("X-Webhook-Timestamp"))
	if raw == "" {
		return nil
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errInvalidTimestamp
	}
	ts := time.Unix(sec, 0)
	delta := now.Sub(ts)
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

// newID 生成时间有序且唯一的事件 ID, 兼作 trace_id。
func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102T150405.000") + "-" + hex.EncodeToString(b), nil
}
