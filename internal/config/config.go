// Package config loads, validates, and writes comms's config.toml, applies
// COMMS_* environment overrides, and handles the timezone pinning protocol
// (resolve "local" to an IANA zone once, then rewrite it into the file so
// the pin survives state-DB loss).
//
// Accounts are declared as repeatable blocks: one [[google]] block is one
// Google identity (a single OAuth consent) providing Gmail and/or Chat, and
// one [[fastmail]] block is one FastMail account. Every account carries a
// permanent `label` that becomes part of its instance id ("gmail:work"), of
// every filename it writes ("gmail-work"), and of its state keys.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"comms/internal/paths"
	"comms/internal/state"
)

// Duration is a time.Duration that unmarshals from TOML duration strings
// such as "5m" or "1h30m".
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler for BurntSushi/toml.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q (want e.g. \"5m\", \"1h30m\"): %w", text, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// Config is the parsed, validated configuration. All TOML fields have
// defaults; the resolved credential locations on each account are filled in
// from ConfigDir and the COMMS_* environment.
type Config struct {
	ArchiveRoot string `toml:"archive_root"`

	// SpamRoot is the parallel tree noise triage moves notes into, keeping
	// each note's YYYY/MM/DD/<name> path. Empty in the file means the
	// default, a "spam" directory beside archive_root; Load fills it in, so
	// after Load it is always absolute and never empty. It may not be inside
	// archive_root, nor archive_root inside it — see Validate.
	SpamRoot string `toml:"spam_root"`

	Timezone    string            `toml:"timezone"`
	Daemon      Daemon            `toml:"daemon"`
	Attachments Attachments       `toml:"attachments"`
	Triage      Triage            `toml:"triage"`
	Google      []GoogleAccount   `toml:"google"`
	FastMail    []FastMailAccount `toml:"fastmail"`
}

// Daemon holds the poll intervals, which stay global per source kind: every
// account of a kind is polled on the same schedule.
type Daemon struct {
	GmailInterval    Duration `toml:"gmail_interval"`
	GChatInterval    Duration `toml:"gchat_interval"`
	FastMailInterval Duration `toml:"fastmail_interval"`
}

// GoogleAccount is one [[google]] block: a single Google identity whose one
// OAuth grant covers both the Gmail and the Chat instance it enables.
type GoogleAccount struct {
	Label   string `toml:"label"`   // permanent; see LabelPattern
	Account string `toml:"account"` // the identity's email address
	Gmail   bool   `toml:"gmail"`   // archive this account's mail
	Chat    bool   `toml:"chat"`    // archive this account's Google Chat

	IncludeDrafts    bool `toml:"include_drafts"`
	MirrorDriveFiles bool `toml:"mirror_drive_files"`
	ShowDeleted      bool `toml:"show_deleted"`
	Reactions        bool `toml:"reactions"` // reserved; rejected by Validate

	// ClientFile optionally points this account at its own OAuth Desktop
	// client JSON; empty means the shared <configdir>/google-client.json.
	ClientFile string `toml:"client_file"`

	// Resolved credential locations (never read from TOML).
	ClientFilePath string `toml:"-"` // OAuth client JSON to use
	TokenFilePath  string `toml:"-"` // <configdir>/google-token-<label>.json
}

// FastMailAccount is one [[fastmail]] block.
type FastMailAccount struct {
	Label   string `toml:"label"`
	Account string `toml:"account"`

	// Resolved credential locations (never read from TOML).
	TokenFilePath string `toml:"-"` // <configdir>/fastmail-token-<label>
	// Token is the literal token from the environment when set; connectors
	// read TokenFilePath instead when it is empty.
	Token string `toml:"-"`
}

// LabelPattern is the account label grammar. Labels land in filenames and in
// state keys, so they are restricted to a short, lowercase, path-safe form.
const LabelPattern = `^[a-z0-9][a-z0-9-]{0,19}$`

var labelRE = regexp.MustCompile(LabelPattern)

// labelPermanence is appended to every label complaint: the label is an
// identity, not a display name.
const labelPermanence = "a label is permanent — it is part of every filename this account writes and of its sync state, " +
	"so renaming a label orphans that account's existing archive (the next sync re-downloads everything under the new label)"

// DefaultPath returns the canonical config file location.
func DefaultPath() string {
	return filepath.Join(paths.ConfigDir(), "config.toml")
}

// Default returns a Config with every non-account field at its documented
// default. Accounts have no defaults: at least one must be configured.
func Default() *Config {
	return &Config{
		ArchiveRoot: "~/Archive",
		Timezone:    "local",
		Daemon: Daemon{
			GmailInterval:    Duration(5 * time.Minute),
			GChatInterval:    Duration(2 * time.Minute),
			FastMailInterval: Duration(5 * time.Minute),
		},
		Attachments: DefaultAttachments(),
		Triage:      DefaultTriage(),
	}
}

// Load reads path, layers it over the defaults, resolves every account's
// credential locations (applying the COMMS_* overrides), expands ~ in
// archive_root, and validates. Unknown TOML keys are an error so typos
// cannot silently disable options.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config file %s does not exist — run `comms init` to create it", path)
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := checkLegacyFormat(path, data); err != nil {
		return nil, err
	}
	cfg := Default()
	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	if v := os.Getenv("COMMS_ARCHIVE_ROOT"); v != "" {
		cfg.ArchiveRoot = v
	}
	root, err := expandTilde(cfg.ArchiveRoot)
	if err != nil {
		return nil, fmt.Errorf("%s: archive_root: %w", path, err)
	}
	cfg.ArchiveRoot = root
	if v := os.Getenv("COMMS_SPAM_ROOT"); v != "" {
		cfg.SpamRoot = v
	}
	if cfg.SpamRoot == "" {
		cfg.SpamRoot = DefaultSpamRoot(cfg.ArchiveRoot)
	} else if cfg.SpamRoot, err = expandTilde(cfg.SpamRoot); err != nil {
		return nil, fmt.Errorf("%s: spam_root: %w", path, err)
	}
	// Resolve the keys whose default is another key's value before anything
	// reads them, so every caller computes the same policy digest.
	cfg.Attachments.applyDerived()
	if err := cfg.Triage.resolve(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.resolveCredentials(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// DefaultSpamRoot is the spam tree used when spam_root is not set: a "spam"
// directory beside archiveRoot (already tilde-expanded). Beside, not inside:
// the two trees are audited as one archive and must never nest.
func DefaultSpamRoot(archiveRoot string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(archiveRoot)), "spam")
}

// legacyProbe reports which single-bracket account sections a config still
// uses. [gmail] and [gchat] have no replacement key of the same name; a
// [fastmail] or [google] TABLE (as opposed to a [[…]] array of tables) is
// either the old single-account shape or a forgotten pair of brackets.
func legacyProbe(data []byte) []string {
	var probe map[string]any
	if _, err := toml.Decode(string(data), &probe); err != nil {
		return nil // malformed TOML: let the real decode report it
	}
	var found []string
	for _, k := range []string{"gmail", "gchat"} {
		if _, ok := probe[k]; ok {
			found = append(found, "["+k+"]")
		}
	}
	for _, k := range []string{"google", "fastmail"} {
		if v, ok := probe[k]; ok {
			if _, isTable := v.(map[string]any); isTable {
				found = append(found, "["+k+"]")
			}
		}
	}
	return found
}

// checkLegacyFormat turns an old single-account config (or a forgotten pair
// of brackets) into an actionable error naming the new per-account block
// format, instead of the cryptic "unknown key(s)" or "incompatible types"
// the decoder would otherwise produce.
func checkLegacyFormat(path string, data []byte) error {
	found := legacyProbe(data)
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(`%s: %s: accounts are declared as repeatable per-account blocks now, and every
account needs a permanent, unique label. One [[google]] block is ONE Google
identity (one OAuth consent) that can provide Gmail and/or Chat:

  [[google]]
  label   = "work"                 # permanent; appears in filenames
  account = "you@example.com"
  gmail   = true
  chat    = true
  include_drafts     = false
  mirror_drive_files = false
  show_deleted       = false

  [[fastmail]]
  label   = "fm"
  account = "you@fastmail.example"

Note the DOUBLE brackets: [[google]] and [[fastmail]] may be repeated once per
account. Run %s in an empty config dir to see a full example`,
		path, strings.Join(found, " / "), "`comms init`")
}

// resolveCredentials fills in each account's credential paths: per-label
// defaults under the config dir, the optional per-account client_file, then
// the COMMS_* environment overrides.
func (c *Config) resolveCredentials() error {
	cfgDir := paths.ConfigDir()
	var errs []error

	singleGoogle := len(c.Google) == 1
	for i := range c.Google {
		g := &c.Google[i]
		client := filepath.Join(cfgDir, "google-client.json")
		if g.ClientFile != "" {
			p, err := expandTilde(g.ClientFile)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: client_file: %w", googleRef(i, g.Label), err))
			} else {
				client = p
			}
		}
		// The token is always per-label: two identities can never share one.
		token := filepath.Join(cfgDir, tokenBase("google-token", g.Label)+".json")
		if v := envFor("COMMS_GOOGLE_CLIENT_FILE", g.Label, singleGoogle); v != "" {
			client = v
		}
		if v := envFor("COMMS_GOOGLE_TOKEN_FILE", g.Label, singleGoogle); v != "" {
			token = v
		}
		g.ClientFilePath, g.TokenFilePath = client, token
	}

	singleFastMail := len(c.FastMail) == 1
	for i := range c.FastMail {
		f := &c.FastMail[i]
		f.TokenFilePath = filepath.Join(cfgDir, tokenBase("fastmail-token", f.Label))
		f.Token = envFor("COMMS_FASTMAIL_TOKEN", f.Label, singleFastMail)
	}
	return errors.Join(errs...)
}

// tokenBase names a per-label credential file; an account with no (yet
// rejected) label still gets a deterministic name so error messages can
// mention a concrete path.
func tokenBase(prefix, label string) string {
	if label == "" {
		return prefix
	}
	return prefix + "-" + label
}

// envFor returns the value of the per-label environment variable
// <base>_<LABEL>, falling back to the unsuffixed <base> only when exactly
// one account of that kind is configured (single is the caller's check).
// LABEL is the label uppercased with '-' replaced by '_'.
func envFor(base, label string, single bool) string {
	if label != "" {
		if v := os.Getenv(base + "_" + EnvSuffix(label)); v != "" {
			return v
		}
	}
	if single {
		return os.Getenv(base)
	}
	return ""
}

// EnvSuffix renders a label the way the per-account COMMS_* variables spell
// it: uppercased, with hyphens turned into underscores.
func EnvSuffix(label string) string {
	return strings.ToUpper(strings.ReplaceAll(label, "-", "_"))
}

func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		if strings.HasPrefix(p, "~") {
			return "", fmt.Errorf("%q: ~user expansion is not supported", p)
		}
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// googleRef / fastMailRef name an account in error messages: by label when
// there is a usable one, by 1-based block position otherwise.
func googleRef(i int, label string) string { return blockRef("google", i, label) }

func fastMailRef(i int, label string) string { return blockRef("fastmail", i, label) }

// blockRef names a config block in diagnostics. The block index is always
// included: two blocks may carry the same label (that is itself an error),
// and "already used by [[google]] "work"" is useless if both blocks render
// identically.
func blockRef(kind string, i int, label string) string {
	if label == "" {
		return fmt.Sprintf("[[%s]] #%d", kind, i+1)
	}
	return fmt.Sprintf("[[%s]] #%d %q", kind, i+1, label)
}

// Validate reports every configuration problem at once via errors.Join.
func (c *Config) Validate() error {
	var errs []error
	if c.ArchiveRoot == "" {
		errs = append(errs, errors.New("archive_root must not be empty"))
	}
	// The spam tree mirrors the archive tree at identical rel paths and both
	// are walked by `comms verify`; one inside the other would make every
	// spam-filed note an orphan of the archive (or the reverse) and let a
	// move land a note inside the tree it was moved out of.
	switch {
	case c.SpamRoot == "":
		errs = append(errs, errors.New("spam_root must not be empty (omit it for the default: a \"spam\" directory beside archive_root)"))
	case c.ArchiveRoot != "" && paths.UnderDir(c.ArchiveRoot, c.SpamRoot):
		errs = append(errs, fmt.Errorf("spam_root %q is inside archive_root %q — the spam tree must sit beside the archive, never inside it", c.SpamRoot, c.ArchiveRoot))
	case c.ArchiveRoot != "" && paths.UnderDir(c.SpamRoot, c.ArchiveRoot):
		errs = append(errs, fmt.Errorf("archive_root %q is inside spam_root %q — the spam tree must sit beside the archive, never around it", c.ArchiveRoot, c.SpamRoot))
	}
	if c.Timezone == "" {
		errs = append(errs, errors.New(`timezone must not be empty (use "local" or an IANA name like "Europe/Amsterdam")`))
	} else if c.Timezone != "local" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			errs = append(errs, fmt.Errorf("timezone %q is not a valid IANA zone: %w", c.Timezone, err))
		}
	}
	for _, iv := range []struct {
		name string
		d    Duration
	}{
		{"daemon.gmail_interval", c.Daemon.GmailInterval},
		{"daemon.gchat_interval", c.Daemon.GChatInterval},
		{"daemon.fastmail_interval", c.Daemon.FastMailInterval},
	} {
		if iv.d.Duration() <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive duration, got %q", iv.name, iv.d.Duration()))
		}
	}
	if err := c.Attachments.validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Triage.validate(); err != nil {
		errs = append(errs, err)
	}

	// Labels are unique across every account of every kind: they key the
	// state store and the filenames of a single merged archive tree.
	seen := make(map[string]string)
	checkLabel := func(ref, label string) {
		switch {
		case label == "":
			errs = append(errs, fmt.Errorf("%s: label is required (short, unique, %s)", ref, labelPermanence))
			return
		case !labelRE.MatchString(label):
			errs = append(errs, fmt.Errorf("%s: label %q is invalid: it must match %s (1-20 chars: lowercase letters, digits and hyphens, not starting with a hyphen) — %s",
				ref, label, LabelPattern, labelPermanence))
			return
		}
		if prev, dup := seen[label]; dup {
			errs = append(errs, fmt.Errorf("%s: label %q is already used by %s: labels must be unique across all accounts of all kinds — %s",
				ref, label, prev, labelPermanence))
			return
		}
		seen[label] = ref
	}

	for i, g := range c.Google {
		ref := googleRef(i, g.Label)
		checkLabel(ref, g.Label)
		if g.Account == "" {
			errs = append(errs, fmt.Errorf("%s: account is required (the Google identity's email address)", ref))
		}
		if !g.Gmail && !g.Chat {
			errs = append(errs, fmt.Errorf("%s: nothing to archive — set gmail = true and/or chat = true, or delete the block", ref))
		}
		if g.Reactions {
			errs = append(errs, fmt.Errorf("%s: reactions = true is not implemented in this version — remove the option or set it to false", ref))
		}
	}
	for i, f := range c.FastMail {
		ref := fastMailRef(i, f.Label)
		checkLabel(ref, f.Label)
		if f.Account == "" {
			errs = append(errs, fmt.Errorf("%s: account is required", ref))
		}
	}
	if len(c.Instances()) == 0 {
		errs = append(errs, errors.New("no accounts are enabled — add at least one [[google]] block (with gmail and/or chat = true) or one [[fastmail]] block"))
	}
	return errors.Join(errs...)
}

// Instance is one configured source instance: a single account's single
// source kind. It is the unit connectors, cursors, state rows and filenames
// are keyed by.
type Instance struct {
	ID      string // "<kind>:<label>", e.g. "gmail:work" — the state source key
	Kind    string // state.SourceGmail | state.SourceGChat | state.SourceFastmail
	Label   string // the owning account's label
	Account string // the account's email address
}

// Tag is the filename-safe rendering of the instance id ("gmail-work"): the
// source component of every stem this instance writes. A colon can never
// reach a filename.
func (i Instance) Tag() string { return state.Tag(i.ID) }

func newInstance(kind, label, account string) Instance {
	return Instance{ID: state.InstanceID(kind, label), Kind: kind, Label: label, Account: account}
}

// Instances returns every enabled instance in a deterministic order: all
// Gmail instances, then all Chat instances, then all FastMail instances,
// each in config declaration order.
func (c *Config) Instances() []Instance {
	out := make([]Instance, 0, 2*len(c.Google)+len(c.FastMail))
	for _, g := range c.Google {
		if g.Gmail {
			out = append(out, newInstance(state.SourceGmail, g.Label, g.Account))
		}
	}
	for _, g := range c.Google {
		if g.Chat {
			out = append(out, newInstance(state.SourceGChat, g.Label, g.Account))
		}
	}
	for _, f := range c.FastMail {
		out = append(out, newInstance(state.SourceFastmail, f.Label, f.Account))
	}
	return out
}

// InstancesForKind returns the enabled instances of one source kind
// (state.SourceGmail, state.SourceGChat, state.SourceFastmail) in the same
// order Instances uses.
func (c *Config) InstancesForKind(kind string) []Instance {
	var out []Instance
	for _, in := range c.Instances() {
		if in.Kind == kind {
			out = append(out, in)
		}
	}
	return out
}

// InstanceByID looks an enabled instance up by its "<kind>:<label>" id.
func (c *Config) InstanceByID(id string) (Instance, bool) {
	for _, in := range c.Instances() {
		if in.ID == id {
			return in, true
		}
	}
	return Instance{}, false
}

// GoogleByLabel returns the [[google]] account with the given label.
func (c *Config) GoogleByLabel(label string) (GoogleAccount, bool) {
	for _, g := range c.Google {
		if g.Label == label {
			return g, true
		}
	}
	return GoogleAccount{}, false
}

// FastMailByLabel returns the [[fastmail]] account with the given label.
func (c *Config) FastMailByLabel(label string) (FastMailAccount, bool) {
	for _, f := range c.FastMail {
		if f.Label == label {
			return f, true
		}
	}
	return FastMailAccount{}, false
}

// Interval returns the daemon poll interval for a source kind; unknown kinds
// get zero.
func (c *Config) Interval(kind string) time.Duration {
	switch kind {
	case state.SourceGmail:
		return c.Daemon.GmailInterval.Duration()
	case state.SourceGChat:
		return c.Daemon.GChatInterval.Duration()
	case state.SourceFastmail:
		return c.Daemon.FastMailInterval.Duration()
	}
	return 0
}

// localtimeLink is a variable so tests can point the "local" resolution at
// a fake symlink.
var localtimeLink = "/etc/localtime"

// ResolveTimezone turns the config timezone value into a concrete
// *time.Location plus the IANA name to pin. Any value other than "local"
// (or "") is loaded directly. "local" is resolved from the host: the
// /etc/localtime symlink target (path suffix after "zoneinfo/"), then $TZ,
// then UTC with a logged warning.
func ResolveTimezone(cfgValue string) (*time.Location, string, error) {
	if cfgValue != "" && cfgValue != "local" {
		loc, err := time.LoadLocation(cfgValue)
		if err != nil {
			return nil, "", fmt.Errorf("timezone %q: %w", cfgValue, err)
		}
		return loc, cfgValue, nil
	}
	if name := zoneFromLocaltime(); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc, name, nil
		}
	}
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc, tz, nil
		}
	}
	slog.Warn("could not determine the host timezone from /etc/localtime or $TZ; pinning UTC — set timezone explicitly in config.toml to change it")
	return time.UTC, "UTC", nil
}

func zoneFromLocaltime() string {
	target, err := os.Readlink(localtimeLink)
	if err != nil {
		return "" // not a symlink (or absent): fall through to $TZ
	}
	const marker = "zoneinfo/"
	i := strings.LastIndex(target, marker)
	if i < 0 {
		return ""
	}
	return target[i+len(marker):]
}

var timezoneLocalLine = regexp.MustCompile(`(?m)^(\s*timezone\s*=\s*)"local"`)

// PinTimezone rewrites the `timezone = "local"` line in configPath to the
// resolved IANA zoneName, preserving every other byte of the file
// (indentation and trailing comments on the line included). It fails if no
// such line exists — e.g. the file was already pinned or hand-edited.
func PinTimezone(configPath, zoneName string) error {
	if _, err := time.LoadLocation(zoneName); err != nil {
		return fmt.Errorf("refusing to pin invalid zone %q: %w", zoneName, err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("pin timezone: %w", err)
	}
	m := timezoneLocalLine.FindSubmatchIndex(data)
	if m == nil {
		return fmt.Errorf(`%s: no 'timezone = "local"' line to pin (already pinned?)`, configPath)
	}
	// Splice: everything through the `timezone = ` prefix (submatch 1),
	// the quoted zone name, then everything after the original "local".
	out := make([]byte, 0, len(data)+len(zoneName))
	out = append(out, data[:m[3]]...)
	out = append(out, '"')
	out = append(out, zoneName...)
	out = append(out, '"')
	out = append(out, data[m[1]:]...)
	info, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("pin timezone: %w", err)
	}
	if err := os.WriteFile(configPath, out, info.Mode().Perm()); err != nil {
		return fmt.Errorf("pin timezone: %w", err)
	}
	return nil
}

const skeleton = `# comms — local communication archiver.
#
# Every account is one repeatable block with a permanent, unique "label".
# The label appears in every filename that account writes (gmail-work_…) and
# keys its sync state, so renaming one orphans that account's archive.
# Labels match ` + "`" + LabelPattern + "`" + `.
#
# Credential files live next to this config (all must be 0600):
#   google-client.json          shared OAuth Desktop client (unless client_file is set)
#   google-token-<label>.json   one OAuth token per [[google]] account
#   fastmail-token-<label>      one API token per [[fastmail]] account
#
# Each can be overridden per account from the environment, with the label
# uppercased and '-' turned into '_':
#   COMMS_GOOGLE_CLIENT_FILE_<LABEL>, COMMS_GOOGLE_TOKEN_FILE_<LABEL>,
#   COMMS_FASTMAIL_TOKEN_<LABEL>
# The unsuffixed forms still work, but only with a single account of that kind.

archive_root = "~/Archive"

# Where noise triage files notes it decides are noise, at the same
# YYYY/MM/DD/<name> path they had in the archive. Nothing is ever deleted;
# ` + "`comms untriage`" + ` moves a note back. Must be BESIDE archive_root — never inside
# it — and on the same volume (a move is an atomic rename).
# spam_root = "~/spam"          # default: a "spam" directory beside archive_root

# "local" is resolved from the host and PINNED back into this file on the first
# run, so the tree's day boundaries stay put afterwards. Naming an IANA zone
# here instead (e.g. "Europe/Amsterdam") makes day bucketing deterministic from
# the very first message, regardless of where the machine travels.
timezone     = "local"

[daemon]
# Poll intervals are global per source kind: every account of a kind polls
# on the same schedule.
gmail_interval    = "5m"
gchat_interval    = "2m"        # short: history-off spaces retain only 24h
fastmail_interval = "5m"

# Attachment storage policy.
#
# comms stores an attachment only when BOTH halves hold: its final extension is
# on the allowlist, AND the content sniffed from its magic bytes is one this
# allowlist permits for that extension. "invoice.pdf.exe" keys on .exe and is
# refused; a .pdf whose bytes are a zip is refused too.
#
# NOTHING IS EVER DROPPED SILENTLY. Every refusal is written into the note —
# both the frontmatter "skipped_attachments:" list and a visible entry under
# "## Attachments" — and into the state DB with the original name, the exact
# byte count, the declared and sniffed types, the reason, and the identity
# needed to fetch it again. Widen anything below and run ` + "`comms refetch`" + ` to pull
# the newly-permitted attachments in; a widened policy is NEVER acted on
# automatically, so a typo here cannot silently download gigabytes.
#
# Allowed by default: pdf; Word/Excel/PowerPoint, modern and legacy, plus their
# macro-FREE templates; OpenDocument; rtf; epub; txt md log csv tsv json xml
# yaml; images including heic/heif/avif/tiff; ics and vcf; audio and video;
# S/MIME and PGP parts; eml and msg; zip.
# Denied by default: macro-enabled Office (docm xlsm pptm dotm xltm potm), svg,
# 7z rar tar gz tgz bz2 xz cab, and every macOS-executable and Windows payload
# type. The executable types are a hard deny — allow_extensions cannot bring
# them back, and asking for one is a config error rather than a silent no-op.
[attachments]
# max_size      = "50MB"    # per attachment. Gmail caps a message near 50MB, so
#                           # this rarely binds there; Chat allows 200MB/file,
#                           # which is where it does.
# chat_max_size = "50MB"    # per Chat attachment; defaults to max_size
# max_message_bytes = "100MB"  # raw RFC822 bytes buffered before parsing
# max_per_message   = "150MB"  # total stored attachment bytes for one message
# max_parts_per_message = 500
# run_budget        = "2GB"    # per sync pass; the next pass continues. 0 = none
# free_space_floor  = "5GB"    # refuse attachments below this much free space,
#                              # but keep writing the (tiny) notes. 0 disables.
#
# These ADD to and SUBTRACT from the built-in lists above — they do not replace
# them. Deny always wins. A bare extension accepts any content for it (comms has
# no content table for a format it does not know); "ext=type/subtype" keeps both
# halves of the rule enforceable. svg, zip and the macro-enabled Office
# extensions are NOT set here — each has its own flag below, and naming one in
# allow_extensions is an error rather than a silent no-op.
# allow_extensions = ["7z=application/x-7z-compressed"]
# deny_extensions  = ["zip"]
#
# allow_containers = true   # keep .zip. comms NEVER decompresses it, so it is
#                           # inert bytes on disk — but it is opaque, and the
#                           # allowlist tells you nothing about the contents.
# allow_svg        = false  # SVG is the one image format that is a program, and
#                           # Obsidian (Electron) renders it in-note without any
#                           # double-click. Reversible later via ` + "`comms refetch`" + `.
# allow_macro_office = false  # docm/xlsm/pptm/dotm/xltm/potm must be denied by
#                             # EXTENSION: they sniff as plain docx/xlsx/pptx,
#                             # so content inspection cannot see the macros.
# on_mismatch = "skip"      # "skip", or "store-warn" to keep a file whose
#                           # content disagrees with its extension and say so.
#
# quarantine = true         # tag every stored attachment com.apple.quarantine.
#   BE CLEAR ABOUT WHAT THIS BUYS. It is NOT a malware scan: XProtect has no
#   signatures for pdf, Office, zip or image files — its signature set gates on
#   app bundles, installers and executables — so for everything on the allowlist
#   above it detects exactly nothing. What the tag actually does is make macOS
#   show a consent prompt on first open and make Word/Excel open the file in
#   Protected View. It is a speed bump and defence in depth; the allowlist is
#   the real boundary. The tag is also best-effort: iCloud sync degrades it, and
#   any app that safe-saves over the file drops it entirely.
#
# scan_command = ["clamdscan", "--fdpass", "--no-summary"]
#   Optional real scanner, run as argv (never through a shell) against the temp
#   file BEFORE it is renamed into the vault, so a rejected file never appears
#   there. Exit 0 = clean, 1 = infected, anything else = error, and an error is
#   never treated as clean. Point it at clamdscan against a running clamd, not
#   clamscan, which reloads a ~1GB signature database on every invocation.
# scan_action = "record"    # "record" stores a flagged file and records the
#                           # verdict; "reject" skips it. "record" is the default
#                           # because antivirus false positives on PDFs and
#                           # Office documents are common and official
#                           # corrections take days — failing closed would
#                           # delete real business documents.

# Noise triage: a separate pass (` + "`comms triage`" + `) that files newsletters,
# notifications and other noise under spam_root, at the same path they had in
# the archive. Nothing is deleted; ` + "`comms untriage`" + ` moves a note back. Sync is
# untouched — every message is archived first, exactly as before. Decisions
# run in layers and stop at the first decisive one: 0 the protect list below,
# 1 bulk-mail headers captured at archive time, 2 the rules in triage.toml
# (written by ` + "`comms init`" + `), 3 an optional local model. ` + "`comms triage --dry-run`" + `
# prints every move before anything moves.
[triage]
# after_sync = false        # run layers 0-2 after each successful sync pass
#                           # (also in the daemon); the model layer never runs
#                           # here unless llm.in_daemon is set.
# header_heuristics = true  # layer 1: Precedence: bulk/junk, Auto-Submitted,
#                           # X-Auto-Response-Suppress, Gmail Promotions/Social
# rules_file = "~/.config/comms/triage.toml"
#
# The protect list: never noise, whatever the other layers say. A note with a
# stored PDF/Office attachment and mail from your own addresses are protected
# always; these globs add to that. Case-insensitive, whole value, * and ?.
# protect_from    = []
# protect_subject = ["*invoice*", "*factuur*", "*receipt*", "*security alert*", "*new sign-in*"]

[triage.llm]
# The model is LAST and OPTIONAL: it sees only what layers 0-2 left undecided,
# gets the frontmatter fields plus a bounded body excerpt, and must answer with
# strict JSON. Anything else — a timeout, a refused connection, prose instead
# of JSON — is undecided, never noise. Its text is delimited and declared
# untrusted in the prompt, so a message cannot instruct the model.
# enabled        = false
# in_daemon      = false    # let the after_sync pass consult it too
# base_url       = "http://127.0.0.1:1234/v1"   # OpenAI-compatible (LM Studio, Ollama, ...)
# model          = ""                            # required when enabled
# timeout        = "20s"
# max_body_chars = 4000

# One [[google]] block is ONE Google identity; a single OAuth consent covers
# both its Gmail and its Chat.
[[google]]
label   = "work"
account = "you@example.com"
gmail   = true
chat    = true
include_drafts     = false
mirror_drive_files = false      # true adds drive.readonly (re-run ` + "`comms auth google work`" + `)
show_deleted       = false
# client_file = "~/.config/comms/google-client-work.json"   # optional; default google-client.json

# An "Internal" OAuth client only accepts users of its OWN Workspace
# organization, so an account in a different org needs its own Cloud project
# and client JSON (or an "External" published client, which brings
# verification and 7-day refresh-token expiry back).
[[google]]
label   = "personal"
account = "you@example.net"
gmail   = true
chat    = true
client_file = "~/.config/comms/google-client-personal.json"

[[fastmail]]
label   = "fm"
account = "you@fastmail.example"
`

// WriteSkeleton writes the commented example config for `comms init` with
// 0600 permissions, creating the parent directory (0700) if needed. It
// refuses to overwrite an existing file.
func WriteSkeleton(configPath string) error {
	if err := paths.EnsureDir(filepath.Dir(configPath)); err != nil {
		return err
	}
	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("config file %s already exists — not overwriting", configPath)
		}
		return fmt.Errorf("write config skeleton: %w", err)
	}
	if _, err := f.WriteString(skeleton); err != nil {
		f.Close()
		return fmt.Errorf("write config skeleton: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write config skeleton: %w", err)
	}
	return nil
}
