//go:build !unix

package cli

import "errors"

// deviceOf cannot identify filesystems here; doctor reports the check as
// inconclusive rather than guessing.
func deviceOf(string) (uint64, error) {
	return 0, errors.New("filesystem identity is not available on this platform")
}
