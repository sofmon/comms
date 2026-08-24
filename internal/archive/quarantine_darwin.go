//go:build darwin

package archive

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// tagOrigin stamps the macOS provenance extended attributes onto path:
// com.apple.quarantine always, and com.apple.metadata:kMDItemWhereFroms when
// the Origin has anything to say.
//
// It is called on the renameio PENDING TEMP FILE, before
// CloseAtomicallyReplace. That ordering is the whole point: the xattr is
// carried across a same-directory rename (verified on macOS 26), so the tag
// is never absent on a path anything can see — not after a crash, and not in
// the window Spotlight and the iCloud FileProvider both react to the rename
// in. Tagging after the rename would leave exactly that gap, which is the
// shape of CVE-2022-3155.
//
// See QuarantineMode for what the tag does and does not buy. It is defense in
// depth, never a scan.
func tagOrigin(path string, o Origin) error {
	value := quarantineValue(time.Now(), newQuarantineUUID())
	if err := setXattr(path, xattrQuarantine, []byte(value)); err != nil {
		return err
	}
	if wf := o.WhereFroms(); len(wf) > 0 {
		if b := bplistStrings(wf); b != nil {
			if err := setXattr(path, xattrWhereFroms, b); err != nil {
				return err
			}
		}
	}
	return nil
}

// setXattr writes one extended attribute, tolerating exactly one failure: a
// volume that cannot hold extended attributes at all.
//
// That is a property of where the archive lives rather than of this file, and
// the attachment is still worth having — an untagged attachment beats an
// unarchived one. Every other errno is reported, and because this runs on the
// temp file the caller aborts before anything becomes visible, so a file
// never lands in the vault silently missing the tag it was supposed to carry.
func setXattr(path, name string, value []byte) error {
	err := unix.Setxattr(path, name, value, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
		return nil
	default:
		return fmt.Errorf("set %s on %s: %w", name, path, err)
	}
}
