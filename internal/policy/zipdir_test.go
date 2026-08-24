package policy

import (
	"bytes"
	"strings"
	"testing"
)

func TestZipEntryNames(t *testing.T) {
	b := zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"word/document.xml", "<w/>"},
		{"word/vbaProject.bin", "macro"},
	})
	got := zipEntryNames(b)
	want := []string{"[Content_Types].xml", "word/document.xml", "word/vbaProject.bin"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("zipEntryNames = %v, want %v", got, want)
	}
}

func TestHasVBAProject(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries [][2]string
		want    bool
	}{
		{"plain docx", [][2]string{{"[Content_Types].xml", "x"}, {"word/document.xml", "x"}}, false},
		{"word macro", [][2]string{{"[Content_Types].xml", "x"}, {"word/vbaProject.bin", "x"}}, true},
		{"excel macro", [][2]string{{"[Content_Types].xml", "x"}, {"xl/vbaProject.bin", "x"}}, true},
		{"powerpoint macro", [][2]string{{"[Content_Types].xml", "x"}, {"ppt/vbaProject.bin", "x"}}, true},
		{"top-level macro", [][2]string{{"vbaProject.bin", "x"}}, true},
		{"case and separator folded", [][2]string{{`WORD\VBAPROJECT.BIN`, "x"}}, true},
		{"leading dot-slash", [][2]string{{"./word/vbaProject.bin", "x"}}, true},
		{"similar name is not a match", [][2]string{{"word/vbaProject.bin.txt", "x"}}, false},
		{"nested deeper is not a match", [][2]string{{"a/word/vbaProject.bin", "x"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasVBAProject(zipOf(t, tc.entries)); got != tc.want {
				t.Errorf("HasVBAProject = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHasVBAProjectIsPermissiveOnGarbage: an unparsable central directory must
// report "no macro found" rather than refusing a real document. The reader is
// also expected never to panic on hostile input.
func TestHasVBAProjectIsPermissiveOnGarbage(t *testing.T) {
	good := zipOf(t, [][2]string{{"word/vbaProject.bin", "x"}})
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"tiny", []byte("PK")},
		{"not a zip", pdfBytes()},
		{"random", randomBytes(4096)},
		{"truncated head", good[:len(good)/2]},
		{"truncated tail", good[len(good)/3:]},
		{"eocd signature in a comment", append(append([]byte{}, good...), []byte("PK\x05\x06junk")...)},
		{"all zeroes", make([]byte, 8192)},
		{"saturated zip64 fields with no locator", saturatedEOCD()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if HasVBAProject(tc.b) {
				t.Error("unparsable input must not be reported as macro-enabled")
			}
			if n := len(zipEntryNames(tc.b)); n != 0 {
				t.Errorf("zipEntryNames returned %d entries for unparsable input", n)
			}
		})
	}
}

// saturatedEOCD is an end-of-central-directory record whose offsets are the
// zip64 escape value, with no zip64 locator behind it.
func saturatedEOCD() []byte {
	b := make([]byte, eocdFixedLen)
	copy(b, []byte{0x50, 0x4b, 0x05, 0x06})
	for i := 12; i < 20; i++ {
		b[i] = 0xFF
	}
	return b
}

// TestZipEntryScanIsBounded proves the walk cannot be turned into an
// unbounded loop by a central directory claiming more entries than it holds.
func TestZipEntryScanIsBounded(t *testing.T) {
	entries := make([][2]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		entries = append(entries, [2]string{"docProps/f" + strings.Repeat("x", i%7) + itoa(i) + ".xml", "y"})
	}
	b := zipOf(t, entries)
	if got := len(zipEntryNames(b)); got != 2000 {
		t.Errorf("zipEntryNames returned %d entries, want 2000", got)
	}
	if maxZipEntriesScanned < 1000 {
		t.Error("the entry cap must leave room for real documents")
	}
}

// TestVBADetectionSurvivesRenaming is the case that motivates the whole
// carve-out: a .docm renamed to .docx is invisible to both the extension deny
// list and content sniffing.
func TestVBADetectionSurvivesRenaming(t *testing.T) {
	content := docmBytes(t)
	if got := Sniff(content); got != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
		t.Fatalf("a .docm must sniff as a plain docx (that is the problem): %q", got)
	}
	p := Default()
	v := p.Decide(Input{Name: "quarterly.docx", Content: content})
	if v.Store || v.Reason != ReasonMacroOffice {
		t.Errorf("Decide = {Store:%v Reason:%q}, want macro_office", v.Store, v.Reason)
	}

	// The check is skipped entirely when the operator opts in.
	s := DefaultSettings()
	s.AllowMacroOffice = true
	allow, err := New(s)
	if err != nil {
		t.Fatal(err)
	}
	if v := allow.Decide(Input{Name: "quarterly.docx", Content: content}); !v.Store {
		t.Errorf("allow_macro_office must keep it: %q", v.Reason)
	}
}

// TestNoInflationHappens guards the carve-out's boundary: the reader looks at
// central-directory headers only, so entry DATA never has to be readable.
// Corrupting every compressed payload must not change the answer.
func TestNoInflationHappens(t *testing.T) {
	b := zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"word/vbaProject.bin", "the payload bytes"},
	})
	// Same length, so every recorded offset still lands where it did: only
	// the entry DATA (and its now-wrong CRC) is destroyed.
	payload := []byte("the payload bytes")
	corrupt := bytes.ReplaceAll(b, payload, bytes.Repeat([]byte{0xFF}, len(payload)))
	if bytes.Equal(corrupt, b) {
		t.Fatal("fixture did not contain the payload to corrupt")
	}
	if !HasVBAProject(corrupt) {
		t.Error("the central-directory scan must not depend on entry data being valid")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
