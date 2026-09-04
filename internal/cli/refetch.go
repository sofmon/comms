package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"comms/internal/config"
	"comms/internal/policy"
	"comms/internal/state"
)

// --- the connector contract -------------------------------------------------
//
// `comms refetch` owns the SELECTION (which recorded skips the current policy
// would now accept), the LEDGER (which rows become resolved, and how), and
// the SAFETY RAILS (dry run, free space, digest bookkeeping). A connector
// owns exactly one thing the CLI cannot do: going back to the source for the
// bytes and re-rendering the message they belong to.
//
// The split matters because the CLI has no way to re-render an email note on
// its own — the note is a projection of the raw RFC822 message, which only
// the connector can fetch. Chat is the exception and is handled entirely on
// this side: its day files are already projections of canonical rows, so the
// connector just needs to land the blob and dirty the day, and refetch
// re-renders it through the ordinary renderDayFiles path.

// ErrRefetchUnsupported is what a connector returns from RefetchMessage when
// it cannot retro-fetch at all. It is not a failure of the run: refetch
// reports the instance, leaves every one of its skip rows unresolved, and
// moves on to the next account.
var ErrRefetchUnsupported = errors.New("this connector cannot retro-fetch skipped attachments yet")

// RefetchRequest is one message's worth of retro-fetch work: the parts of a
// single message whose recorded skips the current policy would now accept.
// Parts is never empty and every element belongs to StableID.
type RefetchRequest struct {
	StableID string

	// Parts are the ledger rows themselves, in the order refetch selected
	// them. Each carries the fetch identity (PartKey), the sanitized name the
	// original decision keyed on, and ContentSHA256 — see RefetchPart.Diverged
	// for what a connector must do with that hash.
	Parts []state.SkippedAttachment

	// Disposition is which tree the message's note lives in, read from its
	// messages row: a mail connector MUST pass it through as
	// archive.EmailMeta.Disposition when it re-renders, so a note noise triage
	// filed under spam_root is rewritten there rather than duplicated under
	// archive_root. Chat requests always carry the archive disposition.
	Disposition state.Disposition
}

// PartKeys is the requested parts' fetch identities, in request order.
func (r RefetchRequest) PartKeys() []string {
	out := make([]string, len(r.Parts))
	for i, p := range r.Parts {
		out[i] = p.PartKey
	}
	return out
}

// RefetchPart is what became of ONE requested part. A connector must return
// exactly one of these per requested part key (unless the whole message is
// gone, see RefetchResult.SourceGone): a part it silently omits is reported
// as unaccounted for and its ledger row is left unresolved, because a skip
// that quietly stops being mentioned is the silent drop this whole ledger
// exists to prevent.
type RefetchPart struct {
	PartKey string

	// Stored reports that the bytes passed the CURRENT policy over the REAL
	// content — policy.Policy.Decide, not PreCheck — and are on disk.
	Stored bool

	// RelPath is the archive-relative path of the stored file, for the log.
	RelPath string

	// Reason is the policy.Reason* that refused the part again, set exactly
	// when Stored is false and Diverged is false. Those rows are resolved
	// still_denied: re-deciding them on every run would be pure churn, and
	// re-recording the skip (the next ordinary sync of that message) re-opens
	// them anyway.
	Reason string

	// SHA256 is the hex sha256 of the bytes as fetched, "" when nothing was
	// fetched.
	SHA256 string

	// Diverged reports that the bytes came back with a different sha256 than
	// the skip row recorded. The connector MUST compare against
	// SkippedAttachment.ContentSHA256 whenever that field is non-empty and,
	// on a mismatch, MUST NOT write the file: the archive would silently gain
	// bytes that are not the ones the note describes. Report the divergence
	// here with the actual SHA256 instead; refetch records it in the failures
	// ledger and leaves the row unresolved for a human to look at.
	Diverged bool

	// Detail is a human-readable elaboration for the log; never parsed.
	Detail string
}

// RefetchResult is one message's outcome.
type RefetchResult struct {
	// SourceGone means the message (or the part) no longer exists upstream,
	// so the bytes are unrecoverable. Every requested part is resolved
	// source_gone and the note is left saying, honestly, that the attachment
	// was skipped.
	SourceGone bool

	// NoteRelPath is the note the connector re-rendered, relative to the
	// root its disposition selects (the request's Disposition). Mail
	// connectors must re-render through archive.Writer.RewriteEmail (which
	// refuses to CREATE a note) with that disposition, and update
	// messages.content_hash in the same commit; refetch cross-checks the path
	// against the one recorded on the skip row and warns when they disagree.
	// Chat connectors leave it empty and dirty the day instead — refetch
	// re-renders chat days itself.
	NoteRelPath string

	// Parts is one entry per requested part key.
	Parts []RefetchPart
}

// Refetcher is the optional connector capability `comms refetch` drives. A
// connector that does not implement it simply cannot be retro-fetched yet;
// its rows stay in the ledger and are reported, never resolved.
//
// Per source, the fetch is:
//   - Gmail: re-fetch format=raw by stable id.
//   - FastMail: re-resolve the stable id to a CURRENT blobId — JMAP blobIds
//     are not guaranteed durable, so the one from the original sync must not
//     be cached and reused.
//   - Chat: insert a pending row into the attachments ledger with the
//     recorded part key and let the existing download path drain it; the
//     dirty_seq protocol then flips "[unavailable]" into a link.
//
// The implementation must re-run the authoritative policy decision over the
// real bytes (policy.Policy.Decide) rather than trusting refetch's offline
// selection, which is deliberately generous: see reconsider.
type Refetcher interface {
	RefetchMessage(ctx context.Context, req RefetchRequest) (RefetchResult, error)
}

// --- the command ------------------------------------------------------------

func newRefetchCmd() *cobra.Command {
	var (
		only   []string
		dryRun bool
		reason string
	)
	c := &cobra.Command{
		Use:   "refetch",
		Short: "Re-fetch attachments the current attachment policy now accepts",
		Long: `Re-evaluate every unresolved skipped attachment under the CURRENT policy and
fetch the ones it now accepts.

This is always explicit. A widened policy is never acted on by a sync or by
the daemon, so a typo in [attachments] cannot silently pull gigabytes onto a
nearly full volume. Nothing is fetched until the plan — how many attachments,
how many bytes, from which account — has been printed.

Skips that the current policy still refuses are marked still_denied so they
stop being reconsidered every run; the next ordinary sync of the same message
re-opens them. Skips whose message has disappeared upstream are marked
source_gone and their notes keep saying, honestly, that the attachment was
never stored.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRefetch(cmd.OutOrStdout(), only, dryRun, reason)
		},
	}
	c.Flags().StringArrayVar(&only, "source", nil,
		"refetch only this source: a kind (gmail), an instance id (gmail:work), or an account label (work); repeatable")
	c.Flags().BoolVar(&dryRun, "dry-run", false,
		"print the plan and stop without fetching anything")
	c.Flags().StringVar(&reason, "reason", "",
		"consider only skips recorded with this reason (one of: "+strings.Join(policy.Reasons(), ", ")+")")
	return c
}

func runRefetch(out io.Writer, only []string, dryRun bool, reason string) error {
	if reason != "" && (!policy.ValidReason(reason) || reason == policy.ReasonNone) {
		return fmt.Errorf("unknown --reason %q (one of: %s)", reason, strings.Join(policy.Reasons(), ", "))
	}
	if dryRun {
		return dryRunRefetch(out, only, reason)
	}

	ctx, stop := signalContext()
	defer stop()

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.Close()

	// Stray temp files are swept, but dirty chat days are deliberately NOT
	// healed here: refetch renders exactly the days it touched, so an
	// unrelated crash-dirty day is left for the next sync rather than being
	// rewritten by a command the user asked to only fetch attachments.
	if removed, serr := a.writer.SweepTemp(); serr != nil {
		return serr
	} else if len(removed) > 0 {
		a.log.Info("removed stray temp files", "count", len(removed))
	}

	plan, err := buildRefetchPlan(a.cfg, a.db, a.policy, only, reason)
	if err != nil {
		return err
	}
	printRefetchPlan(out, plan)

	if plan.Candidates == 0 {
		return a.finishRefetch(out, plan, refetchTally{})
	}
	if err := a.checkRefetchSpace(plan.Bytes); err != nil {
		return err
	}

	srcs, err := a.buildSources(ctx, plan.SelectedIDs())
	if err != nil {
		return err
	}
	byID := make(map[string]boundSource, len(srcs))
	for _, s := range srcs {
		byID[s.inst.ID] = s
	}

	tally, err := a.executeRefetch(ctx, out, plan, byID)
	if err != nil {
		return err
	}
	return a.finishRefetch(out, plan, tally)
}

// dryRunRefetch prints the plan and stops. It deliberately does NOT open the
// app: taking no instance lock is what lets a dry run answer "what would this
// cost me?" beside a running daemon, the same way `comms status` does.
func dryRunRefetch(out io.Writer, only []string, reason string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	pol, err := cfg.Policy()
	if err != nil {
		return err
	}
	dbPath := stateDBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "no state database at %s yet — nothing has been refused, so there is nothing to refetch\n", dbPath)
		return nil
	}
	sdb, err := state.Open(dbPath)
	if err != nil {
		return err
	}
	defer sdb.Close()

	plan, err := buildRefetchPlan(cfg, sdb, pol, only, reason)
	if err != nil {
		return err
	}
	printRefetchPlan(out, plan)
	fmt.Fprintf(out, "\ndry run: nothing was fetched. Re-run without --dry-run to fetch.\n")
	return nil
}

// buildRefetchPlan resolves the --source selectors, reads the unresolved
// ledger, and re-decides every row offline.
func buildRefetchPlan(cfg *config.Config, sdb *state.DB, pol *policy.Policy, only []string, reason string) (refetchPlan, error) {
	insts, err := resolveInstances(cfg, only)
	if err != nil {
		return refetchPlan{}, err
	}
	if len(insts) == 0 {
		return refetchPlan{}, fmt.Errorf("no accounts configured in %s", config.DefaultPath())
	}
	rows, err := sdb.ListUnresolvedSkipped(0)
	if err != nil {
		return refetchPlan{}, err
	}
	recorded, _, err := sdb.AttachmentPolicyDigest()
	if err != nil {
		return refetchPlan{}, err
	}
	plan := planRefetch(pol, rows, cfg, insts, reason)
	plan.RecordedDigest = recorded
	plan.Scoped = len(only) > 0 || reason != ""
	return plan, nil
}

// --- selection --------------------------------------------------------------

// reconsider re-decides ONE recorded skip under the current policy using only
// the columns the ledger row carries: no network, no bytes, no archive reads.
//
// It is deliberately GENEROUS. The authoritative decision is
// policy.Policy.Decide over the real content, which the connector re-runs
// after fetching; this function only has to avoid two mistakes, and they are
// not symmetric. Wrongly selecting a row costs one download that comes back
// still_denied. Wrongly rejecting one silently keeps bytes out of the archive
// forever, which is exactly the failure the skip ledger exists to prevent. So
// anything it cannot settle offline is selected.
//
// What it can settle:
//   - the extension half, the flag-gated denials (macro Office, SVG,
//     containers) and the per-attachment size cap, all via policy.PreCheck on
//     the recorded sanitized name and decoded byte count;
//   - the content half in the one case where content is what refused the row
//     before — extension_content_mismatch — since the tolerated
//     non-mismatches are fixed in the policy engine and cannot be configured,
//     so such a row can only be rescued by the extension's permitted-type set
//     now naming the sniffed type, or by on_mismatch = "store-warn".
//
// The per-message, per-run and free-space budgets are left at zero on
// purpose: they describe a sync pass, not this row. Refetch enforces free
// space once for the whole plan instead (see checkRefetchSpace).
func reconsider(p *policy.Policy, row state.SkippedAttachment) (fetch bool, reason, detail string) {
	v := p.PreCheck(policy.Input{
		Name: row.SanitizedName,
		Size: row.SizeBytes,
		Chat: state.KindOf(row.Source) == state.SourceGChat,
	})
	if !v.Store {
		return false, v.Reason, v.Detail
	}

	if row.Reason == policy.ReasonExtensionContentMismatch &&
		p.Settings().OnMismatch != policy.OnMismatchStoreWarn {
		ext := policy.NormalizeExt(row.SanitizedName)
		if ext != "" && row.SniffedType != "" && !typePermitted(p, ext, row.SniffedType) {
			return false, policy.ReasonExtensionContentMismatch,
				"." + ext + " still does not permit the recorded sniffed type " + row.SniffedType
		}
	}
	return true, policy.ReasonNone, ""
}

// typePermitted reports whether the extension's effective permitted-type set
// names sniffed (or accepts anything).
func typePermitted(p *policy.Policy, ext, sniffed string) bool {
	types := p.PermittedTypes(ext)
	return slices.Contains(types, policy.AnyType) || slices.Contains(types, sniffed)
}

// refetchGroup is one message: the fetch unit, because a connector pays for
// the whole raw message either way and re-renders the note once.
type refetchGroup struct {
	Source   string
	StableID string
	Items    []state.SkippedAttachment // the selected parts, in ledger order
	Bytes    int64
}

// refetchPlan is what a run WOULD do, computed with no network at all.
type refetchPlan struct {
	Digest         string
	RecordedDigest string
	Scoped         bool // --source or --reason narrowed the ledger

	Considered int // unresolved rows in scope
	Candidates int // rows the current policy would now accept
	Bytes      int64
	Groups     []refetchGroup

	// StillDenied counts the in-scope rows the current policy still refuses,
	// per instance then per reason. They are not fetched and not resolved by
	// a dry run.
	StillDenied map[string]map[string]int

	// Unconfigured are instance ids the ledger holds rows for that the config
	// no longer declares; nothing can fetch them.
	Unconfigured map[string]int
}

// SelectedIDs is the instance ids the plan actually needs connectors for, in
// group order.
func (p refetchPlan) SelectedIDs() []string {
	var out []string
	seen := make(map[string]bool)
	for _, g := range p.Groups {
		if !seen[g.Source] {
			seen[g.Source] = true
			out = append(out, g.Source)
		}
	}
	return out
}

// planRefetch turns the unresolved ledger into a plan. rows are expected in
// state.DB.ListUnresolvedSkipped order — (source, first_seen_at, stable_id,
// part_key) — and that order is preserved, so one account's backlog stays
// contiguous and the oldest refusals are fetched first.
//
// cfg supplies every CONFIGURED instance and selected the ones this run
// covers. Both are needed and they are not the same question: an account the
// user simply did not name in --source is skipped quietly, while an account
// the config no longer declares at all is stranded state nothing will ever
// fetch, and gets reported.
func planRefetch(p *policy.Policy, rows []state.SkippedAttachment, cfg *config.Config, selectedInsts []config.Instance, reason string) refetchPlan {
	plan := refetchPlan{
		Digest:       p.PolicyDigest(),
		StillDenied:  make(map[string]map[string]int),
		Unconfigured: make(map[string]int),
	}
	selected := make(map[string]bool, len(selectedInsts))
	for _, in := range selectedInsts {
		selected[in.ID] = true
	}
	configured := make(map[string]bool)
	for _, in := range cfg.Instances() {
		configured[in.ID] = true
	}

	index := make(map[string]int) // source\x00stableID -> Groups index
	for _, row := range rows {
		if reason != "" && row.Reason != reason {
			continue
		}
		if !selected[row.Source] {
			if !configured[row.Source] {
				plan.Unconfigured[row.Source]++
			}
			continue
		}
		plan.Considered++

		fetch, why, _ := reconsider(p, row)
		if !fetch {
			byReason := plan.StillDenied[row.Source]
			if byReason == nil {
				byReason = make(map[string]int)
				plan.StillDenied[row.Source] = byReason
			}
			byReason[why]++
			continue
		}

		plan.Candidates++
		plan.Bytes += row.SizeBytes
		key := row.Source + "\x00" + row.StableID
		i, ok := index[key]
		if !ok {
			i = len(plan.Groups)
			index[key] = i
			plan.Groups = append(plan.Groups, refetchGroup{Source: row.Source, StableID: row.StableID})
		}
		plan.Groups[i].Items = append(plan.Groups[i].Items, row)
		plan.Groups[i].Bytes += row.SizeBytes
	}
	return plan
}

// --- reporting --------------------------------------------------------------

// printRefetchPlan states, before anything is fetched, exactly what would be.
func printRefetchPlan(out io.Writer, plan refetchPlan) {
	fmt.Fprintf(out, "attachment policy digest: %s\n", plan.Digest)
	switch {
	case plan.RecordedDigest == "":
		fmt.Fprintf(out, "  no digest recorded yet — the archive has not been reconsidered under any policy\n")
	case plan.RecordedDigest != plan.Digest:
		fmt.Fprintf(out, "  the archive was last reconsidered under %s — the policy has changed since\n", plan.RecordedDigest)
	default:
		fmt.Fprintf(out, "  unchanged since the last full reconsideration\n")
	}

	fmt.Fprintf(out, "\n%d unresolved skip(s) in scope\n", plan.Considered)
	if plan.Candidates == 0 {
		fmt.Fprintf(out, "  nothing to fetch: the current policy still refuses every one of them\n")
	} else {
		fmt.Fprintf(out, "  WOULD FETCH: %d attachment(s) across %d message(s), %s\n",
			plan.Candidates, len(plan.Groups), byteSize(plan.Bytes))
		type srcTotals struct {
			atts, msgs int
			bytes      int64
		}
		perSource := make(map[string]srcTotals)
		var order []string
		for _, g := range plan.Groups {
			s, seen := perSource[g.Source]
			if !seen {
				order = append(order, g.Source)
			}
			s.atts += len(g.Items)
			s.msgs++
			s.bytes += g.Bytes
			perSource[g.Source] = s
		}
		for _, src := range order {
			s := perSource[src]
			fmt.Fprintf(out, "    %-18s %d attachment(s) in %d message(s), %s\n", src, s.atts, s.msgs, byteSize(s.bytes))
		}
	}

	if len(plan.StillDenied) > 0 {
		fmt.Fprintf(out, "  still refused (left in the ledger, notes unchanged):\n")
		for _, src := range sortedKeys(plan.StillDenied) {
			byReason := plan.StillDenied[src]
			var parts []string
			for _, r := range policy.Reasons() {
				if n := byReason[r]; n > 0 {
					parts = append(parts, fmt.Sprintf("%s %d", r, n))
				}
			}
			fmt.Fprintf(out, "    %-18s %s\n", src, strings.Join(parts, ", "))
		}
	}
	if len(plan.Unconfigured) > 0 {
		fmt.Fprintf(out, "\nwarning: the ledger holds skips for instance(s) the config no longer declares:\n")
		for _, src := range sortedKeys(plan.Unconfigured) {
			fmt.Fprintf(out, "  %-18s %d skip(s) — nothing can fetch them until that label is configured again\n", src, plan.Unconfigured[src])
		}
	}
}

// refetchTally is the outcome of an executed plan.
type refetchTally struct {
	Fetched     int
	StillDenied int
	SourceGone  int
	Diverged    int
	Unaccounted int
	Unsupported []string // instance ids whose connector cannot retro-fetch
	Failed      int      // messages whose fetch errored
	Interrupted bool     // SIGINT/SIGTERM stopped the walk part way
	Bytes       int64
}

// clean reports a run in which every selected row reached a terminal
// disposition with nothing left to look at — the precondition for advancing
// the recorded policy digest, which is the claim "this whole backlog has been
// considered under this policy". An interrupted run has not considered the
// tail of the backlog and may not make that claim.
func (t refetchTally) clean() bool {
	return t.Diverged == 0 && t.Unaccounted == 0 && t.Failed == 0 &&
		len(t.Unsupported) == 0 && !t.Interrupted
}

// --- execution --------------------------------------------------------------

// executeRefetch drives the plan one message at a time. A message that fails
// is logged and left entirely unresolved: no partial resolution, so the next
// run reconsiders the whole message.
func (a *app) executeRefetch(ctx context.Context, out io.Writer, plan refetchPlan, byID map[string]boundSource) (refetchTally, error) {
	var tally refetchTally
	unsupported := make(map[string]bool)
	touchedChat := make(map[string]bool)

	for _, g := range plan.Groups {
		if err := ctx.Err(); err != nil {
			tally.Interrupted = true
			fmt.Fprintf(out, "\ninterrupted — %d attachment(s) fetched so far; re-run `comms refetch` to continue\n", tally.Fetched)
			break
		}
		if unsupported[g.Source] {
			continue
		}
		bound, ok := byID[g.Source]
		if !ok {
			// resolveInstances and buildSources agreed on the selection, so
			// this cannot happen; refuse to guess rather than skip silently.
			return tally, fmt.Errorf("internal: no connector built for %s", g.Source)
		}
		rf, ok := bound.conn.(Refetcher)
		if !ok {
			unsupported[g.Source] = true
			tally.Unsupported = append(tally.Unsupported, g.Source)
			a.log.Warn("connector cannot retro-fetch skipped attachments yet; its ledger rows are left unresolved",
				"source", g.Source)
			continue
		}

		disp, err := a.noteDisposition(g.Source, g.StableID)
		if err != nil {
			return tally, err
		}
		res, err := rf.RefetchMessage(ctx, RefetchRequest{StableID: g.StableID, Parts: g.Items, Disposition: disp})
		switch {
		case errors.Is(err, ErrRefetchUnsupported):
			unsupported[g.Source] = true
			tally.Unsupported = append(tally.Unsupported, g.Source)
			a.log.Warn("connector cannot retro-fetch skipped attachments yet; its ledger rows are left unresolved",
				"source", g.Source)
			continue
		case err != nil:
			tally.Failed++
			a.log.Error("refetch failed; the whole message is left unresolved for the next run",
				"source", g.Source, "stable_id", g.StableID, "err", err)
			continue
		}

		if state.KindOf(g.Source) == state.SourceGChat {
			touchedChat[g.Source] = true
		}
		if err := a.applyRefetchResult(g, res, &tally); err != nil {
			return tally, err
		}
	}

	// Chat day files are projections, so refetch re-renders them here rather
	// than asking the connector to: the blob landed and the day was dirtied,
	// and this is the same render path sync uses.
	for src := range touchedChat {
		if err := a.renderDirtyChatDays(ctx, src); err != nil {
			return tally, err
		}
	}
	return tally, nil
}

// noteDisposition looks up which tree a message's note lives in, so the
// connector re-renders the copy that exists. Chat has no messages row and is
// never triaged, so it is always the archive tree; a mail message with no
// row is one the skip ledger knows but the archive does not (never a normal
// state), and is reported rather than guessed at.
func (a *app) noteDisposition(source, stableID string) (state.Disposition, error) {
	if state.KindOf(source) == state.SourceGChat {
		return state.DispositionArchive, nil
	}
	m, ok, err := a.db.GetMessage(source, stableID)
	if err != nil {
		return "", err
	}
	if !ok {
		a.log.Warn("skip ledger names a message the archive has no row for; assuming the archive tree",
			"source", source, "stable_id", stableID)
		return state.DispositionArchive, nil
	}
	return m.Disposition, nil
}

// applyRefetchResult turns one connector outcome into ledger dispositions.
// Every requested part must come back accounted for: a part the connector did
// not mention is left unresolved and counted, because losing track of a skip
// is the one thing this ledger exists to make impossible.
func (a *app) applyRefetchResult(g refetchGroup, res RefetchResult, tally *refetchTally) error {
	if res.SourceGone {
		for _, it := range g.Items {
			if err := a.db.MarkSkippedResolved(g.Source, g.StableID, it.PartKey, state.SkipResolutionSourceGone); err != nil {
				return err
			}
			tally.SourceGone++
		}
		a.log.Info("message is gone upstream; its skipped attachments are unrecoverable and the note stays honest",
			"source", g.Source, "stable_id", g.StableID)
		return nil
	}

	if res.NoteRelPath != "" && len(g.Items) > 0 && res.NoteRelPath != g.Items[0].NoteRelPath {
		// Not fatal — a re-render may legitimately land elsewhere if the
		// subject changed — but the ledger row now points at a note that no
		// longer carries the entry, so say so.
		a.log.Warn("re-rendered note path differs from the one recorded on the skip row",
			"source", g.Source, "stable_id", g.StableID,
			"recorded", g.Items[0].NoteRelPath, "rendered", res.NoteRelPath)
	}

	seen := make(map[string]bool, len(res.Parts))
	for _, part := range res.Parts {
		seen[part.PartKey] = true
		switch {
		case part.Diverged:
			tally.Diverged++
			msg := fmt.Sprintf("refetched bytes for part %s do not match the recorded content sha256 %s (got %s): nothing was written",
				part.PartKey, orNone(partExpectedSHA(g, part.PartKey)), orNone(part.SHA256))
			a.log.Error("refetch divergence; the file was NOT overwritten and the skip stays unresolved",
				"source", g.Source, "stable_id", g.StableID, "part", part.PartKey, "detail", msg)
			if _, err := a.db.RecordFailure(g.Source, refetchFailureID(g.StableID, part.PartKey), msg); err != nil {
				return err
			}
		case part.Stored:
			if err := a.db.MarkSkippedResolved(g.Source, g.StableID, part.PartKey, state.SkipResolutionFetched); err != nil {
				return err
			}
			tally.Fetched++
			tally.Bytes += partSize(g, part.PartKey)
			a.log.Info("attachment retro-fetched", "source", g.Source, "stable_id", g.StableID,
				"part", part.PartKey, "path", part.RelPath)
		default:
			if err := a.db.MarkSkippedResolved(g.Source, g.StableID, part.PartKey, state.SkipResolutionStillDenied); err != nil {
				return err
			}
			tally.StillDenied++
			a.log.Info("attachment still refused by the current policy over its real bytes",
				"source", g.Source, "stable_id", g.StableID, "part", part.PartKey, "reason", part.Reason)
		}
	}
	for _, it := range g.Items {
		if !seen[it.PartKey] {
			tally.Unaccounted++
			a.log.Error("connector did not report on a requested part; its skip is left unresolved",
				"source", g.Source, "stable_id", g.StableID, "part", it.PartKey)
		}
	}
	return nil
}

func refetchFailureID(stableID, partKey string) string {
	return "refetch:" + stableID + "#" + partKey
}

func partExpectedSHA(g refetchGroup, partKey string) string {
	for _, it := range g.Items {
		if it.PartKey == partKey {
			return it.ContentSHA256
		}
	}
	return ""
}

func partSize(g refetchGroup, partKey string) int64 {
	for _, it := range g.Items {
		if it.PartKey == partKey {
			return it.SizeBytes
		}
	}
	return 0
}

// checkRefetchSpace refuses a plan that would eat into the configured
// free-space floor. The floor is normally enforced per attachment during a
// sync; refetch knows the whole plan up front, so it can say no before the
// first byte instead of half way through.
func (a *app) checkRefetchSpace(want int64) error {
	floor := a.cfg.Attachments.FreeSpaceFloor.Bytes()
	if floor <= 0 {
		return nil
	}
	free, err := freeBytes(a.cfg.ArchiveRoot)
	if err != nil {
		// Unknowable here (non-macOS, or an unreadable root): the per-write
		// check still applies during the fetch itself.
		a.log.Debug("free space could not be read; the per-attachment floor still applies", "err", err)
		return nil
	}
	if int64(free)-want < floor {
		return fmt.Errorf("refusing to refetch: fetching %s would leave %s free on %s, under attachments.free_space_floor (%s) — free some space, lower the floor, or narrow the run with --source/--reason",
			byteSize(want), byteSize(int64(free)-want), a.cfg.ArchiveRoot, byteSize(floor))
	}
	return nil
}

// finishRefetch prints the outcome and advances the recorded policy digest
// when — and only when — the whole backlog has been reconsidered under the
// current policy with nothing left unexplained. A --source or --reason run is
// partial by construction and never advances it, so `comms status` keeps
// nagging until the rest has been looked at.
func (a *app) finishRefetch(out io.Writer, plan refetchPlan, tally refetchTally) error {
	fmt.Fprintf(out, "\nfetched %d attachment(s), %s\n", tally.Fetched, byteSize(tally.Bytes))
	if tally.StillDenied > 0 {
		fmt.Fprintf(out, "still refused over their real bytes: %d (marked %s)\n", tally.StillDenied, state.SkipResolutionStillDenied)
	}
	if tally.SourceGone > 0 {
		fmt.Fprintf(out, "gone upstream: %d (marked %s; the notes stay honest)\n", tally.SourceGone, state.SkipResolutionSourceGone)
	}
	for _, src := range tally.Unsupported {
		fmt.Fprintf(out, "not attempted: %s cannot retro-fetch yet — its skips are unchanged\n", src)
	}
	if tally.Failed > 0 {
		fmt.Fprintf(out, "failed: %d message(s) — left unresolved, re-run `comms refetch`\n", tally.Failed)
	}
	if tally.Unaccounted > 0 {
		fmt.Fprintf(out, "unaccounted: %d requested part(s) were never reported on — left unresolved\n", tally.Unaccounted)
	}
	if tally.Diverged > 0 {
		fmt.Fprintf(out, "DIVERGED: %d attachment(s) came back with different bytes than recorded.\n"+
			"  Nothing was overwritten and nothing was resolved; see the failures ledger in `comms status`.\n", tally.Diverged)
	}

	switch {
	case plan.Scoped:
		fmt.Fprintf(out, "\nthis was a partial run (--source/--reason), so the recorded policy digest is unchanged\n")
	case tally.Interrupted:
		fmt.Fprintf(out, "\nthe recorded policy digest is unchanged: the run stopped before the end of the backlog\n")
	case !tally.clean():
		fmt.Fprintf(out, "\nthe recorded policy digest is unchanged: some rows still need attention\n")
	case plan.RecordedDigest == plan.Digest:
		// Already current; nothing to say.
	default:
		if err := a.db.SetAttachmentPolicyDigest(plan.Digest); err != nil {
			return err
		}
		fmt.Fprintf(out, "\nthe whole backlog has now been reconsidered under policy %s\n", plan.Digest)
	}

	if tally.Diverged > 0 {
		return fmt.Errorf("%d attachment(s) diverged from their recorded content hash", tally.Diverged)
	}
	if tally.Failed > 0 {
		return fmt.Errorf("%d message(s) could not be refetched", tally.Failed)
	}
	return nil
}

// --- small shared helpers ---------------------------------------------------

// byteSize renders a byte count the way the config writes one: decimal units,
// because that is what "50MB" means in [attachments].
func byteSize(n int64) string {
	if n < 0 {
		return fmt.Sprintf("%d B", n)
	}
	switch {
	case n < 1000:
		return fmt.Sprintf("%d B", n)
	case n < 1000*1000:
		return fmt.Sprintf("%.1f kB", float64(n)/1000)
	case n < 1000*1000*1000:
		return fmt.Sprintf("%.1f MB", float64(n)/(1000*1000))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1000*1000*1000))
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none recorded)"
	}
	return s
}

// sortedKeys returns a map's keys in sorted order, so every report is
// deterministic.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
