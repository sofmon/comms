package policy

// The ONE carve-out to save's never-decompress invariant.
//
// No non-test file in this tree imports archive/zip, compress/gzip,
// compress/flate or archive/tar, and this file does not change that: it reads
// the zip CENTRAL DIRECTORY's fixed-layout headers to recover the ENTRY NAMES
// and nothing else. No entry data is read, no stream is inflated, and the code
// below is structurally incapable of inflating one — there is no decompressor
// in it.
//
// It exists for exactly one reason. mimetype v1.4.15 reports macro-enabled
// Office as plain docx/xlsx/pptx, so a .docm renamed to .docx is invisible to
// content sniffing and invisible to the extension deny list. The presence of
// word/vbaProject.bin (or the xl/, ppt/ and top-level variants) in the central
// directory is the only thing left that gives it away.
//
// DO NOT read this carve-out as permission to add zip indexing, attachment
// extraction, or any other inflation. A future feature that wants zip contents
// needs its own decision, not this precedent.
//
// Failure is permissive by design: a central directory this reader cannot
// parse yields "no vbaProject found" and the file follows the ordinary rules.
// The alternative — refusing every zip whose directory looks odd — would skip
// real business documents to defend against an evasion that Word itself could
// not open, since Word reads the same central directory.

import (
	"encoding/binary"
	"strings"
)

const (
	sigEOCD         = 0x06054b50 // end of central directory
	sigCentralFile  = 0x02014b50 // central directory file header
	sigZip64Locator = 0x07064b50 // zip64 end of central directory locator
	sigZip64EOCD    = 0x06064b50 // zip64 end of central directory record

	eocdFixedLen        = 22
	centralFileFixedLen = 46
	zip64LocatorLen     = 20
	maxZipComment       = 1 << 16

	// maxZipEntriesScanned bounds the walk so a hostile central directory
	// cannot turn a bounded attachment into an unbounded loop. Real Office
	// documents have tens to hundreds of entries.
	maxZipEntriesScanned = 100_000
)

// vbaProjectNames are the central-directory paths that mark a macro-enabled
// Office file, compared after normalization (backslashes folded to slashes,
// leading "./" and "/" stripped, ASCII-lowercased).
var vbaProjectNames = map[string]bool{
	"word/vbaproject.bin": true,
	"xl/vbaproject.bin":   true,
	"ppt/vbaproject.bin":  true,
	"vbaproject.bin":      true,
}

// HasVBAProject reports whether b is a zip whose central directory names a
// VBA macro project. It never inflates anything. A zip it cannot parse
// reports false — see the file comment for why that is the right default.
//
// Exported so `save doctor` and tests can exercise the check directly; Decide
// applies it only to content that sniffed as one of the three OOXML types.
func HasVBAProject(b []byte) bool {
	found := false
	walkZipEntryNames(b, func(name string) bool {
		if vbaProjectNames[normalizeZipEntry(name)] {
			found = true
			return false
		}
		return true
	})
	return found
}

// zipEntryNames returns the central-directory entry names of b, capped at
// maxZipEntriesScanned. It is the testable form of walkZipEntryNames.
func zipEntryNames(b []byte) []string {
	var out []string
	walkZipEntryNames(b, func(name string) bool {
		out = append(out, name)
		return true
	})
	return out
}

// walkZipEntryNames calls fn with each central-directory entry name until fn
// returns false, the directory ends, or maxZipEntriesScanned is reached.
func walkZipEntryNames(b []byte, fn func(name string) bool) {
	off, size, ok := centralDirectoryExtent(b)
	if !ok {
		return
	}
	cd := b[off : off+size]
	for n := 0; n < maxZipEntriesScanned; n++ {
		if len(cd) < centralFileFixedLen || le32(cd) != sigCentralFile {
			return
		}
		nameLen := int(le16(cd[28:]))
		extraLen := int(le16(cd[30:]))
		commentLen := int(le16(cd[32:]))
		total := centralFileFixedLen + nameLen + extraLen + commentLen
		if total > len(cd) {
			return
		}
		if !fn(string(cd[centralFileFixedLen : centralFileFixedLen+nameLen])) {
			return
		}
		cd = cd[total:]
	}
}

// centralDirectoryExtent locates the central directory inside b, following the
// zip64 records when the classic end-of-central-directory fields are
// saturated. It returns offsets that are already bounds-checked against b.
func centralDirectoryExtent(b []byte) (off, size int64, ok bool) {
	e := findEOCD(b)
	if e < 0 {
		return 0, 0, false
	}
	eocd := b[e:]
	size = int64(le32(eocd[12:]))
	off = int64(le32(eocd[16:]))

	if size == 0xFFFFFFFF || off == 0xFFFFFFFF || le16(eocd[10:]) == 0xFFFF {
		z64off, z64size, found := zip64Extent(b, e)
		if !found {
			return 0, 0, false
		}
		off, size = z64off, z64size
	}
	if off < 0 || size < 0 || off > int64(len(b)) || size > int64(len(b))-off {
		return 0, 0, false
	}
	return off, size, true
}

// zip64Extent reads the zip64 end-of-central-directory record pointed at by
// the locator that sits immediately before the classic EOCD at eocdAt.
func zip64Extent(b []byte, eocdAt int) (off, size int64, ok bool) {
	l := eocdAt - zip64LocatorLen
	if l < 0 || le32(b[l:]) != sigZip64Locator {
		return 0, 0, false
	}
	recAt := int64(le64(b[l+8:]))
	if recAt < 0 || recAt+56 > int64(len(b)) {
		return 0, 0, false
	}
	rec := b[recAt:]
	if le32(rec) != sigZip64EOCD {
		return 0, 0, false
	}
	size = int64(le64(rec[40:]))
	off = int64(le64(rec[48:]))
	return off, size, true
}

// findEOCD returns the byte offset of the end-of-central-directory record, or
// -1. The record is at most maxZipComment+eocdFixedLen bytes from the end; the
// search runs backwards so a comment containing the signature cannot win.
func findEOCD(b []byte) int {
	if len(b) < eocdFixedLen {
		return -1
	}
	start := len(b) - eocdFixedLen - maxZipComment
	if start < 0 {
		start = 0
	}
	for i := len(b) - eocdFixedLen; i >= start; i-- {
		if le32(b[i:]) != sigEOCD {
			continue
		}
		// The comment length must account for exactly the remaining bytes,
		// which rejects a signature that merely appears inside a comment.
		if int(le16(b[i+20:]))+i+eocdFixedLen == len(b) {
			return i
		}
	}
	return -1
}

// normalizeZipEntry folds a stored entry path to the form vbaProjectNames is
// keyed by. Entry names are stored as raw bytes (CP437 or UTF-8); the paths
// being matched are pure ASCII, so a byte-wise ASCII fold is exact.
func normalizeZipEntry(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimPrefix(name, "/")
	return strings.ToLower(name)
}

func le16(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func le32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func le64(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}
