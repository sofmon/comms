// Package state is the single SQLite store for everything the archiver must
// remember between runs: archive metadata, sync cursors, the per-message
// dedup index, the Gmail backfill work queue, out-of-band attachment
// downloads, the canonical Google Chat message store with its day-file
// projection ledger, and observability ledgers (sync runs, poison-item
// failures).
//
// The store assumes a single writing process (enforced elsewhere by flock);
// within the process it serializes on one connection. All timestamps are
// stored as RFC 3339 UTC strings; server timestamps that participate in
// ordering use a fixed-width nanosecond format (see tsLayout).
//
// EVERY table is keyed by an INSTANCE id — "<kind>:<label>", one configured
// account's one source (see InstanceID) — in its `source` column, never by a
// bare source kind: several accounts of the same kind can be configured, and
// two of them can legitimately see the same space, message id or mailbox.
package state

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers driver "sqlite"

	"save/internal/policy"
)

// schemaVersion gates migrations via PRAGMA user_version.
const schemaVersion = 1

// Well-known meta keys.
const (
	MetaArchiveTZ   = "archive_tz"
	MetaArchiveRoot = "archive_root"

	// MetaAttachmentPolicyDigest is the policy.Policy.PolicyDigest that was in
	// force the last time this archive was synced. When the digest computed
	// from the current config differs, previously skipped attachments may now
	// be storable: `save status` reports the count and `save refetch` — never
	// an automatic sync — actually fetches them. Read and write it through
	// AttachmentPolicyDigest / SetAttachmentPolicyDigest.
	MetaAttachmentPolicyDigest = "attachment_policy_digest"
)

// Source KINDS. These are not source column values on their own: every row
// is keyed by an INSTANCE id combining a kind with one account's label (see
// InstanceID), because the same kind can be configured for several accounts.
const (
	SourceGmail    = "gmail"
	SourceGChat    = "gchat"
	SourceFastmail = "fastmail"
)

// instanceSep separates the kind from the account label in an instance id.
// It is deliberately a character that can never appear in a filename
// component, so an instance id accidentally used as a file tag is loud
// rather than silently wrong (see Tag).
const instanceSep = ":"

// InstanceID builds the source key stored in every source column:
// "<kind>:<label>", e.g. "gmail:work". Kind is one of SourceGmail,
// SourceGChat, SourceFastmail; label is the account's config label.
func InstanceID(kind, label string) string { return kind + instanceSep + label }

// SplitInstance splits an instance id back into its kind and account label.
// ok is false for anything that is not exactly "<kind>:<label>" with both
// parts non-empty (a bare kind constant, in particular, is not an id).
func SplitInstance(id string) (kind, label string, ok bool) {
	kind, label, found := strings.Cut(id, instanceSep)
	if !found || kind == "" || label == "" || strings.Contains(label, instanceSep) {
		return "", "", false
	}
	return kind, label, true
}

// KindOf returns the source kind of an instance id, or "" when id is not a
// well-formed instance id.
func KindOf(id string) string {
	kind, _, ok := SplitInstance(id)
	if !ok {
		return ""
	}
	return kind
}

// Tag renders an instance id as the FILE TAG that names the account in
// archive filenames: "gmail:work" becomes "gmail-work" (a colon is not a
// legal filename character). The tag is for filenames only — it must never
// be used as a state key, just as an instance id must never reach a path.
func Tag(instanceID string) string {
	return strings.ReplaceAll(instanceID, instanceSep, "-")
}

// requireInstance rejects a source column value that is not a well-formed
// instance id. Rows keyed by a bare kind constant would be shared by every
// configured account, and the chat day-file path derives its file tag from
// this value — both failures are silent, so the write paths check up front.
func requireInstance(op, source string) error {
	if _, _, ok := SplitInstance(source); !ok {
		return fmt.Errorf("state: %s: source %q is not an instance id — use InstanceID(kind, label), e.g. %q",
			op, source, InstanceID(SourceGChat, "work"))
	}
	return nil
}

// CursorMsgCreateTime is the per-space chat cursor kind advanced by
// ApplyChatPage; connectors read it via GetCursor(<gchat instance>, space,
// CursorMsgCreateTime).
const CursorMsgCreateTime = "msg_create_time"

// tsLayout formats server timestamps (chat create_time, message ts_utc) with
// fixed-width nanoseconds so byte-wise string comparison — ORDER BY, cursor
// advancement — matches chronological order. Variable-width RFC 3339
// fractions do not sort correctly ("...05Z" > "...05.5Z" byte-wise).
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

const schemaV1 = `
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE cursors (
  source     TEXT NOT NULL,
  scope      TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (source, scope, kind)
);

CREATE TABLE messages (
  source        TEXT NOT NULL,
  stable_id     TEXT NOT NULL,
  rfc822_msgid  TEXT,
  thread_id     TEXT,
  ts_utc        TEXT NOT NULL,
  orig_offset   TEXT,
  day_bucket    TEXT NOT NULL,
  rel_path      TEXT NOT NULL,
  content_hash  TEXT NOT NULL,
  deleted       INTEGER NOT NULL DEFAULT 0,
  archived_at   TEXT NOT NULL,
  PRIMARY KEY (source, stable_id)
);
CREATE INDEX idx_messages_msgid ON messages(rfc822_msgid);
CREATE INDEX idx_messages_day   ON messages(day_bucket);

CREATE TABLE attachments (
  source        TEXT NOT NULL,
  stable_id     TEXT NOT NULL,
  part_key      TEXT NOT NULL,
  rel_path      TEXT NOT NULL,
  bytes         INTEGER,
  status        TEXT NOT NULL,
  attempts      INTEGER NOT NULL DEFAULT 0,
  next_retry_at TEXT,
  last_error    TEXT,
  PRIMARY KEY (source, stable_id, part_key)
);
CREATE INDEX idx_att_due ON attachments(source, status, next_retry_at);

CREATE TABLE gmail_backfill (
  source TEXT NOT NULL,
  msg_id TEXT NOT NULL,
  state  TEXT NOT NULL DEFAULT 'pending',
  PRIMARY KEY (source, msg_id)
);
CREATE INDEX idx_gbf_pending ON gmail_backfill(source) WHERE state = 'pending';

CREATE TABLE chat_spaces (
  source        TEXT NOT NULL,
  space_name    TEXT NOT NULL,
  space_type    TEXT NOT NULL,
  display_name  TEXT,
  display_slug  TEXT NOT NULL,
  history_state TEXT,
  first_seen_at TEXT NOT NULL,
  last_synced_at TEXT,
  PRIMARY KEY (source, space_name)
);

CREATE TABLE chat_messages (
  source           TEXT NOT NULL,
  msg_name         TEXT NOT NULL,
  space_name       TEXT NOT NULL,
  thread_name      TEXT,
  sender_id        TEXT NOT NULL,
  create_time      TEXT NOT NULL,
  last_update_time TEXT,
  day_bucket       TEXT NOT NULL,
  raw_json         TEXT NOT NULL,
  edited           INTEGER NOT NULL DEFAULT 0,
  deleted          INTEGER NOT NULL DEFAULT 0,
  deleted_at       TEXT,
  PRIMARY KEY (source, msg_name),
  FOREIGN KEY (source, space_name) REFERENCES chat_spaces(source, space_name)
);
CREATE INDEX idx_chat_day ON chat_messages(source, space_name, day_bucket, create_time);

CREATE TABLE chat_members (
  source       TEXT NOT NULL,
  space_name   TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  display_name TEXT,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (source, space_name, user_id)
);

CREATE TABLE chat_day_files (
  source       TEXT NOT NULL,
  space_name   TEXT NOT NULL,
  day_bucket   TEXT NOT NULL,
  rel_path     TEXT NOT NULL,
  dirty        INTEGER NOT NULL DEFAULT 1,
  dirty_seq    INTEGER NOT NULL DEFAULT 1,
  content_hash TEXT,
  rendered_at  TEXT,
  PRIMARY KEY (source, space_name, day_bucket)
);
CREATE INDEX idx_cdf_dirty ON chat_day_files(dirty) WHERE dirty = 1;

CREATE TABLE sync_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  source        TEXT NOT NULL,
  kind          TEXT NOT NULL,
  started_at    TEXT NOT NULL,
  finished_at   TEXT,
  ok            INTEGER,
  new_msgs      INTEGER NOT NULL DEFAULT 0,
  updated_msgs  INTEGER NOT NULL DEFAULT 0,
  atts_done     INTEGER NOT NULL DEFAULT 0,
  atts_failed   INTEGER NOT NULL DEFAULT 0,
  error         TEXT
);

CREATE TABLE failures (
  source     TEXT NOT NULL,
  id         TEXT NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT,
  last_at    TEXT NOT NULL,
  PRIMARY KEY (source, id)
);

-- skipped_attachments is the never-drop-silently ledger: one row for every
-- attachment the storage policy REFUSED, carrying both the human record that
-- the .md note restates and the identity "save refetch" needs to fetch the
-- bytes later. It is keyed exactly like attachments — (source, stable_id,
-- part_key) — so a skip and a completed download of the same part line up,
-- and so two accounts that see the same logical attachment keep separate rows.
--
-- content_sha256 is NULLABLE on purpose: on Gmail and FastMail the bytes were
-- downloaded and discarded, so there is a hash, but a Chat blob refused by
-- policy.PreCheck was never fetched at all and there is nothing to hash.
-- Recording a hash we do not have would be a lie the divergence check on
-- refetch would then act on.
--
-- resolved_at/resolution move together (the CHECK enforces it) and are NULL
-- while the skip still stands.
CREATE TABLE skipped_attachments (
  source         TEXT    NOT NULL,
  stable_id      TEXT    NOT NULL,
  part_key       TEXT    NOT NULL,
  orig_name      TEXT    NOT NULL,
  sanitized_name TEXT    NOT NULL,
  size_bytes     INTEGER NOT NULL,
  declared_type  TEXT    NOT NULL,
  declared_ext   TEXT    NOT NULL,
  sniffed_type   TEXT    NOT NULL,
  reason         TEXT    NOT NULL CHECK (reason IN (@REASONS@)),
  policy_digest  TEXT    NOT NULL,
  content_sha256 TEXT,
  note_rel_path  TEXT    NOT NULL,
  day_bucket     TEXT    NOT NULL,
  first_seen_at  TEXT    NOT NULL,
  resolved_at    TEXT,
  resolution     TEXT    CHECK (resolution IS NULL OR resolution IN (@RESOLUTIONS@)),
  CHECK ((resolved_at IS NULL) = (resolution IS NULL)),
  PRIMARY KEY (source, stable_id, part_key)
);
CREATE INDEX idx_skipped_unresolved ON skipped_attachments(source, first_seen_at)
  WHERE resolved_at IS NULL;
CREATE INDEX idx_skipped_note ON skipped_attachments(note_rel_path);
`

// Placeholders substituted by schemaSQL, so the reason and resolution enums
// have exactly ONE definition each in the tree (policy.Reasons and
// SkipResolutions) rather than a second, silently drifting copy in SQL.
const (
	reasonsToken     = "@REASONS@"
	resolutionsToken = "@RESOLUTIONS@"
)

// schemaSQL is the v1 schema with the enum CHECK lists filled in.
func schemaSQL() string {
	s := strings.Replace(schemaV1, reasonsToken, sqlStringList(policy.Reasons()), 1)
	return strings.Replace(s, resolutionsToken, sqlStringList(SkipResolutions()), 1)
}

// sqlStringList renders values as a SQL string literal list: 'a', 'b', 'c'.
func sqlStringList(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return strings.Join(quoted, ", ")
}

// DB is the archiver's state store. Safe for concurrent use within one
// process; cross-process single-writer discipline is the flock's job.
type DB struct {
	sql *sql.DB
}

// Open opens (creating and migrating as needed) the state database at path.
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	// One connection serializes all statements, so in-process writers never
	// see SQLITE_BUSY and transactions cannot interleave.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("state: migrate %s: %w", path, err)
	}
	return &DB{sql: db}, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	switch {
	case v == schemaVersion:
		return nil
	case v > schemaVersion:
		return fmt.Errorf("schema version %d is newer than supported %d", v, schemaVersion)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schemaSQL()); err != nil {
		return fmt.Errorf("apply schema v1: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return tx.Commit()
}

// Close closes the underlying database.
func (d *DB) Close() error {
	return d.sql.Close()
}

// GetMeta returns the value for key; ok is false when the key is absent.
func (d *DB) GetMeta(key string) (value string, ok bool, err error) {
	err = d.sql.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("state: get meta %q: %w", key, err)
	}
	return value, true, nil
}

// SetMeta inserts or replaces the value for key.
func (d *DB) SetMeta(key, value string) error {
	_, err := d.sql.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("state: set meta %q: %w", key, err)
	}
	return nil
}

const upsertCursorSQL = `
	INSERT INTO cursors (source, scope, kind, value, updated_at) VALUES (?,?,?,?,?)
	ON CONFLICT(source, scope, kind) DO UPDATE SET
		value = excluded.value, updated_at = excluded.updated_at`

// GetCursor returns the cursor value for (source, scope, kind); ok is false
// when no cursor is stored.
func (d *DB) GetCursor(source, scope, kind string) (value string, ok bool, err error) {
	err = d.sql.QueryRow(`
		SELECT value FROM cursors WHERE source = ? AND scope = ? AND kind = ?`,
		source, scope, kind).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("state: get cursor %s/%s/%s: %w", source, scope, kind, err)
	}
	return value, true, nil
}

// SetCursor inserts or replaces the cursor value for (source, scope, kind).
func (d *DB) SetCursor(source, scope, kind, value string) error {
	_, err := d.sql.Exec(upsertCursorSQL, source, scope, kind, value, fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("state: set cursor %s/%s/%s: %w", source, scope, kind, err)
	}
	return nil
}

// DeleteCursor removes the cursor for (source, scope, kind); deleting an
// absent cursor is a no-op.
func (d *DB) DeleteCursor(source, scope, kind string) error {
	_, err := d.sql.Exec(`
		DELETE FROM cursors WHERE source = ? AND scope = ? AND kind = ?`,
		source, scope, kind)
	if err != nil {
		return fmt.Errorf("state: delete cursor %s/%s/%s: %w", source, scope, kind, err)
	}
	return nil
}

// --- helpers ---

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func fmtTS(t time.Time) string { return t.UTC().Format(tsLayout) }

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fmtTime(t)
}

func nullTS(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return fmtTS(t)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("state: bad stored time %q: %w", s, err)
	}
	return t, nil
}

func parseNullTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid {
		return time.Time{}, nil
	}
	return parseTime(ns.String)
}
