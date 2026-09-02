package state

import (
	"database/sql"
	"fmt"
	"time"
)

// Disposition says which tree an email note lives in. It is the state
// database's answer, and the only answer: a rel path never says which root
// it is relative to, so everything that turns messages.rel_path into a file
// must resolve it through the row's disposition (archive.Writer.NotePath).
//
// Not to be confused with archive.SkipDisposition, which is what became of a
// refused attachment's bytes. This one is about where a whole note is.
type Disposition string

const (
	// DispositionArchive: the note is under archive_root, where every note is
	// first written and where it stays unless triage decides otherwise.
	DispositionArchive Disposition = "archive"

	// DispositionSpam: noise triage moved the note (and its .d/ attachment
	// directory) to the same rel path under spam_root. Nothing is deleted; the
	// move is reversed by `save untriage`.
	DispositionSpam Disposition = "spam"
)

// Dispositions returns every valid disposition, in a stable order. The
// schema's CHECK constraint is generated from this list.
func Dispositions() []Disposition {
	return []Disposition{DispositionArchive, DispositionSpam}
}

// ValidDisposition reports whether d is one of the Disposition constants. The
// empty string is NOT a disposition — callers that mean "the default" say
// DispositionArchive.
func ValidDisposition(d Disposition) bool {
	return d == DispositionArchive || d == DispositionSpam
}

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
	RelPath     string    // .md path relative to the root Disposition selects
	ContentHash string    // sha256 of the .md bytes
	Deleted     bool      // tombstoned upstream; the file is still on disk

	// The triage columns. CommitMessage never writes them — a note is always
	// committed into the archive tree and only SetDisposition moves it — so a
	// crash-replay re-commit can never undo a triage decision.
	Disposition       Disposition // where the note lives; DispositionArchive on a fresh row
	DispositionReason string      // human-readable why; "" until triage looked at it
	DispositionRule   string      // machine-readable rule id ("protect:attachment", "rules:noise:github", "llm:<model>@v1", "manual")
	DispositionAt     time.Time   // when the disposition was last set; zero until triage looked at it
	TriageDigest      string      // the triage digest the disposition is settled under; "" means "not decided under any"
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
// may produce a fresh content hash) but preserves archived_at, the deleted
// tombstone, and every triage column.
//
// A commit always describes a note in the ARCHIVE tree: connectors write new
// notes there and nothing else, and a re-render of a spam-filed note leaves
// the row's disposition exactly as triage set it. Passing any other
// disposition is a programming error, refused rather than ignored — the only
// thing that moves a note is SetDisposition, after the files have moved.
func (d *DB) CommitMessage(m Message) error {
	if m.Source == "" || m.StableID == "" || m.DayBucket == "" || m.RelPath == "" || m.TS.IsZero() {
		return fmt.Errorf("state: commit message %s/%s: source, stable id, ts, day bucket and rel path are required", m.Source, m.StableID)
	}
	if m.Disposition != "" && m.Disposition != DispositionArchive {
		return fmt.Errorf("state: commit message %s/%s: disposition %q cannot be committed — notes are committed into the archive tree and moved only by SetDisposition",
			m.Source, m.StableID, m.Disposition)
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

// messageColumns is the column list every Message read shares, in
// scanMessage order.
const messageColumns = `source, stable_id, rfc822_msgid, thread_id, ts_utc, orig_offset,
	day_bucket, rel_path, content_hash, deleted,
	disposition, disposition_reason, disposition_rule, disposition_at, triage_digest`

// GetMessage returns one archived email's row; ok is false when no such
// (source, stable_id) has been committed.
func (d *DB) GetMessage(source, stableID string) (m Message, ok bool, err error) {
	rows, err := d.sql.Query(`SELECT `+messageColumns+` FROM messages WHERE source = ? AND stable_id = ?`, source, stableID)
	if err != nil {
		return Message{}, false, fmt.Errorf("state: get message %s/%s: %w", source, stableID, err)
	}
	defer rows.Close()
	out, err := scanMessages(rows)
	if err != nil {
		return Message{}, false, fmt.Errorf("state: get message %s/%s: %w", source, stableID, err)
	}
	if len(out) == 0 {
		return Message{}, false, nil
	}
	return out[0], true, nil
}

// MessagesByRelPath returns every row whose rel_path is rel. A rel path
// embeds the owning instance's tag and a hash over its instance id, so it
// identifies at most one message in practice; the slice shape is honest
// about the schema, which does not enforce that. It is how `save untriage`
// turns a path the user typed back into a message.
func (d *DB) MessagesByRelPath(rel string) ([]Message, error) {
	rows, err := d.sql.Query(`SELECT `+messageColumns+` FROM messages WHERE rel_path = ? ORDER BY source, stable_id`, rel)
	if err != nil {
		return nil, fmt.Errorf("state: messages by rel path %s: %w", rel, err)
	}
	defer rows.Close()
	out, err := scanMessages(rows)
	if err != nil {
		return nil, fmt.Errorf("state: messages by rel path %s: %w", rel, err)
	}
	return out, nil
}

// MessagesByStableID returns every row with the given stable id, across
// instances: a Gmail message id is unique only within one mailbox, so the
// caller decides what to do with more than one hit. It is how
// `save untriage <id>` finds a note without a path.
func (d *DB) MessagesByStableID(stableID string) ([]Message, error) {
	rows, err := d.sql.Query(`SELECT `+messageColumns+` FROM messages WHERE stable_id = ? ORDER BY source`, stableID)
	if err != nil {
		return nil, fmt.Errorf("state: messages by stable id %s: %w", stableID, err)
	}
	defer rows.Close()
	out, err := scanMessages(rows)
	if err != nil {
		return nil, fmt.Errorf("state: messages by stable id %s: %w", stableID, err)
	}
	return out, nil
}

// SetDisposition records where a note now lives and why. It is the ONLY
// writer of the triage columns, and it must be called AFTER the files are in
// the tree it names: the disposition is the source of truth for location, so
// a row that says spam while the note is still under archive_root would make
// verify report a missing file and the mover skip a move that never happened.
// (The reverse order — files moved, row not yet updated — is the recoverable
// one; see archive.Writer.MoveNote.)
//
// digest is the triage digest the decision was made under; "" records that
// the note is NOT settled under any digest, so the next pass looks at it
// again (used when a layer failed transiently, e.g. an unreachable LLM).
// reason is for people, rule for machines.
func (d *DB) SetDisposition(source, stableID string, disp Disposition, reason, rule, digest string) error {
	where := source + "/" + stableID
	if !ValidDisposition(disp) {
		return fmt.Errorf("state: set disposition %s: %q is not a disposition (one of %v)", where, disp, Dispositions())
	}
	res, err := d.sql.Exec(`
		UPDATE messages
		SET disposition = ?, disposition_reason = ?, disposition_rule = ?,
		    disposition_at = ?, triage_digest = ?
		WHERE source = ? AND stable_id = ?`,
		string(disp), nullStr(reason), nullStr(rule), fmtTime(time.Now()), nullStr(digest),
		source, stableID)
	if err != nil {
		return fmt.Errorf("state: set disposition %s: %w", where, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: set disposition %s: %w", where, err)
	}
	if n == 0 {
		return fmt.Errorf("state: set disposition %s: no such message", where)
	}
	return nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		var msgid, thread, offset, reason, rule, at, digest sql.NullString
		var ts string
		var deleted int
		var disp string
		if err := rows.Scan(&m.Source, &m.StableID, &msgid, &thread, &ts, &offset,
			&m.DayBucket, &m.RelPath, &m.ContentHash, &deleted,
			&disp, &reason, &rule, &at, &digest); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		t, err := time.Parse(tsLayout, ts)
		if err != nil {
			return nil, fmt.Errorf("bad stored ts %q: %w", ts, err)
		}
		m.TS = t
		m.RFC822MsgID, m.ThreadID, m.OrigOffset = msgid.String, thread.String, offset.String
		m.Deleted = deleted != 0
		m.Disposition = Disposition(disp)
		m.DispositionReason, m.DispositionRule, m.TriageDigest = reason.String, rule.String, digest.String
		if m.DispositionAt, err = parseNullTime(at); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan messages: %w", err)
	}
	return out, nil
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
