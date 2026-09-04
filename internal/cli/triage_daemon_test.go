package cli

import (
	"context"
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

// archivingSource is a connector stub whose Sync archives one note the
// starter rules file as noise — exactly what a real pass would have done
// just before the after_sync hook runs.
type archivingSource struct {
	name string
	root string
	db   *state.DB
	rel  string
	err  error
}

func (s *archivingSource) Name() string                { return s.name }
func (s *archivingSource) Check(context.Context) error { return nil }
func (s *archivingSource) Sync(context.Context) error {
	if s.err != nil {
		return s.err
	}
	content := triageNote("GitHub <notifications@github.com>", "'[org/repo] Bump deps'", "attachments: []\n")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(s.root, filepath.FromSlash(s.rel))), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.root, filepath.FromSlash(s.rel)), []byte(content), 0o600); err != nil {
		return err
	}
	return s.db.CommitMessage(state.Message{
		Source: s.name, StableID: "gh", TS: time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
		DayBucket: "2026-08-07", RelPath: s.rel, ContentHash: "h",
	})
}

// afterSyncApp opens a real app over a config with the given [triage] block
// and the starter rules.
func afterSyncApp(t *testing.T, triageBlock string) (*app, string) {
	t.Helper()
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"), `archive_root = "`+root+`"
timezone = "UTC"
`+triageBlock+`
[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
chat    = true
`)
	if err := triage.WriteDefaultRules(filepath.Join(cfgDir, "triage.toml")); err != nil {
		t.Fatal(err)
	}
	if err := paths.EnsureDir(paths.StateDir()); err != nil {
		t.Fatal(err)
	}
	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	t.Cleanup(a.Close)
	return a, root
}

const afterSyncRel = "2026/08/07/100000_gmail-work_bump_aaaaaaaa.md"

// TestAfterSyncTriagesTheMailJustArchived: with after_sync on, a clean
// pass is followed by a rules-only triage of that instance, recorded as
// its own "triage" run row; the sync's own run row is untouched by it.
func TestAfterSyncTriagesTheMailJustArchived(t *testing.T) {
	a, root := afterSyncApp(t, "[triage]\nafter_sync = true\n")
	src := &archivingSource{name: state.InstanceID(state.SourceGmail, "work"), root: root, db: a.db, rel: afterSyncRel}
	if err := a.runSourcePass(context.Background(), src); err != nil {
		t.Fatalf("runSourcePass: %v", err)
	}
	cfg, _ := config.Load(config.DefaultPath())
	if !statAt(t, cfg.SpamRoot, afterSyncRel) || statAt(t, root, afterSyncRel) {
		t.Error("the note the pass archived was not triaged")
	}
	m, _, _ := a.db.GetMessage(src.name, "gh")
	if m.Disposition != state.DispositionSpam || m.DispositionRule != "rules:noise:github-notifications" {
		t.Errorf("row: %+v", m)
	}
	runs, err := a.db.RecentRuns(src.name, 5)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range runs {
		kinds = append(kinds, r.Kind)
		if !r.Finished || !r.OK {
			t.Errorf("run %s not finished ok: %+v", r.Kind, r)
		}
	}
	if strings.Join(kinds, ",") != "triage,sync" {
		t.Errorf("run rows = %v, want a triage run after the sync run", kinds)
	}
}

// TestAfterSyncIsOffByDefaultAndSkipsFailedPasses: no hook without the
// switch; no hook after a failed sync; never for chat.
func TestAfterSyncIsOffByDefaultAndSkipsFailedPasses(t *testing.T) {
	a, root := afterSyncApp(t, "")
	src := &archivingSource{name: state.InstanceID(state.SourceGmail, "work"), root: root, db: a.db, rel: afterSyncRel}
	if err := a.runSourcePass(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	if !statAt(t, root, afterSyncRel) {
		t.Error("after_sync = false still triaged")
	}
	if runs, _ := a.db.RecentRuns(src.name, 5); len(runs) != 1 || runs[0].Kind != "sync" {
		t.Errorf("run rows with after_sync off: %+v", runs)
	}

	on, root2 := afterSyncApp(t, "[triage]\nafter_sync = true\n")
	failing := &archivingSource{name: state.InstanceID(state.SourceGmail, "work"), root: root2, db: on.db, rel: afterSyncRel, err: context.DeadlineExceeded}
	if err := on.runSourcePass(context.Background(), failing); err == nil {
		t.Fatal("a failing sync reported success")
	}
	if runs, _ := on.db.RecentRuns(failing.name, 5); len(runs) != 1 {
		t.Errorf("a failed pass still ran triage: %+v", runs)
	}
	chat := &archivingSource{name: state.InstanceID(state.SourceGChat, "work"), root: root2, db: on.db, rel: afterSyncRel}
	chat.err = nil
	// A chat "sync" that archives nothing: the hook must not even start.
	chatSrc := &plainRefetcher{name: chat.name}
	if err := on.runSourcePass(context.Background(), chatSrc); err != nil {
		t.Fatal(err)
	}
	for _, r := range mustRuns(t, on.db, chat.name) {
		if r.Kind == "triage" {
			t.Error("triage ran after a chat pass")
		}
	}
}

func mustRuns(t *testing.T, db *state.DB, src string) []state.SyncRun {
	t.Helper()
	runs, err := db.RecentRuns(src, 10)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// TestAfterSyncNeverCallsTheModelUnlessAskedTo: with the model enabled but
// in_daemon off, the hook is rules-only and an unreachable endpoint costs
// nothing; with in_daemon on, the model is consulted.
func TestAfterSyncNeverCallsTheModelUnlessAskedTo(t *testing.T) {
	calls := 0
	s := llmServer(t, `{"noise": true, "reason": "bulk"}`, 200)
	counting := llmServerCounting(t, &calls)
	_ = s

	a, root := afterSyncApp(t, "[triage]\nafter_sync = true\n[triage.llm]\nenabled = true\nbase_url = \""+counting.URL+"/v1\"\nmodel = \"m\"\n")
	plain := &archivingSource{name: state.InstanceID(state.SourceGmail, "work"), root: root, db: a.db, rel: "2026/08/07/120000_gmail-work_hello_cccccccc.md"}
	// A note the rules leave undecided.
	writeFile(t, filepath.Join(root, filepath.FromSlash(plain.rel)), triageNote("Friend <friend@example.org>", "hello there", "attachments: []\n"))
	if err := a.db.CommitMessage(state.Message{Source: plain.name, StableID: "plain", TS: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), DayBucket: "2026-08-07", RelPath: plain.rel, ContentHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := a.runSourcePass(context.Background(), &plainRefetcher{name: plain.name}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("the after_sync hook called the model %d time(s) with in_daemon off", calls)
	}
	if !statAt(t, root, plain.rel) {
		t.Error("an undecided note moved without the model")
	}
	a.Close()

	b, root2 := afterSyncApp(t, "[triage]\nafter_sync = true\n[triage.llm]\nenabled = true\nin_daemon = true\nbase_url = \""+counting.URL+"/v1\"\nmodel = \"m\"\n")
	writeFile(t, filepath.Join(root2, filepath.FromSlash(plain.rel)), triageNote("Friend <friend@example.org>", "hello there", "attachments: []\n"))
	if err := b.db.CommitMessage(state.Message{Source: plain.name, StableID: "plain", TS: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC), DayBucket: "2026-08-07", RelPath: plain.rel, ContentHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := b.runSourcePass(context.Background(), &plainRefetcher{name: plain.name}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("the model was called %d time(s) with in_daemon on, want 1", calls)
	}
	cfg, _ := config.Load(config.DefaultPath())
	if !statAt(t, cfg.SpamRoot, plain.rel) {
		t.Error("the model's verdict was not applied in the daemon")
	}
}
