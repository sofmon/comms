// Package policy is save's single attachment storage decision engine.
//
// The rule, in one sentence: an attachment is stored if and only if its
// NORMALIZED FINAL EXTENSION is on the allowlist AND the CONTENT SNIFFED from
// its magic bytes is permitted for that extension. Both halves must hold. An
// attachment that fails either half is never renamed, never "stored with a
// warning" (unless the operator explicitly sets on_mismatch = "store-warn"),
// and never silently dropped: every refusal comes back as a Verdict carrying
// a machine-readable Reason plus the sniffed type, which the callers record
// in the note's `skipped_attachments:` list and in the state DB so it can be
// re-fetched later via `save refetch`.
//
// The package is deliberately pure and dependency-light — mimetype, x/text
// and the standard library — so every caller (email pipeline, Chat connector,
// verify, refetch) shares one implementation and one PolicyDigest.
//
// # The sniff limit
//
// mimetype's default read limit is 4096 bytes. At that limit an ordinary
// .docx/.xlsx/.pptx whose word//xl//ppt/ entry sits past 4 KB sniffs as
// application/zip and would be WRONGLY SKIPPED by the rule above. This
// package therefore calls mimetype.SetLimit(0) from its init, so merely
// importing it fixes the limit process-wide; Init is also exported for code
// that wants to be explicit. Anything in this tree that calls mimetype
// directly must import this package (or call Init) first.
//
// # The one never-decompress carve-out
//
// save never decompresses an attachment. There is exactly ONE exception,
// implemented in zipdir.go and used only for macro detection: the zip CENTRAL
// DIRECTORY entry NAMES of an OOXML file are read (never inflated) to catch
// word/vbaProject.bin, xl/vbaProject.bin, ppt/vbaProject.bin or a top-level
// vbaProject.bin — which is how a .docm renamed to .docx is caught, since
// mimetype reports macro-enabled Office as plain docx/xlsx/pptx. That carve-out
// does not license zip indexing, extraction, or any other inflation anywhere.
package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

func init() { Init() }

// Init raises mimetype's detection limit to the whole buffer. It is called
// from this package's init, so importing policy is enough; call it explicitly
// only to document the dependency at a process entry point. It is idempotent
// and safe to call at any time.
//
// The archiver already holds the fully decoded attachment in memory, so
// whole-buffer sniffing costs one linear scan and zero extra I/O.
func Init() { mimetype.SetLimit(0) }

// Reason is the machine-readable enum recorded for every non-plain outcome.
// It is a plain string so it can go straight into a SQL parameter, the note
// frontmatter and JSON without conversion. Use the Reason* constants.
//
// A Verdict with Store == false always carries a non-empty Reason. A Verdict
// with Store == true carries a Reason only when the attachment was stored
// under protest — see Verdict.Warned.
const (
	// ReasonNone is the empty Reason of a clean store.
	ReasonNone = ""

	// ReasonNotAllowlistedExtension: the normalized final extension is not on
	// the effective extension allowlist (including anything the operator
	// removed with deny_extensions, and container formats such as 7z/rar/tar
	// that are off the list by default). Fix by adding it to
	// attachments.allow_extensions and running `save refetch`.
	ReasonNotAllowlistedExtension = "not_allowlisted_extension"

	// ReasonNotAllowlistedContent: the part carried NO usable filename
	// extension and its sniffed content type could not be resolved to an
	// allowlisted extension (application/octet-stream, or a type mimetype has
	// no extension for, such as application/x-ole-storage).
	ReasonNotAllowlistedContent = "not_allowlisted_content"

	// ReasonExtensionContentMismatch: the extension IS allowlisted but the
	// sniffed content type is not permitted for it ("invoice.pdf" whose bytes
	// are a zip). This is the only reason on_mismatch = "store-warn" softens.
	ReasonExtensionContentMismatch = "extension_content_mismatch"

	// ReasonOverSizeCap: bigger than attachments.max_size (or chat_max_size
	// for a Chat upload).
	ReasonOverSizeCap = "over_size_cap"

	// ReasonOverMessageBudget: storing it would push this one message past
	// attachments.max_per_message, or the message already has
	// attachments.max_parts_per_message stored parts.
	ReasonOverMessageBudget = "over_message_budget"

	// ReasonOverRunBudget: this sync pass already spent
	// attachments.run_budget on attachments.
	ReasonOverRunBudget = "over_run_budget"

	// ReasonFreeSpaceFloor: writing it would leave the archive volume with
	// less than attachments.free_space_floor free. The .md note is still
	// written — mail keeps archiving.
	ReasonFreeSpaceFloor = "free_space_floor"

	// ReasonScanHookFlagged: attachments.scan_command exited 1 (infected).
	// With scan_action = "record" (the default) the file is still stored and
	// the verdict recorded; with "reject" it is skipped.
	ReasonScanHookFlagged = "scan_hook_flagged"

	// ReasonMacroOffice: a macro-enabled Office extension
	// (docm/xlsm/pptm/dotm/xltm/potm), or an OOXML file whose zip central
	// directory names a vbaProject.bin — a .docm renamed to .docx. Enable
	// with attachments.allow_macro_office.
	ReasonMacroOffice = "macro_office"

	// ReasonContainerDenied: a .zip with attachments.allow_containers = false.
	// Other container formats (7z, rar, tar, gz, tgz, bz2, xz, cab) are not on
	// the allowlist at all and report not_allowlisted_extension instead, since
	// flipping allow_containers would not bring them back.
	ReasonContainerDenied = "container_denied"

	// ReasonSVGDenied: SVG by extension or by sniffed content
	// (image/svg+xml) with attachments.allow_svg = false. SVG is the one
	// image format that is a program, and the vault is rendered by Obsidian
	// (Electron), which draws it in-note without a Finder double-click.
	ReasonSVGDenied = "svg_denied"
)

// Reasons returns every valid non-empty Reason, in a stable order. Useful for
// schema CHECK constraints and for `save status` breakdowns.
func Reasons() []string {
	return []string{
		ReasonNotAllowlistedExtension,
		ReasonNotAllowlistedContent,
		ReasonExtensionContentMismatch,
		ReasonOverSizeCap,
		ReasonOverMessageBudget,
		ReasonOverRunBudget,
		ReasonFreeSpaceFloor,
		ReasonScanHookFlagged,
		ReasonMacroOffice,
		ReasonContainerDenied,
		ReasonSVGDenied,
	}
}

// ValidReason reports whether r is one of the Reason constants (ReasonNone
// included).
func ValidReason(r string) bool { return r == ReasonNone || slices.Contains(Reasons(), r) }

// Transient reports whether a Reason describes a passing condition of the
// machine or the run rather than a property of the attachment: raising a cap,
// freeing disk, or simply running again can change the answer without any
// policy change. `save refetch` re-tries these regardless of the digest.
func Transient(r string) bool {
	switch r {
	case ReasonOverRunBudget, ReasonFreeSpaceFloor:
		return true
	}
	return false
}

// On-mismatch modes for Settings.OnMismatch.
const (
	// OnMismatchSkip refuses an extension/content disagreement (default).
	OnMismatchSkip = "skip"
	// OnMismatchStoreWarn stores it anyway and records the disagreement.
	OnMismatchStoreWarn = "store-warn"
)

// Scan-hook actions for Settings.ScanAction.
const (
	// ScanActionRecord stores a flagged file and records the verdict
	// (default): antivirus false positives on PDFs and Office documents are
	// well documented, and fail-closed would delete real business documents
	// under a never-drop-silently rule.
	ScanActionRecord = "record"
	// ScanActionReject skips a flagged file.
	ScanActionReject = "reject"
)

// AnyType is the sentinel permitted-type set produced by an allow_extensions
// entry that names an extension save has no built-in content table for. The
// extension half of the rule still applies; the content half cannot, because
// save does not know what that format's bytes look like.
const AnyType = "*"

// Settings is the effective, config-independent input to New. Every byte cap
// is in bytes; a cap <= 0 means "no limit" (FreeSpaceFloor <= 0 disables the
// free-space check). config.Config.PolicySettings fills this in from the
// [attachments] block.
type Settings struct {
	MaxSize            int64 // per attachment (mail, and Chat when ChatMaxSize is 0)
	ChatMaxSize        int64 // per Chat attachment; <= 0 inherits MaxSize
	MaxMessageBytes    int64 // raw RFC822 buffer cap; not used by Decide, part of the digest
	MaxPerMessage      int64 // total stored attachment bytes for one message
	MaxPartsPerMessage int   // stored attachment parts for one message
	RunBudget          int64 // total stored attachment bytes for one sync pass
	FreeSpaceFloor     int64 // never leave the volume with less than this free

	// AllowExtensions ADDS to the built-in allowlist. Two forms:
	//   "7z"                              allow the extension, accept any content
	//   "7z=application/x-7z-compressed"  allow it only for those types
	// A comma- or pipe-separated type list is accepted after '='. For an
	// extension that is already built in, a bare entry is a no-op and an
	// explicit mapping UNIONS with the built-in permitted types.
	AllowExtensions []string

	// DenyExtensions SUBTRACTS from the effective allowlist. Deny always wins.
	DenyExtensions []string

	AllowContainers  bool // .zip (default true)
	AllowSVG         bool // .svg and image/svg+xml content (default false)
	AllowMacroOffice bool // docm/xlsm/pptm/dotm/xltm/potm and vbaProject.bin

	OnMismatch  string   // OnMismatchSkip | OnMismatchStoreWarn
	Quarantine  bool     // tag stored files com.apple.quarantine; NOT part of the digest
	ScanCommand []string // optional argv, never a shell line
	ScanAction  string   // ScanActionRecord | ScanActionReject
}

// DefaultSettings returns the shipped defaults: the business core allowlisted,
// macro-enabled Office and every executable type denied, zip allowed, SVG
// denied, 50 MB per attachment.
func DefaultSettings() Settings {
	return Settings{
		MaxSize:            50 * 1000 * 1000,
		ChatMaxSize:        0,
		MaxMessageBytes:    100 * 1000 * 1000,
		MaxPerMessage:      150 * 1000 * 1000,
		MaxPartsPerMessage: 500,
		RunBudget:          2 * 1000 * 1000 * 1000,
		FreeSpaceFloor:     5 * 1000 * 1000 * 1000,
		AllowContainers:    true,
		AllowSVG:           false,
		AllowMacroOffice:   false,
		OnMismatch:         OnMismatchSkip,
		Quarantine:         true,
		ScanAction:         ScanActionRecord,
	}
}

// Policy is an immutable, precomputed decision engine. Build one per run with
// New and share it: it is safe for concurrent use.
type Policy struct {
	set    Settings
	perExt map[string][]string // effective extension -> sorted permitted sniffed types

	canonical string
	digest    string
}

// Default returns a Policy built from DefaultSettings. It cannot fail.
func Default() *Policy {
	p, err := New(DefaultSettings())
	if err != nil {
		panic("policy: default settings are invalid: " + err.Error()) // unreachable
	}
	return p
}

// New validates s and precomputes the effective allowlist and the digest.
// Errors are phrased to read under an "attachments: " prefix so config can
// pass them straight through.
func New(s Settings) (*Policy, error) {
	switch s.OnMismatch {
	case "":
		s.OnMismatch = OnMismatchSkip
	case OnMismatchSkip, OnMismatchStoreWarn:
	default:
		return nil, fmt.Errorf("on_mismatch: %q is not valid (want %q or %q)", s.OnMismatch, OnMismatchSkip, OnMismatchStoreWarn)
	}
	switch s.ScanAction {
	case "":
		s.ScanAction = ScanActionRecord
	case ScanActionRecord, ScanActionReject:
	default:
		return nil, fmt.Errorf("scan_action: %q is not valid (want %q or %q)", s.ScanAction, ScanActionRecord, ScanActionReject)
	}

	perExt := make(map[string][]string, len(defaultAllow)+len(s.AllowExtensions))
	for ext, types := range defaultAllow {
		perExt[ext] = slices.Clone(types)
	}
	// Flag-gated built-ins.
	if s.AllowSVG {
		perExt["svg"] = []string{"image/svg+xml", "text/xml"}
	}
	if s.AllowContainers {
		perExt["zip"] = []string{"application/zip"}
	}
	if s.AllowMacroOffice {
		for ext, types := range macroOfficeAllow {
			perExt[ext] = slices.Clone(types)
		}
	}

	// allow_extensions ADDS.
	for _, raw := range s.AllowExtensions {
		ext, types, err := parseAllowEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("allow_extensions: %w", err)
		}
		if why, hard := HardDenied(ext); hard {
			return nil, fmt.Errorf("allow_extensions: %q cannot be allowed: %s. save will not store it under any configuration; if you need those bytes, fetch the message from the source by hand", ext, why)
		}
		// A flag-gated extension must be turned on by its flag, so the
		// effective allowlist (and therefore the digest) stays truthful.
		// Listing it here would be a silent no-op the other way round.
		if flag, gated := FlagGated(ext); gated {
			return nil, fmt.Errorf("allow_extensions: %q is controlled by attachments.%s — set %s = true instead of listing the extension", ext, flag, flag)
		}
		switch {
		case len(types) == 0 && perExt[ext] == nil:
			perExt[ext] = []string{AnyType} // unknown format: extension half only
		case len(types) == 0:
			// Already built in: a bare entry keeps the built-in type set.
		default:
			perExt[ext] = unionTypes(perExt[ext], types)
		}
	}

	// deny_extensions SUBTRACTS. Deny always wins.
	for _, raw := range s.DenyExtensions {
		ext, types, err := parseAllowEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("deny_extensions: %w", err)
		}
		if len(types) > 0 {
			return nil, fmt.Errorf("deny_extensions: %q must name an extension only — a deny removes the whole extension, it cannot remove one content type", raw)
		}
		delete(perExt, ext)
	}

	for ext, types := range perExt {
		slices.Sort(types)
		perExt[ext] = slices.Compact(types)
	}

	p := &Policy{set: s, perExt: perExt}
	p.canonical = p.buildCanonical()
	p.digest = digestOf(p.canonical)
	return p, nil
}

// Settings returns the normalized settings this Policy was built from.
func (p *Policy) Settings() Settings { return p.set }

// PolicyDigest returns a stable 16-hex-character fingerprint of everything
// that can change a Verdict: the effective extension -> permitted-types map,
// every cap, every flag, the mismatch mode and the scan hook. It is
// order-insensitive and reproducible across processes and machines.
//
// Store it in the state DB's meta table and on every skip row. When the
// computed digest differs from the stored one, unresolved skips may now be
// storable; `save status` reports the count and `save refetch` — never an
// automatic sync — actually fetches them.
//
// attachments.quarantine is deliberately NOT part of the digest: it changes
// how a stored file is tagged, never whether it is stored.
func (p *Policy) PolicyDigest() string { return p.digest }

// Canonical returns the exact newline-separated text PolicyDigest hashes.
// Exported for diagnostics (`save doctor`) and for tests that need to see
// what changed.
func (p *Policy) Canonical() string { return p.canonical }

// AllowedExtensions returns every effectively allowlisted extension, sorted,
// without leading dots.
func (p *Policy) AllowedExtensions() []string {
	out := make([]string, 0, len(p.perExt))
	for ext := range p.perExt {
		out = append(out, ext)
	}
	slices.Sort(out)
	return out
}

// PermittedTypes returns the sniffed content types permitted for ext (already
// normalized, no leading dot), or nil when ext is not allowlisted. A single
// AnyType element means any content is accepted for that extension. The
// tolerated non-mismatches (see Decide) are NOT listed here.
func (p *Policy) PermittedTypes(ext string) []string {
	types, ok := p.perExt[ext]
	if !ok {
		return nil
	}
	return slices.Clone(types)
}

// MaxSizeFor returns the per-attachment cap in bytes for a mail (chat=false)
// or Chat (chat=true) attachment; <= 0 means no cap.
func (p *Policy) MaxSizeFor(chat bool) int64 {
	if chat && p.set.ChatMaxSize > 0 {
		return p.set.ChatMaxSize
	}
	return p.set.MaxSize
}

// Input is one attachment presented for a decision.
type Input struct {
	// Name is the FINAL sanitized filename — the output of
	// naming.SanitizeFilename (and naming.Unique), not the sender's raw
	// filename. The extension is normalized from this exact string, so the
	// decision matches the name that would land on disk. Empty means the part
	// carried no filename at all.
	Name string

	// Content is the fully decoded attachment (transfer encoding already
	// removed). Decide sniffs it and charges len(Content) against the caps;
	// nil is a legitimate 0-byte attachment.
	Content []byte

	// Size is the decoded byte count used when Content is nil — the
	// pre-download path (PreCheck on a Chat blob, or a Gmail part whose size
	// is known from the metadata).
	Size int64

	// Chat selects chat_max_size instead of max_size.
	Chat bool

	// PartsSoFar is how many attachment parts of this message have already
	// been accepted; MessageBytesSoFar is their total stored bytes.
	PartsSoFar        int
	MessageBytesSoFar int64

	// RunBytesSoFar is the total attachment bytes stored by this sync pass.
	RunBytesSoFar int64

	// FreeSpace is the bytes currently free on the archive volume (from
	// unix.Statfs). Zero or negative means "unknown" and disables the
	// free-space check for this decision.
	FreeSpace int64

	// ScanFlagged is true when attachments.scan_command exited 1 (infected)
	// for this file. Any other non-zero exit is an error and must never be
	// presented as a clean or a flagged result.
	ScanFlagged bool
}

// EffectiveSize is the byte count charged against every cap: len(Content)
// whenever content is present, otherwise the caller-supplied Size.
func (in Input) EffectiveSize() int64 {
	if in.Content != nil {
		return int64(len(in.Content))
	}
	return in.Size
}

// Verdict is the decision. Store and Reason are independent: Store == false
// always carries a Reason, and Store == true carries one only when the file
// was stored under protest (see Warned).
type Verdict struct {
	// Store is whether the bytes may be written into the archive.
	Store bool

	// Reason is "" for a clean store, otherwise one of the Reason constants.
	Reason string

	// SniffedType is the bare "type/subtype" mimetype detected from the
	// content, with any charset parameter stripped. Decide always fills it
	// in, even when refusing — an honest skip record needs it. PreCheck
	// leaves it empty because it never looks at content.
	SniffedType string

	// NormalizedExt is the extension the decision was keyed on, without a
	// leading dot: normalized from Input.Name, or derived from SniffedType
	// when the part had no filename extension (see DerivedExt). It is "" only
	// when neither was available.
	NormalizedExt string

	// DerivedExt reports that NormalizedExt came from the sniffed content
	// rather than from the filename. Callers must record the derived
	// extension in the note and append it to the stored filename.
	DerivedExt bool

	// Detail is a short human-readable elaboration for the note. It is never
	// parsed; Reason is the machine-readable field.
	Detail string
}

// Warned reports a file stored under protest: on_mismatch = "store-warn" over
// an extension/content disagreement, or scan_action = "record" over a flagged
// scan. The note must show the warning next to the link.
func (v Verdict) Warned() bool { return v.Store && v.Reason != ReasonNone }

// Decide is the authoritative decision over the real bytes.
//
// Order of evaluation, which fixes which Reason wins when several apply:
//
//  1. extension-level policy (macro Office, SVG, containers, deny/allow list)
//  2. size caps and budgets (per attachment, per message, per run, free space)
//  3. content: sniff, extensionless derivation, permitted-type check
//  4. SVG content, vbaProject.bin in an OOXML central directory
//  5. the scan hook's verdict
//
// Policy comes before size on purpose: a 200 MB .exe must report
// not_allowlisted_extension, so that raising max_size can never turn its
// recorded skip into something `save refetch` would fetch.
//
// Tolerated non-mismatches, so the rule is not noisy — none of these is
// reported as a mismatch:
//   - a 0-byte attachment (always sniffs text/plain) under any allowlisted
//     extension;
//   - text/plain and the other inert text-data types under a text-family
//     extension (txt md log csv tsv json xml yaml yml ics vcf rtf asc sig pgp),
//     since mimetype's text subtypes are heuristic — text/html, image/svg+xml
//     and the script text types are deliberately NOT tolerated;
//   - any "+xml" type under .xml and any "+json" type under .json;
//   - application/x-ole-storage under doc/xls/ppt/msg, which are genuinely
//     indistinguishable (mimetype returns an empty extension for all of them,
//     so the extension is the only discriminator);
//   - application/octet-stream under a small p7s/p7m/asc/sig/pgp crypto blob.
func (p *Policy) Decide(in Input) Verdict { return p.decide(in, true) }

// PreCheck is the conservative pre-download filter for sources that would
// otherwise pay to fetch bytes they must then throw away (Google Chat, where
// skipping genuinely avoids the download). It applies only the rules that are
// decidable without content: the extension allowlist and the size caps.
//
// A PreCheck Verdict with Store == true DOES NOT authorize storage — it only
// means "nothing decidable without the bytes refuses this". The caller must
// still run Decide on the fetched content before writing. A PreCheck Verdict
// with Store == false is final and must be recorded like any other skip;
// SniffedType is empty because nothing was fetched, and the note must say so
// ("never fetched") rather than implying bytes were downloaded and discarded.
func (p *Policy) PreCheck(in Input) Verdict { return p.decide(in, false) }

func (p *Policy) decide(in Input, withContent bool) Verdict {
	v := Verdict{NormalizedExt: NormalizeExt(in.Name)}
	size := in.EffectiveSize()

	// Sniff once, up front, even when the decision is already lost on the
	// extension: an honest skip record carries the sniffed type.
	var mt *mimetype.MIME
	if withContent {
		mt = mimetype.Detect(in.Content)
		v.SniffedType = essence(mt.String())
	}

	// 1. Extension-level policy.
	if v.NormalizedExt != "" {
		if reason, detail, denied := p.extDenied(v.NormalizedExt); denied {
			v.Reason, v.Detail = reason, detail
			return v
		}
		if _, ok := p.perExt[v.NormalizedExt]; !ok {
			v.Reason = ReasonNotAllowlistedExtension
			v.Detail = "." + v.NormalizedExt + " is not on the attachment allowlist"
			return v
		}
	}

	// 2. Caps and budgets.
	if reason, detail, over := p.overCap(in, size); over {
		v.Reason, v.Detail = reason, detail
		return v
	}

	if !withContent {
		// Everything decidable without the bytes passed. Not an authorization.
		v.Store = true
		return v
	}

	// 3. Content.
	ext := v.NormalizedExt
	if ext == "" {
		derived, reason, detail := p.deriveExt(mt, v.SniffedType)
		if reason != ReasonNone {
			v.Reason, v.Detail = reason, detail
			return v
		}
		ext, v.NormalizedExt, v.DerivedExt = derived, derived, true
		v.Detail = "no filename extension; ." + derived + " derived from the sniffed type"
	}

	// 4a. SVG content is a program wherever it hides.
	if !p.set.AllowSVG && v.SniffedType == "image/svg+xml" {
		v.Reason = ReasonSVGDenied
		v.Detail = "content sniffed as image/svg+xml; SVG is denied unless attachments.allow_svg = true"
		return v
	}

	if !p.permits(ext, v.SniffedType, size) {
		if v.DerivedExt {
			v.Reason = ReasonNotAllowlistedContent
			v.Detail = "no filename extension and sniffed " + v.SniffedType + " is not storable on its own"
			return v
		}
		v.Reason = ReasonExtensionContentMismatch
		v.Detail = "." + ext + " does not permit sniffed " + v.SniffedType
		if p.set.OnMismatch == OnMismatchStoreWarn {
			v.Store = true // stored under protest; Warned() is true
			return v
		}
		return v
	}

	// 4b. Macro hardening: central-directory names only, never inflated.
	if !p.set.AllowMacroOffice && ooxmlTypes[v.SniffedType] && HasVBAProject(in.Content) {
		v.Reason = ReasonMacroOffice
		v.Detail = "zip central directory names a vbaProject.bin (a macro-enabled Office file, whatever the extension says)"
		return v
	}

	// 5. The scan hook.
	if in.ScanFlagged {
		v.Reason = ReasonScanHookFlagged
		v.Store = p.set.ScanAction != ScanActionReject
		if v.Store {
			v.Detail = "scan_command flagged this file; stored anyway because scan_action = \"record\""
		} else {
			v.Detail = "scan_command flagged this file and scan_action = \"reject\""
		}
		return v
	}

	v.Store = true
	return v
}

// extDenied reports the extension-level refusals, most specific first, so the
// Reason names the flag that would undo it.
func (p *Policy) extDenied(ext string) (reason, detail string, denied bool) {
	if why, hard := HardDenied(ext); hard {
		return ReasonNotAllowlistedExtension, why, true
	}
	if !p.set.AllowMacroOffice && macroOfficeExts[ext] {
		return ReasonMacroOffice, "." + ext + " is macro-enabled Office; content sniffing cannot see the difference (mimetype reports it as plain docx/xlsx/pptx), so the extension is the only defence — set attachments.allow_macro_office = true to keep it", true
	}
	if !p.set.AllowSVG && ext == "svg" {
		return ReasonSVGDenied, "SVG is the one image format that is a program, and Obsidian (Electron) renders it in-note without a double-click — set attachments.allow_svg = true to keep it", true
	}
	if !p.set.AllowContainers && ext == "zip" {
		return ReasonContainerDenied, "attachments.allow_containers = false", true
	}
	return ReasonNone, "", false
}

// overCap applies the size caps in a fixed order: per attachment, per
// message (bytes then parts), per run, then the free-space floor.
func (p *Policy) overCap(in Input, size int64) (reason, detail string, over bool) {
	if limit := p.MaxSizeFor(in.Chat); limit > 0 && size > limit {
		key := "attachments.max_size"
		if in.Chat && p.set.ChatMaxSize > 0 {
			key = "attachments.chat_max_size"
		}
		return ReasonOverSizeCap, fmt.Sprintf("%d bytes exceeds %s (%d)", size, key, limit), true
	}
	if p.set.MaxPerMessage > 0 && in.MessageBytesSoFar+size > p.set.MaxPerMessage {
		return ReasonOverMessageBudget, fmt.Sprintf("%d bytes on top of %d already stored for this message exceeds attachments.max_per_message (%d)",
			size, in.MessageBytesSoFar, p.set.MaxPerMessage), true
	}
	if p.set.MaxPartsPerMessage > 0 && in.PartsSoFar >= p.set.MaxPartsPerMessage {
		return ReasonOverMessageBudget, fmt.Sprintf("this message already has %d stored parts, the attachments.max_parts_per_message limit",
			p.set.MaxPartsPerMessage), true
	}
	if p.set.RunBudget > 0 && in.RunBytesSoFar+size > p.set.RunBudget {
		return ReasonOverRunBudget, fmt.Sprintf("%d bytes on top of %d already stored this pass exceeds attachments.run_budget (%d) — the next pass retries",
			size, in.RunBytesSoFar, p.set.RunBudget), true
	}
	if p.set.FreeSpaceFloor > 0 && in.FreeSpace > 0 && in.FreeSpace-size < p.set.FreeSpaceFloor {
		return ReasonFreeSpaceFloor, fmt.Sprintf("writing %d bytes would leave %d free, under attachments.free_space_floor (%d); the note is still written",
			size, in.FreeSpace-size, p.set.FreeSpaceFloor), true
	}
	return ReasonNone, "", false
}

// deriveExt resolves an extensionless part from its content: mimetype's
// canonical extension for the sniffed type, which must itself be allowlisted.
// application/octet-stream (nothing matched) and the types mimetype has no
// extension for (application/x-ole-storage, shared by doc/xls/ppt/msg) cannot
// be resolved and are refused.
func (p *Policy) deriveExt(mt *mimetype.MIME, sniffed string) (ext, reason, detail string) {
	if sniffed == octetStream {
		return "", ReasonNotAllowlistedContent, "no filename extension and the content matched no known format (application/octet-stream)"
	}
	derived := NormalizeExt("x." + strings.TrimPrefix(mt.Extension(), "."))
	if derived == "" {
		return "", ReasonNotAllowlistedContent, "no filename extension and " + sniffed + " has no unambiguous extension (doc, xls, ppt and msg all sniff this way)"
	}
	if r, d, denied := p.extDenied(derived); denied {
		// A flag-gated refusal is more actionable than a generic one.
		if r != ReasonNotAllowlistedExtension {
			return "", r, d
		}
		return "", ReasonNotAllowlistedContent, d
	}
	if _, ok := p.perExt[derived]; !ok {
		return "", ReasonNotAllowlistedContent, "no filename extension and the derived ." + derived + " is not on the attachment allowlist"
	}
	return derived, ReasonNone, ""
}

// permits applies the extension's permitted-type set plus the documented
// tolerated non-mismatches.
func (p *Policy) permits(ext, sniffed string, size int64) bool {
	types, ok := p.perExt[ext]
	if !ok {
		return false
	}
	if len(types) == 1 && types[0] == AnyType {
		return true
	}
	if slices.Contains(types, sniffed) {
		return true
	}
	// A 0-byte attachment always sniffs text/plain; store it under its
	// declared allowlisted extension without complaint.
	if size == 0 {
		return true
	}
	if textFamilyExts[ext] && textFamilyTypes[sniffed] {
		return true
	}
	if ext == "xml" && strings.HasSuffix(sniffed, "+xml") && sniffed != "image/svg+xml" {
		return true
	}
	if ext == "json" && strings.HasSuffix(sniffed, "+json") {
		return true
	}
	if oleFamilyExts[ext] && sniffed == oleStorage {
		return true
	}
	if cryptoBlobExts[ext] && sniffed == octetStream && size <= cryptoBlobMaxBytes {
		return true
	}
	return false
}

// Sniff returns the bare "type/subtype" mimetype detects for content, with any
// charset parameter stripped. Callers should record it on every skip, stored
// or not: it is what makes the record honest and re-decidable offline.
func Sniff(content []byte) string { return essence(mimetype.Detect(content).String()) }

// essence strips the optional parameters mimetype appends to text types
// ("text/plain; charset=utf-8") and lowercases the result.
func essence(s string) string {
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// unionTypes merges b into a without duplicates, dropping the AnyType
// sentinel once a concrete type is known.
func unionTypes(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, t := range append(slices.Clone(a), b...) {
		if t != AnyType && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
