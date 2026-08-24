package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SkipThreshold is the number of recorded processing failures after which a
// (source, id) is skipped — a poison item can never wedge a cursor.
const SkipThreshold = 5

// Failure is one poison-item ledger row.
type Failure struct {
	Source    string // instance id, e.g. "gmail:work"
	ID        string
	Attempts  int
	LastError string
	LastAt    time.Time
}

// RecordFailure increments the failure count for (source, id) and returns
// the new attempt total.
func (d *DB) RecordFailure(source, id, lastErr string) (int, error) {
	var attempts int
	err := d.sql.QueryRow(`
		INSERT INTO failures (source, id, attempts, last_error, last_at)
		VALUES (?,?,1,?,?)
		ON CONFLICT(source, id) DO UPDATE SET
			attempts   = failures.attempts + 1,
			last_error = excluded.last_error,
			last_at    = excluded.last_at
		RETURNING attempts`,
		source, id, nullStr(lastErr), fmtTime(time.Now())).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("state: record failure %s/%s: %w", source, id, err)
	}
	return attempts, nil
}

// ClearFailure forgets a (source, id)'s failure history (called after a
// successful processing, or by a manual re-drive); unknown ids are a no-op.
func (d *DB) ClearFailure(source, id string) error {
	_, err := d.sql.Exec(`
		DELETE FROM failures WHERE source = ? AND id = ?`, source, id)
	if err != nil {
		return fmt.Errorf("state: clear failure %s/%s: %w", source, id, err)
	}
	return nil
}

// IsSkipped reports whether (source, id) has failed SkipThreshold or more
// times and should not be retried this run.
func (d *DB) IsSkipped(source, id string) (bool, error) {
	var attempts int
	err := d.sql.QueryRow(`
		SELECT attempts FROM failures WHERE source = ? AND id = ?`,
		source, id).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state: is skipped %s/%s: %w", source, id, err)
	}
	return attempts >= SkipThreshold, nil
}

// ListFailures returns the whole ledger ordered by source then id, for
// `save status`.
func (d *DB) ListFailures() ([]Failure, error) {
	rows, err := d.sql.Query(`
		SELECT source, id, attempts, COALESCE(last_error, ''), last_at
		FROM failures ORDER BY source, id`)
	if err != nil {
		return nil, fmt.Errorf("state: list failures: %w", err)
	}
	defer rows.Close()
	var out []Failure
	for rows.Next() {
		var f Failure
		var lastAt string
		if err := rows.Scan(&f.Source, &f.ID, &f.Attempts, &f.LastError, &lastAt); err != nil {
			return nil, fmt.Errorf("state: list failures: %w", err)
		}
		if f.LastAt, err = parseTime(lastAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list failures: %w", err)
	}
	return out, nil
}
