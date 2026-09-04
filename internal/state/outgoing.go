package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// OutgoingStatus is the durable lifecycle of one file-backed outbound
// message. A sending row is safe to retry because every provider sender uses
// the row's MessageKey as its remote idempotency identity.
type OutgoingStatus string

const (
	OutgoingPending  OutgoingStatus = "pending"
	OutgoingSending  OutgoingStatus = "sending"
	OutgoingSent     OutgoingStatus = "sent"
	OutgoingArchived OutgoingStatus = "archived"
)

// Outgoing is one durable send attempt. DraftRelPath is relative to send/;
// ArchiveRelPath is relative to archived/ and is assigned after delivery.
type Outgoing struct {
	MessageKey     string
	Source         string
	Kind           string
	DraftRelPath   string
	ContentHash    string
	Status         OutgoingStatus
	Attempts       int
	ProviderID     string
	ArchiveRelPath string
	LastError      string
	PreparedAt     time.Time
	UpdatedAt      time.Time
	SentAt         time.Time
	ArchivedAt     time.Time
}

// PrepareOutgoing inserts a new pending row, or returns the existing row for
// the same deterministic message key. The fields forming the local identity
// must agree on a replay.
func (d *DB) PrepareOutgoing(o Outgoing) (Outgoing, error) {
	if err := requireInstance("prepare outgoing", o.Source); err != nil {
		return Outgoing{}, err
	}
	if o.MessageKey == "" || o.DraftRelPath == "" || o.ContentHash == "" {
		return Outgoing{}, errors.New("state: prepare outgoing: message key, draft path, and content hash are required")
	}
	if o.Kind != "email" && o.Kind != "chat" {
		return Outgoing{}, fmt.Errorf("state: prepare outgoing: invalid kind %q", o.Kind)
	}
	now := time.Now()
	if o.PreparedAt.IsZero() {
		o.PreparedAt = now
	}
	_, err := d.sql.Exec(`
		INSERT INTO outgoing_messages
		(message_key, source, kind, draft_rel_path, content_hash, status, prepared_at, updated_at)
		VALUES (?,?,?,?,?,'pending',?,?)
		ON CONFLICT(message_key) DO NOTHING`,
		o.MessageKey, o.Source, o.Kind, o.DraftRelPath, o.ContentHash,
		fmtTime(o.PreparedAt), fmtTime(now))
	if err != nil {
		return Outgoing{}, fmt.Errorf("state: prepare outgoing %s: %w", o.MessageKey, err)
	}
	got, ok, err := d.GetOutgoing(o.MessageKey)
	if err != nil {
		return Outgoing{}, err
	}
	if !ok {
		return Outgoing{}, fmt.Errorf("state: prepare outgoing %s: row disappeared", o.MessageKey)
	}
	if got.Source != o.Source || got.Kind != o.Kind || got.DraftRelPath != o.DraftRelPath || got.ContentHash != o.ContentHash {
		return Outgoing{}, fmt.Errorf("state: outgoing key %s collides with a different draft", o.MessageKey)
	}
	return got, nil
}

// GetOutgoing returns one ledger row.
func (d *DB) GetOutgoing(key string) (Outgoing, bool, error) {
	row := d.sql.QueryRow(`
		SELECT message_key, source, kind, draft_rel_path, content_hash, status,
		       attempts, COALESCE(provider_id,''), COALESCE(archive_rel_path,''),
		       COALESCE(last_error,''), prepared_at, updated_at, sent_at, archived_at
		FROM outgoing_messages WHERE message_key = ?`, key)
	o, err := scanOutgoing(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Outgoing{}, false, nil
	}
	if err != nil {
		return Outgoing{}, false, fmt.Errorf("state: get outgoing %s: %w", key, err)
	}
	return o, true, nil
}

type rowScanner interface{ Scan(...any) error }

func scanOutgoing(row rowScanner) (Outgoing, error) {
	var o Outgoing
	var prepared, updated string
	var sent, archived sql.NullString
	err := row.Scan(&o.MessageKey, &o.Source, &o.Kind, &o.DraftRelPath, &o.ContentHash,
		&o.Status, &o.Attempts, &o.ProviderID, &o.ArchiveRelPath, &o.LastError,
		&prepared, &updated, &sent, &archived)
	if err != nil {
		return Outgoing{}, err
	}
	if o.PreparedAt, err = parseTime(prepared); err != nil {
		return Outgoing{}, err
	}
	if o.UpdatedAt, err = parseTime(updated); err != nil {
		return Outgoing{}, err
	}
	if o.SentAt, err = parseNullTime(sent); err != nil {
		return Outgoing{}, err
	}
	if o.ArchivedAt, err = parseNullTime(archived); err != nil {
		return Outgoing{}, err
	}
	return o, nil
}

// BeginOutgoingAttempt records the point immediately before a provider call.
func (d *DB) BeginOutgoingAttempt(key string) error {
	res, err := d.sql.Exec(`
		UPDATE outgoing_messages SET status = 'sending', attempts = attempts + 1,
		last_error = NULL, updated_at = ?
		WHERE message_key = ? AND status IN ('pending','sending')`, fmtTime(time.Now()), key)
	return outgoingChanged("begin attempt", key, res, err)
}

// FailOutgoing records an error but deliberately leaves the row in sending:
// a lost HTTP response is ambiguous, and the provider sender must reconcile
// by its deterministic remote identity on the next explicit run.
func (d *DB) FailOutgoing(key, lastErr string) error {
	res, err := d.sql.Exec(`
		UPDATE outgoing_messages SET last_error = ?, updated_at = ?
		WHERE message_key = ? AND status = 'sending'`, nullStr(lastErr), fmtTime(time.Now()), key)
	return outgoingChanged("record failure", key, res, err)
}

// MarkOutgoingSent records remote acceptance before the local file is moved.
func (d *DB) MarkOutgoingSent(key, providerID, archiveRel string, sentAt time.Time) error {
	if providerID == "" || archiveRel == "" || sentAt.IsZero() {
		return errors.New("state: mark outgoing sent: provider id, archive path, and sent time are required")
	}
	res, err := d.sql.Exec(`
		UPDATE outgoing_messages SET status = 'sent', provider_id = ?,
		archive_rel_path = ?, sent_at = ?, last_error = NULL, updated_at = ?
		WHERE message_key = ? AND status IN ('sending','sent')`,
		providerID, archiveRel, fmtTime(sentAt), fmtTime(time.Now()), key)
	return outgoingChanged("mark sent", key, res, err)
}

// MarkOutgoingArchived completes the lifecycle after the atomic rename.
func (d *DB) MarkOutgoingArchived(key string) error {
	now := fmtTime(time.Now())
	res, err := d.sql.Exec(`
		UPDATE outgoing_messages SET status = 'archived', archived_at = ?, updated_at = ?
		WHERE message_key = ? AND status IN ('sent','archived')`, now, now, key)
	return outgoingChanged("mark archived", key, res, err)
}

// UnarchivedOutgoing returns every remotely sent row whose local move may
// have been interrupted, oldest first.
func (d *DB) UnarchivedOutgoing() ([]Outgoing, error) {
	rows, err := d.sql.Query(`
		SELECT message_key, source, kind, draft_rel_path, content_hash, status,
		       attempts, COALESCE(provider_id,''), COALESCE(archive_rel_path,''),
		       COALESCE(last_error,''), prepared_at, updated_at, sent_at, archived_at
		FROM outgoing_messages WHERE status = 'sent' ORDER BY prepared_at, message_key`)
	if err != nil {
		return nil, fmt.Errorf("state: list unarchived outgoing: %w", err)
	}
	defer rows.Close()
	var out []Outgoing
	for rows.Next() {
		o, err := scanOutgoing(rows)
		if err != nil {
			return nil, fmt.Errorf("state: list unarchived outgoing: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list unarchived outgoing: %w", err)
	}
	return out, nil
}

func outgoingChanged(op, key string, res sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("state: %s outgoing %s: %w", op, key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: %s outgoing %s: %w", op, key, err)
	}
	if n == 0 {
		return fmt.Errorf("state: %s outgoing %s: invalid or missing lifecycle state", op, key)
	}
	return nil
}
