package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"save/internal/config"
	"save/internal/paths"
	"save/internal/state"
)

// twoTreeArchive lays out both trees: a note in the archive, a note filed
// under spam, a note whose row says spam but whose file is still in the
// archive (an interrupted move), a note present in both trees, and an
// orphan in the spam tree. Returns the archive root and the spam root.
func twoTreeArchive(t *testing.T, includeProblems bool) (root, spam string) {
	t.Helper()
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeTestConfig(t, filepath.Join(cfgDir, "config.toml"), root, "UTC")
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	spam = cfg.SpamRoot
	if err := paths.EnsureDir(paths.StateDir()); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	src := state.InstanceID(state.SourceGmail, "work")
	put := func(id, rel string, disp state.Disposition, files ...string) {
		t.Helper()
		content := "note " + id + "\n"
		sum := sha256.Sum256([]byte(content))
		for _, r := range files {
			writeFile(t, filepath.Join(r, filepath.FromSlash(rel)), content)
		}
		if err := db.CommitMessage(state.Message{
			Source: src, StableID: id, TS: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
			DayBucket: "2026-08-07", RelPath: rel, ContentHash: hex.EncodeToString(sum[:]),
		}); err != nil {
			t.Fatal(err)
		}
		if disp == state.DispositionSpam {
			if err := db.SetDisposition(src, id, disp, "noise", "rules:noise:x", "0123456789abcdef"); err != nil {
				t.Fatal(err)
			}
		}
	}
	put("kept", "2026/08/07/100000_gmail-work_kept_aaaaaaaa.md", state.DispositionArchive, root)
	put("filed", "2026/08/07/110000_gmail-work_filed_bbbbbbbb.md", state.DispositionSpam, spam)
	if includeProblems {
		put("halfway", "2026/08/07/120000_gmail-work_halfway_cccccccc.md", state.DispositionSpam, root)
		put("doubled", "2026/08/07/130000_gmail-work_doubled_dddddddd.md", state.DispositionArchive, root, spam)
		writeFile(t, filepath.Join(spam, "2026", "08", "07", "140000_gmail-work_stray_eeeeeeee.md"), "x")
	}
	return root, spam
}

func verifyText(t *testing.T) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runVerifyWith(&out, verifyOpts{
		dataless:      func(string) (bool, error) { return false, nil },
		noMaterialize: func() error { return nil },
	})
	return out.String(), err
}

// TestVerifyAuditsBothTrees: a note is checked in the tree its row records
// and looked for in the other, so an interrupted move, a duplicate and an
// orphan in the spam tree are each named — while a clean pair of trees is
// clean.
func TestVerifyAuditsBothTrees(t *testing.T) {
	twoTreeArchive(t, true)
	text, err := verifyText(t)
	if err == nil {
		t.Fatalf("verify reported clean:\n%s", text)
	}
	for _, want := range []string{
		"note recorded in the spam tree is in the archive tree: 2026/08/07/120000_gmail-work_halfway_cccccccc.md",
		"`save triage` reconciles it",
		"note exists in both trees: 2026/08/07/130000_gmail-work_doubled_dddddddd.md",
		"orphan file in the spam tree",
		"140000_gmail-work_stray_eeeeeeee.md",
		"checked 4 emails (2 in the spam tree)",
		"verify found 3 discrepanc",
	} {
		if !strings.Contains(text+err.Error(), want) {
			t.Errorf("verify does not say %q:\n%s\n%v", want, text, err)
		}
	}
	for _, mustNot := range []string{"kept_aaaaaaaa.md", "filed_bbbbbbbb.md", "missing file"} {
		if strings.Contains(text, mustNot) {
			t.Errorf("verify wrongly reports %q:\n%s", mustNot, text)
		}
	}
}

func TestVerifyCleanWithBothTrees(t *testing.T) {
	_, spam := twoTreeArchive(t, false)
	// Writer litter in the spam tree's .tmp is not an orphan either.
	writeFile(t, filepath.Join(spam, ".tmp", ".x_ab12cd34.md42"), "x")
	text, err := verifyText(t)
	if err != nil {
		t.Fatalf("verify: %v\n%s", err, text)
	}
	if !strings.Contains(text, "checked 2 emails (1 in the spam tree)") || !strings.Contains(text, "spam tree agree") {
		t.Errorf("summary:\n%s", text)
	}
	// And a spam tree that does not exist yet is simply empty.
	if err := os.RemoveAll(spam); err != nil {
		t.Fatal(err)
	}
	if text, err := verifyText(t); err == nil || !strings.Contains(text, "missing file: 2026/08/07/110000_gmail-work_filed_bbbbbbbb.md") {
		t.Errorf("a vanished spam tree: %v\n%s", err, text)
	}
}
