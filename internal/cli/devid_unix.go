//go:build unix

package cli

import (
	"fmt"
	"os"
	"syscall"
)

// deviceOf returns the id of the filesystem holding path. Two paths with
// equal ids can be renamed across each other atomically.
func deviceOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat %s: no device information", path)
	}
	return uint64(st.Dev), nil
}
