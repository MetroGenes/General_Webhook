package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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

func TestSavePending_DuplicateDelivery(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{}`), now, "d1", ""); err != nil {
		t.Fatal(err)
	}
	err = s.SavePendingEvent(ctx, "id-2", "src", "1.1.1.1", []byte(`{}`), now, "d1", "")
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
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{"a":1}`), now, "", ""); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, now)
	if err != nil || ev == nil || ev.ID != "id-1" {
		t.Fatalf("claim = %+v err=%v", ev, err)
	}
	ev2, err := s.ClaimPending(ctx, time.Minute, now)
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
	if err := s.SavePendingEvent(ctx, "old", "src", "1.1.1.1", []byte(`{}`), old, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateEventStatus(ctx, "old", "done", old); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePendingEvent(ctx, "pending-old", "src", "1.1.1.1", []byte(`{}`), old, "d-pend", ""); err != nil {
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
);`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	defer s.Close()
	if err := s.SavePendingEvent(context.Background(), "n1", "s", "1.1.1.1", []byte(`{}`), time.Now(), "d1", ""); err != nil {
		t.Fatal(err)
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
	ev, err := s.ClaimPending(context.Background(), time.Minute, time.Now())
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
	if err := s.SavePendingEvent(ctx, "id-1", "src", "1.1.1.1", []byte(`{}`), now, "", ""); err != nil {
		t.Fatal(err)
	}
	lease := 10 * time.Minute
	ev, err := s.ClaimPending(ctx, lease, now)
	if err != nil || ev == nil {
		t.Fatalf("first claim: %+v %v", ev, err)
	}
	ev2, err := s.ClaimPending(ctx, lease, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ev2 != nil {
		t.Fatal("active lease must not be reclaimed after 3 minutes when lease is 10m")
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
