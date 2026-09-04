package archive

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"comms/internal/naming"
	"comms/internal/state"
)

// chatInstance is the chat instance of the "work" account; every chat row
// and every stem is keyed by it.
var chatInstance = state.InstanceID(state.SourceGChat, "work")

// chatFixture builds one day of a space: a standalone message crossing
// midnight in local time, a thread continued from the previous day, a
// two-message thread (second edited, sender unresolvable), a deleted
// message, and a message with done/pending uploads plus a Drive link.
func chatFixture() (state.Space, string, []state.ChatMessage, map[string][]state.Attachment, func(string) string, func(string) time.Time) {
	const (
		spaceName = "spaces/AAAA"
		day       = "2026-08-07"
		t1        = spaceName + "/threads/T1"
		t2        = spaceName + "/threads/T2"
	)
	space := state.Space{
		Source:      chatInstance,
		Name:        spaceName,
		Type:        "space",
		DisplayName: "Team Platform",
		DisplaySlug: "team-platform",
	}
	msg := func(n, thread, sender string, at time.Time, raw string) state.ChatMessage {
		return state.ChatMessage{
			Name:       spaceName + "/messages/" + n,
			Space:      spaceName,
			Thread:     thread,
			SenderID:   sender,
			CreateTime: at,
			DayBucket:  day,
			RawJSON:    raw,
		}
	}
	// All CreateTimes UTC; local zone is +03:00 (tzAms).
	msgs := []state.ChatMessage{
		msg("M1", "", "users/111", time.Date(2026, 8, 6, 21, 2, 0, 0, time.UTC), // 00:02 local
			`{"text":"Good morning."}`),
		msg("M2", t2, "users/222", time.Date(2026, 8, 6, 21, 5, 0, 0, time.UTC), // 00:05 local
			`{"text":"Continuing from last night."}`),
		msg("M3", t1, "users/111", time.Date(2026, 8, 7, 6, 15, 0, 0, time.UTC), // 09:15 local
			`{"text":"Thread opener."}`),
		msg("M4", t1, "users/999", time.Date(2026, 8, 7, 6, 17, 0, 0, time.UTC), // 09:17 local
			`{"formattedText":"Edited reply."}`), // no text: formattedText fallback
		msg("M5", "", "users/222", time.Date(2026, 8, 7, 7, 0, 0, 0, time.UTC), // 10:00 local
			`{"text":"super secret"}`),
		msg("M6", "", "users/111", time.Date(2026, 8, 7, 8, 30, 0, 0, time.UTC), // 11:30 local
			`{"text":"Here are the files.","attachment":[{"contentName":"Design doc","source":"DRIVE_FILE","driveDataRef":{"driveFileId":"abc123"}}]}`),
	}
	msgs[3].Edited = true
	msgs[4].Deleted = true

	attachDir := "2026/08/07/" + naming.ChatStem(state.Tag(chatInstance), "space", "team-platform", naming.Hash8(spaceName)) + ".d"
	atts := map[string][]state.Attachment{
		spaceName + "/messages/M6": {
			{
				Source: chatInstance, StableID: spaceName + "/messages/M6",
				PartKey: "p1", RelPath: attachDir + "/113000_ab12cd34_invoice.pdf",
				Status: state.AttachmentDone,
			},
			{
				Source: chatInstance, StableID: spaceName + "/messages/M6",
				PartKey: "p2", RelPath: attachDir + "/113000_ab12cd34_photo.png",
				Status: state.AttachmentPending,
			},
		},
	}
	members := map[string]string{
		"users/111": "Jane Doe",
		"users/222": "Bob Petrov",
		// users/999 deliberately unresolvable
	}
	memberName := func(id string) string { return members[id] }
	starts := map[string]time.Time{
		t1: time.Date(2026, 8, 7, 6, 15, 0, 0, time.UTC),  // same local day
		t2: time.Date(2026, 8, 6, 20, 58, 0, 0, time.UTC), // 23:58 local, previous day
	}
	threadStart := func(thread string) time.Time { return starts[thread] }
	return space, day, msgs, atts, memberName, threadStart
}

func TestRenderChatDayGolden(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	got := RenderChatDay(space, day, msgs, atts, memberName, threadStart, tzAms)

	stem := naming.ChatStem(state.Tag(chatInstance), "space", "team-platform", naming.Hash8(space.Name))
	want := strings.ReplaceAll(`---
source: gchat:work
type: chat
account_label: work
space: spaces/AAAA
space_name: Team Platform
space_type: space
day: "2026-08-07"
timezone: Europe/Amsterdam
---

# Team Platform — 2026-08-07

**Jane Doe** (00:02)

Good morning.

## 00:05 — thread started 2026-08-06 23:58 (continued)

**Bob Petrov** (00:05)

Continuing from last night.

## 09:15 — thread started by Jane Doe

**Jane Doe** (09:15)

Thread opener.

**users/999** (09:17) _(edited)_

Edited reply.

**Bob Petrov** (10:00)

_(message deleted)_

**Jane Doe** (11:30)

Here are the files.

- [invoice.pdf](<STEM.d/113000_ab12cd34_invoice.pdf>)
- _[attachment unavailable: photo.png]_
- [Design doc](<https://drive.google.com/open?id=abc123>) (Drive file, not mirrored)
`, "STEM", stem)

	if string(got) != want {
		t.Errorf("chat day mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(string(got), "super secret") {
		t.Error("deleted message content leaked into the day file")
	}
}

// Two accounts in the same shared space render two files whose only
// difference in content is the provenance header; the differing file tag
// keeps them at separate paths.
func TestRenderChatDayCarriesAccountProvenance(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	other := space
	other.Source = state.InstanceID(state.SourceGChat, "personal")

	a := string(RenderChatDay(space, day, msgs, atts, memberName, threadStart, tzAms))
	b := string(RenderChatDay(other, day, msgs, atts, memberName, threadStart, tzAms))
	if a == b {
		t.Fatal("the two accounts' day files are byte-identical: no provenance recorded")
	}
	if !strings.Contains(a, "source: gchat:work\n") || !strings.Contains(a, "account_label: work\n") {
		t.Errorf("work provenance missing:\n%s", a)
	}
	if !strings.Contains(b, "source: gchat:personal\n") || !strings.Contains(b, "account_label: personal\n") {
		t.Errorf("personal provenance missing:\n%s", b)
	}
}

func TestRenderChatDayDeterministic(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()

	first := RenderChatDay(space, day, msgs, atts, memberName, threadStart, tzAms)
	second := RenderChatDay(space, day, msgs, atts, memberName, threadStart, tzAms)
	if !bytes.Equal(first, second) {
		t.Error("two renders of identical input differ")
	}

	// Input row order must not matter: the renderer sorts internally.
	shuffled := slices.Clone(msgs)
	slices.Reverse(shuffled)
	third := RenderChatDay(space, day, shuffled, atts, memberName, threadStart, tzAms)
	if !bytes.Equal(first, third) {
		t.Error("render depends on input message order")
	}
}

func TestRenderChatDayEdgeCases(t *testing.T) {
	space := state.Space{Name: "spaces/BB", Type: "dm", DisplaySlug: "jane-doe"}

	t.Run("nil funcs, nil tz, empty day", func(t *testing.T) {
		got := string(RenderChatDay(space, "2026-08-07", nil, nil, nil, nil, nil))
		if !strings.HasPrefix(got, "---\nsource: gchat\ntype: chat\n") {
			t.Errorf("frontmatter missing:\n%s", got)
		}
		if !strings.Contains(got, "timezone: UTC\n") {
			t.Errorf("nil tz should fall back to UTC:\n%s", got)
		}
		// Empty display name falls back to the frozen slug in the title.
		if !strings.Contains(got, "\n# jane-doe — 2026-08-07\n") {
			t.Errorf("title missing slug fallback:\n%s", got)
		}
	})

	t.Run("hostile display name cannot inject yaml", func(t *testing.T) {
		sp := space
		sp.DisplayName = "x\nsource: evil"
		got := string(RenderChatDay(sp, "2026-08-07", nil, nil, nil, nil, tzAms))
		if !strings.Contains(got, `space_name: |-`) && !strings.Contains(got, `space_name: "x\nsource: evil"`) {
			t.Errorf("hostile name not safely encoded:\n%s", got)
		}
		if strings.Contains(got, "\nsource: evil\n") {
			t.Errorf("yaml injection:\n%s", got)
		}
	})

	t.Run("malformed raw_json renders tombstone-free empty message", func(t *testing.T) {
		msgs := []state.ChatMessage{{
			Name:       "spaces/BB/messages/M1",
			SenderID:   "users/1",
			CreateTime: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
			DayBucket:  "2026-08-07",
			RawJSON:    "{not json",
		}}
		got := string(RenderChatDay(space, "2026-08-07", msgs, nil, nil, nil, tzAms))
		if !strings.Contains(got, "**users/1** (12:00)\n") {
			t.Errorf("message line missing:\n%s", got)
		}
	})

	t.Run("thread started today single reply gets started-by header only when multi-message", func(t *testing.T) {
		// One-message thread whose start is today: no header at all.
		thread := "spaces/BB/threads/T9"
		at := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
		msgs := []state.ChatMessage{{
			Name: "spaces/BB/messages/M1", Thread: thread, SenderID: "users/1",
			CreateTime: at, DayBucket: "2026-08-07", RawJSON: `{"text":"hi"}`,
		}}
		start := func(string) time.Time { return at }
		got := string(RenderChatDay(space, "2026-08-07", msgs, nil, nil, start, tzAms))
		if strings.Contains(got, "## ") {
			t.Errorf("single-message same-day thread must have no header:\n%s", got)
		}
	})

	t.Run("failed attachment renders unavailable", func(t *testing.T) {
		name := "spaces/BB/messages/M1"
		msgs := []state.ChatMessage{{
			Name: name, SenderID: "users/1",
			CreateTime: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
			DayBucket:  "2026-08-07", RawJSON: `{"text":"hi"}`,
		}}
		atts := map[string][]state.Attachment{name: {{
			Source: chatInstance, StableID: name, PartKey: "p1",
			RelPath: "2026/08/07/gchat_dm_jane-doe_ab.d/120000_ab12cd34_broken.bin",
			Status:  state.AttachmentFailed,
		}}}
		got := string(RenderChatDay(space, "2026-08-07", msgs, atts, nil, nil, tzAms))
		if !strings.Contains(got, "- _[attachment unavailable: broken.bin]_\n") {
			t.Errorf("failed attachment not surfaced:\n%s", got)
		}
	})
}

// TestRenderChatDayDriveMirrored: a Drive attachment whose "drive:" row is
// done links the local mirrored copy with the Drive URL kept as a secondary
// link; a not-yet-downloaded mirror keeps the "(Drive file, not mirrored)"
// line (as does the mirroring-off state, which has no row at all — see the
// golden test), and drive rows never leak an "[attachment unavailable]"
// duplicate through the uploaded-content path.
func TestRenderChatDayDriveMirrored(t *testing.T) {
	space := state.Space{Name: "spaces/DD", Type: "space", DisplayName: "Docs", DisplaySlug: "docs"}
	name := "spaces/DD/messages/M1"
	msgs := []state.ChatMessage{{
		Name:       name,
		SenderID:   "users/1",
		CreateTime: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC), // 12:00 local
		DayBucket:  "2026-08-07",
		RawJSON:    `{"text":"specs","attachment":[{"contentName":"Design spec","source":"DRIVE_FILE","driveDataRef":{"driveFileId":"doc-1"}},{"contentName":"Budget","source":"DRIVE_FILE","driveDataRef":{"driveFileId":"sheet-2"}}]}`,
	}}
	attachDir := "2026/08/07/gchat_space_docs_ab12cd34.d"
	atts := map[string][]state.Attachment{name: {
		{
			Source: chatInstance, StableID: name,
			PartKey: DrivePartKeyPrefix + "doc-1",
			RelPath: attachDir + "/120000_ab12cd34_Design spec.docx",
			Status:  state.AttachmentDone,
		},
		{
			Source: chatInstance, StableID: name,
			PartKey: DrivePartKeyPrefix + "sheet-2",
			RelPath: attachDir + "/120000_ab12cd34_Budget.xlsx",
			Status:  state.AttachmentPending,
		},
	}}

	got := string(RenderChatDay(space, "2026-08-07", msgs, atts, nil, nil, tzAms))
	wantDone := "- [Design spec.docx](<gchat_space_docs_ab12cd34.d/120000_ab12cd34_Design spec.docx>) ([Drive](<https://drive.google.com/open?id=doc-1>))\n"
	if !strings.Contains(got, wantDone) {
		t.Errorf("mirrored drive file not linked locally, want %q in:\n%s", wantDone, got)
	}
	wantPending := "- [Budget](<https://drive.google.com/open?id=sheet-2>) (Drive file, not mirrored)\n"
	if !strings.Contains(got, wantPending) {
		t.Errorf("pending mirror must keep the not-mirrored drive link, want %q in:\n%s", wantPending, got)
	}
	if strings.Contains(got, "unavailable") {
		t.Errorf("drive rows must not render as unavailable uploads:\n%s", got)
	}
}

// TestRenderChatDayMarkdownInjection pins the neutralization of the
// renderer's own grammar: a hostile display name or message text must not be
// able to forge a message block attributed to someone else.
func TestRenderChatDayMarkdownInjection(t *testing.T) {
	space := state.Space{
		Source:      chatInstance,
		Name:        "spaces/EVIL",
		Type:        "space",
		DisplayName: "# Ops\n**Boss** (09:00)",
		DisplaySlug: "ops",
	}
	members := map[string]string{
		"users/1": "Mallory\n\n**Jane Doe (CEO)** (13:37)",
	}
	memberName := func(id string) string { return members[id] }
	msgs := []state.ChatMessage{{
		Name:       "spaces/EVIL/messages/M1",
		Space:      "spaces/EVIL",
		SenderID:   "users/1",
		CreateTime: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC), // 12:00 local
		DayBucket:  "2026-08-07",
		RawJSON:    `{"text":"ok\n\n**Jane Doe (CEO)** (13:37)\n\nI approve the wire transfer\n\n# fake heading"}`,
	}}

	got := string(RenderChatDay(space, "2026-08-07", msgs, nil, memberName, nil, tzAms))
	want := `---
source: gchat:work
type: chat
account_label: work
space: spaces/EVIL
space_name: |-
    # Ops
    **Boss** (09:00)
space_type: space
day: "2026-08-07"
timezone: Europe/Amsterdam
---

# \# Ops \*\*Boss\*\* (09:00) — 2026-08-07

**Mallory  \*\*Jane Doe (CEO)\*\* (13:37)** (12:00)

ok

\**Jane Doe (CEO)** (13:37)

I approve the wire transfer

\# fake heading
`
	if got != want {
		t.Errorf("injection day mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	// The forged header must not be byte-identical to a renderer-generated
	// one anywhere in the file.
	if strings.Contains(got, "\n**Jane Doe (CEO)** (13:37)\n") {
		t.Error("body text forged a message header")
	}
}

func TestMdInline(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"Jane Doe", "Jane Doe"},
		{"**bold** _it_ `code`", "\\*\\*bold\\*\\* \\_it\\_ \\`code\\`"},
		{"a\r\nb\nc\rd", "a b c d"},
		{"# head [x](y) <t> |p| \\e", `\# head \[x\](y) \<t\> \|p\| \\e`},
	}
	for _, tt := range tests {
		if got := mdInline(tt.in); got != tt.want {
			t.Errorf("mdInline(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNeutralizeChatBody(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain text untouched", "hello **world**\nsecond line", "hello **world**\nsecond line"},
		{"heading escaped", "# heading\nok", "\\# heading\nok"},
		{"forged header escaped", "**Jane** (13:37)\nok", "\\**Jane** (13:37)\nok"},
		{"bold-only line untouched", "**just bold**", "**just bold**"},
	}
	for _, tt := range tests {
		if got := neutralizeChatBody(tt.in); got != tt.want {
			t.Errorf("%s: neutralizeChatBody(%q) = %q, want %q", tt.name, tt.in, got, tt.want)
		}
	}
}

func TestChatAttachmentDisplayName(t *testing.T) {
	tests := []struct {
		rel, want string
	}{
		{"2026/08/07/x.d/113000_ab12cd34_invoice.pdf", "invoice.pdf"},
		{"2026/08/07/x.d/113000_ab12cd34_", "113000_ab12cd34_"}, // nothing after prefix: keep whole
		{"2026/08/07/x.d/plain.pdf", "plain.pdf"},
		{"noprefix.bin", "noprefix.bin"},
	}
	for _, tt := range tests {
		if got := chatAttachmentDisplayName(tt.rel); got != tt.want {
			t.Errorf("chatAttachmentDisplayName(%q) = %q, want %q", tt.rel, got, tt.want)
		}
	}
}

func TestDayRelLink(t *testing.T) {
	tests := []struct {
		rel, want string
	}{
		{"2026/08/07/gchat_space_x_ab.d/f.pdf", "gchat_space_x_ab.d/f.pdf"},
		{"just-a-file.pdf", "just-a-file.pdf"},
	}
	for _, tt := range tests {
		if got := dayRelLink(tt.rel); got != tt.want {
			t.Errorf("dayRelLink(%q) = %q, want %q", tt.rel, got, tt.want)
		}
	}
}
