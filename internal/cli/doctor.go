package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"

	"github.com/spf13/cobra"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/googleauth"
	"save/internal/paths"
	"save/internal/policy"
	"save/internal/ratelimit"
	"save/internal/source/fastmail"
	"save/internal/source/gchat"
	"save/internal/source/gmail"
	"save/internal/state"
	"save/internal/triage"
)

const checkTimeout = 30 * time.Second

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check config, directories, credentials, and connectivity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

// doctorReport accumulates check lines and the problem count.
type doctorReport struct {
	out      io.Writer
	problems int
}

func (d *doctorReport) ok(format string, args ...any) {
	fmt.Fprintf(d.out, "  ok       "+format+"\n", args...)
}

func (d *doctorReport) info(format string, args ...any) {
	fmt.Fprintf(d.out, "  note     "+format+"\n", args...)
}

// warn reports something that will bite later but is not wrong now, so it
// deliberately does NOT count as a problem: doctor still exits 0.
func (d *doctorReport) warn(format string, args ...any) {
	fmt.Fprintf(d.out, "  warn     "+format+"\n", args...)
}

func (d *doctorReport) bad(hint, format string, args ...any) {
	d.problems++
	fmt.Fprintf(d.out, "  PROBLEM  "+format+"\n", args...)
	if hint != "" {
		fmt.Fprintf(d.out, "\n%s\n\n", indent(hint, "           "))
	}
}

// section starts a per-account block of check lines.
func (d *doctorReport) section(format string, args ...any) {
	fmt.Fprintf(d.out, "\n"+format+"\n", args...)
}

// runDoctor checks the live setup with the real platform probes.
func runDoctor(out io.Writer) error {
	return runDoctorWith(out, defaultCloudEnv())
}

func runDoctorWith(out io.Writer, env cloudEnv) error {
	d := &doctorReport{out: out}

	// Config.
	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		d.bad("Run `save init`, then edit the skeleton it writes.", "config: %v", err)
		return fmt.Errorf("doctor found %d problem(s)", d.problems)
	}
	d.ok("config %s loads and validates", cfgPath)

	// Directories: exist and private.
	for _, dir := range []string{paths.ConfigDir(), paths.StateDir()} {
		info, err := os.Stat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			d.bad("Run `save init` to create it with the right permissions.", "directory %s does not exist", dir)
		case err != nil:
			d.bad("", "directory %s: %v", dir, err)
		case !info.IsDir():
			d.bad("", "%s is not a directory", dir)
		case info.Mode().Perm()&0o077 != 0:
			d.bad(fmt.Sprintf("Run: chmod 700 %s", dir), "directory %s has permissions %04o (group/world access)", dir, info.Mode().Perm())
		default:
			d.ok("directory %s (0700)", dir)
		}
	}

	// Timezone resolution and pin.
	loc, zone, tzErr := config.ResolveTimezone(cfg.Timezone)
	if tzErr != nil {
		d.bad(`Set timezone in config.toml to "local" or a valid IANA name like "Europe/Amsterdam".`,
			"timezone: %v", tzErr)
		loc = time.UTC
	} else {
		dbPath := stateDBPath()
		if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
			d.info("no state database yet — the first `save sync` will pin timezone %q", zone)
		} else if ro, roErr := openRO(dbPath); roErr != nil {
			d.bad("", "state database: %v", roErr)
		} else {
			pinned, has, merr := metaRO(ro, state.MetaArchiveTZ)
			ro.Close()
			switch {
			case merr != nil:
				d.bad("", "state database: %v", merr)
			case has && pinned != zone:
				d.bad(fmt.Sprintf("Set timezone = %q in %s (or delete %s to re-pin).", pinned, cfgPath, dbPath),
					"timezone mismatch: state pinned %q, config resolves %q — sync will refuse to start", pinned, zone)
			case has:
				d.ok("timezone pinned to %q and config agrees", pinned)
			default:
				d.info("state database exists but no timezone pinned yet — the next sync pins %q", zone)
			}
		}
	}

	// Archive storage: silent for a plain local directory, loud when the
	// archive lives inside iCloud Drive or another FileProvider.
	d.checkArchiveStorage(cfg.ArchiveRoot, env)

	// The spam tree: the one place a note can be besides the archive.
	d.checkSpamRoot(cfg.ArchiveRoot, cfg.SpamRoot, env)

	// The triage rules and switches.
	d.checkTriage(cfg)

	// The attachment storage policy: what it will keep, what it will refuse,
	// and — honestly — what the quarantine tag does and does not do.
	d.checkAttachmentPolicy(cfg)

	// Per-account credential and connectivity checks. Connectors are
	// constructed without a state DB: Check() is documented not to mutate or
	// read local state.
	log := logger()
	writer := &archive.Writer{
		Root:       cfg.ArchiveRoot,
		SpamRoot:   cfg.SpamRoot,
		TZ:         loc,
		Quarantine: archive.QuarantineFor(cfg.Attachments.Quarantine),
	}
	for _, acct := range cfg.Google {
		d.checkGoogleAccount(cfg, acct, writer, log)
	}
	for _, acct := range cfg.FastMail {
		d.checkFastMailAccount(acct, writer, log)
	}

	if d.problems > 0 {
		return fmt.Errorf("doctor found %d problem(s)", d.problems)
	}
	fmt.Fprintln(out, "\neverything looks good")
	return nil
}

// quarantineScope is the ONLY sanctioned wording for what the
// com.apple.quarantine tag buys, and it is deliberately unglamorous.
//
// This is not editorial caution, it is a verified fact: XProtect's signature
// set — all 94 signatures on this machine — gates on app bundles, installers
// and executables. It has ZERO signatures for pdf, OOXML, zip or images, so
// for every single type on save's allowlist the tag triggers no scan at all.
// Calling it "virus scanning" anywhere in this program would be a lie that a
// user could reasonably act on.
const quarantineScope = "What the tag actually does: macOS shows a consent prompt the first time you open\n" +
	"the file, and Word/Excel open it in Protected View. That is the whole benefit.\n" +
	"It is NOT a malware scan — XProtect has no signatures for PDFs, Office files,\n" +
	"zips or images, so for everything on the allowlist it inspects nothing. The tag\n" +
	"is also best-effort: iCloud sync degrades it, and any app that safe-saves over\n" +
	"the file drops it. The allowlist is the real boundary; this is a speed bump."

// checkAttachmentPolicy reports the effective attachment policy as a compact
// summary — the shape of the allowlist and the contested flags, never the
// full 57-extension list — plus the digest and whether the archive has been
// reconsidered under it.
func (d *doctorReport) checkAttachmentPolicy(cfg *config.Config) {
	d.section("[attachments] — attachment storage policy")

	pol, err := cfg.Policy()
	if err != nil {
		d.bad("Fix the [attachments] block in config.toml.", "%v", err)
		return
	}
	set := pol.Settings()
	exts := pol.AllowedExtensions()

	d.ok("allowlist: %d extension(s) — %s; %d executable type(s) can never be allowed",
		len(exts), sampleExts(exts, 8), len(policy.HardDeniedExtensions()))
	d.ok("contested defaults: zip %s, svg %s, macro-Office %s",
		allowedWord(set.AllowContainers), allowedWord(set.AllowSVG), allowedWord(set.AllowMacroOffice))
	if len(set.AllowExtensions) > 0 || len(set.DenyExtensions) > 0 {
		d.info("local overrides: allow_extensions %v, deny_extensions %v (deny always wins)",
			set.AllowExtensions, set.DenyExtensions)
	}
	d.ok("caps: %s per attachment (chat %s), %s and %d part(s) per message, %s per pass, %s free-space floor",
		capText(set.MaxSize), capText(pol.MaxSizeFor(true)), capText(set.MaxPerMessage),
		set.MaxPartsPerMessage, capText(set.RunBudget), capText(set.FreeSpaceFloor))

	switch set.OnMismatch {
	case policy.OnMismatchStoreWarn:
		d.warn("on_mismatch = %q: a file whose content disagrees with its extension is STORED, with the disagreement recorded in the note", set.OnMismatch)
	default:
		d.ok("on_mismatch = %q: extension/content disagreements are refused and recorded", set.OnMismatch)
	}
	if len(set.ScanCommand) == 0 {
		d.info("no scan_command configured — save runs no content scanner of its own")
	} else {
		d.ok("scan_command %v, scan_action %q (exit 0 clean, 1 flagged, anything else an error and never treated as clean)",
			set.ScanCommand, set.ScanAction)
	}

	if set.Quarantine {
		d.info("quarantine tagging is ON for stored attachments.%s", continued("\n"+quarantineScope))
	} else {
		d.info("quarantine tagging is OFF (attachments.quarantine = false); stored files carry no com.apple.quarantine tag")
	}

	d.checkPolicyDigest(pol.PolicyDigest())
}

// checkTriage reports the noise-triage configuration: whether the rules
// file parses and how many rules it holds, what the protect layer covers,
// and the state of the header and model layers. The model endpoint itself
// is not probed here unless the layer is enabled.
func (d *doctorReport) checkTriage(cfg *config.Config) {
	d.section("[triage] — noise triage")
	tr := cfg.Triage

	rules, err := triage.LoadRules(tr.RulesFilePath)
	switch {
	case err != nil:
		d.bad("Fix the rule and re-run; `save triage --dry-run` shows what the rules would do.", "%v", err)
	case len(rules.Keep)+len(rules.Noise) == 0:
		if _, statErr := os.Stat(tr.RulesFilePath); errors.Is(statErr, fs.ErrNotExist) {
			d.info("no rules file at %s — `save init` writes a starter; until then only the protect and header layers decide", tr.RulesFilePath)
		} else {
			d.info("rules file %s holds no rules — only the protect and header layers decide", tr.RulesFilePath)
		}
	default:
		d.ok("rules file %s: %d keep, %d noise rule(s)", tr.RulesFilePath, len(rules.Keep), len(rules.Noise))
	}
	d.ok("protect list: %d own address(es), %d protect_from, %d protect_subject glob(s); a stored PDF/Office attachment always protects",
		len(cfg.OwnAddresses()), len(tr.ProtectFrom), len(tr.ProtectSubject))
	if tr.HeaderHeuristics {
		d.ok("header heuristics on: Precedence bulk/junk, Auto-Submitted, X-Auto-Response-Suppress and Gmail Promotions/Social are decisive noise")
	} else {
		d.info("header heuristics off (triage.header_heuristics = false)")
	}
	if tr.AfterSync {
		d.info("after_sync = true: layers 0-2 run after every successful mail sync, in `save sync` and in the daemon")
	}
	if !tr.LLM.Enabled {
		d.info("model layer off (triage.llm.enabled = false) — undecided notes stay where they are")
		return
	}
	d.ok("model layer on: %s at %s, %s timeout, %d body chars; consulted only for notes the rules left undecided",
		tr.LLM.Model, tr.LLM.BaseURL, tr.LLM.Timeout.Duration(), tr.LLM.MaxBodyChars)
	if tr.LLM.InDaemon {
		d.warn("triage.llm.in_daemon = true: the daemon's after_sync pass also calls the model; a stopped endpoint costs one timeout per undecided note per pass")
	}
}

// checkPolicyDigest compares the digest the config now produces against the
// one recorded in the state database, reading it directly so doctor never
// creates or migrates anything.
func (d *doctorReport) checkPolicyDigest(digest string) {
	dbPath := stateDBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		d.info("policy digest %s — no state database yet, so nothing has been decided under it", digest)
		return
	}
	ro, err := openRO(dbPath)
	if err != nil {
		d.bad("", "state database: %v", err)
		return
	}
	recorded, has, err := metaRO(ro, state.MetaAttachmentPolicyDigest)
	ro.Close()
	switch {
	case err != nil:
		d.bad("", "state database: %v", err)
	case !has:
		d.info("policy digest %s — not recorded yet; the next `save sync` records it", digest)
	case recorded == digest:
		d.ok("policy digest %s matches the digest this archive was reconsidered under", digest)
	default:
		d.warn("policy digest %s differs from the recorded %s — attachments refused under the old policy may now be storable.%s",
			digest, recorded,
			continued("\nNothing is re-fetched automatically. Run `save status` for the count, then `save refetch --dry-run`."))
	}
}

// continued indents the second and later lines of a multi-line check line to
// sit under the first line's text, past the "  note     " gutter.
func continued(s string) string {
	return strings.ReplaceAll(s, "\n", "\n           ")
}

// sampleExts renders the first n allowlisted extensions plus a count of the
// rest: doctor is a health check, not a reference card.
func sampleExts(exts []string, n int) string {
	if len(exts) <= n {
		return strings.Join(exts, " ")
	}
	return strings.Join(exts[:n], " ") + fmt.Sprintf(" … and %d more", len(exts)-n)
}

func allowedWord(b bool) string {
	if b {
		return "allowed"
	}
	return "denied"
}

// capText renders a byte cap; <= 0 means the cap is off.
func capText(n int64) string {
	if n <= 0 {
		return "no limit"
	}
	return byteSize(n)
}

// checkGoogleAccount reports one [[google]] block: its own client and token
// files, its own scope set, and a live Check per instance it enables. Two
// accounts share nothing here — not even the client file, unless they were
// configured to.
func (d *doctorReport) checkGoogleAccount(cfg *config.Config, acct config.GoogleAccount, writer *archive.Writer, log *slog.Logger) {
	var kinds []string
	if acct.Gmail {
		kinds = append(kinds, state.SourceGmail)
	}
	if acct.Chat {
		kinds = append(kinds, state.SourceGChat)
	}
	d.section("[[google]] %q — %s (%s)", acct.Label, acct.Account, strings.Join(kinds, ", "))

	hint := googleSetup(acct)
	ok, detail := credFileState(acct.ClientFilePath)
	if !ok {
		d.bad(hint, "OAuth client file %s: %s", acct.ClientFilePath, detail)
		return
	}
	d.ok("OAuth client file %s (%s)", acct.ClientFilePath, detail)
	if shared := sharesClientFile(cfg, acct); shared != "" {
		d.info("this client JSON is shared with account %q — that only works if both accounts belong to the same Workspace organization (an \"Internal\" OAuth client rejects outside users); give one of them its own client_file otherwise", shared)
	}

	scopes := googleScopes(acct)
	switch missing, derr := googleauth.ScopeDrift(acct.TokenFilePath, scopes); {
	case errors.Is(derr, googleauth.ErrNeedsAuth):
		d.bad(fmt.Sprintf("Run: save auth google %s", acct.Label),
			"token %s: not authorized yet (%v)", acct.TokenFilePath, derr)
		return
	case derr != nil:
		d.bad("", "token %s: %v", acct.TokenFilePath, derr)
		return
	case len(missing) > 0:
		d.bad(fmt.Sprintf("Re-run `save auth google %s` and approve every permission.", acct.Label),
			"token is missing scopes the config needs: %v", missing)
		return
	}
	tokOK, tokDetail := credFileState(acct.TokenFilePath)
	if !tokOK {
		d.bad(fmt.Sprintf("Run: chmod 600 %s", acct.TokenFilePath), "token file %s: %s", acct.TokenFilePath, tokDetail)
		return
	}
	d.ok("token %s covers all required scopes (%s)", acct.TokenFilePath, tokDetail)

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	ts, err := googleauth.TokenSource(ctx, acct.ClientFilePath, acct.TokenFilePath, scopes)
	if err != nil {
		d.bad(fmt.Sprintf("Run: save auth google %s", acct.Label), "token source: %v", err)
		return
	}
	if acct.Gmail {
		id := state.InstanceID(state.SourceGmail, acct.Label)
		lim := ratelimit.NewUnits(gmail.UnitsPerSecond, gmail.UnitsBurst)
		if src, err := gmail.New(ctx, id, acct, nil, writer, lim, log, ts); err != nil {
			d.bad("", "%s: %v", id, err)
		} else if err := src.Check(ctx); err != nil {
			d.bad(hint, "%s check failed: %v", id, err)
		} else {
			d.ok("%s reachable and authorized for %s", id, acct.Account)
		}
	}
	if acct.Chat {
		id := state.InstanceID(state.SourceGChat, acct.Label)
		if svc, err := chat.NewService(ctx, option.WithTokenSource(ts)); err != nil {
			d.bad("", "%s: %v", id, err)
		} else {
			lim := ratelimit.NewKeyed(chatPerSpacePerSecond, chatPerSpaceBurst, chatOverallPerSecond, chatOverallBurst)
			// Check() only probes spaces.list; no Drive service needed.
			src := gchat.New(id, acct, nil, writer, lim, log, svc, nil)
			if err := src.Check(ctx); err != nil {
				d.bad("If this is a 403, a Workspace admin may be blocking this OAuth client\nfrom the Chat scopes — ask them to allowlist it.", "%s check failed: %v", id, err)
			} else {
				d.ok("%s reachable and authorized", id)
			}
		}
	}
}

// sharesClientFile returns the label of another [[google]] account pointing
// at the same OAuth client JSON, or "".
func sharesClientFile(cfg *config.Config, acct config.GoogleAccount) string {
	for _, other := range cfg.Google {
		if other.Label != acct.Label && other.ClientFilePath == acct.ClientFilePath {
			return other.Label
		}
	}
	return ""
}

// checkFastMailAccount reports one [[fastmail]] block's token and live Check.
func (d *doctorReport) checkFastMailAccount(acct config.FastMailAccount, writer *archive.Writer, log *slog.Logger) {
	id := state.InstanceID(state.SourceFastmail, acct.Label)
	d.section("[[fastmail]] %q — %s", acct.Label, acct.Account)

	switch {
	case acct.Token != "":
		d.ok("token supplied via $SAVE_FASTMAIL_TOKEN_%s", config.EnvSuffix(acct.Label))
	default:
		ok, detail := credFileState(acct.TokenFilePath)
		if !ok {
			hint := fastmailSetup(acct)
			if detail != "missing" {
				hint = fmt.Sprintf("Run: chmod 600 %s", acct.TokenFilePath)
			}
			d.bad(hint, "token file %s: %s", acct.TokenFilePath, detail)
			return
		}
		d.ok("token file %s (%s)", acct.TokenFilePath, detail)
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	tokens := fastmail.FileTokenSource(acct.Token, acct.TokenFilePath)
	src := fastmail.New(id, acct, nil, writer, nil, log, tokens)
	if err := src.Check(ctx); err != nil {
		d.bad(fastmailSetup(acct), "%s check failed: %v", id, err)
		return
	}
	d.ok("%s reachable and authorized for %s", id, acct.Account)
}

func indent(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}
