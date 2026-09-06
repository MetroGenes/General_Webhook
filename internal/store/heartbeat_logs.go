package store

import (
	"context"
	"database/sql"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	heartbeatTargetMaxBytes = 128
	heartbeatOriginMaxBytes = 512
	heartbeatErrorMaxBytes  = 2048
	heartbeatLogDefault     = 50
	heartbeatLogMax         = 100
)

var heartbeatLogURL = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

// HeartbeatLog records a failed outbound push. Target is a logical name;
// Origin contains only scheme and authority, never a push token or URL path.
// Callers supply a classified, redacted Error, with defense-in-depth filtering
// here before anything reaches SQLite.
type HeartbeatLog struct {
	ID         int64     `json:"id"`
	Target     string    `json:"target"`
	Origin     string    `json:"origin"`
	Mode       string    `json:"mode"`
	Health     string    `json:"health"`
	Error      string    `json:"error"`
	HTTPStatus int       `json:"http_status"`
	DurationMS int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
}

// SaveHeartbeatError writes an independent audit record. Callers should give
// ctx a deadline and report a persistence failure only to the ordinary logger,
// since an unavailable database cannot persist its own write error.
func (s *Store) SaveHeartbeatError(ctx context.Context, entry HeartbeatLog) error {
	entry.Target = heartbeatAuditText(entry.Target, heartbeatTargetMaxBytes)
	entry.Origin = heartbeatAuditOrigin(entry.Origin)
	entry.Mode = heartbeatAuditText(entry.Mode, 32)
	entry.Health = heartbeatAuditText(entry.Health, 32)
	entry.Error = heartbeatAuditText(entry.Error, heartbeatErrorMaxBytes)
	if entry.HTTPStatus < 100 || entry.HTTPStatus > 599 {
		entry.HTTPStatus = 0
	}
	if entry.DurationMS < 0 {
		entry.DurationMS = 0
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	entry.CreatedAt = entry.CreatedAt.UTC()
	return s.withDeadlineWriter(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO heartbeat_logs
			(target, origin, mode, health, error, http_status, duration_ms, created_at, created_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, entry.Target, entry.Origin, entry.Mode, entry.Health,
			entry.Error, entry.HTTPStatus, entry.DurationMS, entry.CreatedAt, entry.CreatedAt.UnixMilli())
		return err
	})
}

// ListHeartbeatErrors returns the newest records first, breaking timestamp
// ties with the insertion ID. The hard limit also bounds management responses.
func (s *Store) ListHeartbeatErrors(ctx context.Context, limit int) ([]HeartbeatLog, error) {
	if limit <= 0 {
		limit = heartbeatLogDefault
	}
	if limit > heartbeatLogMax {
		limit = heartbeatLogMax
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, target, origin, mode, health, error, http_status, duration_ms, created_at_ms
		FROM heartbeat_logs ORDER BY created_at_ms DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]HeartbeatLog, 0, limit)
	for rows.Next() {
		var entry HeartbeatLog
		var createdMS int64
		if err := rows.Scan(&entry.ID, &entry.Target, &entry.Origin, &entry.Mode, &entry.Health,
			&entry.Error, &entry.HTTPStatus, &entry.DurationMS, &createdMS); err != nil {
			return nil, err
		}
		entry.CreatedAt = time.UnixMilli(createdMS).UTC()
		logs = append(logs, entry)
	}
	return logs, rows.Err()
}

func heartbeatAuditOrigin(raw string) string {
	// Reject excessive/malformed input wholesale instead of truncating a URL
	// inside its userinfo or returning parser errors containing credentials.
	if len(raw) > 8192 {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	origin := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	if len(origin) > heartbeatOriginMaxBytes {
		return ""
	}
	return origin
}

func heartbeatAuditText(raw string, limit int) string {
	// Limit work as well as stored bytes. Even a truncated URL is removed as a
	// whole token, including malformed paths, instead of parsing and retaining
	// an accidentally embedded credential. Origin is stored separately.
	raw = truncateHeartbeatText(raw, limit*4)
	raw = heartbeatLogURL.ReplaceAllString(raw, "[url]")
	raw = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, raw)
	return truncateHeartbeatText(strings.TrimSpace(raw), limit)
}

func truncateHeartbeatText(raw string, limit int) string {
	if len(raw) > limit {
		raw = raw[:limit]
	}
	// Remove invalid input and any partial final code point without increasing
	// the byte length beyond the persistence/response budget.
	return strings.ToValidUTF8(raw, "")
}
