package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"
)

func prepareStoreFile(path string) error {
	if path == ":memory:" {
		return nil
	}
	if err := tightenStorePermissions(path); err != nil {
		return err
	}
	// SQLite derives WAL/SHM permissions from the main file. Create it privately
	// before the first connection applies journal_mode, not after migration.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create store file: %w", err)
	}
	return file.Close()
}

func tightenStorePermissions(path string) error {
	if path == ":memory:" {
		return nil
	}
	for _, filename := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(filename, 0o600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("chmod store file: %w", err)
		}
	}
	return nil
}

// SQLite's native busy handler can sleep past context cancellation. Bound that
// sleep by the caller's remaining budget for readiness and heartbeat audit
// writes, then restore the normal policy before returning the connection.
func (s *Store) withDeadlineWriter(ctx context.Context, write func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		busyMS := remaining.Milliseconds()
		if busyMS < sqliteBusyTimeoutMS {
			// PRAGMA takes an integer literal, not a SQL bind parameter. This
			// value is computed from a duration, never from external text.
			_, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = `+strconv.FormatInt(busyMS, 10))
			if err != nil {
				return err
			}
			defer func() {
				_, _ = conn.ExecContext(context.Background(), `PRAGMA busy_timeout = `+strconv.Itoa(sqliteBusyTimeoutMS))
			}()
		}
	}
	return write(conn)
}
