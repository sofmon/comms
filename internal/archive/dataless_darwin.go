//go:build darwin

package archive

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// isDataless reports whether p is an iCloud "dataless" placeholder.
//
// With "Optimize Mac Storage" on (com.apple.bird optimize-storage = 1),
// macOS evicts the CONTENTS of an iCloud Drive file while keeping its name,
// size and metadata on disk. The stub carries the SF_DATALESS super-user
// flag (0x40000000) in st_flags. Reading such a file blocks while the
// FileProvider fetches it back (~1-1.6 s here) — or fails outright with
// EDEADLK/ETIMEDOUT when the calling process has its materialization policy
// off, which is the default for anything running outside the user's login
// session (see setiopolicy_np(3), MaterializeDatalessFiles in
// launchd.plist(5)). A `save` run started by launchd is exactly that case.
//
// The archiver never wants to touch such a file: renaming over a dataless
// destination throws away the only local trace of a file whose bytes live in
// iCloud, and the eviction means the archive is being written into a
// directory the user's disk pressure is actively reclaiming.
//
// lstat(2) is served from the placeholder's own metadata, so this check
// never materializes anything, and it does not follow symlinks.
func isDataless(p string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		// A destination that does not exist yet is the normal case for a
		// first write, and cannot be dataless.
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %s: %w", p, err)
	}
	return st.Flags&unix.SF_DATALESS != 0, nil
}
