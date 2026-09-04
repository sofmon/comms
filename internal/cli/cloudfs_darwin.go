//go:build darwin

// macOS FileProvider / iCloud Drive introspection.
//
// Everything in this file exists because an archive_root inside iCloud Drive
// is not a plain directory: with "Optimize Mac Storage" on, the system may
// EVICT a file's contents and leave a zero-block placeholder behind (a
// "dataless" file). stat() still reports the real size and mtime, so nothing
// looks wrong — but the first read blocks for a second or more while the
// provider downloads the bytes back, or fails outright when the reading
// process is not allowed to trigger a download.
//
// Every constant below was read out of the local SDK headers and then
// CONFIRMED at runtime on this machine; see the comments at each one.

package cli

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// datalessFile reports whether path is an evicted cloud placeholder: the
// directory entry and metadata are local, the contents are not.
//
// The probe is a stat, which never materializes anything — verified on this
// machine against a real evicted file (st_flags 0x40000060, still dataless
// after the probe).
func datalessFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	// unix.SF_DATALESS == 0x40000000, matching SF_DATALESS in <sys/stat.h>.
	return st.Flags&unix.SF_DATALESS != 0, nil
}

// trackedPath reports whether path carries UF_TRACKED (0x40), the flag a
// FileProvider sets on the items it owns. It is a heuristic — document-ID
// tracking sets it elsewhere too — so callers use it only to widen a
// path-prefix test, never on its own.
func trackedPath(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return st.Flags&unix.UF_TRACKED != 0, nil
}

// freeBytes reports the space available to this user on the volume holding
// path.
func freeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bsize) * st.Bavail, nil
}

// optimizeStorage reports iCloud Drive's "Optimize Mac Storage" setting.
// known is false when the preference could not be read at all, which is the
// normal answer on a Mac that has never signed into iCloud.
//
// The value lives in the cfprefsd-managed com.apple.bird domain, so it is
// read through `defaults` rather than by parsing the (possibly stale) binary
// plist in ~/Library/Preferences. The command is a fixed argv with no
// caller-supplied input.
func optimizeStorage() (on bool, known bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "com.apple.bird", "optimize-storage").Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(string(out)) {
	case "1", "true", "YES":
		return true, true
	case "0", "false", "NO":
		return false, true
	}
	return false, false
}

// setiopolicy_np(3) constants, for IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES.
//
// Read from <sys/resource.h> in every SDK on this machine (MacOSX12.1,
// MacOSX15, MacOSX15.4, MacOSX26, MacOSX26.5 and the Xcode default all agree):
//
//	#define IOPOL_SCOPE_PROCESS                       0
//	#define IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES 3
//	#define IOPOL_MATERIALIZE_DATALESS_FILES_OFF      1
//
// The iopolicysys(2) command selector is NOT in the public headers — it lives
// in resource_private.h, which Apple does not ship. It was determined
// empirically by round-tripping the policy on this machine: cmd 1 reads it
// back, cmd 2 sets it, every other value in 0..7 returns EINVAL. A full
// set-OFF / get / set-ON / get / bogus-value cycle behaved exactly as
// setiopolicy_np(3) documents, and reading a genuinely evicted iCloud file
// with the policy OFF failed in 104us with EDEADLK and left the file dataless
// — i.e. no download was triggered.
const (
	iopolCmdSet = 2

	iopolScopeProcess                    = 0
	iopolTypeVFSMaterializeDatalessFiles = 3
	iopolMaterializeDatalessFilesOff     = 1
)

// iopolParam mirrors struct _iopol_param_t: three C ints, in this order.
type iopolParam struct {
	scope  int32
	ioType int32
	policy int32
}

// setNoMaterialize pins this process's dataless-file materialization policy
// to OFF, so that a read of an evicted iCloud file fails with EDEADLK instead
// of silently pulling the bytes down over the network. New processes inherit
// the policy, and the system default for a process outside a GUI login
// session is already OFF (setiopolicy_np(3)); this makes it explicit and
// unconditional for `comms verify`.
//
// libSystem's setiopolicy_np() cannot be called without cgo, so this issues
// the underlying iopolicysys(2) trap directly. unix.Syscall is implemented in
// assembly, which is what makes the uintptr(unsafe.Pointer(...)) argument
// legal: the compiler keeps the referenced object alive and unmoved for the
// duration of the call.
func setNoMaterialize() error {
	p := iopolParam{
		scope:  iopolScopeProcess,
		ioType: iopolTypeVFSMaterializeDatalessFiles,
		policy: iopolMaterializeDatalessFilesOff,
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOPOLICYSYS, iopolCmdSet, uintptr(unsafe.Pointer(&p)), 0); errno != 0 {
		return errno
	}
	return nil
}
