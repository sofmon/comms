package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	chat "google.golang.org/api/chat/v1"

	"comms/internal/archive"
	"comms/internal/config"
	"comms/internal/paths"
	"comms/internal/state"
)

// writeFile writes content with 0600, creating the parent directory (0700).
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeTestConfig writes a minimal valid config with one [[google]] account.
// At least one account is now mandatory, but no credentials are needed:
// openApp never touches them.
func writeTestConfig(t *testing.T, path, root, tz string) {
	t.Helper()
	writeFile(t, path, fmt.Sprintf(`archive_root = %q
timezone = %q

[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
chat    = true
`, root, tz))
}

// twoAccountConfig is the real target shape: two Google identities, each
// providing Gmail and Chat, plus one FastMail account. personal carries its
// own client_file, the arrangement a second Workspace org requires.
func twoAccountConfig(root, clientFile string) string {
	return fmt.Sprintf(`archive_root = %q
timezone = "UTC"

[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
chat    = true

[[google]]
label   = "personal"
account = "you@example.net"
gmail   = true
chat    = true
client_file = %q

[[fastmail]]
label   = "fm"
account = "you@fastmail.com"
`, root, clientFile)
}

// setTestEnv points config and state resolution at temp dirs and neutralizes
// the COMMS_* overrides that could leak in from the invoking environment,
// including the per-label variants the test configs would pick up.
func setTestEnv(t *testing.T, cfgDir, stateHome string) {
	t.Helper()
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("COMMS_ARCHIVE_ROOT", "")
	t.Setenv("COMMS_OUTBOX_ROOT", "")
	t.Setenv("COMMS_GOOGLE_CLIENT_FILE", "")
	t.Setenv("COMMS_GOOGLE_TOKEN_FILE", "")
	t.Setenv("COMMS_FASTMAIL_TOKEN", "")
	for _, label := range []string{"WORK", "PERSONAL", "FM"} {
		t.Setenv("COMMS_GOOGLE_CLIENT_FILE_"+label, "")
		t.Setenv("COMMS_GOOGLE_TOKEN_FILE_"+label, "")
		t.Setenv("COMMS_FASTMAIL_TOKEN_"+label, "")
	}
}

// loadTestConfig writes content into a fresh config dir and loads it.
func loadTestConfig(t *testing.T, content string) *config.Config {
	t.Helper()
	cfgDir := t.TempDir()
	setTestEnv(t, cfgDir, t.TempDir())
	path := filepath.Join(cfgDir, "config.toml")
	writeFile(t, path, content)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeGoogleCreds fabricates a usable OAuth client file and a token file
// whose recorded grant covers scopes, so credential checks and
// googleauth.TokenSource succeed without any network or browser.
func writeGoogleCreds(t *testing.T, clientFile, tokenFile string, scopes []string) {
	t.Helper()
	writeFile(t, clientFile, `{"installed":{"client_id":"cid.apps.googleusercontent.com",`+
		`"client_secret":"secret","redirect_uris":["http://localhost"],`+
		`"auth_uri":"https://accounts.google.com/o/oauth2/auth",`+
		`"token_uri":"https://oauth2.googleapis.com/token"}}`)
	env := struct {
		Token  *oauth2.Token `json:"token"`
		Scopes []string      `json:"scopes"`
	}{
		Token:  &oauth2.Token{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)},
		Scopes: scopes,
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, tokenFile, string(b))
}

func TestGoogleScopesIncludeOutboundAndDeduplicateReads(t *testing.T) {
	got := googleScopes(config.GoogleAccount{Gmail: true, Chat: true, SendEmail: true, SendChat: true})
	want := []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.compose",
		chat.ChatSpacesReadonlyScope,
		chat.ChatMessagesReadonlyScope,
		chat.ChatMembershipsReadonlyScope,
		chat.ChatMessagesCreateScope,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("googleScopes = %v, want %v", got, want)
	}
}

func TestOpenAppPinsLocalTimezone(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeTestConfig(t, cfgPath, root, "local")

	_, wantZone, err := config.ResolveTimezone("local")
	if err != nil {
		t.Fatalf("ResolveTimezone: %v", err)
	}

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	if a.tzName != wantZone {
		t.Errorf("tzName = %q, want %q", a.tzName, wantZone)
	}
	pinned, ok, err := a.db.GetMeta(state.MetaArchiveTZ)
	if err != nil || !ok {
		t.Fatalf("meta archive_tz: %q ok=%v err=%v", pinned, ok, err)
	}
	if pinned != wantZone {
		t.Errorf("meta archive_tz = %q, want %q", pinned, wantZone)
	}
	a.Close()

	// The pin must be written back into the config file so it survives
	// state-DB loss.
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"local"`) {
		t.Errorf("config still says local after pinning:\n%s", b)
	}
	if !strings.Contains(string(b), fmt.Sprintf("%q", wantZone)) {
		t.Errorf("config does not contain the pinned zone %q:\n%s", wantZone, b)
	}

	// A second open with matching pin succeeds.
	a2, err := openApp()
	if err != nil {
		t.Fatalf("second openApp: %v", err)
	}
	a2.Close()
}

func TestOpenAppRefusesTimezoneMismatch(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeTestConfig(t, cfgPath, root, "Europe/Amsterdam")

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	a.Close()

	// Simulate a state DB pinned in a different zone (e.g. the config was
	// hand-edited after the first run).
	db, err := state.Open(filepath.Join(paths.StateDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(state.MetaArchiveTZ, "Pacific/Kiritimati"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = openApp()
	if err == nil {
		t.Fatal("openApp succeeded despite a timezone mismatch")
	}
	for _, want := range []string{"Pacific/Kiritimati", "Europe/Amsterdam", "refusing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error %q does not mention %q", err, want)
		}
	}
}

func TestOpenAppKeepsExplicitTimezoneUnpinned(t *testing.T) {
	// An explicit IANA zone in the config is already a pin: the file must
	// not be rewritten.
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	cfgPath := filepath.Join(cfgDir, "config.toml")
	writeTestConfig(t, cfgPath, root, "Europe/Amsterdam")
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	if a.tzName != "Europe/Amsterdam" {
		t.Errorf("tzName = %q, want Europe/Amsterdam", a.tzName)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config file was rewritten for an explicit timezone:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestResolveInstances pins the --source selector grammar: a kind, an
// instance id, or an account label, each resolving to instances in the
// canonical order and deduplicated across overlapping selectors.
func TestResolveInstances(t *testing.T) {
	cfg := loadTestConfig(t, twoAccountConfig(t.TempDir(), filepath.Join(t.TempDir(), "personal.json")))

	for _, tc := range []struct {
		name    string
		sel     []string
		want    []string
		wantErr string
	}{
		{
			name: "no selector is every instance",
			sel:  nil,
			want: []string{"gmail:work", "gmail:personal", "gchat:work", "gchat:personal", "fastmail:fm"},
		},
		{name: "kind", sel: []string{"gmail"}, want: []string{"gmail:work", "gmail:personal"}},
		{name: "kind gchat", sel: []string{"gchat"}, want: []string{"gchat:work", "gchat:personal"}},
		{name: "instance id", sel: []string{"gchat:personal"}, want: []string{"gchat:personal"}},
		{name: "label spans kinds", sel: []string{"work"}, want: []string{"gmail:work", "gchat:work"}},
		{name: "fastmail label", sel: []string{"fm"}, want: []string{"fastmail:fm"}},
		{name: "fastmail kind", sel: []string{"fastmail"}, want: []string{"fastmail:fm"}},
		{
			name: "overlapping selectors dedup and keep canonical order",
			sel:  []string{"work", "gmail"},
			want: []string{"gmail:work", "gmail:personal", "gchat:work"},
		},
		{name: "unknown", sel: []string{"nope"}, wantErr: `unknown --source "nope"`},
		{name: "unknown instance id", sel: []string{"gmail:nope"}, wantErr: `unknown --source "gmail:nope"`},
		{name: "kind of another account's label", sel: []string{"gmail:fm"}, wantErr: `unknown --source "gmail:fm"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveInstances(cfg, tc.sel)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveInstances(%v) succeeded, want error %q", tc.sel, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInstances(%v): %v", tc.sel, err)
			}
			var ids []string
			for _, in := range got {
				ids = append(ids, in.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("resolveInstances(%v) = %v, want %v", tc.sel, ids, tc.want)
			}
		})
	}
}

// TestResolveInstancesKindWithNoAccounts keeps the "valid kind, nothing
// configured" case distinct from a typo.
func TestResolveInstancesKindWithNoAccounts(t *testing.T) {
	cfg := loadTestConfig(t, `archive_root = "/tmp/comms-test"
timezone = "UTC"

[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
`)
	_, err := resolveInstances(cfg, []string{"fastmail"})
	if err == nil {
		t.Fatal("selecting an unconfigured kind succeeded")
	}
	for _, want := range []string{"no account is configured for that source kind", "gmail:work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestResolveInstancesAmbiguousSelector: an account may legally be labelled
// "gmail". A selector that is then both a kind and a label must be refused
// rather than silently interpreted one way.
func TestResolveInstancesAmbiguousSelector(t *testing.T) {
	cfg := loadTestConfig(t, `archive_root = "/tmp/comms-test"
timezone = "UTC"

[[google]]
label   = "gmail"
account = "you@example.com"
gmail   = true
chat    = true
`)
	_, err := resolveInstances(cfg, []string{"gmail"})
	if err == nil {
		t.Fatal("ambiguous selector was accepted")
	}
	for _, want := range []string{"ambiguous", "gmail:gmail", "gchat:gmail"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// The explicit instance ids stay usable.
	got, err := resolveInstances(cfg, []string{"gchat:gmail"})
	if err != nil || len(got) != 1 || got[0].ID != "gchat:gmail" {
		t.Fatalf("resolveInstances(gchat:gmail) = %v, %v", got, err)
	}
}

// TestBuildSourcesTwoAccounts is the end-to-end wiring check: a two-Google
// plus one-FastMail config must produce five independent connectors, each
// named by its own instance id, and --source must be able to narrow that to
// one account's instances.
func TestBuildSourcesTwoAccounts(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	personalClient := filepath.Join(cfgDir, "google-client-personal.json")
	writeFile(t, filepath.Join(cfgDir, "config.toml"), twoAccountConfig(root, personalClient))
	// The FastMail token comes from the per-label variable, so no token file
	// has to exist.
	t.Setenv("COMMS_FASTMAIL_TOKEN_FM", "fm-token")

	googleScopeSet := []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/chat.spaces.readonly",
		"https://www.googleapis.com/auth/chat.messages.readonly",
		"https://www.googleapis.com/auth/chat.memberships.readonly",
	}
	writeGoogleCreds(t, filepath.Join(cfgDir, "google-client.json"),
		filepath.Join(cfgDir, "google-token-work.json"), googleScopeSet)
	writeGoogleCreds(t, personalClient,
		filepath.Join(cfgDir, "google-token-personal.json"), googleScopeSet)

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	a.log = quietLogger()

	ctx := context.Background()
	srcs, err := a.buildSources(ctx, nil)
	if err != nil {
		t.Fatalf("buildSources: %v", err)
	}
	want := []string{"gmail:work", "gmail:personal", "gchat:work", "gchat:personal", "fastmail:fm"}
	var got []string
	for _, s := range srcs {
		got = append(got, s.conn.Name())
		if s.conn.Name() != s.inst.ID {
			t.Errorf("connector name %q does not match instance id %q", s.conn.Name(), s.inst.ID)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("built sources = %v, want %v", got, want)
	}
	// Every instance is a distinct object: nothing is reused between the two
	// accounts.
	seen := make(map[any]bool)
	for _, s := range srcs {
		if seen[s.conn] {
			t.Errorf("connector %s is shared with another instance", s.inst.ID)
		}
		seen[s.conn] = true
	}
	// Accounts are carried through for reporting.
	for _, s := range srcs {
		if s.inst.Label == "personal" && s.inst.Account != "you@example.net" {
			t.Errorf("%s account = %q", s.inst.ID, s.inst.Account)
		}
	}

	// --source work narrows to one account's two instances.
	only, err := a.buildSources(ctx, []string{"work"})
	if err != nil {
		t.Fatalf("buildSources(work): %v", err)
	}
	got = got[:0]
	for _, s := range only {
		got = append(got, s.inst.ID)
	}
	if strings.Join(got, ",") != "gmail:work,gchat:work" {
		t.Errorf("buildSources(work) = %v, want [gmail:work gchat:work]", got)
	}
}

// TestBuildSourcesReportsAccountOnMissingCredentials: with several accounts
// configured, a credential complaint has to say which one is broken.
func TestBuildSourcesReportsAccountOnMissingCredentials(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	personalClient := filepath.Join(cfgDir, "google-client-personal.json")
	writeFile(t, filepath.Join(cfgDir, "config.toml"), twoAccountConfig(root, personalClient))
	t.Setenv("COMMS_FASTMAIL_TOKEN_FM", "fm-token")
	// Only work is authorized; personal has no client file at all.
	writeGoogleCreds(t, filepath.Join(cfgDir, "google-client.json"),
		filepath.Join(cfgDir, "google-token-work.json"),
		[]string{"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/chat.spaces.readonly",
			"https://www.googleapis.com/auth/chat.messages.readonly",
			"https://www.googleapis.com/auth/chat.memberships.readonly"})

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	a.log = quietLogger()

	_, err = a.buildSources(context.Background(), nil)
	if err == nil {
		t.Fatal("buildSources succeeded with an unauthorized account")
	}
	for _, want := range []string{`"personal"`, "you@example.net", personalClient} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "work") {
		t.Errorf("error blames the healthy account too: %q", err)
	}
}

// chatFixture registers a space with one message for one instance and
// returns that instance's dirty day file.
func chatFixture(t *testing.T, db *state.DB, src, space, text string) state.DayFile {
	t.Helper()
	if err := db.UpsertSpace(src, space, "space", "Team Platform", "team-platform", "HISTORY_ON"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMember(src, space, "users/1", "Jane Doe"); err != nil {
		t.Fatal(err)
	}
	msg := state.ChatMessage{
		Source:     src,
		Name:       space + "/messages/m1",
		SenderID:   "users/1",
		CreateTime: time.Date(2026, 8, 7, 14, 30, 0, 0, time.UTC),
		DayBucket:  "2026-08-07",
		RawJSON:    `{"text":"` + text + `"}`,
	}
	page := state.ChatPage{Source: src, Space: space, Messages: []state.ChatMessage{msg}}
	if err := db.ApplyChatPage(context.Background(), page); err != nil {
		t.Fatal(err)
	}
	files, err := db.DirtyDayFiles(src)
	if err != nil || len(files) != 1 {
		t.Fatalf("dirty day files for %s = %d, %v; want 1", src, len(files), err)
	}
	return files[0]
}

func TestRenderDirtyChatDays(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const space = "spaces/AAA"
	src := state.InstanceID(state.SourceGChat, "work")
	f := chatFixture(t, db, src, space, "hello world")
	relPath := f.RelPath

	ctx := context.Background()
	root := filepath.Join(dir, "archive")
	a := &app{
		db:     db,
		writer: &archive.Writer{Root: root, TZ: time.UTC},
		log:    quietLogger(),
	}
	if err := a.renderDirtyChatDays(ctx, src); err != nil {
		t.Fatalf("renderDirtyChatDays: %v", err)
	}

	// The day is clean and its file exists with the message content.
	dirty, err := db.DirtyDayFiles(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Errorf("dirty days after render = %d, want 0", len(dirty))
	}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatalf("day file not written: %v", err)
	}
	content := string(b)
	for _, want := range []string{"hello world", "Jane Doe", "2026-08-07"} {
		if !strings.Contains(content, want) {
			t.Errorf("day file does not contain %q:\n%s", want, content)
		}
	}

	// Re-marking the day dirty and re-rendering is byte-identical (pure
	// projection), and healing on an already-clean ledger is a no-op.
	if err := db.MarkDayDirty(src, space, "2026-08-07"); err != nil {
		t.Fatal(err)
	}
	if err := a.renderDirtyChatDays(ctx, src); err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relPath)))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Error("re-render produced different bytes")
	}
}

// TestChatDayRenderIsInstanceScoped pins the multi-account contract for the
// two render entry points: healing covers every instance, while a sync
// pass's render touches only its own. Two accounts in the SAME space must
// also land in two separate files, distinguished by their file tags.
func TestChatDayRenderIsInstanceScoped(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const space = "spaces/SHARED"
	work := state.InstanceID(state.SourceGChat, "work")
	personal := state.InstanceID(state.SourceGChat, "personal")
	fWork := chatFixture(t, db, work, space, "from work")
	fPersonal := chatFixture(t, db, personal, space, "from personal")

	if fWork.RelPath == fPersonal.RelPath {
		t.Fatalf("both accounts render the shared space into one file: %s", fWork.RelPath)
	}
	for _, want := range []struct{ rel, tag string }{{fWork.RelPath, "gchat-work_"}, {fPersonal.RelPath, "gchat-personal_"}} {
		if !strings.Contains(want.rel, want.tag) {
			t.Errorf("rel path %q does not carry the file tag %q", want.rel, want.tag)
		}
		if strings.Contains(want.rel, ":") {
			t.Errorf("instance id leaked into the path %q", want.rel)
		}
	}

	ctx := context.Background()
	root := filepath.Join(dir, "archive")
	a := &app{
		db:     db,
		writer: &archive.Writer{Root: root, TZ: time.UTC},
		log:    quietLogger(),
	}

	// Startup healing renders both accounts' days.
	if err := a.healChatDays(ctx); err != nil {
		t.Fatalf("healChatDays: %v", err)
	}
	all, err := db.AllDirtyDayFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("dirty days after healing = %d, want 0", len(all))
	}
	for _, f := range []state.DayFile{fWork, fPersonal} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.RelPath)))
		if err != nil {
			t.Fatalf("healing did not write %s: %v", f.RelPath, err)
		}
		if !strings.Contains(string(b), "from "+strings.TrimPrefix(f.Source, "gchat:")) {
			t.Errorf("%s holds the wrong account's message:\n%s", f.RelPath, b)
		}
	}

	// A single instance's pass renders only its own days.
	if err := db.MarkDayDirty(work, space, "2026-08-07"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkDayDirty(personal, space, "2026-08-07"); err != nil {
		t.Fatal(err)
	}
	if err := a.renderDirtyChatDays(ctx, work); err != nil {
		t.Fatalf("renderDirtyChatDays(%s): %v", work, err)
	}
	all, err = db.AllDirtyDayFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Source != personal {
		t.Fatalf("dirty days after a work-only render = %+v, want only personal's", all)
	}
}

// TestRenderDirtyChatDaysContinuedThread pins the "(continued)" header: a
// thread that began on an earlier day must render its origin instant on the
// continuation day (min create_time from the DB, across day buckets).
func TestRenderDirtyChatDaysContinuedThread(t *testing.T) {
	dir := t.TempDir()
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const space = "spaces/AAA"
	src := state.InstanceID(state.SourceGChat, "work")
	if err := db.UpsertSpace(src, space, "space", "Team Platform", "team-platform", "HISTORY_ON"); err != nil {
		t.Fatal(err)
	}
	thread := space + "/threads/T1"
	msg := func(name string, at time.Time, day string) state.ChatMessage {
		return state.ChatMessage{
			Source:     src,
			Name:       space + "/messages/" + name,
			Thread:     thread,
			SenderID:   "users/1",
			CreateTime: at,
			DayBucket:  day,
			RawJSON:    `{"text":"` + name + `"}`,
		}
	}
	ctx := context.Background()
	page := state.ChatPage{Source: src, Space: space, Messages: []state.ChatMessage{
		msg("m1", time.Date(2026, 8, 6, 21, 15, 0, 0, time.UTC), "2026-08-06"),
		msg("m2", time.Date(2026, 8, 7, 9, 30, 0, 0, time.UTC), "2026-08-07"),
	}}
	if err := db.ApplyChatPage(ctx, page); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "archive")
	a := &app{
		db:     db,
		writer: &archive.Writer{Root: root, TZ: time.UTC},
		log:    quietLogger(),
	}
	if err := a.renderDirtyChatDays(ctx, src); err != nil {
		t.Fatalf("renderDirtyChatDays: %v", err)
	}

	files, err := db.DirtyDayFiles(src)
	if err != nil || len(files) != 0 {
		t.Fatalf("dirty after render = %d, %v; want 0", len(files), err)
	}
	var dayFiles []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
			dayFiles = append(dayFiles, p)
		}
		return err
	})
	if err != nil || len(dayFiles) != 2 {
		t.Fatalf("day files = %v, %v; want 2", dayFiles, err)
	}
	second, err := os.ReadFile(dayFiles[1]) // lexical walk: 08/07 after 08/06
	if err != nil {
		t.Fatal(err)
	}
	want := "## 09:30 — thread started 2026-08-06 21:15 (continued)"
	if !strings.Contains(string(second), want) {
		t.Errorf("continuation day misses %q:\n%s", want, second)
	}
	first, err := os.ReadFile(dayFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first), "(continued)") {
		t.Errorf("origin day must not claim continuation:\n%s", first)
	}
}

// TestClearCursorsIsInstanceScoped: --full on one account must not reset
// another account's cursors or backfill queue.
func TestClearCursorsIsInstanceScoped(t *testing.T) {
	cfgDir, stateHome, root := t.TempDir(), t.TempDir(), t.TempDir()
	setTestEnv(t, cfgDir, stateHome)
	writeFile(t, filepath.Join(cfgDir, "config.toml"),
		twoAccountConfig(root, filepath.Join(cfgDir, "google-client-personal.json")))

	a, err := openApp()
	if err != nil {
		t.Fatalf("openApp: %v", err)
	}
	defer a.Close()
	a.log = quietLogger()

	work := state.InstanceID(state.SourceGmail, "work")
	personal := state.InstanceID(state.SourceGmail, "personal")
	for _, src := range []string{work, personal} {
		if err := a.db.SetCursor(src, "", "history_id", "12345"); err != nil {
			t.Fatal(err)
		}
		if err := a.db.EnqueueBackfill(src, []string{"m1", "m2"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := a.clearCursors(work); err != nil {
		t.Fatalf("clearCursors: %v", err)
	}
	if _, ok, err := a.db.GetCursor(work, "", "history_id"); err != nil || ok {
		t.Errorf("work cursor survived --full (ok=%v, err=%v)", ok, err)
	}
	if n, err := a.db.BackfillPendingCount(work); err != nil || n != 0 {
		t.Errorf("work backfill = %d, %v; want 0", n, err)
	}
	if v, ok, err := a.db.GetCursor(personal, "", "history_id"); err != nil || !ok || v != "12345" {
		t.Errorf("personal cursor was cleared too: %q ok=%v err=%v", v, ok, err)
	}
	if n, err := a.db.BackfillPendingCount(personal); err != nil || n != 2 {
		t.Errorf("personal backfill = %d, %v; want 2", n, err)
	}
}
