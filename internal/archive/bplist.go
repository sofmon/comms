package archive

import (
	"encoding/binary"
	"unicode/utf16"
)

// bplistStrings encodes an array of strings as an Apple binary property list
// (bplist00), the format com.apple.metadata:kMDItemWhereFroms is stored in.
//
// Hand-rolled on purpose: the repo has no plist dependency, the shape needed
// here is fixed (one array whose elements are all strings), and the encoding
// was verified byte for byte against `plutil` and against a real Safari
// download's xattr on macOS 26 — `62706c6973743030 a1 01 5f 10 15 <url>` for
// a one-element array, i.e. header, array marker, object refs, string
// objects, offset table, 32-byte trailer.
//
// Layout produced:
//
//	"bplist00"                      8-byte header
//	object 0                        the array: 0xA_ | count, then count refs
//	objects 1..n                    the strings, in array order
//	offset table                    one byte per object (see below)
//	trailer                         32 bytes, fixed shape
//
// A one-byte offset table and one-byte object refs are always sufficient
// here: callers pass a handful of short provenance strings, and the encoder
// refuses anything that would not fit rather than emitting a malformed plist.
func bplistStrings(values []string) []byte {
	if len(values) == 0 || len(values) > 14 {
		// >14 elements would need the long-form array marker, and no caller
		// has that many. Nothing is better than a plist Finder cannot read.
		return nil
	}

	out := make([]byte, 0, 64)
	out = append(out, "bplist00"...)

	offsets := make([]int, 0, len(values)+1)

	// Object 0: the array. Short form (count < 15) plus one ref byte per
	// element; element i is object i+1.
	offsets = append(offsets, len(out))
	out = append(out, byte(0xA0|len(values)))
	for i := range values {
		out = append(out, byte(i+1))
	}

	for _, v := range values {
		offsets = append(offsets, len(out))
		out = appendBplistString(out, v)
	}

	tableOff := len(out)
	if tableOff > 0xFF {
		return nil // would need a wider offset table; not worth supporting
	}
	for _, off := range offsets {
		out = append(out, byte(off))
	}

	// Trailer: 5 unused bytes, sort version, offset-int size, object-ref
	// size, then three big-endian uint64s.
	out = append(out, 0, 0, 0, 0, 0, 0)
	out = append(out, 1) // offsetIntSize
	out = append(out, 1) // objectRefSize
	out = binary.BigEndian.AppendUint64(out, uint64(len(offsets)))
	out = binary.BigEndian.AppendUint64(out, 0) // top object is the array
	out = binary.BigEndian.AppendUint64(out, uint64(tableOff))
	return out
}

// appendBplistString appends one string object: 0x5_ for ASCII, 0x6_ for
// UTF-16BE. The nibble holds the length — in bytes for ASCII, in UTF-16 code
// units for Unicode — or 0xF followed by an integer object when it does not
// fit in four bits.
func appendBplistString(out []byte, s string) []byte {
	if isASCII(s) {
		out = appendBplistMarker(out, 0x50, len(s))
		return append(out, s...)
	}
	units := utf16.Encode([]rune(s))
	out = appendBplistMarker(out, 0x60, len(units))
	for _, u := range units {
		out = binary.BigEndian.AppendUint16(out, u)
	}
	return out
}

// appendBplistMarker writes the type nibble plus the length, spilling into an
// integer object (0x10 = 1 byte, 0x11 = 2 bytes, 0x12 = 4 bytes) when the
// length exceeds 14.
func appendBplistMarker(out []byte, kind byte, n int) []byte {
	if n < 0x0F {
		return append(out, kind|byte(n))
	}
	out = append(out, kind|0x0F)
	switch {
	case n <= 0xFF:
		return append(out, 0x10, byte(n))
	case n <= 0xFFFF:
		out = append(out, 0x11)
		return binary.BigEndian.AppendUint16(out, uint16(n))
	default:
		out = append(out, 0x12)
		return binary.BigEndian.AppendUint32(out, uint32(n))
	}
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
