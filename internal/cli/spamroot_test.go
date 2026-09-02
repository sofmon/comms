package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"save/internal/config"
	"save/internal/policy"
	"save/internal/state"
)

// TestRefetchPassesTheNoteDisposition: the connector re-renders whichever
// copy of the note exists, so refetch must tell it which tree that is — read
// from the messages row, never assumed. Chat has no row and always means the
// archive tree; a mail message without a row is reported, not guessed.
func TestRefetchPassesTheNoteDisposition(t *testing.T) {
	db, _ := skipDB(t)
	at := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	mail := state.InstanceID(state.SourceGmail, "work")
	chat := state.InstanceID(state.SourceGChat, "work")
	for _, id := range []string{"filed", "kept", "rowless"} {
		seedSkip(t, db, state.SkippedAttachment{
			Source: mail, StableID: id, PartKey: "1", OrigName: "late.pdf",
			SniffedType: "application/pdf", Reason: policy.ReasonOverRunBudget, SizeBytes: 10, FirstSeenAt: at,
		})
	}
	seedSkip(t, db, state.SkippedAttachment{
		Source: chat, StableID: "spaces/A/messages/1", PartKey: "media-1", OrigName: "late.pdf",
		SniffedType: "application/pdf", Reason: policy.ReasonOverRunBudget, SizeBytes: 10, FirstSeenAt: at,
	})
	for _, id := range []string{"filed", "kept"} {
		if err := db.CommitMessage(state.Message{
			Source: mail, StableID: id, TS: at, DayBucket: "2026-08-07",
			RelPath: "2026/08/07/" + id + ".md", ContentHash: "h",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetDisposition(mail, "filed", state.DispositionSpam, "noise", "rules:noise:x", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}

	cfg := loadTestConfig(t, plainConfig(t.TempDir()))
	a := refetchApp(t, db, cfg)
	rows, err := db.ListUnresolvedSkipped(0)
	if err != nil {
		t.Fatal(err)
	}
	plan := planRefetch(a.policy, rows, cfg, cfg.Instances(), "")
	if plan.Candidates != 4 {
		t.Fatalf("candidates = %d, want 4", plan.Candidates)
	}
	gone := func(ids ...string) map[string]RefetchResult {
		out := map[string]RefetchResult{}
		for _, id := range ids {
			out[id] = RefetchResult{SourceGone: true}
		}
		return out
	}
	mailFake := &fakeRefetcher{name: mail, results: gone("filed", "kept", "rowless")}
	chatFake := &fakeRefetcher{name: chat, results: gone("spaces/A/messages/1")}
	byID := map[string]boundSource{
		mail: {inst: config.Instance{ID: mail, Kind: state.SourceGmail, Label: "work"}, conn: mailFake},
		chat: {inst: config.Instance{ID: chat, Kind: state.SourceGChat, Label: "work"}, conn: chatFake},
	}
	var out strings.Builder
	if _, err := a.executeRefetch(context.Background(), &out, plan, byID); err != nil {
		t.Fatalf("executeRefetch: %v", err)
	}

	got := map[string]state.Disposition{}
	for _, req := range append(mailFake.asked, chatFake.asked...) {
		got[req.StableID] = req.Disposition
	}
	want := map[string]state.Disposition{
		"filed":               state.DispositionSpam,
		"kept":                state.DispositionArchive,
		"rowless":             state.DispositionArchive,
		"spaces/A/messages/1": state.DispositionArchive,
	}
	for id, disp := range want {
		if got[id] != disp {
			t.Errorf("%s: connector was told disposition %q, want %q", id, got[id], disp)
		}
	}
}

// TestDoctorSpamRootChecks: doctor names the tree, refuses one on another
// volume (a rename cannot cross filesystems), and warns — without counting a
// problem — when it sits inside a sync tree or has no path budget left.
func TestDoctorSpamRootChecks(t *testing.T) {
	home := t.TempDir()
	archive := filepath.Join(home, "Archive")
	spam := filepath.Join(home, "spam")
	// Both exist, so the device probe sees them rather than their parent.
	for _, dir := range []string{archive, spam} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	run := func(spamRoot string, env cloudEnv) (string, int) {
		var out strings.Builder
		d := &doctorReport{out: &out}
		d.checkSpamRoot(archive, spamRoot, env)
		return out.String(), d.problems
	}

	env := fakeCloudEnv(home)
	env.device = func(string) (uint64, error) { return 7, nil }
	text, problems := run(spam, env)
	for _, want := range []string{spam, "shares a volume", "untriage", "nothing is ever deleted"} {
		if !strings.Contains(text, want) {
			t.Errorf("does not mention %q:\n%s", want, text)
		}
	}
	if problems != 0 {
		t.Errorf("the healthy case counted %d problem(s):\n%s", problems, text)
	}

	// Different volumes: every move would fail, so this is a problem.
	split := fakeCloudEnv(home)
	split.device = func(p string) (uint64, error) {
		if strings.HasPrefix(p, spam) {
			return 8, nil
		}
		return 7, nil
	}
	text, problems = run(spam, split)
	if problems != 1 || !strings.Contains(text, "different volume") {
		t.Errorf("a spam_root on another volume: problems = %d:\n%s", problems, text)
	}

	// An unanswerable device probe is reported, not treated as a failure.
	unknown := fakeCloudEnv(home)
	unknown.device = func(string) (uint64, error) { return 0, errors.New("no stat") }
	if text, problems := run(spam, unknown); problems != 0 || !strings.Contains(text, "could not tell") {
		t.Errorf("an unanswerable probe: problems = %d:\n%s", problems, text)
	}

	// Inside iCloud: a warning that the noise syncs too, still exit 0.
	cloud := filepath.Join(home, "Library", "Mobile Documents", "iCloud~md~obsidian", "Documents", "MyVault", "spam")
	text, problems = run(cloud, env)
	if problems != 0 || !strings.Contains(text, "leaves this machine") {
		t.Errorf("a spam_root in iCloud: problems = %d:\n%s", problems, text)
	}

	// No path budget left: a warning naming the cause.
	deep := filepath.Join(home, strings.Repeat("d", 900))
	text, problems = run(deep, env)
	if problems != 0 || !strings.Contains(text, "Shorten it") {
		t.Errorf("a too-long spam_root: problems = %d:\n%s", problems, text)
	}

	// The real probe on the real filesystem agrees two temp dirs share a
	// volume — and judges a not-yet-created spam_root by its parent.
	if same, err := sameDevice(deviceOf, home, filepath.Join(home, "not", "yet", "created")); err != nil || !same {
		t.Errorf("sameDevice on one temp volume = %v, %v", same, err)
	}
}

// TestSpamRootIsWiredEverywhere: the writer built by openApp resolves both
// trees, status reports the spam root, and the run records it in meta like
// the archive root.
func TestSpamRootIsWiredEverywhere(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeTestConfig(t, filepath.Join(cfgDir, "config.toml"), root, "UTC")

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	wantSpam := config.DefaultSpamRoot(root)
	if a.writer.SpamRoot != wantSpam {
		t.Errorf("writer.SpamRoot = %q, want %q", a.writer.SpamRoot, wantSpam)
	}
	if got, ok, err := a.db.GetMeta(state.MetaSpamRoot); err != nil || !ok || got != wantSpam {
		t.Errorf("meta spam_root = %q, %v, %v; want %q", got, ok, err, wantSpam)
	}
	a.Close()

	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := buildStatus(cfg, stateDBPath())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	printStatus(&out, rep)
	if rep.SpamRoot != wantSpam || !strings.Contains(out.String(), "spam root:    "+wantSpam) {
		t.Errorf("status does not report the spam root:\n%s", out.String())
	}
}
