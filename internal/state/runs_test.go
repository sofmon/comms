package state_test

import (
	"testing"

	"comms/internal/state"
)

func TestSyncRunsLifecycle(t *testing.T) {
	db := openTest(t)

	id, err := db.StartRun(state.SourceGmail, "backfill")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	runs, err := db.RecentRuns(state.SourceGmail, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RecentRuns = %d, %v; want 1", len(runs), err)
	}
	if r := runs[0]; r.ID != id || r.Finished || r.Kind != "backfill" || r.StartedAt.IsZero() {
		t.Fatalf("unfinished run = %+v", r)
	}

	stats := state.RunStats{New: 10, Updated: 2, AttsDone: 3, AttsFailed: 1}
	if err := db.FinishRun(id, false, stats, "quota exhausted"); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	runs, _ = db.RecentRuns(state.SourceGmail, 10)
	r := runs[0]
	if !r.Finished || r.OK || r.FinishedAt.IsZero() || r.Stats != stats || r.Error != "quota exhausted" {
		t.Fatalf("finished run = %+v", r)
	}

	if err := db.FinishRun(9999, true, state.RunStats{}, ""); err == nil {
		t.Fatal("FinishRun on unknown id did not error")
	}
}

func TestPruneRuns(t *testing.T) {
	db := openTest(t)

	var gmailIDs []int64
	for i := 0; i < 5; i++ {
		id, err := db.StartRun(state.SourceGmail, "incremental")
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		if err := db.FinishRun(id, true, state.RunStats{}, ""); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}
		gmailIDs = append(gmailIDs, id)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.StartRun(state.SourceGChat, "incremental"); err != nil {
			t.Fatalf("StartRun gchat: %v", err)
		}
	}

	if err := db.PruneRuns(2); err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	// Pruning keeps each source's newest runs independently.
	gmail, _ := db.RecentRuns(state.SourceGmail, 10)
	if len(gmail) != 2 {
		t.Fatalf("gmail runs after prune = %d; want 2", len(gmail))
	}
	if gmail[0].ID != gmailIDs[4] || gmail[1].ID != gmailIDs[3] {
		t.Fatalf("prune kept wrong gmail runs: %d, %d; want %d, %d",
			gmail[0].ID, gmail[1].ID, gmailIDs[4], gmailIDs[3])
	}
	gchat, _ := db.RecentRuns(state.SourceGChat, 10)
	if len(gchat) != 2 {
		t.Fatalf("gchat runs after prune = %d; want 2", len(gchat))
	}
}

func TestFailuresThreshold(t *testing.T) {
	db := openTest(t)

	for i := 1; i <= state.SkipThreshold; i++ {
		n, err := db.RecordFailure(state.SourceGmail, "msg-1", "parse exploded")
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
		if n != i {
			t.Fatalf("RecordFailure returned %d; want %d", n, i)
		}
		skipped, err := db.IsSkipped(state.SourceGmail, "msg-1")
		if err != nil {
			t.Fatalf("IsSkipped: %v", err)
		}
		if want := i >= state.SkipThreshold; skipped != want {
			t.Fatalf("after %d failures IsSkipped = %v; want %v", i, skipped, want)
		}
	}

	// Unknown items are never skipped.
	if skipped, _ := db.IsSkipped(state.SourceGmail, "msg-2"); skipped {
		t.Fatal("unknown id reported skipped")
	}
	// Same id under another source is tracked separately.
	if n, _ := db.RecordFailure(state.SourceFastmail, "msg-1", "x"); n != 1 {
		t.Fatalf("cross-source attempts = %d; want 1", n)
	}

	failures, err := db.ListFailures()
	if err != nil || len(failures) != 2 {
		t.Fatalf("ListFailures = %d, %v; want 2", len(failures), err)
	}
	if failures[0].Source != state.SourceFastmail || failures[1].Source != state.SourceGmail {
		t.Fatalf("ListFailures order = %+v", failures)
	}
	if failures[1].Attempts != state.SkipThreshold || failures[1].LastError != "parse exploded" || failures[1].LastAt.IsZero() {
		t.Fatalf("gmail failure row = %+v", failures[1])
	}

	if err := db.ClearFailure(state.SourceGmail, "msg-1"); err != nil {
		t.Fatalf("ClearFailure: %v", err)
	}
	if skipped, _ := db.IsSkipped(state.SourceGmail, "msg-1"); skipped {
		t.Fatal("still skipped after ClearFailure")
	}
	if failures, _ := db.ListFailures(); len(failures) != 1 {
		t.Fatalf("failures after clear = %d; want 1", len(failures))
	}
	// Clearing an absent row is a no-op.
	if err := db.ClearFailure(state.SourceGmail, "msg-1"); err != nil {
		t.Fatalf("ClearFailure absent: %v", err)
	}
}
