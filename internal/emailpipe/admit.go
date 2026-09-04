package emailpipe

import (
	"fmt"

	"comms/internal/naming"
	"comms/internal/policy"
)

// Options tunes Render. The zero value is the production default: the
// built-in allowlist policy, no run budget consumed yet, and no free-space
// information.
type Options struct {
	// Policy is applied to every attachment, inline part and extracted
	// data: URI image. nil means policy.Default() — the built-in allowlist
	// with the 50 MB per-attachment cap. Connectors should pass the policy
	// built by config.Config.Policy so operator settings take effect.
	Policy *policy.Policy

	// RunBytesSoFar is the attachment bytes this sync pass has already
	// stored, charged against attachments.run_budget. Accumulate
	// EmailDoc.StoredBytes across messages to maintain it.
	RunBytesSoFar int64

	// FreeSpace is the bytes currently free on the archive volume (from
	// unix.Statfs). Zero or negative means unknown and disables the
	// free-space floor for this message; the floor is re-checked at write
	// time regardless.
	FreeSpace int64
}

// admitter applies the policy to the parts of one message, in walk order,
// maintaining the per-message budgets and the filename collision table.
//
// Two invariants make retro-fetch work after a policy widening:
//
//   - Numbering is policy-independent. A part's fallback name is keyed on
//     its index among ALL file parts, so widening the policy never renumbers
//     the parts around it.
//   - Skipped parts reserve their filename. The recorded Name is exactly the
//     name a later fetch will write, and no stored part can take it in the
//     meantime.
type admitter struct {
	pol       *policy.Policy
	attachDir string
	taken     map[string]bool
	doc       *EmailDoc

	parts    int   // parts stored so far for this message
	msgBytes int64 // bytes stored so far for this message
	runBytes int64 // bytes stored by this pass before this message
	free     int64
}

func newAdmitter(pol *policy.Policy, attachDir string, doc *EmailDoc, opts Options) *admitter {
	if pol == nil {
		pol = policy.Default()
	}
	doc.PolicyDigest = pol.PolicyDigest()
	return &admitter{
		pol:       pol,
		attachDir: attachDir,
		taken:     make(map[string]bool),
		doc:       doc,
		runBytes:  opts.RunBytesSoFar,
		free:      opts.FreeSpace,
	}
}

// candidate is one set of bytes offered for storage.
type candidate struct {
	origName string // sender-supplied filename; "" when the part carried none
	declared string // Content-Type media type, parameters stripped
	partKey  string // dotted MIME index, or "<html part>#data-N" for a data: URI
	content  []byte // decoded bytes; nil and empty are both a legitimate 0-byte part

	// fallback yields the name to use when origName survives sanitizing as
	// nothing. It is a function because deriving it sniffs the content, and
	// with the whole-buffer sniff limit that is a full scan of a
	// potentially 50 MB attachment — not worth paying for the common case
	// of a part that already has a usable filename.
	fallback func() string
}

// admit runs the policy over c, records the outcome on the document, and
// returns the File it recorded together with whether the bytes were stored.
// A refused part is appended to doc.Skipped and announced in doc.Warnings —
// never dropped.
func (a *admitter) admit(c candidate) (File, bool) {
	name, derived := a.finalName(c)
	in := policy.Input{
		Name:              name,
		Content:           c.content,
		PartsSoFar:        a.parts,
		MessageBytesSoFar: a.msgBytes,
		RunBytesSoFar:     a.runBytes + a.msgBytes,
		FreeSpace:         a.free,
	}
	// The authoritative decision runs on the FINAL sanitized, collision-free
	// name — the exact string that becomes the filename on disk.
	v := a.pol.Decide(in)

	f := File{
		Name:         name,
		OrigName:     c.origName,
		DeclaredType: c.declared,
		PartKey:      c.partKey,
		Size:         in.EffectiveSize(),
		SniffedType:  v.SniffedType,
		DerivedExt:   v.DerivedExt || derived,
		SkipReason:   v.Reason,
		SkipDetail:   v.Detail,
	}
	if !v.Store {
		f.Skipped = true
		a.doc.Skipped = append(a.doc.Skipped, f)
		a.doc.Warnings = append(a.doc.Warnings, skipWarning(f))
		return f, false
	}

	f.Warned = v.Warned()
	f.Rel = a.attachDir + "/" + name
	f.Content = c.content
	if f.Content == nil {
		f.Content = []byte{} // a stored 0-byte attachment still gets a file
	}
	a.parts++
	a.msgBytes += f.Size
	a.doc.StoredBytes += f.Size
	a.doc.Files = append(a.doc.Files, f)
	if f.Warned {
		a.doc.Warnings = append(a.doc.Warnings, warnedWarning(f))
	}
	return f, true
}

// finalName produces the filename this part will be recorded under, stored
// or not, and reserves it in the collision table.
//
// Sanitizing comes first because internal/naming owns the iCloud
// sync-exclusion rewrite (which can append ".bin" and so CHANGE the
// extension the policy keys on). An extensionless part then gets one
// derivation pass: the policy resolves an extension from the sniffed
// content, it is appended, and the result is sanitized again — so the
// extension the decision keys on is always the one on disk.
//
// The returned derived flag reports that the extension came from the content
// rather than the sender, and must be carried onto the File: the second,
// authoritative Decide sees a name that now HAS an extension and so reports
// DerivedExt = false.
func (a *admitter) finalName(c candidate) (name string, derived bool) {
	name = naming.SanitizeFilename(c.origName, "")
	if name == "" {
		name = naming.SanitizeFilename(c.fallback(), "attachment")
	}
	if policy.NormalizeExt(name) == "" {
		peek := a.pol.Decide(policy.Input{Name: name, Content: c.content})
		if peek.DerivedExt && peek.NormalizedExt != "" {
			if withExt := naming.SanitizeFilename(name+"."+peek.NormalizedExt, ""); withExt != "" {
				name, derived = withExt, true
			}
		}
	}
	return naming.Unique(a.taken, name), derived
}

// skipWarning is the one-line, human-readable form of a refusal. It goes into
// EmailDoc.Warnings — and therefore into the note's frontmatter — so a skip is
// impossible to miss even from a caller that never looks at EmailDoc.Skipped.
func skipWarning(f File) string {
	return fmt.Sprintf("attachment %s (part %s, %d bytes, declared %s, sniffed %s) NOT stored: %s — %s",
		quoteName(f), partLabel(f.PartKey), f.Size, orNone(f.DeclaredType), orNone(f.SniffedType), f.SkipReason, f.SkipDetail)
}

// warnedWarning is the same line for a file stored under protest.
func warnedWarning(f File) string {
	return fmt.Sprintf("attachment %s (part %s, %d bytes, declared %s, sniffed %s) stored under protest: %s — %s",
		quoteName(f), partLabel(f.PartKey), f.Size, orNone(f.DeclaredType), orNone(f.SniffedType), f.SkipReason, f.SkipDetail)
}

func quoteName(f File) string {
	if f.OrigName == "" {
		return fmt.Sprintf("(unnamed) %q", f.Name)
	}
	if f.OrigName != f.Name {
		return fmt.Sprintf("%q (as %q)", f.OrigName, f.Name)
	}
	return fmt.Sprintf("%q", f.OrigName)
}

func partLabel(key string) string {
	if key == "" {
		return "?"
	}
	return key
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
