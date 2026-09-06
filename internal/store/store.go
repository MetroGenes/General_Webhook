package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver
)

// Store persists accepted events, per-action retry state, and audit records.
type Store struct {
	db *sql.DB
}

// SQLite serializes this single-instance store through one connection. WAL
// keeps readers from blocking commits; FULL synchronizes each accepted write.
// Keep external-writer contention short, including during health checks.
const sqliteBusyTimeoutMS = 250

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		created := false
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			created = true
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir store dir: %w", err)
		}
		if created {
			_ = os.Chmod(dir, 0o700)
		}
	}
	if err := prepareStoreFile(path); err != nil {
		return nil, err
	}
	dsn, err := sqliteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := tightenStorePermissions(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteDSN(path string) (string, error) {
	pragmas := url.Values{"_pragma": {
		fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMS),
		"journal_mode(WAL)",
		"synchronous(FULL)",
	}}
	// URI escaping preserves literal '?' and '#' in ordinary file names, while
	// DSN pragmas are reapplied if database/sql has to replace the connection.
	if path == ":memory:" {
		return path + "?" + pragmas.Encode(), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve store path: %w", err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: pragmas.Encode()}
	return u.String(), nil
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
    lease_until          DATETIME,
    lease_owner          TEXT,
    config_hash          TEXT,
    actions_json         TEXT,
    vars_json            TEXT,
    received_at_ms       INTEGER,
    processed_at_ms      INTEGER,
    lease_until_ms       INTEGER,
    next_attempt_at_ms   INTEGER,
    replay_generation    INTEGER NOT NULL DEFAULT 0,
    replayed_at_ms       INTEGER,
    replay_reason        TEXT
);
CREATE TABLE IF NOT EXISTS action_logs (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id          TEXT NOT NULL,
    action            TEXT NOT NULL,
    action_index      INTEGER NOT NULL DEFAULT -1,
    replay_generation INTEGER NOT NULL DEFAULT 0,
    target            TEXT,
    attempt           INTEGER NOT NULL,
    success           INTEGER NOT NULL,
    detail            TEXT,
    duration_ms       INTEGER,
    created_at        DATETIME NOT NULL,
    created_at_ms     INTEGER
);
CREATE TABLE IF NOT EXISTS event_actions (
    event_id           TEXT NOT NULL,
    action_index       INTEGER NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending',
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    next_attempt_at_ms INTEGER,
    last_error         TEXT,
    retry_after_ms     INTEGER,
    completed_at_ms    INTEGER,
    PRIMARY KEY(event_id, action_index)
);
CREATE TABLE IF NOT EXISTS replay_guard (
    source        TEXT NOT NULL,
    body_sha256   TEXT NOT NULL,
    expires_at_ms INTEGER NOT NULL,
    PRIMARY KEY(source, body_sha256)
);
CREATE TABLE IF NOT EXISTS _ready_check (id INTEGER PRIMARY KEY, nonce INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS heartbeat_logs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    target        TEXT NOT NULL,
    origin        TEXT NOT NULL,
    mode          TEXT NOT NULL,
    health        TEXT NOT NULL,
    error         TEXT NOT NULL,
    http_status   INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL,
    created_at    DATETIME NOT NULL,
    created_at_ms INTEGER NOT NULL
);
`
	if _, err := s.db.Exec(base); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := s.ensureColumns("events", map[string]string{
		"external_delivery_id": "TEXT", "body_sha256": "TEXT", "lease_until": "DATETIME",
		"lease_owner": "TEXT", "config_hash": "TEXT", "actions_json": "TEXT", "vars_json": "TEXT",
		"received_at_ms": "INTEGER", "processed_at_ms": "INTEGER", "lease_until_ms": "INTEGER",
		"next_attempt_at_ms": "INTEGER", "replay_generation": "INTEGER NOT NULL DEFAULT 0",
		"replayed_at_ms": "INTEGER", "replay_reason": "TEXT",
	}); err != nil {
		return err
	}
	if err := s.ensureColumns("action_logs", map[string]string{
		"action_index": "INTEGER NOT NULL DEFAULT -1", "replay_generation": "INTEGER NOT NULL DEFAULT 0",
		"created_at_ms": "INTEGER",
	}); err != nil {
		return err
	}
	if err := s.ensureColumns("_ready_check", map[string]string{"nonce": "INTEGER NOT NULL DEFAULT 0"}); err != nil {
		return err
	}
	if err := s.backfillEpochColumns(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_events_hmac_body`); err != nil {
		return fmt.Errorf("migrate drop hmac body index: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE events SET status = 'pending' WHERE status IN ('received', 'rejected')`); err != nil {
		return fmt.Errorf("migrate legacy status: %w", err)
	}
	const indexes = `
CREATE INDEX IF NOT EXISTS idx_events_source ON events(source);
CREATE INDEX IF NOT EXISTS idx_events_status_due ON events(status, next_attempt_at_ms, received_at_ms);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_delivery
    ON events(source, external_delivery_id)
    WHERE external_delivery_id IS NOT NULL AND external_delivery_id != '';
CREATE INDEX IF NOT EXISTS idx_events_body_hash ON events(source, body_sha256, received_at_ms);
CREATE INDEX IF NOT EXISTS idx_action_event ON action_logs(event_id);
CREATE INDEX IF NOT EXISTS idx_action_log_created ON action_logs(created_at_ms);
CREATE INDEX IF NOT EXISTS idx_event_actions_due ON event_actions(status, next_attempt_at_ms);
CREATE INDEX IF NOT EXISTS idx_heartbeat_log_created ON heartbeat_logs(created_at_ms DESC, id DESC);
`
	if _, err := s.db.Exec(indexes); err != nil {
		return fmt.Errorf("migrate indexes: %w", err)
	}
	return nil
}

func (s *Store) ensureColumns(table string, cols map[string]string) error {
	existing, err := s.columnSet(table)
	if err != nil {
		return err
	}
	for name, typ := range cols {
		if existing[name] {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, name, typ)); err != nil {
			return fmt.Errorf("migrate %s column %s: %w", table, name, err)
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
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) backfillEpochColumns() error {
	type eventTimeRow struct {
		id        string
		received  string
		processed sql.NullString
	}
	rows, err := s.db.Query(`SELECT id, CAST(received_at AS TEXT), CAST(processed_at AS TEXT)
		FROM events WHERE received_at_ms IS NULL OR (processed_at IS NOT NULL AND processed_at_ms IS NULL)`)
	if err != nil {
		return fmt.Errorf("query time backfill: %w", err)
	}
	var events []eventTimeRow
	for rows.Next() {
		var row eventTimeRow
		if err := rows.Scan(&row.id, &row.received, &row.processed); err != nil {
			_ = rows.Close()
			return err
		}
		events = append(events, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range events {
		received, err := parseSQLiteTime(row.received)
		if err != nil {
			return fmt.Errorf("parse received_at for %s: %w", row.id, err)
		}
		var processed any
		if row.processed.Valid && row.processed.String != "" {
			t, err := parseSQLiteTime(row.processed.String)
			if err != nil {
				return fmt.Errorf("parse processed_at for %s: %w", row.id, err)
			}
			processed = t.UnixMilli()
		}
		if _, err := s.db.Exec(`UPDATE events SET received_at_ms = ?, processed_at_ms = ? WHERE id = ?`, received.UnixMilli(), processed, row.id); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`UPDATE action_logs SET created_at_ms = CAST(strftime('%s', created_at) AS INTEGER) * 1000 WHERE created_at_ms IS NULL`); err != nil {
		return fmt.Errorf("backfill action log time: %w", err)
	}
	type actionLogTimeRow struct {
		id      int64
		created string
	}
	rows, err = s.db.Query(`SELECT id, CAST(created_at AS TEXT) FROM action_logs WHERE created_at_ms IS NULL`)
	if err != nil {
		return fmt.Errorf("query action log time backfill: %w", err)
	}
	var logs []actionLogTimeRow
	for rows.Next() {
		var row actionLogTimeRow
		if err := rows.Scan(&row.id, &row.created); err != nil {
			_ = rows.Close()
			return err
		}
		logs = append(logs, row)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range logs {
		created, err := parseSQLiteTime(row.created)
		if err != nil {
			return fmt.Errorf("parse action log created_at for %d: %w", row.id, err)
		}
		if _, err := s.db.Exec(`UPDATE action_logs SET created_at_ms = ? WHERE id = ?`, created.UnixMilli(), row.id); err != nil {
			return err
		}
	}
	return nil
}

var (
	ErrDuplicateEvent    = errors.New("duplicate event")
	ErrDuplicateBody     = errors.New("duplicate body")
	ErrReplayUnavailable = errors.New("event replay input unavailable")
	ErrEventNotRetryable = errors.New("event not retryable")
)

type PendingEvent struct {
	ID                 string
	Source             string
	RemoteIP           string
	Payload            []byte
	VarsJSON           string
	ExternalDeliveryID string
	ReceivedAt         time.Time
	RetryStartedAt     time.Time
	ConfigHash         string
	ActionsJSON        string
	ReplayGeneration   int
}

type EventMeta struct {
	ID                 string
	Source             string
	RemoteIP           string
	Status             string
	ReceivedAt         time.Time
	ProcessedAt        *time.Time
	ExternalDeliveryID string
	ConfigHash         string
	ReplayGeneration   int
	ReplayReason       string
}

type ActionState struct {
	Index         int    `json:"index"`
	Status        string `json:"status"`
	AttemptCount  int    `json:"attempt_count"`
	NextAttemptMS int64  `json:"next_attempt_at_ms,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

type ActionLogRow struct {
	Action           string
	ActionIndex      int
	ReplayGeneration int
	Target           string
	Attempt          int
	Success          bool
	Detail           string
	DurationMS       int64
	CreatedAt        time.Time
}

type QueueStats struct {
	Pending             int64   `json:"pending"`
	Processing          int64   `json:"processing"`
	Retrying            int64   `json:"retrying"`
	Dead                int64   `json:"dead"`
	Partial             int64   `json:"partial_legacy"`
	Error               int64   `json:"error_legacy"`
	Done                int64   `json:"done"`
	Skipped             int64   `json:"skipped"`
	ActionsRetrying     int64   `json:"actions_retrying"`
	ActionsDead         int64   `json:"actions_dead"`
	ActionAttempts5M    int64   `json:"action_attempts_5m"`
	ActionFailures5M    int64   `json:"action_failures_5m"`
	ActionFailureRate5M float64 `json:"action_failure_rate_5m"`
	ActionAttempts1H    int64   `json:"action_attempts_1h"`
	ActionFailures1H    int64   `json:"action_failures_1h"`
	ActionFailureRate1H float64 `json:"action_failure_rate_1h"`
	OldestPendingAgeMS  int64   `json:"oldest_pending_age_ms"`
}

func (s *Store) SavePendingEvent(ctx context.Context, id, source, ip string, payload []byte, at time.Time, externalDeliveryID, bodySHA256, configHash, actionsJSON string) error {
	return s.savePendingEvent(ctx, id, source, ip, payload, at, externalDeliveryID, bodySHA256, configHash, actionsJSON, time.Time{})
}

// SavePendingEventGuarded atomically reserves the HMAC body replay key and
// stores the event, closing the former check-then-insert race.
func (s *Store) SavePendingEventGuarded(ctx context.Context, id, source, ip string, payload []byte, at time.Time, externalDeliveryID, bodySHA256, configHash, actionsJSON string, replayExpiresAt time.Time) error {
	return s.savePendingEvent(ctx, id, source, ip, payload, at, externalDeliveryID, bodySHA256, configHash, actionsJSON, replayExpiresAt)
}

func (s *Store) savePendingEvent(ctx context.Context, id, source, ip string, payload []byte, at time.Time, externalDeliveryID, bodySHA256, configHash, actionsJSON string, replayExpiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if !replayExpiresAt.IsZero() && bodySHA256 != "" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM replay_guard WHERE source = ? AND body_sha256 = ? AND expires_at_ms <= ?`, source, bodySHA256, at.UnixMilli()); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replay_guard(source, body_sha256, expires_at_ms) VALUES (?, ?, ?)`, source, bodySHA256, replayExpiresAt.UnixMilli())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrDuplicateBody
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO events
		(id, source, remote_ip, payload, status, received_at, received_at_ms, next_attempt_at_ms,
		 external_delivery_id, body_sha256, config_hash, actions_json)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?)`,
		id, source, ip, payload, at, at.UnixMilli(), at.UnixMilli(), nullable(externalDeliveryID), nullable(bodySHA256), nullable(configHash), nullable(actionsJSON))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicateEvent
	}
	return tx.Commit()
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (s *Store) SaveEvent(ctx context.Context, id, source, ip string, payload []byte, at time.Time) error {
	return s.SavePendingEvent(ctx, id, source, ip, payload, at, "", "", "", "")
}

// HasRecentBodyHash is retained for compatibility; new acceptance paths use
// SavePendingEventGuarded so this query is not used as a security decision.
func (s *Store) HasRecentBodyHash(ctx context.Context, source, bodySHA256 string, since time.Time) (bool, error) {
	if bodySHA256 == "" {
		return false, nil
	}
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE source = ? AND body_sha256 = ? AND received_at_ms >= ? LIMIT 1`, source, bodySHA256, since.UnixMilli()).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) ClaimPending(ctx context.Context, lease time.Duration, now time.Time, owner string) (*PendingEvent, error) {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	nowMS := now.UnixMilli()
	row := tx.QueryRowContext(ctx, `SELECT id, source, COALESCE(remote_ip, ''), payload, COALESCE(vars_json, ''),
		COALESCE(external_delivery_id, ''), received_at_ms, COALESCE(replayed_at_ms, received_at_ms),
		COALESCE(config_hash, ''), COALESCE(actions_json, ''),
		COALESCE(replay_generation, 0)
		FROM events
		WHERE status = 'pending'
		   OR (status = 'retrying' AND COALESCE(next_attempt_at_ms, 0) <= ?)
		   OR (status = 'processing' AND COALESCE(lease_until_ms, 0) < ?)
		ORDER BY COALESCE(next_attempt_at_ms, received_at_ms) ASC LIMIT 1`, nowMS, nowMS)
	var ev PendingEvent
	var receivedMS, retryStartedMS int64
	if err := row.Scan(&ev.ID, &ev.Source, &ev.RemoteIP, &ev.Payload, &ev.VarsJSON, &ev.ExternalDeliveryID,
		&receivedMS, &retryStartedMS, &ev.ConfigHash, &ev.ActionsJSON, &ev.ReplayGeneration); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	ev.ReceivedAt = time.UnixMilli(receivedMS)
	ev.RetryStartedAt = time.UnixMilli(retryStartedMS)
	leaseUntil := now.Add(lease)
	res, err := tx.ExecContext(ctx, `UPDATE events SET status = 'processing', lease_until = ?, lease_until_ms = ?, lease_owner = ?
		WHERE id = ? AND (status = 'pending' OR (status = 'retrying' AND COALESCE(next_attempt_at_ms, 0) <= ?)
		OR (status = 'processing' AND COALESCE(lease_until_ms, 0) < ?))`, leaseUntil, leaseUntil.UnixMilli(), owner, ev.ID, nowMS, nowMS)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	// ClaimPending can recover an expired lease directly, without a preceding
	// requeue pass. Restore unfinished actions in the same transaction as the
	// new lease so a leftover 'running' state cannot strand the event. This also
	// repairs legacy pending/retrying events with an orphaned running action.
	if _, err := tx.ExecContext(ctx, `UPDATE event_actions SET status = 'pending', next_attempt_at_ms = NULL,
		attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END
		WHERE event_id = ? AND status = 'running'`, ev.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ev, nil
}

func (s *Store) RequeueExpiredLeases(ctx context.Context, now time.Time) (int64, error) {
	return s.requeueProcessing(ctx, false, `COALESCE(lease_until_ms, 0) < ?`, now.UnixMilli())
}

// RequeueAllProcessing is a single-instance startup recovery step. In addition
// to old leases, recover orphaned running actions left under pending/retrying
// events by earlier versions, while preserving terminal actions and events.
func (s *Store) RequeueAllProcessing(ctx context.Context) (int64, error) {
	return s.requeueProcessing(ctx, true, `1 = 1`)
}

func (s *Store) requeueProcessing(ctx context.Context, recoverOrphans bool, condition string, args ...any) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var orphanCount int64
	if recoverOrphans {
		res, err := tx.ExecContext(ctx, `UPDATE events SET status = 'pending', next_attempt_at_ms = received_at_ms,
			lease_until = NULL, lease_until_ms = NULL, lease_owner = NULL, processed_at = NULL, processed_at_ms = NULL
			WHERE status IN ('pending', 'retrying') AND EXISTS
			(SELECT 1 FROM event_actions WHERE event_id = events.id AND status = 'running')`)
		if err != nil {
			return 0, err
		}
		orphanCount, err = res.RowsAffected()
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE event_actions SET status = 'pending', next_attempt_at_ms = NULL,
			attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END
			WHERE status = 'running' AND event_id IN (SELECT id FROM events WHERE status = 'pending')`); err != nil {
			return 0, err
		}
	}
	queryArgs := append([]any{}, args...)
	if _, err := tx.ExecContext(ctx, `UPDATE event_actions SET status = 'pending', next_attempt_at_ms = NULL,
		attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END
		WHERE status = 'running' AND event_id IN (SELECT id FROM events WHERE status = 'processing' AND `+condition+`)`, queryArgs...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE events SET status = 'pending', next_attempt_at_ms = received_at_ms,
		lease_until = NULL, lease_until_ms = NULL, lease_owner = NULL, processed_at = NULL, processed_at_ms = NULL
		WHERE status = 'processing' AND `+condition, args...)
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
	return n + orphanCount, nil
}

func (s *Store) RequeueEvent(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE event_actions SET status = 'pending', next_attempt_at_ms = NULL,
		attempt_count = CASE WHEN attempt_count > 0 THEN attempt_count - 1 ELSE 0 END
		WHERE event_id = ? AND status = 'running'`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE events SET status = 'pending', next_attempt_at_ms = received_at_ms,
		lease_until = NULL, lease_until_ms = NULL, lease_owner = NULL, processed_at = NULL, processed_at_ms = NULL
		WHERE id = ? AND status = 'processing'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("event %q not requeueable", id)
	}
	return tx.Commit()
}

func (s *Store) RetryFailedEvent(ctx context.Context, id string) error {
	_, err := s.ReplayDeadEvent(ctx, id, "manual replay", time.Now())
	return err
}

func (s *Store) ReplayDeadEvent(ctx context.Context, id, reason string, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var status, varsJSON string
	var hasPayload int
	var generation int
	err = tx.QueryRowContext(ctx, `SELECT status, CASE WHEN payload IS NOT NULL AND length(payload) > 0 THEN 1 ELSE 0 END,
		COALESCE(vars_json, ''), COALESCE(replay_generation, 0) FROM events WHERE id = ?`, id).Scan(&status, &hasPayload, &varsJSON, &generation)
	if err == sql.ErrNoRows {
		return 0, ErrEventNotRetryable
	}
	if err != nil {
		return 0, err
	}
	if status != "dead" && status != "partial" && status != "error" {
		return 0, ErrEventNotRetryable
	}
	if hasPayload == 0 && varsJSON == "" {
		return 0, ErrReplayUnavailable
	}
	nextGeneration := generation + 1
	if _, err := tx.ExecContext(ctx, `UPDATE event_actions SET status = 'pending', attempt_count = 0,
		next_attempt_at_ms = ?, last_error = NULL, retry_after_ms = NULL, completed_at_ms = NULL
		WHERE event_id = ? AND status = 'dead'`, now.UnixMilli(), id); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE events SET status = 'pending', next_attempt_at_ms = ?, processed_at = NULL,
		processed_at_ms = NULL, lease_until = NULL, lease_until_ms = NULL, lease_owner = NULL,
		replay_generation = ?, replayed_at_ms = ?, replay_reason = ? WHERE id = ?`,
		now.UnixMilli(), nextGeneration, now.UnixMilli(), reason, id)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrEventNotRetryable
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return nextGeneration, nil
}

func (s *Store) PrepareEvent(ctx context.Context, id, varsJSON string, actionCount int, clearPayload bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE events SET vars_json = ? WHERE id = ? AND status = 'processing'`, varsJSON, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("event %q not processing", id)
	}
	for i := 0; i < actionCount; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO event_actions(event_id, action_index, status, attempt_count, next_attempt_at_ms)
			VALUES (?, ?, 'pending', 0, ?)`, id, i, now.UnixMilli()); err != nil {
			return err
		}
	}
	if clearPayload {
		if _, err := tx.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListActionStates(ctx context.Context, eventID string) ([]ActionState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT action_index, status, attempt_count, COALESCE(next_attempt_at_ms, 0), COALESCE(last_error, '')
		FROM event_actions WHERE event_id = ? ORDER BY action_index`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionState
	for rows.Next() {
		var state ActionState
		if err := rows.Scan(&state.Index, &state.Status, &state.AttemptCount, &state.NextAttemptMS, &state.LastError); err != nil {
			return nil, err
		}
		out = append(out, state)
	}
	return out, rows.Err()
}

func (s *Store) StartAction(ctx context.Context, eventID string, index int, now time.Time) (int, error) {
	var attempt int
	err := s.db.QueryRowContext(ctx, `UPDATE event_actions SET status = 'running', attempt_count = attempt_count + 1,
		next_attempt_at_ms = NULL WHERE event_id = ? AND action_index = ?
		AND status IN ('pending', 'retrying') AND COALESCE(next_attempt_at_ms, 0) <= ? RETURNING attempt_count`,
		eventID, index, now.UnixMilli()).Scan(&attempt)
	return attempt, err
}

func (s *Store) CompleteAction(ctx context.Context, eventID string, index int, now time.Time) error {
	return s.updateRunningAction(ctx, eventID, index, `status = 'done', next_attempt_at_ms = NULL,
		last_error = NULL, retry_after_ms = NULL, completed_at_ms = ?`, now.UnixMilli())
}

func (s *Store) ScheduleActionRetry(ctx context.Context, eventID string, index int, next time.Time, detail string, retryAfter time.Duration) error {
	return s.updateRunningAction(ctx, eventID, index, `status = 'retrying', next_attempt_at_ms = ?,
		last_error = ?, retry_after_ms = ?, completed_at_ms = NULL`, next.UnixMilli(), detail, retryAfter.Milliseconds())
}

func (s *Store) DeadAction(ctx context.Context, eventID string, index int, detail string, now time.Time) error {
	return s.updateRunningAction(ctx, eventID, index, `status = 'dead', next_attempt_at_ms = NULL,
		last_error = ?, retry_after_ms = NULL, completed_at_ms = ?`, detail, now.UnixMilli())
}

// ExpireAction records a retry-age deadline before dispatch, without counting
// an attempt or changing actions which have already completed. A single UPDATE
// atomically transitions pending/retrying state; RefreshEventStatus aggregates
// the event after all eligible actions have been considered.
func (s *Store) ExpireAction(ctx context.Context, eventID string, index int, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE event_actions SET status = 'dead', next_attempt_at_ms = NULL,
		last_error = 'maximum retry age exceeded', retry_after_ms = NULL, completed_at_ms = ?
		WHERE event_id = ? AND action_index = ? AND status IN ('pending', 'retrying')`, now.UnixMilli(), eventID, index)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action %s/%d not pending or retrying", eventID, index)
	}
	return nil
}

func (s *Store) updateRunningAction(ctx context.Context, eventID string, index int, assignment string, args ...any) error {
	args = append(args, eventID, index)
	res, err := s.db.ExecContext(ctx, `UPDATE event_actions SET `+assignment+` WHERE event_id = ? AND action_index = ? AND status = 'running'`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("action %s/%d not running", eventID, index)
	}
	return nil
}

func (s *Store) RefreshEventStatus(ctx context.Context, eventID string, now time.Time) (string, error) {
	var total, done, dead, retrying, pending, running int
	var next sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(status = 'done'), 0), COALESCE(SUM(status = 'dead'), 0),
		COALESCE(SUM(status = 'retrying'), 0), COALESCE(SUM(status = 'pending'), 0),
		COALESCE(SUM(status = 'running'), 0), MIN(CASE WHEN status = 'retrying' THEN next_attempt_at_ms END)
		FROM event_actions WHERE event_id = ?`, eventID).Scan(&total, &done, &dead, &retrying, &pending, &running, &next)
	if err != nil {
		return "", err
	}
	if total == 0 {
		return "", fmt.Errorf("event %q has no action state", eventID)
	}
	status := "pending"
	var processed any
	var due any = now.UnixMilli()
	if done == total {
		status, processed, due = "done", now.UnixMilli(), nil
	} else if dead > 0 && done+dead == total {
		status, processed, due = "dead", now.UnixMilli(), nil
	} else if retrying > 0 {
		status = "retrying"
		if next.Valid {
			due = next.Int64
		}
	} else if pending > 0 || running > 0 {
		status = "pending"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE events SET status = ?, processed_at = CASE WHEN ? IS NULL THEN NULL ELSE ? END,
		processed_at_ms = ?, next_attempt_at_ms = ?, lease_until = NULL, lease_until_ms = NULL, lease_owner = NULL WHERE id = ?`,
		status, processed, now, processed, due, eventID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("event %q not found", eventID)
	}
	return status, nil
}

func (s *Store) HasEvent(ctx context.Context, id string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, id).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) UpdateEventStatus(ctx context.Context, id, status string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET status = ?, processed_at = ?, processed_at_ms = ?,
		next_attempt_at_ms = NULL, lease_until = NULL, lease_until_ms = NULL, lease_owner = NULL WHERE id = ?`, status, at, at.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("event %q not found", id)
	}
	return nil
}

func (s *Store) SaveActionLog(ctx context.Context, eventID, action, target string, attempt int, success bool, detail string, dur time.Duration, at time.Time) error {
	return s.SaveActionAttemptLog(ctx, eventID, -1, 0, action, target, attempt, success, detail, dur, at)
}

func (s *Store) SaveActionAttemptLog(ctx context.Context, eventID string, actionIndex, generation int, action, target string, attempt int, success bool, detail string, dur time.Duration, at time.Time) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM events WHERE id = ?`, eventID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("event %q not found", eventID)
		}
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO action_logs
		(event_id, action, action_index, replay_generation, target, attempt, success, detail, duration_ms, created_at, created_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, eventID, action, actionIndex, generation, target, attempt,
		boolToInt(success), detail, dur.Milliseconds(), at, at.UnixMilli())
	return err
}

func (s *Store) GetPayload(ctx context.Context, id string) (payload []byte, source string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT payload, source FROM events WHERE id = ?`, id).Scan(&payload, &source)
	return
}

func (s *Store) GetEvent(ctx context.Context, id string) (*EventMeta, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, source, COALESCE(remote_ip, ''), status, received_at_ms,
		processed_at_ms, COALESCE(external_delivery_id, ''), COALESCE(config_hash, ''),
		COALESCE(replay_generation, 0), COALESCE(replay_reason, '') FROM events WHERE id = ?`, id)
	var ev EventMeta
	var receivedMS int64
	var processedMS sql.NullInt64
	if err := row.Scan(&ev.ID, &ev.Source, &ev.RemoteIP, &ev.Status, &receivedMS, &processedMS,
		&ev.ExternalDeliveryID, &ev.ConfigHash, &ev.ReplayGeneration, &ev.ReplayReason); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	ev.ReceivedAt = time.UnixMilli(receivedMS)
	if processedMS.Valid {
		t := time.UnixMilli(processedMS.Int64)
		ev.ProcessedAt = &t
	}
	return &ev, nil
}

func (s *Store) ListActionLogs(ctx context.Context, eventID string, limit int) ([]ActionLogRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT action, COALESCE(action_index, -1), COALESCE(replay_generation, 0),
		COALESCE(target, ''), attempt, success, COALESCE(detail, ''), COALESCE(duration_ms, 0), COALESCE(created_at_ms, 0)
		FROM action_logs WHERE event_id = ? ORDER BY id DESC LIMIT ?`, eventID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionLogRow
	for rows.Next() {
		var r ActionLogRow
		var success int
		var createdMS int64
		if err := rows.Scan(&r.Action, &r.ActionIndex, &r.ReplayGeneration, &r.Target, &r.Attempt, &success,
			&r.Detail, &r.DurationMS, &createdMS); err != nil {
			return nil, err
		}
		r.Success = success != 0
		r.CreatedAt = time.UnixMilli(createdMS)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Stats(ctx context.Context, now time.Time) (*QueueStats, error) {
	st := &QueueStats{}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM events GROUP BY status`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			_ = rows.Close()
			return nil, err
		}
		switch status {
		case "pending":
			st.Pending = n
		case "processing":
			st.Processing = n
		case "retrying":
			st.Retrying = n
		case "dead":
			st.Dead = n
		case "partial":
			st.Partial = n
		case "error":
			st.Error = n
		case "done":
			st.Done = n
		case "skipped":
			st.Skipped = n
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(status = 'retrying'), 0), COALESCE(SUM(status = 'dead'), 0) FROM event_actions`).Scan(&st.ActionsRetrying, &st.ActionsDead); err != nil {
		return nil, err
	}
	fiveMinutesAgo := now.Add(-5 * time.Minute).UnixMilli()
	oneHourAgo := now.Add(-time.Hour).UnixMilli()
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN created_at_ms >= ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at_ms >= ? AND success = 0 THEN 1 ELSE 0 END), 0),
		COUNT(*), COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0)
		FROM action_logs WHERE created_at_ms >= ?`,
		fiveMinutesAgo, fiveMinutesAgo, oneHourAgo).
		Scan(&st.ActionAttempts5M, &st.ActionFailures5M, &st.ActionAttempts1H, &st.ActionFailures1H); err != nil {
		return nil, err
	}
	if st.ActionAttempts5M > 0 {
		st.ActionFailureRate5M = float64(st.ActionFailures5M) / float64(st.ActionAttempts5M)
	}
	if st.ActionAttempts1H > 0 {
		st.ActionFailureRate1H = float64(st.ActionFailures1H) / float64(st.ActionAttempts1H)
	}
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(received_at_ms) FROM events WHERE status IN ('pending','processing','retrying')`).Scan(&oldest); err != nil {
		return nil, err
	}
	if oldest.Valid && now.UnixMilli() > oldest.Int64 {
		st.OldestPendingAgeMS = now.UnixMilli() - oldest.Int64
	}
	return st, nil
}

func parseSQLiteTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, " m="); i >= 0 {
		raw = raw[:i]
	}
	if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if epoch > -100_000_000_000 && epoch < 100_000_000_000 {
			return time.Unix(epoch, 0), nil
		}
		return time.UnixMilli(epoch), nil
	}
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999+00:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported sqlite time %q", raw)
}

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) ReadyCheck(ctx context.Context) error {
	return s.withDeadlineWriter(ctx, func(conn *sql.Conn) error {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		// Toggle a stored value so repeated probes still commit a real write.
		if _, err := tx.ExecContext(ctx, `INSERT INTO _ready_check(id, nonce) VALUES (1, 1)
			ON CONFLICT(id) DO UPDATE SET nonce = 1 - nonce`); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (s *Store) PurgeOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	terminal := `status IN ('done','dead','skipped','partial','error')`
	age := `COALESCE(processed_at_ms, received_at_ms) < ?`
	if _, err := tx.ExecContext(ctx, `DELETE FROM action_logs WHERE event_id IN (SELECT id FROM events WHERE `+age+` AND `+terminal+`)`, cutoff.UnixMilli()); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_actions WHERE event_id IN (SELECT id FROM events WHERE `+age+` AND `+terminal+`)`, cutoff.UnixMilli()); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE `+age+` AND `+terminal, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM replay_guard WHERE expires_at_ms < ?`, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM heartbeat_logs WHERE created_at_ms < ?`, cutoff.UnixMilli()); err != nil {
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

func (s *Store) RenewLease(ctx context.Context, id string, lease time.Duration, now time.Time, owner string) error {
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	until := now.Add(lease)
	res, err := s.db.ExecContext(ctx, `UPDATE events SET lease_until = ?, lease_until_ms = ? WHERE id = ? AND status = 'processing'
		AND (lease_owner = ? OR lease_owner IS NULL OR lease_owner = '')`, until, until.UnixMilli(), id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("event %q not processing", id)
	}
	return nil
}

func (s *Store) ClearPayload(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE id = ?`, id)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
