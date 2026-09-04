package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpamRootDefaultsBesideArchiveRoot: with nothing configured the spam
// tree is a sibling of the archive, never a child of it.
func TestSpamRootDefaultsBesideArchiveRoot(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	base := t.TempDir()
	cfg, err := Load(writeConfig(t, `archive_root = "`+filepath.Join(base, "Communication")+`"`+minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(base, "spam"); cfg.SpamRoot != want {
		t.Errorf("SpamRoot = %q, want %q", cfg.SpamRoot, want)
	}

	// The same rule applied to a tilde archive root.
	home, _ := os.UserHomeDir()
	cfg, err = Load(writeConfig(t, `archive_root = "~/Archive"`+minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "spam"); cfg.SpamRoot != want {
		t.Errorf("SpamRoot for ~/Archive = %q, want %q", cfg.SpamRoot, want)
	}
	if got := DefaultSpamRoot("/x/y/Communication/"); got != "/x/y/spam" {
		t.Errorf("DefaultSpamRoot with a trailing slash = %q", got)
	}
}

// TestSpamRootExplicitAndEnv: an explicit value is tilde-expanded, and the
// environment override wins over the file, like COMMS_ARCHIVE_ROOT does.
func TestSpamRootExplicitAndEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	home, _ := os.UserHomeDir()

	cfg, err := Load(writeConfig(t, `archive_root = "~/Archive"
spam_root = "~/Noise"`+minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(home, "Noise"); cfg.SpamRoot != want {
		t.Errorf("SpamRoot = %q, want %q", cfg.SpamRoot, want)
	}

	override := t.TempDir()
	t.Setenv("COMMS_SPAM_ROOT", override)
	cfg, err = Load(writeConfig(t, `archive_root = "~/Archive"
spam_root = "~/Noise"`+minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SpamRoot != override {
		t.Errorf("SpamRoot with COMMS_SPAM_ROOT = %q, want %q", cfg.SpamRoot, override)
	}
}

// TestSpamRootMayNotNestWithArchiveRoot: both trees mirror each other and are
// audited together, so one inside the other is refused in both directions —
// and so is the same directory for both.
func TestSpamRootMayNotNestWithArchiveRoot(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	base := t.TempDir()
	for name, tc := range map[string]struct{ archive, spam, want string }{
		"spam inside archive": {filepath.Join(base, "a"), filepath.Join(base, "a", "spam"), "inside archive_root"},
		"archive inside spam": {filepath.Join(base, "s", "a"), filepath.Join(base, "s"), "archive_root"},
		"same directory":      {filepath.Join(base, "a"), filepath.Join(base, "a"), "inside archive_root"},
		"unclean nesting":     {filepath.Join(base, "a"), filepath.Join(base, "a", "..", "a", "x"), "inside archive_root"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `archive_root = "`+tc.archive+`"
spam_root = "`+tc.spam+`"`+minimalValid))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	// A sibling with a shared prefix is fine: "/a/b" is not inside "/a/bc".
	if _, err := Load(writeConfig(t, `archive_root = "`+filepath.Join(base, "arch")+`"
spam_root = "`+filepath.Join(base, "archive-spam")+`"`+minimalValid)); err != nil {
		t.Errorf("a sibling sharing a name prefix was refused: %v", err)
	}
}
