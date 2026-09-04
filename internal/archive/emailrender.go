package archive

import (
	"bytes"
	"fmt"
	"path"
	"strings"
	"time"

	"comms/internal/emailpipe"
	"comms/internal/policy"
)

// SkipDisposition says what happened to the bytes of an attachment the policy
// refused. It exists because the honest sentence differs by source and the
// renderer must not guess: on Gmail and FastMail the whole message is
// downloaded, so the bytes really were fetched and then discarded, while a
// Google Chat blob refused by policy.PreCheck genuinely was never fetched.
//
// The connector sets it; the renderer only translates it.
type SkipDisposition string

const (
	// SkipDispositionUnspecified is the zero value. The note then says only
	// that no copy was kept, and claims nothing either way about whether the
	// bytes were transferred. Connectors should always set one of the two
	// real values instead.
	SkipDispositionUnspecified SkipDisposition = ""

	// SkipBytesDiscarded: the bytes arrived (the whole message was
	// downloaded) and were thrown away rather than written. Gmail
	// (format=raw) and FastMail (whole-blob fetch).
	SkipBytesDiscarded SkipDisposition = "discarded"

	// SkipBytesNotFetched: the refusal happened before the download, so the
	// bytes never crossed the network. Google Chat, where the policy runs as
	// a PreCheck on the declared name and size.
	SkipBytesNotFetched SkipDisposition = "not_fetched"
)

// validSkipDisposition rejects a value that is neither the zero value nor one
// of the two constants — a programming error in the connector, not data.
func validSkipDisposition(d SkipDisposition) bool {
	switch d {
	case SkipDispositionUnspecified, SkipBytesDiscarded, SkipBytesNotFetched:
		return true
	}
	return false
}

// sentence renders the disposition as the clause the note shows.
func (d SkipDisposition) sentence() string {
	switch d {
	case SkipBytesDiscarded:
		return "The bytes arrived with the message and were discarded rather than written to disk."
	case SkipBytesNotFetched:
		return "The bytes were never downloaded."
	default:
		return "No copy was kept."
	}
}

// EmailRenderVersion is the frontmatter layout version stamped into every
// email note as render_version. Notes are immutable once written, so a
// layout change is never applied retroactively; the version lets a reader
// tell an OLD note from a NEW one that merely has nothing to say.
//
//	1: the original layout (notes written before this field existed carry
//	   no render_version at all, which reads as 1).
//	2: adds headers — the bulk-mail headers emailpipe.TriageHeaderNames
//	   captures at ingest. Absent on a v2 note means the message carried
//	   none; absent on a v1 note means nobody looked. Noise triage's header
//	   layer therefore says nothing about a v1 note and the other layers
//	   decide.
const EmailRenderVersion = 2

// emailFrontmatter is marshaled with yaml.v3 only — hostile header values
// (subjects, addresses, sender-supplied filenames) must never be able to
// inject YAML structure.
type emailFrontmatter struct {
	Source        string `yaml:"source"` // instance id, e.g. "gmail:work"
	Type          string `yaml:"type"`
	RenderVersion int    `yaml:"render_version"` // EmailRenderVersion
	Account       string `yaml:"account"`        // the account's email address
	AccountLabel  string `yaml:"account_label"`  // its permanent config label

	MessageID string   `yaml:"message_id"`
	ThreadID  string   `yaml:"thread_id"`
	Date      string   `yaml:"date"`     // Date: header instant, original offset, RFC 3339; "" if unparseable
	DateUTC   string   `yaml:"date_utc"` // server timestamp, RFC 3339 UTC
	From      []string `yaml:"from"`
	To        []string `yaml:"to"`
	Cc        []string `yaml:"cc"`
	Subject   string   `yaml:"subject"`
	Labels    []string `yaml:"labels"`

	// Headers are the triage headers the message carried (emailpipe
	// .EmailDoc.Headers), omitted entirely when there were none. yaml.v3
	// writes map keys sorted, so the bytes are deterministic.
	Headers map[string]string `yaml:"headers,omitempty"`

	Attachments []string `yaml:"attachments"` // rel paths from the .md's directory

	// SkippedAttachments records every part the attachment policy refused.
	// Omitted entirely in the common case, so a note with nothing skipped is
	// byte-identical to one written before this field existed.
	SkippedAttachments []skippedEntry `yaml:"skipped_attachments,omitempty"`

	// PolicyDigest identifies the policy that produced the verdicts above. It
	// is what tells a later run that the policy changed and the skips are
	// worth re-deciding.
	PolicyDigest string `yaml:"attachment_policy_digest,omitempty"`

	Warnings []string `yaml:"warnings,omitempty"`
}

// skippedEntry is the machine-readable form of one refusal, mirroring the
// state.SkippedAttachment row recorded for the same part. It deliberately
// duplicates what the visible "## Attachments" entry says: the frontmatter is
// for tooling, the section is for the reader, and a skip must be impossible
// to miss in either.
//
// content_sha256 is absent: by the time the renderer sees a refused part its
// bytes have already been released (emailpipe.File.Content is nil exactly
// when Skipped), so the hash lives only in the state DB row, which the
// connector records from the bytes it still holds.
type skippedEntry struct {
	Name         string `yaml:"name"` // the sanitized name that would have been used
	OrigName     string `yaml:"orig_name,omitempty"`
	PartKey      string `yaml:"part_key,omitempty"`
	Size         int64  `yaml:"size"` // decoded bytes
	DeclaredType string `yaml:"declared_type,omitempty"`
	DeclaredExt  string `yaml:"declared_ext,omitempty"`
	SniffedType  string `yaml:"sniffed_type,omitempty"`
	Reason       string `yaml:"reason"` // a policy.Reason* constant
	Detail       string `yaml:"detail,omitempty"`
	Disposition  string `yaml:"disposition"` // a SkipDisposition constant
	Recoverable  bool   `yaml:"recoverable"` // `comms refetch` can retry this row
}

// renderEmailMD renders the full .md bytes: YAML frontmatter, body, and — even
// when the body fell back or is empty — an Attachments section listing both
// what was written and what the policy refused, so nothing the archiver saw
// can be invisible from the document (never-drop rule).
func renderEmailMD(doc *emailpipe.EmailDoc, meta EmailMeta) ([]byte, error) {
	fm := emailFrontmatter{
		Source:        validStr(meta.Source),
		Type:          "email",
		RenderVersion: EmailRenderVersion,
		Account:       validStr(meta.Account),
		AccountLabel:  validStr(meta.AccountLabel),
		MessageID:     validStr(doc.MessageID),
		ThreadID:      validStr(meta.ThreadID),
		DateUTC:       meta.ServerTime.UTC().Format(time.RFC3339),
		From:          validStrs(doc.From),
		To:            validStrs(doc.To),
		Cc:            validStrs(doc.Cc),
		Subject:       validStr(doc.Subject),
		Labels:        validStrs(meta.Labels),
		Headers:       validMap(doc.Headers),
		PolicyDigest:  validStr(doc.PolicyDigest),
		Warnings:      validStrs(doc.Warnings),
	}
	if !doc.Date.IsZero() {
		fm.Date = doc.Date.Format(time.RFC3339)
	}
	fm.Attachments = make([]string, 0, len(doc.Files))
	for _, f := range doc.Files {
		fm.Attachments = append(fm.Attachments, f.Rel)
	}
	for _, f := range doc.Skipped {
		fm.SkippedAttachments = append(fm.SkippedAttachments, skippedEntryFor(f, meta.SkipDisposition))
	}

	var buf bytes.Buffer
	if err := writeFrontmatter(&buf, fm); err != nil {
		return nil, err
	}
	if body := strings.TrimRight(doc.BodyMD, "\n"); body != "" {
		buf.WriteString("\n")
		buf.WriteString(body)
		buf.WriteString("\n")
	}
	if len(doc.Files) > 0 || len(doc.Skipped) > 0 {
		buf.WriteString("\n## Attachments\n\n")
		for _, f := range doc.Files {
			writeStoredEntry(&buf, f)
		}
		for _, f := range doc.Skipped {
			writeSkippedEntry(&buf, f, meta.SkipDisposition)
		}
	}
	return buf.Bytes(), nil
}

// skippedEntryFor builds the frontmatter record for one refused part. The
// extension is recomputed with policy.NormalizeExt from the final sanitized
// name, so it is exactly the string the decision keyed on — including the
// ".bin" the iCloud sync-exclusion sanitizer may have appended.
func skippedEntryFor(f emailpipe.File, d SkipDisposition) skippedEntry {
	return skippedEntry{
		Name:         validStr(f.Name),
		OrigName:     validStr(f.OrigName),
		PartKey:      validStr(f.PartKey),
		Size:         f.Size,
		DeclaredType: validStr(f.DeclaredType),
		DeclaredExt:  policy.NormalizeExt(f.Name),
		SniffedType:  validStr(f.SniffedType),
		Reason:       validStr(f.SkipReason),
		Detail:       validStr(f.SkipDetail),
		Disposition:  string(d),
		// A part with no fetch identity cannot be asked for again, whatever
		// the policy later says.
		Recoverable: f.PartKey != "",
	}
}

// writeStoredEntry renders one written attachment as a link. A file stored
// under protest (attachments.on_mismatch = "store-warn", or a flagged scan
// with scan_action = "record") says so on the same line: it is on disk, but
// the policy objected and the reader should know which files those are.
func writeStoredEntry(buf *bytes.Buffer, f emailpipe.File) {
	fmt.Fprintf(buf, "- [%s](%s)", mdLinkText(path.Base(f.Rel)), mdLinkDest(f.Rel))
	if f.Warned {
		// The reason token goes in a code span, so it is wrapped with mdCode
		// rather than mdInline: backslash escapes are inert inside a code
		// span and would render as literal backslashes.
		fmt.Fprintf(buf, " — **stored despite a policy objection**: %s (%s)",
			reasonEnglish(f.SkipReason), mdCode(f.SkipReason))
		if d := oneLine(f.SkipDetail); d != "" {
			fmt.Fprintf(buf, " — %s", mdInline(d))
		}
	}
	buf.WriteString("\n")
}

// writeSkippedEntry renders one refusal as a visible, bolded block under
// "## Attachments". The frontmatter carries the same facts for tooling; this
// is the copy a human reading the note cannot scroll past.
func writeSkippedEntry(buf *bytes.Buffer, f emailpipe.File, d SkipDisposition) {
	fmt.Fprintf(buf, "- **Not stored — %s**", mdCode(f.Name))
	if on := oneLine(f.OrigName); on != "" && on != oneLine(f.Name) {
		fmt.Fprintf(buf, " (sent as %s)", mdCode(on))
	}
	fmt.Fprintf(buf, " — %s", sizeText(f.Size))
	fmt.Fprintf(buf, ", declared %s", mdCode(orUnknown(f.DeclaredType)))
	if f.SniffedType != "" {
		fmt.Fprintf(buf, ", detected %s", mdCode(f.SniffedType))
	} else {
		buf.WriteString(", content not inspected")
	}
	if pk := oneLine(f.PartKey); pk != "" {
		fmt.Fprintf(buf, ", part %s", mdCode(pk))
	}
	buf.WriteString("\n")

	fmt.Fprintf(buf, "  - Reason: %s (%s)", reasonEnglish(f.SkipReason), mdCode(f.SkipReason))
	if detail := oneLine(f.SkipDetail); detail != "" {
		fmt.Fprintf(buf, " — %s", mdInline(detail))
	}
	buf.WriteString("\n")
	fmt.Fprintf(buf, "  - %s %s\n", d.sentence(), recoveryHint(f.SkipReason))
}

// recoveryHint tells the reader how the skip can be undone. Transient reasons
// (a run budget spent, disk space below the floor) come back on the next
// refetch with no config change at all; the rest need the policy widened
// first.
func recoveryHint(reason string) string {
	if policy.Transient(reason) {
		return "`comms refetch` retries this on a later run without any config change."
	}
	return "Widen the `[attachments]` policy and run `comms refetch` to retrieve it."
}

// reasonEnglish translates a policy.Reason* constant into the plain sentence
// the note shows. It never interpolates the raw token — the caller renders
// that separately in a code span — so an unrecognized reason cannot inject
// Markdown here.
func reasonEnglish(reason string) string {
	switch reason {
	case policy.ReasonNotAllowlistedExtension:
		return "its file extension is not on the archive's allowlist"
	case policy.ReasonNotAllowlistedContent:
		return "the content type detected from its bytes is not allowed for that extension"
	case policy.ReasonExtensionContentMismatch:
		return "its file extension and the content type detected from its bytes disagree"
	case policy.ReasonOverSizeCap:
		return "it is larger than the per-attachment size cap"
	case policy.ReasonOverMessageBudget:
		return "this message's attachments had already used up the per-message budget"
	case policy.ReasonOverRunBudget:
		return "this sync pass had already used up its attachment budget"
	case policy.ReasonFreeSpaceFloor:
		return "free space on the archive volume is below the configured floor"
	case policy.ReasonScanHookFlagged:
		return "the configured scan_command flagged it"
	case policy.ReasonMacroOffice:
		return "macro-enabled Office documents are refused by extension"
	case policy.ReasonContainerDenied:
		return "archive containers are not stored (`allow_containers` is off)"
	case policy.ReasonSVGDenied:
		return "SVG images are not stored (`allow_svg` is off)"
	case policy.ReasonNone:
		return "the attachment policy refused it, without recording a reason"
	default:
		return "the attachment policy refused it"
	}
}

// sizeText renders a byte count exactly, with a rounded SI form alongside it
// once the exact number stops being readable. Deterministic and
// locale-independent: the same count always renders the same bytes.
func sizeText(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d bytes", n)
	}
	const unit = 1000
	units := [...]string{"kB", "MB", "GB", "TB", "PB"}
	v, i := float64(n), -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%d bytes (%.1f %s)", n, v, units[i])
}

func orUnknown(s string) string {
	if oneLine(s) == "" {
		return "(no type)"
	}
	return s
}

// mdCode wraps s in a code span, choosing a backtick fence longer than any
// run of backticks inside it (CommonMark's escape hatch) and padding with
// spaces when the content itself starts or ends with one. A sender-supplied
// filename full of backticks therefore cannot break out of the span.
func mdCode(s string) string {
	s = oneLine(s)
	if s == "" {
		return "(unnamed)"
	}
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	return fence + pad + s + pad + fence
}
