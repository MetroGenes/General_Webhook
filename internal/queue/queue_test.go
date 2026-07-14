package queue

import (
	"context"
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
	cfg := &config.Config{Sources: []config.Source{*src}}
	if err := st.SavePendingEvent(context.Background(), eventID, src.Name, "127.0.0.1", []byte(`{"message":"hello"}`), time.Now(), "", ""); err != nil {
		t.Fatalf("SavePendingEvent: %v", err)
	}

	got := make(chan map[string]string, 1)
	q := New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 1}, handler.NewRegistry(captureHandler{got: got}), st, cfg)
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

func TestWakeAfterStopDoesNotPanic(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()
	cfg := &config.Config{}
	q := New(config.QueueConfig{Workers: 1, Buffer: 1, MaxRetries: 1}, handler.NewRegistry(), st, cfg)
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
		t.Fatalf("lease=%s shorter than exec timeout", lease)
	}
	// Must not reclaim halfway through a 300s job when comparing 3 minutes later with this lease.
	if lease <= 3*time.Minute {
		t.Fatalf("lease=%s still too short vs prior 2m default", lease)
	}
}
