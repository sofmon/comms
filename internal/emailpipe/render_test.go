package emailpipe

import (
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"save/internal/naming"
	"save/internal/policy"
)

var update = flag.Bool("update", false, "rewrite golden files")

const attachDir = "143205_gmail_test_a1b2c3d4.d"

func render(t *testing.T, fixture string) *EmailDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := Render(raw, attachDir)
	if err != nil {
		t.Fatalf("Render(%s): %v", fixture, err)
	}
	return doc
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run `go test -update` once): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("body mismatch with %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func rels(doc *EmailDoc) []string {
	out := make([]string, len(doc.Files))
	for i, f := range doc.Files {
		out[i] = f.Rel
	}
	return out
}

func wantRels(t *testing.T, doc *EmailDoc, want ...string) {
	t.Helper()
	got := rels(doc)
	if len(got) != len(want) {
		t.Fatalf("Files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Files[%d].Rel = %q, want %q", i, got[i], want[i])
		}
	}
}

func hasWarning(doc *EmailDoc, substr string) bool {
	for _, w := range doc.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestRenderPlain(t *testing.T) {
	doc := render(t, "plain.eml")

	if doc.Subject != "Quarterly numbers" {
		t.Errorf("Subject = %q", doc.Subject)
	}
	if doc.MessageID != "<20260807143205.12345@example.com>" {
		t.Errorf("MessageID = %q (angle brackets must be kept)", doc.MessageID)
	}
	if want := []string{"José Pérez <jose@example.com>"}; !equal(doc.From, want) {
		t.Errorf("From = %v, want %v", doc.From, want)
	}
	if want := []string{"Ana Ruiz <ana@example.com>", "bob@example.com"}; !equal(doc.To, want) {
		t.Errorf("To = %v, want %v", doc.To, want)
	}
	if want := []string{"Carol <carol@example.com>"}; !equal(doc.Cc, want) {
		t.Errorf("Cc = %v, want %v", doc.Cc, want)
	}
	wantDate := time.Date(2026, 8, 7, 14, 32, 5, 0, time.FixedZone("", 3*3600))
	if !doc.Date.Equal(wantDate) {
		t.Errorf("Date = %v, want %v", doc.Date, wantDate)
	}
	if _, off := doc.Date.Zone(); off != 3*3600 {
		t.Errorf("Date offset = %d, want +0300 preserved", off)
	}
	if !strings.Contains(doc.BodyMD, "the Q3 numbers are attached") {
		t.Errorf("BodyMD = %q", doc.BodyMD)
	}
	wantRels(t, doc) // no files
	if len(doc.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", doc.Warnings)
	}
}

func TestRenderTable(t *testing.T) {
	doc := render(t, "table.eml")

	wantRels(t, doc)
	for _, must := range []string{"| Region | Revenue |", "|--------|", "| North", "~~flat~~", "**up**"} {
		if !strings.Contains(doc.BodyMD, must) {
			t.Errorf("BodyMD missing %q:\n%s", must, doc.BodyMD)
		}
	}
	for _, banned := range []string{"hotpink", "alert(", "Weekly</title>"} {
		if strings.Contains(doc.BodyMD, banned) {
			t.Errorf("BodyMD leaked style/script/head content %q:\n%s", banned, doc.BodyMD)
		}
	}
	checkGolden(t, "table.md.golden", doc.BodyMD)
}

func TestRenderRelatedCID(t *testing.T) {
	doc := render(t, "related_cid.eml")

	// The PNG lives in OtherParts (no Content-Disposition) and has no
	// filename: it must still be saved, with a sniffed extension.
	wantRels(t, doc, attachDir+"/attachment-1.png")
	if !strings.HasPrefix(string(doc.Files[0].Content), "\x89PNG") {
		t.Errorf("attachment content is not the decoded PNG: %q", doc.Files[0].Content[:8])
	}
	if !strings.Contains(doc.BodyMD, "("+attachDir+"/attachment-1.png)") {
		t.Errorf("BodyMD does not link the inline image:\n%s", doc.BodyMD)
	}
	if strings.Contains(doc.BodyMD, "cid:") {
		t.Errorf("BodyMD still contains a cid: reference:\n%s", doc.BodyMD)
	}
	if !strings.Contains(doc.BodyMD, "Ghost logo") {
		t.Errorf("unresolvable cid: image did not degrade to alt text:\n%s", doc.BodyMD)
	}
	if !hasWarning(doc, "cid:ghost@nowhere") {
		t.Errorf("missing unresolved-cid warning, got %v", doc.Warnings)
	}
	checkGolden(t, "related_cid.md.golden", doc.BodyMD)
}

// TestRenderSecondInlineHTMLRecorded: enmime renders only the FIRST unnamed
// inline text/html part into Envelope.HTML; later ones would otherwise
// appear nowhere. They are still walked and still put to the policy — and
// .html is not on the allowlist, so the never-drop rule is satisfied by an
// honest skip record rather than by a file. Widening is one config line:
// attachments.allow_extensions = ["html=text/html"].
func TestRenderSecondInlineHTMLRecorded(t *testing.T) {
	doc := render(t, "multi_html.eml")

	wantRels(t, doc, attachDir+"/pic.png")
	f := onlySkipped(t, doc)
	if f.Name != "attachment-2.html" {
		t.Errorf("skipped Name = %q, want attachment-2.html", f.Name)
	}
	if f.SkipReason != policy.ReasonNotAllowlistedExtension {
		t.Errorf("SkipReason = %q", f.SkipReason)
	}
	if f.SniffedType != "text/html" {
		t.Errorf("SniffedType = %q, want text/html — a skip record must still be honest", f.SniffedType)
	}
	if f.Size == 0 || f.Rel != "" || f.Content != nil {
		t.Errorf("skipped file = %+v, want Size>0, no Rel, no Content", f)
	}
	if !strings.Contains(doc.BodyMD, "this is the body") {
		t.Errorf("BodyMD lost the first html part:\n%s", doc.BodyMD)
	}
	if strings.Contains(doc.BodyMD, "Second section") {
		t.Errorf("BodyMD duplicated the second html part:\n%s", doc.BodyMD)
	}
	checkGolden(t, "multi_html.md.golden", doc.BodyMD)
}

// TestRenderUndispositionedSecondHTMLRecorded: with no Content-Disposition
// at all, a second text/html part lands in none of enmime's part slices
// (Attachments/Inlines/OtherParts) — the rescue walk must still reach it, so
// that it is at least recorded rather than vanishing.
func TestRenderUndispositionedSecondHTMLRecorded(t *testing.T) {
	raw := "From: a@example.com\r\n" +
		"To: b@example.com\r\n" +
		"Subject: Two bare html parts\r\n" +
		"Date: Fri, 07 Aug 2026 14:32:05 +0300\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"bb\"\r\n" +
		"\r\n" +
		"--bb\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>the body part</p>\r\n" +
		"--bb\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>the dropped part</p>\r\n" +
		"--bb--\r\n"
	doc, err := Render([]byte(raw), attachDir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	wantRels(t, doc)
	f := onlySkipped(t, doc)
	if f.Name != "attachment-1.html" || f.SkipReason != policy.ReasonNotAllowlistedExtension {
		t.Errorf("skipped = %q/%q, want attachment-1.html/not_allowlisted_extension", f.Name, f.SkipReason)
	}
	if f.PartKey != "2" {
		t.Errorf("PartKey = %q, want the dotted MIME index \"2\"", f.PartKey)
	}
	if !strings.Contains(doc.BodyMD, "the body part") {
		t.Errorf("BodyMD = %q, want the first html part", doc.BodyMD)
	}
	if !hasWarning(doc, "NOT stored: not_allowlisted_extension") {
		t.Errorf("skip is invisible in Warnings: %v", doc.Warnings)
	}
}

func TestRenderDupNames(t *testing.T) {
	doc := render(t, "dup_names.eml")

	wantRels(t, doc, attachDir+"/invoice.pdf", attachDir+"/invoice_2.pdf")
	if got := string(doc.Files[0].Content); got != "%PDF-1.4\n% fake first invoice\n" {
		t.Errorf("Files[0] content = %q", got)
	}
	if got := string(doc.Files[1].Content); got != "%PDF-1.4\n% fake second invoice\n" {
		t.Errorf("Files[1] content = %q", got)
	}
}

func TestRenderMissingName(t *testing.T) {
	doc := render(t, "missing_name.eml")

	// application/octet-stream attachment without a filename: the extension
	// comes from content sniffing (it is really a PNG).
	wantRels(t, doc, attachDir+"/attachment-1.png")
}

func TestRenderCP1251(t *testing.T) {
	doc := render(t, "cp1251.eml")

	if doc.Subject != "Привет мир" {
		t.Errorf("Subject = %q, want decoded windows-1251", doc.Subject)
	}
	if want := []string{"Жанна Иванова <zhanna@example.ru>"}; !equal(doc.From, want) {
		t.Errorf("From = %v, want %v", doc.From, want)
	}
	for _, must := range []string{"Привет, мир!", "Это тест."} {
		if !strings.Contains(doc.BodyMD, must) {
			t.Errorf("BodyMD missing %q:\n%s", must, doc.BodyMD)
		}
	}
	wantRels(t, doc)
}

func TestRenderBrokenMIME(t *testing.T) {
	doc := render(t, "broken.eml")

	// Broken transfer encoding must surface as a warning, never drop the
	// attachment or fail the render.
	if len(doc.Warnings) == 0 {
		t.Error("expected warnings for malformed base64, got none")
	}
	wantRels(t, doc, attachDir+"/damaged.pdf")
	if len(doc.Files[0].Content) == 0 {
		t.Error("damaged attachment decoded to nothing")
	}
	if !strings.Contains(doc.BodyMD, "Body before the damaged attachment.") {
		t.Errorf("BodyMD = %q", doc.BodyMD)
	}
}

func TestRenderDataURI(t *testing.T) {
	doc := render(t, "data_uri.eml")

	wantRels(t, doc, attachDir+"/inline-1.gif")
	wantGIF, _ := base64.StdEncoding.DecodeString(
		"R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")
	if string(doc.Files[0].Content) != string(wantGIF) {
		t.Errorf("extracted gif bytes differ: got %d bytes", len(doc.Files[0].Content))
	}
	if !strings.Contains(doc.BodyMD, "("+attachDir+"/inline-1.gif)") {
		t.Errorf("BodyMD does not link the extracted image:\n%s", doc.BodyMD)
	}
	if strings.Contains(doc.BodyMD, "data:") {
		t.Errorf("BodyMD still embeds a data: URI:\n%s", doc.BodyMD)
	}
	checkGolden(t, "data_uri.md.golden", doc.BodyMD)
}

func TestRenderCaseCollision(t *testing.T) {
	doc := render(t, "case_collision.eml")

	// APFS is case-insensitive: Report.PDF and report.pdf collide, so the
	// second gets a deterministic _2 suffix.
	wantRels(t, doc, attachDir+"/Report.PDF", attachDir+"/report_2.pdf")
}

func TestRenderHugeHTMLFallsBackToText(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: big@example.com\r\n")
	b.WriteString("To: you@example.com\r\n")
	b.WriteString("Subject: Huge\r\n")
	b.WriteString("Date: Thu, 07 Aug 2026 00:00:00 +0000\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"b\"\r\n\r\n")
	b.WriteString("--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("tiny text alternative\r\n")
	b.WriteString("--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n")
	b.WriteString("<html><body>")
	row := strings.Repeat("x", 1023) + "\n"
	for b.Len() < maxHTMLBytes+64*1024 {
		b.WriteString("<p>" + row + "</p>")
	}
	b.WriteString("</body></html>\r\n--b--\r\n")

	doc, err := Render([]byte(b.String()), attachDir)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if doc.BodyMD != "tiny text alternative" {
		t.Errorf("BodyMD = %q, want the text part", truncateForLog(doc.BodyMD))
	}
	if !hasWarning(doc, "used the text part instead") {
		t.Errorf("missing oversized-html warning, got %v", doc.Warnings)
	}
}

// TestRenderCorpus sweeps every fixture: Render must succeed, every File.Rel
// must live under the attachment dir, no two Rels may collide on a
// case/normalization-insensitive filesystem, and every skipped part must be
// a complete, honest record.
func TestRenderCorpus(t *testing.T) {
	wantFiles := map[string]int{
		"plain.eml":          0,
		"table.eml":          0,
		"related_cid.eml":    1,
		"dup_names.eml":      2,
		"missing_name.eml":   1,
		"cp1251.eml":         0,
		"broken.eml":         1,
		"data_uri.eml":       1,
		"case_collision.eml": 2,
		"multi_html.eml":     1, // the second html part is recorded, not stored
	}
	wantSkipped := map[string]int{"multi_html.eml": 1}
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	seenFixtures := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".eml") {
			continue
		}
		seenFixtures++
		t.Run(e.Name(), func(t *testing.T) {
			doc := render(t, e.Name())
			want, known := wantFiles[e.Name()]
			if !known {
				t.Fatalf("fixture %s missing from wantFiles map", e.Name())
			}
			if len(doc.Files) != want {
				t.Errorf("Files count = %d, want %d (%v)", len(doc.Files), want, rels(doc))
			}
			if got := len(doc.Skipped); got != wantSkipped[e.Name()] {
				t.Errorf("Skipped count = %d, want %d", got, wantSkipped[e.Name()])
			}
			checkSkipRecords(t, doc)
			keys := make(map[string]bool)
			for _, f := range doc.Files {
				if !strings.HasPrefix(f.Rel, attachDir+"/") {
					t.Errorf("File.Rel %q does not start with attach dir", f.Rel)
				}
				base := strings.TrimPrefix(f.Rel, attachDir+"/")
				if strings.Contains(base, "/") {
					t.Errorf("File.Rel %q nests below the attach dir", f.Rel)
				}
				key := naming.CollisionKey(base)
				if keys[key] {
					t.Errorf("colliding attachment name %q", f.Rel)
				}
				keys[key] = true
			}
		})
	}
	if seenFixtures != len(wantFiles) {
		t.Errorf("found %d .eml fixtures, wantFiles covers %d", seenFixtures, len(wantFiles))
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
