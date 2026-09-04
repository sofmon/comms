package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"comms/internal/state"
)

// clearEnv blanks every env var the package reads so ambient developer
// environment cannot leak into tests. t.Setenv also restores originals.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"COMMS_CONFIG_DIR", "COMMS_ARCHIVE_ROOT", "COMMS_OUTBOX_ROOT", "COMMS_FASTMAIL_TOKEN",
		"COMMS_GOOGLE_CLIENT_FILE", "COMMS_GOOGLE_TOKEN_FILE",
		"COMMS_FASTMAIL_TOKEN_FM", "COMMS_FASTMAIL_TOKEN_FM_TWO",
		"COMMS_GOOGLE_CLIENT_FILE_WORK", "COMMS_GOOGLE_TOKEN_FILE_WORK",
		"COMMS_GOOGLE_CLIENT_FILE_PERSONAL", "COMMS_GOOGLE_TOKEN_FILE_PERSONAL",
		"XDG_CONFIG_HOME", "XDG_STATE_HOME", "TZ",
	} {
		t.Setenv(k, "")
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalValid = `
[[google]]
label   = "work"
account = "jane@example.com"
gmail   = true
chat    = true

[[fastmail]]
label   = "fm"
account = "me@fastmail.com"
`

// twoGoogle is the real target shape: two Google identities, each with Gmail
// and Chat, plus one FastMail account.
const twoGoogle = `
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
client_file = "/etc/comms/google-client-personal.json"

[[fastmail]]
label   = "fm"
account = "me@fastmail.com"
`

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)

	cfg, err := Load(writeConfig(t, minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	home, _ := os.UserHomeDir()
	if got, want := cfg.ArchiveRoot, filepath.Join(home, "Archive"); got != want {
		t.Errorf("ArchiveRoot = %q, want %q (tilde-expanded default)", got, want)
	}
	if cfg.Timezone != "local" {
		t.Errorf("Timezone = %q, want local", cfg.Timezone)
	}
	if got := cfg.Daemon.GmailInterval.Duration(); got != 5*time.Minute {
		t.Errorf("GmailInterval = %v, want 5m", got)
	}
	if got := cfg.Daemon.GChatInterval.Duration(); got != 2*time.Minute {
		t.Errorf("GChatInterval = %v, want 2m", got)
	}
	if got := cfg.Daemon.FastMailInterval.Duration(); got != 5*time.Minute {
		t.Errorf("FastMailInterval = %v, want 5m", got)
	}
	g := cfg.Google[0]
	if g.IncludeDrafts || g.MirrorDriveFiles || g.ShowDeleted || g.Reactions {
		t.Errorf("boolean options should default to false: %+v", g)
	}
	if got, want := g.ClientFilePath, filepath.Join(cfgDir, "google-client.json"); got != want {
		t.Errorf("ClientFilePath = %q, want the shared default %q", got, want)
	}
	if got, want := g.TokenFilePath, filepath.Join(cfgDir, "google-token-work.json"); got != want {
		t.Errorf("TokenFilePath = %q, want %q", got, want)
	}
	f := cfg.FastMail[0]
	if got, want := f.TokenFilePath, filepath.Join(cfgDir, "fastmail-token-fm"); got != want {
		t.Errorf("FastMail TokenFilePath = %q, want %q", got, want)
	}
	if f.Token != "" {
		t.Errorf("FastMail Token = %q, want empty without env", f.Token)
	}
}

func TestInstances(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	cfg, err := Load(writeConfig(t, twoGoogle))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := cfg.Instances()
	want := []Instance{
		{ID: "gmail:work", Kind: state.SourceGmail, Label: "work", Account: "you@example.com"},
		{ID: "gmail:personal", Kind: state.SourceGmail, Label: "personal", Account: "you@example.net"},
		{ID: "gchat:work", Kind: state.SourceGChat, Label: "work", Account: "you@example.com"},
		{ID: "gchat:personal", Kind: state.SourceGChat, Label: "personal", Account: "you@example.net"},
		{ID: "fastmail:fm", Kind: state.SourceFastmail, Label: "fm", Account: "me@fastmail.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("Instances = %+v; want %d entries", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Instances[%d] = %+v; want %+v", i, got[i], want[i])
		}
	}
	if tag := got[0].Tag(); tag != "gmail-work" {
		t.Errorf("Tag = %q; want gmail-work", tag)
	}

	if in := cfg.InstancesForKind(state.SourceGChat); len(in) != 2 || in[1].ID != "gchat:personal" {
		t.Errorf("InstancesForKind(gchat) = %+v", in)
	}
	if in, ok := cfg.InstanceByID("gmail:personal"); !ok || in.Label != "personal" {
		t.Errorf("InstanceByID = %+v, %v", in, ok)
	}
	if _, ok := cfg.InstanceByID("gmail:nope"); ok {
		t.Error("InstanceByID found an unconfigured instance")
	}
	if g, ok := cfg.GoogleByLabel("personal"); !ok || g.Account != "you@example.net" {
		t.Errorf("GoogleByLabel = %+v, %v", g, ok)
	}
	if f, ok := cfg.FastMailByLabel("fm"); !ok || f.Account != "me@fastmail.com" {
		t.Errorf("FastMailByLabel = %+v, %v", f, ok)
	}
	if _, ok := cfg.GoogleByLabel("fm"); ok {
		t.Error("GoogleByLabel matched a FastMail label")
	}
	// Intervals stay global per kind.
	if cfg.Interval(state.SourceGChat) != 2*time.Minute || cfg.Interval("nope") != 0 {
		t.Errorf("Interval = %v / %v", cfg.Interval(state.SourceGChat), cfg.Interval("nope"))
	}
}

func TestInstancesSkipDisabledHalves(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	cfg, err := Load(writeConfig(t, `
[[google]]
label   = "mailonly"
account = "a@b.c"
gmail   = true

[[google]]
label   = "chatonly"
account = "d@e.f"
chat    = true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, in := range cfg.Instances() {
		ids = append(ids, in.ID)
	}
	if len(ids) != 2 || ids[0] != "gmail:mailonly" || ids[1] != "gchat:chatonly" {
		t.Fatalf("Instances = %v; want [gmail:mailonly gchat:chatonly]", ids)
	}
}

func TestLoadFull(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)
	cfg, err := Load(writeConfig(t, `
archive_root = "/data/archive"
timezone     = "Europe/Amsterdam"

[daemon]
gmail_interval    = "10m"
gchat_interval    = "90s"
fastmail_interval = "1h"

[[google]]
label   = "work"
account = "jane@example.com"
gmail   = true
chat    = true
include_drafts     = true
mirror_drive_files = true
show_deleted       = true
reactions          = false

[[google]]
label       = "personal"
account     = "jane@example.net"
gmail       = true
client_file = "~/creds/personal.json"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ArchiveRoot != "/data/archive" {
		t.Errorf("ArchiveRoot = %q", cfg.ArchiveRoot)
	}
	if cfg.Timezone != "Europe/Amsterdam" {
		t.Errorf("Timezone = %q", cfg.Timezone)
	}
	if got := cfg.Daemon.GmailInterval.Duration(); got != 10*time.Minute {
		t.Errorf("GmailInterval = %v", got)
	}
	if got := cfg.Daemon.GChatInterval.Duration(); got != 90*time.Second {
		t.Errorf("GChatInterval = %v", got)
	}
	if got := cfg.Daemon.FastMailInterval.Duration(); got != time.Hour {
		t.Errorf("FastMailInterval = %v", got)
	}
	g := cfg.Google[0]
	if !g.IncludeDrafts || !g.MirrorDriveFiles || !g.ShowDeleted || g.Reactions {
		t.Errorf("google[0] = %+v", g)
	}
	// Per-account client_file wins over the shared default and is
	// tilde-expanded; the token stays per label.
	home, _ := os.UserHomeDir()
	s := cfg.Google[1]
	if want := filepath.Join(home, "creds", "personal.json"); s.ClientFilePath != want {
		t.Errorf("personal ClientFilePath = %q, want %q", s.ClientFilePath, want)
	}
	if want := filepath.Join(cfgDir, "google-token-personal.json"); s.TokenFilePath != want {
		t.Errorf("personal TokenFilePath = %q, want %q", s.TokenFilePath, want)
	}
	if s.Chat {
		t.Error("personal chat should be false")
	}
	if len(cfg.FastMail) != 0 {
		t.Errorf("FastMail = %+v; want none", cfg.FastMail)
	}
}

func TestEnvOverridesPerLabel(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)
	t.Setenv("COMMS_ARCHIVE_ROOT", "/env/root")
	t.Setenv("COMMS_GOOGLE_CLIENT_FILE_WORK", "/env/client-work.json")
	t.Setenv("COMMS_GOOGLE_TOKEN_FILE_PERSONAL", "/env/token-personal.json")
	t.Setenv("COMMS_FASTMAIL_TOKEN_FM", "fmt1-secret")
	// The unsuffixed forms must be IGNORED while several accounts of the
	// kind exist: they would silently point both identities at one file.
	t.Setenv("COMMS_GOOGLE_CLIENT_FILE", "/env/shared-client.json")
	t.Setenv("COMMS_GOOGLE_TOKEN_FILE", "/env/shared-token.json")

	cfg, err := Load(writeConfig(t, twoGoogle))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ArchiveRoot != "/env/root" {
		t.Errorf("ArchiveRoot = %q, want env override /env/root", cfg.ArchiveRoot)
	}
	work, personal := cfg.Google[0], cfg.Google[1]
	if work.ClientFilePath != "/env/client-work.json" {
		t.Errorf("work ClientFilePath = %q", work.ClientFilePath)
	}
	if want := filepath.Join(cfgDir, "google-token-work.json"); work.TokenFilePath != want {
		t.Errorf("work TokenFilePath = %q, want the per-label default %q (unsuffixed env must not apply)", work.TokenFilePath, want)
	}
	if personal.TokenFilePath != "/env/token-personal.json" {
		t.Errorf("personal TokenFilePath = %q", personal.TokenFilePath)
	}
	if personal.ClientFilePath != "/etc/comms/google-client-personal.json" {
		t.Errorf("personal ClientFilePath = %q, want the config value (unsuffixed env must not apply)", personal.ClientFilePath)
	}
	if cfg.FastMail[0].Token != "fmt1-secret" {
		t.Errorf("FastMail Token = %q", cfg.FastMail[0].Token)
	}
}

func TestEnvOverridesUnsuffixedSingleAccount(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	t.Setenv("COMMS_GOOGLE_CLIENT_FILE", "/env/client.json")
	t.Setenv("COMMS_GOOGLE_TOKEN_FILE", "/env/token.json")
	t.Setenv("COMMS_FASTMAIL_TOKEN", "fmt1-secret")

	cfg, err := Load(writeConfig(t, minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	g := cfg.Google[0]
	if g.ClientFilePath != "/env/client.json" || g.TokenFilePath != "/env/token.json" {
		t.Errorf("unsuffixed env not applied to the single account: %+v", g)
	}
	if cfg.FastMail[0].Token != "fmt1-secret" {
		t.Errorf("FastMail Token = %q", cfg.FastMail[0].Token)
	}

	// The per-label form still wins over the unsuffixed one.
	t.Setenv("COMMS_GOOGLE_CLIENT_FILE_WORK", "/env/client-work.json")
	cfg, err = Load(writeConfig(t, minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Google[0].ClientFilePath != "/env/client-work.json" {
		t.Errorf("per-label env did not win: %q", cfg.Google[0].ClientFilePath)
	}
}

func TestEnvSuffix(t *testing.T) {
	if got := EnvSuffix("acme-corp"); got != "ACME_CORP" {
		t.Errorf("EnvSuffix = %q, want ACME_CORP", got)
	}
}

func TestTildeExpansion(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(writeConfig(t, `archive_root = "~/MyArchive"`+minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.ArchiveRoot, filepath.Join(home, "MyArchive"); got != want {
		t.Errorf("ArchiveRoot = %q, want %q", got, want)
	}

	// Env override values are tilde-expanded too.
	t.Setenv("COMMS_ARCHIVE_ROOT", "~/EnvArchive")
	cfg, err = Load(writeConfig(t, minimalValid))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.ArchiveRoot, filepath.Join(home, "EnvArchive"); got != want {
		t.Errorf("ArchiveRoot = %q, want %q", got, want)
	}

	// ~user is not supported and must fail loudly, not silently misresolve.
	t.Setenv("COMMS_ARCHIVE_ROOT", "") // undo the override from the case above
	if _, err := Load(writeConfig(t, `archive_root = "~root/x"`+minimalValid)); err == nil {
		t.Error("want error for ~user path")
	}
	if _, err := Load(writeConfig(t, minimalValid+"\n[[google]]\nlabel=\"two\"\naccount=\"a@b.c\"\ngmail=true\nclient_file=\"~root/x.json\"\n")); err == nil {
		t.Error("want error for ~user client_file")
	}
}

func TestLoadErrors(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	tests := []struct {
		name    string
		content string
		wantSub string
	}{
		{"unknown key", `archive_roots = "/x"` + minimalValid, "unknown key"},
		{"unknown section key", minimalValid + "\n[gmail2]\nx = 1\n", "unknown key"},
		{"unknown account key", minimalValid + "\n[[google]]\nlabel=\"x\"\naccount=\"a@b.c\"\ngmail=true\nnope=1\n", "unknown key"},
		{"bad duration", `[daemon]` + "\n" + `gmail_interval = "5 minutes"` + minimalValid, "duration"},
		{"zero interval", `[daemon]` + "\n" + `gmail_interval = "0s"` + minimalValid, "daemon.gmail_interval"},
		{"negative interval", `[daemon]` + "\n" + `gchat_interval = "-2m"` + minimalValid, "daemon.gchat_interval"},
		{"empty archive_root", `archive_root = ""` + minimalValid, "archive_root"},
		{"bad timezone", `timezone = "Nope/Nowhere"` + minimalValid, "timezone"},
		{"empty timezone", `timezone = ""` + minimalValid, "timezone"},
		{"no accounts at all", `archive_root = "/x"` + "\n", "no accounts are enabled"},
		{"google without account", "[[google]]\nlabel = \"x\"\ngmail = true\n", "account is required"},
		{"google with nothing enabled", "[[google]]\nlabel = \"x\"\naccount = \"a@b.c\"\n", "nothing to archive"},
		{"fastmail without account", "[[fastmail]]\nlabel = \"fm\"\n", "account is required"},
		// reactions is accepted syntactically but the pass is unimplemented:
		// silently ignoring the opt-in would be data loss relative to what the
		// config promises, so Validate rejects it loudly.
		{"reactions unimplemented", "[[google]]\nlabel=\"x\"\naccount=\"a@b.c\"\nchat=true\nreactions=true\n", "not implemented"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

func TestLabelValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	tests := []struct {
		name    string
		content string
		wantSub []string
	}{
		{
			"missing label",
			"[[google]]\naccount = \"a@b.c\"\ngmail = true\n",
			[]string{"label is required", "orphans"},
		},
		{
			"uppercase label",
			"[[google]]\nlabel = \"WORK\"\naccount = \"a@b.c\"\ngmail = true\n",
			[]string{"is invalid", LabelPattern, "orphans"},
		},
		{
			"label with underscore",
			"[[google]]\nlabel = \"a_b\"\naccount = \"a@b.c\"\ngmail = true\n",
			[]string{"is invalid", "orphans"},
		},
		{
			"label starting with hyphen",
			"[[google]]\nlabel = \"-x\"\naccount = \"a@b.c\"\ngmail = true\n",
			[]string{"is invalid"},
		},
		{
			"label too long",
			"[[google]]\nlabel = \"" + strings.Repeat("a", 21) + "\"\naccount = \"a@b.c\"\ngmail = true\n",
			[]string{"is invalid"},
		},
		{
			"duplicate labels within a kind",
			"[[google]]\nlabel=\"dup\"\naccount=\"a@b.c\"\ngmail=true\n[[google]]\nlabel=\"dup\"\naccount=\"d@e.f\"\ngmail=true\n",
			[]string{"already used by", "unique across all accounts", "orphans"},
		},
		{
			"duplicate label across kinds",
			"[[google]]\nlabel=\"dup\"\naccount=\"a@b.c\"\ngmail=true\n[[fastmail]]\nlabel=\"dup\"\naccount=\"d@e.f\"\n",
			[]string{"already used by"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, sub := range tt.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error does not mention %q:\n%v", sub, err)
				}
			}
		})
	}

	// Hyphenated labels of legal length are fine.
	if _, err := Load(writeConfig(t, "[[google]]\nlabel=\"acme-corp-1\"\naccount=\"a@b.c\"\ngmail=true\n")); err != nil {
		t.Errorf("hyphenated label rejected: %v", err)
	}
}

func TestLegacySingleAccountFormatIsRejected(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	legacy := []struct{ name, content string }{
		{"gmail table", "[gmail]\nenabled = true\naccount = \"a@b.c\"\n"},
		{"gchat table", "[gchat]\nenabled = true\n"},
		{"fastmail table", "[fastmail]\nenabled = true\naccount = \"a@b.c\"\n"},
		{"all three", "[gmail]\naccount=\"a@b.c\"\n[gchat]\nenabled=true\n[fastmail]\naccount=\"d@e.f\"\n"},
		// Single brackets on the new key: a forgotten bracket must not
		// surface as a TOML type error.
		{"single-bracket google", "[google]\nlabel=\"work\"\naccount=\"a@b.c\"\ngmail=true\n"},
	}
	for _, tt := range legacy {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil {
				t.Fatal("legacy config accepted")
			}
			msg := err.Error()
			for _, sub := range []string{"[[google]]", "[[fastmail]]", "label"} {
				if !strings.Contains(msg, sub) {
					t.Errorf("legacy error does not name %q:\n%v", sub, err)
				}
			}
			if strings.Contains(msg, "unknown key") {
				t.Errorf("legacy config produced the cryptic unknown-key error:\n%v", err)
			}
		})
	}

	// The new array-of-tables form must NOT be mistaken for the old table.
	if _, err := Load(writeConfig(t, minimalValid)); err != nil {
		t.Errorf("[[fastmail]] array of tables rejected as legacy: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	clearEnv(t)
	_, err := Load(filepath.Join(t.TempDir(), "config.toml"))
	if err == nil || !strings.Contains(err.Error(), "comms init") {
		t.Fatalf("want error pointing at `comms init`, got: %v", err)
	}
}

func TestWriteSkeleton(t *testing.T) {
	clearEnv(t)
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)
	path := filepath.Join(cfgDir, "config.toml")

	if err := WriteSkeleton(path); err != nil {
		t.Fatalf("WriteSkeleton: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("skeleton mode = %04o, want 0600", perm)
	}
	dirInfo, err := os.Stat(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %04o, want 0700", perm)
	}

	// The skeleton must parse and carry the documented defaults.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("skeleton does not load: %v", err)
	}
	if cfg.Timezone != "local" || cfg.Daemon.GChatInterval.Duration() != 2*time.Minute {
		t.Errorf("skeleton values unexpected: tz=%q gchat=%v", cfg.Timezone, cfg.Daemon.GChatInterval.Duration())
	}
	// Two Google accounts (the second demonstrating client_file) and one
	// FastMail account: five instances in total.
	if len(cfg.Google) != 2 || len(cfg.FastMail) != 1 {
		t.Fatalf("skeleton accounts = %d google, %d fastmail; want 2 and 1", len(cfg.Google), len(cfg.FastMail))
	}
	if len(cfg.Instances()) != 5 {
		t.Errorf("skeleton instances = %+v; want 5", cfg.Instances())
	}
	if cfg.Google[1].ClientFile == "" {
		t.Error("skeleton's second google block should demonstrate client_file")
	}
	if cfg.Google[0].ClientFilePath != filepath.Join(cfgDir, "google-client.json") {
		t.Errorf("skeleton's first google block should use the shared client: %q", cfg.Google[0].ClientFilePath)
	}
	for _, g := range cfg.Google {
		if g.Account == "" || g.Label == "" {
			t.Errorf("skeleton google block incomplete: %+v", g)
		}
	}
	if cfg.FastMail[0].Account == "" {
		t.Error("skeleton should carry a placeholder fastmail account")
	}

	if err := WriteSkeleton(path); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second WriteSkeleton should refuse to overwrite, got: %v", err)
	}
}

func TestPinTimezone(t *testing.T) {
	clearEnv(t)
	const before = `# header comment
archive_root = "~/Archive"
timezone     = "local"          # resolved+pinned to an IANA zone on first run

[daemon]
gmail_interval = "5m"
`
	const after = `# header comment
archive_root = "~/Archive"
timezone     = "Europe/Amsterdam"          # resolved+pinned to an IANA zone on first run

[daemon]
gmail_interval = "5m"
`
	path := writeConfig(t, before)
	if err := PinTimezone(path, "Europe/Amsterdam"); err != nil {
		t.Fatalf("PinTimezone: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != after {
		t.Errorf("pinned file mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, after)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("pinned file mode = %04o, want 0600 preserved", perm)
	}

	// Already pinned: nothing to rewrite.
	if err := PinTimezone(path, "Europe/Amsterdam"); err == nil {
		t.Error("second pin should fail (no local line left)")
	}
}

func TestPinTimezoneRejectsInvalidZone(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "timezone = \"local\"\n")
	if err := PinTimezone(path, "Nope/Nowhere"); err == nil {
		t.Fatal("want error for invalid zone")
	}
}

func TestPinTimezoneSkeletonRoundTrip(t *testing.T) {
	clearEnv(t)
	cfgDir := t.TempDir()
	t.Setenv("COMMS_CONFIG_DIR", cfgDir)
	path := filepath.Join(cfgDir, "config.toml")
	if err := WriteSkeleton(path); err != nil {
		t.Fatal(err)
	}
	if err := PinTimezone(path, "America/New_York"); err != nil {
		t.Fatalf("PinTimezone: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after pin: %v", err)
	}
	if cfg.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q after pin", cfg.Timezone)
	}
}

func TestResolveTimezoneExplicit(t *testing.T) {
	clearEnv(t)
	loc, name, err := ResolveTimezone("Europe/Amsterdam")
	if err != nil {
		t.Fatalf("ResolveTimezone: %v", err)
	}
	if name != "Europe/Amsterdam" || loc.String() != "Europe/Amsterdam" {
		t.Errorf("got loc=%v name=%q", loc, name)
	}
	if _, _, err := ResolveTimezone("Nope/Nowhere"); err == nil {
		t.Error("want error for unknown zone")
	}
}

func TestResolveTimezoneLocal(t *testing.T) {
	clearEnv(t)
	origLink := localtimeLink
	t.Cleanup(func() { localtimeLink = origLink })

	t.Run("from localtime symlink", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "localtime")
		if err := os.Symlink("/usr/share/zoneinfo/Europe/Berlin", link); err != nil {
			t.Fatal(err)
		}
		localtimeLink = link
		loc, name, err := ResolveTimezone("local")
		if err != nil {
			t.Fatalf("ResolveTimezone: %v", err)
		}
		if name != "Europe/Berlin" {
			t.Errorf("name = %q, want Europe/Berlin", name)
		}
		if loc == nil {
			t.Error("loc is nil")
		}
	})

	t.Run("fallback to TZ", func(t *testing.T) {
		localtimeLink = filepath.Join(t.TempDir(), "missing")
		t.Setenv("TZ", "America/New_York")
		_, name, err := ResolveTimezone("local")
		if err != nil {
			t.Fatalf("ResolveTimezone: %v", err)
		}
		if name != "America/New_York" {
			t.Errorf("name = %q, want America/New_York", name)
		}
	})

	t.Run("fallback to UTC", func(t *testing.T) {
		localtimeLink = filepath.Join(t.TempDir(), "missing")
		t.Setenv("TZ", "")
		loc, name, err := ResolveTimezone("local")
		if err != nil {
			t.Fatalf("ResolveTimezone: %v", err)
		}
		if name != "UTC" || loc != time.UTC {
			t.Errorf("got loc=%v name=%q, want UTC fallback", loc, name)
		}
	})

	t.Run("empty value means local", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "localtime")
		if err := os.Symlink("/var/db/timezone/zoneinfo/Europe/Berlin", link); err != nil {
			t.Fatal(err)
		}
		localtimeLink = link
		_, name, err := ResolveTimezone("")
		if err != nil {
			t.Fatalf("ResolveTimezone: %v", err)
		}
		if name != "Europe/Berlin" {
			t.Errorf("name = %q, want Europe/Berlin", name)
		}
	})
}

func TestSendingConfigAndInstances(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	base := t.TempDir()
	cfg, err := Load(writeConfig(t, `archive_root = "`+filepath.Join(base, "archive")+`"
[sending]
root = "`+filepath.Join(base, "outbox")+`"
[[google]]
label = "work"
account = "you@example.com"
send_email = true
send_chat = true
[[fastmail]]
label = "fm"
account = "you@fastmail.example"
send_email = true
`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Sending.SendDir(), filepath.Join(base, "outbox", "send"); got != want {
		t.Errorf("SendDir = %q, want %q", got, want)
	}
	got := cfg.SendingInstances()
	want := []string{"gmail:work", "gchat:work", "fastmail:fm"}
	if len(got) != len(want) {
		t.Fatalf("SendingInstances = %+v", got)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("SendingInstances[%d] = %+v, want %s", i, got[i], want[i])
		}
	}
	if len(cfg.Instances()) != 1 || cfg.Instances()[0].ID != "fastmail:fm" {
		t.Fatalf("archive Instances = %+v; send-only Google must not become archive sources", cfg.Instances())
	}
}

func TestSendingRootDefaultOverrideAndNesting(t *testing.T) {
	clearEnv(t)
	t.Setenv("COMMS_CONFIG_DIR", t.TempDir())
	base := t.TempDir()
	archive := filepath.Join(base, "archive")
	cfg, err := Load(writeConfig(t, `archive_root = "`+archive+`"`+minimalValid))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Sending.Root, archive+"-outbox"; got != want {
		t.Fatalf("default sending.root = %q, want %q", got, want)
	}

	override := filepath.Join(base, "custom")
	t.Setenv("COMMS_OUTBOX_ROOT", override)
	cfg, err = Load(writeConfig(t, `archive_root = "`+archive+`"`+minimalValid))
	if err != nil || cfg.Sending.Root != override {
		t.Fatalf("override sending.root = %q, %v", cfg.Sending.Root, err)
	}

	t.Setenv("COMMS_OUTBOX_ROOT", "")
	_, err = Load(writeConfig(t, `archive_root = "`+archive+`"
[sending]
root = "`+filepath.Join(archive, "outbox")+`"
`+minimalValid))
	if err == nil || !strings.Contains(err.Error(), "separate sibling") {
		t.Fatalf("nested outbox error = %v", err)
	}
}
