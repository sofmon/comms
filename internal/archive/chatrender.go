package archive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"save/internal/naming"
	"save/internal/state"
)

// chatFrontmatter is marshaled with yaml.v3 only — hostile space display
// names must never be able to inject YAML structure.
type chatFrontmatter struct {
	Source       string `yaml:"source"` // instance id, e.g. "gchat:work"
	Type         string `yaml:"type"`
	AccountLabel string `yaml:"account_label"` // the archiving account's label
	Space        string `yaml:"space"`         // resource name 'spaces/AAAA'
	SpaceName    string `yaml:"space_name"`
	SpaceType    string `yaml:"space_type"`
	Day          string `yaml:"day"`
	Timezone     string `yaml:"timezone"` // IANA name of the pinned archive zone
}

// rawChatMessage is the minimal decode of a chat_messages.raw_json row: the
// full API Message is canonical in the DB; rendering needs only the text and
// the Drive attachment references (uploaded attachments live in the
// attachments table instead).
type rawChatMessage struct {
	Text          string              `json:"text"`
	FormattedText string              `json:"formattedText"`
	Attachment    []rawChatAttachment `json:"attachment"`
}

type rawChatAttachment struct {
	ContentName  string `json:"contentName"`
	Source       string `json:"source"`
	DriveURL     string `json:"driveUrl"` // connector-provided convenience field, if present
	DriveDataRef struct {
		DriveFileID string `json:"driveFileId"`
	} `json:"driveDataRef"`
}

func (a rawChatAttachment) isDrive() bool {
	return a.DriveDataRef.DriveFileID != "" || a.DriveURL != "" || a.Source == "DRIVE_FILE"
}

func (a rawChatAttachment) url() string {
	if a.DriveURL != "" {
		return a.DriveURL
	}
	if a.DriveDataRef.DriveFileID != "" {
		return "https://drive.google.com/open?id=" + a.DriveDataRef.DriveFileID
	}
	return ""
}

// chatAttPrefix matches the "HHMMSS_msghash8_" prefix naming.ChatAttachmentName
// puts on chat attachment files; stripping it recovers the display name.
var chatAttPrefix = regexp.MustCompile(`^[0-9]{6}_[0-9a-f]{8}_`)

// DrivePartKeyPrefix namespaces attachments-table part keys that hold a Drive
// file id rather than a Chat media resource name. The gchat connector
// registers such rows when mirror_drive_files is on; the renderer matches
// them back to the message's Drive references by file id.
const DrivePartKeyPrefix = "drive:"

// ChatDay is everything RenderChatDayFor needs to project one space's
// conversation-day file.
type ChatDay struct {
	Space state.Space
	Day   string // "YYYY-MM-DD" in the pinned archive timezone

	// Messages are the day's rows in any order; they are sorted internally by
	// create time then resource name.
	Messages []state.ChatMessage

	// Attachments maps a message resource name to its attachments-table rows
	// (uploaded content only — Drive references render from raw_json).
	Attachments map[string][]state.Attachment

	// Skipped are the attachment policy's refusals for this day
	// (state.DB.SkippedForNote on the day file's path). Each row's StableID
	// is the owning message's resource name. Rows already resolved as
	// "fetched" are ignored: the real attachment row renders the link
	// instead. Everything else renders as a visible, honest non-entry —
	// a chat blob save decided not to download must not simply vanish.
	Skipped []state.SkippedAttachment

	// Disposition words those entries. Chat refuses before downloading, so
	// the gchat connector passes SkipBytesNotFetched.
	Disposition SkipDisposition

	// MemberName resolves 'users/{id}' to a display name, returning "" on a
	// cache miss (the opaque id is shown instead). May be nil.
	MemberName func(userID string) string

	// ThreadStart returns a thread's overall first-message create time (min
	// over all days), or the zero time when unknown; a thread whose start
	// precedes Day renders a "(continued)" header. May be nil.
	ThreadStart func(threadName string) time.Time

	// TZ is the pinned archive timezone; nil falls back to UTC.
	TZ *time.Location
}

// RenderChatDay renders one space's conversation-day file. It is a pure
// projection: identical inputs produce byte-identical output (the day file
// is regenerated whole and atomically replaced whenever any row changes).
//
// It is the no-skips form of RenderChatDayFor, kept because most callers have
// nothing to record; a connector that applies the attachment policy before
// downloading should use RenderChatDayFor so its refusals reach the note.
func RenderChatDay(space state.Space, day string, msgs []state.ChatMessage, atts map[string][]state.Attachment, memberName func(userID string) string, threadStart func(threadName string) time.Time, tz *time.Location) []byte {
	return RenderChatDayFor(ChatDay{
		Space:       space,
		Day:         day,
		Messages:    msgs,
		Attachments: atts,
		MemberName:  memberName,
		ThreadStart: threadStart,
		TZ:          tz,
	})
}

// RenderChatDayFor renders one space's conversation-day file, including any
// attachments the policy refused.
func RenderChatDayFor(d ChatDay) []byte {
	space, day, msgs, atts := d.Space, d.Day, d.Messages, d.Attachments
	memberName, threadStart := d.MemberName, d.ThreadStart

	tz := d.TZ
	if tz == nil {
		tz = time.UTC
	}

	// Refusals are grouped by their owning message and rendered with it, in
	// part-key order — the same lexicographic order the attachment rows use,
	// so "10" sorts before "2" consistently across both lists.
	skipped := make(map[string][]state.SkippedAttachment)
	for _, s := range d.Skipped {
		if s.Resolution == state.SkipResolutionFetched {
			continue // the bytes are on disk now; the attachment row links them
		}
		skipped[s.StableID] = append(skipped[s.StableID], s)
	}
	for _, rows := range skipped {
		slices.SortStableFunc(rows, func(a, b state.SkippedAttachment) int {
			return strings.Compare(a.PartKey, b.PartKey)
		})
	}

	ms := slices.Clone(msgs)
	slices.SortStableFunc(ms, func(a, b state.ChatMessage) int {
		if c := a.CreateTime.Compare(b.CreateTime); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	// Group by thread, groups ordered by their first message of the day.
	// A message without a thread forms its own single-message group.
	type group struct {
		thread string
		msgs   []state.ChatMessage
	}
	var groups []*group
	byThread := make(map[string]*group)
	for _, m := range ms {
		if m.Thread == "" {
			groups = append(groups, &group{msgs: []state.ChatMessage{m}})
			continue
		}
		g, ok := byThread[m.Thread]
		if !ok {
			g = &group{thread: m.Thread}
			byThread[m.Thread] = g
			groups = append(groups, g)
		}
		g.msgs = append(g.msgs, m)
	}

	// Resolved names are interpolated into the renderer's own grammar
	// (message headers, thread headers), so they are Markdown-neutralized:
	// a hostile display name must not be able to forge a message block.
	resolve := func(userID string) string {
		if memberName != nil {
			if n := memberName(userID); n != "" {
				return mdInline(n)
			}
		}
		return mdInline(userID) // unresolvable: show the opaque 'users/{id}'
	}

	var buf bytes.Buffer
	writeChatFrontmatter(&buf, space, day, tz)

	title := space.DisplayName
	if title == "" {
		title = space.DisplaySlug
	}
	fmt.Fprintf(&buf, "\n# %s — %s\n", mdInline(title), day)

	for _, g := range groups {
		first := g.msgs[0]
		localFirst := first.CreateTime.In(tz)

		header := ""
		if g.thread != "" && threadStart != nil {
			if ts := threadStart(g.thread); !ts.IsZero() {
				if orig := ts.In(tz); naming.DayBucket(orig) < day {
					// Thread began on an earlier day; today opens mid-thread.
					header = fmt.Sprintf("## %s — thread started %s (continued)",
						localFirst.Format("15:04"), orig.Format("2006-01-02 15:04"))
				}
			}
		}
		if header == "" && len(g.msgs) > 1 {
			header = fmt.Sprintf("## %s — thread started by %s",
				localFirst.Format("15:04"), resolve(first.SenderID))
		}
		// Single-message threads that are not continuations get no header:
		// the message line below already carries the time.
		if header != "" {
			buf.WriteString("\n")
			buf.WriteString(header)
			buf.WriteString("\n")
		}
		for _, m := range g.msgs {
			renderChatMessage(&buf, m, atts[m.Name], skipped[m.Name], d.Disposition, resolve, tz)
		}
	}
	return buf.Bytes()
}

func writeChatFrontmatter(buf *bytes.Buffer, space state.Space, day string, tz *time.Location) {
	// Provenance comes from the space's instance id: which account archived
	// this copy. Two accounts in one shared space write two files whose
	// contents would otherwise be indistinguishable.
	source, label := space.Source, ""
	if source == "" {
		source = state.SourceGChat
	} else if _, l, ok := state.SplitInstance(source); ok {
		label = l
	}
	fm := chatFrontmatter{
		Source:       validStr(source),
		Type:         "chat",
		AccountLabel: validStr(label),
		Space:        validStr(space.Name),
		SpaceName:    validStr(space.DisplayName),
		SpaceType:    validStr(space.Type),
		Day:          day,
		Timezone:     tz.String(),
	}
	// All fields are valid-UTF-8 strings, so Marshal cannot fail; the guard
	// keeps a hypothetical failure from producing an unfenced file.
	if err := writeFrontmatter(buf, fm); err != nil {
		buf.Reset()
		buf.WriteString("---\nsource: gchat\ntype: chat\n---\n")
	}
}

func renderChatMessage(buf *bytes.Buffer, m state.ChatMessage, rows []state.Attachment, skips []state.SkippedAttachment, disp SkipDisposition, resolve func(string) string, tz *time.Location) {
	fmt.Fprintf(buf, "\n**%s** (%s)", resolve(m.SenderID), m.CreateTime.In(tz).Format("15:04"))
	if m.Edited && !m.Deleted {
		buf.WriteString(" _(edited)_")
	}
	buf.WriteString("\n")

	if m.Deleted {
		buf.WriteString("\n_(message deleted)_\n")
		// A deleted message can still own refusals — the attachment policy
		// declined the blob before the sender retracted the message — and
		// they stay visible, because the record of what was not kept
		// outlives the message body.
		writeChatSkips(buf, skips, disp)
		return
	}

	// Minimal decode; a malformed row renders as an empty message rather
	// than failing the whole day.
	var raw rawChatMessage
	_ = json.Unmarshal([]byte(m.RawJSON), &raw)

	text := strings.TrimRight(raw.Text, "\n")
	if text == "" {
		text = strings.TrimRight(raw.FormattedText, "\n")
	}
	if text != "" {
		buf.WriteString("\n")
		buf.WriteString(neutralizeChatBody(text))
		buf.WriteString("\n")
	}

	var lines []string
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b state.Attachment) int {
		return strings.Compare(a.PartKey, b.PartKey)
	})
	// Drive mirror rows are keyed by file id and rendered with the message's
	// Drive references below; only uploaded content renders from rows here.
	driveRows := make(map[string]state.Attachment)
	for _, a := range rows {
		if id, ok := strings.CutPrefix(a.PartKey, DrivePartKeyPrefix); ok {
			driveRows[id] = a
			continue
		}
		name := chatAttachmentDisplayName(a.RelPath)
		if a.Status == state.AttachmentDone {
			lines = append(lines, fmt.Sprintf("- [%s](%s)", mdLinkText(name), mdLinkDest(dayRelLink(a.RelPath))))
		} else { // pending or failed: visible, never silently dropped
			lines = append(lines, fmt.Sprintf("- _[attachment unavailable: %s]_", mdLinkText(name)))
		}
	}
	for _, a := range raw.Attachment {
		if !a.isDrive() {
			continue // uploaded content is covered by the attachments table
		}
		if row, ok := driveRows[a.DriveDataRef.DriveFileID]; ok && row.Status == state.AttachmentDone {
			// Mirrored copy on disk: link it, keep the Drive URL secondary.
			line := fmt.Sprintf("- [%s](%s)", mdLinkText(chatAttachmentDisplayName(row.RelPath)), mdLinkDest(dayRelLink(row.RelPath)))
			if url := a.url(); url != "" {
				line += fmt.Sprintf(" ([Drive](%s))", mdLinkDest(url))
			}
			lines = append(lines, line)
			continue
		}
		name := validStr(a.ContentName)
		if name == "" {
			name = "Drive file"
		}
		if url := a.url(); url != "" {
			lines = append(lines, fmt.Sprintf("- [%s](%s) (Drive file, not mirrored)", mdLinkText(name), mdLinkDest(url)))
		} else {
			lines = append(lines, fmt.Sprintf("- %s (Drive file, not mirrored)", mdLinkText(name)))
		}
	}
	if len(lines) > 0 {
		buf.WriteString("\n")
		for _, l := range lines {
			buf.WriteString(l)
			buf.WriteString("\n")
		}
	}
	writeChatSkips(buf, skips, disp)
}

// writeChatSkips renders the attachments the policy refused for one message.
// Chat day files have no frontmatter list to hide behind — they are pure
// projections — so the visible entry is the whole record, and it carries the
// same facts the email note's entries do: the name, the size, what the sender
// declared, why it was refused, and how to get it back.
func writeChatSkips(buf *bytes.Buffer, skips []state.SkippedAttachment, disp SkipDisposition) {
	if len(skips) == 0 {
		return
	}
	buf.WriteString("\n")
	for _, s := range skips {
		name := s.OrigName
		if oneLine(name) == "" {
			name = s.SanitizedName
		}
		fmt.Fprintf(buf, "- **Not stored — %s** — %s", mdCode(name), sizeText(s.SizeBytes))
		if oneLine(s.DeclaredType) != "" {
			fmt.Fprintf(buf, ", declared %s", mdCode(s.DeclaredType))
		}
		if s.SniffedType != "" {
			fmt.Fprintf(buf, ", detected %s", mdCode(s.SniffedType))
		}
		fmt.Fprintf(buf, "\n  - Reason: %s (%s)\n", reasonEnglish(s.Reason), mdCode(s.Reason))
		switch s.Resolution {
		case state.SkipResolutionSourceGone:
			buf.WriteString("  - The message or the attachment no longer exists upstream, so this cannot be recovered.\n")
		case state.SkipResolutionStillDenied:
			buf.WriteString("  - Re-checked under the current policy and still refused.\n")
		default:
			fmt.Fprintf(buf, "  - %s %s\n", disp.sentence(), recoveryHint(s.Reason))
		}
	}
}

// chatAttachmentDisplayName recovers the human name from an attachment rel
// path by stripping the deterministic "HHMMSS_msghash8_" prefix.
func chatAttachmentDisplayName(rel string) string {
	base := path.Base(rel)
	if m := chatAttPrefix.FindString(base); m != "" && len(base) > len(m) {
		return base[len(m):]
	}
	return base
}

// mdInline neutralizes a string interpolated into the renderer's own
// Markdown grammar (sender names, the space title): invalid UTF-8 is
// repaired, newlines collapse to single spaces, and structural punctuation
// is backslash-escaped, so a hostile display name can neither fake a header
// nor break out of its line.
func mdInline(s string) string {
	s = validStr(s)
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '*', '_', '`', '#', '[', ']', '<', '>', '|':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// chatHeaderLine matches a body line that would parse exactly like the
// renderer's own "**name** (HH:MM)" message header.
var chatHeaderLine = regexp.MustCompile(`^\*\*.*\*\* \([0-9]{2}:[0-9]{2}\)`)

// neutralizeChatBody breaks message-block forgery in body text while keeping
// what the user actually typed byte-preserved: only lines that would parse
// as one of the renderer's own structural forms — a heading, or a message
// header — get their leading character backslash-escaped.
func neutralizeChatBody(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "#") || chatHeaderLine.MatchString(l) {
			lines[i] = "\\" + l
		}
	}
	return strings.Join(lines, "\n")
}

// dayRelLink converts an archive-root-relative attachment path
// ("YYYY/MM/DD/<stem>.d/<name>") to a path relative to the day file's own
// directory ("<stem>.d/<name>").
func dayRelLink(rel string) string {
	dir := path.Base(path.Dir(rel))
	if dir == "." || dir == "/" {
		return path.Base(rel)
	}
	return dir + "/" + path.Base(rel)
}
