package naming

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"
)

func TestSlug(t *testing.T) {
	cases := []struct {
		in, fallback, want string
	}{
		{"Quarterly Report", "no-subject", "quarterly-report"},
		{"Re: Invoice #448 / July", "no-subject", "re-invoice-448-july"},
		{"", "no-subject", "no-subject"},
		{"!!! ???", "no-subject", "no-subject"},
		{"  spaces   and\ttabs  ", "x", "spaces-and-tabs"},
		{"under_scores.and.dots", "x", "under-scores-and-dots"},
		{"CON", "x", "con-x"},
		{"ЕЛЕКТРОННА ПОЩА", "untitled", "untitled"}, // non-ASCII dropped entirely
		{"café münchen", "x", "caf-mnchen"},
		{strings.Repeat("a", 100), "x", strings.Repeat("a", 60)},
	}
	for _, c := range cases {
		if got := Slug(c.in, c.fallback); got != c.want {
			t.Errorf("Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugNormalizationStable(t *testing.T) {
	nfc := "café"                  // composed
	nfd := norm.NFD.String("café") // decomposed
	if Slug(nfc, "x") != Slug(nfd, "x") {
		t.Errorf("NFC and NFD forms must slug identically")
	}
}

func TestSlugTruncationRuneBoundary(t *testing.T) {
	// 60-byte cut must not split a multi-byte rune anywhere in the pipeline.
	in := strings.Repeat("a", 59) + "bcd"
	got := Slug(in, "x")
	if len(got) > 60 {
		t.Errorf("slug exceeds 60 bytes: %d", len(got))
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in, fallback, want string
	}{
		{"invoice.pdf", "f", "invoice.pdf"},
		{"weird/na:me*?.pdf", "f", "weird_na_me__.pdf"},
		{"trailing. . .", "f", "trailing"},
		{"", "attachment-3.bin", "attachment-3.bin"},
		{"...", "attachment-1.bin", "attachment-1.bin"},
		{"CON.txt", "f", "CON-x.txt"},
		{"Report.PDF", "f", "Report.PDF"}, // case preserved
	}
	for _, c := range cases {
		if got := SanitizeFilename(c.in, c.fallback); got != c.want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeFilenamePreservesExtensionOnTruncate(t *testing.T) {
	in := strings.Repeat("x", 200) + ".pdf"
	got := SanitizeFilename(in, "f")
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("extension lost: %q", got)
	}
	if len(got) > 100 {
		t.Errorf("name exceeds 100 bytes: %d", len(got))
	}
}

func TestUniqueCasefoldCollisions(t *testing.T) {
	taken := map[string]bool{}
	a := Unique(taken, "Report.PDF")
	b := Unique(taken, "report.pdf") // APFS collision with a
	c := Unique(taken, "report.pdf")
	if a != "Report.PDF" {
		t.Errorf("first name mangled: %q", a)
	}
	if b != "report_2.pdf" {
		t.Errorf("casefold collision not suffixed: %q", b)
	}
	if c != "report_3.pdf" {
		t.Errorf("third collision wrong: %q", c)
	}
}

func TestUniqueNormalizationCollision(t *testing.T) {
	taken := map[string]bool{}
	Unique(taken, "café.txt")
	got := Unique(taken, norm.NFD.String("café.txt"))
	if !strings.HasPrefix(got, "cafe") && !strings.HasPrefix(got, "café_2") {
		t.Errorf("NFD form did not collide with NFC form: %q", got)
	}
}

func TestStems(t *testing.T) {
	ts := time.Date(2026, 8, 7, 14, 32, 5, 0, time.UTC)
	if got := DayDir(ts); got != "2026/08/07" {
		t.Errorf("DayDir = %q", got)
	}
	if got := DayBucket(ts); got != "2026-08-07" {
		t.Errorf("DayBucket = %q", got)
	}
	stem := EmailStem(ts, "gmail-work", "Re: Invoice July", "a1b2c3d4")
	if stem != "143205_gmail-work_re-invoice-july_a1b2c3d4" {
		t.Errorf("EmailStem = %q", stem)
	}
	if got := AttachDir(stem); got != stem+".d" {
		t.Errorf("AttachDir = %q", got)
	}
	cs := ChatStem("gchat-work", "space", "team-platform", "9f8e7d6c")
	if cs != "gchat-work_space_team-platform_9f8e7d6c" {
		t.Errorf("ChatStem = %q", cs)
	}
	an := ChatAttachmentName(ts, "ab12cd34", "rollout-plan.pdf")
	if an != "143205_ab12cd34_rollout-plan.pdf" {
		t.Errorf("ChatAttachmentName = %q", an)
	}
}

// Account labels may contain hyphens, so the file tags built from them
// ("gmail-acme-corp") do too. The stem separator is '_', which a tag can
// never contain, so every stem field stays recoverable and two different
// (tag, slug) splits can never produce the same stem.
func TestStemsWithHyphenatedTagRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 7, 14, 32, 5, 0, time.UTC)
	const tag = "gmail-acme-corp"

	stem := EmailStem(ts, tag, "Q3 report: draft", "a1b2c3d4")
	parts := strings.SplitN(stem, "_", 3)
	if len(parts) != 3 || parts[0] != "143205" || parts[1] != tag {
		t.Fatalf("EmailStem %q does not split back into time/tag/rest: %q", stem, parts)
	}
	if rest := parts[2]; rest != "q3-report-draft_a1b2c3d4" {
		t.Fatalf("EmailStem tail = %q; want slug_hash", rest)
	}

	const chatTag = "gchat-acme-corp"
	cs := ChatStem(chatTag, "space", "team-platform", "9f8e7d6c")
	cparts := strings.SplitN(cs, "_", 2)
	if len(cparts) != 2 || cparts[0] != chatTag {
		t.Fatalf("ChatStem %q does not yield the tag back: %q", cs, cparts)
	}
	if cs != chatTag+"_space_team-platform_9f8e7d6c" {
		t.Fatalf("ChatStem = %q", cs)
	}

	// Ambiguity check: moving a hyphenated segment from the tag into the
	// slug must produce a different filename, not the same one.
	if a, b := ChatStem("gchat-a", "space", "b-c", "9f8e7d6c"), ChatStem("gchat-a-b", "space", "c", "9f8e7d6c"); a == b {
		t.Fatalf("hyphenated tag/slug split is ambiguous: both render %q", a)
	}

	// Tags never contain the instance-id separator: a stem must be usable as
	// a filename component as-is.
	for _, s := range []string{stem, cs} {
		if strings.ContainsAny(s, `:/\`) {
			t.Fatalf("stem %q contains a character illegal in a filename", s)
		}
	}
}

func TestHash8Deterministic(t *testing.T) {
	first := Hash8("gmail:abc")
	if again := Hash8("gmail:abc"); again != first {
		t.Fatal("hash not deterministic")
	}
	if len(Hash8("x")) != 8 {
		t.Fatalf("hash length: %q", Hash8("x"))
	}
	if Hash8("gmail:abc") == Hash8("fastmail:abc") {
		t.Fatal("source prefix must change hash")
	}
}
