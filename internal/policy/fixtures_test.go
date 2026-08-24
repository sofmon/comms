package policy

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// Test fixtures.
//
// These build real magic bytes rather than mocking the sniffer: the whole
// point of the policy is that it agrees with what mimetype actually reports
// for real files, so a mocked sniffer would test nothing. archive/zip appears
// here and ONLY here — a test file — which keeps the never-decompress
// invariant ("no non-test file imports archive/zip") literally true.

func pdfBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n%%EOF\n")
}

func pngBytes() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 64)...)
}

func jpegBytes() []byte {
	return append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0}, 64)...)
}

func gifBytes() []byte {
	return []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00,")
}

func svgBytes() []byte {
	return []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"></svg>`)
}

func textBytes() []byte { return []byte("hello world\nthis is plain text\n") }

func csvBytes() []byte { return []byte("a,b,c\n1,2,3\n4,5,6\n") }

func jsonBytes() []byte { return []byte(`{"a":1,"b":[2,3]}`) }

func xmlBytes() []byte { return []byte(`<?xml version="1.0"?><invoice><id>7</id></invoice>`) }

func rtfBytes() []byte { return []byte(`{\rtf1\ansi hello}`) }

func icsBytes() []byte { return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n") }

func vcfBytes() []byte {
	return []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:A B\r\nEND:VCARD\r\n")
}

func emlBytes() []byte {
	return []byte("From: a@b.c\r\nTo: d@e.f\r\nSubject: hi\r\n" +
		"Date: Mon, 1 Jan 2024 00:00:00 +0000\r\nMessage-ID: <1@x>\r\n\r\nbody\r\n")
}

// oleBytes is the compound-file header shared by .doc, .xls, .ppt and .msg;
// mimetype stops at application/x-ole-storage and returns an EMPTY extension
// for all four, which is exactly why the extension is the discriminator.
func oleBytes() []byte {
	return append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, bytes.Repeat([]byte{0}, 1024)...)
}

func exeBytes() []byte {
	return append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 128)...)
}

// randomBytes matches no signature: mimetype reports application/octet-stream.
func randomBytes(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + 0x80) // never valid UTF-8 text, no known magic
	}
	return out
}

// zipOf builds a zip with the given entries, stored (never deflated) so entry
// offsets are predictable.
func zipOf(t *testing.T, entries [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		f, err := w.CreateHeader(&zip.FileHeader{Name: e[0], Method: zip.Store})
		if err != nil {
			t.Fatalf("zip entry %q: %v", e[0], err)
		}
		if _, err := f.Write([]byte(e[1])); err != nil {
			t.Fatalf("zip write %q: %v", e[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func plainZipBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{{"notes.txt", "hello"}})
}

func docxBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"word/document.xml", "<w:document/>"},
	})
}

func xlsxBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"xl/workbook.xml", "<workbook/>"},
	})
}

func pptxBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"ppt/presentation.xml", "<p/>"},
	})
}

// docmBytes is a macro-enabled Word document: byte-for-byte a docx as far as
// mimetype is concerned, distinguished only by word/vbaProject.bin in the
// central directory.
func docmBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"word/document.xml", "<w:document/>"},
		{"word/vbaProject.bin", "\x00\x01macro payload"},
	})
}

// bigDocxBytes is the regression fixture for the sniff limit: the word/ entry
// sits well past mimetype's 4096-byte default, so without SetLimit(0) this
// real .docx sniffs as application/zip and the allowlist would refuse it.
func bigDocxBytes(t *testing.T) []byte {
	t.Helper()
	filler := strings.Repeat("<Override PartName=\"/x\" ContentType=\"y\"/>\n", 200) // ~8 KB
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types>" + filler + "</Types>"},
		{"word/document.xml", "<w:document/>"},
	})
}

// odfBytes builds an OpenDocument/EPUB style zip: the uncompressed "mimetype"
// entry at offset 30 is what mimetype keys on.
func odfBytes(t *testing.T, mime string) []byte {
	t.Helper()
	return zipOf(t, [][2]string{{"mimetype", mime}, {"content.xml", "<x/>"}})
}
