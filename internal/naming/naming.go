// Package naming derives every archive file and directory name.
//
// All rules here are load-bearing for idempotency: paths must be
// deterministic across runs, derived only from immutable message fields,
// and collision-free on a case- and normalization-insensitive filesystem
// (APFS). Nothing in this package may consult mutable data.
package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	slugMaxBytes     = 60
	filenameMaxBytes = 100
	extMaxBytes      = 20
)

// windows-reserved device basenames (checked case-insensitively on the
// segment before the first dot).
var reserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// Hash8 returns the first 8 lowercase hex chars of SHA-256(s). Callers pass
// an immutable identity string: for email the INSTANCE id plus the message
// id ("gmail:work:<message id>"), for chat the space resource name (two
// accounts sharing a space are already separated by their differing file
// tags, so the space hash stays account-independent).
func Hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// Slug converts free text (subject, space display name) into a lowercase
// [a-z0-9-] slug of at most 60 bytes. fallback is returned when nothing
// survives (e.g. "no-subject", "untitled").
//
// A slug can never contain a dot, so the iCloud extension and ".nosync"
// rules cannot reach it, but a whole-name rule can: the subjects "Dropbox",
// "OneDrive", "tmp" and "IDrive Sync" all slug straight onto the exclusion
// list. Those get the same "-x" marker as a reserved device name, so a slug
// used on its own is safe as well as one embedded in a stem.
func Slug(s, fallback string) string {
	s = strings.ToLower(norm.NFC.String(s))
	var b strings.Builder
	prevDash := true // suppress leading dash
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case unicode.IsSpace(r) || r == '/' || r == '-' || r == '_' || r == '.':
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(truncateBytes(strings.Trim(b.String(), "-"), slugMaxBytes), "-")
	if out == "" {
		out = fallback
	}
	if reserved[out] || SyncExcluded(out) {
		// Truncate first so the marker cannot push the slug past its cap.
		// Every reserved and excluded name is short, so in practice this
		// only appends. "con-x" / "dropbox-x" are themselves neither
		// reserved nor excluded, so the rewrite is idempotent.
		out = strings.Trim(truncateBytes(out, slugMaxBytes-2), "-") + "-x"
	}
	return out
}

// SanitizeFilename makes an attachment name safe for the archive while
// keeping the original as recognizable as possible: NFC-normalized, control
// and forbidden characters replaced, trailing dots/spaces trimmed, capped at
// 100 bytes on a rune boundary preserving the extension. fallback (already
// safe, e.g. "attachment-3.pdf") is used when nothing survives.
//
// Attachment names are the one place a sender's own text becomes a filename,
// so they are also the one place an iCloud sync-exclusion rule can be hit.
// The result is guaranteed never to be SyncExcluded: after the charset and
// length work, ensureSyncable applies its documented rewrite ("draft.tmp" ->
// "draft.tmp.bin", "Dropbox" -> "_Dropbox", "a.NoSync.pdf" -> "a_NoSync.pdf")
// and the name is re-shortened if that pushed it past 100 bytes. The rewrite
// is idempotent, so sanitizing an already-sanitized name is a no-op.
func SanitizeFilename(name, fallback string) string {
	name = norm.NFC.String(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return ensureSyncable(fallback)
	}
	ext := path.Ext(name)
	if len(ext) > extMaxBytes {
		ext = ""
	}
	if out, ok := capName(strings.TrimSuffix(name, ext), ext, filenameMaxBytes); ok {
		return out
	}
	return ensureSyncable(fallback)
}

// CollisionKey folds a filename the way APFS compares names: NFC-normalized
// and case-folded. Two names with equal keys would collide on disk.
func CollisionKey(name string) string {
	return cases.Fold().String(norm.NFC.String(name))
}

// Unique returns name, or name with a deterministic _2/_3… suffix before the
// extension, such that its CollisionKey is absent from taken; the chosen
// key is recorded in taken. Callers must feed names in a deterministic
// order (MIME part index, message order) so re-runs pick identical suffixes.
//
// Every candidate passes through the iCloud guard before its key is
// recorded, so the suffixing can never hand back an excluded name even if a
// caller feeds Unique something SanitizeFilename never produced.
func Unique(taken map[string]bool, name string) string {
	name = ensureSyncable(name)
	if key := CollisionKey(name); !taken[key] {
		taken[key] = true
		return name
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		cand := ensureSyncable(fmt.Sprintf("%s_%d%s", base, i, ext))
		if key := CollisionKey(cand); !taken[key] {
			taken[key] = true
			return cand
		}
	}
}

// DayDir renders the archive day directory ("2026/08/07") for t. The caller
// is responsible for t already being in the pinned archive timezone.
func DayDir(t time.Time) string {
	return t.Format("2006/01/02")
}

// DayBucket renders the DB day key ("2026-08-07") for t (pinned zone).
func DayBucket(t time.Time) string {
	return t.Format("2006-01-02")
}

// EmailStem returns "HHMMSS_<tag>_<subject-slug>_<hash8>" — the email .md
// basename without extension. t must be in the pinned archive timezone.
//
// sourceTag is the account's FILE TAG ("gmail-work"), never the instance id
// ("gmail:work"): a colon is not a legal filename character. Tags contain
// only [a-z0-9-], so the '_' separators stay unambiguous.
//
// The ensureSyncable call is defence in depth: a stem always begins with six
// digits and carries no dot, so no iCloud exclusion rule can reach it, and
// the call is a no-op. It exists so a future change to the stem shape cannot
// reintroduce a silently unsynced file.
func EmailStem(t time.Time, sourceTag, subject, hash8 string) string {
	return ensureSyncable(fmt.Sprintf("%s_%s_%s_%s", t.Format("150405"), sourceTag, Slug(subject, "no-subject"), hash8))
}

// ChatStem returns "<tag>_<spacetype>_<space-slug>_<hash8>" — the
// conversation-day .md basename without extension. slug must be the space's
// frozen slug (already produced by Slug at first sight).
//
// sourceTag is the account's FILE TAG ("gchat-work"), never the instance id:
// the tag is what makes two accounts' copies of the same shared space
// distinct files.
//
// slug comes from the state DB (frozen at first sight), so unlike EmailStem
// this function cannot re-derive it. The ensureSyncable call therefore also
// covers a slug frozen by an older build: the stem as a whole is what iCloud
// matches, and a tag_type_slug_hash stem can never match a rule.
func ChatStem(sourceTag, spaceType, slug, hash8 string) string {
	return ensureSyncable(fmt.Sprintf("%s_%s_%s_%s", sourceTag, spaceType, slug, hash8))
}

// AttachDir returns the sibling attachment directory name for a stem.
//
// A directory is excluded by name exactly like a file, and an excluded
// directory is not even enumerated by the file provider — it would take every
// attachment inside it down with it — so the ".d" name is guarded too. A
// leading dot does NOT exclude anything (verified), so "<stem>.d" syncs.
func AttachDir(stem string) string {
	return ensureSyncable(stem + ".d")
}

// ChatAttachmentName returns "<HHMMSS>_<msghash8>_<name>" — chat attachment
// names carry the owning message's time and hash so two same-named uploads
// in one day (even in the same second) never collide.
//
// The "HHMMSS_hash8_" prefix defeats the whole-name and prefix exclusion
// rules on its own, but not the extension or ".nosync" ones — those live in
// the tail — so the composed name is guarded as well. For a sanitized tail
// the guard is a no-op, and the six-digit prefix that chatrender strips back
// off is never disturbed.
func ChatAttachmentName(t time.Time, msgHash8, sanitized string) string {
	return ensureSyncable(fmt.Sprintf("%s_%s_%s", t.Format("150405"), msgHash8, sanitized))
}

// truncateBytes cuts s to at most max bytes on a rune boundary.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
