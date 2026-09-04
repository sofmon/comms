package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comms/internal/config"
	"comms/internal/state"
)

// TestStatusPerInstanceSections: every configured instance gets its own
// section identified by instance id and account address, counts are read
// per instance rather than per kind, and state left behind by an instance
// the config no longer declares is called out.
func TestStatusPerInstanceSections(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeFile(t, cfgPath, twoAccountConfig(root, filepath.Join(cfgDir, "google-client-personal.json")))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// One archived mail on each Gmail account, with different counts so a
	// per-kind aggregation would be visible.
	for i, src := range []string{"gmail:work", "gmail:personal"} {
		for n := 0; n <= i; n++ {
			m := state.Message{
				Source:      src,
				StableID:    string(rune('a'+i)) + string(rune('0'+n)),
				TS:          time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
				DayBucket:   "2026-08-07",
				RelPath:     "2026/08/07/100000_" + state.Tag(src) + "_x_00000000.md",
				ContentHash: "h",
			}
			if err := db.CommitMessage(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Chat rows only for personal.
	chatFixture(t, db, "gchat:personal", "spaces/AAA", "hello")
	// A cursor left behind by an instance that is no longer configured (the
	// fingerprint of a renamed label).
	if err := db.SetCursor("gmail:oldname", "", "history_id", "42"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}

	var ids []string
	byID := make(map[string]sourceStatus)
	for _, ss := range rep.Sources {
		ids = append(ids, ss.Name)
		byID[ss.Name] = ss
	}
	want := "gmail:work,gmail:personal,gchat:work,gchat:personal,fastmail:fm"
	if strings.Join(ids, ",") != want {
		t.Fatalf("status sections = %v, want %v", ids, want)
	}
	if got := byID["gmail:work"]; got.Account != "you@example.com" || got.Label != "work" || got.Kind != state.SourceGmail {
		t.Errorf("gmail:work section = %+v", got)
	}
	if got := byID["gmail:work"].Messages; got == nil || got.Total != 1 {
		t.Errorf("gmail:work messages = %+v, want 1", got)
	}
	if got := byID["gmail:personal"].Messages; got == nil || got.Total != 2 {
		t.Errorf("gmail:personal messages = %+v, want 2", got)
	}
	if got := byID["gchat:personal"].Chat; got == nil || got.Messages != 1 {
		t.Errorf("gchat:personal chat counts = %+v, want 1 message", got)
	}
	if got := byID["gchat:work"].Chat; got == nil || got.Messages != 0 {
		t.Errorf("gchat:work chat counts = %+v, want 0 messages (personal's must not leak)", got)
	}
	if strings.Join(rep.UnconfiguredSources, ",") != "gmail:oldname" {
		t.Errorf("unconfigured sources = %v, want [gmail:oldname]", rep.UnconfiguredSources)
	}

	var out strings.Builder
	printStatus(&out, rep)
	text := out.String()
	for _, want := range []string{
		"gmail:work — you@example.com",
		"gchat:personal — you@example.net",
		"fastmail:fm — you@fastmail.com",
		"gmail:oldname",
		"no longer declares",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status output does not contain %q:\n%s", want, text)
		}
	}
}
