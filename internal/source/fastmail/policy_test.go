package fastmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jmap "git.sr.ht/~rockorager/go-jmap"

	"save/internal/policy"
	"save/internal/state"
)

// Attachment-policy behaviour of the FastMail connector.
//
// FastMail is the channel where the allowlist is genuinely load-bearing:
// unlike Gmail and Google Chat, no upstream refusal of executable attachment
// types is confirmed for it, so these tests are the only thing standing
// between a hostile mailbox and the vault.

// --- fixtures -------------------------------------------------------------

type part struct {
	name    string
	ctype   string
	content []byte
}

func rawMultipart(msgID string, parts ...part) string {
	var b strings.Builder
	b.WriteString("From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: Hello World\r\n" +
		"Date: Mon, 04 Aug 2026 10:00:00 +0200\r\n" +
		"Message-Id: <" + msgID + "@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BB\"\r\n\r\n" +
		"--BB\r\nContent-Type: text/plain\r\n\r\nHi with attach.\r\n")
	for _, p := range parts {
		b.WriteString("--BB\r\n" +
			"Content-Type: " + p.ctype + "; name=\"" + p.name + "\"\r\n" +
			"Content-Disposition: attachment; filename=\"" + p.name + "\"\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			wrap76(base64.StdEncoding.EncodeToString(p.content)) + "\r\n")
	}
	b.WriteString("--BB--\r\n")
	return b.String()
}

func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76])
		b.WriteString("\r\n")
		s = s[76:]
	}
	b.WriteString(s)
	return b.String()
}

func pdfBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n%%EOF\n")
}

func exeBytes() []byte {
	return append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 512)...)
}

func archivedNote(t *testing.T, root string) (rel, body string) {
	t.Helper()
	var found string
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".md") {
			found = p
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if found == "" {
		t.Fatalf("no .md archived under %s", root)
	}
	b, err := os.ReadFile(found)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	r, err := filepath.Rel(root, found)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	return filepath.ToSlash(r), string(b)
}

func unresolvedSkips(t *testing.T, db *state.DB) []state.SkippedAttachment {
	t.Helper()
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatalf("list skipped: %v", err)
	}
	return rows
}

// processOne runs one email end to end through the real processEmail path.
func processOne(t *testing.T, s *Source, fake *fakeAPI, id, blob, raw string) {
	t.Helper()
	fake.blobs = map[jmap.ID][]byte{jmap.ID(blob): []byte(raw)}
	if err := s.processEmail(context.Background(), testConn(fake), testBoxes(), testEmail(id, blob, "mb-in")); err != nil {
		t.Fatalf("processEmail: %v", err)
	}
}

// --- tests ----------------------------------------------------------------

// TestDeniedPartStillArchivesNoteAndBody: the message survives its refused
// attachment, and the refusal is on the record with the fetch identity a
// later `save refetch` needs.
func TestDeniedPartStillArchivesNoteAndBody(t *testing.T) {
	s, db, root := newTestSource(t)
	s.freeSpace = func(string) int64 { return 0 } // unknown: floor disabled
	fake := &fakeAPI{t: t}
	processOne(t, s, fake, "e1", "b1", rawMultipart("m1",
		part{name: "report.pdf", ctype: "application/pdf", content: pdfBytes()},
		part{name: "payroll.exe", ctype: "application/octet-stream", content: exeBytes()},
	))

	rel, body := archivedNote(t, root)
	if !strings.Contains(body, "Hi with attach.") {
		t.Errorf("the note lost its body:\n%s", body)
	}
	if !strings.Contains(body, "payroll.exe") {
		t.Errorf("the note does not mention the refusal:\n%s", body)
	}

	var names []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			names = append(names, filepath.Base(p))
		}
		return nil
	})
	for _, n := range names {
		if strings.Contains(n, "payroll") {
			t.Fatalf("the denied attachment reached the archive as %q", n)
		}
	}
	if !contains(names, "report.pdf") {
		t.Errorf("report.pdf was not archived; files = %v", names)
	}

	rows := unresolvedSkips(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Source != testInstance || r.StableID != "e1" || r.PartKey == "" {
		t.Errorf("skip identity = %s/%s/%s", r.Source, r.StableID, r.PartKey)
	}
	if r.Reason != policy.ReasonNotAllowlistedExtension {
		t.Errorf("reason = %q", r.Reason)
	}
	if r.SniffedType == "" {
		t.Error("sniffed type empty: the whole blob was downloaded, so the bytes WERE seen")
	}
	if r.NoteRelPath != rel {
		t.Errorf("note rel path = %q, want %q", r.NoteRelPath, rel)
	}
	if r.PolicyDigest != policy.Default().PolicyDigest() {
		t.Errorf("policy digest = %q", r.PolicyDigest)
	}
	// A policy skip is not an item failure.
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 0 {
		t.Errorf("policy skips reached the failures ledger: %+v", fails)
	}
	// The message itself committed, so it is never re-fetched.
	if seen, _ := db.SeenMessage(testInstance, "e1"); !seen {
		t.Error("the message was not committed: a refused attachment must not cost the message")
	}
}

// TestFreeSpaceFloorKeepsTheNote: on a nearly-full volume the attachment is
// refused and the note is still written. The sync never aborts.
func TestFreeSpaceFloorKeepsTheNote(t *testing.T) {
	set := policy.DefaultSettings()
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	s, db, root := newTestSource(t)
	s.pol = pol
	s.freeSpace = func(string) int64 { return set.FreeSpaceFloor + 8 }

	fake := &fakeAPI{t: t}
	processOne(t, s, fake, "e1", "b1", rawMultipart("m1",
		part{name: "report.pdf", ctype: "application/pdf", content: pdfBytes()}))

	if _, body := archivedNote(t, root); !strings.Contains(body, "Hi with attach.") {
		t.Errorf("the note was not archived with its body:\n%s", body)
	}
	rows := unresolvedSkips(t, db)
	if len(rows) != 1 || rows[0].Reason != policy.ReasonFreeSpaceFloor {
		t.Fatalf("skipped = %+v, want one %s", rows, policy.ReasonFreeSpaceFloor)
	}
}

// TestRunBudgetCutoff: the second message's attachment is refused once the
// pass has spent its budget, and both notes are still archived.
func TestRunBudgetCutoff(t *testing.T) {
	pdf := pdfBytes()
	set := policy.DefaultSettings()
	set.RunBudget = int64(len(pdf)) + 1
	set.FreeSpaceFloor = 0
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	s, db, root := newTestSource(t)
	s.pol = pol
	s.freeSpace = func(string) int64 { return 0 }

	fake := &fakeAPI{t: t}
	processOne(t, s, fake, "e1", "b1", rawMultipart("m1",
		part{name: "a.pdf", ctype: "application/pdf", content: pdf}))
	processOne(t, s, fake, "e2", "b2", rawMultipart("m2",
		part{name: "b.pdf", ctype: "application/pdf", content: pdf}))

	rows := unresolvedSkips(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want exactly one over-budget refusal: %+v", len(rows), rows)
	}
	if rows[0].Reason != policy.ReasonOverRunBudget || rows[0].StableID != "e2" {
		t.Fatalf("skip = %s on %s, want %s on e2", rows[0].Reason, rows[0].StableID, policy.ReasonOverRunBudget)
	}
	if n := countNotes(t, root); n != 2 {
		t.Errorf("archived %d notes, want 2 — the budget limits attachments, never messages", n)
	}
}

// TestRunBudgetResetsPerPass: the budget is per Sync, so a new pass may store
// again. Without the reset a long-running daemon would refuse every
// attachment forever after the first budget-filling pass.
func TestRunBudgetResetsPerPass(t *testing.T) {
	s, _, _ := newTestSource(t)
	s.runBytes = 999
	stop := errors.New("stop before the network")
	s.dial = func(context.Context) (*conn, error) { return nil, stop }
	if err := s.Sync(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("sync error = %v, want the dial sentinel", err)
	}
	if s.runBytes != 0 {
		t.Errorf("runBytes = %d after Sync started, want 0 (the budget is per pass)", s.runBytes)
	}
}

// TestBlobOverMessageCapIsAnItemFailure: an oversized blob must not be
// buffered, and the refusal must be deterministic (recorded once, never
// retried forever as if it were a network hiccup).
func TestBlobOverMessageCapIsAnItemFailure(t *testing.T) {
	set := policy.DefaultSettings()
	set.MaxMessageBytes = 128
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	s, db, root := newTestSource(t)
	s.pol = pol

	fake := &fakeAPI{t: t}
	processOne(t, s, fake, "e1", "b1", rawMultipart("m1",
		part{name: "a.pdf", ctype: "application/pdf", content: bytes.Repeat([]byte("x"), 4096)}))

	if n := countNotes(t, root); n != 0 {
		t.Errorf("archived %d notes; a message that cannot be buffered cannot be parsed", n)
	}
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fails) != 1 || !strings.Contains(fails[0].LastError, "max_message_bytes") {
		t.Fatalf("failures = %+v, want one naming attachments.max_message_bytes", fails)
	}
}

// TestBlobBodyCapHasHeadroom: the transport ceiling must sit above the exact
// message cap, or a legitimate message of exactly max_message_bytes would be
// truncated by the wrapper before the exact check ever ran.
func TestBlobBodyCapHasHeadroom(t *testing.T) {
	if got := blobBodyCap(0); got != 0 {
		t.Errorf("blobBodyCap(0) = %d, want 0 (cap disabled)", got)
	}
	const max = 100 * 1000 * 1000
	if got := blobBodyCap(max); got <= max {
		t.Errorf("blobBodyCap(%d) = %d, want strictly more than the exact cap", max, got)
	}
}

// --- helpers --------------------------------------------------------------

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func countNotes(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".md") {
			n++
		}
		return nil
	})
	return n
}
