package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动, 无需 CGO
)

// Store 用 SQLite 保存事件审计与 outbox 任务。
type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir store dir: %w", err)
		}
		_ = os.Chmod(dir, 0o700)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite 单写, 串行化避免 "database is locked"

	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		_ = db.Close()
		return nil, fmt.Errorf("chmod store: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) migrate() error {
	const base = `
CREATE TABLE IF NOT EXISTS events (
    id                   TEXT PRIMARY KEY,
    source               TEXT NOT NULL,
    remote_ip            TEXT,
    payload              BLOB,
    status               TEXT NOT NULL DEFAULT 'pending',
    received_at          DATETIME NOT NULL,
    processed_at         DATETIME,
    external_delivery_id TEXT,
    body_sha256          TEXT,
    lease_until          DATETIME
);
CREATE TABLE IF NOT EXISTS action_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id    TEXT NOT NULL,
    action      TEXT NOT NULL,
    target      TEXT,
    attempt     INTEGER NOT NULL,
    success     INTEGER NOT NULL,
    detail      TEXT,
    duration_ms INTEGER,
    created_at  DATETIME NOT NULL
);
`
	if _, err := s.db.Exec(base); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := s.ensureColumns(); err != nil {
		return err
	}
	// Drop legacy body-hash uniqueness: identical payloads with distinct delivery IDs are valid.
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_events_hmac_body`); err != nil {
		return fmt.Errorf("migrate drop hmac body index: %w", err)
	}
	// Upgrade legacy statuses into the outbox state machine.
	// received: never processed under the old model → pending
	// rejected: was queue-full at accept time; payload is durable → pending for reprocessing
	if _, err := s.db.Exec(`UPDATE events SET status = 'pending' WHERE status IN ('received', 'rejected')`); err != nil {
		return fmt.Errorf("migrate legacy status: %w", err)
	}
	const indexes = `
CREATE INDEX IF NOT EXISTS idx_events_source ON events(source);
CREATE INDEX IF NOT EXISTS idx_events_status ON events(status);
CREATE INDEX IF NOT EXISTS idx_events_lease ON events(status, lease_until);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_delivery
    ON events(source, external_delivery_id)
    WHERE external_delivery_id IS NOT NULL AND external_delivery_id != '';
CREATE INDEX IF NOT EXISTS idx_action_event ON action_logs(event_id);
`
	if _, err := s.db.Exec(indexes); err != nil {
		return fmt.Errorf("migrate indexes: %w", err)
	}
	return nil
}

func (s *Store) ensureColumns() error {
	cols := map[string]string{
		"external_delivery_id": "TEXT",
		"body_sha256":          "TEXT",
		"lease_until":          "DATETIME",
	}
	existing, err := s.columnSet("events")
	if err != nil {
		return err
	}
	for name, typ := range cols {
		if existing[name] {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE events ADD COLUMN %s %s`, name, typ)); err != nil {
			return fmt.Errorf("migrate column %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) columnSet(table string) (map[string]bool, error) {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ErrDuplicateEvent indicates the event id or delivery already exists.
var ErrDuplicateEvent = fmt.Errorf("duplicate event")

// PendingEvent is a claimed outbox job.
type PendingEvent struct {
	ID                 string
	Source             string
	RemoteIP           string
	Payload            []byte
	ExternalDeliveryID string
	ReceivedAt         time.Time
}

// SavePendingEvent inserts a new pending outbox event.
// externalDeliveryID and bodySHA256 may be empty; non-empty values participate in uniqueness.
func (s *Store) SavePendingEvent(ctx context.Context, id, source, ip string, payload []byte, at time.Time, externalDeliveryID, bodySHA256 string) error {
	var delivery any
	if externalDeliveryID != "" {
		delivery = externalDeliveryID
	}
	var digest any
	if bodySHA256 != "" {
		digest = bodySHA256
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO events
		 (id, source, remote_ip, payload, status, received_at, external_delivery_id, body_sha256)
		 VALUES (?, ?, ?, ?, 'pending', ?, ?, ?)`,
		id, source, ip, payload, at, delivery, digest)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDuplicateEvent
	}
	return nil
}

// SaveEvent is kept for tests and inserts a received/pending-compatible row without delivery identity.
func (s *Store) SaveEvent(ctx context.Context, id, source, ip string, payload []byte, at time.Time) error {
	return s.SavePendingEvent(ctx, id, source, ip, payload, at, "", "")
}

// ClaimPending marks one pending (or lease-expired processing) event as processing and returns it.
func (s *Store) ClaimPending(ctx context.Context, lease time.Duration, now time.Time) (*PendingEvent, error) {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	leaseUntil := now.Add(lease)
	row := tx.QueryRowContext(ctx, `
		SELECT id, source, remote_ip, payload, COALESCE(external_delivery_id, ''), received_at
		FROM events
		WHERE status = 'pending'
		   OR (status = 'processing' AND (lease_until IS NULL OR lease_until < ?))
		ORDER BY received_at ASC
		LIMIT 1`, now)
	var ev PendingEvent
	var receivedAt time.Time
	if err := row.Scan(&ev.ID, &ev.Source, &ev.RemoteIP, &ev.Payload, &ev.ExternalDeliveryID, &receivedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	ev.ReceivedAt = receivedAt

	res, err := tx.ExecContext(ctx,
		`UPDATE events SET status = 'processing', lease_until = ?
		 WHERE id = ? AND (status = 'pending' OR status = 'processing')`,
		leaseUntil, ev.ID)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}

// RequeueExpiredLeases resets processing jobs whose lease expired back to pending.
func (s *Store) RequeueExpiredLeases(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET status = 'pending', lease_until = NULL
		 WHERE status = 'processing' AND (lease_until IS NULL OR lease_until < ?)`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// HasEvent reports whether an event id already exists.
func (s *Store) HasEvent(ctx context.Context, id string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, id).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// UpdateEventStatus 更新事件最终状态 (done/partial/skipped/error)。
func (s *Store) UpdateEventStatus(ctx context.Context, id, status string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET status = ?, processed_at = ?, lease_until = NULL WHERE id = ?`, status, at, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("event %q not found", id)
	}
	return nil
}

// SaveActionLog 记录一次动作执行结果。
func (s *Store) SaveActionLog(ctx context.Context, eventID, action, target string, attempt int, success bool, detail string, dur time.Duration, at time.Time) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, eventID).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("event %q not found", eventID)
	}
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO action_logs (event_id, action, target, attempt, success, detail, duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, action, target, attempt, boolToInt(success), detail, dur.Milliseconds(), at)
	return err
}

// GetPayload 返回事件原始 payload, 用于重放。
func (s *Store) GetPayload(ctx context.Context, id string) (payload []byte, source string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT payload, source FROM events WHERE id = ?`, id).Scan(&payload, &source)
	return
}

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// ReadyCheck verifies the store can write (readiness).
func (s *Store) ReadyCheck(ctx context.Context) error {
	if err := s.Ping(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _ready_check (id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO _ready_check(id) VALUES (1) ON CONFLICT(id) DO UPDATE SET id = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

// PurgeOlderThan deletes terminal events (and cascaded action logs) older than cutoff.
// pending/processing rows are retained so long downtime cannot drop unfinished work.
func (s *Store) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	const terminal = `status IN ('done', 'partial', 'skipped', 'error')`
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM action_logs WHERE event_id IN (SELECT id FROM events WHERE received_at < ? AND `+terminal+`)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE received_at < ? AND `+terminal, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// RenewLease extends the lease on a processing event owned by the current worker.
func (s *Store) RenewLease(ctx context.Context, id string, lease time.Duration, now time.Time) error {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET lease_until = ? WHERE id = ? AND status = 'processing'`,
		now.Add(lease), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("event %q not processing", id)
	}
	return nil
}

// ClearPayload removes the stored payload after processing (privacy retention).
func (s *Store) ClearPayload(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE id = ?`, id)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
