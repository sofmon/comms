package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	chat "google.golang.org/api/chat/v1"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"golang.org/x/oauth2"

	"comms/internal/archive"
	"comms/internal/config"
	"comms/internal/googleauth"
	"comms/internal/lock"
	"comms/internal/paths"
	"comms/internal/policy"
	"comms/internal/ratelimit"
	gchatsender "comms/internal/sender/gchat"
	gmailsender "comms/internal/sender/gmail"
	"comms/internal/source"
	"comms/internal/source/fastmail"
	"comms/internal/source/gchat"
	"comms/internal/source/gmail"
	"comms/internal/state"
)

// Limiter and housekeeping parameters chosen per the plan; the Gmail
// quota-unit numbers come from the connector itself.
//
// Which buckets are shared between accounts follows the shape of the
// upstream quota:
//   - Gmail's 6,000 units/min budget is metered per USER, so every Gmail
//     instance gets its own Units bucket. The competing per-project ceiling
//     (1.2M units/min ≈ 20,000 units/s) is three orders of magnitude above
//     UnitsPerSecond × a handful of accounts, so no shared ceiling is
//     needed to stay under it.
//   - Chat's per-space read cap is metered per SPACE regardless of who
//     reads it, so all Chat instances share ONE Keyed limiter: its key is
//     the space resource name, which makes two accounts sitting in the same
//     space queue behind one bucket. The overall ceiling in that same
//     limiter is therefore shared too — deliberately conservative, and
//     cheap at Chat's 2-minute cadence and few requests per pass.
//   - FastMail accounts are separate users on separate sessions, so each
//     gets its own bucket.
const (
	chatPerSpacePerSecond = 10 // documented cap is 15/s per space, shared
	chatPerSpaceBurst     = 10
	chatOverallPerSecond  = 25
	chatOverallBurst      = 25
	fastmailPerSecond     = 5 // one unit per HTTP round trip
	fastmailBurst         = 5
	keepRunsPerSource     = 500
)

// driveReadonlyScope is added to the Google grant when mirror_drive_files
// is enabled (the Chat connector then fetches DRIVE_FILE attachments).
const driveReadonlyScope = "https://www.googleapis.com/auth/drive.readonly"

// logLevel is shared by every command's logger; --verbose lowers it.
var logLevel = new(slog.LevelVar)

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
}

// stateDBPath is the canonical state database location.
func stateDBPath() string {
	return filepath.Join(paths.StateDir(), "state.db")
}

// app is the shared wiring for the commands that mutate the archive (sync,
// run): validated config, the exclusive instance lock, the open state DB,
// and the archive writer bound to the pinned timezone.
type app struct {
	cfg     *config.Config
	cfgPath string
	db      *state.DB
	writer  *archive.Writer
	tzName  string // pinned IANA zone name

	// policy is THE attachment storage policy for this process, built once
	// from the [attachments] block through config.Config.Policy so every
	// caller shares one PolicyDigest. Connectors must be handed this exact
	// pointer, never their own policy.New.
	policy *policy.Policy

	log         *slog.Logger
	stateDBPath string
	release     func() // instance lock; idempotent
}

// openApp loads the config, takes the instance lock, opens the state DB,
// and runs the timezone pinning protocol: the zone resolved from the config
// must match meta.archive_tz exactly — a mismatch would bucket new messages
// into different day folders than the existing archive, so it refuses to
// start. On first run the resolved zone is recorded in meta AND written
// back into config.toml (the pin must survive state-DB loss).
func openApp() (*app, error) {
	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	// Built before the lock: an invalid [attachments] block is a config
	// error, and there is no point holding the instance lock to report one.
	// config.Load already validated it, so this only fails on a code bug.
	pol, err := cfg.Policy()
	if err != nil {
		return nil, err
	}
	stateDir := paths.StateDir()
	if err := paths.EnsureDir(stateDir); err != nil {
		return nil, err
	}
	release, err := lock.Acquire(stateDir)
	if err != nil {
		return nil, err
	}
	a := &app{
		cfg:         cfg,
		cfgPath:     cfgPath,
		policy:      pol,
		log:         logger(),
		stateDBPath: filepath.Join(stateDir, "state.db"),
		release:     release,
	}
	ok := false
	defer func() {
		if !ok {
			a.Close()
		}
	}()

	db, err := state.Open(a.stateDBPath)
	if err != nil {
		return nil, err
	}
	a.db = db

	loc, zone, err := config.ResolveTimezone(cfg.Timezone)
	if err != nil {
		return nil, err
	}
	pinned, has, err := db.GetMeta(state.MetaArchiveTZ)
	if err != nil {
		return nil, err
	}
	switch {
	case has && pinned != zone:
		return nil, fmt.Errorf(
			"refusing to start: the state database has the archive timezone pinned to %q but the config resolves to %q — "+
				"new messages would land in different day folders than the existing archive. "+
				"Set timezone = %q in %s to continue with the existing archive, or delete %s to re-pin from scratch",
			pinned, zone, pinned, cfgPath, a.stateDBPath)
	case !has:
		if err := db.SetMeta(state.MetaArchiveTZ, zone); err != nil {
			return nil, err
		}
		if cfg.Timezone == "" || cfg.Timezone == "local" {
			if err := config.PinTimezone(cfgPath, zone); err != nil {
				return nil, fmt.Errorf("timezone resolved to %q but could not be pinned into the config: %w", zone, err)
			}
		}
		a.log.Info("archive timezone pinned", "zone", zone)
	}

	// Record the archive root; a change is legal (the tree moved) but worth
	// surfacing, since rel paths only make sense inside one root.
	if root, hasRoot, err := db.GetMeta(state.MetaArchiveRoot); err != nil {
		return nil, err
	} else if hasRoot && root != cfg.ArchiveRoot {
		a.log.Warn("archive_root changed since the last run; the archive tree must have been moved along with it",
			"was", root, "now", cfg.ArchiveRoot)
	}
	if err := db.SetMeta(state.MetaArchiveRoot, cfg.ArchiveRoot); err != nil {
		return nil, err
	}
	// The spam tree is recorded the same way: spam-filed rel paths resolve
	// only inside it, so a change means that tree moved too.
	if root, hasRoot, err := db.GetMeta(state.MetaSpamRoot); err != nil {
		return nil, err
	} else if hasRoot && root != cfg.SpamRoot {
		a.log.Warn("spam_root changed since the last run; the spam tree must have been moved along with it",
			"was", root, "now", cfg.SpamRoot)
	}
	if err := db.SetMeta(state.MetaSpamRoot, cfg.SpamRoot); err != nil {
		return nil, err
	}

	if err := a.checkPolicyDigest(); err != nil {
		return nil, err
	}

	a.tzName = zone
	a.writer = &archive.Writer{
		Root:       cfg.ArchiveRoot,
		SpamRoot:   cfg.SpamRoot,
		TZ:         loc,
		Quarantine: archive.QuarantineFor(cfg.Attachments.Quarantine),
	}
	ok = true
	return a, nil
}

// checkPolicyDigest runs the startup half of the retro-fetch protocol, and
// deliberately does nothing else.
//
// On a fresh archive it records the digest now in force. When the recorded
// digest differs it says so, loudly, and STOPS THERE: a changed policy is
// never acted on by a sync or by the daemon, because acting on it would mean
// a typo in [attachments] could quietly pull gigabytes onto a volume that is
// already nearly full. `comms status` reports how much of the backlog the new
// policy would accept; `comms refetch` is the only thing that fetches it, and
// the only thing that advances the recorded digest.
func (a *app) checkPolicyDigest() error {
	digest := a.policy.PolicyDigest()
	recorded, has, err := a.db.AttachmentPolicyDigest()
	if err != nil {
		return err
	}
	switch {
	case !has:
		return a.db.SetAttachmentPolicyDigest(digest)
	case recorded != digest:
		a.log.Warn("the attachment policy has changed since this archive was last reconsidered; "+
			"nothing is re-fetched automatically — run `comms refetch --dry-run` to see what it would now accept",
			"recorded", recorded, "now", digest)
	}
	return nil
}

// Close releases the instance lock and the state DB. Safe to call more than
// once and on a partially constructed app.
func (a *app) Close() {
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
	if a.release != nil {
		a.release()
	}
}

// heal is the startup healing pass shared by sync and run: sweep renameio
// temp litter, then render any chat day left dirty by a crash between row
// commit and render.
func (a *app) heal(ctx context.Context) error {
	removed, err := a.writer.SweepTemp()
	if err != nil {
		return err
	}
	if len(removed) > 0 {
		a.log.Info("startup healing: removed stray temp files", "count", len(removed))
	}
	return a.healChatDays(ctx)
}

// googleScopes derives the OAuth scope set ONE [[google]] account needs.
// Outbound capabilities add their write scope plus the narrow read scope used
// to reconcile an interrupted delivery. A single consent covers both archive
// and send capabilities, so this is the scope set of its one token file.
func googleScopes(acct config.GoogleAccount) []string {
	var scopes []string
	add := func(scope string) {
		if !slices.Contains(scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if acct.Gmail || acct.SendEmail {
		add(gmail.Scope)
	}
	if acct.SendEmail {
		add(gmailsender.Scope)
	}
	if acct.Chat {
		add(chat.ChatSpacesReadonlyScope)
		add(chat.ChatMessagesReadonlyScope)
		add(chat.ChatMembershipsReadonlyScope)
		// Drive mirroring is a Chat-only feature: the connector fetches
		// DRIVE_FILE attachments it finds in messages.
		if acct.MirrorDriveFiles {
			add(driveReadonlyScope)
		}
	}
	if acct.SendChat {
		add(chat.ChatMessagesReadonlyScope)
		add(gchatsender.Scope)
	}
	return scopes
}

// boundSource is a constructed connector together with the config instance
// it serves. Commands need both: the connector to run, and the instance to
// name the account in output and to pick the kind's daemon interval.
type boundSource struct {
	inst config.Instance
	conn source.Source
}

// sourceKinds is the canonical order source kinds run in; it is also the
// order config.Instances reports instances in.
var sourceKinds = []string{state.SourceGmail, state.SourceGChat, state.SourceFastmail}

// buildSources constructs one connector per selected instance (see
// resolveInstances for what a selector may be; no selectors means every
// configured instance). Credentials are verified per account before its
// connectors are built, so a config change that grew the scope set fails
// with a clear, account-specific re-auth instruction instead of runtime
// 403s.
func (a *app) buildSources(ctx context.Context, only []string) ([]boundSource, error) {
	insts, err := resolveInstances(a.cfg, only)
	if err != nil {
		return nil, err
	}

	// One token source per Google LABEL. An account's Gmail and Chat
	// instances share a single grant, and the source persists refreshed
	// tokens back to the file — two sources over one file would race.
	googleTS := make(map[string]oauth2.TokenSource, len(a.cfg.Google))
	tokenSourceFor := func(acct config.GoogleAccount) (oauth2.TokenSource, error) {
		if ts, ok := googleTS[acct.Label]; ok {
			return ts, nil
		}
		ts, err := googleTokenSource(ctx, acct)
		if err != nil {
			return nil, err
		}
		googleTS[acct.Label] = ts
		return ts, nil
	}
	// Shared across every Chat instance on purpose — see the limiter notes
	// on the constants above.
	var chatLimiter *ratelimit.Keyed

	out := make([]boundSource, 0, len(insts))
	for _, in := range insts {
		switch in.Kind {
		case state.SourceGmail:
			acct, ok := a.cfg.GoogleByLabel(in.Label)
			if !ok {
				return nil, fmt.Errorf("%s: no [[google]] block labelled %q", in.ID, in.Label)
			}
			ts, err := tokenSourceFor(acct)
			if err != nil {
				return nil, err
			}
			lim := ratelimit.NewUnits(gmail.UnitsPerSecond, gmail.UnitsBurst)
			g, err := gmail.New(ctx, in.ID, acct, a.db, a.writer, lim, a.log, ts, gmail.WithPolicy(a.policy))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", in.ID, err)
			}
			out = append(out, boundSource{inst: in, conn: g})

		case state.SourceGChat:
			acct, ok := a.cfg.GoogleByLabel(in.Label)
			if !ok {
				return nil, fmt.Errorf("%s: no [[google]] block labelled %q", in.ID, in.Label)
			}
			ts, err := tokenSourceFor(acct)
			if err != nil {
				return nil, err
			}
			svc, err := chat.NewService(ctx, option.WithTokenSource(ts))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", in.ID, err)
			}
			var driveSvc *drive.Service
			if acct.MirrorDriveFiles {
				driveSvc, err = drive.NewService(ctx, option.WithTokenSource(ts))
				if err != nil {
					return nil, fmt.Errorf("%s: drive: %w", in.ID, err)
				}
			}
			if chatLimiter == nil {
				chatLimiter = ratelimit.NewKeyed(chatPerSpacePerSecond, chatPerSpaceBurst, chatOverallPerSecond, chatOverallBurst)
			}
			out = append(out, boundSource{inst: in, conn: gchat.New(in.ID, acct, a.db, a.writer, chatLimiter, a.log, svc, driveSvc, gchat.WithPolicy(a.policy))})

		case state.SourceFastmail:
			acct, ok := a.cfg.FastMailByLabel(in.Label)
			if !ok {
				return nil, fmt.Errorf("%s: no [[fastmail]] block labelled %q", in.ID, in.Label)
			}
			if acct.Token == "" {
				if err := paths.CheckCredentialPerms(acct.TokenFilePath); err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						return nil, fmt.Errorf("%s (%s): no FastMail token at %s — run `comms auth fastmail %s`",
							in.ID, acct.Account, acct.TokenFilePath, acct.Label)
					}
					return nil, err
				}
			}
			lim := ratelimit.NewUnits(fastmailPerSecond, fastmailBurst)
			tokens := fastmail.FileTokenSource(acct.Token, acct.TokenFilePath)
			out = append(out, boundSource{inst: in, conn: fastmail.New(in.ID, acct, a.db, a.writer, lim, a.log, tokens, fastmail.WithPolicy(a.policy))})

		default:
			return nil, fmt.Errorf("unknown source kind %q in instance %q", in.Kind, in.ID)
		}
	}
	return out, nil
}

// resolveInstances maps `--source` selectors onto configured instances. A
// selector is a source KIND ("gmail": every Gmail instance), an INSTANCE ID
// ("gmail:work": exactly that one), or an account LABEL ("work": every
// instance of that account). No selectors means every configured instance.
// The result keeps config.Instances' canonical order and is deduplicated,
// so overlapping selectors cannot run one instance twice.
func resolveInstances(cfg *config.Config, selectors []string) ([]config.Instance, error) {
	all := cfg.Instances()
	if len(selectors) == 0 {
		return all, nil
	}
	want := make(map[string]bool, len(all))
	for _, sel := range selectors {
		ids, err := matchSelector(cfg, all, sel)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			want[id] = true
		}
	}
	var out []config.Instance
	for _, in := range all {
		if want[in.ID] {
			out = append(out, in)
		}
	}
	return out, nil
}

// matchSelector resolves one selector to instance ids. An instance id wins
// outright (it contains a colon, which no kind or label can); otherwise a
// value that is both a configured kind and a configured label is ambiguous
// and rejected rather than guessed.
func matchSelector(cfg *config.Config, all []config.Instance, sel string) ([]string, error) {
	var byID, byKind, byLabel []string
	for _, in := range all {
		switch {
		case in.ID == sel:
			byID = append(byID, in.ID)
		case in.Kind == sel:
			byKind = append(byKind, in.ID)
		case in.Label == sel:
			byLabel = append(byLabel, in.ID)
		}
	}
	switch {
	case len(byID) > 0:
		return byID, nil
	case len(byKind) > 0 && len(byLabel) > 0:
		return nil, fmt.Errorf("--source %q is ambiguous: it is both a source kind (%s) and an account label (%s) — select with explicit instance ids instead",
			sel, strings.Join(byKind, ", "), strings.Join(byLabel, ", "))
	case len(byKind) > 0:
		return byKind, nil
	case len(byLabel) > 0:
		return byLabel, nil
	}
	// Nothing matched: a real source kind with no account configured for it
	// deserves a more specific complaint than "unknown".
	for _, kind := range sourceKinds {
		if sel != kind {
			continue
		}
		return nil, fmt.Errorf("--source %q: no account is configured for that source kind in %s (%s)",
			sel, config.DefaultPath(), selectorHelp(cfg))
	}
	return nil, fmt.Errorf("unknown --source %q (%s)", sel, selectorHelp(cfg))
}

// selectorHelp lists every accepted --source value for the current config.
func selectorHelp(cfg *config.Config) string {
	all := cfg.Instances()
	var kinds, labels, ids []string
	seenKind, seenLabel := map[string]bool{}, map[string]bool{}
	for _, in := range all {
		if !seenKind[in.Kind] {
			seenKind[in.Kind] = true
			kinds = append(kinds, in.Kind)
		}
		if !seenLabel[in.Label] {
			seenLabel[in.Label] = true
			labels = append(labels, in.Label)
		}
		ids = append(ids, in.ID)
	}
	return fmt.Sprintf("valid: kind %s, label %s, or instance id %s",
		strings.Join(kinds, "|"), strings.Join(labels, "|"), strings.Join(ids, "|"))
}

// googleTokenSource checks one account's credential files and scope drift,
// then returns the persisted token source both of that account's instances
// share.
func googleTokenSource(ctx context.Context, acct config.GoogleAccount) (oauth2.TokenSource, error) {
	if err := checkGoogleCredentials(acct); err != nil {
		return nil, err
	}
	return googleauth.TokenSource(ctx, acct.ClientFilePath, acct.TokenFilePath, googleScopes(acct))
}

// checkGoogleCredentials verifies that one account's OAuth client and token
// files exist, are private, and that the stored grant still covers the
// scopes the config now needs. Every message names the account: with
// several [[google]] blocks a bare "run `comms auth google`" would not say
// which one is broken.
func checkGoogleCredentials(acct config.GoogleAccount) error {
	if err := paths.CheckCredentialPerms(acct.ClientFilePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("account %q (%s): no Google OAuth client file at %s — download a Desktop-app client JSON from the Google Cloud Console (run `comms doctor` for the walkthrough)",
				acct.Label, acct.Account, acct.ClientFilePath)
		}
		return fmt.Errorf("account %q (%s): %w", acct.Label, acct.Account, err)
	}
	scopes := googleScopes(acct)
	missing, err := googleauth.ScopeDrift(acct.TokenFilePath, scopes)
	switch {
	case errors.Is(err, googleauth.ErrNeedsAuth):
		return fmt.Errorf("account %q (%s): %w — run `comms auth google %s`", acct.Label, acct.Account, err, acct.Label)
	case err != nil:
		return fmt.Errorf("account %q (%s): %w", acct.Label, acct.Account, err)
	case len(missing) > 0:
		return fmt.Errorf("account %q (%s): the stored Google token %s is missing scopes the config now needs (%v) — re-run `comms auth google %s`",
			acct.Label, acct.Account, acct.TokenFilePath, missing, acct.Label)
	}
	if err := paths.CheckCredentialPerms(acct.TokenFilePath); err != nil {
		return fmt.Errorf("account %q (%s): %w", acct.Label, acct.Account, err)
	}
	return nil
}

// runSourcePass is THE single sync pass, shared verbatim by `comms sync` and
// every daemon loop: record the run, run the connector's Sync, render any
// dirty chat day files, finish the run, prune old run rows.
func (a *app) runSourcePass(ctx context.Context, src source.Source) error {
	runID, err := a.db.StartRun(src.Name(), "sync")
	if err != nil {
		return err
	}
	syncErr := src.Sync(ctx)
	// Only chat dirties day files, and only its own instance's: another
	// account's dirty day belongs to that account's pass (startup healing
	// covers instances that are not syncing this run).
	var renderErr error
	if state.KindOf(src.Name()) == state.SourceGChat {
		renderErr = a.renderDirtyChatDays(ctx, src.Name())
	}
	passErr := errors.Join(syncErr, renderErr)
	msg := ""
	if passErr != nil {
		msg = passErr.Error()
	}
	if err := a.db.FinishRun(runID, passErr == nil, state.RunStats{}, msg); err != nil {
		passErr = errors.Join(passErr, err)
	}
	if err := a.db.PruneRuns(keepRunsPerSource); err != nil {
		passErr = errors.Join(passErr, err)
	}
	// Noise triage after a clean pass, when asked for. It is its own run
	// row and never the sync's failure: the mail is archived and the
	// cursors have advanced whatever triage makes of it.
	if passErr == nil && a.cfg.Triage.AfterSync && state.KindOf(src.Name()) != state.SourceGChat {
		a.triageAfterSync(ctx, src.Name())
	}
	return passErr
}

// triageAfterSync runs the rules-only layers over one mail instance's
// unsettled notes — the ones this pass just archived, plus any an earlier
// pass left — as `[triage] after_sync = true` asks. The model layer joins
// only with `[triage.llm] in_daemon = true`, so a stopped endpoint can
// never stall the archiver. Failures are logged and recorded on the run
// row; nothing here can fail the sync.
func (a *app) triageAfterSync(ctx context.Context, instanceID string) {
	runID, err := a.db.StartRun(instanceID, "triage")
	if err != nil {
		a.log.Error("triage after sync: could not record the run", "source", instanceID, "err", err)
		return
	}
	tally, err := a.triagePass(ctx, instanceID)
	msg := ""
	if err != nil {
		msg = err.Error()
		a.log.Error("triage after sync failed; the notes stay where they are and are looked at next pass", "source", instanceID, "err", err)
	} else {
		a.log.Info("triage after sync", "source", instanceID,
			"to_spam", tally.MovedToSpam, "back", tally.MovedBack, "settled", tally.Settled,
			"reconciled", tally.Reconciled, "not_read", tally.NotRead, "failed", tally.Failed)
		if tally.Failed > 0 {
			msg = fmt.Sprintf("%d note(s) could not be triaged", tally.Failed)
		}
	}
	if ferr := a.db.FinishRun(runID, err == nil && tally.Failed == 0, state.RunStats{}, msg); ferr != nil {
		a.log.Error("triage after sync: could not finish the run row", "source", instanceID, "err", ferr)
	}
}

// triagePass builds and applies a plan for one instance, rules-only unless
// the model is allowed in the daemon. The rules file is re-read every pass,
// so an edit takes effect without a restart.
func (a *app) triagePass(ctx context.Context, instanceID string) (triageTally, error) {
	judge, err := llmJudge(a.cfg, true)
	if err != nil {
		return triageTally{}, err
	}
	cls, err := newClassifier(a.cfg, judge)
	if err != nil {
		return triageTally{}, err
	}
	r := &triageRun{cfg: a.cfg, db: a.db, writer: a.writer, cls: cls, log: a.log}
	plan, err := r.buildTriagePlan(ctx, triageOpts{only: []string{instanceID}})
	if err != nil {
		return triageTally{}, err
	}
	return r.applyTriagePlan(ctx, plan), nil
}

// healChatDays renders every dirty chat day of EVERY instance. Startup
// healing cannot be scoped to the instances syncing this run: a crash
// leaves whichever account was mid-write dirty, and `comms sync --source X`
// would otherwise never repair account Y.
func (a *app) healChatDays(ctx context.Context) error {
	files, err := a.db.AllDirtyDayFiles()
	if err != nil {
		return err
	}
	return a.renderDayFiles(ctx, files)
}

// renderDirtyChatDays regenerates one instance's dirty chat day files.
func (a *app) renderDirtyChatDays(ctx context.Context, source string) error {
	files, err := a.db.DirtyDayFiles(source)
	if err != nil {
		return err
	}
	return a.renderDayFiles(ctx, files)
}

// renderDayFiles rewrites each day file as a pure projection of the
// canonical rows: whole-file render, atomic replace, then the ledger row is
// marked clean. A failed day is logged and left dirty for the next run.
func (a *app) renderDayFiles(ctx context.Context, files []state.DayFile) error {
	// The member cache is keyed by instance AND space: two accounts see
	// their own membership rows for the very same shared space.
	memberCache := make(map[string]map[string]string)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.renderChatDay(f, memberCache); err != nil {
			a.log.Warn("chat day render failed; left dirty for the next run",
				"source", f.Source, "space", f.Space, "day", f.DayBucket, "err", err)
		}
	}
	return nil
}

func (a *app) renderChatDay(f state.DayFile, memberCache map[string]map[string]string) error {
	sp, ok, err := a.db.GetSpace(f.Source, f.Space)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("space %q not registered for %s", f.Space, f.Source)
	}
	msgs, err := a.db.MessagesForDay(f.Source, f.Space, f.DayBucket)
	if err != nil {
		return err
	}
	atts := make(map[string][]state.Attachment)
	for _, m := range msgs {
		rows, aerr := a.db.AttachmentsForMessage(f.Source, m.Name)
		if aerr != nil {
			return aerr
		}
		if len(rows) > 0 {
			atts[m.Name] = rows
		}
	}
	cacheKey := f.Source + "\x00" + f.Space
	cache := memberCache[cacheKey]
	if cache == nil {
		cache = make(map[string]string)
		memberCache[cacheKey] = cache
	}
	memberName := func(userID string) string {
		if name, hit := cache[userID]; hit {
			return name
		}
		name, _, merr := a.db.GetMember(f.Source, f.Space, userID)
		if merr != nil {
			a.log.Warn("member lookup failed", "source", f.Source, "space", f.Space, "user", userID, "err", merr)
			name = ""
		}
		cache[userID] = name
		return name
	}
	// threadStart feeds the "(continued)" header for threads that began on
	// an earlier day; a failed lookup degrades to the plain header (zero
	// time means unknown) rather than aborting the render.
	threadStarts := make(map[string]time.Time)
	threadStart := func(thread string) time.Time {
		if ts, hit := threadStarts[thread]; hit {
			return ts
		}
		ts, _, terr := a.db.ThreadFirstCreateTime(f.Source, f.Space, thread)
		if terr != nil {
			a.log.Warn("thread start lookup failed", "source", f.Source, "space", f.Space, "thread", thread, "err", terr)
			ts = time.Time{}
		}
		threadStarts[thread] = ts
		return ts
	}
	content := archive.RenderChatDay(sp, f.DayBucket, msgs, atts, memberName, threadStart, a.writer.TZ)
	hash, err := a.writer.WriteChatDay(f.RelPath, content)
	if err != nil {
		return err
	}
	return a.db.MarkDayRendered(f.Source, f.Space, f.DayBucket, f.DirtySeq, hash)
}

// clearCursors deletes every stored cursor of one INSTANCE (the --full
// switch); for a Gmail instance the persistent backfill queue is cleared
// too, since its 'done' rows would mask ids a fresh enumeration must
// reconsider. Other accounts' cursors are untouched.
func (a *app) clearCursors(instanceID string) error {
	ro, err := openRO(a.stateDBPath)
	if err != nil {
		return err
	}
	rows, err := listCursorsRO(ro, instanceID)
	ro.Close()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := a.db.DeleteCursor(r.Source, r.Scope, r.Kind); err != nil {
			return err
		}
	}
	if state.KindOf(instanceID) == state.SourceGmail {
		if err := a.db.ClearBackfill(instanceID); err != nil {
			return err
		}
	}
	a.log.Info("cursors cleared for full re-sync (dedup prevents duplicate files)",
		"source", instanceID, "cursors", len(rows))
	return nil
}

// clearFailures forgets the recorded per-item failures of the given
// instances (the --retry-failed switch) so previously skipped poison items
// are tried again.
func (a *app) clearFailures(instanceIDs []string) error {
	fails, err := a.db.ListFailures()
	if err != nil {
		return err
	}
	sel := make(map[string]bool, len(instanceIDs))
	for _, n := range instanceIDs {
		sel[n] = true
	}
	cleared := 0
	for _, f := range fails {
		if !sel[f.Source] {
			continue
		}
		if err := a.db.ClearFailure(f.Source, f.ID); err != nil {
			return err
		}
		cleared++
	}
	if cleared > 0 {
		a.log.Info("failure ledger cleared; previously skipped items will be retried", "items", cleared)
	}
	return nil
}
