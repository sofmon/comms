package gchat

// mapping.go holds the pure functions that turn API responses into state
// rows, deterministic archive paths, filters and cursors. Everything here is
// side-effect free; tests pin the exact behavior.

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	chat "google.golang.org/api/chat/v1"

	"comms/internal/archive"
	"comms/internal/naming"
	"comms/internal/policy"
	"comms/internal/state"
)

const (
	// cursorLayout renders cursor timestamps with fixed-width nanoseconds
	// (matching the state store's server-timestamp format); parseCursor
	// accepts any RFC 3339 fraction width.
	cursorLayout = "2006-01-02T15:04:05.000000000Z07:00"

	// rewind is subtracted from the message cursor when building the
	// createTime list filter. The documented filter grammar offers only a
	// strict '>' operator, so a second message sharing the cursor's exact
	// createTime would otherwise be skipped forever; the 1-second overlap
	// refetches the boundary and the msg_name primary key absorbs the
	// duplicates. This rewind is mandatory — see TestListFilterRewind.
	rewind = time.Second

	// eventsWindow is how far back spaceEvents.list reaches; requesting an
	// older start_time is rejected, so starts are clamped (with a small
	// margin against clock skew between us and the server).
	eventsWindow      = 28 * 24 * time.Hour
	eventsClampMargin = time.Minute

	// eventsRewind is subtracted from the events cursor when building the
	// spaceEvents start_time. The overlap re-observes events that were still
	// committing server-side while the previous pass listed (or that clock
	// skew hid); ApplyChatPage's sticky upserts absorb the replays.
	eventsRewind = 5 * time.Minute
)

// itemError is a per-item (single message) processing failure, routed to the
// failures ledger so a poison item never wedges the cursor.
type itemError struct {
	ID  string
	Err error
}

// spaceMeta is the identity a space's archive paths derive from: the frozen
// display slug and space type as stored in chat_spaces, never the values
// fresh from the API (a rename must not move files).
type spaceMeta struct {
	Name string // resource name "spaces/AAAA"
	Type string // "space" | "group" | "dm"
	Slug string // frozen display slug
}

// spaceTypeOf maps the API SpaceType enum onto the archive's short names.
func spaceTypeOf(apiType string) string {
	switch apiType {
	case "GROUP_CHAT":
		return "group"
	case "DIRECT_MESSAGE":
		return "dm"
	default: // "SPACE" and any future value
		return "space"
	}
}

// slugFallback is the slug used when a display name yields nothing (DMs have
// no display name at all under user auth).
func slugFallback(spaceType string) string {
	switch spaceType {
	case "dm":
		return "dm"
	case "group":
		return "group"
	default:
		return "untitled"
	}
}

// listFilter builds the messages.list createTime filter for a stored cursor,
// rewound by 1s (see rewind). A zero cursor means full history: no filter.
func listFilter(cursor time.Time) string {
	if cursor.IsZero() {
		return ""
	}
	return fmt.Sprintf("createTime > %q", cursor.Add(-rewind).UTC().Format(time.RFC3339Nano))
}

func formatCursor(t time.Time) string {
	return t.UTC().Format(cursorLayout)
}

func parseCursor(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

// clampEventsStart bounds an events start time to the API's 28-day window.
// gap is true when a stored cursor is older than the window — edits and
// deletions between the cursor and the clamped start were never observed.
func clampEventsStart(cursor, now time.Time) (start time.Time, gap bool) {
	oldest := now.Add(-eventsWindow + eventsClampMargin)
	if cursor.IsZero() {
		return oldest, false
	}
	if cursor.Before(oldest) {
		return oldest, true
	}
	return cursor, false
}

// eventsListStart returns the spaceEvents.list start_time for a stored
// cursor: the cursor rewound by eventsRewind (see eventsRewind), clamped to
// the 28-day window. gap reports whether the raw cursor itself predates the
// window — events between it and the clamped start were genuinely never
// observed; the rewind alone never raises it, since the overlap it clips was
// already listed by an earlier pass.
func eventsListStart(cursor, now time.Time) (start time.Time, gap bool) {
	_, gap = clampEventsStart(cursor, now)
	from := cursor
	if !from.IsZero() {
		from = from.Add(-eventsRewind)
	}
	start, _ = clampEventsStart(from, now)
	return start, gap
}

// eventsFilter builds the spaceEvents.list filter: message updates and
// deletions after start (start_time is exclusive). The server automatically
// includes the corresponding batch event types.
func eventsFilter(start time.Time) string {
	return fmt.Sprintf(`start_time = "%s" AND (event_types:"google.workspace.chat.message.v1.updated" OR event_types:"google.workspace.chat.message.v1.deleted")`,
		start.UTC().Format(time.RFC3339))
}

// spaceOfMessage extracts "spaces/X" from "spaces/X/messages/Y".
func spaceOfMessage(msgName string) string {
	parts := strings.SplitN(msgName, "/", 3)
	if len(parts) < 3 {
		return msgName
	}
	return parts[0] + "/" + parts[1]
}

// dayFromRelPath recovers the day bucket ("2026-08-07") from an archive rel
// path ("2026/08/07/<stem>.d/<name>").
func dayFromRelPath(rel string) (string, bool) {
	parts := strings.SplitN(rel, "/", 4)
	if len(parts) < 4 {
		return "", false
	}
	day := parts[0] + "-" + parts[1] + "-" + parts[2]
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", false
	}
	return day, true
}

// dayNoteRel is the deterministic rel path of a space's conversation-day
// file: the note every skip of that day must point at.
//
// It mirrors state's own (unexported) chatDayRelPath exactly — same stem, same
// day directory — because a skip whose note_rel_path does not match the file
// the renderer writes is a skip nobody can find. TestDayNoteRelMatchesState
// pins the two together so they cannot drift apart.
func dayNoteRel(tag, spaceType, slug, spaceName string, local time.Time) string {
	stem := naming.ChatStem(tag, spaceType, slug, naming.Hash8(spaceName))
	return path.Join(naming.DayDir(local), stem+".md")
}

// buildPage maps one page of API messages onto a state.ChatPage for the
// account identified by src (its instance id, "gchat:<label>"): src keys
// every row the page commits, and state.Tag(src) is the source component of
// the day-file stem the attachment paths hang off. prevCursor
// is the cursor value already durable; the page's cursor is set only when
// this page moves it forward (a refetched overlap page must never regress
// it). maxCreate is the running high-water mark for the caller to thread
// into the next page. mirrorDrive selects whether DRIVE_FILE attachments get
// pending download rows (the mirror_drive_files option).
//
// A message whose buildRow fails is handled under the poison protocol: if
// isSkipped reports it past the failure threshold it is passed over for good
// (later messages advance the cursor beyond it); otherwise it is reported as
// an itemError and mapping STOPS — no later message is emitted, so the
// cursor freezes at the last good message before the failure and the next
// run's rewound filter re-lists it (retry until skip-listed, never silent
// loss). A nil isSkipped selects the events-refetch behavior: no cursor is
// at stake there, so failures are reported and mapping continues.
// Refusals decided here are returned separately as skipped: they are NOT part
// of the page (state.ChatPage has no slot for them) and the caller must make
// them durable BEFORE committing the page, because the page commit advances
// the cursor past these messages for good.
func buildPage(src string, sp spaceMeta, msgs []*chat.Message, tz *time.Location, prevCursor time.Time, mirrorDrive bool, pol *policy.Policy, isSkipped func(id string) bool) (page state.ChatPage, skipped []state.SkippedAttachment, maxCreate time.Time, errs []itemError, stopped bool) {
	page.Source = src
	page.Space = sp.Name
	maxCreate = prevCursor
	// The stem carries the FILE TAG, never the instance id: a colon must
	// never reach a filename. The space hash is unchanged — two accounts
	// sharing a space are already separated by their differing tags.
	tag := state.Tag(src)
	stem := naming.ChatStem(tag, sp.Type, sp.Slug, naming.Hash8(sp.Name))
	for _, m := range msgs {
		if m == nil || m.Name == "" {
			continue
		}
		row, plan, err := buildRow(src, tag, stem, sp, m, tz, mirrorDrive, pol)
		if err != nil {
			if isSkipped == nil {
				errs = append(errs, itemError{ID: m.Name, Err: err})
				continue
			}
			if isSkipped(m.Name) {
				continue // past the threshold: passed over for good
			}
			errs = append(errs, itemError{ID: m.Name, Err: err})
			stopped = true
			break
		}
		page.Messages = append(page.Messages, row)
		page.Attachments = append(page.Attachments, plan.Pending...)
		skipped = append(skipped, plan.Skipped...)
		if row.CreateTime.After(maxCreate) {
			maxCreate = row.CreateTime
		}
	}
	if maxCreate.After(prevCursor) {
		page.Cursor = formatCursor(maxCreate)
	}
	return page, skipped, maxCreate, errs, stopped
}

// buildRow maps one API message onto its canonical row plus the attachment
// plan (pending downloads, and the refusals the policy decided without
// downloading anything). raw_json is the full API message — the source of
// truth the renderer reads.
func buildRow(src, tag, stem string, sp spaceMeta, m *chat.Message, tz *time.Location, mirrorDrive bool, pol *policy.Policy) (state.ChatMessage, attachPlan, error) {
	create, err := time.Parse(time.RFC3339, m.CreateTime)
	if err != nil {
		return state.ChatMessage{}, attachPlan{}, fmt.Errorf("bad createTime %q: %w", m.CreateTime, err)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return state.ChatMessage{}, attachPlan{}, fmt.Errorf("marshal message: %w", err)
	}
	local := create.In(tz)
	row := state.ChatMessage{
		Name:       m.Name,
		Thread:     threadName(m),
		SenderID:   senderID(m),
		CreateTime: create,
		DayBucket:  naming.DayBucket(local),
		RawJSON:    string(raw),
		Edited:     m.LastUpdateTime != "",
		Deleted:    m.DeleteTime != "" || m.DeletionMetadata != nil,
	}
	if m.LastUpdateTime != "" {
		if t, terr := time.Parse(time.RFC3339, m.LastUpdateTime); terr == nil {
			row.LastUpdateTime = t
		}
	}
	if m.DeleteTime != "" {
		if t, terr := time.Parse(time.RFC3339, m.DeleteTime); terr == nil {
			row.DeletedAt = t
		}
	}
	return row, pendingAttachments(src, tag, stem, sp, m, local, mirrorDrive, pol), nil
}

// attachPlan is the outcome of putting one message's attachments to the
// policy before anything is downloaded: the blobs to fetch, and the blobs
// that will never be fetched (recorded, never dropped).
type attachPlan struct {
	Pending []state.PendingAttachment
	Skipped []state.SkippedAttachment
}

// pendingAttachments returns the download rows for a message's attachments at
// their deterministic archive paths:
//
//	<dayDir>/<chatStem>.d/<HHMMSS>_<msgHash8>_<sanitized-name>
//
// Uploaded content always gets a row. Drive files get one only when
// mirrorDrive is on, part-keyed "drive:<fileId>" (otherwise the renderer
// links them from raw_json); Google-native types carry the extension of the
// Office format they will be exported as, chosen from the message's
// contentType so the path stays deterministic. Duplicate content names within
// one message are disambiguated by naming.Unique in attachment order, which
// is deterministic across refetches.
//
// # The pre-download policy pass
//
// Chat is the ONE source where the attachment policy can pay for itself
// twice: the blob lives behind a separate media.download call, so a refusal
// decided here means the bytes are never transferred at all. Every candidate
// is therefore put to policy.PreCheck BEFORE its pending row is created; a
// refused one produces a state.SkippedAttachment instead, and the day file
// shows it.
//
// Two deliberate limitations, both recorded honestly in the row:
//
//   - No sniffed type. PreCheck never looks at content, and there is no
//     content to look at — that is the whole point. sniffed_type is stored
//     empty, which state.SkippedAttachment documents as meaning exactly
//     "the bytes were never fetched", and the day file says "never fetched"
//     rather than claiming they were downloaded and discarded.
//   - No declared size. chat.Attachment carries contentName and contentType
//     but NO size field (verified against chat/v1), so the size caps cannot
//     bind here and Size is left 0. attachments.max_size is enforced after
//     the download, in downloadOne, where the real byte count exists — which
//     is also where the content half of the allowlist is applied, because
//     PreCheck passing is explicitly not an authorization to store.
//
// The sender's declared contentType is recorded but never decided on: it is
// an attacker-controlled header, and the frozen rule is extension AND sniffed
// content, both of which are checked where they can be.
//
// A skip's SanitizedName and the pending row's basename are the same string,
// and the collision table is advanced for refused candidates too, so a later
// `comms refetch` under a widened policy writes exactly the name recorded here
// and cannot renumber the attachments around it.
func pendingAttachments(src, tag, stem string, sp spaceMeta, m *chat.Message, local time.Time, mirrorDrive bool, pol *policy.Policy) attachPlan {
	if len(m.Attachment) == 0 {
		return attachPlan{}
	}
	if pol == nil {
		pol = policy.Default()
	}
	dayDir := naming.DayDir(local)
	attachDir := naming.AttachDir(stem)
	msgHash := naming.Hash8(m.Name)
	taken := make(map[string]bool)
	var plan attachPlan
	for i, a := range m.Attachment {
		if a == nil {
			continue
		}
		var partKey, name string
		switch {
		case a.Source == "UPLOADED_CONTENT":
			if a.AttachmentDataRef == nil || a.AttachmentDataRef.ResourceName == "" {
				continue // nothing to download by
			}
			partKey = a.AttachmentDataRef.ResourceName
			name = naming.SanitizeFilename(a.ContentName, fmt.Sprintf("attachment-%d", i+1))
		case mirrorDrive && a.Source == "DRIVE_FILE":
			if a.DriveDataRef == nil || a.DriveDataRef.DriveFileId == "" {
				continue // no file id to fetch by; raw_json keeps the reference
			}
			partKey = archive.DrivePartKeyPrefix + a.DriveDataRef.DriveFileId
			name = naming.SanitizeFilename(a.ContentName, fmt.Sprintf("attachment-%d", i+1))
			if _, ext, ok := driveExport(a.ContentType); ok && !strings.HasSuffix(strings.ToLower(name), ext) {
				name += ext
			}
		default:
			continue
		}
		name = naming.Unique(taken, name)
		// The policy keys on the FINAL on-disk basename, prefix and all: that
		// is the string naming produced (its iCloud sync-exclusion guard can
		// change the extension), and the extension it decides on must be the
		// one that lands in the vault.
		base := naming.ChatAttachmentName(local, msgHash, name)
		if v := pol.PreCheck(policy.Input{Name: base, Chat: true}); !v.Store {
			plan.Skipped = append(plan.Skipped, state.SkippedAttachment{
				Source:        src,
				StableID:      m.Name,
				PartKey:       partKey,
				OrigName:      a.ContentName,
				SanitizedName: base,
				SizeBytes:     0, // Chat does not publish a size before download
				DeclaredType:  a.ContentType,
				DeclaredExt:   policy.NormalizeExt(base),
				SniffedType:   "", // never fetched, so never sniffed
				Reason:        v.Reason,
				PolicyDigest:  pol.PolicyDigest(),
				NoteRelPath:   dayNoteRel(tag, sp.Type, sp.Slug, sp.Name, local),
				DayBucket:     naming.DayBucket(local),
			})
			continue
		}
		plan.Pending = append(plan.Pending, state.PendingAttachment{
			StableID: m.Name,
			PartKey:  partKey,
			RelPath:  path.Join(dayDir, attachDir, base),
		})
	}
	return plan
}

// driveExport maps a Google-native MIME type onto the Office format the Drive
// API exports it as (Docs→docx, Sheets→xlsx, Slides→pptx). ok is false for
// every other type — those download byte-identically via alt=media.
func driveExport(mimeType string) (exportMime, ext string, ok bool) {
	switch mimeType {
	case "application/vnd.google-apps.document":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx", true
	case "application/vnd.google-apps.spreadsheet":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx", true
	case "application/vnd.google-apps.presentation":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation", ".pptx", true
	}
	return "", "", false
}

func threadName(m *chat.Message) string {
	if m.Thread == nil {
		return ""
	}
	return m.Thread.Name
}

func senderID(m *chat.Message) string {
	if m.Sender == nil {
		return ""
	}
	return m.Sender.Name
}
