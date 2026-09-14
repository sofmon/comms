package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	"comms/internal/naming"
)

// Every chat table is keyed by the INSTANCE the rows were fetched with (the
// `source` column, e.g. "gchat:work"): two configured accounts can be
// members of the same space and see the same message resource names, and
// each archives its own independent copy.

// Space is one Google Chat space (DM, group chat, or named space) as seen by
// one account.
type Space struct {
	Source       string // instance id of the account that sees this space
	Name         string // resource name 'spaces/AAAA'
	Type         string // 'dm' | 'group' | 'space'
	DisplayName  string
	DisplaySlug  string // frozen at first sight; never re-derived
	HistoryState string // 'HISTORY_ON' | 'HISTORY_OFF' | ""
	FirstSeenAt  time.Time
	LastSyncedAt time.Time // zero if never synced
}

// UpsertSpace inserts or refreshes a space row for one account. space_type,
// display_slug and first_seen_at are FROZEN per (source, space): they are
// written only when the row is new, so neither a space rename nor a
// group-chat-to-space upgrade ever moves already-written day files (both are
// path components of the day-file stem). Everything else (display name,
// history state, last_synced_at) tracks the latest sighting.
func (d *DB) UpsertSpace(source, name, spaceType, displayName, displaySlug, historyState string) error {
	if source == "" || name == "" || spaceType == "" || displaySlug == "" {
		return fmt.Errorf("state: upsert space %s/%s: source, name, type and slug are required", source, name)
	}
	if err := requireInstance("upsert space "+name, source); err != nil {
		return err
	}
	now := fmtTime(time.Now())
	_, err := d.sql.Exec(`
		INSERT INTO chat_spaces
			(source, space_name, space_type, display_name, display_slug, history_state,
			 first_seen_at, last_synced_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(source, space_name) DO UPDATE SET
			display_name   = excluded.display_name,
			history_state  = excluded.history_state,
			last_synced_at = excluded.last_synced_at`,
		source, name, spaceType, nullStr(displayName), displaySlug, nullStr(historyState), now, now)
	if err != nil {
		return fmt.Errorf("state: upsert space %s/%s: %w", source, name, err)
	}
	return nil
}

// GetSpace returns one account's space row; ok is false when that account
// has never seen the space.
func (d *DB) GetSpace(source, name string) (sp Space, ok bool, err error) {
	var displayName, historyState, lastSynced sql.NullString
	var firstSeen string
	err = d.sql.QueryRow(`
		SELECT source, space_name, space_type, display_name, display_slug, history_state,
		       first_seen_at, last_synced_at
		FROM chat_spaces WHERE source = ? AND space_name = ?`, source, name).
		Scan(&sp.Source, &sp.Name, &sp.Type, &displayName, &sp.DisplaySlug, &historyState,
			&firstSeen, &lastSynced)
	if errors.Is(err, sql.ErrNoRows) {
		return Space{}, false, nil
	}
	if err != nil {
		return Space{}, false, fmt.Errorf("state: get space %s/%s: %w", source, name, err)
	}
	sp.DisplayName = displayName.String
	sp.HistoryState = historyState.String
	if sp.FirstSeenAt, err = parseTime(firstSeen); err != nil {
		return Space{}, false, err
	}
	if sp.LastSyncedAt, err = parseNullTime(lastSynced); err != nil {
		return Space{}, false, err
	}
	return sp, true, nil
}

// ListSpaces returns all spaces known to one account, ordered by resource
// name.
func (d *DB) ListSpaces(source string) ([]Space, error) {
	rows, err := d.sql.Query(`
		SELECT space_name FROM chat_spaces WHERE source = ? ORDER BY space_name`, source)
	if err != nil {
		return nil, fmt.Errorf("state: list spaces %s: %w", source, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("state: list spaces %s: %w", source, err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list spaces %s: %w", source, err)
	}
	out := make([]Space, 0, len(names))
	for _, n := range names {
		sp, ok, err := d.GetSpace(source, n)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, sp)
		}
	}
	return out, nil
}

// ChatMessage is one canonical chat message row; raw_json holds the full API
// Message and is the source of truth for rendering.
type ChatMessage struct {
	Source         string // instance id; filled on reads, page's value wins on writes
	Name           string // resource name 'spaces/A/messages/B' (dedup key within source)
	Space          string // filled on reads; on writes the page's space wins
	Thread         string // thread resource name; "" if none
	SenderID       string // 'users/123'
	CreateTime     time.Time
	LastUpdateTime time.Time // zero if never updated
	DayBucket      string    // "YYYY-MM-DD", computed by the caller in the pinned TZ
	RawJSON        string
	Edited         bool
	Deleted        bool
	DeletedAt      time.Time // zero unless deleted
}

// PendingAttachment is an out-of-band chat download registered together with
// its owning page (same transaction, so the advanced cursor can never
// outrun a lost attachment row).
type PendingAttachment struct {
	StableID string // owning chat message resource name
	PartKey  string
	RelPath  string
}

// ChatPage is one fetched page of a space's messages plus the cursor value
// that acknowledges it.
type ChatPage struct {
	Source      string // instance id of the fetching account, e.g. "gchat:work"
	Space       string
	Messages    []ChatMessage
	Attachments []PendingAttachment
	Cursor      string // new msg_create_time cursor; "" leaves it unchanged
}

// ApplyChatPage applies one page in a single transaction: upsert
// chat_messages rows (edits/deletes folded in sticky — a replayed stale page
// never un-edits or resurrects a message), register pending attachment rows,
// mark every affected (source, space, day) dirty in chat_day_files, and
// advance the space's msg_create_time cursor. A crash at any point leaves
// either the whole page applied or none of it.
//
// The space must have been registered with UpsertSpace for the same source
// first (its frozen slug determines the day files' deterministic rel paths).
func (d *DB) ApplyChatPage(ctx context.Context, page ChatPage) error {
	if err := requireInstance("apply chat page", page.Source); err != nil {
		return err
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: apply chat page: %w", err)
	}
	defer tx.Rollback()

	var spaceType, slug string
	err = tx.QueryRowContext(ctx, `
		SELECT space_type, display_slug FROM chat_spaces
		WHERE source = ? AND space_name = ?`,
		page.Source, page.Space).Scan(&spaceType, &slug)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("state: apply chat page: space %q not registered for %s (UpsertSpace first)", page.Space, page.Source)
	}
	if err != nil {
		return fmt.Errorf("state: apply chat page: load space %s/%s: %w", page.Source, page.Space, err)
	}

	days := make(map[string]bool)
	for _, m := range page.Messages {
		if m.Name == "" || m.DayBucket == "" || m.CreateTime.IsZero() {
			return fmt.Errorf("state: apply chat page: message %q: name, day bucket and create time are required", m.Name)
		}
		if m.Space != "" && m.Space != page.Space {
			return fmt.Errorf("state: apply chat page: message %q belongs to %q, page is for %q", m.Name, m.Space, page.Space)
		}
		if m.Source != "" && m.Source != page.Source {
			return fmt.Errorf("state: apply chat page: message %q was fetched by instance %q, page is for %q", m.Name, m.Source, page.Source)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_messages
				(source, msg_name, space_name, thread_name, sender_id, create_time,
				 last_update_time, day_bucket, raw_json, edited, deleted, deleted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(source, msg_name) DO UPDATE SET
				thread_name      = excluded.thread_name,
				sender_id        = excluded.sender_id,
				last_update_time = excluded.last_update_time,
				raw_json         = excluded.raw_json,
				edited           = MAX(chat_messages.edited, excluded.edited),
				deleted          = MAX(chat_messages.deleted, excluded.deleted),
				deleted_at       = COALESCE(excluded.deleted_at, chat_messages.deleted_at)`,
			page.Source, m.Name, page.Space, nullStr(m.Thread), m.SenderID, fmtTS(m.CreateTime),
			nullTS(m.LastUpdateTime), m.DayBucket, m.RawJSON,
			boolInt(m.Edited), boolInt(m.Deleted), nullTime(m.DeletedAt),
		); err != nil {
			return fmt.Errorf("state: apply chat page: upsert %q: %w", m.Name, err)
		}
		days[m.DayBucket] = true
	}

	dayList := make([]string, 0, len(days))
	for day := range days {
		dayList = append(dayList, day)
	}
	slices.Sort(dayList)
	for _, day := range dayList {
		relPath, err := chatDayRelPath(page.Source, day, spaceType, slug, page.Space)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, upsertDirtyDaySQL, page.Source, page.Space, day, relPath); err != nil {
			return fmt.Errorf("state: apply chat page: mark day %s dirty: %w", day, err)
		}
	}

	for _, a := range page.Attachments {
		if a.StableID == "" || a.PartKey == "" || a.RelPath == "" {
			return fmt.Errorf("state: apply chat page: attachment %s/%s: all fields are required", a.StableID, a.PartKey)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attachments (source, stable_id, part_key, rel_path, status)
			VALUES (?,?,?,?,'pending')
			ON CONFLICT(source, stable_id, part_key) DO NOTHING`,
			page.Source, a.StableID, a.PartKey, a.RelPath); err != nil {
			return fmt.Errorf("state: apply chat page: attachment %s/%s: %w", a.StableID, a.PartKey, err)
		}
	}

	if page.Cursor != "" {
		if _, err := tx.ExecContext(ctx, upsertCursorSQL,
			page.Source, page.Space, CursorMsgCreateTime, page.Cursor, fmtTime(time.Now())); err != nil {
			return fmt.Errorf("state: apply chat page: advance cursor: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: apply chat page: commit: %w", err)
	}
	return nil
}

// upsertDirtyDaySQL marks a day dirty and bumps its monotonic dirty_seq.
// Every dirtying write bumps the sequence so a render pass that snapshotted
// the rows before this write can never clear the flag (see MarkDayRendered).
const upsertDirtyDaySQL = `
	INSERT INTO chat_day_files (source, space_name, day_bucket, rel_path, dirty, dirty_seq)
	VALUES (?,?,?,?,1,1)
	ON CONFLICT(source, space_name, day_bucket) DO UPDATE SET
		dirty = 1, dirty_seq = chat_day_files.dirty_seq + 1`

// chatDayRelPath derives the deterministic day-file path from immutable
// inputs only (the account's file tag + day bucket + frozen slug + space
// resource name). The stem carries Tag(source), never the instance id: a
// colon must never reach a filename.
func chatDayRelPath(source, day, spaceType, slug, spaceName string) (string, error) {
	if err := requireInstance("chat day path", source); err != nil {
		return "", err
	}
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "", fmt.Errorf("state: bad day bucket %q: %w", day, err)
	}
	stem := naming.ChatStem(Tag(source), spaceType, slug, naming.Hash8(spaceName))
	return path.Join(naming.DayDir(t), stem+".md"), nil
}

// MessagesForDay returns every message of (source, space, day) ordered by
// create time (resource name breaks exact ties) — the projection input for
// rendering that day's file.
func (d *DB) MessagesForDay(source, space, day string) ([]ChatMessage, error) {
	rows, err := d.sql.Query(`
		SELECT msg_name, thread_name, sender_id, create_time, last_update_time,
		       day_bucket, raw_json, edited, deleted, deleted_at
		FROM chat_messages
		WHERE source = ? AND space_name = ? AND day_bucket = ?
		ORDER BY create_time, msg_name`, source, space, day)
	if err != nil {
		return nil, fmt.Errorf("state: messages for %s/%s/%s: %w", source, space, day, err)
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var thread, lastUpdate, deletedAt sql.NullString
		var create string
		var edited, deleted int
		if err := rows.Scan(&m.Name, &thread, &m.SenderID, &create, &lastUpdate,
			&m.DayBucket, &m.RawJSON, &edited, &deleted, &deletedAt); err != nil {
			return nil, fmt.Errorf("state: messages for %s/%s/%s: %w", source, space, day, err)
		}
		m.Source = source
		m.Space = space
		m.Thread = thread.String
		m.Edited = edited != 0
		m.Deleted = deleted != 0
		if m.CreateTime, err = parseTime(create); err != nil {
			return nil, err
		}
		if m.LastUpdateTime, err = parseNullTime(lastUpdate); err != nil {
			return nil, err
		}
		if m.DeletedAt, err = parseNullTime(deletedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: messages for %s/%s/%s: %w", source, space, day, err)
	}
	return out, nil
}

// ThreadFirstCreateTime returns the earliest create_time of any message in
// (source, space, threadName) across all day buckets — the instant the
// thread began, as this account saw it. Deleted tombstone rows count:
// deletion does not change when a thread started. ok is false when the
// thread has no rows at all.
func (d *DB) ThreadFirstCreateTime(source, space, threadName string) (t time.Time, ok bool, err error) {
	var min sql.NullString
	err = d.sql.QueryRow(`
		SELECT MIN(create_time) FROM chat_messages
		WHERE source = ? AND space_name = ? AND thread_name = ?`,
		source, space, threadName).Scan(&min)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("state: thread first create time %s/%s/%s: %w", source, space, threadName, err)
	}
	if !min.Valid {
		return time.Time{}, false, nil
	}
	if t, err = parseTime(min.String); err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// HasChatSpaces reports whether the given account has ever registered a
// chat space.
func (d *DB) HasChatSpaces(source string) (bool, error) {
	var n int
	if err := d.sql.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM chat_spaces WHERE source = ?)`, source).Scan(&n); err != nil {
		return false, fmt.Errorf("state: has chat spaces %s: %w", source, err)
	}
	return n != 0, nil
}

// DaysWithSender returns the distinct day buckets (ascending) that contain
// messages sent by userID in (source, space).
func (d *DB) DaysWithSender(source, space, userID string) ([]string, error) {
	rows, err := d.sql.Query(`
		SELECT DISTINCT day_bucket FROM chat_messages
		WHERE source = ? AND space_name = ? AND sender_id = ?
		ORDER BY day_bucket`, source, space, userID)
	if err != nil {
		return nil, fmt.Errorf("state: days with sender %s/%s/%s: %w", source, space, userID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var day string
		if err := rows.Scan(&day); err != nil {
			return nil, fmt.Errorf("state: days with sender %s/%s/%s: %w", source, space, userID, err)
		}
		out = append(out, day)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: days with sender %s/%s/%s: %w", source, space, userID, err)
	}
	return out, nil
}

// GetMember returns the cached display name for (source, space, userID); ok
// is false on a cache miss.
func (d *DB) GetMember(source, space, userID string) (name string, ok bool, err error) {
	var ns sql.NullString
	err = d.sql.QueryRow(`
		SELECT display_name FROM chat_members
		WHERE source = ? AND space_name = ? AND user_id = ?`,
		source, space, userID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("state: get member %s/%s/%s: %w", source, space, userID, err)
	}
	return ns.String, true, nil
}

// SetMember inserts or refreshes one member-cache entry.
func (d *DB) SetMember(source, space, userID, displayName string) error {
	_, err := d.sql.Exec(`
		INSERT INTO chat_members (source, space_name, user_id, display_name, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(source, space_name, user_id) DO UPDATE SET
			display_name = excluded.display_name,
			updated_at   = excluded.updated_at`,
		source, space, userID, nullStr(displayName), fmtTime(time.Now()))
	if err != nil {
		return fmt.Errorf("state: set member %s/%s/%s: %w", source, space, userID, err)
	}
	return nil
}

// Chat name sources distinguish automatic People API results from local
// overrides and negative cache entries. The canonical identity remains user_id.
const (
	chatNamePeople     = "people"
	chatNameOverride   = "override"
	chatNameUnresolved = "unresolved"
)

// ChatPeopleDue returns distinct sender ids whose People profile name has
// never been checked or is stale. Local overrides are never returned.
func (d *DB) ChatPeopleDue(source string, peopleBefore, unresolvedBefore time.Time) ([]string, error) {
	if err := requireInstance("list chat people due", source); err != nil {
		return nil, err
	}
	rows, err := d.sql.Query(`
		SELECT DISTINCT m.sender_id
		FROM chat_messages m
		LEFT JOIN chat_people p ON p.source = m.source AND p.user_id = m.sender_id
		WHERE m.source = ? AND m.sender_id <> '' AND m.sender_id <> 'users/app'
		  AND (p.user_id IS NULL
		       OR (p.name_source = 'people' AND p.updated_at < ?)
		       OR (p.name_source = 'unresolved' AND p.updated_at < ?))
		ORDER BY m.sender_id`, source, fmtTime(peopleBefore), fmtTime(unresolvedBefore))
	if err != nil {
		return nil, fmt.Errorf("state: list chat people due %s: %w", source, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("state: list chat people due %s: %w", source, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list chat people due %s: %w", source, err)
	}
	return out, nil
}

// GetChatPersonName returns an account-scoped enriched display name. ok is
// false for both an absent row and a cached unresolved profile.
func (d *DB) GetChatPersonName(source, userID string) (name string, ok bool, err error) {
	var ns sql.NullString
	err = d.sql.QueryRow(`
		SELECT display_name FROM chat_people WHERE source = ? AND user_id = ?`,
		source, userID).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("state: get chat person %s/%s: %w", source, userID, err)
	}
	return ns.String, ns.Valid && ns.String != "", nil
}

// SetChatPersonFromPeople records one People API result. An empty name is a
// retryable negative cache entry. A local override always wins. Any effective
// name change dirties every day on which this sender appears, atomically with
// the cache update.
func (d *DB) SetChatPersonFromPeople(source, userID, displayName string) (bool, error) {
	if err := requireInstance("set chat person", source); err != nil {
		return false, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("state: set chat person %s/%s: %w", source, userID, err)
	}
	defer tx.Rollback()

	var prev sql.NullString
	var prevSource string
	err = tx.QueryRow(`SELECT display_name, name_source FROM chat_people WHERE source = ? AND user_id = ?`, source, userID).Scan(&prev, &prevSource)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return false, fmt.Errorf("state: set chat person %s/%s: %w", source, userID, err)
	}
	if !missing && prevSource == chatNameOverride {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("state: set chat person %s/%s: commit: %w", source, userID, err)
		}
		return false, nil
	}
	nameSource := chatNamePeople
	if displayName == "" {
		nameSource = chatNameUnresolved
	}
	if _, err := tx.Exec(`
		INSERT INTO chat_people (source, user_id, display_name, name_source, updated_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(source, user_id) DO UPDATE SET
			display_name = excluded.display_name,
			name_source = excluded.name_source,
			updated_at = excluded.updated_at`,
		source, userID, nullStr(displayName), nameSource, fmtTime(time.Now())); err != nil {
		return false, fmt.Errorf("state: set chat person %s/%s: %w", source, userID, err)
	}
	changed := (!missing && prev.String != displayName) || (missing && displayName != "")
	if changed {
		if err := dirtySenderDaysTx(tx, source, userID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("state: set chat person %s/%s: commit: %w", source, userID, err)
	}
	return changed, nil
}

// SyncChatNameOverrides makes the configured override map authoritative for
// this account. Removing an override deletes it so the next People pass can
// resolve the id again. Changed effective names dirty all affected days.
func (d *DB) SyncChatNameOverrides(source string, overrides map[string]string) (int, error) {
	if err := requireInstance("sync chat name overrides", source); err != nil {
		return 0, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("state: sync chat name overrides %s: %w", source, err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT user_id, display_name FROM chat_people WHERE source = ? AND name_source = 'override'`, source)
	if err != nil {
		return 0, fmt.Errorf("state: sync chat name overrides %s: %w", source, err)
	}
	existing := make(map[string]string)
	for rows.Next() {
		var id string
		var name sql.NullString
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return 0, fmt.Errorf("state: sync chat name overrides %s: %w", source, err)
		}
		existing[id] = name.String
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("state: sync chat name overrides %s: %w", source, err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("state: sync chat name overrides %s: %w", source, err)
	}

	changed := 0
	for id, oldName := range existing {
		if _, keep := overrides[id]; keep {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM chat_people WHERE source = ? AND user_id = ? AND name_source = 'override'`, source, id); err != nil {
			return 0, fmt.Errorf("state: remove chat name override %s/%s: %w", source, id, err)
		}
		if oldName != "" {
			if err := dirtySenderDaysTx(tx, source, id); err != nil {
				return 0, err
			}
			changed++
		}
	}

	keys := make([]string, 0, len(overrides))
	for id := range overrides {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	for _, id := range keys {
		name := overrides[id]
		var prev sql.NullString
		err := tx.QueryRow(`SELECT display_name FROM chat_people WHERE source = ? AND user_id = ?`, source, id).Scan(&prev)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return 0, fmt.Errorf("state: read chat name override %s/%s: %w", source, id, err)
		}
		if _, err := tx.Exec(`
			INSERT INTO chat_people (source, user_id, display_name, name_source, updated_at)
			VALUES (?,?,?,'override',?)
			ON CONFLICT(source, user_id) DO UPDATE SET
				display_name = excluded.display_name,
				name_source = 'override',
				updated_at = excluded.updated_at`,
			source, id, name, fmtTime(time.Now())); err != nil {
			return 0, fmt.Errorf("state: set chat name override %s/%s: %w", source, id, err)
		}
		if missing || prev.String != name {
			if err := dirtySenderDaysTx(tx, source, id); err != nil {
				return 0, err
			}
			changed++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("state: sync chat name overrides %s: commit: %w", source, err)
	}
	return changed, nil
}

func dirtySenderDaysTx(tx *sql.Tx, source, userID string) error {
	_, err := tx.Exec(`
		UPDATE chat_day_files
		SET dirty = 1, dirty_seq = dirty_seq + 1
		WHERE source = ? AND EXISTS (
			SELECT 1 FROM chat_messages m
			WHERE m.source = chat_day_files.source
			  AND m.space_name = chat_day_files.space_name
			  AND m.day_bucket = chat_day_files.day_bucket
			  AND m.sender_id = ?
		)`, source, userID)
	if err != nil {
		return fmt.Errorf("state: dirty days for chat sender %s/%s: %w", source, userID, err)
	}
	return nil
}

// DayFile is one row of the chat day-file projection ledger.
type DayFile struct {
	Source      string // instance id that owns this projection
	Space       string
	DayBucket   string
	RelPath     string
	Dirty       bool
	DirtySeq    int64     // dirty_seq at read time; pass to MarkDayRendered
	ContentHash string    // "" until first rendered
	RenderedAt  time.Time // zero until first rendered
}

const dirtyDayFilesSQL = `
	SELECT source, space_name, day_bucket, rel_path, dirty_seq, content_hash, rendered_at
	FROM chat_day_files WHERE dirty = 1`

// DirtyDayFiles returns one account's day files needing (re-)rendering,
// ordered by space then day. Each row carries the dirty_seq it was read at;
// the renderer echoes it back to MarkDayRendered so a concurrent dirtying
// write (which bumps the sequence) is never clobbered.
func (d *DB) DirtyDayFiles(source string) ([]DayFile, error) {
	rows, err := d.sql.Query(dirtyDayFilesSQL+`
		AND source = ? ORDER BY space_name, day_bucket`, source)
	if err != nil {
		return nil, fmt.Errorf("state: dirty day files %s: %w", source, err)
	}
	defer rows.Close()
	return scanDayFiles(rows)
}

// AllDirtyDayFiles returns every account's dirty day files, ordered by
// source, space then day — the startup healing pass renders them all,
// including instances that are not part of the current sync.
func (d *DB) AllDirtyDayFiles() ([]DayFile, error) {
	rows, err := d.sql.Query(dirtyDayFilesSQL + ` ORDER BY source, space_name, day_bucket`)
	if err != nil {
		return nil, fmt.Errorf("state: dirty day files: %w", err)
	}
	defer rows.Close()
	return scanDayFiles(rows)
}

func scanDayFiles(rows *sql.Rows) ([]DayFile, error) {
	var out []DayFile
	for rows.Next() {
		f := DayFile{Dirty: true}
		var hash, rendered sql.NullString
		if err := rows.Scan(&f.Source, &f.Space, &f.DayBucket, &f.RelPath, &f.DirtySeq, &hash, &rendered); err != nil {
			return nil, fmt.Errorf("state: dirty day files: %w", err)
		}
		f.ContentHash = hash.String
		t, err := parseNullTime(rendered)
		if err != nil {
			return nil, err
		}
		f.RenderedAt = t
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: dirty day files: %w", err)
	}
	return out, nil
}

// MarkDayRendered records a completed render: the day is clean and its file
// content hash is remembered for idempotency checks. seqSeen must be the
// DirtySeq the renderer read the row at (via DirtyDayFiles); the clear is
// conditional on dirty_seq still equaling it, so a write that landed after
// the renderer's snapshot leaves the day dirty for the next pass instead of
// being silently dropped from the file. A stale seqSeen is not an error.
func (d *DB) MarkDayRendered(source, space, day string, seqSeen int64, contentHash string) error {
	_, err := d.sql.Exec(`
		UPDATE chat_day_files
		SET dirty = 0, content_hash = ?, rendered_at = ?
		WHERE source = ? AND space_name = ? AND day_bucket = ? AND dirty_seq = ?`,
		contentHash, fmtTime(time.Now()), source, space, day, seqSeen)
	if err != nil {
		return fmt.Errorf("state: mark day rendered %s/%s/%s: %w", source, space, day, err)
	}
	return nil
}

// MarkDayDirty forces a re-render of (source, space, day) — used when a late
// member cache fill or an edit event invalidates an already-rendered file.
// Creating the ledger row on demand requires the space to be registered for
// that account.
func (d *DB) MarkDayDirty(source, space, day string) error {
	res, err := d.sql.Exec(`
		UPDATE chat_day_files SET dirty = 1, dirty_seq = dirty_seq + 1
		WHERE source = ? AND space_name = ? AND day_bucket = ?`, source, space, day)
	if err != nil {
		return fmt.Errorf("state: mark day dirty %s/%s/%s: %w", source, space, day, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("state: mark day dirty %s/%s/%s: %w", source, space, day, err)
	} else if n > 0 {
		return nil
	}
	sp, ok, err := d.GetSpace(source, space)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("state: mark day dirty: space %q not registered for %s", space, source)
	}
	relPath, err := chatDayRelPath(source, day, sp.Type, sp.DisplaySlug, space)
	if err != nil {
		return err
	}
	if _, err := d.sql.Exec(upsertDirtyDaySQL, source, space, day, relPath); err != nil {
		return fmt.Errorf("state: mark day dirty %s/%s/%s: %w", source, space, day, err)
	}
	return nil
}
