package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigDir(t *testing.T) {
	tests := []struct {
		name       string
		commsDir   string
		xdgConfig  string
		want       string // exact match when non-empty
		wantSuffix string // suffix match otherwise
	}{
		{name: "comms_config_dir wins", commsDir: "/custom/cfg", xdgConfig: "/xdg", want: "/custom/cfg"},
		{name: "xdg_config_home", xdgConfig: "/xdg", want: filepath.Join("/xdg", "comms")},
		{name: "default unix style", wantSuffix: filepath.Join(".config", "comms")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("COMMS_CONFIG_DIR", tt.commsDir)
			t.Setenv("XDG_CONFIG_HOME", tt.xdgConfig)
			got := ConfigDir()
			if tt.want != "" && got != tt.want {
				t.Fatalf("ConfigDir() = %q, want %q", got, tt.want)
			}
			if tt.wantSuffix != "" && !strings.HasSuffix(got, tt.wantSuffix) {
				t.Fatalf("ConfigDir() = %q, want suffix %q", got, tt.wantSuffix)
			}
		})
	}
}

func TestStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdgstate")
	if got, want := StateDir(), filepath.Join("/xdgstate", "comms"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
	t.Setenv("XDG_STATE_HOME", "")
	if got, wantSuffix := StateDir(), filepath.Join(".local", "state", "comms"); !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("StateDir() = %q, want suffix %q", got, wantSuffix)
	}
}

func TestEnsureDir(t *testing.T) {
	tests := []struct {
		name string
		prep func(t *testing.T, dir string)
	}{
		{name: "fresh nested", prep: func(t *testing.T, dir string) {}},
		{name: "pre-existing loose perms", prep: func(t *testing.T, dir string) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "a", "b")
			tt.prep(t, dir)
			if err := EnsureDir(dir); err != nil {
				t.Fatalf("EnsureDir: %v", err)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Fatalf("mode = %04o, want 0700", perm)
			}
		})
	}
}

func TestCheckCredentialPerms(t *testing.T) {
	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr bool
	}{
		{name: "0600 ok", mode: 0o600},
		{name: "0400 ok", mode: 0o400},
		{name: "group-readable refused", mode: 0o640, wantErr: true},
		{name: "world-readable refused", mode: 0o644, wantErr: true},
		{name: "group-writable refused", mode: 0o620, wantErr: true},
		{name: "world-only refused", mode: 0o604, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cred")
			if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil { // exact mode, immune to umask
				t.Fatal(err)
			}
			err := CheckCredentialPerms(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("mode %04o: want error, got nil", tt.mode)
				}
				if !strings.Contains(err.Error(), "chmod 600") {
					t.Fatalf("error should tell the user the chmod command, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("mode %04o: unexpected error: %v", tt.mode, err)
			}
		})
	}
}

func TestCheckCredentialPermsMissingFile(t *testing.T) {
	if err := CheckCredentialPerms(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

func TestCheckCredentialPermsSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckCredentialPerms(link); err == nil {
		t.Fatal("want error for symlinked credential file, got nil")
	}
}
