package state

import (
	"database/sql"
	"fmt"
	"time"
)

// RunStats are the counters a finished sync run reports.
type RunStats struct {
	New        int64 // newly archived messages
	Updated    int64 // updated/edited/tombstoned messages
	AttsDone   int64
	AttsFailed int64
}

// SyncRun is one recorded run for `comms status`.
type SyncRun struct {
	ID         int64
	Source     string // instance id, e.g. "gmail:work"
	Kind       string // 'backfill'|'incremental'|'events'|'render'|'att_retry'
	StartedAt  time.Time
	Finished   bool
	FinishedAt time.Time // zero while running (or after a hard kill)
	OK         bool      // meaningful only when Finished
	Stats      RunStats
	Error      string
}

// StartRun records the start of a run and returns its id. A run that never
// finishes (hard kill) stays visible as unfinished.
func (d *DB) StartRun(source, kind string) (int64, error) {
	res, err := d.sql.Exec(`
		INSERT INTO sync_runs (source, kind, started_at) VALUES (?,?,?)`,
		source, kind, fmtTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("state: start run %s/%s: %w", source, kind, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("state: start run %s/%s: %w", source, kind, err)
	}
	return id, nil
}

// FinishRun records a run's outcome.
func (d *DB) FinishRun(id int64, ok bool, stats RunStats, errMsg string) error {
	res, err := d.sql.Exec(`
		UPDATE sync_runs
		SET finished_at = ?, ok = ?, new_msgs = ?, updated_msgs = ?,
		    atts_done = ?, atts_failed = ?, error = ?
		WHERE id = ?`,
		fmtTime(time.Now()), boolInt(ok), stats.New, stats.Updated,
		stats.AttsDone, stats.AttsFailed, nullStr(errMsg), id)
	if err != nil {
		return fmt.Errorf("state: finish run %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("state: finish run %d: %w", id, err)
	} else if n == 0 {
		return fmt.Errorf("state: finish run %d: no such run", id)
	}
	return nil
}

// RecentRuns returns the source's newest n runs, newest first.
func (d *DB) RecentRuns(source string, n int) ([]SyncRun, error) {
	rows, err := d.sql.Query(`
		SELECT id, source, kind, started_at, finished_at, ok,
		       new_msgs, updated_msgs, atts_done, atts_failed, error
		FROM sync_runs WHERE source = ?
		ORDER BY id DESC LIMIT ?`, source, n)
	if err != nil {
		return nil, fmt.Errorf("state: recent runs %s: %w", source, err)
	}
	defer rows.Close()
	var out []SyncRun
	for rows.Next() {
		var r SyncRun
		var started string
		var finished, errMsg sql.NullString
		var ok sql.NullInt64
		if err := rows.Scan(&r.ID, &r.Source, &r.Kind, &started, &finished, &ok,
			&r.Stats.New, &r.Stats.Updated, &r.Stats.AttsDone, &r.Stats.AttsFailed,
			&errMsg); err != nil {
			return nil, fmt.Errorf("state: recent runs %s: %w", source, err)
		}
		if r.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if r.FinishedAt, err = parseNullTime(finished); err != nil {
			return nil, err
		}
		r.Finished = ok.Valid
		r.OK = ok.Valid && ok.Int64 != 0
		r.Error = errMsg.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: recent runs %s: %w", source, err)
	}
	return out, nil
}

// PruneRuns deletes all but each instance's newest keepPerSource runs.
func (d *DB) PruneRuns(keepPerSource int) error {
	_, err := d.sql.Exec(`
		DELETE FROM sync_runs WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY source ORDER BY id DESC) AS rn
				FROM sync_runs
			) WHERE rn > ?
		)`, keepPerSource)
	if err != nil {
		return fmt.Errorf("state: prune runs: %w", err)
	}
	return nil
}
