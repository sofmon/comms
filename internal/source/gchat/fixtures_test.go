package gchat

import (
	"archive/zip"
	"bytes"
	"testing"
)

// Real magic bytes for the connector tests.
//
// The attachment policy sniffs content, so a fixture that merely CLAIMS to be
// a .docx (bytes "DOCX-bytes") is correctly refused as an
// extension/content mismatch. Tests that are about something else — Drive
// mirroring, download retries — must therefore carry plausible bytes, or they
// end up asserting the policy's behaviour by accident.
//
// archive/zip appears in test files only, which keeps the never-decompress
// invariant ("no non-test file imports archive/zip") literally true.

func jpegBytes() []byte {
	return append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0}, 64)...)
}

func pdfBytes() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\ntrailer\n%%EOF\n")
}

func textBytes() []byte { return []byte("hello world\nthis is plain text\n") }

// exeBytes is a Windows PE header: hard-denied by extension, and the content
// half of a .pdf/.jpg mismatch.
func exeBytes() []byte {
	return append([]byte("MZ\x90\x00"), bytes.Repeat([]byte{0}, 128)...)
}

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

func docxBytes(t *testing.T) []byte {
	t.Helper()
	return zipOf(t, [][2]string{
		{"[Content_Types].xml", "<Types/>"},
		{"word/document.xml", "<w:document/>"},
	})
}
