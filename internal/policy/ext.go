package policy

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// extMaxBytes bounds a configurable extension token. Anything longer is not an
// extension anybody attaches; the bound keeps the allowlist a lookup table
// rather than a pattern language.
const extMaxBytes = 24

// NormalizeExt returns the extension a decision is keyed on: NFC-normalized,
// case-folded, trailing dots and Unicode spaces stripped, and only the LAST
// extension kept. It returns "" when the name has no usable extension.
//
// The name passed in must be the FINAL sanitized filename — what
// naming.SanitizeFilename (and naming.Unique) produced, which is the name that
// would actually land on disk. Sanitizing can change the extension:
// naming.ensureSyncable appends ".bin" to a name whose extension iCloud
// refuses to sync ("draft.tmp" -> "draft.tmp.bin"), and that .bin is what the
// allowlist must see. Keying on the sender's raw filename instead would decide
// about a name that never exists.
//
// Only the last extension counts, which is the whole point:
//
//	"invoice.pdf.exe" -> "exe"   (denied)
//	"invoice.pdf.zip" -> "zip"
//	"payload.exe. . " -> "exe"   (macOS and Windows both ignore the tail)
//	"report.PDF"      -> "pdf"
//	".bashrc"         -> ""      (a leading dot is not an extension)
//	"README"          -> ""      (extensionless: resolved from content)
func NormalizeExt(name string) string {
	if name == "" {
		return ""
	}
	n := norm.NFC.String(name)
	// Trailing dots and spaces are ignored by both macOS apps and Windows,
	// so they are a place to hide the real extension.
	n = strings.TrimRightFunc(n, func(r rune) bool { return r == '.' || unicode.IsSpace(r) })
	i := strings.LastIndexByte(n, '.')
	if i <= 0 || i == len(n)-1 {
		return ""
	}
	ext := n[i+1:]
	if strings.ContainsAny(ext, `/\`) {
		return "" // a separator smuggled past the caller: no extension here
	}
	ext = strings.TrimSpace(cases.Fold().String(ext))
	if ext == "" || len(ext) > extMaxBytes {
		return ""
	}
	return ext
}

// parseAllowEntry parses one allow_extensions / deny_extensions element.
// Accepted forms, with or without a leading dot and in any case:
//
//	"pdf"                                 extension only
//	".PDF"                                same
//	"7z=application/x-7z-compressed"      extension with its permitted types
//	"7z = application/x-7z-compressed, application/zip"
//
// The type list may be separated by commas or pipes. Errors name the offending
// element so the operator can find it in config.toml.
func parseAllowEntry(raw string) (ext string, types []string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil, fmt.Errorf("empty entry: remove it, or write an extension such as \"pdf\"")
	}
	var typePart string
	var mapped bool
	if i := strings.IndexByte(s, '='); i >= 0 {
		s, typePart, mapped = strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
	}
	ext = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, ".")))
	if err := validExtToken(ext); err != nil {
		return "", nil, fmt.Errorf("%q: %w", raw, err)
	}
	for _, t := range strings.FieldsFunc(typePart, func(r rune) bool { return r == ',' || r == '|' }) {
		t = essence(t)
		if t == "" {
			continue
		}
		if strings.Count(t, "/") != 1 || strings.HasPrefix(t, "/") || strings.HasSuffix(t, "/") {
			return "", nil, fmt.Errorf("%q: %q is not a \"type/subtype\" content type (e.g. \"application/x-7z-compressed\")", raw, t)
		}
		types = append(types, t)
	}
	if mapped && len(types) == 0 {
		return "", nil, fmt.Errorf("%q: nothing after '=' — write at least one \"type/subtype\", or drop the '=' to accept any content", raw)
	}
	return ext, types, nil
}

// validExtToken enforces the extension grammar for configurable entries:
// lowercase ASCII letters, digits, and the few punctuation characters real
// extensions use.
func validExtToken(ext string) error {
	if ext == "" {
		return fmt.Errorf("no extension before '=' (write e.g. \"7z=application/x-7z-compressed\")")
	}
	if len(ext) > extMaxBytes {
		return fmt.Errorf("extension %q is longer than %d bytes", ext, extMaxBytes)
	}
	for i, r := range ext {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '+' || r == '-' || r == '_') && i > 0:
		default:
			return fmt.Errorf("extension %q is not a plain extension: use lowercase letters and digits (e.g. \"pdf\", \"7z\", \"p7s\") with no dots", ext)
		}
	}
	return nil
}
