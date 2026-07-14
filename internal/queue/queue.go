package queue

import (
	"context"
	"sync"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/handler"
	"github.com/MetroGenes/General_Webhook/internal/logger"
	"github.com/MetroGenes/General_Webhook/internal/parser"
	"github.com/MetroGenes/General_Webhook/internal/router"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

const (
	defaultHTTPActionTimeout = 15 * time.Second
	defaultExecTimeout       = 60 * time.Second
	leaseSafetyMargin        = 2 * time.Minute
)

// Event is a wake hint for a durable outbox row (payload lives in the store).
type Event struct {
	ID         string
	Source     *config.Source
	ReceivedAt time.Time
}

// Queue is a durable outbox worker pool with an in-memory wake channel.
type Queue struct {
	wake       chan struct{}
	workers    int
	maxRetries int
	lease      time.Duration
	reg        *handler.Registry
	st         *store.Store
	cfg        *config.Config
	wg         sync.WaitGroup
	mu         sync.Mutex
	stopped    bool
	stopCh     chan struct{}
}

func New(cfg config.QueueConfig, reg *handler.Registry, st *store.Store, sources *config.Config) *Queue {
	buf := cfg.Buffer
	if buf <= 0 {
		buf = 1
	}
	retries := cfg.MaxRetries
	if retries <= 0 {
		retries = 3
	}
	return &Queue{
		wake:       make(chan struct{}, buf),
		workers:    cfg.Workers,
		maxRetries: retries,
		lease:      computeLease(sources, retries),
		reg:        reg,
		st:         st,
		cfg:        sources,
		stopCh:     make(chan struct{}),
	}
}

// computeLease covers the worst-case action duration including retries and backoff, plus margin.
// It must be at least as long as any configured exec timeout so long jobs are not double-claimed.
func computeLease(cfg *config.Config, maxRetries int) time.Duration {
	maxAction := defaultHTTPActionTimeout
	if cfg != nil {
		for _, src := range cfg.Sources {
			for _, ac := range src.Actions {
				t := actionTimeout(ac)
				if t > maxAction {
					maxAction = t
				}
			}
		}
	}
	if maxRetries < 1 {
		maxRetries = 1
	}
	// Linear backoff used by runAction: 1s + 2s + ... + (n-1)s
	var backoff time.Duration
	for i := 1; i < maxRetries; i++ {
		backoff += time.Duration(i) * time.Second
	}
	lease := time.Duration(maxRetries)*maxAction + backoff + leaseSafetyMargin
	if lease < 5*time.Minute {
		lease = 5 * time.Minute
	}
	return lease
}

func actionTimeout(ac config.ActionConfig) time.Duration {
	switch ac.Type {
	case "exec":
		if ac.Timeout.Duration > 0 {
			return ac.Timeout.Duration
		}
		return defaultExecTimeout
	default:
		return defaultHTTPActionTimeout
	}
}

// Start launches workers that claim pending outbox jobs.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			q.worker(ctx)
		}()
	}
	q.Wake()
}

func (q *Queue) worker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.stopCh:
			return
		case <-q.wake:
			q.drain(ctx)
		case <-ticker.C:
			q.drain(ctx)
		}
	}
}

func (q *Queue) drain(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.stopCh:
			return
		default:
		}
		if q.isStopped() {
			return
		}
		_, _ = q.st.RequeueExpiredLeases(ctx, time.Now())
		ev, err := q.st.ClaimPending(ctx, q.lease, time.Now())
		if err != nil {
			logger.From(ctx).Error("claim pending", "err", err)
			return
		}
		if ev == nil {
			return
		}
		src := q.cfg.SourceByName(ev.Source)
		if src == nil {
			logger.From(ctx).Error("unknown source for pending event", "source", ev.Source, "id", ev.ID)
			_ = q.st.UpdateEventStatus(context.WithoutCancel(ctx), ev.ID, "error", time.Now())
			continue
		}
		q.process(ctx, &Event{ID: ev.ID, Source: src, ReceivedAt: ev.ReceivedAt}, ev.Payload, ev.ExternalDeliveryID)
	}
}

// Wake signals workers that new work may be available. Safe after Stop (no-op).
func (q *Queue) Wake() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return false
	}
	select {
	case q.wake <- struct{}{}:
		return true
	default:
		return true
	}
}

// Enqueue is an alias for Wake kept for compatibility with call sites that still
// have an Event; the durable row must already exist.
func (q *Queue) Enqueue(ev *Event) bool {
	_ = ev
	return q.Wake()
}

func (q *Queue) isStopped() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stopped
}

// Stop stops claiming and waits for in-flight workers to finish (or ctx).
func (q *Queue) Stop(ctx context.Context) error {
	q.mu.Lock()
	if !q.stopped {
		q.stopped = true
		close(q.stopCh)
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) Wait() {
	q.wg.Wait()
}

func (q *Queue) process(parent context.Context, ev *Event, payload []byte, deliveryID string) {
	ctx := logger.WithTrace(parent, ev.ID)
	log := logger.From(ctx)

	renewCtx, cancelRenew := context.WithCancel(context.WithoutCancel(parent))
	defer cancelRenew()
	go q.leaseHeartbeat(renewCtx, ev.ID)

	if len(payload) == 0 {
		var err error
		payload, _, err = q.st.GetPayload(ctx, ev.ID)
		if err != nil {
			log.Error("load event payload", "err", err)
			q.finish(ctx, ev.ID, "error")
			return
		}
	}

	vars := parser.Extract(payload, ev.Source.Extract)
	vars["event_id"] = ev.ID
	vars["delivery_id"] = deliveryID

	ok, err := router.Match(ev.Source.Rules, vars)
	if err != nil {
		log.Error("rule match error", "err", err)
		q.finish(ctx, ev.ID, "error")
		return
	}
	if !ok {
		log.Debug("rules not matched, skip", "source", ev.Source.Name)
		q.finish(ctx, ev.ID, "skipped")
		return
	}

	allOK := true
	for _, ac := range ev.Source.Actions {
		h, found := q.reg.Get(ac.Type)
		if !found {
			log.Error("no handler for action", "type", ac.Type)
			allOK = false
			continue
		}
		if !q.runAction(ctx, ev.ID, h, ac, vars) {
			allOK = false
		}
	}

	status := "done"
	if !allOK {
		status = "partial"
	}
	q.finish(ctx, ev.ID, status)
}

func (q *Queue) leaseHeartbeat(ctx context.Context, eventID string) {
	interval := q.lease / 3
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := q.st.RenewLease(context.WithoutCancel(ctx), eventID, q.lease, time.Now()); err != nil {
				logger.From(ctx).Error("renew lease", "err", err, "event_id", eventID)
			}
		}
	}
}

func (q *Queue) runAction(ctx context.Context, eventID string, h handler.Handler, ac config.ActionConfig, vars map[string]string) bool {
	log := logger.From(ctx)
	for attempt := 1; attempt <= q.maxRetries; attempt++ {
		attemptVars := copyVars(vars)
		attemptVars["attempt"] = itoa(attempt)

		start := time.Now()
		res, err := h.Handle(ctx, ac, attemptVars)
		dur := time.Since(start)
		if err == nil {
			log.Info("action ok", "type", ac.Type, "target", logger.RedactSensitive(res.Target),
				"attempt", attempt, "duration_ms", dur.Milliseconds())
			q.logAction(ctx, eventID, ac.Type, res.Target, attempt, true, res.Detail, dur)
			return true
		}
		log.Error("action failed", "type", ac.Type, "attempt", attempt, "err", logger.RedactSensitive(err.Error()))
		q.logAction(ctx, eventID, ac.Type, res.Target, attempt, false, err.Error(), dur)

		if attempt < q.maxRetries {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
	}
	return false
}

func (q *Queue) logAction(ctx context.Context, eventID, action, target string, attempt int, success bool, detail string, dur time.Duration) {
	wctx := context.WithoutCancel(ctx)
	target = logger.RedactSensitive(target)
	detail = logger.RedactSensitive(detail)
	if err := q.st.SaveActionLog(wctx, eventID, action, target, attempt, success, detail, dur, time.Now()); err != nil {
		logger.From(ctx).Error("save action log", "err", err)
	}
}

func (q *Queue) finish(ctx context.Context, id, status string) {
	wctx := context.WithoutCancel(ctx)
	if err := q.st.UpdateEventStatus(wctx, id, status, time.Now()); err != nil {
		logger.From(ctx).Error("update event status", "err", err)
	}
	if q.cfg != nil && q.cfg.Store.RetainPayload != nil && !*q.cfg.Store.RetainPayload {
		if err := q.st.ClearPayload(wctx, id); err != nil {
			logger.From(ctx).Error("clear payload", "err", err)
		}
	}
}

func copyVars(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
