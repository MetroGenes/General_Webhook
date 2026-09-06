package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHeartbeatErrorLogs_SanitizeBeforePersistence(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Millisecond)
	err := s.SaveHeartbeatError(context.Background(), HeartbeatLog{
		Target:     strings.Repeat("目标", 100),
		Origin:     "https://FAKE-USER:FAKE-PASSWORD@kuma.example:8443/api/push/FAKE-PATH?token=FAKE-QUERY#FAKE-FRAGMENT",
		Mode:       strings.Repeat("kuma", 100),
		Health:     strings.Repeat("down", 100),
		Error:      `GET "HTTPS://FAKE-USER:FAKE-PASSWORD@kuma.example/api/push/FAKE-PATH?token=FAKE-QUERY": bad TLS; https://kuma.example/FAKE-PATH/%zz ` + strings.Repeat("故障\n", 2000),
		HTTPStatus: 700,
		DurationMS: -1,
		CreatedAt:  now,
	})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListHeartbeatErrors(context.Background(), 1)
	if err != nil || len(logs) != 1 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	entry := logs[0]
	if entry.Origin != "https://kuma.example:8443" || !entry.CreatedAt.Equal(now) || entry.ID <= 0 {
		t.Fatalf("unexpected metadata: %+v", entry)
	}
	if len(entry.Target) > heartbeatTargetMaxBytes || len(entry.Origin) > heartbeatOriginMaxBytes ||
		len(entry.Error) > heartbeatErrorMaxBytes || len(entry.Mode) > 32 || len(entry.Health) > 32 {
		t.Fatalf("unbounded audit record lengths: target=%d origin=%d error=%d mode=%d health=%d",
			len(entry.Target), len(entry.Origin), len(entry.Error), len(entry.Mode), len(entry.Health))
	}
	if !utf8.ValidString(entry.Error) || !utf8.ValidString(entry.Target) || strings.ContainsAny(entry.Error, "\r\n\t") {
		t.Fatal("audit record has invalid UTF-8 or control characters")
	}
	if entry.HTTPStatus != 0 || entry.DurationMS != 0 {
		t.Fatalf("invalid numeric metadata not normalized: %+v", entry)
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"FAKE-USER", "FAKE-PASSWORD", "FAKE-PATH", "FAKE-QUERY", "FAKE-FRAGMENT"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("audit output contains %s", secret)
		}
	}
	var storedOrigin, storedError string
	if err := s.db.QueryRow(`SELECT origin, error FROM heartbeat_logs WHERE id = ?`, entry.ID).Scan(&storedOrigin, &storedError); err != nil {
		t.Fatal(err)
	}
	if storedOrigin != entry.Origin || storedError != entry.Error {
		t.Fatal("redaction must happen before persistence, not only when reading logs")
	}
	for _, key := range []string{`"http_status":`, `"duration_ms":`, `"created_at":`} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("missing JSON key %s", key)
		}
	}
}

func TestHeartbeatErrorLogs_OrderingAndLimit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 120; i++ {
		if err := s.SaveHeartbeatError(context.Background(), HeartbeatLog{
			Target: fmt.Sprintf("monitor-%03d", i), Origin: "http://kuma:3001", Mode: "uptime-kuma",
			Health: "up", Error: "request timeout", HTTPStatus: 504, DurationMS: 25,
			CreatedAt: now.Add(time.Duration(i%5) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ limit, want int }{{1, 1}, {0, 50}, {-1, 50}, {1000, 100}} {
		logs, err := s.ListHeartbeatErrors(context.Background(), tc.limit)
		if err != nil || len(logs) != tc.want {
			t.Fatalf("limit=%d got=%d want=%d err=%v", tc.limit, len(logs), tc.want, err)
		}
		for i := 1; i < len(logs); i++ {
			previous, current := logs[i-1], logs[i]
			if previous.CreatedAt.Before(current.CreatedAt) || (previous.CreatedAt.Equal(current.CreatedAt) && previous.ID <= current.ID) {
				t.Fatalf("unstable order at %d: previous=%+v current=%+v", i, previous, current)
			}
		}
	}
}

func TestHeartbeatErrorLogs_RetentionUsesSameCutoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cutoff := time.Now().UTC().Truncate(time.Millisecond)
	for _, at := range []time.Time{cutoff.Add(-time.Millisecond), cutoff, cutoff.Add(time.Millisecond)} {
		if err := s.SaveHeartbeatError(ctx, HeartbeatLog{Target: "kuma", Error: "timeout", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	old := cutoff.Add(-time.Hour)
	if err := s.SaveEvent(ctx, "old-terminal", "src", "", nil, old); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateEventStatus(ctx, "old-terminal", "done", old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEvent(ctx, "old-pending", "src", "", nil, old); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeOlderThan(ctx, cutoff); err != nil || n != 1 {
		t.Fatalf("event purge count=%d err=%v; heartbeat rows must not alter the existing event count", n, err)
	}
	logs, err := s.ListHeartbeatErrors(ctx, 10)
	if err != nil || len(logs) != 2 || !logs[1].CreatedAt.Equal(cutoff) {
		t.Fatalf("retained=%+v err=%v", logs, err)
	}
	if exists, err := s.HasEvent(ctx, "old-pending"); err != nil || !exists {
		t.Fatalf("pending event lost: exists=%v err=%v", exists, err)
	}
}

func TestHeartbeatErrorLogs_RespectConnectionDeadline(t *testing.T) {
	s := newTestStore(t)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.SaveHeartbeatError(ctx, HeartbeatLog{Target: "kuma", Error: "timeout"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("blocked audit write: duration=%s err=%v", time.Since(start), err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err := s.ListHeartbeatErrors(context.Background(), 10)
	if err != nil || len(logs) != 0 {
		t.Fatalf("canceled write persisted later: logs=%+v err=%v", logs, err)
	}
}

func TestHeartbeatErrorLogs_MalformedOriginIsNotStored(t *testing.T) {
	s := newTestStore(t)
	for _, origin := range []string{"https://kuma.example/FAKE-TOKEN/%zz", "not a URL", "file:///FAKE-TOKEN", "https://" + strings.Repeat("secret", 2000)} {
		if err := s.SaveHeartbeatError(context.Background(), HeartbeatLog{Origin: origin, Error: "failed"}); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.ListHeartbeatErrors(context.Background(), 10)
	if err != nil || len(logs) != 4 {
		t.Fatalf("logs=%+v err=%v", logs, err)
	}
	for _, entry := range logs {
		if entry.Origin != "" || entry.CreatedAt.IsZero() {
			t.Fatalf("invalid origin/default timestamp: %+v", entry)
		}
	}
}
