package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comms/internal/emailpipe"
	"comms/internal/naming"
	"comms/internal/policy"
)

// refetchFixture is the same message before and after a policy widening: the
// attachment that was refused as over-cap is now stored, exactly what
// `comms refetch` produces when it re-renders with the new policy.
func refetchFixture(t *testing.T) (before, after *emailpipe.EmailDoc, meta EmailMeta, stem string) {
	t.Helper()
	server := time.Date(2026, 8, 7, 12, 32, 5, 0, time.UTC)
	stem = naming.EmailStem(server.In(tzAms), "gmail-work", "Deck", naming.Hash8("gmail:work:deck1"))
	ad := naming.AttachDir(stem)

	part := emailpipe.File{
		Name: "keynote.mp4", OrigName: "keynote.mp4",
		DeclaredType: "video/mp4", PartKey: "2", Size: 12,
		SniffedType: "video/mp4",
	}
	skipped := part
	skipped.Skipped = true
	skipped.SkipReason = policy.ReasonOverSizeCap
	skipped.SkipDetail = "12 bytes exceeds the cap"

	stored := part
	stored.Rel = ad + "/keynote.mp4"
	stored.Content = []byte("mp4-content!")

	before = &emailpipe.EmailDoc{
		Subject: "Deck", MessageID: "<deck@example.com>", BodyMD: "Slides.\n",
		PolicyDigest: "fb70e01d2e46670e",
		Skipped:      []emailpipe.File{skipped},
	}
	after = &emailpipe.EmailDoc{
		Subject: "Deck", MessageID: "<deck@example.com>", BodyMD: "Slides.\n",
		PolicyDigest: "0123456789abcdef", // the widened policy
		Files:        []emailpipe.File{stored},
		StoredBytes:  12,
	}
	meta = EmailMeta{
		Source: "gmail:work", SourceTag: "gmail-work", Account: "u@example.com",
		AccountLabel: "work", StableID: "deck1", ServerTime: server,
		SkipDisposition: SkipBytesDiscarded,
	}
	return before, after, meta, stem
}

// TestRewriteEmailTurnsSkipIntoLink is the retro-fetch path end to end: the
// refetched bytes land on disk, the skip record disappears from both the
// frontmatter and the visible section, a real link replaces it, and the
// returned hash changes so the caller can update messages.content_hash.
func TestRewriteEmailTurnsSkipIntoLink(t *testing.T) {
	before, after, meta, stem := refetchFixture(t)
	w := &Writer{Root: t.TempDir(), TZ: tzAms}

	rel, hash1, err := w.WriteEmail(before, meta)
	if err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}
	first := readNote(t, w, rel)
	if !strings.Contains(first, "skipped_attachments:") || !strings.Contains(first, "**Not stored") {
		t.Fatalf("the first note does not record the skip:\n%s", first)
	}
	attPath := filepath.Join(w.Root, "2026/08/07", stem+".d", "keynote.mp4")
	if _, err := os.Stat(attPath); !os.IsNotExist(err) {
		t.Fatalf("the skipped attachment exists before the refetch (err=%v)", err)
	}

	rel2, hash2, err := w.RewriteEmail(after, meta)
	if err != nil {
		t.Fatalf("RewriteEmail: %v", err)
	}
	if rel2 != rel {
		t.Errorf("re-render moved the note: %q -> %q", rel, rel2)
	}
	if hash2 == hash1 {
		t.Error("the content hash did not change, so the caller would keep a stale messages.content_hash")
	}

	second := readNote(t, w, rel2)
	if strings.Contains(second, "skipped_attachments:") || strings.Contains(second, "**Not stored") {
		t.Errorf("the re-rendered note still records the skip:\n%s", second)
	}
	if !strings.Contains(second, "- [keynote.mp4](<"+stem+".d/keynote.mp4>)") {
		t.Errorf("the re-rendered note does not link the refetched file:\n%s", second)
	}
	if !strings.Contains(second, "attachment_policy_digest: 0123456789abcdef") {
		t.Errorf("the re-rendered note kept the old policy digest:\n%s", second)
	}
	// Files-first: the bytes are on disk, at 0600, and the hash the caller
	// commits describes the note that is actually there.
	got, err := os.ReadFile(attPath)
	if err != nil {
		t.Fatalf("refetched attachment: %v", err)
	}
	if string(got) != "mp4-content!" {
		t.Errorf("refetched attachment content = %q", got)
	}
	if mode := fileMode(t, attPath); mode != 0o600 {
		t.Errorf("refetched attachment mode = %o, want 0600", mode)
	}
	if hash2 != hashBytes([]byte(second)) {
		t.Error("the returned hash does not match the bytes on disk")
	}
	if litter := dotFiles(t, w.Root); len(litter) != 0 {
		t.Errorf("temp litter: %v", litter)
	}
}

// TestRewriteEmailRefusesToCreate: a refetch that has drifted onto a path
// with no note must fail loudly rather than fork a second copy of a message.
func TestRewriteEmailRefusesToCreate(t *testing.T) {
	_, after, meta, stem := refetchFixture(t)
	w := &Writer{Root: t.TempDir(), TZ: tzAms}

	_, _, err := w.RewriteEmail(after, meta)
	if !errors.Is(err, ErrNoteMissing) {
		t.Fatalf("RewriteEmail on a missing note = %v, want ErrNoteMissing", err)
	}
	// And it wrote nothing at all — not the note, not the attachment whose
	// bytes it was handed.
	for _, rel := range []string{
		"2026/08/07/" + stem + ".md",
		"2026/08/07/" + stem + ".d/keynote.mp4",
	} {
		if _, err := os.Stat(filepath.Join(w.Root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s exists after a refused re-render (err=%v)", rel, err)
		}
	}

	// A directory sitting where the note belongs is not a note either.
	if err := os.MkdirAll(filepath.Join(w.Root, "2026/08/07", stem+".md"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.RewriteEmail(after, meta); !errors.Is(err, ErrNoteMissing) {
		t.Errorf("RewriteEmail over a directory = %v, want ErrNoteMissing", err)
	}
}

// TestRewriteEmailIsDeterministic: re-rendering twice is a no-op, so a
// refetch that is interrupted and retried converges instead of churning the
// note's hash.
func TestRewriteEmailIsDeterministic(t *testing.T) {
	before, after, meta, _ := refetchFixture(t)
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	if _, _, err := w.WriteEmail(before, meta); err != nil {
		t.Fatal(err)
	}
	_, hashA, err := w.RewriteEmail(after, meta)
	if err != nil {
		t.Fatal(err)
	}
	_, hashB, err := w.RewriteEmail(after, meta)
	if err != nil {
		t.Fatal(err)
	}
	if hashA != hashB {
		t.Errorf("re-render is not idempotent: %q vs %q", hashA, hashB)
	}
}

// TestRewriteEmailKeepsRemainingSkips: a partial widening still leaves an
// honest record for whatever is still refused.
func TestRewriteEmailKeepsRemainingSkips(t *testing.T) {
	before, after, meta, _ := refetchFixture(t)
	still := before.Skipped[0]
	still.Name, still.PartKey = "payload.exe", "3"
	still.SkipReason = policy.ReasonNotAllowlistedExtension
	still.SkipDetail = `extension "exe" is not allowlisted`
	before.Skipped = append(before.Skipped, still)
	after.Skipped = []emailpipe.File{still}

	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel, _, err := w.WriteEmail(before, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.RewriteEmail(after, meta); err != nil {
		t.Fatal(err)
	}
	note := readNote(t, w, rel)
	if !strings.Contains(note, "payload.exe") || !strings.Contains(note, "not_allowlisted_extension") {
		t.Errorf("the still-refused part vanished from the note:\n%s", note)
	}
	if strings.Contains(note, "keynote.mp4**") {
		t.Errorf("the refetched part is still listed as skipped:\n%s", note)
	}
	if !strings.Contains(note, "- [keynote.mp4](<") {
		t.Errorf("the refetched part is not linked:\n%s", note)
	}
}

func readNote(t *testing.T, w *Writer, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
