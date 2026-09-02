// Package emailpipe turns raw RFC 2822 bytes into a rendered EmailDoc:
// Markdown body, decoded attachment files, and parsed headers. It is the
// single rendering path shared by the Gmail and FastMail connectors.
//
// The implementation lives in render.go with the signatures:
//
//	func Render(raw []byte, attachDirName string) (*EmailDoc, error)
//	func RenderWithOptions(raw []byte, attachDirName string, opts Options) (*EmailDoc, error)
//
// attachDirName is the final path element of the message's sibling
// attachment directory (e.g. "143205_gmail_re-invoice_a1b2c3d4.d"); every
// File.Rel and every link in BodyMD must start with it so links are relative
// to the .md file's directory.
//
// # The attachment policy
//
// Every attachment, inline part and extracted data: URI image is put to
// save/internal/policy before its bytes are kept. The policy is
// allowlist-only: bytes are stored iff the normalized final extension is
// allowlisted AND the sniffed content type is permitted for that extension.
// A part the policy refuses is NEVER dropped silently — it lands in
// EmailDoc.Skipped with its identity, its byte count, its sniffed type and
// the machine-readable reason, and a matching line is appended to
// EmailDoc.Warnings so it is visible in the note's frontmatter even before a
// caller learns to render Skipped itself.
package emailpipe

import "time"

// EmailDoc is the rendered form of one RFC 2822 message. Produced by
// Render, consumed by the archive writer (which adds source-level metadata
// such as account, labels, and stable ids to the frontmatter).
type EmailDoc struct {
	Subject   string
	MessageID string   // RFC 5322 Message-ID header, angle brackets kept; "" if absent
	From      []string // "Name <addr>" formatted, RFC 2047 decoded
	To        []string
	Cc        []string
	Date      time.Time // Date: header instant with its original offset; zero if unparseable
	BodyMD    string    // converted body; cid:/data: image refs already rewritten to relative paths

	// Headers are the bulk-mail and automation headers noise triage keys on,
	// captured here because only the archiver ever sees the raw message:
	// once the note is written nothing can consult a header that was not
	// persisted without fetching the message again. Keys are lowercase
	// header names from TriageHeaderNames; a repeated header's values are
	// joined with ", ". Values are decoded, single-line and length-capped so
	// a hostile header cannot bloat or restructure the frontmatter. nil when
	// the message carries none of them.
	Headers map[string]string

	// Files are the parts the policy accepted: every one has a Rel under the
	// attachment directory and its decoded Content, and every one must be
	// written to disk. Body conversion failing never empties this slice.
	Files []File

	// Skipped are the parts the policy refused, in the same deterministic
	// order the parts were walked. Each carries Skipped = true, a SkipReason
	// from save/internal/policy, its decoded Size, its SniffedType and the
	// fetch identity (PartKey) needed to retro-fetch it after a policy
	// widening — but no Rel and no Content, because nothing was written.
	//
	// It is deliberately a separate slice from Files: a caller that iterates
	// Files and writes every entry stays correct without knowing this field
	// exists, and can never be tricked into writing a refused part to a
	// zero-length path.
	Skipped []File

	// StoredBytes is the total decoded size of Files, for the caller to
	// accumulate into the next message's Options.RunBytesSoFar.
	StoredBytes int64

	// PolicyDigest identifies the policy that produced every verdict in this
	// document (policy.Policy.PolicyDigest). Record it with each skip: it is
	// what tells a later run that the policy has changed and the skip is
	// worth re-deciding.
	PolicyDigest string

	Warnings []string // non-fatal parse/convert/policy problems, surfaced in frontmatter
}

// File is one attachment, inline part, or extracted data: URI image, decoded
// (transfer encoding already removed).
//
// A file the policy accepted has Rel set (always beginning with the
// attachDirName passed to Render) and Content populated. A file the policy
// refused has Skipped = true, an empty Rel and nil Content — it exists to be
// recorded, not written — but keeps every identifying field, so the skip can
// be re-decided offline and re-fetched later.
type File struct {
	// Rel is the path relative to the .md file's directory, always beginning
	// with attachDirName. Empty exactly when Skipped is true.
	Rel string

	// Content is the decoded part. nil exactly when Skipped is true (a
	// stored zero-byte attachment has non-nil, zero-length Content).
	Content []byte

	// Name is the final sanitized filename: path.Base(Rel) for a stored
	// file, and for a skipped one the name that WOULD have been used — the
	// same name a retro-fetch will write, because skipped parts reserve
	// their name in the collision table exactly like stored ones.
	Name string

	// OrigName is the sender-supplied filename before sanitizing, "" when
	// the part carried none.
	OrigName string

	// DeclaredType is the part's Content-Type media type with parameters
	// stripped ("application/pdf"), or the media type of a data: URI. It is
	// what the sender claimed; SniffedType is what the bytes actually are.
	DeclaredType string

	// PartKey is the dotted MIME index of the part within the message
	// ("2.1.3", enmime's PartID). Data: URI images extracted from the HTML
	// body get the body part's key plus "#data-N". Together with the source
	// instance id and the message's stable id it is the fetch identity of
	// this part.
	PartKey string

	// Size is the decoded byte count, set for stored and skipped files
	// alike (a skipped file has no Content to measure).
	Size int64

	// SniffedType is the bare "type/subtype" detected from the content by
	// save/internal/policy. It is ALWAYS populated, including on a skip:
	// that is what makes the record honest and re-decidable without the
	// bytes.
	SniffedType string

	// DerivedExt reports that Name's extension came from the sniffed
	// content because the part had no usable filename extension.
	DerivedExt bool

	// Skipped is true when the policy refused to store this part.
	Skipped bool

	// Warned is true when the policy stored this part under protest
	// (attachments.on_mismatch = "store-warn", or a flagged scan with
	// scan_action = "record"). SkipReason explains why. Skipped and Warned
	// are never both true.
	Warned bool

	// SkipReason is the machine-readable policy reason: one of the
	// policy.Reason* constants, "" on a clean store. It is set both on a
	// skip and on a warned store, so callers persist the same enum either
	// way.
	SkipReason string

	// SkipDetail is the human-readable elaboration for the note. It is never
	// parsed; SkipReason is the machine-readable field.
	SkipDetail string
}
