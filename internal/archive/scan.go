package archive

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// ChatStemKey identifies one archived conversation: the file tag of the
// account that wrote the day file plus the space hash. Two accounts that
// share a space write separate files, so the tag is part of the key.
type ChatStemKey struct {
	Tag   string // "gchat-<label>", e.g. "gchat-work"
	Hash8 string // naming.Hash8(space resource name)
}

// ChatStemInfo is the space identity encoded in an on-disk chat day-file
// stem "gchat-<label>_<spacetype>_<slug>_<hash8>.md".
type ChatStemInfo struct {
	SpaceType string // "space" | "group" | "dm"
	Slug      string // the space's frozen display slug
}

// chatStemName matches chat day-file basenames, capturing the account label
// (the part of the file tag after "gchat-"), space type, slug and hash8.
// Labels are [a-z0-9-] only, so the label capture stops at the first '_'.
// The slug match is greedy, so a slug containing underscores keeps them and
// the trailing hash8 is always the real one.
var chatStemName = regexp.MustCompile(`^gchat-([a-z0-9-]+)_(space|group|dm)_(.+)_([0-9a-f]{8})\.md$`)

// dayDirRel matches an archive-root-relative day directory ("2026/08/07").
var dayDirRel = regexp.MustCompile(`^\d{4}/\d{2}/\d{2}$`)

// ScanChatStems walks root's YYYY/MM/DD day directories and returns, keyed
// by (file tag, hash8), the space identity each chat day-file stem encodes.
// Keying by tag as well as hash keeps two accounts' copies of the same
// shared space apart, so one account's frozen slug can never be reclaimed
// into another's state. The first file seen for a key wins (WalkDir is
// lexical, so the earliest day). The Root/.tmp scratch directory is skipped,
// and a missing root yields an empty map.
func ScanChatStems(root string) (map[ChatStemKey]ChatStemInfo, error) {
	out := make(map[ChatStemKey]ChatStemInfo)
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && os.IsNotExist(err) {
				return fs.SkipAll // nothing archived yet
			}
			return err
		}
		if d.IsDir() {
			if p != root && d.Name() == TempDirName {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if !dayDirRel.MatchString(filepath.ToSlash(filepath.Dir(rel))) {
			return nil
		}
		m := chatStemName.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		key := ChatStemKey{Tag: "gchat-" + m[1], Hash8: m[4]}
		if _, seen := out[key]; !seen {
			out[key] = ChatStemInfo{SpaceType: m[2], Slug: m[3]}
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("archive: scan chat stems: %w", walkErr)
	}
	return out, nil
}
