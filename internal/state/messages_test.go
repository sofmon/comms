package state_test

import (
	"reflect"
	"testing"
	"time"

	"comms/internal/state"
)

func TestEmailDedup(t *testing.T) {
	db := openTest(t)

	msg := state.Message{
		Source:      state.SourceGmail,
		StableID:    "18f2a",
		RFC822MsgID: "<abc@example.com>",
		ThreadID:    "t1",
		TS:          time.Date(2026, 8, 7, 12, 32, 5, 123000000, time.UTC),
		OrigOffset:  "+02:00",
		DayBucket:   "2026-08-07",
		RelPath:     "2026/08/07/143205_gmail_re-invoice_a1b2c3d4.md",
		ContentHash: "hash-1",
	}

	seen, err := db.SeenMessage(msg.Source, msg.StableID)
	if err != nil || seen {
		t.Fatalf("SeenMessage before commit = %v, %v; want false, nil", seen, err)
	}
	if err := db.CommitMessage(msg); err != nil {
		t.Fatalf("CommitMessage: %v", err)
	}
	seen, err = db.SeenMessage(msg.Source, msg.StableID)
	if err != nil || !seen {
		t.Fatalf("SeenMessage after commit = %v, %v; want true, nil", seen, err)
	}

	// Re-commit (crash-replay re-render) must not create a second row.
	msg.ContentHash = "hash-2"
	if err := db.CommitMessage(msg); err != nil {
		t.Fatalf("re-CommitMessage: %v", err)
	}
	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatalf("MessageCounts: %v", err)
	}
	if got := counts[state.SourceGmail]; got.Total != 1 || got.Deleted != 0 {
		t.Fatalf("counts after re-commit = %+v; want Total 1, Deleted 0", got)
	}

	// Same stable id under another source is a distinct message.
	other := msg
	other.Source = state.SourceFastmail
	if err := db.CommitMessage(other); err != nil {
		t.Fatalf("CommitMessage other source: %v", err)
	}
	counts, _ = db.MessageCounts()
	if counts[state.SourceGmail].Total != 1 || counts[state.SourceFastmail].Total != 1 {
		t.Fatalf("cross-source counts = %+v", counts)
	}
}

func TestMessageTombstone(t *testing.T) {
	db := openTest(t)

	msg := state.Message{
		Source:      state.SourceFastmail,
		StableID:    "M1",
		TS:          time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		DayBucket:   "2026-08-07",
		RelPath:     "2026/08/07/090000_fastmail_hello_deadbeef.md",
		ContentHash: "h",
	}
	if err := db.CommitMessage(msg); err != nil {
		t.Fatalf("CommitMessage: %v", err)
	}
	if err := db.MarkMessageDeleted(msg.Source, msg.StableID); err != nil {
		t.Fatalf("MarkMessageDeleted: %v", err)
	}
	// Tombstoning an id we never archived must be a harmless no-op.
	if err := db.MarkMessageDeleted(msg.Source, "never-seen"); err != nil {
		t.Fatalf("MarkMessageDeleted unknown: %v", err)
	}

	counts, err := db.MessageCounts()
	if err != nil {
		t.Fatalf("MessageCounts: %v", err)
	}
	if got := counts[state.SourceFastmail]; got.Total != 1 || got.Deleted != 1 {
		t.Fatalf("counts = %+v; want Total 1, Deleted 1", got)
	}
	// Tombstoned is still seen: the file stays on disk.
	if seen, _ := db.SeenMessage(msg.Source, msg.StableID); !seen {
		t.Fatal("tombstoned message no longer seen")
	}
}

func TestCommitMessageValidation(t *testing.T) {
	db := openTest(t)
	bad := state.Message{Source: state.SourceGmail} // everything else missing
	if err := db.CommitMessage(bad); err == nil {
		t.Fatal("CommitMessage accepted an incomplete row")
	}
}

func TestBackfillQueue(t *testing.T) {
	db := openTest(t)
	gm := state.InstanceID(state.SourceGmail, "work")

	if err := db.EnqueueBackfill(gm, []string{"a", "b", "c"}); err != nil {
		t.Fatalf("EnqueueBackfill: %v", err)
	}
	// Overlapping replay of an enumeration page: duplicates ignored.
	if err := db.EnqueueBackfill(gm, []string{"b", "c", "d"}); err != nil {
		t.Fatalf("EnqueueBackfill replay: %v", err)
	}
	if n, err := db.BackfillPendingCount(gm); err != nil || n != 4 {
		t.Fatalf("BackfillPendingCount = %d, %v; want 4", n, err)
	}

	ids, err := db.NextPendingBackfill(gm, 10)
	if err != nil {
		t.Fatalf("NextPendingBackfill: %v", err)
	}
	if want := []string{"a", "b", "c", "d"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("NextPendingBackfill = %v; want %v (enqueue order)", ids, want)
	}
	if ids, _ := db.NextPendingBackfill(gm, 2); !reflect.DeepEqual(ids, []string{"a", "b"}) {
		t.Fatalf("NextPendingBackfill(2) = %v; want [a b]", ids)
	}

	if err := db.MarkBackfillDone(gm, "b"); err != nil {
		t.Fatalf("MarkBackfillDone: %v", err)
	}
	if err := db.MarkBackfillDone(gm, "nope"); err != nil {
		t.Fatalf("MarkBackfillDone unknown: %v", err)
	}
	if n, _ := db.BackfillPendingCount(gm); n != 3 {
		t.Fatalf("pending after done = %d; want 3", n)
	}
	if ids, _ := db.NextPendingBackfill(gm, 10); !reflect.DeepEqual(ids, []string{"a", "c", "d"}) {
		t.Fatalf("pending ids after done = %v; want [a c d]", ids)
	}

	if err := db.ClearBackfill(gm); err != nil {
		t.Fatalf("ClearBackfill: %v", err)
	}
	if n, _ := db.BackfillPendingCount(gm); n != 0 {
		t.Fatalf("pending after clear = %d; want 0", n)
	}
}

// Gmail message ids are only unique within one mailbox, so two accounts
// backfilling ids that happen to collide must keep independent queues.
func TestBackfillQueueIsAccountScoped(t *testing.T) {
	db := openTest(t)
	a := state.InstanceID(state.SourceGmail, "work")
	b := state.InstanceID(state.SourceGmail, "personal")

	if err := db.EnqueueBackfill(a, []string{"m1", "m2"}); err != nil {
		t.Fatalf("EnqueueBackfill(a): %v", err)
	}
	if err := db.EnqueueBackfill(b, []string{"m1"}); err != nil {
		t.Fatalf("EnqueueBackfill(b): %v", err)
	}
	if n, _ := db.BackfillPendingCount(a); n != 2 {
		t.Fatalf("a pending = %d; want 2", n)
	}
	if n, _ := db.BackfillPendingCount(b); n != 1 {
		t.Fatalf("b pending = %d; want 1 (same id, different account)", n)
	}

	// Finishing a's copy of the shared id leaves b's queued.
	if err := db.MarkBackfillDone(a, "m1"); err != nil {
		t.Fatalf("MarkBackfillDone: %v", err)
	}
	if ids, _ := db.NextPendingBackfill(b, 10); !reflect.DeepEqual(ids, []string{"m1"}) {
		t.Fatalf("b pending ids = %v; want [m1]", ids)
	}

	// Clearing a's queue on backfill completion leaves b's intact.
	if err := db.ClearBackfill(a); err != nil {
		t.Fatalf("ClearBackfill(a): %v", err)
	}
	if n, _ := db.BackfillPendingCount(b); n != 1 {
		t.Fatalf("b pending after a's clear = %d; want 1", n)
	}
}
