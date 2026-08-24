package policy

import (
	"strings"
	"testing"
)

func TestNormalizeExt(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"invoice.pdf", "pdf"},
		{"INVOICE.PDF", "pdf"},
		{"Invoice.PdF", "pdf"},

		// Only the LAST extension counts — this is the whole point.
		{"invoice.pdf.exe", "exe"},
		{"invoice.pdf.zip", "zip"},
		{"a.b.c.d.docx", "docx"},

		// Trailing dots and spaces are ignored by the OS, so they cannot be
		// allowed to hide the real extension.
		{"payload.exe.", "exe"},
		{"payload.exe ", "exe"},
		{"payload.exe. . ", "exe"},
		{"payload.exe ", "exe"}, // non-breaking space
		{"report.pdf...", "pdf"},

		// No extension.
		{"", ""},
		{"README", ""},
		{".bashrc", ""},
		{".", ""},
		{"..", ""},
		{"name.", ""},
		{"dir/name", ""},

		// A separator smuggled into the tail is not an extension.
		{`weird.pd/f`, ""},
		{`weird.pd\f`, ""},

		// Unicode: NFC first, then a full case fold.
		{"CAFÉ.PDF", "pdf"},
		{"file.PDF́", "pdf́"}, // folded, still not on any allowlist
		{"file.ÄÖÜ", "äöü"},

		// Absurdly long tails are not extensions.
		{"x." + strings.Repeat("a", extMaxBytes+1), ""},
		{"x." + strings.Repeat("a", extMaxBytes), strings.Repeat("a", extMaxBytes)},
	} {
		if got := NormalizeExt(tc.in); got != tc.want {
			t.Errorf("NormalizeExt(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeExtIsIdempotent(t *testing.T) {
	for _, in := range []string{"invoice.pdf.exe", "A.PDF", "x.docx", "payload.exe. "} {
		ext := NormalizeExt(in)
		if again := NormalizeExt("f." + ext); again != ext {
			t.Errorf("NormalizeExt is not idempotent for %q: %q -> %q", in, ext, again)
		}
	}
}

func TestParseAllowEntry(t *testing.T) {
	for _, tc := range []struct {
		in        string
		wantExt   string
		wantTypes []string
	}{
		{"pdf", "pdf", nil},
		{".PDF", "pdf", nil},
		{"  pdf  ", "pdf", nil},
		{"7z=application/x-7z-compressed", "7z", []string{"application/x-7z-compressed"}},
		{"7z = application/x-7z-compressed , application/zip", "7z",
			[]string{"application/x-7z-compressed", "application/zip"}},
		{"7z=application/zip|application/x-7z-compressed", "7z",
			[]string{"application/zip", "application/x-7z-compressed"}},
		{"P7S=Application/PKCS7-Signature", "p7s", []string{"application/pkcs7-signature"}},
	} {
		ext, types, err := parseAllowEntry(tc.in)
		if err != nil {
			t.Errorf("parseAllowEntry(%q): %v", tc.in, err)
			continue
		}
		if ext != tc.wantExt {
			t.Errorf("parseAllowEntry(%q) ext = %q, want %q", tc.in, ext, tc.wantExt)
		}
		if strings.Join(types, ",") != strings.Join(tc.wantTypes, ",") {
			t.Errorf("parseAllowEntry(%q) types = %v, want %v", tc.in, types, tc.wantTypes)
		}
	}

	for _, bad := range []string{"", "  ", "tar.gz", "pd f", "-pdf", "7z=", "7z=nope", "7z=a/b/c", "=application/zip",
		strings.Repeat("a", extMaxBytes+1)} {
		if _, _, err := parseAllowEntry(bad); err == nil {
			t.Errorf("parseAllowEntry(%q) should have failed", bad)
		}
	}
}
