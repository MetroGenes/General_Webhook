package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestOpen_TightensPermissions(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "webhook.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("db mode = %o, want no group/other bits", fi.Mode().Perm())
	}
}

func TestOpen_DoesNotChmodExistingDir(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(shared, "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir mode = %o, want 0755 unchanged", fi.Mode().Perm())
	}
}

func TestSavePending_DuplicateDelivery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{}`), now, "d1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	err = s.SavePendingEvent(ctx, "id-2", "src", "1.1.1.1", []byte(`{}`), now, "d1", "", "", "")
	if err != ErrDuplicateEvent {
		t.Fatalf("got %v, want ErrDuplicateEvent", err)
	}
}

func TestClaimPendingAndFinish(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{"a":1}`), now, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, now, "owner-1")
	if err != nil || ev == nil || ev.ID != "id-1" {
		t.Fatalf("claim = %+v err=%v", ev, err)
	}
	ev2, err := s.ClaimPending(ctx, time.Minute, now, "owner-1")
	if err != nil || ev2 != nil {
		t.Fatalf("second claim should be nil, got %+v err=%v", ev2, err)
	}
	if err := s.UpdateEventStatus(ctx, "id-1", "done", now); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateEventStatus_NotFound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpdateEventStatus(context.Background(), "missing", "done", time.Now()); err == nil {
		t.Fatal("want error")
	}
}

func TestSaveActionLog_EventNotFound(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveActionLog(context.Background(), "missing", "http", "http://x", 1, true, "ok", time.Second, time.Now()); err == nil {
		t.Fatal("want error")
	}
}

func TestPurgeOlderThan(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	if err := s.SavePendingEvent(ctx, "old", "src", "1.1.1.1", []byte(`{}`), old, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateEventStatus(ctx, "old", "done", old); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePendingEvent(ctx, "pending-old", "src", "1.1.1.1", []byte(`{}`), old, "d-pend", "", "", ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.PurgeOlderThan(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged=%d err=%v want 1 (terminal only)", n, err)
	}
	ok, err := s.HasEvent(ctx, "pending-old")
	if err != nil || !ok {
		t.Fatalf("pending event should be retained, ok=%v err=%v", ok, err)
	}
}

func TestMigrate_OldSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE events (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    remote_ip TEXT,
    payload BLOB,
    status TEXT NOT NULL DEFAULT 'received',
    received_at DATETIME NOT NULL,
    processed_at DATETIME
);
CREATE TABLE action_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT,
    attempt INTEGER NOT NULL,
    success INTEGER NOT NULL,
    detail TEXT,
    duration_ms INTEGER,
    created_at DATETIME NOT NULL
);
CREATE TABLE _ready_check (id INTEGER PRIMARY KEY);
INSERT INTO _ready_check(id) VALUES (1);
INSERT INTO events(id, source, remote_ip, payload, status, received_at)
VALUES ('legacy-log', 'src', '1.1.1.1', '{}', 'done', datetime('now'));
INSERT INTO action_logs(event_id, action, target, attempt, success, detail, duration_ms, created_at)
VALUES ('legacy-log', 'http', 'https://example.test', 1, 0, 'failed', 10,
        '2024-01-02 03:04:05.123456789 +0000 UTC');`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	defer s.Close()
	if err := s.ReadyCheck(context.Background()); err != nil {
		t.Fatalf("legacy readiness table did not migrate: %v", err)
	}
	if err := s.SavePendingEvent(context.Background(), "n1", "s", "1.1.1.1", []byte(`{}`), time.Now(), "d1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListActionLogs(context.Background(), "legacy-log", 1)
	if err != nil || len(logs) != 1 {
		t.Fatalf("legacy logs=%+v err=%v", logs, err)
	}
	// The durable compatibility column is millisecond precision.
	wantCreated := time.Date(2024, 1, 2, 3, 4, 5, 123000000, time.UTC)
	if !logs[0].CreatedAt.Equal(wantCreated) {
		t.Fatalf("created_at=%s want=%s", logs[0].CreatedAt, wantCreated)
	}
}

func TestMigrate_ReceivedToPending(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE events (
    id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    remote_ip TEXT,
    payload BLOB,
    status TEXT NOT NULL DEFAULT 'received',
    received_at DATETIME NOT NULL,
    processed_at DATETIME
);
CREATE TABLE action_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT,
    attempt INTEGER NOT NULL,
    success INTEGER NOT NULL,
    detail TEXT,
    duration_ms INTEGER,
    created_at DATETIME NOT NULL
);
INSERT INTO events(id, source, remote_ip, payload, status, received_at)
VALUES ('legacy-1', 'src', '1.1.1.1', '{"x":1}', 'received', datetime('now'));
`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ev, err := s.ClaimPending(context.Background(), time.Minute, time.Now(), "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if ev == nil || ev.ID != "legacy-1" {
		t.Fatalf("expected legacy received to be claimable, got %+v", ev)
	}
}

func TestClaimPending_DoesNotReclaimActiveLease(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{}`), now, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	lease := 10 * time.Minute
	ev, err := s.ClaimPending(ctx, lease, now, "owner-1")
	if err != nil || ev == nil {
		t.Fatalf("first claim: %+v %v", ev, err)
	}
	ev2, err := s.ClaimPending(ctx, lease, now.Add(3*time.Minute), "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if ev2 != nil {
		t.Fatal("active lease must not be reclaimed after 3 minutes when lease is 10m")
	}
}

func TestRequeueEventAndRetry(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{}`), now, "", "hash", "cfg", `{"name":"src","actions":[]}`); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, now, "owner-1")
	if err != nil || ev == nil {
		t.Fatalf("claim: %+v %v", ev, err)
	}
	if ev.ActionsJSON == "" || ev.ConfigHash != "cfg" {
		t.Fatalf("snapshot missing: %+v", ev)
	}
	if err := s.RequeueEvent(ctx, "id-1"); err != nil {
		t.Fatal(err)
	}
	ev2, err := s.ClaimPending(ctx, time.Minute, now, "owner-2")
	if err != nil || ev2 == nil || ev2.ID != "id-1" {
		t.Fatalf("reclaim after requeue: %+v %v", ev2, err)
	}
	if err := s.UpdateEventStatus(ctx, "id-1", "partial", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryFailedEvent(ctx, "id-1"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Stats(ctx, now)
	if err != nil || st.Pending != 1 {
		t.Fatalf("stats=%+v err=%v", st, err)
	}
}

func TestReadyCheck_Writes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ReadyCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSavePendingEventGuarded_AtomicallyRejectsBodyReplay(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	ctx := context.Background()
	if err := s.SavePendingEventGuarded(ctx, "one", "src", "1.1.1.1", []byte(`{}`), now,
		"delivery-one", "same-hash", "", "", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	err = s.SavePendingEventGuarded(ctx, "two", "src", "1.1.1.1", []byte(`{}`), now,
		"delivery-two", "same-hash", "", "", now.Add(10*time.Minute))
	if !errors.Is(err, ErrDuplicateBody) {
		t.Fatalf("got %v, want ErrDuplicateBody", err)
	}
}

func TestDurableActionRetryLifecycle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	if err := s.SavePendingEvent(ctx, "event", "src", "1.1.1.1", []byte(`{"message":"hello"}`), now, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if ev, err := s.ClaimPending(ctx, time.Minute, now, "owner"); err != nil || ev == nil {
		t.Fatalf("claim: %+v %v", ev, err)
	}
	if err := s.PrepareEvent(ctx, "event", `{"message":"hello"}`, 1, true, now); err != nil {
		t.Fatal(err)
	}
	attempt, err := s.StartAction(ctx, "event", 0, now)
	if err != nil || attempt != 1 {
		t.Fatalf("start attempt=%d err=%v", attempt, err)
	}
	next := now.Add(time.Minute)
	if err := s.ScheduleActionRetry(ctx, "event", 0, next, "temporary", 0); err != nil {
		t.Fatal(err)
	}
	status, err := s.RefreshEventStatus(ctx, "event", now)
	if err != nil || status != "retrying" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if ev, err := s.ClaimPending(ctx, time.Minute, now.Add(30*time.Second), "owner"); err != nil || ev != nil {
		t.Fatalf("claimed before due: %+v %v", ev, err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, next, "owner")
	if err != nil || ev == nil {
		t.Fatalf("claim due: %+v %v", ev, err)
	}
	if len(ev.Payload) != 0 || ev.VarsJSON == "" {
		t.Fatalf("payload should be cleared only after durable vars: %+v", ev)
	}
	attempt, err = s.StartAction(ctx, "event", 0, next)
	if err != nil || attempt != 2 {
		t.Fatalf("retry attempt=%d err=%v", attempt, err)
	}
	if err := s.CompleteAction(ctx, "event", 0, next); err != nil {
		t.Fatal(err)
	}
	status, err = s.RefreshEventStatus(ctx, "event", next)
	if err != nil || status != "done" {
		t.Fatalf("final status=%q err=%v", status, err)
	}
}

func TestReplayDeadEvent_RequiresPayloadOrVars(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "event", "src", "", []byte(`{}`), now, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateEventStatus(ctx, "event", "dead", now); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPayload(ctx, "event"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplayDeadEvent(ctx, "event", "test", now); !errors.Is(err, ErrReplayUnavailable) {
		t.Fatalf("got %v, want ErrReplayUnavailable", err)
	}
}

func TestStats_UsesEpochMilliseconds(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := time.Now().Add(-2 * time.Minute)
	if err := s.SavePendingEvent(context.Background(), "old", "src", "", []byte(`{}`), old, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	now := old.Add(2 * time.Minute)
	if err := s.SaveActionAttemptLog(context.Background(), "old", 0, 0, "http", "target", 1, false, "failed", time.Second, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveActionAttemptLog(context.Background(), "old", 0, 0, "http", "target", 2, true, "ok", time.Second, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveActionAttemptLog(context.Background(), "old", 0, 0, "http", "target", 3, false, "old failure", time.Second, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stats, err := s.Stats(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.OldestPendingAgeMS < 119000 {
		t.Fatalf("oldest age = %dms", stats.OldestPendingAgeMS)
	}
	if stats.ActionAttempts5M != 1 || stats.ActionFailures5M != 1 || stats.ActionFailureRate5M != 1 {
		t.Fatalf("5m stats=%+v", stats)
	}
	if stats.ActionAttempts1H != 2 || stats.ActionFailures1H != 1 || stats.ActionFailureRate1H != 0.5 {
		t.Fatalf("1h stats=%+v", stats)
	}
}

func TestDeadReplay_ResetsOnlyFailedAction(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "event", "src", "", []byte(`{}`), now, "", "", "", `{"name":"src","actions":[]}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPending(ctx, time.Minute, now, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareEvent(ctx, "event", `{"event_id":"event"}`, 2, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, "event", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, "event", 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, "event", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := s.DeadAction(ctx, "event", 1, "permanent", now); err != nil {
		t.Fatal(err)
	}
	if status, err := s.RefreshEventStatus(ctx, "event", now); err != nil || status != "dead" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if err := s.SaveActionAttemptLog(ctx, "event", 1, 0, "http", "https://example.test", 1, false, "permanent", time.Second, now); err != nil {
		t.Fatal(err)
	}
	if logs, err := s.ListActionLogs(ctx, "event", 10); err != nil || len(logs) != 1 || logs[0].ActionIndex != 1 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	generation, err := s.ReplayDeadEvent(ctx, "event", "downstream recovered", now.Add(time.Minute))
	if err != nil || generation != 1 {
		t.Fatalf("generation=%d err=%v", generation, err)
	}
	states, err := s.ListActionStates(ctx, "event")
	if err != nil || len(states) != 2 || states[0].Status != "done" || states[1].Status != "pending" {
		t.Fatalf("states=%+v err=%v", states, err)
	}
	meta, err := s.GetEvent(ctx, "event")
	if err != nil || meta.ReplayGeneration != 1 || meta.ReplayReason != "downstream recovered" {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
}

func TestRequeueAllProcessing_RecoversRunningAction(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SaveEvent(ctx, "event", "src", "", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPending(ctx, time.Minute, now, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareEvent(ctx, "event", `{}`, 1, false, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, "event", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RenewLease(ctx, "event", time.Minute, now, "owner"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.RequeueAllProcessing(ctx); err != nil || n != 1 {
		t.Fatalf("requeued=%d err=%v", n, err)
	}
	states, err := s.ListActionStates(ctx, "event")
	if err != nil || states[0].Status != "pending" || states[0].AttemptCount != 0 {
		t.Fatalf("states=%+v err=%v", states, err)
	}
}

func TestRewriteActionSnapshots(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.SavePendingEvent(ctx, "event", "src", "", []byte(`{}`), time.Now(), "", "", "", `{"secret":"old"}`); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RewriteActionSnapshots(ctx, func(raw string) (string, error) {
		return strings.ReplaceAll(raw, "old", "reference"), nil
	})
	if err != nil || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
}
