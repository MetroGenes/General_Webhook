package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "webhook.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedSnapshots(t *testing.T, s *Store, snapshots map[string]string) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now()
	for id, raw := range snapshots {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(id, source, received_at, received_at_ms, actions_json)
			VALUES (?, 'test', ?, ?, ?)`, id, now, now.UnixMilli(), nullable(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRewriteActionSnapshots_PagesKeepOrderAndProgress(t *testing.T) {
	s := newTestStore(t)
	snapshots := map[string]string{"": "raw:"}
	for i := 0; i < snapshotRewriteBatchRows*2+7; i++ {
		id := fmt.Sprintf("event-%04d", i)
		if i%11 == 0 {
			snapshots[id] = "" // Null/empty snapshots do not affect the cursor.
		} else {
			snapshots[id] = "raw:" + id
		}
	}
	seedSnapshots(t, s, snapshots)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var visited []string
	expected := make(map[string]string, len(snapshots))
	for id, raw := range snapshots {
		expected[id] = raw
	}
	changed, err := s.RewriteActionSnapshots(ctx, func(raw string) (string, error) {
		id := strings.TrimPrefix(raw, "raw:")
		if ok, err := s.HasEvent(ctx, id); err != nil || !ok {
			return "", fmt.Errorf("callback cannot use the store: exists=%v err=%w", ok, err)
		}
		visited = append(visited, id)
		updated := "rewritten:" + id
		if len(visited)%3 == 0 {
			updated = "" // Removing rows from the scan must not skip later IDs.
		}
		expected[id] = updated
		return updated, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCount := 0
	for _, raw := range snapshots {
		if raw != "" {
			wantCount++
		}
	}
	if changed != int64(wantCount) || len(visited) != wantCount || !sort.StringsAreSorted(visited) {
		t.Fatalf("changed=%d visited=%d sorted=%v want=%d", changed, len(visited), sort.StringsAreSorted(visited), wantCount)
	}
	for i := 1; i < len(visited); i++ {
		if visited[i] == visited[i-1] {
			t.Fatalf("repeated ID %q", visited[i])
		}
	}
	for id, want := range expected {
		var got string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(actions_json, '') FROM events WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("snapshot %q=%q want=%q", id, got, want)
		}
	}
}

func TestActionSnapshotBatch_LimitsRowsAndBytes(t *testing.T) {
	t.Run("rows", func(t *testing.T) {
		s := newTestStore(t)
		snapshots := make(map[string]string)
		for i := 0; i < snapshotRewriteBatchRows+1; i++ {
			snapshots[fmt.Sprintf("id-%04d", i)] = `{"actions":[]}`
		}
		seedSnapshots(t, s, snapshots)
		batch, err := s.actionSnapshotBatch(context.Background(), "", false)
		if err != nil || len(batch) != snapshotRewriteBatchRows {
			t.Fatalf("batch rows=%d err=%v", len(batch), err)
		}
		next, err := s.actionSnapshotBatch(context.Background(), batch[len(batch)-1].id, true)
		if err != nil || len(next) != 1 {
			t.Fatalf("next rows=%d err=%v", len(next), err)
		}
	})
	t.Run("bytes and oversized record", func(t *testing.T) {
		s := newTestStore(t)
		snapshots := make(map[string]string)
		for i := 0; i < 9; i++ {
			snapshots[fmt.Sprintf("id-%04d", i)] = strings.Repeat("s", snapshotRewriteBatchBytes/4)
		}
		snapshots["id-0004"] = strings.Repeat("large", snapshotRewriteBatchBytes/2)
		seedSnapshots(t, s, snapshots)
		var cursor string
		haveCursor := false
		seen := 0
		sawOversized := false
		for {
			batch, err := s.actionSnapshotBatch(context.Background(), cursor, haveCursor)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch) == 0 {
				break
			}
			var bytes int
			for _, row := range batch {
				bytes += len(row.id) + len(row.raw)
			}
			if bytes > snapshotRewriteBatchBytes {
				if len(batch) != 1 || batch[0].id != "id-0004" {
					t.Fatalf("unbounded batch: rows=%d bytes=%d", len(batch), bytes)
				}
				sawOversized = true
			}
			seen += len(batch)
			cursor, haveCursor = batch[len(batch)-1].id, true
		}
		if seen != len(snapshots) || !sawOversized {
			t.Fatalf("seen=%d want=%d oversized=%v", seen, len(snapshots), sawOversized)
		}
	})
}

func TestRewriteActionSnapshots_BoundedLiveHistory(t *testing.T) {
	s := newTestStore(t)
	const count = 1024
	raw := strings.Repeat("s", 32*1024)
	snapshots := make(map[string]string, count)
	for i := 0; i < count; i++ {
		snapshots[fmt.Sprintf("id-%05d", i)] = raw
	}
	seedSnapshots(t, s, snapshots)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	visits := 0
	changed, err := s.RewriteActionSnapshots(context.Background(), func(raw string) (string, error) {
		if visits == 0 {
			runtime.GC()
			var atFirstCallback runtime.MemStats
			runtime.ReadMemStats(&atFirstCallback)
			growth := int64(atFirstCallback.HeapAlloc) - int64(before.HeapAlloc)
			t.Logf("32 MiB history, live heap growth before first callback: %d bytes", growth)
			if growth > 8<<20 {
				return "", fmt.Errorf("history buffered in memory: heap grew %d bytes", growth)
			}
		}
		visits++
		return raw, nil
	})
	if err != nil || changed != 0 || visits != count {
		t.Fatalf("changed=%d visits=%d err=%v", changed, visits, err)
	}
}

func TestRewriteActionSnapshots_PartialFailureAndCancellation(t *testing.T) {
	s := newTestStore(t)
	seedSnapshots(t, s, map[string]string{"a": "old-a", "b": "old-b", "c": "old-c"})
	wantErr := errors.New("stop rewrite")
	changed, err := s.RewriteActionSnapshots(context.Background(), func(raw string) (string, error) {
		if raw == "old-b" {
			return "", wantErr
		}
		return "new-" + raw, nil
	})
	if !errors.Is(err, wantErr) || changed != 1 {
		t.Fatalf("changed=%d err=%v", changed, err)
	}
	var raw string
	if err := s.db.QueryRow(`SELECT actions_json FROM events WHERE id='c'`).Scan(&raw); err != nil || raw != "old-c" {
		t.Fatalf("unvisited row=%q err=%v", raw, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	changed, err = s.RewriteActionSnapshots(ctx, func(raw string) (string, error) {
		t.Fatal("callback ran after cancellation")
		return raw, nil
	})
	if !errors.Is(err, context.Canceled) || changed != 0 {
		t.Fatalf("cancel changed=%d err=%v", changed, err)
	}
}
