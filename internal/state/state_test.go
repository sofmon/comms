package state_test

import (
	"path/filepath"
	"testing"

	"comms/internal/state"
)

// openTest opens a fresh store in a temp dir and closes it on cleanup.
func openTest(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	db, err := state.Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := db.SetMeta(state.MetaArchiveTZ, "Europe/Amsterdam"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not re-run migrations or lose data.
	for i := 0; i < 2; i++ {
		db, err = state.Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		got, ok, err := db.GetMeta(state.MetaArchiveTZ)
		if err != nil || !ok || got != "Europe/Amsterdam" {
			t.Fatalf("reopen %d: GetMeta = %q, %v, %v; want Europe/Amsterdam, true, nil", i, got, ok, err)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("reopen %d: Close: %v", i, err)
		}
	}
}

func TestInstanceIDHelpers(t *testing.T) {
	id := state.InstanceID(state.SourceGmail, "work")
	if id != "gmail:work" {
		t.Fatalf("InstanceID = %q; want gmail:work", id)
	}
	if got := state.Tag(id); got != "gmail-work" {
		t.Fatalf("Tag = %q; want gmail-work", got)
	}
	if got := state.KindOf(id); got != state.SourceGmail {
		t.Fatalf("KindOf = %q; want %q", got, state.SourceGmail)
	}
	kind, label, ok := state.SplitInstance(id)
	if !ok || kind != state.SourceGmail || label != "work" {
		t.Fatalf("SplitInstance = %q, %q, %v", kind, label, ok)
	}

	// A hyphenated label survives the round trip; the tag keeps the hyphen.
	hy := state.InstanceID(state.SourceGChat, "acme-corp")
	if _, label, ok := state.SplitInstance(hy); !ok || label != "acme-corp" {
		t.Fatalf("SplitInstance(%q) = %q, %v", hy, label, ok)
	}
	if got := state.Tag(hy); got != "gchat-acme-corp" {
		t.Fatalf("Tag(%q) = %q; want gchat-acme-corp", hy, got)
	}

	// A bare kind constant is NOT an instance id: rows must never be keyed
	// by one.
	for _, bad := range []string{"gmail", "", ":work", "gmail:", "gmail:a:b"} {
		if kind, label, ok := state.SplitInstance(bad); ok {
			t.Fatalf("SplitInstance(%q) = %q, %q, true; want not ok", bad, kind, label)
		}
		if got := state.KindOf(bad); got != "" {
			t.Fatalf("KindOf(%q) = %q; want empty", bad, got)
		}
	}
}

func TestMeta(t *testing.T) {
	db := openTest(t)

	if _, ok, err := db.GetMeta(state.MetaArchiveRoot); err != nil || ok {
		t.Fatalf("GetMeta on empty = ok %v, err %v; want false, nil", ok, err)
	}
	if err := db.SetMeta(state.MetaArchiveRoot, "/tmp/a"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := db.SetMeta(state.MetaArchiveRoot, "/tmp/b"); err != nil {
		t.Fatalf("SetMeta overwrite: %v", err)
	}
	got, ok, err := db.GetMeta(state.MetaArchiveRoot)
	if err != nil || !ok || got != "/tmp/b" {
		t.Fatalf("GetMeta = %q, %v, %v; want /tmp/b, true, nil", got, ok, err)
	}
}

func TestCursorLifecycle(t *testing.T) {
	db := openTest(t)

	if _, ok, err := db.GetCursor("gmail", "", "history_id"); err != nil || ok {
		t.Fatalf("GetCursor on empty = ok %v, err %v; want false, nil", ok, err)
	}

	// Distinct (source, scope, kind) triples must not collide.
	sets := []struct{ source, scope, kind, value string }{
		{"gmail", "", "history_id", "12345"},
		{"gmail", "", "backfill_page_token", "tok-1"},
		{"fastmail", "", "email_state", "s99"},
		{"gchat", "spaces/AAA", "msg_create_time", "2026-08-07T10:00:00Z"},
		{"gchat", "spaces/BBB", "msg_create_time", "2026-08-07T11:00:00Z"},
	}
	for _, s := range sets {
		if err := db.SetCursor(s.source, s.scope, s.kind, s.value); err != nil {
			t.Fatalf("SetCursor(%v): %v", s, err)
		}
	}
	for _, s := range sets {
		got, ok, err := db.GetCursor(s.source, s.scope, s.kind)
		if err != nil || !ok || got != s.value {
			t.Fatalf("GetCursor(%s/%s/%s) = %q, %v, %v; want %q", s.source, s.scope, s.kind, got, ok, err, s.value)
		}
	}

	// Overwrite.
	if err := db.SetCursor("gmail", "", "history_id", "67890"); err != nil {
		t.Fatalf("SetCursor overwrite: %v", err)
	}
	if got, _, _ := db.GetCursor("gmail", "", "history_id"); got != "67890" {
		t.Fatalf("after overwrite: got %q, want 67890", got)
	}

	// Delete only removes the addressed triple.
	if err := db.DeleteCursor("gmail", "", "history_id"); err != nil {
		t.Fatalf("DeleteCursor: %v", err)
	}
	if _, ok, _ := db.GetCursor("gmail", "", "history_id"); ok {
		t.Fatal("cursor still present after delete")
	}
	if _, ok, _ := db.GetCursor("gmail", "", "backfill_page_token"); !ok {
		t.Fatal("sibling cursor vanished after delete")
	}
	if err := db.DeleteCursor("gmail", "", "history_id"); err != nil {
		t.Fatalf("DeleteCursor on absent: %v", err)
	}
}
