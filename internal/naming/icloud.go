package naming

// iCloud sync-exclusion rules.
//
// iCloud Drive (CloudDocs behind the modern FileProvider) SILENTLY refuses to
// sync any item whose NAME matches a built-in exclusion list. The item is
// written to disk, hashed into the state DB, reported clean by `comms verify`
// — and never leaves the machine. That is the worst failure mode this
// archiver can have, so every name it generates is checked here.
//
// Verified empirically on this machine (macOS 26.5.2, build 25F84,
// FileProvider 4018.120.24) inside the real target container
// iCloud~md~obsidian: `fileproviderctl evaluate` on probes named `file.nosync`
// (file), `tmpdir.nosync` (directory), `.tmp` (directory) and `file.tmp` all
// returned `isExcludedFromSync = 1` with
// `BRCloudDocsErrorDomain Code=83 "Excluded From Sync Due To Filename"`, while
// a control `plainfile.bin` in the same directory returned
// `isExcludedFromSync = 0; isUploaded = 1`. Children of an excluded directory
// are not even enumerated by the provider. The remainder of the list is the
// reverse-engineered CloudDocs filename filter.
//
// Two rules the archiver relies on, and which must NOT be "fixed" here:
//   - The staging directory <root>/.tmp is excluded on purpose. Its name is
//     load-bearing: nothing partially written is ever uploaded. Callers that
//     walk a path (FirstSyncExcluded) must exempt it deliberately.
//   - A leading dot does NOT exclude anything. That folklore is false: the
//     user's own `.obsidian` directory reports isExcludedFromSync = 0 and
//     isUploaded = 1. So `<stem>.d` attachment directories sync normally.

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// syncExcludedSubstrings are refused anywhere in a component's name.
var syncExcludedSubstrings = []string{".nosync", ".weakpkg"}

// syncExcludedNames are refused as a complete component name.
var syncExcludedNames = map[string]bool{
	"tmp":                 true,
	".tmp":                true,
	".ds_store":           true,
	".ubd":                true,
	"desktop.ini":         true,
	"$recycle.bin":        true,
	"dropbox":             true,
	".dropbox":            true,
	".dropbox.attr":       true,
	"onedrive":            true,
	"idrive-sync":         true,
	"microsoft user data": true,
	"icon\r":              true,
}

// syncExcludedExts are refused as a component's final extension (stored
// without the leading dot).
//
// "ubd" is listed by the sources ambiguously — as the bare name ".ubd" in
// some, as an extension alongside "tmp" in others. It is treated as BOTH
// here: the cost of the extra rewrite is one dead extension nobody attaches,
// the cost of guessing wrong the other way is a silent hole in the archive.
var syncExcludedExts = map[string]bool{
	"tmp":                     true,
	"ubd":                     true,
	"photoslibrary":           true,
	"photolibrary":            true,
	"aplibrary":               true,
	"migratedphotolibrary":    true,
	"migratedaperturelibrary": true,
}

// syncExcludedPrefixes are refused when a component's name starts with them
// (Office and Cocoa scratch files).
var syncExcludedPrefixes = []string{"~$", "(a document being saved"}

const (
	// syncSafeExt is appended to a name whose EXTENSION is excluded. The
	// offending extension stays in the stem, so the real type remains
	// discoverable ("draft.tmp" -> "draft.tmp.bin").
	syncSafeExt = ".bin"

	// syncSafeMark is prepended to a name whose WHOLE NAME or PREFIX is
	// excluded ("Dropbox" -> "_Dropbox", "~$q3.docx" -> "_~$q3.docx"). No
	// excluded name or prefix starts with '_', so one pass always suffices
	// and the extension is left untouched.
	syncSafeMark = "_"
)

// SyncExcluded reports whether iCloud Drive would silently refuse to sync a
// filesystem item with this name. name must be ONE path component; the result
// is undefined for a value containing a separator (use FirstSyncExcluded for
// a whole path).
//
// Matching is case-insensitive, applying the same per-rune fold as
// strings.ToLower, so "DROPBOX", "Draft.TMP" and "x.NoSync.pdf" all match.
func SyncExcluded(name string) bool {
	if name == "" {
		return false
	}
	low := strings.ToLower(name)
	for _, sub := range syncExcludedSubstrings {
		if strings.Contains(low, sub) {
			return true
		}
	}
	if syncExcludedNames[low] {
		return true
	}
	if ext := strings.TrimPrefix(path.Ext(low), "."); ext != "" && syncExcludedExts[ext] {
		return true
	}
	for _, p := range syncExcludedPrefixes {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// ensureSyncable rewrites name until SyncExcluded(name) is false, keeping the
// original recognizable and the real extension discoverable. The rewrite is:
//
//  1. Every case-insensitive occurrence of ".nosync" / ".weakpkg" has its
//     leading dot turned into an underscore ("plan.NoSync.pdf" ->
//     "plan_NoSync.pdf"). Nothing else can break a "contains" rule.
//  2. If the extension is excluded, ".bin" is appended, demoting the
//     offending extension into the stem ("draft.tmp" -> "draft.tmp.bin").
//  3. If the whole name, or its prefix, is excluded, "_" is prepended
//     ("Dropbox" -> "_Dropbox", "~$q3.docx" -> "_~$q3.docx",
//     "desktop.ini" -> "_desktop.ini").
//
// The steps run in that order and step 3 tests the result of step 2, so the
// postcondition !SyncExcluded(out) always holds: no rule can be reintroduced
// by a later step ('_' and ".bin" appear in no token, no excluded name or
// prefix begins with '_', and ".bin" is not an excluded extension).
//
// It is idempotent by construction: a name that is already syncable is
// returned untouched, so sanitizing an already-sanitized name is a no-op.
// Growth is bounded by 5 bytes (1 for the mark, 4 for the extension).
func ensureSyncable(name string) string {
	if name == "" || !SyncExcluded(name) {
		return name
	}
	// 1. Break every "contains" token. The match always begins at the
	// token's '.', and only '.' folds to '.', so exactly one ASCII byte is
	// rewritten and the length never changes.
	for _, tok := range syncExcludedSubstrings {
		for {
			i := indexFold(name, tok)
			if i < 0 {
				break
			}
			name = name[:i] + syncSafeMark + name[i+1:]
		}
	}
	// 2. Excluded extension: demote it into the stem.
	if ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), "."); ext != "" && syncExcludedExts[ext] {
		name += syncSafeExt
	}
	// 3. Excluded whole name or prefix: mark the front.
	low := strings.ToLower(name)
	if syncExcludedNames[low] {
		return syncSafeMark + name
	}
	for _, p := range syncExcludedPrefixes {
		if strings.HasPrefix(low, p) {
			return syncSafeMark + name
		}
	}
	return name
}

// indexFold returns the byte offset in s of the leftmost case-insensitive
// occurrence of the ASCII token, or -1. Matching is per-rune (unicode.ToLower,
// the same fold strings.ToLower applies) rather than over a lowercased copy,
// so the returned offset indexes s itself even when a multi-byte rune folds
// to a single ASCII byte.
func indexFold(s, token string) int {
	if token == "" {
		return -1
	}
	for i := range s { // rune start offsets only
		if matchFoldAt(s, i, token) {
			return i
		}
	}
	return -1
}

// matchFoldAt reports whether the ASCII token matches s at byte offset i
// under per-rune lowercasing.
func matchFoldAt(s string, i int, token string) bool {
	for k := 0; k < len(token); k++ {
		if i >= len(s) {
			return false
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if unicode.ToLower(r) != rune(token[k]) {
			return false
		}
		i += size
	}
	return true
}

// FirstSyncExcluded returns the first component of p that iCloud would
// silently refuse to sync, or "" when every component is syncable. Both
// slash- and OS-separated paths are accepted.
//
// The archiver's own <root>/.tmp staging directory is excluded ON PURPOSE, so
// callers checking a path that may contain it must skip that component
// themselves rather than treating the result as an error.
func FirstSyncExcluded(p string) string {
	for _, c := range strings.Split(filepath.ToSlash(p), "/") {
		if c != "" && SyncExcluded(c) {
			return c
		}
	}
	return ""
}

// Path budget.
//
// APFS caps a single name at 255 UTF-8 bytes. There is no documented
// whole-path cap, but backups and iCloud start failing somewhere around
// 1023-1024 bytes, so the archiver budgets 1000. That budget is tight in the
// intended deployment: the Obsidian container root
// "/Users/<user>/Library/Mobile Documents/iCloud~md~obsidian/Documents/<vault>"
// is already ~79 characters before "Communication/YYYY/MM/DD/<stem>.d/<file>"
// begins.
const (
	// MaxComponentBytes is the hard per-component filesystem limit. Names
	// this package generates are capped far below it (100 bytes); this is the
	// ceiling for components it merely passes through.
	MaxComponentBytes = 255

	// MaxPathBytes is the conservative whole-path budget for an absolute
	// path. Beyond it, writes succeed on APFS but tools (and iCloud) start
	// failing to open the result.
	MaxPathBytes = 1000
)

// ErrComponentTooLong and ErrPathTooLong classify the failures reported by
// CheckComponent, CheckPath and CheckArchivePath. Callers match them with
// errors.Is to route the item to the failures ledger.
var (
	ErrComponentTooLong = errors.New("naming: path component too long")
	ErrPathTooLong      = errors.New("naming: path exceeds the path budget")
)

// CheckComponent reports an error when name does not fit MaxComponentBytes.
func CheckComponent(name string) error {
	if len(name) > MaxComponentBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d: %q", ErrComponentTooLong, len(name), MaxComponentBytes, name)
	}
	return nil
}

// CheckPath validates p against both budgets: the whole path against
// MaxPathBytes and every component against MaxComponentBytes. p should be
// absolute — the whole-path budget is only meaningful for the path that
// actually reaches the kernel.
func CheckPath(p string) error {
	if len(p) > MaxPathBytes {
		return fmt.Errorf("%w: %d bytes exceeds %d: %q", ErrPathTooLong, len(p), MaxPathBytes, p)
	}
	for _, c := range strings.Split(filepath.ToSlash(p), "/") {
		if err := CheckComponent(c); err != nil {
			return err
		}
	}
	return nil
}

// CheckArchivePath validates the absolute path the archive writer is about to
// create, joining root with a slash-separated archive-relative path exactly
// as the writer does. A non-nil error means the write must fail loudly (a
// failures-ledger entry) instead of producing a path that cannot be opened.
func CheckArchivePath(root, rel string) error {
	return CheckPath(filepath.Join(root, filepath.FromSlash(rel)))
}

// PathBudget reports how many bytes remain under root for an
// archive-relative path (the separator after root is already deducted). It is
// negative when root alone busts the budget. Use it in preflight/doctor
// output: "YYYY/MM/DD/" plus a 100-byte stem plus ".d/" plus a 100-byte
// attachment name needs roughly 220 bytes of headroom.
func PathBudget(root string) int {
	return MaxPathBytes - len(root) - 1
}

// TruncateComponent caps one path component at MaxComponentBytes on a rune
// boundary, preserving a short extension, and applies the same reserved-device
// and iCloud-exclusion guards as SanitizeFilename. It returns fallback when
// nothing usable survives. It is idempotent.
func TruncateComponent(name, fallback string) string {
	if len(name) <= MaxComponentBytes && !SyncExcluded(name) {
		return name
	}
	ext := path.Ext(name)
	if len(ext) > extMaxBytes {
		ext = ""
	}
	if out, ok := capName(strings.TrimSuffix(name, ext), ext, MaxComponentBytes); ok {
		return out
	}
	return ensureSyncable(fallback)
}

// capName assembles base+ext so that the result is at most max bytes, is not
// a reserved device name, and is not SyncExcluded. base is shortened on a
// rune boundary until the guards' additions fit inside max; ok is false when
// nothing survives. Deterministic: the same inputs always yield the same name.
func capName(base, ext string, max int) (string, bool) {
	for budget := max - len(ext); budget > 0; {
		b := strings.Trim(truncateBytes(base, budget), " .")
		if b == "" {
			return "", false
		}
		cand := b + ext
		if reserved[strings.ToLower(strings.SplitN(cand, ".", 2)[0])] {
			cand = b + "-x" + ext
		}
		cand = ensureSyncable(cand)
		if len(cand) <= max {
			return cand, true
		}
		// The guards pushed it over; give back exactly the overflow and
		// retry. budget strictly decreases, so this terminates.
		budget -= len(cand) - max
	}
	return "", false
}
