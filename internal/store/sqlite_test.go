package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLitePolicy_AppliesToReplacementConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "literal?database#name.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file name was interpreted as URI parameters: %v", err)
	}
	for _, replace := range []bool{false, true} {
		if replace {
			s.db.SetMaxIdleConns(0)
		}
		var journal string
		var synchronous, busy int
		if err := s.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if journal != "wal" || synchronous != 2 || busy != sqliteBusyTimeoutMS {
			t.Fatalf("replacement=%v: journal=%s synchronous=%d busy=%d", replace, journal, synchronous, busy)
		}
	}
}

func TestReadyCheck_UsesMigratedTableAndRealWrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var tableCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = '_ready_check'`).Scan(&tableCount); err != nil || tableCount != 1 {
		t.Fatalf("readiness table missing after migration: count=%d err=%v", tableCount, err)
	}
	if err := s.ReadyCheck(ctx); err != nil {
		t.Fatal(err)
	}
	var first, second int
	if err := s.db.QueryRow(`SELECT nonce FROM _ready_check WHERE id = 1`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT nonce FROM _ready_check WHERE id = 1`).Scan(&second); err != nil || first == second {
		t.Fatalf("readiness did not change a persisted value: first=%d second=%d err=%v", first, second, err)
	}
	if _, err := s.db.Exec(`DROP TABLE _ready_check`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyCheck(ctx); err == nil {
		t.Fatal("readiness must not recreate schema at probe time")
	}
}

func TestReadyCheck_ReadOnlyDatabaseStillHasReadableStats(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.ReadyCheck(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA query_only = ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stats(ctx, time.Now()); err != nil {
		t.Fatalf("read-only statistics should still succeed: %v", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := s.ReadyCheck(checkCtx); err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("read-only store reported ready: %v", err)
	}
	if err := s.SaveHeartbeatError(checkCtx, HeartbeatLog{Target: "kuma", Error: "timeout"}); err == nil {
		t.Fatal("read-only database accepted an audit write")
	}
	logs, err := s.ListHeartbeatErrors(ctx, 1)
	if err != nil || len(logs) != 0 {
		t.Fatalf("failed write left a heartbeat record: logs=%+v err=%v", logs, err)
	}
}

func TestSQLiteExternalWriter_ContentionIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writer.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer other.Exec(`ROLLBACK`)
	for _, check := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"audit", func(ctx context.Context) error {
			return s.SaveHeartbeatError(ctx, HeartbeatLog{Target: "kuma", Error: "request timeout"})
		}},
		{"readiness", s.ReadyCheck},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		start := time.Now()
		err := check.run(ctx)
		duration := time.Since(start)
		cancel()
		if err == nil || duration > 200*time.Millisecond {
			t.Fatalf("external write lock exceeded %s budget: duration=%s err=%v", check.name, duration, err)
		}
		t.Logf("external writer held lock; %s failure returned in %s (%v)", check.name, duration, err)
		var busy int
		if err := s.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != sqliteBusyTimeoutMS {
			t.Fatalf("default contention policy not restored: timeout=%d err=%v", busy, err)
		}
	}
	if _, err := other.Exec(`ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadyCheck(context.Background()); err != nil {
		t.Fatalf("store did not recover after lock release: %v", err)
	}
}

// The helper acknowledges committed state while retaining its live SQLite
// connection. The parent kills the process without Close or a WAL checkpoint.
func TestSQLiteCrashHelper(t *testing.T) {
	if os.Getenv("WEBHOOK_STORE_CRASH_HELPER") != "1" {
		return
	}
	s, err := Open(os.Getenv("WEBHOOK_STORE_CRASH_DB"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.SavePendingEventGuarded(ctx, "crash-event", "src", "", []byte(`{"message":"durable"}`), now,
		"delivery-one", "guard-hash", "config", `{"actions":[]}`, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimPending(ctx, time.Hour, now, "crashed-owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.PrepareEvent(ctx, "crash-event", `{"message":"durable"}`, 2, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, "crash-event", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, "crash-event", 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, "crash-event", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveActionAttemptLog(ctx, "crash-event", 0, 0, "http", "target", 1, true, "ok", time.Millisecond, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveHeartbeatError(ctx, HeartbeatLog{Target: "kuma", Error: "request timeout", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	fmt.Println("committed")
	for {
		time.Sleep(time.Hour)
	}
}

func TestSQLiteCommittedState_SurvivesProcessKill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSQLiteCrashHelper$")
	cmd.Env = append(os.Environ(), "WEBHOOK_STORE_CRASH_HELPER=1", "WEBHOOK_STORE_CRASH_DB="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "committed\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("helper did not confirm its commit: output=%q err=%v stderr=%s", line, err, stderr.String())
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("helper has no uncheckpointed WAL: info=%v err=%v", info, err)
	}
	for _, filename := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("database/sidecar permissions: file=%s info=%v err=%v", filename, info, err)
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper was expected to exit through process kill")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var integrity string
	if err := s.db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%q err=%v", integrity, err)
	}
	if n, err := s.RequeueAllProcessing(context.Background()); err != nil || n != 1 {
		t.Fatalf("startup recovery=%d err=%v", n, err)
	}
	ev, err := s.ClaimPending(context.Background(), time.Minute, time.Now(), "new-owner")
	if err != nil || ev == nil || ev.ID != "crash-event" || ev.VarsJSON != `{"message":"durable"}` || len(ev.Payload) != 0 {
		t.Fatalf("committed event/vars lost: ev=%+v err=%v", ev, err)
	}
	states, err := s.ListActionStates(context.Background(), "crash-event")
	if err != nil || len(states) != 2 || states[0].Status != "done" || states[1].Status != "pending" || states[1].AttemptCount != 0 {
		t.Fatalf("committed action state lost: %+v err=%v", states, err)
	}
	if logs, err := s.ListActionLogs(context.Background(), "crash-event", 10); err != nil || len(logs) != 1 || !logs[0].Success {
		t.Fatalf("action audit lost: %+v err=%v", logs, err)
	}
	if logs, err := s.ListHeartbeatErrors(context.Background(), 10); err != nil || len(logs) != 1 || logs[0].Target != "kuma" {
		t.Fatalf("heartbeat audit lost: %+v err=%v", logs, err)
	}
	now := time.Now()
	err = s.SavePendingEventGuarded(context.Background(), "duplicate", "src", "", []byte(`{}`), now,
		"delivery-two", "guard-hash", "", "", now.Add(time.Hour))
	if !errors.Is(err, ErrDuplicateBody) {
		t.Fatalf("committed replay guard lost: %v", err)
	}
}
