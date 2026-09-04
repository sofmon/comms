// Package archive owns everything written beneath the archive root: the
// atomic file writer for the merged YYYY/MM/DD tree and all Markdown
// rendering (email documents and chat day-file projections).
//
// Write ordering is load-bearing: WriteEmail lands every attachment before
// the .md file, and every individual write is staged in the Root/.tmp
// scratch directory and atomically renamed into place (same filesystem), so
// a crash can leave stray temp files in .tmp (cleaned by SweepTemp) but
// never a half-written or dangling-link archive file.
//
// The archive root normally lives inside an iCloud Drive container, which
// adds three failure modes an ordinary directory does not have, all handled
// in writeAtomic and all reported as errors so the caller never commits a
// state-DB row for a write that did not really happen:
// names iCloud silently refuses to sync (ErrSyncExcluded), files whose
// contents have been evicted to the cloud (ErrDataless), and renames the
// FileProvider observes and reconciles asynchronously (ErrRenameUnconfirmed).
//
// # Attachments the policy refused
//
// comms/internal/policy decides what may be stored; this package is where the
// refusals become visible. Nothing is ever dropped silently, so every refused
// part is rendered twice into its note — once as a machine-readable
// skipped_attachments frontmatter entry, once as a bolded block under
// "## Attachments" — and SkippedRows turns the same refusals into the state
// DB rows that make them re-decidable and re-fetchable later. The note and
// the ledger are built from one EmailDoc so they cannot disagree.
//
// Because a widened policy has to be able to turn a skip record back into a
// real link, RewriteEmail re-renders an already-archived note in place and
// returns its new content hash. That is the piece `comms refetch` needs; chat
// day files need no equivalent, being whole-file projections already.
//
// # Provenance metadata
//
// Attachment files — never comms's own .md notes — are tagged with
// com.apple.quarantine and com.apple.metadata:kMDItemWhereFroms on the staged
// temp file, before the rename, so the attributes are never missing from a
// path anything can observe. See QuarantineMode for the honest scope of what
// that buys: a Gatekeeper consent prompt and Office Protected View, and
// specifically NOT a malware scan.
package archive

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/renameio/v2"
	"gopkg.in/yaml.v3"

	"comms/internal/emailpipe"
	"comms/internal/naming"
	"comms/internal/policy"
	"comms/internal/state"
)

// Writer writes archive files under Root. TZ is the pinned archive timezone
// (resolved once at startup from meta.archive_tz); all day bucketing and
// in-file times render in it.
//
// Dataless and Stat are injection seams — both nil in production, where the
// platform defaults apply. They exist so the iCloud-specific guards below can
// be exercised on any OS.
type Writer struct {
	Root string
	TZ   *time.Location

	// SpamRoot is the root of the parallel tree noise triage moves notes
	// into, mirroring Root's YYYY/MM/DD layout at identical rel paths. Every
	// note is first written under Root; only a state.DispositionSpam row
	// resolves here. Empty means no spam tree is configured, and resolving a
	// spam disposition is then an error rather than a silent fallback to Root.
	SpamRoot string

	// Quarantine controls com.apple.quarantine tagging of attachment files
	// (never of .md notes). The zero value tags, matching the
	// attachments.quarantine = true config default; pass
	// QuarantineFor(cfg.Attachments.Quarantine) to honour the operator.
	Quarantine QuarantineMode

	// Dataless reports whether the absolute path is an iCloud placeholder
	// whose contents have been evicted (SF_DATALESS). nil means the platform
	// default: the real st_flags check on macOS, a constant false elsewhere.
	Dataless func(path string) (bool, error)

	// Stat confirms a completed rename, and confirms that RewriteEmail's
	// target note exists. nil means os.Lstat.
	Stat func(path string) (os.FileInfo, error)

	// TagOrigin stamps the macOS provenance xattrs onto a staged temp file.
	// nil means the platform default: real setxattr calls on macOS, a no-op
	// elsewhere.
	TagOrigin func(path string, o Origin) error
}

// Sentinel errors for the archive-integrity guards. Callers classify with
// errors.Is to route the item into the failures ledger: all three mean "this
// write did not happen", so no state-DB row may be committed for it. Each is
// a bare noun phrase (like fs.ErrNotExist); the writer wraps it with the
// "archive: ..." context that says which write failed.
var (
	// ErrDataless means the write would have clobbered — or moved — a file
	// whose only copy currently lives in iCloud.
	ErrDataless = errors.New("file contents evicted to iCloud (dataless)")

	// ErrRenameUnconfirmed means the atomic rename reported success but the
	// destination did not afterwards stat as a regular file of the expected
	// size. The bytes on disk are not trustworthy.
	ErrRenameUnconfirmed = errors.New("rename could not be confirmed")

	// ErrSyncExcluded means a destination name would be silently refused by
	// iCloud Drive: the file would be written, hashed and verified locally and
	// never leave the machine.
	ErrSyncExcluded = errors.New("name is excluded from iCloud sync")

	// ErrNoteMissing means RewriteEmail was pointed at a note that is not
	// there. Unlike the three above it is not an integrity failure: it says
	// the message has never been archived, so the caller should fall back to
	// WriteEmail (and, if it thought the note existed, reconcile its state
	// DB) rather than retry.
	ErrNoteMissing = errors.New("email note does not exist")
)

// EmailMeta is the source-level metadata a connector adds to a rendered
// EmailDoc; together they fully determine the .md bytes and path.
//
// Source and SourceTag are the two renderings of one account instance and
// are NOT interchangeable: Source ("gmail:work") is the state key, the hash
// input and the frontmatter provenance; SourceTag ("gmail-work") is the only
// one that may appear in a filename.
type EmailMeta struct {
	Source       string // instance id: "<kind>:<label>", e.g. "gmail:work"
	SourceTag    string // file tag: "<kind>-<label>", e.g. "gmail-work"
	Account      string // the account's email address
	AccountLabel string // the account's config label, e.g. "work"
	StableID     string // immutable per-account message id
	ThreadID     string
	Labels       []string // Gmail label names / JMAP mailbox names
	// ServerTime is the server-assigned instant (Gmail internalDate, JMAP
	// receivedAt) in UTC; it alone drives day bucketing.
	ServerTime time.Time

	// SkipDisposition says what became of the bytes of any attachment the
	// policy refused, and is the only thing that lets the note describe a
	// skip truthfully. Both mail connectors download the whole message, so
	// both should pass SkipBytesDiscarded; the renderer never guesses.
	SkipDisposition SkipDisposition

	// Disposition is which tree the note lives in — the messages row's
	// state.Disposition, which the caller must have read from the state DB.
	// The zero value means the archive tree. WriteEmail accepts only that:
	// a new note is always written under Root, and only noise triage moves
	// it. RewriteEmail honours it, so a refetch re-renders a spam-filed note
	// in place under SpamRoot instead of forking a second copy under Root.
	//
	// It is unrelated to SkipDisposition above, which is about an
	// attachment's bytes; this is about the note as a whole.
	Disposition state.Disposition
}

// RootFor returns the tree a disposition selects: Root for the archive
// (and for the zero value), SpamRoot for spam. It is the single place the
// disposition-to-root mapping lives; every absolute path in this program
// that starts from a messages.rel_path must come through here or NotePath.
func (w *Writer) RootFor(d state.Disposition) (string, error) {
	switch d {
	case "", state.DispositionArchive:
		if w.Root == "" {
			return "", errors.New("archive: writer root not set")
		}
		return w.Root, nil
	case state.DispositionSpam:
		if w.SpamRoot == "" {
			return "", errors.New("archive: a note is recorded as spam but no spam tree is configured (spam_root)")
		}
		return w.SpamRoot, nil
	}
	return "", fmt.Errorf("archive: unknown note disposition %q (one of %v)", d, state.Dispositions())
}

// NotePath resolves a root-relative path to its absolute location under the
// tree disposition d selects.
func (w *Writer) NotePath(rel string, d state.Disposition) (string, error) {
	if err := checkRel(rel); err != nil {
		return "", fmt.Errorf("archive: note path: %w", err)
	}
	root, err := w.RootFor(d)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// origin is the provenance stamped onto this message's attachment files.
// The Message-ID identifies the message to a human reading Finder's "Where
// from"; the opaque per-account stable id is the fallback when the sender
// omitted one.
func (m EmailMeta) origin(doc *emailpipe.EmailDoc) Origin {
	ref := doc.MessageID
	if oneLine(ref) == "" {
		ref = m.StableID
	}
	return Origin{Source: m.Source, Ref: ref}
}

// WriteEmail writes one email: every attachment/inline file first, then the
// .md — each write an atomic rename staged in Root/.tmp — and returns the
// .md path relative to Root plus the sha256 hex of its bytes. Identical
// inputs produce byte-identical files at identical paths, so replays are
// no-ops.
//
// Only doc.Files are written. doc.Skipped — the parts the attachment policy
// refused — are rendered into the note, in the frontmatter and visibly under
// "## Attachments", and are never written anywhere. Pair this with
// SkippedRows to record the same refusals in the state DB.
func (w *Writer) WriteEmail(doc *emailpipe.EmailDoc, meta EmailMeta) (relPath, contentHash string, err error) {
	return w.writeEmail(doc, meta, false)
}

// RewriteEmail re-renders an email note that already exists, writing any
// attachments in doc.Files that are not yet on disk first, then atomically
// replacing the .md and returning its new content hash. The caller must
// update messages.content_hash with it in the same commit.
//
// This is the counterpart to `comms refetch`: a widened policy turns a skip
// into a real attachment, and without a re-render the bytes would land on
// disk while the note still said they were skipped. It is deliberately a
// separate entry point from WriteEmail — it refuses to CREATE a note,
// returning ErrNoteMissing instead, so a refetch that has drifted onto the
// wrong path cannot quietly fork a second copy of a message.
//
// It never deletes: a narrowed policy leaves already-archived attachment
// files in place, and the note simply stops linking them.
func (w *Writer) RewriteEmail(doc *emailpipe.EmailDoc, meta EmailMeta) (relPath, contentHash string, err error) {
	return w.writeEmail(doc, meta, true)
}

func (w *Writer) writeEmail(doc *emailpipe.EmailDoc, meta EmailMeta, mustExist bool) (relPath, contentHash string, err error) {
	if err := w.check(); err != nil {
		return "", "", err
	}
	if doc == nil {
		return "", "", errors.New("archive: write email: nil doc")
	}
	if meta.Source == "" || meta.SourceTag == "" || meta.StableID == "" {
		return "", "", errors.New("archive: write email: source (instance id), source tag and stable id are required")
	}
	if _, _, ok := state.SplitInstance(meta.Source); !ok {
		return "", "", fmt.Errorf("archive: write email %s/%s: source must be an instance id \"<kind>:<label>\" (a bare source kind would make two accounts share one archive)",
			meta.Source, meta.StableID)
	}
	// The instance id must never reach a filename; the tag is the only form
	// allowed in a stem. '_' is rejected too: it separates the stem's
	// fields, so a tag containing one would make the stem unparseable.
	if meta.SourceTag != state.Tag(meta.Source) || strings.ContainsAny(meta.SourceTag, `:/\_`) {
		return "", "", fmt.Errorf("archive: write email %s/%s: source tag %q must be the file tag of the source, %q",
			meta.Source, meta.StableID, meta.SourceTag, state.Tag(meta.Source))
	}
	if meta.ServerTime.IsZero() {
		return "", "", fmt.Errorf("archive: write email %s/%s: server time is required", meta.Source, meta.StableID)
	}
	// The disposition decides whether the note claims the refused bytes ever
	// crossed the network. An unrecognized value is a connector bug, and must
	// not reach a reader as a plausible-looking sentence.
	if !validSkipDisposition(meta.SkipDisposition) {
		return "", "", fmt.Errorf("archive: write email %s/%s: unknown skip disposition %q (use SkipBytesDiscarded or SkipBytesNotFetched)",
			meta.Source, meta.StableID, meta.SkipDisposition)
	}
	// A new note is always written into the archive tree: connectors do not
	// classify, triage does, and it moves notes after the fact. Only a
	// re-render follows the row's disposition, so a refetch of a spam-filed
	// note lands on the copy that exists rather than creating a second one.
	if !mustExist && meta.Disposition != "" && meta.Disposition != state.DispositionArchive {
		return "", "", fmt.Errorf("archive: write email %s/%s: a new note cannot be written with disposition %q — notes are written to the archive tree and moved by triage",
			meta.Source, meta.StableID, meta.Disposition)
	}
	root, err := w.RootFor(meta.Disposition)
	if err != nil {
		return "", "", fmt.Errorf("archive: write email %s/%s: %w", meta.Source, meta.StableID, err)
	}

	local := meta.ServerTime.In(w.TZ)
	dayDir := naming.DayDir(local)
	// Stem: the file tag. Hash: the instance id, so two accounts that
	// archive the same message id still get distinct hashes.
	stem := naming.EmailStem(local, meta.SourceTag, doc.Subject, naming.Hash8(meta.Source+":"+meta.StableID))
	attachDir := naming.AttachDir(stem)
	rel := path.Join(dayDir, stem+".md")

	// Validate every rel before writing anything: the connector must have
	// rendered with the attach dir this writer derives, or the body's
	// links and the sibling-.d layout invariant would silently break.
	// checkDest additionally rejects anything iCloud would refuse to sync or
	// the filesystem could not open, so a doomed message fails whole rather
	// than stranding half its attachments on disk.
	for _, f := range doc.Files {
		if err := checkRel(f.Rel); err != nil {
			return "", "", fmt.Errorf("archive: write email %s/%s: %w", meta.Source, meta.StableID, err)
		}
		if !strings.HasPrefix(f.Rel, attachDir+"/") || len(f.Rel) == len(attachDir)+1 {
			return "", "", fmt.Errorf("archive: write email %s/%s: attachment rel %q is not under %q (attach dir mismatch)",
				meta.Source, meta.StableID, f.Rel, attachDir)
		}
		if err := w.checkDest(root, path.Join(dayDir, f.Rel)); err != nil {
			return "", "", fmt.Errorf("archive: write email %s/%s: %w", meta.Source, meta.StableID, err)
		}
	}
	if err := w.checkDest(root, rel); err != nil {
		return "", "", fmt.Errorf("archive: write email %s/%s: %w", meta.Source, meta.StableID, err)
	}
	// A re-render must find the note it claims to be re-rendering, and must
	// find it before anything is written, so a mis-targeted refetch leaves no
	// trace at all.
	if mustExist {
		if err := w.mustExist(root, rel); err != nil {
			return "", "", fmt.Errorf("archive: rewrite email %s/%s: %w", meta.Source, meta.StableID, err)
		}
	}

	// Attachment files carry the macOS provenance xattrs; the .md note does
	// not — it is comms's own text, not something that arrived from outside.
	origin := meta.origin(doc)
	for _, f := range doc.Files {
		if err := w.writeAtomic(root, writeSpec{
			rel:     path.Join(dayDir, f.Rel),
			content: f.Content,
			tag:     true,
			origin:  origin,
		}); err != nil {
			return "", "", err
		}
	}

	content, err := renderEmailMD(doc, meta)
	if err != nil {
		return "", "", fmt.Errorf("archive: write email %s/%s: %w", meta.Source, meta.StableID, err)
	}
	if err := w.writeAtomic(root, writeSpec{rel: rel, content: content}); err != nil {
		return "", "", err
	}
	return rel, hashBytes(content), nil
}

// mustExist reports ErrNoteMissing unless rel is already a regular file
// under root. It reuses the Writer's Stat seam so the check behaves the same
// in tests as the post-rename confirmation does.
func (w *Writer) mustExist(root, rel string) error {
	stat := os.Lstat
	if w.Stat != nil {
		stat = w.Stat
	}
	fi, err := stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("%w: %s (write it with WriteEmail first)", ErrNoteMissing, rel)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file (mode %v)", ErrNoteMissing, rel, fi.Mode().Type())
	}
	return nil
}

// SkippedRows converts the parts the attachment policy refused into state DB
// rows, ready for state.DB.UpsertSkippedBatch. noteRelPath is what WriteEmail
// returned for the same document.
//
// It exists so the note and the ledger cannot disagree: both are derived here
// from the same EmailDoc, the same EmailMeta and the same note path, which is
// exactly the agreement state.SkippedAttachment requires.
//
// ContentSHA256 is left empty. By the time the policy has refused a part its
// bytes are already released (emailpipe.File.Content is nil exactly when
// Skipped), so a connector that still holds the raw message should fill the
// hash in before recording; a row without one is still valid, refetch simply
// cannot verify against it.
func (w *Writer) SkippedRows(doc *emailpipe.EmailDoc, meta EmailMeta, noteRelPath string) ([]state.SkippedAttachment, error) {
	if err := w.check(); err != nil {
		return nil, err
	}
	if doc == nil || len(doc.Skipped) == 0 {
		return nil, nil
	}
	if noteRelPath == "" {
		return nil, errors.New("archive: skipped rows: note rel path is required (a skip nobody can find in a note is a silent drop)")
	}
	day := naming.DayBucket(meta.ServerTime.In(w.TZ))
	rows := make([]state.SkippedAttachment, 0, len(doc.Skipped))
	for _, f := range doc.Skipped {
		rows = append(rows, state.SkippedAttachment{
			Source:        meta.Source,
			StableID:      meta.StableID,
			PartKey:       f.PartKey,
			OrigName:      f.OrigName,
			SanitizedName: f.Name,
			SizeBytes:     f.Size,
			DeclaredType:  f.DeclaredType,
			DeclaredExt:   policy.NormalizeExt(f.Name),
			SniffedType:   f.SniffedType,
			Reason:        f.SkipReason,
			PolicyDigest:  doc.PolicyDigest,
			NoteRelPath:   noteRelPath,
			DayBucket:     day,
		})
	}
	return rows, nil
}

// WriteChatDay atomically replaces the chat day file at relPath (relative to
// Root) and returns the sha256 hex of content. relPath is validated exactly
// like an email destination (path budget, iCloud sync exclusion, dataless
// guard, rename confirmation).
//
// It writes comms's own rendered text, so nothing is quarantine-tagged. Chat
// attachment blobs are bytes that arrived from outside and must go through
// WriteChatAttachment instead.
func (w *Writer) WriteChatDay(relPath string, content []byte) (contentHash string, err error) {
	if err := w.check(); err != nil {
		return "", err
	}
	if err := checkRel(relPath); err != nil {
		return "", fmt.Errorf("archive: write chat day: %w", err)
	}
	// Chat day files are never triaged, so they always live in the archive
	// tree; resolving the root explicitly keeps that a stated fact rather
	// than an accident of which field was handy.
	root, err := w.RootFor(state.DispositionArchive)
	if err != nil {
		return "", err
	}
	if err := w.writeAtomic(root, writeSpec{rel: relPath, content: content}); err != nil {
		return "", err
	}
	return hashBytes(content), nil
}

// WriteChatAttachment atomically writes one downloaded chat attachment blob
// at relPath (relative to Root) and returns the sha256 hex of its bytes.
//
// It is WriteChatDay with the macOS provenance tagging that downloaded
// content should carry: origin identifies the space or message the blob came
// from, and shows up in Finder's "Where from".
func (w *Writer) WriteChatAttachment(relPath string, content []byte, origin Origin) (contentHash string, err error) {
	if err := w.check(); err != nil {
		return "", err
	}
	if err := checkRel(relPath); err != nil {
		return "", fmt.Errorf("archive: write chat attachment: %w", err)
	}
	// Chat blobs belong to chat day files, which are never triaged.
	root, err := w.RootFor(state.DispositionArchive)
	if err != nil {
		return "", err
	}
	if err := w.writeAtomic(root, writeSpec{rel: relPath, content: content, tag: true, origin: origin}); err != nil {
		return "", err
	}
	return hashBytes(content), nil
}

// SweepTemp empties the .tmp scratch directory of every configured root —
// its entries are renameio litter from a crash between temp-file creation
// and rename. It never touches anything outside .tmp: a root may hold
// pre-existing user files that are not this program's to delete. Returns
// the root-relative paths removed (the spam tree's prefixed with its root,
// so the log can tell the two apart). Called once at startup.
func (w *Writer) SweepTemp() (removed []string, err error) {
	if w.Root == "" {
		return nil, errors.New("archive: writer root not set")
	}
	removed, err = sweepTempUnder(w.Root, "", removed)
	if err != nil || w.SpamRoot == "" {
		return removed, err
	}
	return sweepTempUnder(w.SpamRoot, w.SpamRoot+"/", removed)
}

func sweepTempUnder(root, prefix string, removed []string) ([]string, error) {
	tmpDir := filepath.Join(root, TempDirName)
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return removed, nil // nothing staged yet
		}
		return removed, fmt.Errorf("archive: sweep temp: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(tmpDir, e.Name())); err != nil {
			return removed, fmt.Errorf("archive: sweep temp: %w", err)
		}
		removed = append(removed, prefix+path.Join(TempDirName, e.Name()))
	}
	return removed, nil
}

func (w *Writer) check() error {
	if w.Root == "" {
		return errors.New("archive: writer root not set")
	}
	if w.TZ == nil {
		return errors.New("archive: writer timezone not set")
	}
	return nil
}

// writeFrontmatter marshals v with yaml.v3 and wraps it in --- fences.
func writeFrontmatter(buf *bytes.Buffer, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frontmatter: %w", err)
	}
	buf.WriteString("---\n")
	buf.Write(b) // yaml.Marshal output always ends in \n
	buf.WriteString("---\n")
	return nil
}

// TempDirName is the scratch directory directly under Root that every
// atomic write stages its renameio temp file in. It lives inside Root so
// the final rename never crosses filesystems (staying atomic); SweepTemp
// empties it at startup, and all archive scans skip it.
const TempDirName = ".tmp"

// writeSpec is one atomic write: where it goes, what it holds, and whether
// the bytes came from outside and so deserve the macOS provenance tag.
type writeSpec struct {
	rel     string
	content []byte

	// tag requests com.apple.quarantine (and kMDItemWhereFroms, when origin
	// says anything). True for attachment files, false for comms's own .md
	// notes — tagging text this program generated would be a lie about where
	// it came from, and would make Obsidian's own files look downloaded.
	tag    bool
	origin Origin
}

// writeAtomic writes spec.content to root/spec.rel via a renameio temp file
// staged in root/.tmp (same filesystem, so the rename is atomic), replacing
// atomically. root is whichever tree the caller resolved through RootFor:
// the staging directory must live under the SAME root as the destination,
// which is why it is a parameter rather than always Root. The archive holds
// private mail: files are 0600, dirs 0700.
//
// Three guards wrap the rename, all of them there because the archive root
// normally sits inside an iCloud Drive container:
//
//   - checkDest rejects a destination iCloud would silently not sync, or that
//     busts the path budget, before anything is created for it.
//   - the dataless check refuses to rename over — or move — a placeholder
//     whose bytes have been evicted to iCloud. The destination is checked
//     twice, once before the bytes are staged (so a large attachment is not
//     written for nothing) and once immediately before the rename, since
//     eviction can happen in between on the nearly-full disk this is deployed
//     on; the staged temp file is checked once, just before it moves.
//   - confirmRename re-stats the destination afterwards, because the rename
//     is observed by the FileProvider and reconciled asynchronously; a
//     state-DB row must never be committed for a file that is not there.
//
// Quarantine tagging happens on the PENDING TEMP FILE, before the rename, so
// the attribute is never missing from a path anything can observe. See
// QuarantineMode: it is a consent prompt and a Protected View trigger, not a
// malware scan.
func (w *Writer) writeAtomic(root string, spec writeSpec) error {
	rel, content := spec.rel, spec.content
	if err := w.checkDest(root, rel); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := w.assertNotDataless(abs, "destination"); err != nil {
		return fmt.Errorf("archive: write %s: %w", rel, err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("archive: mkdir %s: %w", dir, err)
	}
	tmpDir := filepath.Join(root, TempDirName)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return fmt.Errorf("archive: mkdir %s: %w", tmpDir, err)
	}
	pf, err := renameio.NewPendingFile(abs,
		renameio.WithTempDir(tmpDir),
		renameio.WithStaticPermissions(0o600))
	if err != nil {
		return fmt.Errorf("archive: temp file for %s: %w", rel, err)
	}
	defer pf.Cleanup()
	if _, err := pf.Write(content); err != nil {
		return fmt.Errorf("archive: write %s: %w", rel, err)
	}
	// Tagging happens HERE, on the pending file, never after the rename.
	// TestQuarantineIsOnTheTempFileBeforeTheRename observes the attribute at
	// exactly this point through the Dataless seam below; moving this call
	// after CloseAtomicallyReplace reopens the untagged window.
	if err := w.tagPending(pf.Name(), spec); err != nil {
		return fmt.Errorf("archive: write %s: %w", rel, err)
	}
	// Last look before the rename: neither the file we are about to move nor
	// the one we are about to replace may be an evicted placeholder.
	if err := w.assertNotDataless(pf.Name(), "staged temp file"); err != nil {
		return fmt.Errorf("archive: write %s: %w", rel, err)
	}
	if err := w.assertNotDataless(abs, "destination"); err != nil {
		return fmt.Errorf("archive: write %s: %w", rel, err)
	}
	if err := pf.CloseAtomicallyReplace(); err != nil {
		return fmt.Errorf("archive: replace %s: %w", rel, err)
	}
	return w.confirmRename(rel, abs, int64(len(content)))
}

// tagPending applies the provenance xattrs to the staged temp file, if this
// write wants them and the operator has not turned tagging off.
//
// A failure here aborts the write while the file is still invisible, so an
// attachment never lands in the vault silently missing a tag it was supposed
// to carry. The one tolerated case — a volume with no extended-attribute
// support at all — is absorbed inside the platform implementation, since an
// untagged attachment still beats an unarchived one.
func (w *Writer) tagPending(tmpPath string, spec writeSpec) error {
	if !spec.tag || !w.Quarantine.enabled() {
		return nil
	}
	tag := tagOrigin
	if w.TagOrigin != nil {
		tag = w.TagOrigin
	}
	if err := tag(tmpPath, spec.origin); err != nil {
		return fmt.Errorf("quarantine tag: %w", err)
	}
	return nil
}

// checkDest validates one root-relative destination before the writer
// creates anything for it.
//
// Both failures are permanent for the item and actionable for the user, so
// they belong in the failures ledger rather than in a retry loop: an
// over-budget path is a property of the root, and an excluded name means a
// connector handed the writer a name that never went through
// naming.SanitizeFilename.
func (w *Writer) checkDest(root, rel string) error {
	if err := naming.CheckArchivePath(root, rel); err != nil {
		if errors.Is(err, naming.ErrPathTooLong) {
			which := "archive root"
			if root != w.Root && root == w.SpamRoot {
				which = "spam root"
			}
			return fmt.Errorf("cannot write %s: %w; the %s %q already uses %d of the %d-byte budget, leaving %d bytes for root-relative paths and this one needs %d — the %s is the likely cause: move it to a shorter path (`comms doctor` reports the remaining budget)",
				rel, err, which, root, len(root), naming.MaxPathBytes, naming.PathBudget(root), len(rel), which)
		}
		return fmt.Errorf("cannot write %s: %w", rel, err)
	}
	// rel is always archive-relative, so it can never legitimately contain
	// the deliberately-excluded <root>/.tmp staging component.
	if c := naming.FirstSyncExcluded(rel); c != "" {
		return fmt.Errorf("cannot write %s: %w: the component %q matches iCloud Drive's filename exclusion list, so the file would be written locally and never uploaded; every sender-supplied name must go through naming.SanitizeFilename before it reaches the writer",
			rel, ErrSyncExcluded, c)
	}
	return nil
}

// assertNotDataless fails when p is an evicted iCloud placeholder. what names
// p's role ("destination", "staged temp file") for the error message. A check
// that cannot be performed is also a failure: an unreadable flag is not proof
// that the file is safe to move or replace.
func (w *Writer) assertNotDataless(p, what string) error {
	dataless := isDataless
	if w.Dataless != nil {
		dataless = w.Dataless
	}
	switch ok, err := dataless(p); {
	case err != nil:
		return fmt.Errorf("dataless check on the %s %s: %w", what, p, err)
	case ok:
		return fmt.Errorf("%s %s: %w by \"Optimize Mac Storage\"; touching it would destroy the local trace of a file whose contents now exist only in the cloud — free disk space (or turn Optimize Mac Storage off), let the file materialize, and re-run",
			what, p, ErrDataless)
	}
	return nil
}

// confirmRename re-stats a just-renamed destination and reports an error
// unless it is a regular file holding exactly want bytes. Cheap insurance
// against the iCloud FileProvider reconciling the rename asynchronously (or
// otherwise interfering): the caller may only commit its state-DB row once
// this returns nil.
func (w *Writer) confirmRename(rel, abs string, want int64) error {
	stat := os.Lstat
	if w.Stat != nil {
		stat = w.Stat
	}
	fi, err := stat(abs)
	if err != nil {
		return fmt.Errorf("archive: %w for %s: the rename reported success but the destination could not be stat'ed: %w", ErrRenameUnconfirmed, rel, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("archive: %w for %s: the destination is not a regular file (mode %v)", ErrRenameUnconfirmed, rel, fi.Mode().Type())
	}
	if fi.Size() != want {
		return fmt.Errorf("archive: %w for %s: wrote %d bytes but the destination holds %d", ErrRenameUnconfirmed, rel, want, fi.Size())
	}
	return nil
}

// checkRel rejects rel paths that are empty, absolute, unclean, or escape
// the archive root.
func checkRel(rel string) error {
	if rel == "" {
		return errors.New("empty rel path")
	}
	if path.IsAbs(rel) || filepath.IsAbs(filepath.FromSlash(rel)) {
		return fmt.Errorf("absolute rel path %q", rel)
	}
	if path.Clean(rel) != rel {
		return fmt.Errorf("unclean rel path %q", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return fmt.Errorf("rel path %q escapes the archive root", rel)
		}
	}
	return nil
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mdLinkText escapes the characters that would break a [text](dest) link.
func mdLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}

// mdLinkDest wraps a link destination in angle brackets (CommonMark's form
// for destinations containing spaces) with the brackets themselves escaped.
func mdLinkDest(s string) string {
	s = strings.NewReplacer("<", "%3C", ">", "%3E", "\n", "%0A", "\r", "%0D").Replace(s)
	return "<" + s + ">"
}

// validStr replaces invalid UTF-8 so yaml.v3 marshaling cannot fail on
// hostile header bytes; the message must archive rather than error.
func validStr(s string) string {
	return strings.ToValidUTF8(s, "�")
}

func validStrs(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = validStr(s)
	}
	return out
}

// validMap is validStr over a map's keys and values; nil stays nil so an
// omitempty field is still omitted.
func validMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[validStr(k)] = validStr(v)
	}
	return out
}
