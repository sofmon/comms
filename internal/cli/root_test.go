package cli

import "testing"

// TestCommandRegistration pins the full command tree: every documented
// subcommand must be present, including both auth providers.
func TestCommandRegistration(t *testing.T) {
	root := newRoot()

	have := make(map[string]bool)
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"init", "auth", "sync", "run", "status", "doctor", "verify", "version"} {
		if !have[want] {
			t.Errorf("subcommand %q is not registered", want)
		}
	}

	var auth map[string]bool
	for _, c := range root.Commands() {
		if c.Name() == "auth" {
			auth = make(map[string]bool)
			for _, sub := range c.Commands() {
				auth[sub.Name()] = true
			}
		}
	}
	for _, want := range []string{"google", "fastmail"} {
		if !auth[want] {
			t.Errorf("auth subcommand %q is not registered", want)
		}
	}
}

// TestSyncFlags pins the sync command's flag set.
func TestSyncFlags(t *testing.T) {
	root := newRoot()
	for _, c := range root.Commands() {
		if c.Name() != "sync" {
			continue
		}
		for _, flag := range []string{"source", "full", "retry-failed"} {
			if c.Flags().Lookup(flag) == nil {
				t.Errorf("sync is missing the --%s flag", flag)
			}
		}
		return
	}
	t.Fatal("sync command not found")
}
