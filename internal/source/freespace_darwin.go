//go:build darwin

package source

import "golang.org/x/sys/unix"

// FreeSpace reports the bytes available to this user on the volume holding
// path.
//
// It is the input to the attachment policy's free-space floor
// (policy.Input.FreeSpace): below attachments.free_space_floor an attachment
// is refused with policy.ReasonFreeSpaceFloor while the (tiny) .md note is
// still archived, so a filling disk degrades the archive gracefully instead of
// stopping the sync.
//
// Bavail — not Bfree — is deliberate: the reserved blocks a superuser could
// still write into are not space this process can use.
//
// Caveat worth remembering when reading a free-space number off this: with
// iCloud Drive's "Optimize Mac Storage" on, the system may evict archived
// files and give their space back, so free space is not a reliable proxy for
// how much of the archive is actually local.
func FreeSpace(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bsize) * int64(st.Bavail), nil
}
