// Package paths resolves save's config and state directories and enforces
// the unix-style permission rules for the files inside them.
//
// The plan pins unix-style locations on every platform — ~/.config/save and
// ~/.local/state/save — so we deliberately do not use xdg.ConfigHome /
// xdg.StateHome (on macOS those point at ~/Library/Application Support).
// github.com/adrg/xdg supplies home-directory resolution; the XDG_* and
// SAVE_CONFIG_DIR environment overrides are read at call time so tests and
// wrappers can redirect them.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"
)

const appDir = "save"

// ConfigDir returns the directory holding config.toml and credential files:
// $SAVE_CONFIG_DIR if set, else $XDG_CONFIG_HOME/save, else ~/.config/save.
func ConfigDir() string {
	if d := os.Getenv("SAVE_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, appDir)
	}
	return filepath.Join(xdg.Home, ".config", appDir)
}

// StateDir returns the directory holding state.db and the instance lock:
// $XDG_STATE_HOME/save if set, else ~/.local/state/save.
func StateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, appDir)
	}
	return filepath.Join(xdg.Home, ".local", "state", appDir)
}

// UnderDir reports whether path is dir itself or anything beneath it. Both
// are cleaned first, so "/a/b" contains "/a/b/../b/c" and does not contain
// "/a/bc". It is pure string work: neither path has to exist.
func UnderDir(dir, path string) bool {
	dir, path = filepath.Clean(dir), filepath.Clean(path)
	return dir == path || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// EnsureDir creates path (and parents) and forces its permissions to 0700.
// The explicit Chmod matters: MkdirAll is a no-op on an existing directory
// and never tightens the mode of one created earlier with looser permissions.
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("chmod 700 %s: %w", path, err)
	}
	return nil
}

// CheckCredentialPerms refuses credential files readable or writable by
// group or world, mirroring ssh's private-key check. It also refuses
// non-regular files (a symlink pointing elsewhere defeats the check).
func CheckCredentialPerms(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("credential file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("credential file %s is not a regular file (mode %v)", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("permissions %04o for %q are too open: credential files must be accessible only by you — run: chmod 600 %q", perm, path, path)
	}
	return nil
}
