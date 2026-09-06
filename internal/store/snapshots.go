package store

import (
	"context"
	"fmt"
)

const (
	snapshotRewriteBatchRows  = 128
	snapshotRewriteBatchBytes = 1 << 20
)

type actionSnapshotRow struct{ id, raw string }

// RewriteActionSnapshots replaces persisted credentials with runtime secret
// references at startup. Keyset pagination bounds live history to 128 rows and
// 1 MiB per batch. A larger individual snapshot is processed alone: the string
// callback necessarily needs that whole record. Including one look-ahead row,
// memory is O(batch byte budget + largest snapshot), not O(history size).
//
// The cursor is closed before callbacks or writes so callbacks may safely use
// the store's single database connection. Partial progress is committed and the
// operation can be retried; unchanged rows never generate writes.
func (s *Store) RewriteActionSnapshots(ctx context.Context, rewrite func(string) (string, error)) (int64, error) {
	if rewrite == nil {
		return 0, nil
	}
	var changed int64
	var after string
	haveCursor := false
	for {
		batch, err := s.actionSnapshotBatch(ctx, after, haveCursor)
		if err != nil {
			return changed, err
		}
		if len(batch) == 0 {
			return changed, nil
		}
		for _, row := range batch {
			if err := ctx.Err(); err != nil {
				return changed, err
			}
			updated, err := rewrite(row.raw)
			if err != nil {
				return changed, fmt.Errorf("rewrite snapshot %s: %w", row.id, err)
			}
			if updated != row.raw {
				// Do not overwrite a newer snapshot if a caller uses this outside
				// the normal startup-only migration window.
				res, err := s.db.ExecContext(ctx, `UPDATE events SET actions_json = ? WHERE id = ? AND actions_json = ?`, updated, row.id, row.raw)
				if err != nil {
					return changed, err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return changed, err
				}
				changed += n
			}
		}
		after, haveCursor = batch[len(batch)-1].id, true
	}
}

func (s *Store) actionSnapshotBatch(ctx context.Context, after string, haveCursor bool) ([]actionSnapshotRow, error) {
	query := `SELECT id, actions_json FROM events WHERE actions_json IS NOT NULL AND actions_json != ''`
	var args []any
	if haveCursor {
		query += ` AND id > ?`
		args = append(args, after)
	}
	query += ` ORDER BY id LIMIT ?`
	args = append(args, snapshotRewriteBatchRows)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	batch := make([]actionSnapshotRow, 0, snapshotRewriteBatchRows)
	bytes := 0
	for rows.Next() {
		var row actionSnapshotRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			return nil, err
		}
		size := len(row.id) + len(row.raw)
		if len(batch) > 0 && size > snapshotRewriteBatchBytes-bytes {
			// This row remains beyond the saved primary-key cursor and will be
			// the first row in the next batch, even if this batch is rewritten.
			break
		}
		batch = append(batch, row)
		bytes += size
		if bytes >= snapshotRewriteBatchBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return batch, nil
}
