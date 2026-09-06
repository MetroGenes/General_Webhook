package store

import (
	"context"
	"testing"
	"time"
)

func prepareRunningEvent(t *testing.T, s *Store, id string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.SaveEvent(ctx, id, "src", "", []byte(`{}`), now); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, now, "old-owner")
	if err != nil || ev == nil || ev.ID != id {
		t.Fatalf("claim %q: ev=%+v err=%v", id, ev, err)
	}
	if err := s.PrepareEvent(ctx, id, `{}`, 2, true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, id, 0, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteAction(ctx, id, 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartAction(ctx, id, 1, now); err != nil {
		t.Fatal(err)
	}
}

func TestClaimPending_RecoversExpiredActionAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	prepareRunningEvent(t, s, "event", now)
	if ev, err := s.ClaimPending(ctx, time.Minute, now.Add(30*time.Second), "new-owner"); err != nil || ev != nil {
		t.Fatalf("active lease was reclaimed: ev=%+v err=%v", ev, err)
	}
	states, err := s.ListActionStates(ctx, "event")
	if err != nil || states[1].Status != "running" || states[1].AttemptCount != 1 {
		t.Fatalf("active action changed: %+v err=%v", states, err)
	}

	// Abort only action recovery to prove that the newly claimed lease is rolled
	// back as well; a partial lease change must never escape this transaction.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_action_recovery BEFORE UPDATE ON event_actions
		WHEN OLD.status = 'running' AND NEW.status = 'pending'
		BEGIN SELECT RAISE(ABORT, 'injected recovery failure'); END`); err != nil {
		t.Fatal(err)
	}
	expired := now.Add(2 * time.Minute)
	if ev, err := s.ClaimPending(ctx, time.Minute, expired, "new-owner"); err == nil || ev != nil {
		t.Fatalf("claim survived recovery failure: ev=%+v err=%v", ev, err)
	}
	var owner string
	if err := s.db.QueryRow(`SELECT lease_owner FROM events WHERE id = 'event'`).Scan(&owner); err != nil || owner != "old-owner" {
		t.Fatalf("lease partially committed: owner=%q err=%v", owner, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_action_recovery`); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, expired, "new-owner")
	if err != nil || ev == nil {
		t.Fatalf("direct recovery claim: ev=%+v err=%v", ev, err)
	}
	states, err = s.ListActionStates(ctx, "event")
	if err != nil || len(states) != 2 || states[0].Status != "done" || states[0].AttemptCount != 1 ||
		states[1].Status != "pending" || states[1].AttemptCount != 0 {
		t.Fatalf("recovered actions=%+v err=%v", states, err)
	}
	if attempt, err := s.StartAction(ctx, "event", 1, expired); err != nil || attempt != 1 {
		t.Fatalf("recovered action cannot run: attempt=%d err=%v", attempt, err)
	}
}

func TestRequeueAllProcessing_RepairsOrphanedRunningActions(t *testing.T) {
	for _, parentStatus := range []string{"pending", "retrying"} {
		t.Run(parentStatus, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			prepareRunningEvent(t, s, "orphan", now)
			if _, err := s.db.Exec(`UPDATE events SET status = ?, next_attempt_at_ms = ?, lease_owner = NULL, lease_until_ms = NULL WHERE id = 'orphan'`,
				parentStatus, now.Add(24*time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			// A normal scheduled retry keeps its future due time during startup.
			if err := s.SaveEvent(ctx, "scheduled", "src", "", []byte(`{}`), now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE events SET status = 'retrying', next_attempt_at_ms = ? WHERE id = 'scheduled'`, now.Add(time.Hour).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if n, err := s.RequeueAllProcessing(ctx); err != nil || n != 1 {
				t.Fatalf("recovered=%d err=%v", n, err)
			}
			states, err := s.ListActionStates(ctx, "orphan")
			if err != nil || states[0].Status != "done" || states[1].Status != "pending" || states[1].AttemptCount != 0 {
				t.Fatalf("states=%+v err=%v", states, err)
			}
			if n, err := s.RequeueAllProcessing(ctx); err != nil || n != 0 {
				t.Fatalf("recovery not idempotent: n=%d err=%v", n, err)
			}
			ev, err := s.ClaimPending(ctx, time.Minute, now, "new-owner")
			if err != nil || ev == nil || ev.ID != "orphan" {
				t.Fatalf("orphan was not made immediately runnable: ev=%+v err=%v", ev, err)
			}
			if ev, err := s.ClaimPending(ctx, time.Minute, now, "new-owner"); err != nil || ev != nil {
				t.Fatalf("normal future retry was disturbed: ev=%+v err=%v", ev, err)
			}
		})
	}
}

func TestExpireAction_DoesNotInventAttemptAndReplayResetsAge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	prepareRunningEvent(t, s, "event", now)
	if err := s.ScheduleActionRetry(ctx, "event", 1, now.Add(time.Hour), "temporary", 0); err != nil {
		t.Fatal(err)
	}
	if status, err := s.RefreshEventStatus(ctx, "event", now); err != nil || status != "retrying" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	expired := now.Add(2 * time.Hour)
	if _, err := s.ClaimPending(ctx, time.Minute, expired, "new-owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireAction(ctx, "event", 1, expired); err != nil {
		t.Fatal(err)
	}
	if err := s.ExpireAction(ctx, "event", 0, expired); err == nil {
		t.Fatal("completed action must not be expired")
	}
	states, err := s.ListActionStates(ctx, "event")
	if err != nil || states[0].Status != "done" || states[1].Status != "dead" || states[1].AttemptCount != 1 || states[1].LastError != "maximum retry age exceeded" {
		t.Fatalf("expired states=%+v err=%v", states, err)
	}
	if logs, err := s.ListActionLogs(ctx, "event", 10); err != nil || len(logs) != 0 {
		t.Fatalf("expiration invented dispatch logs: %+v err=%v", logs, err)
	}
	if status, err := s.RefreshEventStatus(ctx, "event", expired); err != nil || status != "dead" {
		t.Fatalf("final status=%s err=%v", status, err)
	}
	replayAt := expired.Add(time.Minute)
	if _, err := s.ReplayDeadEvent(ctx, "event", "manual", replayAt); err != nil {
		t.Fatal(err)
	}
	ev, err := s.ClaimPending(ctx, time.Minute, replayAt, "new-owner")
	if err != nil || ev == nil || !ev.RetryStartedAt.Equal(replayAt) {
		t.Fatalf("replay age not reset: ev=%+v err=%v", ev, err)
	}
	states, err = s.ListActionStates(ctx, "event")
	if err != nil || states[0].Status != "done" || states[1].Status != "pending" || states[1].AttemptCount != 0 {
		t.Fatalf("replay states=%+v err=%v", states, err)
	}
}
