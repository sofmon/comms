package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"comms/internal/config"
	"comms/internal/state"
)

func statAt(t *testing.T, root, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	switch {
	case err == nil:
		return true
	case os.IsNotExist(err):
		return false
	}
	t.Fatalf("stat %s under %s: %v", rel, root, err)
	return false
}

func openTestDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestTriageAppliesThePlan: the noise note and its attachment folder cross
// into the spam tree, its row says so with the rule and the digest, every
// evaluated note gets a ledger row, the note the tree lacks is left
// unsettled, a second run is a no-op — and a rules change that no longer
// calls the note noise brings it back.
func TestTriageAppliesThePlan(t *testing.T) {
	root, rels := triageArchive(t)
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	spam := cfg.SpamRoot
	src := state.InstanceID(state.SourceGmail, "work")
	// The noise note gets an attachment folder that must travel with it.
	attachDir := strings.TrimSuffix(rels[0], ".md") + ".d"
	writeFile(t, filepath.Join(root, filepath.FromSlash(attachDir), "logo.png"), "png")

	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatalf("runTriage: %v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"WOULD MOVE to spam: 1 note(s)",
		"moved 1 note(s) to spam, 0 back to the archive; 2 left in place and settled",
		"not read: 1 note(s)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output does not say %q:\n%s", want, text)
		}
	}
	if !statAt(t, spam, rels[0]) || statAt(t, root, rels[0]) {
		t.Errorf("the noise note did not move: spam=%v archive=%v", statAt(t, spam, rels[0]), statAt(t, root, rels[0]))
	}
	if !statAt(t, spam, attachDir+"/logo.png") || statAt(t, root, attachDir) {
		t.Error("the attachment folder did not follow the note")
	}
	for _, rel := range rels[1:3] {
		if !statAt(t, root, rel) {
			t.Errorf("a kept note left the archive: %s", rel)
		}
	}

	db := openTestDB(t)
	digest := ""
	for _, tc := range []struct {
		id      string
		disp    state.Disposition
		rule    string
		settled bool
		verdict state.TriageVerdict
	}{
		{"gh", state.DispositionSpam, "rules:noise:github-notifications", true, state.TriageNoise},
		{"inv", state.DispositionArchive, "protect:attachment", true, state.TriageSignal},
		{"plain", state.DispositionArchive, "", true, state.TriageUndecided},
		{"gone", state.DispositionArchive, "", false, ""},
	} {
		m, _, err := db.GetMessage(src, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Disposition != tc.disp || m.DispositionRule != tc.rule || (m.TriageDigest != "") != tc.settled {
			t.Errorf("%s row = disposition %s rule %q digest %q; want %s %q settled=%v", tc.id, m.Disposition, m.DispositionRule, m.TriageDigest, tc.disp, tc.rule, tc.settled)
		}
		if tc.settled {
			if digest == "" {
				digest = m.TriageDigest
			} else if m.TriageDigest != digest {
				t.Errorf("%s settled under %s, others under %s", tc.id, m.TriageDigest, digest)
			}
			if m.DispositionAt.IsZero() || m.DispositionReason == "" {
				t.Errorf("%s row has no reason/time: %+v", tc.id, m)
			}
		}
		dec, ok, _ := db.GetTriageDecision(src, tc.id)
		if tc.verdict == "" {
			if ok {
				t.Errorf("%s has a ledger row though it was never read", tc.id)
			}
		} else if !ok || dec.Verdict != tc.verdict || dec.Digest != digest {
			t.Errorf("%s ledger = %+v, %v; want %s under %s", tc.id, dec, ok, tc.verdict, digest)
		}
	}
	db.Close()

	// Idempotent: the settled notes are out of scope; only the missing one
	// is looked at again, and nothing moves.
	out.Reset()
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 note(s) in scope") || !strings.Contains(out.String(), "moved 0 note(s) to spam, 0 back") {
		t.Errorf("second run was not a no-op:\n%s", out.String())
	}

	// The rules change: without the GitHub rule the note is undecided, and
	// undecided is never noise — it comes back, attachment folder and all.
	writeFile(t, cfg.Triage.RulesFilePath, "[[keep]]\nname = \"billing\"\nsubject = [\"*receipt*\"]\n")
	out.Reset()
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatalf("run after a rules change: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "would move back to the archive: 1") || !strings.Contains(out.String(), "0 note(s) to spam, 1 back") {
		t.Errorf("rules change did not bring the note back:\n%s", out.String())
	}
	if !statAt(t, root, rels[0]) || statAt(t, spam, rels[0]) || !statAt(t, root, attachDir+"/logo.png") {
		t.Error("the note (or its folder) is not back in the archive")
	}
	db = openTestDB(t)
	m, _, _ := db.GetMessage(src, "gh")
	if m.Disposition != state.DispositionArchive || m.DispositionRule != "" || m.TriageDigest == "" || m.TriageDigest == digest {
		t.Errorf("row after the rules change: %+v", m)
	}
}

// TestTriageReconcilesAnInterruptedMove: a note found in the other tree
// than its row records is treated as a move that died before the row was
// written — the dry run says so, and the real run corrects the row and
// brings the attachment folder across before applying the verdict.
func TestTriageReconcilesAnInterruptedMove(t *testing.T) {
	root, rels := triageArchive(t)
	cfg, _ := config.Load(config.DefaultPath())
	spam := cfg.SpamRoot
	src := state.InstanceID(state.SourceGmail, "work")
	attachDir := strings.TrimSuffix(rels[0], ".md") + ".d"
	writeFile(t, filepath.Join(root, filepath.FromSlash(attachDir), "logo.png"), "png")
	// The crash: the .md crossed, the .d/ and the row did not.
	if err := os.MkdirAll(filepath.Join(spam, "2026", "08", "07"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, filepath.FromSlash(rels[0])), filepath.Join(spam, filepath.FromSlash(rels[0]))); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 note(s) found in the other tree") || !strings.Contains(out.String(), "would reconcile: "+rels[0]+" is in the spam tree but recorded in the archive tree") {
		t.Errorf("dry run does not report the interrupted move:\n%s", out.String())
	}
	if statAt(t, spam, attachDir) {
		t.Error("the dry run moved the attachment folder")
	}

	out.Reset()
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatalf("runTriage: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "reconciled: 1 note(s)") {
		t.Errorf("run does not report the reconciliation:\n%s", out.String())
	}
	if !statAt(t, spam, attachDir+"/logo.png") || statAt(t, root, attachDir) {
		t.Error("the attachment folder did not follow the note")
	}
	db := openTestDB(t)
	m, _, _ := db.GetMessage(src, "gh")
	// Reconciled to spam, then the verdict (noise) agreed and settled it.
	if m.Disposition != state.DispositionSpam || m.DispositionRule != "rules:noise:github-notifications" || m.TriageDigest == "" {
		t.Errorf("row after reconciliation: %+v", m)
	}
}

// TestUntriage: by path in the spam tree the note and its folder come back
// and the row says manual; a later pass leaves it alone; by id it works
// too; the ambiguous and unknown forms are refused.
func TestUntriage(t *testing.T) {
	root, rels := triageArchive(t)
	cfg, _ := config.Load(config.DefaultPath())
	spam := cfg.SpamRoot
	src := state.InstanceID(state.SourceGmail, "work")
	attachDir := strings.TrimSuffix(rels[0], ".md") + ".d"
	writeFile(t, filepath.Join(root, filepath.FromSlash(attachDir), "logo.png"), "png")
	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatal(err)
	}
	if !statAt(t, spam, rels[0]) {
		t.Fatal("setup: the noise note did not move")
	}

	out.Reset()
	if err := runUntriage(&out, filepath.Join(spam, filepath.FromSlash(rels[0]))); err != nil {
		t.Fatalf("untriage by path: %v", err)
	}
	if !strings.Contains(out.String(), "moved "+rels[0]+" back to the archive (attachment folder: true)") {
		t.Errorf("untriage output:\n%s", out.String())
	}
	if !statAt(t, root, rels[0]) || statAt(t, spam, rels[0]) || !statAt(t, root, attachDir+"/logo.png") {
		t.Error("the note (or its folder) is not back")
	}
	db := openTestDB(t)
	m, _, _ := db.GetMessage(src, "gh")
	if m.Disposition != state.DispositionArchive || m.DispositionRule != manualRule || !strings.Contains(m.DispositionReason, "untriage") {
		t.Errorf("row after untriage: %+v", m)
	}
	dec, ok, _ := db.GetTriageDecision(src, "gh")
	if !ok || dec.Verdict != state.TriageSignal || dec.Layer != state.TriageLayerManual || dec.Rule != "untriage" {
		t.Errorf("ledger after untriage: %+v", dec)
	}
	db.Close()

	// The rules still call it noise; a pass leaves it alone and says so.
	out.Reset()
	if err := runTriage(&out, triageOpts{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 note(s) placed by `comms untriage` are left alone") || !statAt(t, root, rels[0]) {
		t.Errorf("a later pass re-filed a note the operator moved back:\n%s", out.String())
	}
	// Until the operator asks for exactly that.
	out.Reset()
	if err := runTriage(&out, triageOpts{reclassify: true, onlyLayer: "manual"}); err != nil {
		t.Fatal(err)
	}
	if !statAt(t, spam, rels[0]) {
		t.Errorf("--reclassify --only manual did not re-evaluate the note:\n%s", out.String())
	}

	// By "<instance>/<id>", and by bare id; a note already in the archive
	// is recorded without moving.
	out.Reset()
	if err := runUntriage(&out, src+"/gh"); err != nil {
		t.Fatalf("untriage by instance/id: %v", err)
	}
	if !statAt(t, root, rels[0]) {
		t.Error("untriage by id did not move the note back")
	}
	out.Reset()
	if err := runUntriage(&out, "plain"); err != nil || !strings.Contains(out.String(), "was already in the archive") {
		t.Errorf("untriage of a note already in the archive: %v\n%s", err, out.String())
	}
	for _, bad := range []string{"nope", "gmail:other/gh", filepath.Join(t.TempDir(), "x.md")} {
		if err := runUntriage(&out, bad); err == nil {
			t.Errorf("untriage %q succeeded", bad)
		}
	}
}
