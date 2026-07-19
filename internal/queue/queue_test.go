package queue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/handler"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

func TestProcess_LoadsPayloadFromStoreViaClaim(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	const eventID = "event-1"
	src := &config.Source{
		Name:    "test",
		Extract: map[string]string{"message": "$.message"},
		Actions: []config.ActionConfig{{
			Type: "capture",
		}},
	}
	snap, err := config.SnapshotSource(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Sources: []config.Source{*src}}
	if err := st.SavePendingEvent(context.Background(), eventID, src.Name, "127.0.0.1", []byte(`{"message":"hello"}`), time.Now(), "", "", "hash", snap); err != nil {
		t.Fatalf("SavePendingEvent: %v", err)
	}

	got := make(chan map[string]string, 1)
	q := New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 1}, handler.NewRegistry(captureHandler{got: got}), st, cfg, "owner-1")
	q.Start(context.Background())

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	select {
	case vars := <-got:
		if vars["message"] != "hello" {
			t.Fatalf("message = %q, want %q", vars["message"], "hello")
		}
		if vars["event_id"] != eventID {
			t.Fatalf("event_id = %q", vars["event_id"])
		}
	case <-stopCtx.Done():
		t.Fatal("timeout waiting for handler")
	}

	if err := q.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestProcess_CancelRequeuesPending(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const eventID = "event-cancel"
	src := &config.Source{
		Name: "test",
		Actions: []config.ActionConfig{{
			Type: "slow",
		}},
	}
	snap, _ := config.SnapshotSource(src)
	cfg := &config.Config{Sources: []config.Source{*src}}
	if err := st.SavePendingEvent(context.Background(), eventID, src.Name, "127.0.0.1", []byte(`{}`), time.Now(), "", "", "hash", snap); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	q := New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 3}, handler.NewRegistry(slowHandler{started: started}), st, cfg, "owner-1")
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	q.Start(workerCtx)

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not start")
	}
	cancelWorker()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	_ = q.Stop(drainCtx)
	q.Wait()

	ev, err := st.GetEvent(context.Background(), eventID)
	if err != nil || ev == nil {
		t.Fatalf("GetEvent: %+v %v", ev, err)
	}
	if ev.Status == "partial" || ev.Status == "done" || ev.Status == "error" || ev.Status == "skipped" {
		t.Fatalf("canceled in-flight must not be terminal, status=%q", ev.Status)
	}
	if ev.Status != "pending" && ev.Status != "processing" {
		t.Fatalf("status=%q, want pending or processing", ev.Status)
	}
	// Ensure it can be claimed again after requeue/expiry path.
	if ev.Status == "processing" {
		_ = st.RequeueEvent(context.Background(), eventID)
	}
	claimed, err := st.ClaimPending(context.Background(), time.Minute, time.Now(), "owner-2")
	if err != nil || claimed == nil || claimed.ID != eventID {
		t.Fatalf("should reclaim after cancel, got %+v err=%v", claimed, err)
	}
}

func TestWakeAfterStopDoesNotPanic(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()
	cfg := &config.Config{}
	q := New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 1}, handler.NewRegistry(), st, cfg, "owner-1")
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	_ = q.Stop(stopCtx)
	if q.Wake() {
		t.Fatal("Wake after Stop should return false")
	}
	if q.Enqueue(&Event{ID: "x"}) {
		t.Fatal("Enqueue after Stop should return false")
	}
}

type captureHandler struct {
	got chan map[string]string
}

func (h captureHandler) Type() string { return "capture" }

func (h captureHandler) Handle(_ context.Context, _ config.ActionConfig, vars map[string]string) (handler.Result, error) {
	h.got <- vars
	return handler.Result{Target: "capture"}, nil
}

type slowHandler struct {
	started chan struct{}
}

func (h slowHandler) Type() string { return "slow" }

func (h slowHandler) Handle(ctx context.Context, _ config.ActionConfig, _ map[string]string) (handler.Result, error) {
	select {
	case <-h.started:
	default:
		close(h.started)
	}
	select {
	case <-ctx.Done():
		return handler.Result{Target: "slow"}, ctx.Err()
	case <-time.After(10 * time.Second):
		return handler.Result{Target: "slow"}, errors.New("unexpected finish")
	}
}

func TestComputeLease_CoversExecTimeout(t *testing.T) {
	cfg := &config.Config{Sources: []config.Source{{
		Name: "custom",
		Actions: []config.ActionConfig{{
			Type:    "exec",
			Command: "/bin/true",
			Timeout: config.Duration{Duration: 300 * time.Second},
		}},
	}}}
	lease := computeLease(cfg, 3)
	if lease < 300*time.Second {
		t.Fatalf("lease = %s, want >= 300s", lease)
	}
}

func TestProcess_PersistsRetryAndDoesNotRepeatSuccessfulAction(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().Truncate(time.Millisecond)
	src := &config.Source{
		Name:    "test",
		Extract: map[string]string{"message": "$.message"},
		Actions: []config.ActionConfig{{Type: "always-good"}, {Type: "flaky"}},
	}
	snapshot, err := config.SnapshotSource(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SavePendingEvent(context.Background(), "event-retry", src.Name, "127.0.0.1",
		[]byte(`{"message":"hello"}`), now, "", "", "hash", snapshot); err != nil {
		t.Fatal(err)
	}
	good := &countingHandler{typ: "always-good"}
	flaky := &countingHandler{typ: "flaky", failFirst: true}
	cfg := &config.Config{Sources: []config.Source{*src}}
	retain := false
	cfg.Store.RetainPayload = &retain
	q := New(config.QueueConfig{
		Workers: 1, Buffer: 1, MaxRetries: 1,
		RetryBase:     config.Duration{Duration: time.Minute},
		MaxRetryDelay: config.Duration{Duration: time.Minute},
		MaxRetryAge:   config.Duration{Duration: time.Hour},
	}, handler.NewRegistry(good, flaky), st, cfg, "owner")
	clock := now
	q.now = func() time.Time { return clock }
	q.jitter = func(time.Duration) time.Duration { return time.Minute }

	ev, err := st.ClaimPending(context.Background(), time.Minute, clock, "owner")
	if err != nil || ev == nil {
		t.Fatalf("claim: %+v %v", ev, err)
	}
	q.process(context.Background(), ev, src)
	meta, err := st.GetEvent(context.Background(), ev.ID)
	if err != nil || meta.Status != "retrying" {
		t.Fatalf("first pass status=%+v err=%v", meta, err)
	}
	if good.calls != 1 || flaky.calls != 1 {
		t.Fatalf("first pass calls good=%d flaky=%d", good.calls, flaky.calls)
	}
	if early, err := st.ClaimPending(context.Background(), time.Minute, clock.Add(30*time.Second), "owner"); err != nil || early != nil {
		t.Fatalf("retry claimed before due: %+v %v", early, err)
	}

	clock = clock.Add(time.Minute)
	ev, err = st.ClaimPending(context.Background(), time.Minute, clock, "owner")
	if err != nil || ev == nil {
		t.Fatalf("claim retry: %+v %v", ev, err)
	}
	if len(ev.Payload) != 0 || ev.VarsJSON == "" {
		t.Fatalf("retry must use durable vars after payload cleanup: %+v", ev)
	}
	q.process(context.Background(), ev, src)
	meta, err = st.GetEvent(context.Background(), ev.ID)
	if err != nil || meta.Status != "done" {
		t.Fatalf("final status=%+v err=%v", meta, err)
	}
	if good.calls != 1 || flaky.calls != 2 {
		t.Fatalf("successful action repeated: good=%d flaky=%d", good.calls, flaky.calls)
	}
	if flaky.lastMessage != "hello" {
		t.Fatalf("durable vars message=%q", flaky.lastMessage)
	}
}

func TestManualReplay_ResetsRetryAgeDeadline(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	old := now.Add(-48 * time.Hour)
	src := &config.Source{Name: "test", Actions: []config.ActionConfig{{Type: "flaky"}}}
	snapshot, err := config.SnapshotSource(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SavePendingEvent(ctx, "old-dead", src.Name, "", []byte(`{}`), old, "", "", "hash", snapshot); err != nil {
		t.Fatal(err)
	}
	if ev, err := st.ClaimPending(ctx, time.Minute, now, "owner"); err != nil || ev == nil {
		t.Fatalf("claim=%+v err=%v", ev, err)
	}
	if err := st.PrepareEvent(ctx, "old-dead", `{}`, 1, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartAction(ctx, "old-dead", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := st.DeadAction(ctx, "old-dead", 0, "initial failure", now); err != nil {
		t.Fatal(err)
	}
	if status, err := st.RefreshEventStatus(ctx, "old-dead", now); err != nil || status != "dead" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if _, err := st.ReplayDeadEvent(ctx, "old-dead", "downstream recovered", now); err != nil {
		t.Fatal(err)
	}
	ev, err := st.ClaimPending(ctx, time.Minute, now, "owner")
	if err != nil || ev == nil {
		t.Fatalf("claim replay=%+v err=%v", ev, err)
	}
	if !ev.RetryStartedAt.Equal(now) {
		t.Fatalf("retry anchor=%s want=%s", ev.RetryStartedAt, now)
	}

	flaky := &countingHandler{typ: "flaky", failFirst: true}
	cfg := &config.Config{Sources: []config.Source{*src}}
	q := New(config.QueueConfig{
		Workers: 1, Buffer: 1, MaxRetries: 1,
		RetryBase:     config.Duration{Duration: time.Minute},
		MaxRetryDelay: config.Duration{Duration: time.Minute},
		MaxRetryAge:   config.Duration{Duration: time.Hour},
	}, handler.NewRegistry(flaky), st, cfg, "owner")
	q.now = func() time.Time { return now }
	q.jitter = func(time.Duration) time.Duration { return time.Minute }
	q.process(ctx, ev, src)
	meta, err := st.GetEvent(ctx, "old-dead")
	if err != nil || meta == nil || meta.Status != "retrying" {
		t.Fatalf("replayed old event should retain retry budget: meta=%+v err=%v", meta, err)
	}
}

type countingHandler struct {
	typ         string
	failFirst   bool
	calls       int
	lastMessage string
}

func (h *countingHandler) Type() string { return h.typ }

func (h *countingHandler) Handle(_ context.Context, _ config.ActionConfig, vars map[string]string) (handler.Result, error) {
	h.calls++
	h.lastMessage = vars["message"]
	if h.failFirst && h.calls == 1 {
		return handler.Result{Target: h.typ}, errors.New("temporary failure")
	}
	return handler.Result{Target: h.typ}, nil
}
