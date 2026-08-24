package archive

import (
	"strings"
	"testing"

	"save/internal/policy"
	"save/internal/state"
)

func chatSkip(msgName, partKey, name, reason string) state.SkippedAttachment {
	return state.SkippedAttachment{
		Source: chatInstance, StableID: msgName, PartKey: partKey,
		OrigName: name, SanitizedName: name, SizeBytes: 209715200,
		DeclaredType: "video/quicktime", Reason: reason,
		PolicyDigest: "fb70e01d2e46670e", NoteRelPath: "2026/08/07/x.md",
		DayBucket: "2026-08-07",
	}
}

// TestRenderChatDayShowsSkips: a chat blob the policy refused before
// downloading leaves no attachments-table row, so without an explicit entry
// it would vanish from the day file entirely — a silent drop. Chat day files
// have no frontmatter list, so the visible entry is the whole record.
func TestRenderChatDayShowsSkips(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	const m6 = "spaces/AAAA/messages/M6"

	got := string(RenderChatDayFor(ChatDay{
		Space: space, Day: day, Messages: msgs, Attachments: atts,
		Skipped: []state.SkippedAttachment{
			chatSkip(m6, "p9", "all-hands.mov", policy.ReasonOverSizeCap),
		},
		Disposition: SkipBytesNotFetched,
		MemberName:  memberName, ThreadStart: threadStart, TZ: tzAms,
	}))

	for _, want := range []string{
		"**Not stored — `all-hands.mov`** — 209715200 bytes (209.7 MB), declared `video/quicktime`",
		"Reason: it is larger than the per-attachment size cap (`over_size_cap`)",
		"The bytes were never downloaded.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("day file is missing %q:\n%s", want, got)
		}
	}
	// Chat refuses before the download, so it must never claim the bytes
	// arrived.
	if strings.Contains(got, "arrived with the message") {
		t.Errorf("chat day file claims the bytes were downloaded:\n%s", got)
	}
	// The entry belongs to its message, not to the end of the file: it lands
	// after that message's own attachment lines.
	pdfAt := strings.Index(got, "113000_ab12cd34_invoice.pdf")
	skipAt := strings.Index(got, "all-hands.mov")
	if pdfAt < 0 || skipAt < pdfAt {
		t.Errorf("the skip entry is not rendered with its message (pdf=%d skip=%d)", pdfAt, skipAt)
	}
}

// TestRenderChatDaySkipResolutions: a resolved skip must stop claiming to be
// an open question. A fetched one disappears (the attachment row links the
// bytes); the other two stay visible and say what happened.
func TestRenderChatDaySkipResolutions(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	const m6 = "spaces/AAAA/messages/M6"

	render := func(rows ...state.SkippedAttachment) string {
		return string(RenderChatDayFor(ChatDay{
			Space: space, Day: day, Messages: msgs, Attachments: atts,
			Skipped: rows, Disposition: SkipBytesNotFetched,
			MemberName: memberName, ThreadStart: threadStart, TZ: tzAms,
		}))
	}

	fetched := chatSkip(m6, "p9", "recovered.mov", policy.ReasonOverSizeCap)
	fetched.Resolution = state.SkipResolutionFetched
	if got := render(fetched); strings.Contains(got, "recovered.mov") {
		t.Errorf("a fetched skip is still shown as not stored:\n%s", got)
	}

	gone := chatSkip(m6, "p9", "lost.mov", policy.ReasonOverSizeCap)
	gone.Resolution = state.SkipResolutionSourceGone
	got := render(gone)
	if !strings.Contains(got, "no longer exists upstream") {
		t.Errorf("a source_gone skip does not say so:\n%s", got)
	}
	if strings.Contains(got, "save refetch") {
		t.Errorf("a source_gone skip still offers a refetch:\n%s", got)
	}

	denied := chatSkip(m6, "p9", "still.mov", policy.ReasonSVGDenied)
	denied.Resolution = state.SkipResolutionStillDenied
	if got := render(denied); !strings.Contains(got, "still refused") {
		t.Errorf("a still_denied skip does not say so:\n%s", got)
	}
}

// TestRenderChatDaySkipsAreDeterministic: the day file is a whole-file
// projection rewritten on every change, so unordered input must still produce
// identical bytes.
func TestRenderChatDaySkipsAreDeterministic(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	const m6 = "spaces/AAAA/messages/M6"
	rows := []state.SkippedAttachment{
		chatSkip(m6, "p10", "b.mov", policy.ReasonOverSizeCap),
		chatSkip(m6, "p2", "a.mov", policy.ReasonOverSizeCap),
	}
	render := func(in []state.SkippedAttachment) string {
		return string(RenderChatDayFor(ChatDay{
			Space: space, Day: day, Messages: msgs, Attachments: atts,
			Skipped: in, Disposition: SkipBytesNotFetched,
			MemberName: memberName, ThreadStart: threadStart, TZ: tzAms,
		}))
	}
	first := render(rows)
	second := render([]state.SkippedAttachment{rows[1], rows[0]})
	if first != second {
		t.Errorf("input order changed the output:\n%s\n---\n%s", first, second)
	}
	// Part keys sort lexicographically, matching the attachment rows: "p10"
	// before "p2".
	if a, b := strings.Index(first, "b.mov"), strings.Index(first, "a.mov"); a < 0 || b < a {
		t.Errorf("skips are not in part-key order (p10=%d p2=%d)", a, b)
	}
}

// TestRenderChatDaySkipsOnDeletedMessage: the record of what was not kept
// outlives the message body.
func TestRenderChatDaySkipsOnDeletedMessage(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	const m5 = "spaces/AAAA/messages/M5" // the deleted one
	got := string(RenderChatDayFor(ChatDay{
		Space: space, Day: day, Messages: msgs, Attachments: atts,
		Skipped:    []state.SkippedAttachment{chatSkip(m5, "p1", "gone.mov", policy.ReasonOverSizeCap)},
		MemberName: memberName, ThreadStart: threadStart, TZ: tzAms,
	}))
	if !strings.Contains(got, "_(message deleted)_") || !strings.Contains(got, "gone.mov") {
		t.Errorf("a deleted message dropped its skip record:\n%s", got)
	}
}

// TestRenderChatDayUnchangedWithoutSkips: the wrapper must stay byte-identical
// to what it produced before skips existed, so existing day files do not churn.
func TestRenderChatDayUnchangedWithoutSkips(t *testing.T) {
	space, day, msgs, atts, memberName, threadStart := chatFixture()
	legacy := RenderChatDay(space, day, msgs, atts, memberName, threadStart, tzAms)
	viaOptions := RenderChatDayFor(ChatDay{
		Space: space, Day: day, Messages: msgs, Attachments: atts,
		MemberName: memberName, ThreadStart: threadStart, TZ: tzAms,
	})
	if string(legacy) != string(viaOptions) {
		t.Error("RenderChatDay and RenderChatDayFor disagree with no skips")
	}
}
