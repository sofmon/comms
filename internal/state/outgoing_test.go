package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutgoingLifecycleAndRecoveryList(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	prepared := time.Date(2026, 9, 4, 9, 30, 0, 0, time.UTC)
	row, err := db.PrepareOutgoing(Outgoing{
		MessageKey: "abc", Source: "gmail:work", Kind: "email",
		DraftRelPath: "hello.md", ContentHash: "deadbeef", PreparedAt: prepared,
	})
	if err != nil || row.Status != OutgoingPending || !row.PreparedAt.Equal(prepared) {
		t.Fatalf("PrepareOutgoing = %+v, %v", row, err)
	}
	if err := db.BeginOutgoingAttempt("abc"); err != nil {
		t.Fatal(err)
	}
	if err := db.FailOutgoing("abc", "lost response"); err != nil {
		t.Fatal(err)
	}
	row, _, err = db.GetOutgoing("abc")
	if err != nil || row.Status != OutgoingSending || row.Attempts != 1 || row.LastError != "lost response" {
		t.Fatalf("after failure = %+v, %v", row, err)
	}

	sentAt := prepared.Add(time.Minute)
	if err := db.BeginOutgoingAttempt("abc"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkOutgoingSent("abc", "remote-1", "2026/09/04/hello_abc.md", sentAt); err != nil {
		t.Fatal(err)
	}
	pending, err := db.UnarchivedOutgoing()
	if err != nil || len(pending) != 1 || pending[0].ProviderID != "remote-1" {
		t.Fatalf("UnarchivedOutgoing = %+v, %v", pending, err)
	}
	if err := db.MarkOutgoingArchived("abc"); err != nil {
		t.Fatal(err)
	}
	pending, err = db.UnarchivedOutgoing()
	if err != nil || len(pending) != 0 {
		t.Fatalf("after archive = %+v, %v", pending, err)
	}
	row, ok, err := db.GetOutgoing("abc")
	if err != nil || !ok || row.Status != OutgoingArchived || row.ArchivedAt.IsZero() || row.Attempts != 2 {
		t.Fatalf("final row = %+v, ok=%v err=%v", row, ok, err)
	}
}

func TestOutgoingRejectsBadIdentityAndTransitions(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.PrepareOutgoing(Outgoing{MessageKey: "x", Source: "gmail", Kind: "email", DraftRelPath: "x.md", ContentHash: "h"}); err == nil || !strings.Contains(err.Error(), "instance id") {
		t.Fatalf("bad source error = %v", err)
	}
	if _, err := db.PrepareOutgoing(Outgoing{MessageKey: "x", Source: "gmail:work", Kind: "sms", DraftRelPath: "x.md", ContentHash: "h"}); err == nil || !strings.Contains(err.Error(), "invalid kind") {
		t.Fatalf("bad kind error = %v", err)
	}
	if err := db.MarkOutgoingArchived("missing"); err == nil {
		t.Fatal("archived a missing row")
	}
}
