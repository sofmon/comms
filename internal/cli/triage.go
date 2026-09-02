package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"save/internal/archive"
	"save/internal/config"
	"save/internal/state"
	"save/internal/triage"
)

const triageLong = `Classify archived email notes as signal or noise and move the noise into
spam_root, at the same YYYY/MM/DD path each note had in the archive.

Sync is untouched: every message is archived first, exactly as before, and
triage is a separate pass over notes already on disk. Nothing is ever deleted
— a note filed as noise keeps its attachment folder and comes back with
` + "`save untriage`" + `.

Decisions run in layers and stop at the first decisive one: the protect list
(a stored PDF/Office attachment, your own outgoing mail, the protect_* globs
in config.toml), the bulk-mail headers captured at archive time, the [[keep]]
and [[noise]] rules in triage.toml, and — only if enabled — a local model,
which is consulted last and whose failures count as undecided, never noise.

A note already decided under the current rules is skipped; --reclassify
re-evaluates every note in scope. --dry-run prints every move, file by file,
and moves nothing. --explain shows what each layer made of one note.`

// triageOpts carries the command's flags.
type triageOpts struct {
	only       []string
	day, since string
	reclassify bool
	onlyLayer  string
	dryRun     bool
	explain    string
}

func newTriageCmd() *cobra.Command {
	var o triageOpts
	c := &cobra.Command{
		Use:   "triage",
		Short: "Move noise (newsletters, notifications, bulk mail) into spam_root",
		Long:  triageLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTriage(cmd.OutOrStdout(), o)
		},
	}
	c.Flags().StringArrayVar(&o.only, "source", nil,
		"triage only this source: a kind (gmail), an instance id (gmail:work), or an account label (work); repeatable")
	c.Flags().StringVar(&o.day, "day", "", "consider only notes archived on this day (YYYY-MM-DD, in the archive's pinned timezone)")
	c.Flags().StringVar(&o.since, "since", "", "consider only notes archived on or after this day (YYYY-MM-DD)")
	c.Flags().BoolVar(&o.reclassify, "reclassify", false, "re-evaluate notes already decided under the current rules")
	c.Flags().StringVar(&o.onlyLayer, "only", "", "with --reclassify: revisit only decisions made by \"rules\" (layers 0-2) or by the \"llm\"")
	c.Flags().BoolVar(&o.dryRun, "dry-run", false, "print the plan — every move, with its rule and reason — and move nothing")
	c.Flags().StringVar(&o.explain, "explain", "", "explain one note: which layers fired and why (path to the .md)")
	return c
}

func runTriage(out io.Writer, o triageOpts) error {
	if err := o.validate(); err != nil {
		return err
	}
	if o.explain != "" {
		return explainNote(out, o.explain)
	}
	if o.dryRun {
		return dryRunTriage(out, o)
	}

	ctx, stop := signalContext()
	defer stop()
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.Close()
	// Stray temp files are swept (a move creates none, but a re-render
	// through refetch might have); nothing else is healed here — this
	// command moves notes, and only notes.
	if removed, serr := a.writer.SweepTemp(); serr != nil {
		return serr
	} else if len(removed) > 0 {
		a.log.Info("removed stray temp files", "count", len(removed))
	}
	judge, err := llmJudge(a.cfg, false)
	if err != nil {
		return err
	}
	cls, err := newClassifier(a.cfg, judge)
	if err != nil {
		return err
	}
	r := &triageRun{cfg: a.cfg, db: a.db, writer: a.writer, cls: cls, log: a.log}
	plan, err := r.buildTriagePlan(ctx, o)
	if err != nil {
		return err
	}
	printTriagePlan(out, plan, false)
	tally := r.applyTriagePlan(ctx, plan)
	return finishTriage(out, tally)
}

// manualRule is the disposition_rule `save untriage` records. A note the
// operator moved back by hand is theirs: no later pass re-files it, whatever
// the rules say, until they run `save triage` on it again with --reclassify
// --only manual.
const manualRule = "manual"

func (o triageOpts) validate() error {
	for _, d := range []struct{ flag, v string }{{"day", o.day}, {"since", o.since}} {
		if d.v == "" {
			continue
		}
		if _, err := time.Parse("2006-01-02", d.v); err != nil {
			return fmt.Errorf("--%s %q: want YYYY-MM-DD", d.flag, d.v)
		}
	}
	if o.day != "" && o.since != "" {
		return errors.New("--day and --since are mutually exclusive")
	}
	switch o.onlyLayer {
	case "", "rules", "llm", "manual":
	default:
		return fmt.Errorf("--only %q: want \"rules\", \"llm\" or \"manual\"", o.onlyLayer)
	}
	if o.onlyLayer != "" && !o.reclassify {
		return errors.New("--only narrows a --reclassify; without --reclassify only undecided notes are looked at anyway")
	}
	if o.explain != "" && (o.dryRun || o.reclassify || o.day != "" || o.since != "" || len(o.only) > 0) {
		return errors.New("--explain takes one note and no other selector")
	}
	return nil
}

// llmJudge builds the model layer from [triage.llm] when it is enabled, or
// returns nil (the layer off). inDaemon says the caller is the after_sync
// hook, which may only use the model when in_daemon is set too — so a
// stopped endpoint can never stall the archiver.
func llmJudge(cfg *config.Config, inDaemon bool) (triage.Judge, error) {
	l := cfg.Triage.LLM
	if !l.Enabled || (inDaemon && !l.InDaemon) {
		return nil, nil
	}
	j, err := triage.NewLLMJudge(triage.LLMOptions{
		BaseURL: l.BaseURL, Model: l.Model, Timeout: l.Timeout.Duration(), MaxBodyChars: l.MaxBodyChars,
	})
	if err != nil {
		return nil, err
	}
	return j, nil
}

// newClassifier builds the pass's classifier from the config: the rules
// file, the protect list, the header switch and — when the caller supplies
// one — the model layer.
func newClassifier(cfg *config.Config, judge triage.Judge) (*triage.Classifier, error) {
	rules, err := triage.LoadRules(cfg.Triage.RulesFilePath)
	if err != nil {
		return nil, err
	}
	return triage.New(triage.Options{
		Rules:            rules,
		OwnAddresses:     cfg.OwnAddresses(),
		ProtectFrom:      cfg.Triage.ProtectFrom,
		ProtectSubject:   cfg.Triage.ProtectSubject,
		HeaderHeuristics: cfg.Triage.HeaderHeuristics,
		Judge:            judge,
	})
}

// triageRun is everything one pass reads with. dataless is the iCloud
// eviction probe, nil for the platform default.
type triageRun struct {
	cfg      *config.Config
	db       *state.DB
	writer   *archive.Writer
	cls      *triage.Classifier
	dataless func(string) (bool, error)
	log      *slog.Logger
}

func (r *triageRun) isDataless(p string) (bool, error) {
	if r.dataless != nil {
		return r.dataless(p)
	}
	return datalessFile(p)
}

// triageItem is one evaluated note.
type triageItem struct {
	Msg      state.Message
	Decision triage.Decision

	// Want is where the verdict says the note belongs. Noise means spam;
	// signal and undecided mean the archive — undecided is never noise, and
	// a note only ever sits in spam because a rule said so.
	Want state.Disposition

	// FoundIn is set when the note was not where the database says but was
	// in the other tree: a move a crash interrupted before the row was
	// updated. The plan reads the note from there; applying it first
	// reconciles the row (and the attachment directory) to the file's
	// location, then acts on the verdict.
	FoundIn state.Disposition

	// Problem is set when the note could not be evaluated: "evicted" (its
	// bytes are in iCloud and reading would download them), "missing",
	// "not-email", or "unreadable: ...". Such a note is neither moved nor
	// settled.
	Problem string
}

// where is the tree the note is actually in.
func (it triageItem) where() state.Disposition {
	if it.FoundIn != "" {
		return it.FoundIn
	}
	return it.Msg.Disposition
}

// Move reports whether applying the plan moves this note, and where.
func (it triageItem) Move() (to state.Disposition, ok bool) {
	if it.Problem != "" || it.Decision.Transient {
		return "", false
	}
	if it.Want != it.where() {
		return it.Want, true
	}
	return "", false
}

// triagePlan is what a pass WOULD do.
type triagePlan struct {
	Digest     string
	RulesPath  string
	Keep       int
	Noise      int
	Heuristics bool
	Model      string // "" when the model layer is off
	Scoped     bool

	Items []triageItem
	// Manual counts notes in scope that `save untriage` placed and which
	// the pass therefore leaves alone (unless --only manual).
	Manual int
	// Unconfigured are instance ids the ledger holds rows for that the
	// config no longer declares; nothing can read their notes.
	Unconfigured map[string]int
}

func (p *triagePlan) tally() (toSpam, toArchive, protected, kept, byModel, undecided, transient, evicted, missing, unreadable, reconcile int, perSource map[string]int) {
	perSource = map[string]int{}
	for _, it := range p.Items {
		if it.FoundIn != "" {
			reconcile++
		}
		switch {
		case it.Problem == "evicted":
			evicted++
			continue
		case it.Problem == "missing":
			missing++
			continue
		case it.Problem != "":
			unreadable++
			continue
		}
		if it.Decision.Transient {
			transient++
			continue
		}
		if to, ok := it.Move(); ok {
			if to == state.DispositionSpam {
				toSpam++
				perSource[it.Msg.Source]++
			} else {
				toArchive++
			}
		}
		switch it.Decision.Layer {
		case state.TriageLayerProtect:
			protected++
		case state.TriageLayerRules:
			if it.Decision.Verdict == state.TriageSignal {
				kept++
			}
		case state.TriageLayerLLM:
			if it.Decision.Verdict == state.TriageSignal {
				byModel++
			}
		case state.TriageLayerNone:
			undecided++
		}
	}
	return
}

// mailInstances resolves --source to the mail instances only: chat is never
// triaged, so a selector that yields only chat instances is an error rather
// than a silent no-op.
func mailInstances(cfg *config.Config, only []string) ([]config.Instance, error) {
	insts, err := resolveInstances(cfg, only)
	if err != nil {
		return nil, err
	}
	var mail []config.Instance
	for _, in := range insts {
		if in.Kind != state.SourceGChat {
			mail = append(mail, in)
		}
	}
	if len(mail) == 0 {
		if len(insts) > 0 {
			return nil, fmt.Errorf("--source %v selects only Google Chat, which is never triaged (chat day files stay in the archive)", only)
		}
		return nil, fmt.Errorf("no mail accounts configured in %s", config.DefaultPath())
	}
	return mail, nil
}

// buildTriagePlan reads every note in scope and classifies it. It moves
// nothing and writes nothing.
func (r *triageRun) buildTriagePlan(ctx context.Context, o triageOpts) (*triagePlan, error) {
	insts, err := mailInstances(r.cfg, o.only)
	if err != nil {
		return nil, err
	}
	rules := r.cls.Rules()
	plan := &triagePlan{
		Digest:       r.cls.Digest(),
		RulesPath:    rules.Path,
		Keep:         len(rules.Keep),
		Noise:        len(rules.Noise),
		Heuristics:   r.cfg.Triage.HeaderHeuristics,
		Scoped:       len(o.only) > 0 || o.day != "" || o.since != "",
		Unconfigured: map[string]int{},
	}
	if r.cls.HasJudge() {
		plan.Model = r.cfg.Triage.LLM.Model
	}
	sources := make([]string, len(insts))
	for i, in := range insts {
		sources[i] = in.ID
	}
	rows, err := r.db.MessagesForTriage(state.TriageScope{
		Sources: sources, Day: o.day, Since: o.since,
		Digest: plan.Digest, Reclassify: o.reclassify, Only: o.onlyLayer,
	})
	if err != nil {
		return nil, err
	}
	for _, m := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m.DispositionRule == manualRule && o.onlyLayer != "manual" {
			plan.Manual++
			continue
		}
		plan.Items = append(plan.Items, r.evaluate(ctx, m))
	}
	// Stranded rows are reported so a renamed label is not mistaken for a
	// tidy archive.
	if len(o.only) == 0 {
		known, err := r.db.MessageCounts()
		if err != nil {
			return nil, err
		}
		configured := map[string]bool{}
		for _, in := range r.cfg.Instances() {
			configured[in.ID] = true
		}
		for src, c := range known {
			if !configured[src] {
				plan.Unconfigured[src] = int(c.Total)
			}
		}
	}
	return plan, nil
}

// evaluate reads and classifies one note without touching anything. A note
// missing from the tree its row names is looked for in the other tree —
// that is what an interrupted move looks like — and read from there.
func (r *triageRun) evaluate(ctx context.Context, m state.Message) triageItem {
	it := triageItem{Msg: m, Want: state.DispositionArchive}
	abs, err := r.writer.NotePath(m.RelPath, m.Disposition)
	if err != nil {
		it.Problem = "unreadable: " + err.Error()
		return it
	}
	n, problem := r.readNote(abs)
	if problem == "missing" {
		other := otherTree(m.Disposition)
		if otherAbs, err := r.writer.NotePath(m.RelPath, other); err == nil {
			if n2, p2 := r.readNote(otherAbs); p2 != "missing" {
				n, problem, it.FoundIn = n2, p2, other
			}
		}
	}
	if problem != "" {
		it.Problem = problem
		return it
	}
	it.Decision = r.cls.Classify(ctx, n)
	if it.Decision.Verdict == state.TriageNoise {
		it.Want = state.DispositionSpam
	}
	return it
}

func otherTree(d state.Disposition) state.Disposition {
	if d == state.DispositionSpam {
		return state.DispositionArchive
	}
	return state.DispositionSpam
}

// triageTally is the outcome of an applied plan.
type triageTally struct {
	MovedToSpam int
	MovedBack   int
	Settled     int // notes whose verdict left them in place, now recorded under the digest
	Reconciled  int // rows corrected to where an interrupted move had left the file
	Transient   int // model failures: left alone and unsettled
	NotRead     int // evicted, missing, unreadable
	Failed      int // moves that errored; left unsettled for the next run
	Interrupted bool
}

// applyTriagePlan carries the plan out one note at a time. Per note the
// order is: ledger row (the decision), then the files (MoveNote), then the
// messages row (SetDisposition) — so a crash at any point leaves a state
// the next pass recovers, and the database never claims a location the
// files have not reached. A note whose move fails is logged and left
// unsettled; nothing partial is recorded for it.
func (r *triageRun) applyTriagePlan(ctx context.Context, plan *triagePlan) triageTally {
	var t triageTally
	digest := plan.Digest
	for _, it := range plan.Items {
		if err := ctx.Err(); err != nil {
			t.Interrupted = true
			break
		}
		if it.Problem != "" {
			t.NotRead++
			continue
		}
		src, id := it.Msg.Source, it.Msg.StableID
		if it.FoundIn != "" {
			if err := r.reconcile(it); err != nil {
				t.Failed++
				r.log.Error("could not reconcile a note an interrupted move left behind; left for the next run",
					"source", src, "stable_id", id, "path", it.Msg.RelPath, "err", err)
				continue
			}
			t.Reconciled++
			it.Msg.Disposition, it.FoundIn = it.FoundIn, ""
		}

		d := it.Decision
		if d.Transient {
			// Undecided because a layer failed: record the attempt, settle
			// nothing, keep the note where it is.
			if err := r.db.UpsertTriageDecision(state.TriageDecision{
				Source: src, StableID: id, Verdict: state.TriageUndecided,
				Layer: state.TriageLayerNone, Reason: d.Reason, Digest: digest,
			}); err != nil {
				r.log.Error("ledger write failed", "source", src, "stable_id", id, "err", err)
				t.Failed++
				continue
			}
			if err := r.db.SetDisposition(src, id, it.Msg.Disposition, d.Reason, "", ""); err != nil {
				r.log.Error("state write failed", "source", src, "stable_id", id, "err", err)
				t.Failed++
				continue
			}
			t.Transient++
			continue
		}

		rule := d.Rule
		if d.Layer == state.TriageLayerNone {
			rule = ""
		}
		if err := r.db.UpsertTriageDecision(state.TriageDecision{
			Source: src, StableID: id, Verdict: d.Verdict, Layer: d.Layer, Rule: rule, Reason: d.Reason, Digest: digest,
		}); err != nil {
			r.log.Error("ledger write failed", "source", src, "stable_id", id, "err", err)
			t.Failed++
			continue
		}
		if to, ok := it.Move(); ok {
			res, err := r.writer.MoveNote(it.Msg.RelPath, to)
			if err != nil {
				t.Failed++
				r.log.Error("move failed; the note stays where it is and is looked at again next run",
					"source", src, "stable_id", id, "path", it.Msg.RelPath, "to", to, "err", err)
				continue
			}
			if err := r.db.SetDisposition(src, id, to, d.Reason, d.RuleID(), digest); err != nil {
				// The files moved and the row did not: exactly the state the
				// next pass reconciles. Say so and stop, since a failing
				// database will fail the next note too.
				r.log.Error("the note moved but its row could not be updated; the next run reconciles it",
					"source", src, "stable_id", id, "path", it.Msg.RelPath, "err", err)
				t.Failed++
				continue
			}
			if to == state.DispositionSpam {
				t.MovedToSpam++
			} else {
				t.MovedBack++
			}
			r.log.Info("note moved", "source", src, "stable_id", id, "path", it.Msg.RelPath,
				"to", to, "rule", d.RuleID(), "attach_dir", res.MovedAttachDir)
			continue
		}
		if err := r.db.SetDisposition(src, id, it.Msg.Disposition, d.Reason, d.RuleID(), digest); err != nil {
			r.log.Error("state write failed", "source", src, "stable_id", id, "err", err)
			t.Failed++
			continue
		}
		t.Settled++
	}
	return t
}

// reconcile finishes a move a crash interrupted: the .md is already under
// it.FoundIn, so MoveNote only brings the attachment directory across, and
// the row is set to the file's location — with the decision the ledger
// recorded before the move when it agrees with where the file is, and a
// plain "reconciled" otherwise.
func (r *triageRun) reconcile(it triageItem) error {
	src, id := it.Msg.Source, it.Msg.StableID
	if _, err := r.writer.MoveNote(it.Msg.RelPath, it.FoundIn); err != nil {
		return err
	}
	reason, rule, digest := "reconciled after an interrupted move", "reconcile", ""
	if dec, ok, err := r.db.GetTriageDecision(src, id); err != nil {
		return err
	} else if ok && (dec.Verdict == state.TriageNoise) == (it.FoundIn == state.DispositionSpam) {
		reason, rule, digest = dec.Reason, dec.Layer+":"+dec.Rule, dec.Digest
		if dec.Layer == state.TriageLayerNone {
			rule = ""
		}
	}
	r.log.Warn("note found in the other tree; an earlier move was interrupted — reconciling the row to the file",
		"source", src, "stable_id", id, "path", it.Msg.RelPath, "found_in", it.FoundIn)
	return r.db.SetDisposition(src, id, it.FoundIn, reason, rule, digest)
}

// finishTriage prints the outcome.
func finishTriage(out io.Writer, t triageTally) error {
	fmt.Fprintf(out, "\nmoved %d note(s) to spam, %d back to the archive; %d left in place and settled\n", t.MovedToSpam, t.MovedBack, t.Settled)
	if t.Reconciled > 0 {
		fmt.Fprintf(out, "reconciled: %d note(s) an interrupted move had left half-recorded\n", t.Reconciled)
	}
	if t.Transient > 0 {
		fmt.Fprintf(out, "model unavailable for %d note(s): left where they are, looked at again next run\n", t.Transient)
	}
	if t.NotRead > 0 {
		fmt.Fprintf(out, "not read: %d note(s) (evicted, missing or unreadable) — see `save verify`\n", t.NotRead)
	}
	if t.Interrupted {
		fmt.Fprintf(out, "interrupted — re-run `save triage` to continue; every note not yet recorded is looked at again\n")
	}
	if t.Failed > 0 {
		fmt.Fprintf(out, "failed: %d note(s) — left unsettled, re-run `save triage` (details in the log above)\n", t.Failed)
		return fmt.Errorf("%d note(s) could not be triaged", t.Failed)
	}
	return nil
}

// readNote reads a note for classification. An evicted iCloud placeholder
// is skipped rather than read: reading would download it, which is exactly
// what `save verify` refuses to do by accident, and a triage pass runs in
// the daemon where that download is silent.
func (r *triageRun) readNote(abs string) (*triage.Note, string) {
	switch dl, err := r.isDataless(abs); {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "missing"
	case err != nil:
		return nil, "unreadable: " + err.Error()
	case dl:
		return nil, "evicted"
	}
	b, err := os.ReadFile(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "missing"
	case err != nil && evictedRead(err):
		return nil, "evicted"
	case err != nil:
		return nil, "unreadable: " + err.Error()
	}
	n, err := triage.ParseNote(b)
	switch {
	case errors.Is(err, triage.ErrNotEmailNote):
		return nil, "not-email"
	case err != nil:
		return nil, "unreadable: " + err.Error()
	}
	return n, ""
}

// dryRunTriage prints the plan and stops. Like `save refetch --dry-run` it
// takes no instance lock, so it can answer beside a running daemon.
func dryRunTriage(out io.Writer, o triageOpts) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	dbPath := stateDBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(out, "no state database at %s yet — nothing has been archived, so there is nothing to triage\n", dbPath)
		return nil
	}
	sdb, err := state.Open(dbPath)
	if err != nil {
		return err
	}
	defer sdb.Close()
	loc, _, err := config.ResolveTimezone(cfg.Timezone)
	if err != nil {
		return err
	}
	// The dry run consults the model too when it is enabled: the plan is
	// only worth printing if it is the plan the real run would make.
	judge, err := llmJudge(cfg, false)
	if err != nil {
		return err
	}
	cls, err := newClassifier(cfg, judge)
	if err != nil {
		return err
	}
	r := &triageRun{
		cfg: cfg, db: sdb, cls: cls, log: logger(),
		writer: &archive.Writer{Root: cfg.ArchiveRoot, SpamRoot: cfg.SpamRoot, TZ: loc},
	}
	ctx, stop := signalContext()
	defer stop()
	plan, err := r.buildTriagePlan(ctx, o)
	if err != nil {
		return err
	}
	printTriagePlan(out, plan, true)
	fmt.Fprintf(out, "\ndry run: nothing was moved. Re-run without --dry-run to move.\n")
	return nil
}

// printTriagePlan states, before anything moves, exactly what would. With
// files set, every move is listed with its rule and reason.
func printTriagePlan(out io.Writer, plan *triagePlan, files bool) {
	fmt.Fprintf(out, "triage digest: %s\n", plan.Digest)
	heur := "on"
	if !plan.Heuristics {
		heur = "off"
	}
	model := "off"
	if plan.Model != "" {
		model = plan.Model
	}
	rulesWhere := plan.RulesPath
	if plan.Keep+plan.Noise == 0 {
		rulesWhere += " (no rules — `save init` writes a starter file)"
	}
	fmt.Fprintf(out, "  rules: %s — %d keep, %d noise; header heuristics %s; model %s\n", rulesWhere, plan.Keep, plan.Noise, heur, model)

	toSpam, toArchive, protected, kept, byModel, undecided, transient, evicted, missing, unreadable, reconcile, perSource := plan.tally()
	fmt.Fprintf(out, "\n%d note(s) in scope\n", len(plan.Items))
	if plan.Manual > 0 {
		fmt.Fprintf(out, "  %d note(s) placed by `save untriage` are left alone (re-evaluate them with --reclassify --only manual)\n", plan.Manual)
	}
	if reconcile > 0 {
		fmt.Fprintf(out, "  %d note(s) found in the other tree than the database records (an interrupted move) — the row is corrected first, then the verdict applied\n", reconcile)
	}
	if toSpam == 0 {
		fmt.Fprintf(out, "  nothing to move to spam\n")
	} else {
		fmt.Fprintf(out, "  WOULD MOVE to spam: %d note(s)\n", toSpam)
		for _, src := range sortedKeys(perSource) {
			fmt.Fprintf(out, "    %-18s %d note(s)\n", src, perSource[src])
		}
	}
	if toArchive > 0 {
		fmt.Fprintf(out, "  would move back to the archive: %d note(s) (the current rules no longer call them noise)\n", toArchive)
	}
	fmt.Fprintf(out, "  would stay: %d protected, %d kept by a rule, %d kept by the model, %d undecided\n", protected, kept, byModel, undecided)
	if transient > 0 {
		fmt.Fprintf(out, "  model unavailable for %d note(s): left where they are and not settled — they are looked at again next run\n", transient)
	}
	if evicted+missing+unreadable > 0 {
		fmt.Fprintf(out, "  not read: %d evicted to iCloud (reading would download them), %d missing, %d unreadable — see `save verify`\n", evicted, missing, unreadable)
	}
	if files {
		printed := false
		for _, it := range plan.Items {
			to, ok := it.Move()
			if !ok {
				continue
			}
			if !printed {
				fmt.Fprintf(out, "\nmoves (file, rule, reason):\n")
				printed = true
			}
			arrow := "→ spam"
			if to == state.DispositionArchive {
				arrow = "→ archive"
			}
			fmt.Fprintf(out, "  %s  %s  %s\n    %s\n", it.Msg.RelPath, arrow, orNone(it.Decision.RuleID()), it.Decision.Reason)
		}
		for _, it := range plan.Items {
			switch {
			case it.Problem == "missing":
				fmt.Fprintf(out, "  missing: %s (%s/%s) — the state database names a note that is in neither tree; run `save verify`\n",
					it.Msg.RelPath, it.Msg.Source, it.Msg.StableID)
			case it.FoundIn != "":
				fmt.Fprintf(out, "  would reconcile: %s is in the %s tree but recorded in the %s tree\n",
					it.Msg.RelPath, it.FoundIn, it.Msg.Disposition)
			}
		}
	}
	if len(plan.Unconfigured) > 0 {
		fmt.Fprintf(out, "\nwarning: the archive holds notes for instance(s) the config no longer declares:\n")
		for _, src := range sortedKeys(plan.Unconfigured) {
			fmt.Fprintf(out, "  %-18s %d note(s) — not triaged until that label is configured again\n", src, plan.Unconfigured[src])
		}
	}
}

// explainNote classifies one note and prints what every layer saw, plus
// what the state database has recorded for it. It reads the note wherever
// the path points, so a note in either tree — or a copy somewhere else
// entirely — can be asked about.
func explainNote(out io.Writer, notePath string) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	judge, err := llmJudge(cfg, false)
	if err != nil {
		return err
	}
	cls, err := newClassifier(cfg, judge)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(notePath)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	n, err := triage.ParseNote(b)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "note: %s\n", abs)
	fmt.Fprintf(out, "  from %s; subject %q; account %s; render_version %d\n",
		strings.Join(n.From, ", "), n.Subject, n.AccountLabel, n.RenderVersion)

	// Which tree is it in, and what does the row say?
	rel, disp, inTree := relInTree(cfg, abs)
	if !inTree {
		fmt.Fprintf(out, "  location: outside both archive_root and spam_root — classified from the file alone\n")
	} else {
		fmt.Fprintf(out, "  location: %s tree, %s\n", disp, rel)
		if _, err := os.Stat(stateDBPath()); err == nil {
			sdb, err := state.Open(stateDBPath())
			if err != nil {
				return err
			}
			rows, err := sdb.MessagesByRelPath(rel)
			if err != nil {
				sdb.Close()
				return err
			}
			for _, m := range rows {
				fmt.Fprintf(out, "  recorded: disposition %s", m.Disposition)
				if m.DispositionRule != "" {
					fmt.Fprintf(out, " by %s (%s) at %s", m.DispositionRule, m.DispositionReason, m.DispositionAt.Format(time.RFC3339))
				}
				if m.TriageDigest != "" {
					fmt.Fprintf(out, ", settled under %s", m.TriageDigest)
				}
				fmt.Fprintln(out)
				if m.Disposition != disp {
					fmt.Fprintf(out, "  WARNING: the state database says %s but the file is in the %s tree — run `save verify`\n", m.Disposition, disp)
				}
			}
			if len(rows) == 0 {
				fmt.Fprintf(out, "  recorded: no messages row for this path\n")
			}
			sdb.Close()
		}
	}

	ctx, stop := signalContext()
	defer stop()
	d := cls.Classify(ctx, n)
	fmt.Fprintf(out, "\nunder digest %s this note is %s", cls.Digest(), strings.ToUpper(string(d.Verdict)))
	if d.Layer != state.TriageLayerNone {
		fmt.Fprintf(out, " by %s", d.RuleID())
	}
	fmt.Fprintf(out, ": %s\n", d.Reason)
	for _, line := range d.Trace {
		fmt.Fprintf(out, "  %s\n", line)
	}
	return nil
}

// relInTree reports which configured tree abs lies in and its rel path
// there.
func relInTree(cfg *config.Config, abs string) (rel string, disp state.Disposition, ok bool) {
	for _, tree := range []struct {
		root string
		disp state.Disposition
	}{{cfg.ArchiveRoot, state.DispositionArchive}, {cfg.SpamRoot, state.DispositionSpam}} {
		if !underDir(tree.root, abs) {
			continue
		}
		r, err := filepath.Rel(tree.root, abs)
		if err != nil {
			continue
		}
		return filepath.ToSlash(r), tree.disp, true
	}
	return "", "", false
}
