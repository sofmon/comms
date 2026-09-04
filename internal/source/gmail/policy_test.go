package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gmailv1 "google.golang.org/api/gmail/v1"

	"comms/internal/policy"
	"comms/internal/state"
)

// Attachment-policy behaviour of the Gmail connector.
//
// The invariant every test here defends is the same one: a refused attachment
// is REFUSED, not lost. The message still archives, the note still says what
// arrived, and the state DB still holds enough identity to fetch the bytes
// again if the operator widens the policy later.

// --- fixtures -------------------------------------------------------------

type part struct {
	name    string
	ctype   string
	content []byte
}

// rawMultipart builds a real multipart/mixed message: a text body plus one
// base64 attachment per part. The bytes matter — the policy sniffs them.
func rawMultipart(subject string, parts ...part) string {
	var b strings.Builder
	b.WriteString("From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Date: Fri, 07 Aug 2026 11:30:00 +0200\r\n" +
		"Message-ID: <" + subject + "@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BOUND\"\r\n\r\n" +
		"--BOUND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Body of " + subject + ".\r\n")
	for _, p := range parts {
		b.WriteString("--BOUND\r\n" +
			"Content-Type: " + p.ctype + "; name=\"" + p.name + "\"\r\n" +
			"Content-Disposition: attachment; filename=\"" + p.name + "\"\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			wrap76(base64.StdEncoding.EncodeToString(p.content)) + "\r\n")
	}
	b.WriteString("--BOUND--\r\n")
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

func msgWithParts(id, subject string, parts ...part) *gmailv1.Message {
	return &gmailv1.Message{
		Id:           id,
		ThreadId:     "t-" + id,
		InternalDate: testTime.UnixMilli(),
		LabelIds:     []string{"INBOX"},
		Raw:          base64.RawURLEncoding.EncodeToString([]byte(rawMultipart(subject, parts...))),
	}
}

func pdfBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n%%EOF\n")
}

func exeBytes() []byte {
	return append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 512)...)
}

// syncOnce drives one full backfill pass.
func syncOnce(t *testing.T, s *Source) {
	t.Helper()
	if err := s.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func fakeFor(msgs ...*gmailv1.Message) *fakeAPI {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.Id
	}
	return &fakeAPI{
		profileFn: func() (*gmailv1.Profile, error) {
			return &gmailv1.Profile{EmailAddress: "work@example.com", HistoryId: 100}, nil
		},
		listFn: func(string) (*gmailv1.ListMessagesResponse, error) {
			return &gmailv1.ListMessagesResponse{Messages: listIDs(ids...)}, nil
		},
		getFn:     msgSet(msgs...),
		historyFn: emptyHistory(100),
	}
}

func skippedRows(t *testing.T, db *state.DB) []state.SkippedAttachment {
	t.Helper()
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatalf("list skipped: %v", err)
	}
	return rows
}

// noteBody reads the single archived .md under root.
func noteBody(t *testing.T, root string) (rel, body string) {
	t.Helper()
	var found string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".md") {
			found = p
		}
		return nil
	})
	if err != nil {
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

// --- tests ----------------------------------------------------------------

// TestDeniedPartStillArchivesNoteAndBody is the headline guarantee: one
// refused attachment must not cost the message. The note, the body and the
// allowlisted sibling attachment all land; only the denied bytes do not.
func TestDeniedPartStillArchivesNoteAndBody(t *testing.T) {
	msg := msgWithParts("m1", "quarterly",
		part{name: "report.pdf", ctype: "application/pdf", content: pdfBytes()},
		part{name: "payroll.exe", ctype: "application/octet-stream", content: exeBytes()},
	)
	api := fakeFor(msg)
	s, db, root := newTestSource(t, api, testAccount("work@example.com"))
	syncOnce(t, s)

	rel, body := noteBody(t, root)
	if !strings.Contains(body, "Body of quarterly.") {
		t.Errorf("note lost the body:\n%s", body)
	}

	// The allowlisted sibling is on disk; the denied one is not, anywhere.
	var names []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			names = append(names, filepath.Base(p))
		}
		return nil
	})
	if !slicesContains(names, "report.pdf") {
		t.Errorf("report.pdf was not archived; files = %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "payroll") {
			t.Errorf("the denied attachment reached the archive as %q", n)
		}
	}

	// It is recorded, with everything a retro-fetch needs.
	rows := skippedRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Source != testSourceID || r.StableID != "m1" || r.PartKey == "" {
		t.Errorf("skip identity = %s/%s/%s", r.Source, r.StableID, r.PartKey)
	}
	if r.Reason != policy.ReasonNotAllowlistedExtension {
		t.Errorf("reason = %q, want %q", r.Reason, policy.ReasonNotAllowlistedExtension)
	}
	if r.OrigName != "payroll.exe" || r.DeclaredExt != "exe" {
		t.Errorf("orig name %q / declared ext %q", r.OrigName, r.DeclaredExt)
	}
	if r.SniffedType == "" {
		t.Error("sniffed type is empty: Gmail downloads the whole message, so the bytes WERE seen")
	}
	if r.SizeBytes != int64(len(exeBytes())) {
		t.Errorf("size = %d, want %d", r.SizeBytes, len(exeBytes()))
	}
	if r.NoteRelPath != rel {
		t.Errorf("note rel path = %q, want the archived note %q", r.NoteRelPath, rel)
	}
	if r.DayBucket != "2026-08-07" {
		t.Errorf("day bucket = %q", r.DayBucket)
	}
	if r.PolicyDigest != policy.Default().PolicyDigest() {
		t.Errorf("policy digest = %q, want the default policy's %q", r.PolicyDigest, policy.Default().PolicyDigest())
	}

	// The note names it too, so the DB and the file agree.
	if !strings.Contains(body, "payroll.exe") {
		t.Errorf("the note does not mention the refused attachment:\n%s", body)
	}

	// A policy skip is not a failure and must not hold anything back.
	assertNoFailures(t, db)
	if _, ok := cursor(t, db, cursorHistory); !ok {
		t.Error("history cursor did not advance: a policy skip must never hold a cursor")
	}
}

// TestFreeSpaceFloorKeepsTheNote pins the degradation the plan requires: on a
// nearly-full volume the attachment is refused and the (tiny) note is still
// written. Archiving the mail must never stop because of the blobs.
func TestFreeSpaceFloorKeepsTheNote(t *testing.T) {
	msg := msgWithParts("m1", "invoice",
		part{name: "report.pdf", ctype: "application/pdf", content: pdfBytes()})
	api := fakeFor(msg)

	set := policy.DefaultSettings()
	set.FreeSpaceFloor = 5 * 1000 * 1000 * 1000
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	db, root := newStore(t)
	// Just over the floor: writing even a few dozen bytes would cross it.
	s := newTestSourceIn(t, db, root, api, testAccount("work@example.com"),
		WithPolicy(pol), WithFreeSpace(func(string) int64 { return set.FreeSpaceFloor + 8 }))
	syncOnce(t, s)

	_, body := noteBody(t, root)
	if !strings.Contains(body, "Body of invoice.") {
		t.Errorf("the note was not archived with its body:\n%s", body)
	}
	rows := skippedRows(t, db)
	if len(rows) != 1 || rows[0].Reason != policy.ReasonFreeSpaceFloor {
		t.Fatalf("skipped rows = %+v, want one %s", rows, policy.ReasonFreeSpaceFloor)
	}
	if !policy.Transient(rows[0].Reason) {
		t.Error("free_space_floor must be a transient reason so refetch retries it once space is back")
	}
	assertNoFailures(t, db)
}

// TestFreeSpaceUnknownDoesNotBlock: a probe that cannot answer (every non-mac
// platform, or a failing statfs) must disable the floor, not refuse
// everything.
func TestFreeSpaceUnknownDoesNotBlock(t *testing.T) {
	msg := msgWithParts("m1", "invoice",
		part{name: "report.pdf", ctype: "application/pdf", content: pdfBytes()})
	api := fakeFor(msg)
	db, root := newStore(t)
	s := newTestSourceIn(t, db, root, api, testAccount("work@example.com"),
		WithFreeSpace(func(string) int64 { return 0 }))
	syncOnce(t, s)

	if rows := skippedRows(t, db); len(rows) != 0 {
		t.Fatalf("unknown free space refused an attachment: %+v", rows)
	}
}

// TestRunBudgetCutoff: once a pass has stored attachments.run_budget bytes it
// stops storing more, records why, and keeps archiving notes. The reason is
// transient, so the next pass picks the bytes up.
func TestRunBudgetCutoff(t *testing.T) {
	pdf := pdfBytes()
	set := policy.DefaultSettings()
	set.RunBudget = int64(len(pdf)) + 1 // room for exactly one
	set.FreeSpaceFloor = 0
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	m1 := msgWithParts("m1", "first", part{name: "a.pdf", ctype: "application/pdf", content: pdf})
	m2 := msgWithParts("m2", "second", part{name: "b.pdf", ctype: "application/pdf", content: pdf})
	api := fakeFor(m1, m2)
	db, root := newStore(t)
	s := newTestSourceIn(t, db, root, api, testAccount("work@example.com"), WithPolicy(pol))
	// One worker keeps the ordering deterministic: with parallel fetchers
	// either message could be the one that wins the budget.
	syncOneWorker(t, s)

	rows := skippedRows(t, db)
	if len(rows) != 1 {
		t.Fatalf("skipped rows = %d, want exactly one over-budget refusal: %+v", len(rows), rows)
	}
	if rows[0].Reason != policy.ReasonOverRunBudget {
		t.Fatalf("reason = %q, want %q", rows[0].Reason, policy.ReasonOverRunBudget)
	}
	if !policy.Transient(rows[0].Reason) {
		t.Error("over_run_budget must be transient: the next pass has a fresh budget")
	}
	if n := countMD(t, root); n != 2 {
		t.Errorf("archived %d notes, want 2 — the budget limits attachments, never messages", n)
	}
	assertNoFailures(t, db)
}

// TestMaxMessageBytesRefusesOversizedRaw: the raw-buffer cap is enforced on
// the DECODED message, and an over-cap message is an item failure (there is
// no note to hang an attachment skip off) rather than a silent drop.
func TestMaxMessageBytesRefusesOversizedRaw(t *testing.T) {
	set := policy.DefaultSettings()
	set.MaxMessageBytes = 256
	pol, err := policy.New(set)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	msg := msgWithParts("m1", "huge",
		part{name: "a.pdf", ctype: "application/pdf", content: bytes.Repeat([]byte("x"), 4096)})
	api := fakeFor(msg)
	db, root := newStore(t)
	s := newTestSourceIn(t, db, root, api, testAccount("work@example.com"), WithPolicy(pol))
	syncOnce(t, s)

	if n := countMD(t, root); n != 0 {
		t.Errorf("archived %d notes; an unparseable-because-unbuffered message must not produce one", n)
	}
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatalf("list failures: %v", err)
	}
	if len(fails) != 1 || !strings.Contains(fails[0].LastError, "max_message_bytes") {
		t.Fatalf("failures = %+v, want one naming attachments.max_message_bytes", fails)
	}
}

// TestRawBodyCap documents the headroom between the operator's message cap
// and the transport ceiling: base64 in a JSON envelope is bigger than the
// message, so capping the body at exactly max_message_bytes would truncate
// legitimate mail.
func TestRawBodyCap(t *testing.T) {
	if got := rawBodyCap(0); got != 0 {
		t.Errorf("rawBodyCap(0) = %d, want 0 (cap disabled)", got)
	}
	const max = 100 * 1000 * 1000
	got := rawBodyCap(max)
	if got <= max*4/3 {
		t.Errorf("rawBodyCap(%d) = %d, too tight for base64-in-JSON (needs > 4/3)", max, got)
	}
}

// --- helpers --------------------------------------------------------------

func newStore(t *testing.T) (*state.DB, string) {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, t.TempDir()
}

// syncOneWorker drains a backfill with a single fetcher so that per-message
// ordering is deterministic.
func syncOneWorker(t *testing.T, s *Source) {
	t.Helper()
	labels, err := s.loadLabels(context.Background())
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	if err := s.enumerate(context.Background()); err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	stats := newRunStats()
	ids, err := s.db.NextPendingBackfill(s.id, 100)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	for _, id := range ids {
		if err := s.processQueued(context.Background(), id, labels, stats); err != nil {
			t.Fatalf("process %s: %v", id, err)
		}
	}
}

func assertNoFailures(t *testing.T, db *state.DB) {
	t.Helper()
	fails, err := db.ListFailures()
	if err != nil {
		t.Fatalf("list failures: %v", err)
	}
	if len(fails) != 0 {
		t.Errorf("policy skips reached the failures ledger: %+v", fails)
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
