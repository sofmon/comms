package archive

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// QuarantineMode selects whether attachment files get the macOS
// com.apple.quarantine extended attribute.
//
// # What the tag buys, and what it emphatically does not
//
//   - It makes Gatekeeper show the "downloaded from the internet" consent
//     prompt before an app, installer or script is launched, and it makes
//     Microsoft Office for Mac open the document in Protected View. That is
//     the whole benefit.
//
//   - It does NOT get the file scanned for malware. Every XProtect signature
//     on this machine gates on app bundles, installers or executables; for
//     pdf, OOXML, zip and image files — which is essentially the entire
//     allowlist — a quarantine tag triggers no content scan whatsoever.
//     Never describe this as antivirus, in docs, in config comments or in a
//     commit message.
//
//   - It is best effort and not durable. An application that safe-saves over
//     an archived attachment silently un-quarantines it, and an iCloud
//     round trip degrades the value to its flags field alone.
//
// The extension-and-content allowlist in comms/internal/policy is the real
// boundary; this is a speed bump behind it, kept because it is nearly free.
//
// The zero value tags, matching the attachments.quarantine = true config
// default, so a Writer built without thinking about it is the safer one.
type QuarantineMode int

const (
	// QuarantineOn tags attachment files. Zero value.
	QuarantineOn QuarantineMode = iota
	// QuarantineOff writes attachment files untagged
	// (attachments.quarantine = false).
	QuarantineOff
)

// QuarantineFor maps the attachments.quarantine config flag onto the mode,
// so a caller never has to remember which way the zero value points.
func QuarantineFor(enabled bool) QuarantineMode {
	if enabled {
		return QuarantineOn
	}
	return QuarantineOff
}

func (m QuarantineMode) enabled() bool { return m != QuarantineOff }

// Origin is the provenance stamped onto an attachment file as
// com.apple.metadata:kMDItemWhereFroms, which Finder shows as "Where from" in
// Get Info and which survives the file being copied out of the archive.
//
// Apple's convention for that array is [origin, referrer]; comms writes
// [source instance id, message identity], which is the closest honest
// analogue for mail. Both fields are optional: an Origin with neither set
// writes no metadata at all.
type Origin struct {
	// Source is the account instance id, e.g. "gmail:work".
	Source string
	// Ref identifies the message the attachment arrived on: its Message-ID
	// where there is one, otherwise the source's stable id.
	Ref string
}

// WhereFroms renders the Origin as the kMDItemWhereFroms string array: the
// non-empty fields in order, each collapsed to one line and repaired to valid
// UTF-8. It returns nil when there is nothing to record.
func (o Origin) WhereFroms() []string {
	var out []string
	for _, s := range []string{o.Source, o.Ref} {
		if s = oneLine(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (o Origin) empty() bool { return oneLine(o.Source) == "" && oneLine(o.Ref) == "" }

// oneLine repairs invalid UTF-8 and collapses line breaks to spaces, so a
// hostile header value cannot smuggle structure into a metadata value.
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(validStr(s)))
}

// Extended-attribute names. kMDItemWhereFroms is namespaced under
// com.apple.metadata: so Spotlight picks it up as an item attribute.
const (
	xattrQuarantine = "com.apple.quarantine"
	xattrWhereFroms = "com.apple.metadata:kMDItemWhereFroms"
)

// quarantineFlags is the flags field of the com.apple.quarantine value.
//
// 0081 is the literal observed on ordinary downloaded files on this machine —
// the plain "downloaded, not yet opened, written by a non-sandboxed agent"
// shape, and what iOS-originated files in iCloud Drive carry. The individual
// bits (0x0001/0x0002/0x0040/0x0080/...) have no documented meaning, so the
// value is copied verbatim rather than derived from bit semantics.
const quarantineFlags = "0081"

// quarantineAgent is the third field: the program that put the file there.
const quarantineAgent = "comms"

// quarantineValue builds the com.apple.quarantine string:
// flags;hex-epoch;agent;UUID, four semicolon-separated fields, verified
// against real on-disk values on macOS 26.
//
// The UUID does not need to exist in the LaunchServices QuarantineEventsV2
// database — files carrying unknown, and even empty, UUIDs are common on disk
// and behave normally. Downstream treatment keys off the value and the file's
// type, not off who wrote it, so a comms-tagged file is treated exactly like a
// browser-tagged one.
func quarantineValue(t time.Time, id string) string {
	return fmt.Sprintf("%s;%x;%s;%s", quarantineFlags, t.Unix(), quarantineAgent, id)
}

// newQuarantineUUID returns an uppercase RFC 4122 version 4 UUID, the shape
// LaunchServices writes. Hand-rolled because the repo has no UUID dependency
// and this is the only caller.
func newQuarantineUUID() string {
	var b [16]byte
	// crypto/rand.Read is documented never to return an error and always to
	// fill b entirely; it panics rather than returning short.
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // variant 10x
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
}
