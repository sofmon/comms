package state_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"comms/internal/state"
)

func TestAttachmentRetryDueList(t *testing.T) {
	db := openTest(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for _, part := range []string{"p1", "p2", "p3"} {
		if err := db.UpsertPendingAttachment(instA, "spaces/A/messages/m1", part, "2026/08/07/f.d/"+part); err != nil {
			t.Fatalf("UpsertPendingAttachment(%s): %v", part, err)
		}
	}

	// Never-tried rows (no scheduled retry) are due immediately.
	due, err := db.DueAttachments(instA, now, 10)
	if err != nil {
		t.Fatalf("DueAttachments: %v", err)
	}
	if len(due) != 3 {
		t.Fatalf("initial due = %d rows; want 3", len(due))
	}
	if due[0].Status != state.AttachmentPending || !due[0].NextRetryAt.IsZero() {
		t.Fatalf("fresh row = %+v; want pending with zero NextRetryAt", due[0])
	}

	// limit is honored.
	if due, _ := db.DueAttachments(instA, now, 1); len(due) != 1 {
		t.Fatalf("DueAttachments limit 1 returned %d rows", len(due))
	}

	// Transient failure: not due until nextRetryAt arrives.
	retryAt := now.Add(time.Hour)
	if err := db.MarkAttachmentRetry(instA, "spaces/A/messages/m1", "p1", "429", 1, retryAt); err != nil {
		t.Fatalf("MarkAttachmentRetry: %v", err)
	}
	if due, _ := db.DueAttachments(instA, now, 10); len(due) != 2 {
		t.Fatalf("due while backing off = %d rows; want 2", len(due))
	}
	if due, _ := db.DueAttachments(instA, now.Add(2*time.Hour), 10); len(due) != 3 {
		t.Fatalf("due after backoff = %d rows; want 3", len(due))
	}

	// Permanent failure: never due again.
	if err := db.MarkAttachmentFailed(instA, "spaces/A/messages/m1", "p2", "404 gone", 3); err != nil {
		t.Fatalf("MarkAttachmentFailed: %v", err)
	}
	// Done: never due again.
	if err := db.MarkAttachmentDone(instA, "spaces/A/messages/m1", "p3", 12345); err != nil {
		t.Fatalf("MarkAttachmentDone: %v", err)
	}
	due, _ = db.DueAttachments(instA, now.Add(24*time.Hour), 10)
	if len(due) != 1 || due[0].PartKey != "p1" {
		t.Fatalf("due after done/failed = %+v; want only p1", due)
	}

	// Replaying the ingestion upsert must not resurrect finished rows.
	if err := db.UpsertPendingAttachment(instA, "spaces/A/messages/m1", "p3", "2026/08/07/f.d/p3"); err != nil {
		t.Fatalf("replay upsert: %v", err)
	}
	atts, err := db.AttachmentsForMessage(instA, "spaces/A/messages/m1")
	if err != nil {
		t.Fatalf("AttachmentsForMessage: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("AttachmentsForMessage = %d rows; want 3", len(atts))
	}
	byPart := map[string]state.Attachment{}
	for _, a := range atts {
		byPart[a.PartKey] = a
	}
	if a := byPart["p1"]; a.Status != state.AttachmentPending || a.Attempts != 1 ||
		a.LastError != "429" || !a.NextRetryAt.Equal(retryAt) {
		t.Fatalf("p1 = %+v; want pending, 1 attempt, 429, retry %v", a, retryAt)
	}
	if a := byPart["p2"]; a.Status != state.AttachmentFailed || a.Attempts != 3 ||
		a.LastError != "404 gone" || !a.NextRetryAt.IsZero() {
		t.Fatalf("p2 = %+v; want failed permanently", a)
	}
	if a := byPart["p3"]; a.Status != state.AttachmentDone || a.Bytes != 12345 ||
		a.LastError != "" || !a.NextRetryAt.IsZero() {
		t.Fatalf("p3 = %+v; want done with 12345 bytes", a)
	}

	counts, err := db.AttachmentCounts()
	if err != nil {
		t.Fatalf("AttachmentCounts: %v", err)
	}
	if c := counts[instA]; c.Pending != 1 || c.Done != 1 || c.Failed != 1 {
		t.Fatalf("counts = %+v; want 1/1/1", c)
	}
}

func TestMarkAttachmentDoneAndDirtyDay(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	registerSpace(t, db)
	ts := time.Date(2026, 8, 7, 11, 30, 0, 0, time.UTC)
	msgName := testSpace + "/messages/m1"

	if err := db.ApplyChatPage(ctx, state.ChatPage{
		Source:   instA,
		Space:    testSpace,
		Messages: []state.ChatMessage{chatMsg("m1", ts, "2026-08-07")},
		Attachments: []state.PendingAttachment{{
			StableID: msgName, PartKey: "p1", RelPath: "2026/08/07/f.d/113000_ab_x.png",
		}},
	}); err != nil {
		t.Fatalf("ApplyChatPage: %v", err)
	}
	dirty, _ := db.DirtyDayFiles(instA)
	if len(dirty) != 1 {
		t.Fatalf("dirty = %d; want 1", len(dirty))
	}
	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", dirty[0].DirtySeq, "hash-1"); err != nil {
		t.Fatalf("MarkDayRendered: %v", err)
	}

	// The download completes on a later run (day clean): done mark and day
	// dirtying must land together, so a crash can never leave a done blob
	// invisible behind a clean day file.
	if err := db.MarkAttachmentDoneAndDirtyDay(instA, msgName, "p1", 77); err != nil {
		t.Fatalf("MarkAttachmentDoneAndDirtyDay: %v", err)
	}
	atts, _ := db.AttachmentsForMessage(instA, msgName)
	if len(atts) != 1 || atts[0].Status != state.AttachmentDone || atts[0].Bytes != 77 {
		t.Fatalf("attachment = %+v; want done with 77 bytes", atts)
	}
	dirty, _ = db.DirtyDayFiles(instA)
	if len(dirty) != 1 || dirty[0].DayBucket != "2026-08-07" {
		t.Fatalf("dirty after done = %+v; want the owning day re-dirtied", dirty)
	}

	// An unknown attachment row is an error and dirties nothing.
	if err := db.MarkDayRendered(instA, testSpace, "2026-08-07", dirty[0].DirtySeq, "hash-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAttachmentDoneAndDirtyDay(instA, msgName, "pNOPE", 1); err == nil {
		t.Fatal("MarkAttachmentDoneAndDirtyDay on unknown row did not error")
	}
	if dirty, _ := db.DirtyDayFiles(instA); len(dirty) != 0 {
		t.Fatalf("failed done-mark dirtied a day: %+v", dirty)
	}
}

// TestMarkAttachmentDoneAndDirtyDayIsAccountScoped: both accounts archive
// the same chat message and each registers its own copy of the upload. One
// account's completed download must dirty only its own day file.
func TestMarkAttachmentDoneAndDirtyDayIsAccountScoped(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 11, 30, 0, 0, time.UTC)
	msgName := testSpace + "/messages/m1"

	for _, in := range []string{instA, instB} {
		registerSpaceFor(t, db, in)
		if err := db.ApplyChatPage(ctx, state.ChatPage{
			Source:   in,
			Space:    testSpace,
			Messages: []state.ChatMessage{chatMsg("m1", ts, "2026-08-07")},
			Attachments: []state.PendingAttachment{{
				StableID: msgName, PartKey: "p1", RelPath: "2026/08/07/f.d/113000_ab_x.png",
			}},
		}); err != nil {
			t.Fatalf("ApplyChatPage(%s): %v", in, err)
		}
		dirty, _ := db.DirtyDayFiles(in)
		if len(dirty) != 1 {
			t.Fatalf("dirty(%s) = %d; want 1", in, len(dirty))
		}
		if err := db.MarkDayRendered(in, testSpace, "2026-08-07", dirty[0].DirtySeq, "hash"); err != nil {
			t.Fatalf("MarkDayRendered(%s): %v", in, err)
		}
	}

	if err := db.MarkAttachmentDoneAndDirtyDay(instA, msgName, "p1", 77); err != nil {
		t.Fatalf("MarkAttachmentDoneAndDirtyDay: %v", err)
	}
	if dirty, _ := db.DirtyDayFiles(instA); len(dirty) != 1 {
		t.Fatalf("A's day not re-dirtied: %d rows", len(dirty))
	}
	if dirty, _ := db.DirtyDayFiles(instB); len(dirty) != 0 {
		t.Fatalf("A's download dirtied B's day file: %+v", dirty)
	}
	if atts, _ := db.AttachmentsForMessage(instB, msgName); len(atts) != 1 || atts[0].Status != state.AttachmentPending {
		t.Fatalf("B's attachment row = %+v; want still pending", atts)
	}
}

// TestDueAttachmentsAreAccountScoped: the pending-download ledger is shared
// by every configured account, so a sibling mid-backfill would otherwise
// fill the whole due window with rows the caller may not touch and starve
// its own downloads.
func TestDueAttachmentsAreAccountScoped(t *testing.T) {
	db := openTest(t)
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	// A large backlog for B, one row for A. "gchat:personal" sorts ahead of
	// every "gchat:work" row, so an unfiltered query with a small limit
	// would return nothing of A's.
	for i := 0; i < 50; i++ {
		part := "p" + strconv.Itoa(i)
		if err := db.UpsertPendingAttachment(instB, "spaces/A/messages/m1", part, "2026/08/07/b.d/"+part); err != nil {
			t.Fatalf("UpsertPendingAttachment(B, %s): %v", part, err)
		}
	}
	if err := db.UpsertPendingAttachment(instA, "spaces/A/messages/m1", "p0", "2026/08/07/a.d/p0"); err != nil {
		t.Fatalf("UpsertPendingAttachment(A): %v", err)
	}

	due, err := db.DueAttachments(instA, now, 10)
	if err != nil {
		t.Fatalf("DueAttachments(A): %v", err)
	}
	if len(due) != 1 || due[0].Source != instA || due[0].PartKey != "p0" {
		t.Fatalf("due(A) = %+v; want only A's single row (a sibling backlog starved it)", due)
	}
	due, err = db.DueAttachments(instB, now, 10)
	if err != nil {
		t.Fatalf("DueAttachments(B): %v", err)
	}
	if len(due) != 10 {
		t.Fatalf("due(B) = %d rows; want the limit, 10", len(due))
	}
	for _, a := range due {
		if a.Source != instB {
			t.Fatalf("due(B) returned another account's row: %+v", a)
		}
	}
}

func TestAttachmentUpdateUnknownRow(t *testing.T) {
	db := openTest(t)
	if err := db.MarkAttachmentDone(instA, "spaces/X/messages/m", "p", 1); err == nil {
		t.Fatal("MarkAttachmentDone on unknown row did not error")
	}
	if err := db.MarkAttachmentRetry(instA, "spaces/X/messages/m", "p", "e", 1, time.Now()); err == nil {
		t.Fatal("MarkAttachmentRetry on unknown row did not error")
	}
	if err := db.MarkAttachmentFailed(instA, "spaces/X/messages/m", "p", "e", 1); err == nil {
		t.Fatal("MarkAttachmentFailed on unknown row did not error")
	}
}
