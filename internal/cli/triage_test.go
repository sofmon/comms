package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comms/internal/config"
	"comms/internal/paths"
	"comms/internal/state"
	"comms/internal/triage"
)

// triageNote renders a v2 email note's bytes for the given identity.
func triageNote(from, subject string, extra string) string {
	return `---
source: gmail:work
type: email
render_version: 2
account: you@example.com
account_label: work
message_id: <x@example.com>
thread_id: t
date: "2026-08-07T14:32:05+02:00"
date_utc: "2026-08-07T12:32:05Z"
from:
    - ` + from + `
to:
    - you@example.com
cc: []
subject: ` + subject + `
labels:
    - INBOX
` + extra + `---

Body.
`
}

// triageArchive builds a config, a state database and an archive holding
// four notes: one a rule files as noise, one protected by a stored PDF, one
// nobody decides, and one the database names but the tree lacks. It writes
// the starter triage.toml. Returns the archive root and the rel paths in
// that order.
func triageArchive(t *testing.T) (root string, rels []string) {
	t.Helper()
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeTestConfig(t, filepath.Join(cfgDir, "config.toml"), root, "UTC")
	if err := triage.WriteDefaultRules(filepath.Join(cfgDir, "triage.toml")); err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDir(paths.StateDir()); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	src := state.InstanceID(state.SourceGmail, "work")
	notes := []struct {
		id, rel, content string
	}{
		{"gh", "2026/08/07/100000_gmail-work_bump_aaaaaaaa.md",
			triageNote("GitHub <notifications@github.com>", "'[org/repo] Bump deps'", "attachments: []\n")},
		{"inv", "2026/08/07/110000_gmail-work_report_bbbbbbbb.md",
			triageNote("Vendor <bills@vendor.example>", "Monthly report", "attachments:\n    - 110000_gmail-work_report_bbbbbbbb.d/report.pdf\n")},
		{"plain", "2026/08/07/120000_gmail-work_hello_cccccccc.md",
			triageNote("Friend <friend@example.org>", "hello there", "attachments: []\n")},
		{"gone", "2026/08/07/130000_gmail-work_lost_dddddddd.md", ""},
	}
	for i, n := range notes {
		if n.content != "" {
			writeFile(t, filepath.Join(root, filepath.FromSlash(n.rel)), n.content)
		}
		if err := db.CommitMessage(state.Message{
			Source: src, StableID: n.id, TS: time.Date(2026, 8, 7, 10+i, 0, 0, 0, time.UTC),
			DayBucket: "2026-08-07", RelPath: n.rel, ContentHash: "h",
		}); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, n.rel)
	}
	return root, rels
}

// TestTriageDryRunPrintsThePlanAndMovesNothing: the plan names every move
// with its rule and reason, counts what stays and why, reports the note the
// tree lacks — and leaves both the tree and the database exactly as found.
func TestTriageDryRunPrintsThePlanAndMovesNothing(t *testing.T) {
	root, rels := triageArchive(t)
	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{dryRun: true}); err != nil {
		t.Fatalf("runTriage --dry-run: %v\n%s", err, out.String())
	}
	text := out.String()
	for _, want := range []string{
		"triage digest: ",
		"keep, 5 noise; header heuristics on; model off",
		"4 note(s) in scope",
		"WOULD MOVE to spam: 1 note(s)",
		"gmail:work         1 note(s)",
		"would stay: 1 protected, 0 kept by a rule, 0 kept by the model, 1 undecided",
		"not read: 0 evicted to iCloud (reading would download them), 1 missing, 0 unreadable",
		"moves (file, rule, reason):",
		rels[0] + "  → spam  rules:noise:github-notifications",
		"noise rule github-notifications: from \"notifications@github.com\" matched \"notifications@github.com\"",
		"missing: " + rels[3],
		"dry run: nothing was moved",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, rels[1]+"  →") || strings.Contains(text, rels[2]+"  →") {
		t.Errorf("a note that stays is listed as a move:\n%s", text)
	}
	// Nothing moved, nothing recorded.
	for _, rel := range rels[:3] {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("dry run moved %s: %v", rel, err)
		}
	}
	cfg, _ := config.Load(config.DefaultPath())
	if entries, _ := os.ReadDir(cfg.SpamRoot); len(entries) != 0 {
		t.Errorf("dry run wrote into the spam tree: %v", entries)
	}
	db, err := state.Open(stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, _, _ := db.GetMessage(state.InstanceID(state.SourceGmail, "work"), "gh")
	if m.Disposition != state.DispositionArchive || m.TriageDigest != "" {
		t.Errorf("dry run recorded a decision: %+v", m)
	}
	if _, ok, _ := db.GetTriageDecision(state.InstanceID(state.SourceGmail, "work"), "gh"); ok {
		t.Error("dry run wrote to the ledger")
	}
}

// TestTriageDryRunScopeAndSelectors: --day/--since narrow by day bucket,
// --source by instance, and chat-only selections are refused.
func TestTriageDryRunScopeAndSelectors(t *testing.T) {
	triageArchive(t)
	run := func(o triageOpts) (string, error) {
		o.dryRun = true
		var out bytes.Buffer
		err := runTriage(&out, o)
		return out.String(), err
	}
	if text, err := run(triageOpts{day: "2026-08-06"}); err != nil || !strings.Contains(text, "0 note(s) in scope") {
		t.Errorf("--day on an empty day: %v\n%s", err, text)
	}
	if text, err := run(triageOpts{since: "2026-08-07", only: []string{"work"}}); err != nil || !strings.Contains(text, "4 note(s) in scope") {
		t.Errorf("--since --source: %v\n%s", err, text)
	}
	if _, err := run(triageOpts{only: []string{"gchat"}}); err == nil || !strings.Contains(err.Error(), "never triaged") {
		t.Errorf("a chat-only selection: err = %v", err)
	}
	for name, o := range map[string]triageOpts{
		"bad day":                 {day: "yesterday"},
		"day and since":           {day: "2026-08-07", since: "2026-08-01"},
		"only without reclassify": {onlyLayer: "llm"},
		"unknown only":            {reclassify: true, onlyLayer: "vibes"},
		"explain with a selector": {explain: "x.md", day: "2026-08-07"},
	} {
		if _, err := run(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestTriageExplain: one note, every layer's account of it, and what the
// database has on record.
func TestTriageExplain(t *testing.T) {
	root, rels := triageArchive(t)
	var out bytes.Buffer
	if err := runTriage(&out, triageOpts{explain: filepath.Join(root, filepath.FromSlash(rels[0]))}); err != nil {
		t.Fatalf("--explain: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"location: archive tree, " + rels[0],
		"recorded: disposition archive",
		"is NOISE by rules:noise:github-notifications",
		"layer 0 protect: no stored document attachment",
		"layer 1 headers: no bulk markers",
		"layer 2 rules: noise github-notifications",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("explain does not say %q:\n%s", want, text)
		}
	}
	// A copy outside both trees is classified from the file alone.
	copyPath := filepath.Join(t.TempDir(), "copy.md")
	b, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(rels[1])))
	if err := os.WriteFile(copyPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runTriage(&out, triageOpts{explain: copyPath}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "outside both") || !strings.Contains(out.String(), "is SIGNAL by protect:attachment") {
		t.Errorf("explain of a stray copy:\n%s", out.String())
	}
}

// TestInitWritesTriageRules: `comms init` ships the starter rules beside the
// config, 0600, and leaves an existing file alone.
func TestInitWritesTriageRules(t *testing.T) {
	cfgDir, stateHome := t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runInit(cmd); err != nil {
		t.Fatalf("init: %v", err)
	}
	rulesPath := filepath.Join(cfgDir, "triage.toml")
	info, err := os.Stat(rulesPath)
	if err != nil {
		t.Fatalf("triage.toml not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("triage.toml mode = %04o, want 0600", info.Mode().Perm())
	}
	if _, err := triage.LoadRules(rulesPath); err != nil {
		t.Errorf("the written rules do not load: %v", err)
	}
	if !strings.Contains(out.String(), "wrote starter rules") || !strings.Contains(out.String(), "comms triage --dry-run") {
		t.Errorf("init output:\n%s", out.String())
	}
	// Second run: untouched.
	if err := os.WriteFile(rulesPath, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runInit(cmd); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rulesPath); string(b) != "# mine\n" {
		t.Error("init overwrote an existing triage.toml")
	}
	if !strings.Contains(out.String(), "already exists — left untouched") {
		t.Errorf("second init output:\n%s", out.String())
	}
}

// TestDoctorReportsTriage: the section names the rules file and its counts,
// the protect list, and the layer switches; a broken rules file is a
// problem, a missing one a note.
func TestDoctorReportsTriage(t *testing.T) {
	cfgDir, stateHome := t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), plainConfig(t.TempDir()))
	doctor := func() string {
		var out strings.Builder
		_ = runDoctorWith(&out, fakeCloudEnv(t.TempDir()))
		return out.String()
	}

	text := doctor()
	for _, want := range []string{
		"[triage] — noise triage",
		"no rules file at " + filepath.Join(cfgDir, "triage.toml"),
		"protect list: 1 own address(es), 0 protect_from, 5 protect_subject glob(s)",
		"header heuristics on",
		"model layer off",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor does not say %q:\n%s", want, text)
		}
	}

	if err := triage.WriteDefaultRules(filepath.Join(cfgDir, "triage.toml")); err != nil {
		t.Fatal(err)
	}
	if text := doctor(); !strings.Contains(text, "rules file "+filepath.Join(cfgDir, "triage.toml")+": 3 keep, 5 noise rule(s)") {
		t.Errorf("doctor with the starter rules:\n%s", text)
	}

	writeFile(t, filepath.Join(cfgDir, "triage.toml"), "[[noise]]\nname = \"x\"\n")
	if text := doctor(); !strings.Contains(text, "PROBLEM") || !strings.Contains(text, "sets no field") {
		t.Errorf("doctor with a broken rule:\n%s", text)
	}
}

// TestStatusReportsTriageLedger: per instance, the verdict totals and the
// split by deciding rule.
func TestStatusReportsTriageLedger(t *testing.T) {
	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	src := state.InstanceID(state.SourceGmail, "work")
	dbPath := statusFixture(t, "", func(db *state.DB) {
		for _, d := range []state.TriageDecision{
			{Source: src, StableID: "a", Verdict: state.TriageNoise, Layer: state.TriageLayerRules, Rule: "noise:github", Reason: "r", Digest: "d"},
			{Source: src, StableID: "b", Verdict: state.TriageNoise, Layer: state.TriageLayerHeaders, Rule: "precedence:bulk", Reason: "r", Digest: "d"},
			{Source: src, StableID: "c", Verdict: state.TriageSignal, Layer: state.TriageLayerProtect, Rule: "attachment", Reason: "r", Digest: "d"},
			{Source: src, StableID: "d", Verdict: state.TriageUndecided, Layer: state.TriageLayerNone, Reason: "r", Digest: "d"},
		} {
			if err := db.UpsertTriageDecision(d); err != nil {
				t.Fatal(err)
			}
		}
	})
	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	printStatus(&out, rep)
	for _, want := range []string{
		"triage:       2 noise (filed under spam_root), 1 signal, 1 undecided",
		"noise     headers:precedence:bulk",
		"noise     rules:noise:github",
		"signal    protect:attachment",
		"undecided none",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status does not say %q:\n%s", want, out.String())
		}
	}
	if rep.Sources[0].Triage == nil || rep.Sources[0].Triage.Noise != 2 || len(rep.Sources[0].Triage.ByRule) != 4 {
		t.Errorf("json shape: %+v", rep.Sources[0].Triage)
	}
}
