package archive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comms/internal/emailpipe"
	"comms/internal/naming"
	"comms/internal/policy"
)

// skipEmailFixture is a message whose attachments exercise every branch of
// the skip renderer at once: a clean store, a store under protest, a refusal
// with sniffed content, a refusal whose bytes were never inspected, and a
// transient refusal that comes back on its own.
func skipEmailFixture() (*emailpipe.EmailDoc, EmailMeta, string) {
	server := time.Date(2026, 8, 7, 12, 32, 5, 0, time.UTC) // 15:32:05 in Sofia
	stem := naming.EmailStem(server.In(tzAms), "gmail-work", "Q3 pack", naming.Hash8("gmail:work:skip1"))
	ad := naming.AttachDir(stem)
	doc := &emailpipe.EmailDoc{
		Subject:      "Q3 pack",
		MessageID:    "<pack@example.com>",
		From:         []string{"Ann <ann@example.com>"},
		BodyMD:       "See attached.\n",
		PolicyDigest: "fb70e01d2e46670e",
		Files: []emailpipe.File{
			{
				Rel: ad + "/invoice.pdf", Content: []byte("pdf-bytes"),
				Name: "invoice.pdf", OrigName: "invoice.pdf",
				DeclaredType: "application/pdf", PartKey: "2", Size: 9,
				SniffedType: "application/pdf",
			},
			{
				Rel: ad + "/notes.txt", Content: []byte("txt"),
				Name: "notes.txt", OrigName: "notes.txt",
				DeclaredType: "text/plain", PartKey: "3", Size: 3,
				SniffedType: "application/zip",
				Warned:      true,
				SkipReason:  policy.ReasonExtensionContentMismatch,
				SkipDetail:  `extension "txt" does not permit application/zip`,
			},
		},
		Skipped: []emailpipe.File{
			{
				Name: "budget.docm", OrigName: "budget.docm",
				DeclaredType: "application/vnd.ms-word.document.macroEnabled.12",
				PartKey:      "4", Size: 24576,
				SniffedType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
				Skipped:     true,
				SkipReason:  policy.ReasonMacroOffice,
				SkipDetail:  "macro-enabled Office documents are denied by extension",
			},
			{
				Name: "keynote.mp4", OrigName: "Q3 all-hands `final`.mp4",
				DeclaredType: "video/mp4", PartKey: "5", Size: 89128960,
				SniffedType: "video/mp4",
				Skipped:     true,
				SkipReason:  policy.ReasonOverSizeCap,
				SkipDetail:  "89128960 bytes exceeds the 50000000 byte cap",
			},
			{
				Name: "roster.csv", OrigName: "",
				DeclaredType: "", PartKey: "6", Size: 512,
				SniffedType: "",
				Skipped:     true,
				SkipReason:  policy.ReasonOverRunBudget,
				SkipDetail:  "this pass already stored 2000000000 bytes",
			},
		},
		Warnings: []string{"part 7: unknown charset"},
	}
	meta := EmailMeta{
		Source:          "gmail:work",
		SourceTag:       "gmail-work",
		Account:         "user@example.com",
		AccountLabel:    "work",
		StableID:        "skip1",
		ThreadID:        "t9",
		ServerTime:      server,
		SkipDisposition: SkipBytesDiscarded,
	}
	return doc, meta, stem
}

// TestWriteEmailGoldenSkipped pins the exact bytes of a note that carries
// both stored and refused attachments. Refusals must be visible twice — as a
// machine-readable frontmatter list and as a bolded block under
// "## Attachments" — so neither a tool nor a reader can miss one.
func TestWriteEmailGoldenSkipped(t *testing.T) {
	doc, meta, stem := skipEmailFixture()
	want := strings.ReplaceAll(`---
source: gmail:work
type: email
render_version: 2
account: user@example.com
account_label: work
message_id: <pack@example.com>
thread_id: t9
date: ""
date_utc: "2026-08-07T12:32:05Z"
from:
    - Ann <ann@example.com>
to: []
cc: []
subject: Q3 pack
labels: []
attachments:
    - STEM.d/invoice.pdf
    - STEM.d/notes.txt
skipped_attachments:
    - name: budget.docm
      orig_name: budget.docm
      part_key: "4"
      size: 24576
      declared_type: application/vnd.ms-word.document.macroEnabled.12
      declared_ext: docm
      sniffed_type: application/vnd.openxmlformats-officedocument.wordprocessingml.document
      reason: macro_office
      detail: macro-enabled Office documents are denied by extension
      disposition: discarded
      recoverable: true
    - name: keynote.mp4
      orig_name: Q3 all-hands `+"`final`"+`.mp4
      part_key: "5"
      size: 89128960
      declared_type: video/mp4
      declared_ext: mp4
      sniffed_type: video/mp4
      reason: over_size_cap
      detail: 89128960 bytes exceeds the 50000000 byte cap
      disposition: discarded
      recoverable: true
    - name: roster.csv
      part_key: "6"
      size: 512
      declared_ext: csv
      reason: over_run_budget
      detail: this pass already stored 2000000000 bytes
      disposition: discarded
      recoverable: true
attachment_policy_digest: fb70e01d2e46670e
warnings:
    - 'part 7: unknown charset'
---

See attached.

## Attachments

- [invoice.pdf](<STEM.d/invoice.pdf>)
- [notes.txt](<STEM.d/notes.txt>) — **stored despite a policy objection**: its file extension and the content type detected from its bytes disagree (`+"`extension_content_mismatch`"+`) — extension "txt" does not permit application/zip
- **Not stored — `+"`budget.docm`"+`** — 24576 bytes (24.6 kB), declared `+"`application/vnd.ms-word.document.macroEnabled.12`"+`, detected `+"`application/vnd.openxmlformats-officedocument.wordprocessingml.document`"+`, part `+"`4`"+`
  - Reason: macro-enabled Office documents are refused by extension (`+"`macro_office`"+`) — macro-enabled Office documents are denied by extension
  - The bytes arrived with the message and were discarded rather than written to disk. Widen the `+"`[attachments]`"+` policy and run `+"`comms refetch`"+` to retrieve it.
- **Not stored — `+"`keynote.mp4`"+`** (sent as `+"``Q3 all-hands `final`.mp4``"+`) — 89128960 bytes (89.1 MB), declared `+"`video/mp4`"+`, detected `+"`video/mp4`"+`, part `+"`5`"+`
  - Reason: it is larger than the per-attachment size cap (`+"`over_size_cap`"+`) — 89128960 bytes exceeds the 50000000 byte cap
  - The bytes arrived with the message and were discarded rather than written to disk. Widen the `+"`[attachments]`"+` policy and run `+"`comms refetch`"+` to retrieve it.
- **Not stored — `+"`roster.csv`"+`** — 512 bytes, declared `+"`(no type)`"+`, content not inspected, part `+"`6`"+`
  - Reason: this sync pass had already used up its attachment budget (`+"`over_run_budget`"+`) — this pass already stored 2000000000 bytes
  - The bytes arrived with the message and were discarded rather than written to disk. `+"`comms refetch`"+` retries this on a later run without any config change.
`, "STEM", stem)

	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatalf("WriteEmail: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("note mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Refused parts are records, not files: nothing may exist on disk for
	// them, and nothing may link to a zero-length path.
	for _, f := range doc.Skipped {
		p := filepath.Join(w.Root, "2026/08/07", stem+".d", f.Name)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("skipped attachment %s was written (err=%v)", f.Name, err)
		}
	}
	if bytes.Contains(got, []byte("](<>)")) {
		t.Error("note contains a link to an empty destination")
	}
}

// TestRenderEmailDeterministic: two renders of the same document must produce
// identical bytes, skips and all — the archive's replay-is-a-no-op guarantee
// covers the new frontmatter list and the new visible entries too.
func TestRenderEmailDeterministic(t *testing.T) {
	doc, meta, _ := skipEmailFixture()
	first, err := renderEmailMD(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderEmailMD(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("renders differ:\n%s\n---\n%s", first, second)
	}

	// And through the writer, which is what the connectors call.
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel1, hash1, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	rel2, hash2, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	if rel1 != rel2 || hash1 != hash2 {
		t.Errorf("re-write not stable: (%q,%q) vs (%q,%q)", rel1, hash1, rel2, hash2)
	}
	if litter := dotFiles(t, w.Root); len(litter) != 0 {
		t.Errorf("temp litter after writes: %v", litter)
	}
}

// TestWriteEmailRejectsUnknownDisposition: the disposition decides whether the
// note claims the bytes were transferred, so an unrecognized value is a
// connector bug and must not reach a reader as a plausible-looking sentence.
func TestWriteEmailRejectsUnknownDisposition(t *testing.T) {
	doc, meta, _ := skipEmailFixture()
	meta.SkipDisposition = SkipDisposition("maybe")
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	if _, _, err := w.WriteEmail(doc, meta); err == nil {
		t.Fatal("WriteEmail accepted an unknown skip disposition")
	}
}

// TestSkipDispositionWording: each source's sentence must match what actually
// happened to the bytes. Getting this wrong is not a cosmetic bug — it tells
// the reader the wrong thing about whether their data crossed the network.
func TestSkipDispositionWording(t *testing.T) {
	tests := []struct {
		disp     SkipDisposition
		want     string
		mustNot  string
		wantJSON string
	}{
		{SkipBytesDiscarded, "arrived with the message and were discarded", "never downloaded", "discarded"},
		{SkipBytesNotFetched, "never downloaded", "arrived with the message", "not_fetched"},
		{SkipDispositionUnspecified, "No copy was kept.", "downloaded", ""},
	}
	for _, tt := range tests {
		t.Run(string(tt.disp)+"|", func(t *testing.T) {
			doc, meta, _ := skipEmailFixture()
			meta.SkipDisposition = tt.disp
			got, err := renderEmailMD(doc, meta)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), tt.want) {
				t.Errorf("note does not say %q:\n%s", tt.want, got)
			}
			if strings.Contains(string(got), tt.mustNot) {
				t.Errorf("note wrongly says %q:\n%s", tt.mustNot, got)
			}
			if !strings.Contains(string(got), "disposition: "+quoteYAML(tt.wantJSON)) {
				t.Errorf("frontmatter disposition is not %q:\n%s", tt.wantJSON, got)
			}
		})
	}
}

func quoteYAML(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// TestSkippedRowsMatchNote: the DB ledger and the note are two views of one
// refusal and must agree, because a skip recorded in only one of them is
// half a silent drop.
func TestSkippedRowsMatchNote(t *testing.T) {
	doc, meta, _ := skipEmailFixture()
	w := &Writer{Root: t.TempDir(), TZ: tzAms}
	rel, _, err := w.WriteEmail(doc, meta)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := w.SkippedRows(doc, meta, rel)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(doc.Skipped) {
		t.Fatalf("got %d rows for %d skips", len(rows), len(doc.Skipped))
	}
	note, err := os.ReadFile(filepath.Join(w.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		f := doc.Skipped[i]
		switch {
		case r.Source != meta.Source, r.StableID != meta.StableID:
			t.Errorf("row %d has the wrong identity: %+v", i, r)
		case r.PartKey != f.PartKey || r.SanitizedName != f.Name || r.SizeBytes != f.Size:
			t.Errorf("row %d does not describe the part: %+v", i, r)
		case r.Reason != f.SkipReason || r.SniffedType != f.SniffedType:
			t.Errorf("row %d verdict mismatch: %+v", i, r)
		case r.NoteRelPath != rel:
			t.Errorf("row %d points at %q, not the note %q", i, r.NoteRelPath, rel)
		case r.DayBucket != "2026-08-07":
			t.Errorf("row %d day bucket = %q", i, r.DayBucket)
		case r.PolicyDigest != doc.PolicyDigest:
			t.Errorf("row %d policy digest = %q", i, r.PolicyDigest)
		}
		// The extension the row records is the one the policy keyed on: the
		// final sanitized name's, not the sender's original.
		if want := policy.NormalizeExt(f.Name); r.DeclaredExt != want {
			t.Errorf("row %d declared ext = %q, want %q", i, r.DeclaredExt, want)
		}
		if !strings.Contains(string(note), f.Name) {
			t.Errorf("note does not mention skipped %q", f.Name)
		}
	}

	// A skip with no note to live in is a silent drop; refuse to build rows.
	if _, err := w.SkippedRows(doc, meta, ""); err == nil {
		t.Error("SkippedRows accepted an empty note path")
	}
	// No skips, no rows.
	clean, cleanMeta, _ := fullEmailFixture(t)
	if rows, err := w.SkippedRows(clean, cleanMeta, "x.md"); err != nil || rows != nil {
		t.Errorf("SkippedRows on a clean doc = %v, %v", rows, err)
	}
}

// TestSanitizedNameStaysAllowlisted guards the interaction between the iCloud
// sync-exclusion sanitizer and the policy: SanitizeFilename may append ".bin"
// to an excluded name, which would silently change the extension the note and
// the ledger report. Whatever it produces, the two must key on the same
// string.
func TestSanitizedNameStaysAllowlisted(t *testing.T) {
	for _, orig := range []string{"invoice.pdf", "photo.png", "notes.txt", "deck.pptx"} {
		final := naming.SanitizeFilename(orig, "")
		if got, want := policy.NormalizeExt(final), policy.NormalizeExt(orig); got != want {
			t.Errorf("sanitize(%q) = %q: extension changed %q -> %q", orig, final, want, got)
		}
		if types := policy.Default().PermittedTypes(policy.NormalizeExt(final)); len(types) == 0 {
			t.Errorf("sanitize(%q) = %q is no longer allowlisted", orig, final)
		}
	}
}

// TestMdCodeCannotEscape: filenames come from senders, so a name full of
// backticks must not be able to break out of its code span and forge note
// structure.
func TestMdCodeCannotEscape(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain.pdf", "`plain.pdf`"},
		{"a`b.pdf", "``a`b.pdf``"},
		{"``x``.pdf", "``` ``x``.pdf ```"},
		{"`.pdf", "`` `.pdf ``"},
		{"line\nbreak.pdf", "`line break.pdf`"},
		{"", "(unnamed)"},
	}
	for _, tt := range tests {
		if got := mdCode(tt.in); got != tt.want {
			t.Errorf("mdCode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSizeText(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 bytes"},
		{999, "999 bytes"},
		{1000, "1000 bytes (1.0 kB)"},
		{24576, "24576 bytes (24.6 kB)"},
		{52428800, "52428800 bytes (52.4 MB)"},
		{-1, "-1 bytes"},
	}
	for _, tt := range tests {
		if got := sizeText(tt.n); got != tt.want {
			t.Errorf("sizeText(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// TestReasonEnglishCoversEveryReason: a reason with no sentence would render
// as the generic fallback, quietly telling the reader less than the archiver
// knows. Every policy reason must have its own words.
func TestReasonEnglishCoversEveryReason(t *testing.T) {
	generic := reasonEnglish("some-reason-that-does-not-exist")
	for _, r := range policy.Reasons() {
		if got := reasonEnglish(r); got == generic {
			t.Errorf("reason %q has no sentence of its own", r)
		}
	}
}
