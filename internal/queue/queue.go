package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
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

type Event struct {
	ID         string
	Source     *config.Source
	ReceivedAt time.Time
}

// Queue is a durable event worker. Retry state lives in event_actions; the
// in-memory channel is only a wake hint.
type Queue struct {
	wake          chan struct{}
	workers       int
	maxRetries    int
	retryBase     time.Duration
	maxRetryDelay time.Duration
	maxRetryAge   time.Duration
	lease         time.Duration
	owner         string
	reg           *handler.Registry
	st            *store.Store
	cfg           *config.Config
	wg            sync.WaitGroup
	mu            sync.Mutex
	stopped       bool
	stopCh        chan struct{}
	liveWorkers   atomic.Int32
	now           func() time.Time
	jitter        func(time.Duration) time.Duration
}

func New(cfg config.QueueConfig, reg *handler.Registry, st *store.Store, sources *config.Config, owner string) *Queue {
	buf := cfg.Buffer
	if buf <= 0 {
		buf = 1
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = 1
	}
	retries := cfg.MaxRetries
	if retries < 0 {
		retries = 0
	}
	retryBase := cfg.RetryBase.Duration
	if retryBase <= 0 {
		retryBase = time.Second
	}
	maxRetryDelay := cfg.MaxRetryDelay.Duration
	if maxRetryDelay < retryBase {
		maxRetryDelay = 15 * time.Minute
	}
	maxRetryAge := cfg.MaxRetryAge.Duration
	if maxRetryAge < retryBase {
		maxRetryAge = 24 * time.Hour
	}
	if owner == "" {
		owner = "local"
	}
	q := &Queue{
		wake:          make(chan struct{}, buf),
		workers:       workers,
		maxRetries:    retries,
		retryBase:     retryBase,
		maxRetryDelay: maxRetryDelay,
		maxRetryAge:   maxRetryAge,
		lease:         computeLease(sources, retries+1),
		owner:         owner,
		reg:           reg,
		st:            st,
		cfg:           sources,
		stopCh:        make(chan struct{}),
		now:           time.Now,
	}
	q.jitter = func(cap time.Duration) time.Duration {
		if cap <= 0 {
			return 0
		}
		return time.Duration(rand.Int64N(int64(cap) + 1))
	}
	return q
}

// computeLease covers one pass over every configured action. Delays between
// attempts are persisted, so they do not consume a lease or block a worker.
func computeLease(cfg *config.Config, _ int) time.Duration {
	longestPass := defaultHTTPActionTimeout
	if cfg != nil {
		for _, src := range cfg.Sources {
			var pass time.Duration
			for _, ac := range src.Actions {
				pass += actionTimeout(ac)
			}
			if pass > longestPass {
				longestPass = pass
			}
		}
	}
	lease := longestPass + leaseSafetyMargin
	if lease < 5*time.Minute {
		lease = 5 * time.Minute
	}
	return lease
}

func actionTimeout(ac config.ActionConfig) time.Duration {
	if ac.Type == "exec" {
		if ac.Timeout.Duration > 0 {
			return ac.Timeout.Duration
		}
		return defaultExecTimeout
	}
	return defaultHTTPActionTimeout
}

func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		q.liveWorkers.Add(1)
		go func() {
			defer q.liveWorkers.Add(-1)
			defer q.wg.Done()
			q.worker(ctx)
		}()
	}
	q.Wake()
}

func (q *Queue) Ready() bool {
	return !q.isStopped() && q.liveWorkers.Load() > 0
}

func (q *Queue) worker(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
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
		now := q.now()
		_, _ = q.st.RequeueExpiredLeases(ctx, now)
		ev, err := q.st.ClaimPending(ctx, q.lease, now, q.owner)
		if err != nil {
			logger.From(ctx).Error("claim pending", "err", err)
			return
		}
		if ev == nil {
			return
		}
		src, err := resolveSource(q.cfg, ev)
		if err != nil {
			logger.From(ctx).Error("resolve source for pending event", "source", ev.Source, "id", ev.ID, "err", err)
			_ = q.st.UpdateEventStatus(context.WithoutCancel(ctx), ev.ID, "dead", q.now())
			continue
		}
		q.process(ctx, ev, src)
	}
}

func resolveSource(cfg *config.Config, ev *store.PendingEvent) (*config.Source, error) {
	if ev.ActionsJSON != "" {
		src, err := config.ParseSourceSnapshot(ev.ActionsJSON)
		if err != nil {
			return nil, err
		}
		if src.Name == "" {
			src.Name = ev.Source
		}
		return src, nil
	}
	if cfg == nil {
		return nil, errors.New("nil config")
	}
	src := cfg.SourceByName(ev.Source)
	if src == nil {
		return nil, errors.New("unknown source")
	}
	return src, nil
}

func (q *Queue) Wake() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return false
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

func (q *Queue) Enqueue(_ *Event) bool { return q.Wake() }

func (q *Queue) isStopped() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stopped
}

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

func (q *Queue) Wait() { q.wg.Wait() }

func (q *Queue) process(parent context.Context, ev *store.PendingEvent, src *config.Source) {
	ctx := logger.WithTrace(parent, ev.ID)
	log := logger.From(ctx)
	renewCtx, cancelRenew := context.WithCancel(context.WithoutCancel(parent))
	defer cancelRenew()
	go q.leaseHeartbeat(renewCtx, ev.ID)

	vars, ok := q.executionVars(ctx, ev, src)
	if !ok {
		return
	}
	clearPayload := q.cfg != nil && q.cfg.Store.RetainPayload != nil && !*q.cfg.Store.RetainPayload
	varsJSON, err := json.Marshal(vars)
	if err != nil {
		log.Error("marshal execution vars", "err", err)
		q.requeue(ctx, ev.ID)
		return
	}
	if err := q.st.PrepareEvent(context.WithoutCancel(ctx), ev.ID, string(varsJSON), len(src.Actions), clearPayload, q.now()); err != nil {
		log.Error("prepare durable action state", "err", err)
		q.requeue(ctx, ev.ID)
		return
	}

	states, err := q.st.ListActionStates(ctx, ev.ID)
	if err != nil {
		log.Error("list action state", "err", err)
		q.requeue(ctx, ev.ID)
		return
	}
	stateByIndex := make(map[int]store.ActionState, len(states))
	for _, state := range states {
		stateByIndex[state.Index] = state
	}

	for index, ac := range src.Actions {
		if isCanceled(parent) {
			q.requeue(ctx, ev.ID)
			return
		}
		state, exists := stateByIndex[index]
		if !exists || state.Status == "done" || state.Status == "dead" {
			continue
		}
		now := q.now()
		if state.Status == "retrying" && state.NextAttemptMS > now.UnixMilli() {
			continue
		}
		attempt, err := q.st.StartAction(ctx, ev.ID, index, now)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			log.Error("start action", "action_index", index, "err", err)
			q.requeue(ctx, ev.ID)
			return
		}
		attemptVars := copyVars(vars)
		attemptVars["attempt"] = strconv.Itoa(attempt)
		attemptVars["action_index"] = strconv.Itoa(index)

		h, found := q.reg.Get(ac.Type)
		if !found {
			detail := "no handler for action type " + ac.Type
			q.logAction(ctx, ev, index, ac.Type, "", attempt, false, detail, 0)
			if err := q.st.DeadAction(context.WithoutCancel(ctx), ev.ID, index, detail, q.now()); err != nil {
				log.Error("dead-letter missing handler", "err", err)
				q.requeue(ctx, ev.ID)
				return
			}
			continue
		}

		start := q.now()
		res, actionErr := h.Handle(ctx, ac, attemptVars)
		duration := q.now().Sub(start)
		if actionErr == nil {
			q.logAction(ctx, ev, index, ac.Type, res.Target, attempt, true, res.Detail, duration)
			if err := q.st.CompleteAction(context.WithoutCancel(ctx), ev.ID, index, q.now()); err != nil {
				log.Error("complete action", "err", err)
				q.requeue(ctx, ev.ID)
				return
			}
			continue
		}

		q.logAction(ctx, ev, index, ac.Type, res.Target, attempt, false, actionErr.Error(), duration)
		if isCanceled(ctx) || errors.Is(actionErr, context.Canceled) {
			q.requeue(ctx, ev.ID)
			return
		}
		detail := logger.RedactSensitive(actionErr.Error())
		maxAttempts := q.maxRetries + 1
		if handler.IsNonRetryable(actionErr) || attempt >= maxAttempts {
			if err := q.st.DeadAction(context.WithoutCancel(ctx), ev.ID, index, detail, q.now()); err != nil {
				log.Error("dead-letter action", "err", err)
				q.requeue(ctx, ev.ID)
				return
			}
			continue
		}

		retryAfter := handler.RetryAfter(actionErr)
		delay := retryAfter
		if delay <= 0 {
			delay = q.retryDelay(attempt)
		}
		next := q.now().Add(delay)
		if next.After(ev.RetryStartedAt.Add(q.maxRetryAge)) {
			if err := q.st.DeadAction(context.WithoutCancel(ctx), ev.ID, index, "retry deadline exceeded: "+detail, q.now()); err != nil {
				log.Error("dead-letter retry deadline", "err", err)
				q.requeue(ctx, ev.ID)
				return
			}
			continue
		}
		if err := q.st.ScheduleActionRetry(context.WithoutCancel(ctx), ev.ID, index, next, detail, retryAfter); err != nil {
			log.Error("schedule action retry", "err", err)
			q.requeue(ctx, ev.ID)
			return
		}
	}

	status, err := q.st.RefreshEventStatus(context.WithoutCancel(ctx), ev.ID, q.now())
	if err != nil {
		log.Error("refresh event status", "err", err)
		q.requeue(ctx, ev.ID)
		return
	}
	log.Info("event processing pass complete", "status", status)
}

func (q *Queue) executionVars(ctx context.Context, ev *store.PendingEvent, src *config.Source) (map[string]string, bool) {
	log := logger.From(ctx)
	if ev.VarsJSON != "" {
		var vars map[string]string
		if err := json.Unmarshal([]byte(ev.VarsJSON), &vars); err != nil {
			log.Error("parse durable execution vars", "err", err)
			_ = q.st.UpdateEventStatus(context.WithoutCancel(ctx), ev.ID, "dead", q.now())
			return nil, false
		}
		vars["event_id"] = ev.ID
		vars["delivery_id"] = ev.ExternalDeliveryID
		return vars, true
	}
	payload := ev.Payload
	if len(payload) == 0 {
		var err error
		payload, _, err = q.st.GetPayload(ctx, ev.ID)
		if err != nil {
			log.Error("load event payload", "err", err)
			q.requeue(ctx, ev.ID)
			return nil, false
		}
	}
	if len(payload) == 0 {
		log.Error("event has no payload or durable vars")
		_ = q.st.UpdateEventStatus(context.WithoutCancel(ctx), ev.ID, "dead", q.now())
		return nil, false
	}
	vars := parser.Extract(payload, src.Extract)
	vars["event_id"] = ev.ID
	vars["delivery_id"] = ev.ExternalDeliveryID
	matched, err := router.Match(src.Rules, vars)
	if err != nil {
		log.Error("rule match error", "err", err)
		_ = q.st.UpdateEventStatus(context.WithoutCancel(ctx), ev.ID, "dead", q.now())
		return nil, false
	}
	if !matched {
		if err := q.st.UpdateEventStatus(context.WithoutCancel(ctx), ev.ID, "skipped", q.now()); err != nil {
			log.Error("mark skipped", "err", err)
			q.requeue(ctx, ev.ID)
			return nil, false
		}
		if q.cfg != nil && q.cfg.Store.RetainPayload != nil && !*q.cfg.Store.RetainPayload {
			if err := q.st.ClearPayload(context.WithoutCancel(ctx), ev.ID); err != nil {
				log.Error("clear skipped payload", "err", err)
			}
		}
		return nil, false
	}
	return vars, true
}

func (q *Queue) retryDelay(attempt int) time.Duration {
	cap := q.retryBase
	for i := 1; i < attempt && cap < q.maxRetryDelay; i++ {
		if cap > q.maxRetryDelay/2 {
			cap = q.maxRetryDelay
			break
		}
		cap *= 2
	}
	if cap > q.maxRetryDelay {
		cap = q.maxRetryDelay
	}
	return q.jitter(cap)
}

func (q *Queue) leaseHeartbeat(ctx context.Context, eventID string) {
	interval := q.lease / 3
	if interval < 15*time.Second {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := q.st.RenewLease(context.WithoutCancel(ctx), eventID, q.lease, q.now(), q.owner); err != nil {
				logger.From(ctx).Error("renew lease", "err", err, "event_id", eventID)
			}
		}
	}
}

func (q *Queue) logAction(ctx context.Context, ev *store.PendingEvent, actionIndex int, action, target string, attempt int, success bool, detail string, dur time.Duration) {
	target = logger.RedactSensitive(target)
	detail = logger.RedactSensitive(detail)
	if err := q.st.SaveActionAttemptLog(context.WithoutCancel(ctx), ev.ID, actionIndex, ev.ReplayGeneration,
		action, target, attempt, success, detail, dur, q.now()); err != nil {
		logger.From(ctx).Error("save action log", "err", err)
	}
}

func (q *Queue) requeue(ctx context.Context, id string) {
	if err := q.st.RequeueEvent(context.WithoutCancel(ctx), id); err != nil {
		logger.From(ctx).Error("requeue event", "err", err, "event_id", id)
	}
}

func isCanceled(ctx context.Context) bool { return ctx != nil && ctx.Err() != nil }

func copyVars(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for key, value := range in {
		out[key] = value
	}
	return out
}
