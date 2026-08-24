//go:build !darwin

package source

import "errors"

// ErrFreeSpaceUnsupported is returned by FreeSpace off macOS. Callers must
// treat it as "unknown", not as a failure: policy.Input.FreeSpace <= 0
// disables the free-space floor for that decision, which is the correct
// degradation — save is a macOS archiver and the floor exists because of
// iCloud Drive.
var ErrFreeSpaceUnsupported = errors.New("source: free-space probe is only implemented on macOS")

// FreeSpace reports the bytes available on the volume holding path. Off macOS
// there is no supported probe, so it answers "unknown" (0) with
// ErrFreeSpaceUnsupported; see the darwin implementation for the real one.
func FreeSpace(string) (int64, error) { return 0, ErrFreeSpaceUnsupported }
