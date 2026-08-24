package state_test

import (
	"context"
	"testing"
	"time"

	"save/internal/naming"
	"save/internal/state"
)

const testSpace = "spaces/AAA"

// The two chat instances of a two-Google-account config. Every chat table is
// keyed by these, not by the bare "gchat" kind.
var (
	instA = state.InstanceID(state.SourceGChat, "work")
	instB = state.InstanceID(state.SourceGChat, "personal")
)

func registerSpaceFor(t *testing.T, db *state.DB, source string) {
	t.Helper()
	if err := db.UpsertSpace(source, testSpace, "space", "Team Platform", "team-platform", "HISTORY_ON"); err != nil {
		t.Fatalf("UpsertSpace(%s): %v", source, err)
	}
}

func registerSpace(t *testing.T, db *state.DB) {
	t.Helper()
	registerSpaceFor(t, db, instA)
}

func chatMsg(name string, ts time.Time, day string) state.ChatMessage {
	return state.ChatMessage{
		Name:       testSpace + "/messages/" + name,
		SenderID:   "users/1",
		CreateTime: ts,
		DayBucket:  day,
		RawJSON:    `{"name":"` + name + `"}`,
	}
}

func TestApplyChatPageAtomicity(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	// A page against an unregistered space is refused outright.
	err := db.ApplyChatPage(ctx, state.ChatPage{
		Source:   instA,
		Space:    "spaces/UNKNOWN",
		Messages: []state.ChatMessage{chatMsg("m0", ts, "2026-08-07")},
	})
	if err == nil {
		t.Fatal("ApplyChatPage accepted an unregistered space")
	}

	// A page whose source is not an instance id is refused: rows keyed by a
	// bare kind would be shared by every configured account.
	for _, bad := range []string{"", state.SourceGChat} {
		if err := db.ApplyChatPage(ctx, state.ChatPage{
			Source:   bad,
			Space:    testSpace,
			Messages: []state.ChatMessage{chatMsg("m0", ts, "2026-08-07")},
		}); err == nil {
			t.Fatalf("ApplyChatPage accepted source %q", bad)
		}
	}
	if err := db.UpsertSpace(state.SourceGChat, testSpace, "space", "X", "x", ""); err == nil {
		t.Fatal("UpsertSpace accepted a bare source kind")
	}

	// Crash simulation: the second message is invalid, so the failure hits
	// after the first message, the day mark, and part of the page were
	// already executed inside the transaction. Everything must roll back.
	bad := state.ChatMessage{ // missing name
		SenderID: "users/2", CreateTime: ts, DayBucket: "2026-08-07", RawJSON: "{}",
	}
	err = db.ApplyChatPage(ctx, state.ChatPage{
		Source:      instA,
		Space:       testSpace,
		Messages:    []state.ChatMessage{chatMsg("m1", ts, "2026-08-07"), bad},
		Attachments: []state.PendingAttachment{{StableID: testSpace + "/messages/m1", PartKey: "p1", RelPath: "x"}},
		Cursor:      "2026-08-07T10:00:00Z",
	})
	if err == nil {
		t.Fatal("ApplyChatPage accepted an invalid message")
	}
	if msgs, _ := db.MessagesForDay(instA, testSpace, "2026-08-07"); len(msgs) != 0 {
		t.Fatalf("rollback leaked %d chat messages", len(msgs))
	}
	if dirty, _ := db.DirtyDayFiles(instA); len(dirty) != 0 {
		t.Fatalf("rollback leaked %d dirty day files", len(dirty))
	}
	if _, ok, _ := db.GetCursor(instA, testSpace, state.CursorMsgCreateTime); ok {
		t.Fatal("rollback leaked a cursor advance")
	}
	if atts, _ := db.AttachmentsForMessage(instA, testSpace+"/messages/m1"); len(atts) != 0 {
		t.Fatalf("rollback leaked %d attachment rows", len(atts))
	}

	// The same page, valid: everything lands together.
	page := state.ChatPage{
		Source: instA,
		Space:  testSpace,
		Messages: []state.ChatMessage{
			chatMsg("m1", ts, "2026-08-07"),
			chatMsg("m2", ts.Add(24*time.Hour), "2026-08-08"),
		},
		Attachments: []state.PendingAttachment{{
			StableID: testSpace + "/messages/m1",
			PartKey:  "p1",
			RelPath:  "2026/08/07/gchat-work_space_team-platform_x.d/100000_ab_f.png",
		}},
		Cursor: "2026-08-08T10:00:00Z",
	}
	if err := db.ApplyChatPage(ctx, page); err != nil {
		t.Fatalf("ApplyChatPage: %v", err)
	}

	msgs, err := db.MessagesForDay(instA, testSpace, "2026-08-07")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("MessagesForDay = %d msgs, %v; want 1", len(msgs), err)
	}
	if m := msgs[0]; m.Name != testSpace+"/messages/m1" || m.Space != testSpace ||
		m.Source != instA || !m.CreateTime.Equal(ts) || m.RawJSON != `{"name":"m1"}` {
		t.Fatalf("stored message = %+v", m)
	}

	wantRel := "2026/08/07/" + naming.ChatStem(state.Tag(instA), "space", "team-platform", naming.Hash8(testSpace)) + ".md"
	dirty, err := db.DirtyDayFiles(instA)
	if err != nil || len(dirty) != 2 {
		t.Fatalf("DirtyDayFiles = %d rows, %v; want 2", len(dirty), err)
	}
	if dirty[0].DayBucket != "2026-08-07" || dirty[0].RelPath != wantRel || dirty[0].Source != instA {
		t.Fatalf("dirty[0] = %+v; want day 2026-08-07 rel %q", dirty[0], wantRel)
	}

	cur, ok, err := db.GetCursor(instA, testSpace, state.CursorMsgCreateTime)
	if err != nil || !ok || cur != "2026-08-08T10:00:00Z" {
		t.Fatalf("cursor = %q, %v, %v", cur, ok, err)
	}
	if atts, _ := db.AttachmentsForMessage(instA, testSpace+"/messages/m1"); len(atts) != 1 {
		t.Fatalf("attachment rows = %d; want 1", len(atts))
	}
}

// TestChatRowsAreAccountScoped is the core multi-account regression: two
// configured Google accounts are members of the SAME space and therefore see
// the same space and message resource names. Nothing may be shared between
// them — not the space row, not the messages, not the day-file projection,
// not the cursor, not the member cache.
func TestChatRowsAreAccountScoped(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	registerSpaceFor(t, db, instA)
	// The second account joined later and sees a renamed space, so its
	// frozen slug differs — freezing is per (source, space).
	if err := db.UpsertSpace(instB, testSpace, "space", "Platform Guild", "platform-guild", "HISTORY_ON"); err != nil {
		t.Fatalf("UpsertSpace(B): %v", err)
	}

	// A registers only for itself.
	if has, err := db.HasChatSpaces(instA); err != nil || !has {
		t.Fatalf("HasChatSpaces(A) = %v, %v", has, err)
	}
	if has, err := db.HasChatSpaces(state.InstanceID(state.SourceGChat, "third")); err != nil || has {
		t.Fatalf("HasChatSpaces(unknown) = %v, %v; want false", has, err)
	}
	spA, _, _ := db.GetSpace(instA, testSpace)
	spB, _, _ := db.GetSpace(instB, testSpace)
	if spA.DisplaySlug != "team-platform" || spB.DisplaySlug != "platform-guild" {
		t.Fatalf("frozen slugs bled across accounts: A=%q B=%q", spA.DisplaySlug, spB.DisplaySlug)
	}
	if spA.Source != instA || spB.Source != instB {
		t.Fatalf("space rows carry the wrong source: %q / %q", spA.Source, spB.Source)
	}

	// The very same message resource name arrives for both accounts.
	msg := chatMsg("m1", ts, "2026-08-07")
	for _, in := range []string{instA, instB} {
		if err := db.ApplyChatPage(ctx, state.ChatPage{
			Source:   in,
			Space:    testSpace,
			Messages: []state.ChatMessage{msg},
			Cursor:   "2026-08-07T10:00:00Z",
		}); err != nil {
			t.Fatalf("ApplyChatPage(%s): %v", in, err)
		}
	}
	for _, in := range []string{instA, instB} {
		msgs, err := db.MessagesForDay(in, testSpace, "2026-08-07")
		if err != nil || len(msgs) != 1 || msgs[0].Source != in {
			t.Fatalf("MessagesForDay(%s) = %+v, %v; want exactly one row keyed by that instance", in, msgs, err)
		}
	}

	// Two day files, distinguished by the account's file tag.
	all, err := db.AllDirtyDayFiles()
	if err != nil || len(all) != 2 {
		t.Fatalf("AllDirtyDayFiles = %d rows, %v; want 2", len(all), err)
	}
	wantA := "2026/08/07/" + naming.ChatStem("gchat-work", "space", "team-platform", naming.Hash8(testSpace)) + ".md"
	wantB := "2026/08/07/" + naming.ChatStem("gchat-personal", "space", "platform-guild", naming.Hash8(testSpace)) + ".md"
	got := map[string]string{all[0].Source: all[0].RelPath, all[1].Source: all[1].RelPath}
	if got[instA] != wantA || got[instB] != wantB {
		t.Fatalf("day-file paths = %v; want %q and %q", got, wantA, wantB)
	}

	// Rendering A's day leaves B's dirty.
	dirtyA, _ := db.DirtyDayFiles(instA)
	if len(dirtyA) != 1 {
		t.Fatalf("DirtyDayFiles(A) = %d; want 1", len(dirtyA))
	}
	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", dirtyA[0].DirtySeq, "hash-a"); err != nil {
		t.Fatalf("MarkDayRendered(A): %v", err)
	}
	if d, _ := db.DirtyDayFiles(instA); len(d) != 0 {
		t.Fatalf("A still dirty after render: %d", len(d))
	}
	if d, _ := db.DirtyDayFiles(instB); len(d) != 1 {
		t.Fatalf("rendering A cleared B's day file: %d rows", len(d))
	}

	// Cursors, member caches and per-day sender indexes stay separate.
	if err := db.SetMember(instA, testSpace, "users/1", "Jane (work)"); err != nil {
		t.Fatalf("SetMember: %v", err)
	}
	if _, ok, _ := db.GetMember(instB, testSpace, "users/1"); ok {
		t.Fatal("member cache leaked across accounts")
	}
	if _, ok, _ := db.GetCursor(instB, testSpace, state.CursorMsgCreateTime); !ok {
		t.Fatal("B has no cursor of its own")
	}
	if days, _ := db.DaysWithSender(instB, testSpace, "users/1"); len(days) != 1 {
		t.Fatalf("DaysWithSender(B) = %v; want B's own day", days)
	}
}

func TestApplyChatPageRejectsForeignMessageSource(t *testing.T) {
	db := openTest(t)
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	m := chatMsg("m1", ts, "2026-08-07")
	m.Source = instB // belongs to the other account
	err := db.ApplyChatPage(context.Background(), state.ChatPage{
		Source: instA, Space: testSpace, Messages: []state.ChatMessage{m},
	})
	if err == nil {
		t.Fatal("ApplyChatPage accepted a message tagged with another account's instance")
	}
}

func TestApplyChatPageEditSticky(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	m := chatMsg("m1", ts, "2026-08-07")
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{m}}); err != nil {
		t.Fatalf("initial page: %v", err)
	}

	// Edit arrives via events pass.
	edited := m
	edited.Edited = true
	edited.LastUpdateTime = ts.Add(time.Minute)
	edited.RawJSON = `{"name":"m1","text":"edited"}`
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{edited}}); err != nil {
		t.Fatalf("edit page: %v", err)
	}

	// A replayed stale page (overlap refetch carrying the pre-edit flags)
	// must not un-edit the message; raw_json still tracks the replay.
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{m}}); err != nil {
		t.Fatalf("stale replay page: %v", err)
	}

	msgs, _ := db.MessagesForDay(instA, testSpace, "2026-08-07")
	if len(msgs) != 1 {
		t.Fatalf("got %d messages; want 1", len(msgs))
	}
	if !msgs[0].Edited {
		t.Fatal("edited flag regressed after stale replay")
	}

	// Deletion tombstone is sticky the same way.
	deleted := m
	deleted.Deleted = true
	deleted.DeletedAt = ts.Add(2 * time.Minute)
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{deleted}}); err != nil {
		t.Fatalf("delete page: %v", err)
	}
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{m}}); err != nil {
		t.Fatalf("stale replay after delete: %v", err)
	}
	msgs, _ = db.MessagesForDay(instA, testSpace, "2026-08-07")
	if !msgs[0].Deleted || msgs[0].DeletedAt.IsZero() {
		t.Fatalf("deletion tombstone regressed: %+v", msgs[0])
	}
}

func TestFrozenSlugNeverOverwritten(t *testing.T) {
	db := openTest(t)
	registerSpace(t, db)

	// The space is renamed server-side and its type changes (a group chat
	// upgraded to a named space would report "space"); slug, type and
	// first-seen stay frozen — both are day-file path components.
	if err := db.UpsertSpace(instA, testSpace, "group", "Platform Guild", "platform-guild", "HISTORY_OFF"); err != nil {
		t.Fatalf("re-UpsertSpace: %v", err)
	}
	sp, ok, err := db.GetSpace(instA, testSpace)
	if err != nil || !ok {
		t.Fatalf("GetSpace = %v, %v", ok, err)
	}
	if sp.DisplaySlug != "team-platform" {
		t.Fatalf("frozen slug overwritten: %q", sp.DisplaySlug)
	}
	if sp.Type != "space" {
		t.Fatalf("frozen space type overwritten: %q", sp.Type)
	}
	if sp.DisplayName != "Platform Guild" || sp.HistoryState != "HISTORY_OFF" {
		t.Fatalf("mutable fields not updated: %+v", sp)
	}
	if sp.FirstSeenAt.IsZero() || sp.LastSyncedAt.IsZero() {
		t.Fatalf("timestamps missing: %+v", sp)
	}

	if _, ok, _ := db.GetSpace(instA, "spaces/NOPE"); ok {
		t.Fatal("GetSpace found an unknown space")
	}
	if _, ok, _ := db.GetSpace(instB, testSpace); ok {
		t.Fatal("GetSpace found another account's space")
	}
	spaces, err := db.ListSpaces(instA)
	if err != nil || len(spaces) != 1 || spaces[0].Name != testSpace {
		t.Fatalf("ListSpaces = %+v, %v", spaces, err)
	}
	if spaces, err := db.ListSpaces(instB); err != nil || len(spaces) != 0 {
		t.Fatalf("ListSpaces(B) = %+v, %v; want none", spaces, err)
	}
}

func TestMessagesForDaySubSecondOrder(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)

	// Same second, different nanoseconds: the whole-second message must sort
	// first. (Catches variable-width fraction encodings, where "…05Z" sorts
	// after "…05.5Z" byte-wise.)
	base := time.Date(2026, 8, 7, 10, 0, 5, 0, time.UTC)
	page := state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{
		chatMsg("b", base.Add(500*time.Millisecond), "2026-08-07"),
		chatMsg("a", base, "2026-08-07"),
		chatMsg("c", base.Add(40*time.Millisecond), "2026-08-07"),
	}}
	if err := db.ApplyChatPage(ctx, page); err != nil {
		t.Fatalf("ApplyChatPage: %v", err)
	}
	msgs, err := db.MessagesForDay(instA, testSpace, "2026-08-07")
	if err != nil {
		t.Fatalf("MessagesForDay: %v", err)
	}
	var got []string
	for _, m := range msgs {
		got = append(got, m.Name)
	}
	want := []string{
		testSpace + "/messages/a",
		testSpace + "/messages/c",
		testSpace + "/messages/b",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v; want %v", got, want)
		}
	}
	// Round-trip preserves sub-second precision.
	if !msgs[1].CreateTime.Equal(base.Add(40 * time.Millisecond)) {
		t.Fatalf("create time lost precision: %v", msgs[1].CreateTime)
	}
}

func TestDayFileRenderCycle(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	page := state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{chatMsg("m1", ts, "2026-08-07")}}
	if err := db.ApplyChatPage(ctx, page); err != nil {
		t.Fatalf("ApplyChatPage: %v", err)
	}
	dirty, _ := db.DirtyDayFiles(instA)
	if len(dirty) != 1 {
		t.Fatalf("dirty = %d; want 1", len(dirty))
	}

	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", dirty[0].DirtySeq, "hash-1"); err != nil {
		t.Fatalf("MarkDayRendered: %v", err)
	}
	if dirty, _ := db.DirtyDayFiles(instA); len(dirty) != 0 {
		t.Fatalf("still dirty after render: %d", len(dirty))
	}

	// New same-day message re-dirties without losing the rel path.
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{chatMsg("m2", ts.Add(time.Minute), "2026-08-07")}}); err != nil {
		t.Fatalf("second page: %v", err)
	}
	dirty, _ = db.DirtyDayFiles(instA)
	if len(dirty) != 1 || dirty[0].RelPath == "" || dirty[0].ContentHash != "hash-1" {
		t.Fatalf("re-dirtied row = %+v", dirty)
	}

	// Manual dirty on an already-tracked day.
	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", dirty[0].DirtySeq, "hash-2"); err != nil {
		t.Fatalf("MarkDayRendered: %v", err)
	}
	if err := db.MarkDayDirty(instA, testSpace, "2026-08-07"); err != nil {
		t.Fatalf("MarkDayDirty: %v", err)
	}
	if dirty, _ := db.DirtyDayFiles(instA); len(dirty) != 1 {
		t.Fatal("MarkDayDirty on tracked day did not dirty it")
	}

	// Manual dirty on an untracked day creates the ledger row with the
	// deterministic path.
	if err := db.MarkDayDirty(instA, testSpace, "2026-09-01"); err != nil {
		t.Fatalf("MarkDayDirty untracked: %v", err)
	}
	dirty, _ = db.DirtyDayFiles(instA)
	if len(dirty) != 2 {
		t.Fatalf("dirty = %d; want 2", len(dirty))
	}
	wantRel := "2026/09/01/" + naming.ChatStem(state.Tag(instA), "space", "team-platform", naming.Hash8(testSpace)) + ".md"
	if dirty[1].RelPath != wantRel {
		t.Fatalf("untracked day rel = %q; want %q", dirty[1].RelPath, wantRel)
	}

	// Unknown space cannot be dirtied — and neither can a space that only
	// another account has registered.
	if err := db.MarkDayDirty(instA, "spaces/NOPE", "2026-09-01"); err == nil {
		t.Fatal("MarkDayDirty accepted an unknown space")
	}
	if err := db.MarkDayDirty(instB, testSpace, "2026-09-01"); err == nil {
		t.Fatal("MarkDayDirty accepted a space registered by another account")
	}
}

func TestMarkDayRenderedStaleSeqLeavesDayDirty(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	// Dirty the day and snapshot it the way a render pass does.
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{chatMsg("m1", ts, "2026-08-07")}}); err != nil {
		t.Fatalf("first page: %v", err)
	}
	dirty, err := db.DirtyDayFiles(instA)
	if err != nil || len(dirty) != 1 {
		t.Fatalf("DirtyDayFiles = %d rows, %v; want 1", len(dirty), err)
	}
	staleSeq := dirty[0].DirtySeq

	// A concurrent write lands after the snapshot: the seq is bumped.
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{chatMsg("m2", ts.Add(time.Minute), "2026-08-07")}}); err != nil {
		t.Fatalf("second page: %v", err)
	}

	// Clearing with the stale seq must be a no-op: the day stays dirty so
	// the next pass re-renders it with the newer message included.
	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", staleSeq, "stale-hash"); err != nil {
		t.Fatalf("MarkDayRendered stale: %v", err)
	}
	dirty, err = db.DirtyDayFiles(instA)
	if err != nil || len(dirty) != 1 {
		t.Fatalf("day was cleared by a stale render: %d rows, %v; want still dirty", len(dirty), err)
	}
	if dirty[0].ContentHash == "stale-hash" {
		t.Fatal("stale render recorded its content hash")
	}

	// Clearing with the current seq works.
	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", dirty[0].DirtySeq, "fresh-hash"); err != nil {
		t.Fatalf("MarkDayRendered fresh: %v", err)
	}
	if dirty, _ := db.DirtyDayFiles(instA); len(dirty) != 0 {
		t.Fatal("current-seq render did not clear the day")
	}

	// MarkDayDirty also bumps the seq, guarding attachment/member fills.
	if err := db.MarkDayDirty(instA, testSpace, "2026-08-07"); err != nil {
		t.Fatalf("MarkDayDirty: %v", err)
	}
	dirty, _ = db.DirtyDayFiles(instA)
	if len(dirty) != 1 || dirty[0].DirtySeq <= staleSeq {
		t.Fatalf("MarkDayDirty seq = %+v; want a bump past %d", dirty, staleSeq)
	}
}

func TestThreadFirstCreateTime(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	registerSpaceFor(t, db, instB)
	thread := testSpace + "/threads/T1"
	early := time.Date(2026, 8, 6, 21, 0, 0, 500000000, time.UTC)
	late := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	opener := chatMsg("m1", early, "2026-08-06")
	opener.Thread = thread
	opener.Deleted = true // tombstones still date the thread's start
	opener.DeletedAt = late
	reply := chatMsg("m2", late, "2026-08-07")
	reply.Thread = thread
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{opener, reply}}); err != nil {
		t.Fatalf("ApplyChatPage: %v", err)
	}

	got, ok, err := db.ThreadFirstCreateTime(instA, testSpace, thread)
	if err != nil || !ok {
		t.Fatalf("ThreadFirstCreateTime = %v, %v", ok, err)
	}
	if !got.Equal(early) {
		t.Fatalf("first create time = %v; want %v (min across days, sub-second kept)", got, early)
	}

	if _, ok, err := db.ThreadFirstCreateTime(instA, testSpace, testSpace+"/threads/NOPE"); err != nil || ok {
		t.Fatalf("unknown thread = %v, %v; want miss", ok, err)
	}
	if _, ok, err := db.ThreadFirstCreateTime(instA, "spaces/OTHER", thread); err != nil || ok {
		t.Fatalf("wrong space = %v, %v; want miss", ok, err)
	}
	// The other account has not archived this thread: it must not borrow A's
	// history to render a "(continued)" header.
	if _, ok, err := db.ThreadFirstCreateTime(instB, testSpace, thread); err != nil || ok {
		t.Fatalf("other account = %v, %v; want miss", ok, err)
	}
}

func TestHasChatSpaces(t *testing.T) {
	db := openTest(t)
	if has, err := db.HasChatSpaces(instA); err != nil || has {
		t.Fatalf("HasChatSpaces on empty = %v, %v; want false", has, err)
	}
	registerSpace(t, db)
	if has, err := db.HasChatSpaces(instA); err != nil || !has {
		t.Fatalf("HasChatSpaces after register = %v, %v; want true", has, err)
	}
	if has, err := db.HasChatSpaces(instB); err != nil || has {
		t.Fatalf("HasChatSpaces(other account) = %v, %v; want false", has, err)
	}
}

func TestDaysWithSender(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)

	m1 := chatMsg("m1", ts, "2026-08-07")
	m2 := chatMsg("m2", ts.Add(-24*time.Hour), "2026-08-06")
	m3 := chatMsg("m3", ts.Add(time.Minute), "2026-08-07") // same day, same sender
	other := chatMsg("m4", ts, "2026-08-09")
	other.SenderID = "users/2"
	if err := db.ApplyChatPage(ctx, state.ChatPage{Source: instA, Space: testSpace, Messages: []state.ChatMessage{m1, m2, m3, other}}); err != nil {
		t.Fatalf("ApplyChatPage: %v", err)
	}

	days, err := db.DaysWithSender(instA, testSpace, "users/1")
	if err != nil {
		t.Fatalf("DaysWithSender: %v", err)
	}
	want := []string{"2026-08-06", "2026-08-07"}
	if len(days) != len(want) || days[0] != want[0] || days[1] != want[1] {
		t.Fatalf("DaysWithSender = %v; want %v", days, want)
	}
	if days, _ := db.DaysWithSender(instA, testSpace, "users/NOPE"); len(days) != 0 {
		t.Fatalf("unknown sender days = %v; want none", days)
	}
	if days, _ := db.DaysWithSender(instB, testSpace, "users/1"); len(days) != 0 {
		t.Fatalf("other account days = %v; want none", days)
	}
}

func TestChatMembers(t *testing.T) {
	db := openTest(t)

	if _, ok, err := db.GetMember(instA, testSpace, "users/1"); err != nil || ok {
		t.Fatalf("GetMember on empty = %v, %v; want miss", ok, err)
	}
	if err := db.SetMember(instA, testSpace, "users/1", "Jane Doe"); err != nil {
		t.Fatalf("SetMember: %v", err)
	}
	if err := db.SetMember(instA, testSpace, "users/1", "Jane A. Doe"); err != nil {
		t.Fatalf("SetMember overwrite: %v", err)
	}
	name, ok, err := db.GetMember(instA, testSpace, "users/1")
	if err != nil || !ok || name != "Jane A. Doe" {
		t.Fatalf("GetMember = %q, %v, %v", name, ok, err)
	}
	// Same user id in another space is a distinct cache entry.
	if _, ok, _ := db.GetMember(instA, "spaces/BBB", "users/1"); ok {
		t.Fatal("member cache leaked across spaces")
	}
	// So is the same (space, user) seen by another account.
	if _, ok, _ := db.GetMember(instB, testSpace, "users/1"); ok {
		t.Fatal("member cache leaked across accounts")
	}
}
