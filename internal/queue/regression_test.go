package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MetroGenes/General_Webhook/internal/config"
	"github.com/MetroGenes/General_Webhook/internal/handler"
	"github.com/MetroGenes/General_Webhook/internal/policy"
	"github.com/MetroGenes/General_Webhook/internal/store"
)

func TestExecutionVars_RejectedValuesNeverEnterAudit(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		t.Run(strconv.FormatBool(failCommit), func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			src := &config.Source{
				Name: "logs", Extract: map[string]string{"service": "$.service"},
				LogPolicy: &config.LogPolicyConfig{Field: "service", Services: []string{"allowed"}},
				Redaction: &config.RedactionConfig{Patterns: []string{`FAKE-SECRET-[A-Z]+`}},
			}
			if err := compileSourceRedaction(src); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]string{"service": "FAKE-SECRET-TOKEN\nreview_injected_metric 123"})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Truncate(time.Millisecond)
			if err := st.SavePendingEvent(ctx, "rejected", src.Name, "", payload, now, "", "", "", ""); err != nil {
				t.Fatal(err)
			}
			ev, err := st.ClaimPending(ctx, time.Minute, now, "test")
			if err != nil || ev == nil {
				t.Fatalf("claim=%+v err=%v", ev, err)
			}
			retain := false
			cfg := &config.Config{Sources: []config.Source{*src}, Store: config.StoreConfig{RetainPayload: &retain}}
			q := New(config.QueueConfig{}, handler.NewRegistry(), st, cfg, "test")
			if failCommit {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if vars, ok := q.executionVars(ctx, ev, src); ok || vars != nil {
				t.Fatalf("rejected vars were deliverable: %+v", vars)
			}
			snapshot, err := json.Marshal(q.DropSnapshot())
			if err != nil {
				t.Fatal(err)
			}
			audit := logs.String() + q.DropSummary() + string(snapshot)
			for _, forbidden := range []string{"FAKE-SECRET-TOKEN", "review_injected_metric", `"value":`} {
				if strings.Contains(audit, forbidden) {
					t.Fatalf("rejected input leaked into audit: %s", audit)
				}
			}
			if failCommit {
				if len(q.DropSnapshot()) != 0 {
					t.Fatal("uncommitted skip was counted as a rejection")
				}
				return
			}
			if q.DropSnapshot()[src.Name][policy.ReasonAllowlistRejected] != 1 {
				t.Fatalf("committed skip not counted: %s", snapshot)
			}
			meta, err := st.GetEvent(ctx, ev.ID)
			if err != nil || meta == nil || meta.Status != "skipped" {
				t.Fatalf("event=%+v err=%v", meta, err)
			}
			if payload, _, err := st.GetPayload(ctx, ev.ID); err != nil || len(payload) != 0 {
				t.Fatalf("skipped payload retained: len=%d err=%v", len(payload), err)
			}
		})
	}
}

func TestProcess_RetryAndReplayPreserveIdentityAndRedactedValues(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const eventID = "20260906T133000.123-0123456789abcdef"
	const deliveryID = "feedfacecafebeef"
	src := &config.Source{
		Name: "logs",
		Extract: map[string]string{
			"message": "$.message", "event_id": "$.event_id", "delivery_id": "$.delivery_id",
		},
		Redaction: &config.RedactionConfig{Patterns: []string{`\b[0-9a-fA-F]{16}\b`}},
		Actions:   []config.ActionConfig{{Type: "good"}, {Type: "identity-capture"}},
	}
	if err := compileSourceRedaction(src); err != nil {
		t.Fatal(err)
	}
	snapshot, err := config.SnapshotSource(src)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Truncate(time.Millisecond)
	if err := st.SavePendingEvent(ctx, eventID, src.Name, "",
		[]byte(`{"message":"token feedfacecafebeef","event_id":"forged","delivery_id":"forged"}`),
		clock, deliveryID, "", "", snapshot); err != nil {
		t.Fatal(err)
	}
	good := &countingHandler{typ: "good"}
	observer := &identityCaptureHandler{failures: 2}
	retain := false
	cfg := &config.Config{Sources: []config.Source{*src}, Store: config.StoreConfig{RetainPayload: &retain}}
	q := New(config.QueueConfig{
		MaxRetries: 1, RetryBase: config.Duration{Duration: time.Minute},
		MaxRetryDelay: config.Duration{Duration: time.Minute}, MaxRetryAge: config.Duration{Duration: time.Hour},
	}, handler.NewRegistry(good, observer), st, cfg, "test")
	q.now = func() time.Time { return clock }
	q.jitter = func(time.Duration) time.Duration { return time.Minute }
	for pass, wantStatus := range []string{"retrying", "dead", "done"} {
		if pass == 2 {
			clock = clock.Add(48 * time.Hour)
			if _, err := st.ReplayDeadEvent(ctx, eventID, "downstream recovered", clock); err != nil {
				t.Fatal(err)
			}
		}
		ev, err := st.ClaimPending(ctx, q.lease, clock, "test")
		if err != nil || ev == nil {
			t.Fatalf("claim pass %d: %+v %v", pass, ev, err)
		}
		if pass > 0 && (len(ev.Payload) != 0 || ev.VarsJSON == "") {
			t.Fatalf("pass %d must use durable redacted vars after payload cleanup", pass)
		}
		q.process(ctx, ev, src)
		meta, err := st.GetEvent(ctx, eventID)
		if err != nil || meta == nil || meta.Status != wantStatus {
			t.Fatalf("pass %d status=%+v err=%v", pass, meta, err)
		}
		clock = clock.Add(time.Minute)
	}
	if good.calls != 1 || len(observer.observations) != 3 {
		t.Fatalf("completed action replayed: good=%d retryable=%d", good.calls, len(observer.observations))
	}
	for pass, observed := range observer.observations {
		attempt := []int{1, 2, 1}[pass]
		want := handler.ExecutionMetadata{EventID: eventID, ActionIndex: 1, Attempt: attempt}
		if !observed.trusted || observed.meta != want {
			t.Errorf("pass %d metadata=%+v trusted=%v want=%+v", pass, observed.meta, observed.trusted, want)
		}
		if observed.vars["event_id"] != "20260906T133000.123-***" || observed.vars["delivery_id"] != "***" || observed.vars["message"] != "token ***" {
			t.Errorf("pass %d redaction changed or restored raw identity: %+v", pass, observed.vars)
		}
		if observed.vars["attempt"] != strconv.Itoa(attempt) || observed.vars["action_index"] != "1" {
			t.Errorf("pass %d attempt template vars=%+v", pass, observed.vars)
		}
	}
}

type identityObservation struct {
	meta    handler.ExecutionMetadata
	trusted bool
	vars    map[string]string
}

type identityCaptureHandler struct {
	observations []identityObservation
	failures     int
}

func (*identityCaptureHandler) Type() string { return "identity-capture" }

func (h *identityCaptureHandler) Handle(ctx context.Context, _ config.ActionConfig, vars map[string]string) (handler.Result, error) {
	meta, ok := handler.ExecutionMetadataFrom(ctx)
	h.observations = append(h.observations, identityObservation{meta: meta, trusted: ok, vars: copyVars(vars)})
	if len(h.observations) <= h.failures {
		return handler.Result{}, errors.New("temporary downstream failure")
	}
	return handler.Result{}, nil
}

func TestProcess_ExpiredRetryDoesNotDispatchAndReplayResetsDeadline(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Hour, 2 * time.Hour} {
		t.Run(elapsed.String(), func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			src := &config.Source{Name: "test", Actions: []config.ActionConfig{{Type: "good"}, {Type: "flaky"}}}
			initial := time.Now().Truncate(time.Millisecond)
			clock := initial
			if err := st.SavePendingEvent(ctx, "expires", src.Name, "", []byte(`{}`), clock, "", "", "", ""); err != nil {
				t.Fatal(err)
			}
			good := &countingHandler{typ: "good"}
			flaky := &countingHandler{typ: "flaky", failFirst: true}
			cfg := &config.Config{Sources: []config.Source{*src}}
			q := New(config.QueueConfig{
				MaxRetries: 2, RetryBase: config.Duration{Duration: time.Minute},
				MaxRetryDelay: config.Duration{Duration: time.Minute}, MaxRetryAge: config.Duration{Duration: time.Hour},
			}, handler.NewRegistry(good, flaky), st, cfg, "test")
			q.now = func() time.Time { return clock }
			q.jitter = func(time.Duration) time.Duration { return time.Minute }
			process := func() {
				t.Helper()
				ev, err := st.ClaimPending(ctx, q.lease, clock, "test")
				if err != nil || ev == nil {
					t.Fatalf("claim=%+v err=%v", ev, err)
				}
				q.process(ctx, ev, src)
			}
			process()
			clock = initial.Add(elapsed)
			process()
			if good.calls != 1 || flaky.calls != 1 {
				t.Fatalf("overdue action dispatched: good=%d flaky=%d", good.calls, flaky.calls)
			}
			states, err := st.ListActionStates(ctx, "expires")
			if err != nil || len(states) != 2 || states[0].Status != "done" || states[1].Status != "dead" || states[1].AttemptCount != 1 {
				t.Fatalf("expiry changed successful action or attempt count: %+v err=%v", states, err)
			}
			if !strings.Contains(states[1].LastError, "retry age") {
				t.Fatalf("missing durable expiry reason: %+v", states[1])
			}
			logs, err := st.ListActionLogs(ctx, "expires", 10)
			if err != nil || len(logs) != 2 {
				t.Fatalf("expiry fabricated an execution attempt: %+v err=%v", logs, err)
			}
			meta, err := st.GetEvent(ctx, "expires")
			if err != nil || meta == nil || meta.Status != "dead" {
				t.Fatalf("expired event=%+v err=%v", meta, err)
			}
			if _, err := st.ReplayDeadEvent(ctx, "expires", "operator replay", clock); err != nil {
				t.Fatal(err)
			}
			process()
			meta, err = st.GetEvent(ctx, "expires")
			if err != nil || meta == nil || meta.Status != "done" || good.calls != 1 || flaky.calls != 2 {
				t.Fatalf("replay failed to reset expiry: meta=%+v good=%d flaky=%d err=%v", meta, good.calls, flaky.calls, err)
			}
		})
	}
}

func TestProcess_ExpiredPendingActionHasNoAttempt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := &config.Source{Name: "test", Actions: []config.ActionConfig{{Type: "good"}}}
	initial := time.Now().Truncate(time.Millisecond)
	if err := st.SavePendingEvent(ctx, "expired-pending", src.Name, "", []byte(`{}`), initial, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	clock := initial.Add(time.Hour)
	good := &countingHandler{typ: "good"}
	cfg := &config.Config{Sources: []config.Source{*src}}
	q := New(config.QueueConfig{MaxRetryAge: config.Duration{Duration: time.Hour}}, handler.NewRegistry(good), st, cfg, "test")
	q.now = func() time.Time { return clock }
	ev, err := st.ClaimPending(ctx, q.lease, clock, "test")
	if err != nil || ev == nil {
		t.Fatalf("claim=%+v err=%v", ev, err)
	}
	q.process(ctx, ev, src)
	states, err := st.ListActionStates(ctx, ev.ID)
	if err != nil || len(states) != 1 || states[0].Status != "dead" || states[0].AttemptCount != 0 || good.calls != 0 {
		t.Fatalf("expired pending action executed: states=%+v calls=%d err=%v", states, good.calls, err)
	}
}
