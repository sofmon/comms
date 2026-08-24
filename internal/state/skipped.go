package state

import (
	"database/sql"
	"fmt"
	"slices"
	"time"

	"save/internal/policy"
)

// Resolutions for a skipped_attachments row. A resolution is terminal: it
// says how the skip stopped being an open question, not that the bytes are
// necessarily in the archive.
const (
	// SkipResolutionFetched: `save refetch` fetched the bytes, they passed
	// the current policy, and the file is on disk. The owning note must be
	// re-rendered so it shows a link instead of the skip entry.
	SkipResolutionFetched = "fetched"

	// SkipResolutionSourceGone: the message or the part no longer exists
	// upstream, so the bytes are unrecoverable. The note stays honest — it
	// keeps saying the attachment was skipped — and refetch stops asking.
	SkipResolutionSourceGone = "source_gone"

	// SkipResolutionStillDenied: re-evaluated under the widened policy and
	// still refused. The row drops out of ListUnresolvedSkipped so a refetch
	// does not re-decide it on every run; recording the skip again (a later
	// sync of the same message) re-opens it.
	SkipResolutionStillDenied = "still_denied"
)

// SkipResolutions returns every valid resolution, in a stable order. The
// schema's CHECK constraint is generated from this list.
func SkipResolutions() []string {
	return []string{SkipResolutionFetched, SkipResolutionSourceGone, SkipResolutionStillDenied}
}

// ValidSkipResolution reports whether r is one of the SkipResolution
// constants. The empty string is NOT a resolution — it is the absence of one.
func ValidSkipResolution(r string) bool { return slices.Contains(SkipResolutions(), r) }

// policyDigestChars is the width of a policy.Policy.PolicyDigest, taken from
// the policy package itself rather than duplicated as a literal, so widening
// the digest can never leave this validation silently rejecting every write.
var policyDigestChars = len(policy.Default().PolicyDigest())

// sha256HexChars is the width of a hex-encoded SHA-256.
const sha256HexChars = 64

// SkippedAttachment is one refusal by the attachment storage policy: an
// attachment save decided not to store, recorded with enough identity to
// fetch it later and enough detail for the .md note to explain itself.
//
// Nothing is ever dropped silently: every Verdict with Store == false becomes
// one of these rows AND one entry in the owning note. The two must agree, so
// the note path and day bucket are part of the row.
type SkippedAttachment struct {
	// Identity — the same triple that keys the attachments ledger, so a skip
	// and a later successful download of the same part are the same key.
	Source   string // instance id, e.g. "gmail:work"; NEVER a bare kind
	StableID string // owning message (gmail message id / JMAP Email id / chat resource name)
	PartKey  string // dotted MIME index ("2.1.3") or attachment resource name

	// What arrived. OrigName is the sender's filename and SanitizedName the
	// name save would have written (naming.SanitizeFilename output, the same
	// string the policy keyed its decision on). Either may be empty: inline
	// parts often carry no filename at all.
	OrigName      string
	SanitizedName string

	// SizeBytes is the DECODED byte count (transfer encoding already removed).
	SizeBytes int64

	// DeclaredType is the part's Content-Type header and DeclaredExt the
	// extension the filename claimed, both as received. SniffedType is what
	// the magic bytes actually said — empty only when the bytes were never
	// fetched (a Chat blob refused by policy.PreCheck).
	DeclaredType string
	DeclaredExt  string
	SniffedType  string

	// Reason is a policy.Reason* constant: the machine-readable answer to
	// "why not?", and what `save refetch` re-decides against.
	Reason string

	// PolicyDigest is the policy.Policy.PolicyDigest in force when the
	// decision was made. A row whose digest differs from the current policy's
	// is worth re-deciding; policy.Transient reasons are worth retrying even
	// when it does not.
	PolicyDigest string

	// ContentSHA256 is the hex SHA-256 of the decoded bytes, or "" when there
	// are no bytes to hash. Gmail and FastMail download the whole message, so
	// the bytes existed and were discarded; a Chat blob refused before the
	// download genuinely was never fetched. Refetch verifies against this
	// value and records a divergence rather than overwriting silently.
	ContentSHA256 string

	// NoteRelPath is the .md that carries the human-readable skip entry
	// (relative to the archive root), and DayBucket its "YYYY-MM-DD" in the
	// pinned archive timezone. A skip nobody can find in a note is a silent
	// drop, so both are required.
	NoteRelPath string
	DayBucket   string

	// FirstSeenAt is when the skip was first recorded; re-recording the same
	// skip preserves it. Zero on input means "now".
	FirstSeenAt time.Time

	// ResolvedAt and Resolution are output-only: they are set by
	// MarkSkippedResolved, never by UpsertSkipped, and always move together.
	ResolvedAt time.Time
	Resolution string
}

// Resolved reports whether the skip has been dispositioned.
func (s SkippedAttachment) Resolved() bool { return s.Resolution != "" }

// skippedColumns is the column list every read shares, in scanSkipped order.
const skippedColumns = `source, stable_id, part_key, orig_name, sanitized_name,
	size_bytes, declared_type, declared_ext, sniffed_type, reason, policy_digest,
	content_sha256, note_rel_path, day_bucket, first_seen_at, resolved_at, resolution`

// upsertSkippedSQL records a refusal. first_seen_at is deliberately absent
// from the SET list — the row keeps the moment it was first refused — and
// content_sha256 is COALESCEd so a later observation that has no bytes to
// hash (a pre-download refusal) never erases a hash an earlier one captured.
//
// resolved_at/resolution are forced back to NULL: recording a skip means the
// attachment is refused RIGHT NOW, so a previously resolved row re-opens.
const upsertSkippedSQL = `
	INSERT INTO skipped_attachments (` + skippedColumns + `)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,NULL)
	ON CONFLICT(source, stable_id, part_key) DO UPDATE SET
		orig_name      = excluded.orig_name,
		sanitized_name = excluded.sanitized_name,
		size_bytes     = excluded.size_bytes,
		declared_type  = excluded.declared_type,
		declared_ext   = excluded.declared_ext,
		sniffed_type   = excluded.sniffed_type,
		reason         = excluded.reason,
		policy_digest  = excluded.policy_digest,
		content_sha256 = COALESCE(excluded.content_sha256, skipped_attachments.content_sha256),
		note_rel_path  = excluded.note_rel_path,
		day_bucket     = excluded.day_bucket,
		resolved_at    = NULL,
		resolution     = NULL`

// UpsertSkipped records one refused attachment, replacing any earlier record
// of the same (source, stable_id, part_key). See upsertSkippedSQL for exactly
// what a re-record preserves.
func (d *DB) UpsertSkipped(s SkippedAttachment) error {
	args, err := skippedArgs(s)
	if err != nil {
		return err
	}
	if _, err := d.sql.Exec(upsertSkippedSQL, args...); err != nil {
		return fmt.Errorf("state: record skipped attachment %s/%s/%s: %w", s.Source, s.StableID, s.PartKey, err)
	}
	return nil
}

// UpsertSkippedBatch records every refusal of one message in a single
// transaction, so a crash can never leave a note listing skips the DB has
// only half of. An empty slice is a no-op; the whole batch is validated
// before anything is written.
func (d *DB) UpsertSkippedBatch(rows []SkippedAttachment) error {
	if len(rows) == 0 {
		return nil
	}
	args := make([][]any, len(rows))
	for i, s := range rows {
		a, err := skippedArgs(s)
		if err != nil {
			return err
		}
		args[i] = a
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("state: record skipped attachments: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(upsertSkippedSQL)
	if err != nil {
		return fmt.Errorf("state: record skipped attachments: %w", err)
	}
	defer stmt.Close()
	for i, a := range args {
		if _, err := stmt.Exec(a...); err != nil {
			return fmt.Errorf("state: record skipped attachment %s/%s/%s: %w",
				rows[i].Source, rows[i].StableID, rows[i].PartKey, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: record skipped attachments: %w", err)
	}
	return nil
}

// skippedArgs validates one row and renders it as upsertSkippedSQL's
// parameters. Validation is in Go rather than left to the schema's CHECKs so
// the error names the field and the caller, not a constraint number.
func skippedArgs(s SkippedAttachment) ([]any, error) {
	where := fmt.Sprintf("%s/%s/%s", s.Source, s.StableID, s.PartKey)
	fail := func(format string, a ...any) ([]any, error) {
		return nil, fmt.Errorf("state: record skipped attachment %s: %s", where, fmt.Sprintf(format, a...))
	}

	if err := requireInstance("record skipped attachment "+s.StableID+"/"+s.PartKey, s.Source); err != nil {
		return nil, err
	}
	if s.StableID == "" || s.PartKey == "" {
		return fail("stable id and part key are required — they are how `save refetch` finds the bytes again")
	}
	if s.NoteRelPath == "" || s.DayBucket == "" {
		return fail("note rel path and day bucket are required — a skip that is not tied to a note is a silent drop")
	}
	if !policy.ValidReason(s.Reason) || s.Reason == policy.ReasonNone {
		return fail("reason %q is not a policy.Reason* constant (one of %v)", s.Reason, policy.Reasons())
	}
	if !isLowerHex(s.PolicyDigest, policyDigestChars) {
		return fail("policy digest %q is not %d lowercase hex characters — pass policy.Policy.PolicyDigest()", s.PolicyDigest, policyDigestChars)
	}
	if s.ContentSHA256 != "" && !isLowerHex(s.ContentSHA256, sha256HexChars) {
		return fail("content sha256 %q is not %d lowercase hex characters (leave it empty when the bytes were never fetched)", s.ContentSHA256, sha256HexChars)
	}
	if s.SizeBytes < 0 {
		return fail("size %d is negative", s.SizeBytes)
	}

	first := s.FirstSeenAt
	if first.IsZero() {
		first = time.Now()
	}
	return []any{
		s.Source, s.StableID, s.PartKey, s.OrigName, s.SanitizedName,
		s.SizeBytes, s.DeclaredType, s.DeclaredExt, s.SniffedType, s.Reason,
		s.PolicyDigest, nullStr(s.ContentSHA256), s.NoteRelPath, s.DayBucket,
		fmtTime(first),
	}, nil
}

// ListUnresolvedSkipped returns skips that still stand, across EVERY
// configured instance, ordered by instance then by when they were first
// refused — the order `save refetch` walks them in, and grouped so one
// account's backlog is contiguous. limit <= 0 means no limit.
func (d *DB) ListUnresolvedSkipped(limit int) ([]SkippedAttachment, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := d.sql.Query(`
		SELECT `+skippedColumns+`
		FROM skipped_attachments
		WHERE resolved_at IS NULL
		ORDER BY source, first_seen_at, stable_id, part_key
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("state: list unresolved skipped attachments: %w", err)
	}
	defer rows.Close()
	return scanSkipped(rows)
}

// MarkSkippedResolved dispositions one skip. It is an error to resolve a row
// that does not exist: a refetch that thinks it resolved something the ledger
// never recorded is a bug worth surfacing, not a no-op.
func (d *DB) MarkSkippedResolved(source, stableID, partKey, resolution string) error {
	where := fmt.Sprintf("%s/%s/%s", source, stableID, partKey)
	if !ValidSkipResolution(resolution) {
		return fmt.Errorf("state: resolve skipped attachment %s: resolution %q is not valid (one of %v)",
			where, resolution, SkipResolutions())
	}
	res, err := d.sql.Exec(`
		UPDATE skipped_attachments SET resolved_at = ?, resolution = ?
		WHERE source = ? AND stable_id = ? AND part_key = ?`,
		fmtTime(time.Now()), resolution, source, stableID, partKey)
	if err != nil {
		return fmt.Errorf("state: resolve skipped attachment %s: %w", where, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: resolve skipped attachment %s: %w", where, err)
	}
	if n == 0 {
		return fmt.Errorf("state: resolve skipped attachment %s: no such row", where)
	}
	return nil
}

// SkippedForNote returns every skip recorded against one note, resolved and
// unresolved, ordered by instance and part key — everything a re-render needs
// to restate the note's `skipped_attachments:` list without re-deciding
// anything.
//
// Resolved rows are included on purpose, because their resolution changes
// what the note should say: SkipResolutionFetched means the bytes are now on
// disk and the entry must become a link, while SkipResolutionSourceGone and
// SkipResolutionStillDenied are still skips and must still be shown.
//
// The part-key order is lexicographic, matching AttachmentsForMessage, so
// dotted MIME indices sort "10" before "2". A renderer that wants numeric
// MIME order must re-sort; the order here only has to be deterministic, so
// that re-rendering an unchanged note produces identical bytes.
func (d *DB) SkippedForNote(noteRelPath string) ([]SkippedAttachment, error) {
	rows, err := d.sql.Query(`
		SELECT `+skippedColumns+`
		FROM skipped_attachments
		WHERE note_rel_path = ?
		ORDER BY source, stable_id, part_key`, noteRelPath)
	if err != nil {
		return nil, fmt.Errorf("state: skipped attachments for note %s: %w", noteRelPath, err)
	}
	defer rows.Close()
	return scanSkipped(rows)
}

// SkipCounts is one instance's skipped-attachment tally for `save status`.
// The ByReason maps are keyed by policy.Reason* constants and omit reasons
// with no rows.
type SkipCounts struct {
	Total      int64 // every skip ever recorded for this instance
	Unresolved int64 // the subset that still stands

	ByReason           map[string]int64 // Total, split by reason
	UnresolvedByReason map[string]int64 // Unresolved, split by reason
}

// CountSkipped returns the per-instance, per-reason skip tallies, keyed by
// instance id. Instances with no skips are absent from the map.
func (d *DB) CountSkipped() (map[string]SkipCounts, error) {
	rows, err := d.sql.Query(`
		SELECT source, reason, resolved_at IS NULL, COUNT(*)
		FROM skipped_attachments
		GROUP BY source, reason, resolved_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("state: skipped attachment counts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]SkipCounts)
	for rows.Next() {
		var source, reason string
		var unresolved bool
		var n int64
		if err := rows.Scan(&source, &reason, &unresolved, &n); err != nil {
			return nil, fmt.Errorf("state: skipped attachment counts: %w", err)
		}
		c, ok := out[source]
		if !ok {
			c = SkipCounts{
				ByReason:           make(map[string]int64),
				UnresolvedByReason: make(map[string]int64),
			}
		}
		c.Total += n
		c.ByReason[reason] += n
		if unresolved {
			c.Unresolved += n
			c.UnresolvedByReason[reason] += n
		}
		out[source] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: skipped attachment counts: %w", err)
	}
	return out, nil
}

func scanSkipped(rows *sql.Rows) ([]SkippedAttachment, error) {
	var out []SkippedAttachment
	for rows.Next() {
		var s SkippedAttachment
		var sha, resolvedAt, resolution sql.NullString
		var firstSeen string
		if err := rows.Scan(&s.Source, &s.StableID, &s.PartKey, &s.OrigName,
			&s.SanitizedName, &s.SizeBytes, &s.DeclaredType, &s.DeclaredExt,
			&s.SniffedType, &s.Reason, &s.PolicyDigest, &sha, &s.NoteRelPath,
			&s.DayBucket, &firstSeen, &resolvedAt, &resolution); err != nil {
			return nil, fmt.Errorf("state: scan skipped attachment: %w", err)
		}
		var err error
		if s.FirstSeenAt, err = parseTime(firstSeen); err != nil {
			return nil, err
		}
		if s.ResolvedAt, err = parseNullTime(resolvedAt); err != nil {
			return nil, err
		}
		s.ContentSHA256 = sha.String
		s.Resolution = resolution.String
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: scan skipped attachments: %w", err)
	}
	return out, nil
}

// --- attachment policy digest ---

// AttachmentPolicyDigest returns the policy digest recorded for this archive;
// ok is false before the first sync has stored one. A digest that differs
// from the current policy's means unresolved skips are worth re-deciding.
func (d *DB) AttachmentPolicyDigest() (digest string, ok bool, err error) {
	return d.GetMeta(MetaAttachmentPolicyDigest)
}

// SetAttachmentPolicyDigest records the policy digest in force. It rejects
// anything that is not a well-formed digest, so a caller that passes a
// canonical policy string, a config value or an empty string fails loudly
// instead of poisoning the "has the policy changed?" comparison forever.
func (d *DB) SetAttachmentPolicyDigest(digest string) error {
	if !isLowerHex(digest, policyDigestChars) {
		return fmt.Errorf("state: set attachment policy digest: %q is not %d lowercase hex characters — pass policy.Policy.PolicyDigest()",
			digest, policyDigestChars)
	}
	return d.SetMeta(MetaAttachmentPolicyDigest, digest)
}

// isLowerHex reports whether s is exactly n lowercase hex digits.
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
