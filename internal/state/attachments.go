package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Attachment statuses.
const (
	AttachmentPending = "pending"
	AttachmentDone    = "done"
	AttachmentFailed  = "failed"
)

// Attachment is one out-of-band download (Google Chat uploads in v1). The
// owning message commits independently; a flaky blob never blocks a cursor.
type Attachment struct {
	Source      string // instance id, e.g. "gchat:work"
	StableID    string // owning message (chat: message resource name)
	PartKey     string // attachmentDataRef resource name or part index
	RelPath     string // deterministic target path relative to archive root
	Bytes       int64  // size on disk once done; 0 before
	Status      string // AttachmentPending | AttachmentDone | AttachmentFailed
	Attempts    int
	NextRetryAt time.Time // zero when unscheduled (new, done, or failed)
	LastError   string
}

// UpsertPendingAttachment records a pending download. If the row already
// exists — in any status — it is left untouched, so replaying an ingestion
// page never resets a completed or failed download.
func (d *DB) UpsertPendingAttachment(source, stableID, partKey, relPath string) error {
	if source == "" || stableID == "" || partKey == "" || relPath == "" {
		return fmt.Errorf("state: upsert attachment %s/%s/%s: all fields are required", source, stableID, partKey)
	}
	_, err := d.sql.Exec(`
		INSERT INTO attachments (source, stable_id, part_key, rel_path, status)
		VALUES (?,?,?,?,'pending')
		ON CONFLICT(source, stable_id, part_key) DO NOTHING`,
		source, stableID, partKey, relPath)
	if err != nil {
		return fmt.Errorf("state: upsert attachment %s/%s/%s: %w", source, stableID, partKey, err)
	}
	return nil
}

// DueAttachments returns up to limit of ONE instance's pending attachments
// whose retry time has arrived (never-tried rows, with no scheduled retry,
// come first). The filter on source is what keeps one account's backlog from
// filling the window and starving another account's downloads.
func (d *DB) DueAttachments(source string, now time.Time, limit int) ([]Attachment, error) {
	rows, err := d.sql.Query(`
		SELECT source, stable_id, part_key, rel_path, COALESCE(bytes, 0),
		       status, attempts, next_retry_at, COALESCE(last_error, '')
		FROM attachments
		WHERE source = ? AND status = 'pending'
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY COALESCE(next_retry_at, ''), stable_id, part_key
		LIMIT ?`, source, fmtTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("state: due attachments for %s: %w", source, err)
	}
	defer rows.Close()
	return scanAttachments(rows)
}

// MarkAttachmentDone records a completed download and its size on disk.
func (d *DB) MarkAttachmentDone(source, stableID, partKey string, bytes int64) error {
	return d.finishAttachment(source, stableID, partKey, `
		UPDATE attachments
		SET status = 'done', bytes = ?, next_retry_at = NULL, last_error = NULL
		WHERE source = ? AND stable_id = ? AND part_key = ?`,
		bytes, source, stableID, partKey)
}

// MarkAttachmentDoneAndDirtyDay marks a chat attachment done and, in the
// same transaction, marks the owning message's (source, space, day) dirty in
// the day-file ledger. Making both one commit means a crash can never leave
// a 'done' attachment whose day file still shows "[unavailable]" — the
// states are either both recorded or neither. source is the owning chat
// instance id and stableID the owning chat message's resource name; if no
// such chat_messages row exists, only the done mark is committed.
func (d *DB) MarkAttachmentDoneAndDirtyDay(source, stableID, partKey string, bytes int64) error {
	fail := func(err error) error {
		return fmt.Errorf("state: attachment done+dirty %s/%s/%s: %w", source, stableID, partKey, err)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE attachments
		SET status = 'done', bytes = ?, next_retry_at = NULL, last_error = NULL
		WHERE source = ? AND stable_id = ? AND part_key = ?`,
		bytes, source, stableID, partKey)
	if err != nil {
		return fail(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fail(err)
	} else if n == 0 {
		return fail(errors.New("no such attachment row"))
	}

	var space, day string
	err = tx.QueryRow(`
		SELECT space_name, day_bucket FROM chat_messages
		WHERE source = ? AND msg_name = ?`,
		source, stableID).Scan(&space, &day)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit() // no owning chat message: nothing to dirty
	}
	if err != nil {
		return fail(err)
	}

	res, err = tx.Exec(`
		UPDATE chat_day_files SET dirty = 1, dirty_seq = dirty_seq + 1
		WHERE source = ? AND space_name = ? AND day_bucket = ?`, source, space, day)
	if err != nil {
		return fail(err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fail(err)
	} else if n == 0 {
		// Ledger row missing (day never registered): create it dirty from
		// the space's frozen identity, mirroring MarkDayDirty.
		var spaceType, slug string
		if err := tx.QueryRow(`
			SELECT space_type, display_slug FROM chat_spaces
			WHERE source = ? AND space_name = ?`,
			source, space).Scan(&spaceType, &slug); err != nil {
			return fail(err)
		}
		relPath, err := chatDayRelPath(source, day, spaceType, slug, space)
		if err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(upsertDirtyDaySQL, source, space, day, relPath); err != nil {
			return fail(err)
		}
	}
	return tx.Commit()
}

// MarkAttachmentRetry records a transient failure: the row stays pending and
// becomes due again at nextRetryAt.
func (d *DB) MarkAttachmentRetry(source, stableID, partKey, lastErr string, attempts int, nextRetryAt time.Time) error {
	return d.finishAttachment(source, stableID, partKey, `
		UPDATE attachments
		SET status = 'pending', attempts = ?, last_error = ?, next_retry_at = ?
		WHERE source = ? AND stable_id = ? AND part_key = ?`,
		attempts, nullStr(lastErr), fmtTime(nextRetryAt), source, stableID, partKey)
}

// MarkAttachmentFailed records a permanent failure ('failed' status, never
// retried automatically; surfaced by `save status`).
func (d *DB) MarkAttachmentFailed(source, stableID, partKey, lastErr string, attempts int) error {
	return d.finishAttachment(source, stableID, partKey, `
		UPDATE attachments
		SET status = 'failed', attempts = ?, last_error = ?, next_retry_at = NULL
		WHERE source = ? AND stable_id = ? AND part_key = ?`,
		attempts, nullStr(lastErr), source, stableID, partKey)
}

func (d *DB) finishAttachment(source, stableID, partKey, query string, args ...any) error {
	res, err := d.sql.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("state: update attachment %s/%s/%s: %w", source, stableID, partKey, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: update attachment %s/%s/%s: %w", source, stableID, partKey, err)
	}
	if n == 0 {
		return fmt.Errorf("state: update attachment %s/%s/%s: no such row", source, stableID, partKey)
	}
	return nil
}

// AttachmentsForMessage returns all attachment rows owned by one message,
// ordered by part key — the renderer consults these to decide between a
// link and an [unavailable] marker.
func (d *DB) AttachmentsForMessage(source, stableID string) ([]Attachment, error) {
	rows, err := d.sql.Query(`
		SELECT source, stable_id, part_key, rel_path, COALESCE(bytes, 0),
		       status, attempts, next_retry_at, COALESCE(last_error, '')
		FROM attachments
		WHERE source = ? AND stable_id = ?
		ORDER BY part_key`, source, stableID)
	if err != nil {
		return nil, fmt.Errorf("state: attachments for %s/%s: %w", source, stableID, err)
	}
	defer rows.Close()
	return scanAttachments(rows)
}

// AttachmentCounts summarizes one instance's attachment rows by status.
type AttachmentCounts struct {
	Pending int64
	Done    int64
	Failed  int64
}

// AttachmentCounts returns per-instance status totals for `save status`,
// keyed by instance id.
func (d *DB) AttachmentCounts() (map[string]AttachmentCounts, error) {
	rows, err := d.sql.Query(`
		SELECT source, status, COUNT(*) FROM attachments GROUP BY source, status`)
	if err != nil {
		return nil, fmt.Errorf("state: attachment counts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]AttachmentCounts)
	for rows.Next() {
		var source, status string
		var n int64
		if err := rows.Scan(&source, &status, &n); err != nil {
			return nil, fmt.Errorf("state: attachment counts: %w", err)
		}
		c := out[source]
		switch status {
		case AttachmentPending:
			c.Pending = n
		case AttachmentDone:
			c.Done = n
		case AttachmentFailed:
			c.Failed = n
		}
		out[source] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: attachment counts: %w", err)
	}
	return out, nil
}

func scanAttachments(rows *sql.Rows) ([]Attachment, error) {
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var retry sql.NullString
		if err := rows.Scan(&a.Source, &a.StableID, &a.PartKey, &a.RelPath,
			&a.Bytes, &a.Status, &a.Attempts, &retry, &a.LastError); err != nil {
			return nil, fmt.Errorf("state: scan attachment: %w", err)
		}
		t, err := parseNullTime(retry)
		if err != nil {
			return nil, err
		}
		a.NextRetryAt = t
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: scan attachments: %w", err)
	}
	return out, nil
}
