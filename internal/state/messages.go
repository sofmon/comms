package state

import (
	"database/sql"
	"fmt"
	"time"
)

// Message is the DB row committed after an email's .md and attachment files
// are durably on disk (write ordering: files first, row second).
type Message struct {
	Source      string    // instance id, e.g. "gmail:work"
	StableID    string    // gmail message id / JMAP Email id
	RFC822MsgID string    // Message-ID header; "" if absent
	ThreadID    string    // "" if absent
	TS          time.Time // server timestamp (internalDate / receivedAt)
	OrigOffset  string    // Date: header UTC offset, e.g. "+02:00"; "" if unknown
	DayBucket   string    // "YYYY-MM-DD" in the pinned archive timezone
	RelPath     string    // .md path relative to archive root
	ContentHash string    // sha256 of the .md bytes
}

// SeenMessage reports whether (source, stableID) is already archived
// (tombstoned rows count as seen — the file is still on disk).
func (d *DB) SeenMessage(source, stableID string) (bool, error) {
	var one int
	err := d.sql.QueryRow(`
		SELECT 1 FROM messages WHERE source = ? AND stable_id = ?`,
		source, stableID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("state: seen message %s/%s: %w", source, stableID, err)
	}
	return true, nil
}

// CommitMessage records an archived email. Re-committing an existing
// (source, stable_id) updates the mutable columns (a crash-replay re-render
// may produce a fresh content hash) but preserves archived_at and the
// deleted tombstone.
func (d *DB) CommitMessage(m Message) error {
	if m.Source == "" || m.StableID == "" || m.DayBucket == "" || m.RelPath == "" || m.TS.IsZero() {
		return fmt.Errorf("state: commit message %s/%s: source, stable id, ts, day bucket and rel path are required", m.Source, m.StableID)
	}
	_, err := d.sql.Exec(`
		INSERT INTO messages
			(source, stable_id, rfc822_msgid, thread_id, ts_utc, orig_offset,
			 day_bucket, rel_path, content_hash, archived_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source, stable_id) DO UPDATE SET
			rfc822_msgid = excluded.rfc822_msgid,
			thread_id    = excluded.thread_id,
			ts_utc       = excluded.ts_utc,
			orig_offset  = excluded.orig_offset,
			day_bucket   = excluded.day_bucket,
			rel_path     = excluded.rel_path,
			content_hash = excluded.content_hash`,
		m.Source, m.StableID, nullStr(m.RFC822MsgID), nullStr(m.ThreadID),
		fmtTS(m.TS), nullStr(m.OrigOffset), m.DayBucket, m.RelPath,
		m.ContentHash, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("state: commit message %s/%s: %w", m.Source, m.StableID, err)
	}
	return nil
}

// MarkMessageDeleted sets the DB-only tombstone for an archived message; the
// file on disk is never touched. Tombstoning an unknown id is a no-op (the
// server may report deletions for messages we never archived).
func (d *DB) MarkMessageDeleted(source, stableID string) error {
	_, err := d.sql.Exec(`
		UPDATE messages SET deleted = 1 WHERE source = ? AND stable_id = ?`,
		source, stableID)
	if err != nil {
		return fmt.Errorf("state: tombstone message %s/%s: %w", source, stableID, err)
	}
	return nil
}

// MessageCount summarizes one instance's archived messages.
type MessageCount struct {
	Total   int64
	Deleted int64 // tombstoned subset of Total
}

// MessageCounts returns per-instance archive totals for `save status`, keyed
// by instance id.
func (d *DB) MessageCounts() (map[string]MessageCount, error) {
	rows, err := d.sql.Query(`
		SELECT source, COUNT(*), COALESCE(SUM(deleted), 0)
		FROM messages GROUP BY source`)
	if err != nil {
		return nil, fmt.Errorf("state: message counts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]MessageCount)
	for rows.Next() {
		var source string
		var c MessageCount
		if err := rows.Scan(&source, &c.Total, &c.Deleted); err != nil {
			return nil, fmt.Errorf("state: message counts: %w", err)
		}
		out[source] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: message counts: %w", err)
	}
	return out, nil
}

// EnqueueBackfill adds one Gmail instance's message ids to its backfill
// queue in one transaction; already-queued ids are ignored, so replaying an
// enumeration page is harmless. The queue is per instance: message ids are
// not unique across accounts.
func (d *DB) EnqueueBackfill(source string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if source == "" {
		return fmt.Errorf("state: enqueue backfill: source is required")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("state: enqueue backfill: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO gmail_backfill (source, msg_id) VALUES (?,?)`)
	if err != nil {
		return fmt.Errorf("state: enqueue backfill: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if id == "" {
			return fmt.Errorf("state: enqueue backfill: empty message id")
		}
		if _, err := stmt.Exec(source, id); err != nil {
			return fmt.Errorf("state: enqueue backfill %s/%s: %w", source, id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: enqueue backfill: %w", err)
	}
	return nil
}

// NextPendingBackfill returns up to n of one instance's pending queue ids in
// enqueue order.
func (d *DB) NextPendingBackfill(source string, n int) ([]string, error) {
	rows, err := d.sql.Query(`
		SELECT msg_id FROM gmail_backfill WHERE source = ? AND state = 'pending'
		ORDER BY rowid LIMIT ?`, source, n)
	if err != nil {
		return nil, fmt.Errorf("state: next pending backfill %s: %w", source, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("state: next pending backfill %s: %w", source, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: next pending backfill %s: %w", source, err)
	}
	return ids, nil
}

// MarkBackfillDone marks one instance's queue id done; unknown ids are a
// no-op.
func (d *DB) MarkBackfillDone(source, id string) error {
	_, err := d.sql.Exec(`
		UPDATE gmail_backfill SET state = 'done' WHERE source = ? AND msg_id = ?`,
		source, id)
	if err != nil {
		return fmt.Errorf("state: mark backfill done %s/%s: %w", source, id, err)
	}
	return nil
}

// BackfillPendingCount returns the number of pending queue ids for one
// instance.
func (d *DB) BackfillPendingCount(source string) (int64, error) {
	var n int64
	err := d.sql.QueryRow(`
		SELECT COUNT(*) FROM gmail_backfill WHERE source = ? AND state = 'pending'`,
		source).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("state: backfill pending count %s: %w", source, err)
	}
	return n, nil
}

// ClearBackfill drops one instance's whole queue (called when its backfill
// completes); other accounts' queues are untouched.
func (d *DB) ClearBackfill(source string) error {
	if _, err := d.sql.Exec(`DELETE FROM gmail_backfill WHERE source = ?`, source); err != nil {
		return fmt.Errorf("state: clear backfill %s: %w", source, err)
	}
	return nil
}
