package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"comms/internal/config"
	"comms/internal/policy"
	"comms/internal/state"
)

func newStatusCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show per-account cursors, counts, failures, and recent runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.OutOrStdout(), jsonOut)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON")
	return c
}

// statusReport is the full status document; --json emits it verbatim.
type statusReport struct {
	GeneratedAt      time.Time      `json:"generated_at"`
	ArchiveRoot      string         `json:"archive_root"`
	SpamRoot         string         `json:"spam_root"` // where noise triage files notes
	StateDB          string         `json:"state_db"`
	TimezoneConfig   string         `json:"timezone_config"` // zone the config resolves to
	TimezonePinned   string         `json:"timezone_pinned"` // meta.archive_tz; "" before first sync
	TimezoneMismatch bool           `json:"timezone_mismatch"`
	AttachmentPolicy *policyStatus  `json:"attachment_policy,omitempty"`
	Sources          []sourceStatus `json:"sources"`
	// UnconfiguredSources are instance ids the state database still holds
	// rows for but the config no longer declares — the fingerprint of a
	// renamed or deleted account label.
	UnconfiguredSources []string `json:"unconfigured_sources,omitempty"`
}

// policyStatus is the attachment-policy header: the digest now in force, the
// digest the whole backlog was last reconsidered under, and — computed
// offline, with no network — how much of that backlog the current policy
// would now accept.
//
// A difference is never acted on automatically. This block exists precisely
// so the user finds out about it here and then decides, rather than a sync
// deciding for them.
type policyStatus struct {
	Digest         string `json:"digest"`
	RecordedDigest string `json:"recorded_digest,omitempty"`
	Changed        bool   `json:"changed"`

	Unresolved       int   `json:"unresolved_skips"`
	NowAccepted      int   `json:"now_accepted"`
	NowAcceptedBytes int64 `json:"now_accepted_bytes"`
}

// skippedAttachmentStatus is one instance's never-drop-silently ledger.
type skippedAttachmentStatus struct {
	Total      int64 `json:"total"`
	Unresolved int64 `json:"unresolved"`

	NotStoredBytes  int64 `json:"not_stored_bytes"`
	UnresolvedBytes int64 `json:"unresolved_bytes"`

	ByReason           map[string]int64 `json:"by_reason,omitempty"`
	UnresolvedByReason map[string]int64 `json:"unresolved_by_reason,omitempty"`
}

// triageStatus is one instance's noise-triage ledger.
type triageStatus struct {
	Noise     int64 `json:"noise"`     // filed under spam_root
	Signal    int64 `json:"signal"`    // kept, by a protect rule, a keep rule, the model, or untriage
	Undecided int64 `json:"undecided"` // no layer decided; kept

	// ByRule splits the counts by verdict, deciding layer and rule.
	ByRule []triageRuleStatus `json:"by_rule,omitempty"`
}

type triageRuleStatus struct {
	Verdict string `json:"verdict"`
	Layer   string `json:"layer"`
	Rule    string `json:"rule,omitempty"`
	Count   int64  `json:"count"`
}

// sourceStatus is one configured INSTANCE: one account's one source kind.
type sourceStatus struct {
	Name        string                  `json:"name"` // instance id, e.g. "gmail:work"
	Kind        string                  `json:"kind"`
	Label       string                  `json:"label"`
	Account     string                  `json:"account"`
	Messages    *state.MessageCount     `json:"messages,omitempty"` // email sources
	Chat        *chatCounts             `json:"chat,omitempty"`     // gchat
	Attachments *state.AttachmentCounts `json:"attachments,omitempty"`
	// SkippedAttachments is the attachment-policy ledger, and is NOT the same
	// thing as SkippedItems below: this is "the policy refused these bytes",
	// that is "these items failed to process too many times".
	SkippedAttachments *skippedAttachmentStatus `json:"skipped_attachments,omitempty"`
	// Triage is the noise-triage ledger: how many notes each layer and rule
	// decided, and what it decided.
	Triage           *triageStatus   `json:"triage,omitempty"`
	BackfillPending  int64           `json:"backfill_pending,omitempty"` // gmail
	Cursors          []cursorRow     `json:"cursors,omitempty"`
	SkippedItems     []state.Failure `json:"skipped_items,omitempty"`      // at/over the skip threshold
	FailingItems     int             `json:"failing_items"`                // below the threshold, still retried
	HistoryOffSpaces []string        `json:"history_off_spaces,omitempty"` // gchat
	RecentRuns       []state.SyncRun `json:"recent_runs,omitempty"`
}

func runStatus(out io.Writer, jsonOut bool) error {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	dbPath := stateDBPath()
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		if jsonOut {
			fmt.Fprintln(out, "{}")
			return nil
		}
		fmt.Fprintf(out, "no state database at %s yet — run `comms sync` first\n", dbPath)
		return nil
	}

	rep, err := buildStatus(cfg, dbPath)
	if err != nil {
		return err
	}
	if jsonOut {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(b))
		return nil
	}
	printStatus(out, rep)
	return nil
}

func buildStatus(cfg *config.Config, dbPath string) (*statusReport, error) {
	sdb, err := state.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer sdb.Close()
	ro, err := openRO(dbPath)
	if err != nil {
		return nil, err
	}
	defer ro.Close()

	rep := &statusReport{
		GeneratedAt: time.Now().UTC(),
		ArchiveRoot: cfg.ArchiveRoot,
		SpamRoot:    cfg.SpamRoot,
		StateDB:     dbPath,
	}
	if _, zone, err := config.ResolveTimezone(cfg.Timezone); err == nil {
		rep.TimezoneConfig = zone
	}
	if pinned, ok, err := sdb.GetMeta(state.MetaArchiveTZ); err != nil {
		return nil, err
	} else if ok {
		rep.TimezonePinned = pinned
		rep.TimezoneMismatch = rep.TimezoneConfig != "" && pinned != rep.TimezoneConfig
	}

	msgCounts, err := sdb.MessageCounts()
	if err != nil {
		return nil, err
	}
	attCounts, err := sdb.AttachmentCounts()
	if err != nil {
		return nil, err
	}
	failures, err := sdb.ListFailures()
	if err != nil {
		return nil, err
	}
	skipCounts, err := sdb.CountSkipped()
	if err != nil {
		return nil, err
	}
	skipBytes, err := skippedBytesRO(ro)
	if err != nil {
		return nil, err
	}
	triageCounts, err := sdb.CountTriage()
	if err != nil {
		return nil, err
	}
	if rep.AttachmentPolicy, err = buildPolicyStatus(cfg, sdb); err != nil {
		return nil, err
	}

	configured := make(map[string]bool)
	for _, in := range cfg.Instances() {
		configured[in.ID] = true
		ss := sourceStatus{Name: in.ID, Kind: in.Kind, Label: in.Label, Account: in.Account}

		if c, ok := msgCounts[in.ID]; ok {
			cc := c
			ss.Messages = &cc
		}
		if c, ok := attCounts[in.ID]; ok {
			cc := c
			ss.Attachments = &cc
		}
		if c, ok := skipCounts[in.ID]; ok {
			b := skipBytes[in.ID]
			ss.SkippedAttachments = &skippedAttachmentStatus{
				Total:              c.Total,
				Unresolved:         c.Unresolved,
				NotStoredBytes:     b.NotStored,
				UnresolvedBytes:    b.Unresolved,
				ByReason:           c.ByReason,
				UnresolvedByReason: c.UnresolvedByReason,
			}
		}
		if c, ok := triageCounts[in.ID]; ok {
			ts := &triageStatus{Noise: c.Noise, Signal: c.Signal, Undecided: c.Undecided}
			for _, rc := range c.ByRule {
				ts.ByRule = append(ts.ByRule, triageRuleStatus{Verdict: string(rc.Verdict), Layer: rc.Layer, Rule: rc.Rule, Count: rc.Count})
			}
			ss.Triage = ts
		}
		cursors, err := listCursorsRO(ro, in.ID)
		if err != nil {
			return nil, err
		}
		ss.Cursors = cursors
		for _, f := range failures {
			if f.Source != in.ID {
				continue
			}
			if f.Attempts >= state.SkipThreshold {
				ss.SkippedItems = append(ss.SkippedItems, f)
			} else {
				ss.FailingItems++
			}
		}
		runs, err := sdb.RecentRuns(in.ID, 5)
		if err != nil {
			return nil, err
		}
		ss.RecentRuns = runs

		switch in.Kind {
		case state.SourceGmail:
			pending, err := sdb.BackfillPendingCount(in.ID)
			if err != nil {
				return nil, err
			}
			ss.BackfillPending = pending
		case state.SourceGChat:
			cc, err := chatCountsRO(ro, in.ID)
			if err != nil {
				return nil, err
			}
			ss.Chat = &cc
			spaces, err := sdb.ListSpaces(in.ID)
			if err != nil {
				return nil, err
			}
			for _, sp := range spaces {
				if sp.HistoryState == "HISTORY_OFF" {
					label := sp.Name
					if sp.DisplayName != "" {
						label += " (" + sp.DisplayName + ")"
					}
					ss.HistoryOffSpaces = append(ss.HistoryOffSpaces, label)
				}
			}
		}
		rep.Sources = append(rep.Sources, ss)
	}

	// State rows keyed by an instance the config no longer declares are
	// stranded: nothing will ever sync or re-render them again. A renamed
	// label is exactly this, so it is worth naming out loud.
	known, err := listSourcesRO(ro)
	if err != nil {
		return nil, err
	}
	for _, src := range known {
		if !configured[src] {
			rep.UnconfiguredSources = append(rep.UnconfiguredSources, src)
		}
	}
	return rep, nil
}

// buildPolicyStatus compares the policy the config now describes against the
// one the archive was last reconsidered under, and re-decides the unresolved
// backlog against it. The re-decision is entirely offline — every skip row
// carries the size, the extension and the sniffed type its original verdict
// used — so `comms status` stays a read-only, no-network command.
func buildPolicyStatus(cfg *config.Config, sdb *state.DB) (*policyStatus, error) {
	pol, err := cfg.Policy()
	if err != nil {
		return nil, err
	}
	recorded, _, err := sdb.AttachmentPolicyDigest()
	if err != nil {
		return nil, err
	}
	ps := &policyStatus{
		Digest:         pol.PolicyDigest(),
		RecordedDigest: recorded,
		Changed:        recorded != "" && recorded != pol.PolicyDigest(),
	}
	rows, err := sdb.ListUnresolvedSkipped(0)
	if err != nil {
		return nil, err
	}
	ps.Unresolved = len(rows)
	for _, row := range rows {
		if fetch, _, _ := reconsider(pol, row); fetch {
			ps.NowAccepted++
			ps.NowAcceptedBytes += row.SizeBytes
		}
	}
	return ps, nil
}

func printStatus(out io.Writer, rep *statusReport) {
	fmt.Fprintf(out, "archive root: %s\n", rep.ArchiveRoot)
	fmt.Fprintf(out, "spam root:    %s\n", rep.SpamRoot)
	fmt.Fprintf(out, "state db:     %s\n", rep.StateDB)
	switch {
	case rep.TimezonePinned == "":
		fmt.Fprintf(out, "timezone:     %s (not pinned yet — first sync pins it)\n", rep.TimezoneConfig)
	case rep.TimezoneMismatch:
		fmt.Fprintf(out, "timezone:     MISMATCH — pinned %s but config resolves %s (sync will refuse to start)\n",
			rep.TimezonePinned, rep.TimezoneConfig)
	default:
		fmt.Fprintf(out, "timezone:     %s (pinned)\n", rep.TimezonePinned)
	}
	printPolicyStatus(out, rep.AttachmentPolicy)

	for _, ss := range rep.Sources {
		fmt.Fprintf(out, "\n%s — %s\n", ss.Name, ss.Account)
		if ss.Messages != nil {
			fmt.Fprintf(out, "  messages:     %d archived (%d deleted upstream)\n", ss.Messages.Total, ss.Messages.Deleted)
		}
		if ss.Chat != nil {
			fmt.Fprintf(out, "  chat:         %d messages (%d deleted), %d day files (%d dirty)\n",
				ss.Chat.Messages, ss.Chat.Deleted, ss.Chat.DayFiles, ss.Chat.DirtyDays)
		}
		if ss.BackfillPending > 0 {
			fmt.Fprintf(out, "  backfill:     %d messages still queued\n", ss.BackfillPending)
		}
		if ss.Attachments != nil {
			fmt.Fprintf(out, "  attachments:  %d pending, %d done, %d failed\n",
				ss.Attachments.Pending, ss.Attachments.Done, ss.Attachments.Failed)
		}
		printSkippedAttachments(out, ss.SkippedAttachments)
		printTriageStatus(out, ss.Triage)
		if len(ss.Cursors) == 0 {
			fmt.Fprintf(out, "  cursors:      none (backfill not started or not finished)\n")
		} else {
			fmt.Fprintf(out, "  cursors:\n")
			for _, c := range ss.Cursors {
				scope := ""
				if c.Scope != "" {
					scope = c.Scope + " "
				}
				fmt.Fprintf(out, "    %s%s = %s (age %s)\n", scope, c.Kind, truncate(c.Value, 28), age(c.UpdatedAt))
			}
		}
		if ss.FailingItems > 0 {
			fmt.Fprintf(out, "  failing:      %d item(s) below the skip threshold, still retried\n", ss.FailingItems)
		}
		if len(ss.SkippedItems) > 0 {
			fmt.Fprintf(out, "  skipped:      %d poison item(s) — re-drive with `comms sync --retry-failed`\n", len(ss.SkippedItems))
			for _, f := range ss.SkippedItems {
				fmt.Fprintf(out, "    %s (attempts %d): %s\n", f.ID, f.Attempts, truncate(f.LastError, 80))
			}
		}
		for _, w := range ss.HistoryOffSpaces {
			fmt.Fprintf(out, "  warning:      history is OFF for %s — messages older than 24h are unrecoverable if syncing falls behind\n", w)
		}
		if len(ss.RecentRuns) > 0 {
			fmt.Fprintf(out, "  recent runs:\n")
			for _, r := range ss.RecentRuns {
				switch {
				case !r.Finished:
					fmt.Fprintf(out, "    %s %s: unfinished (running or killed)\n", r.StartedAt.Format(time.RFC3339), r.Kind)
				case r.OK:
					fmt.Fprintf(out, "    %s %s: ok (%s)\n", r.StartedAt.Format(time.RFC3339), r.Kind,
						r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
				default:
					fmt.Fprintf(out, "    %s %s: FAILED: %s\n", r.StartedAt.Format(time.RFC3339), r.Kind, truncate(r.Error, 80))
				}
			}
		}
	}

	if len(rep.UnconfiguredSources) > 0 {
		fmt.Fprintf(out, "\nwarning: the state database holds rows for instance(s) the config no longer declares:\n")
		for _, s := range rep.UnconfiguredSources {
			fmt.Fprintf(out, "  %s\n", s)
		}
		fmt.Fprintf(out, "  Their archived files stay on disk but nothing syncs or re-renders them.\n"+
			"  If a label was renamed, restore the old label to adopt that archive again.\n")
	}
}

// printPolicyStatus prints the attachment-policy header. It never says "run
// comms refetch" without first saying how much would be fetched: that number
// is the whole point of refusing to do it automatically.
func printPolicyStatus(out io.Writer, ps *policyStatus) {
	if ps == nil {
		return
	}
	if ps.Changed {
		fmt.Fprintf(out, "attachments:  policy CHANGED — now %s, archive last reconsidered under %s\n",
			ps.Digest, ps.RecordedDigest)
	} else {
		fmt.Fprintf(out, "attachments:  policy %s\n", ps.Digest)
	}
	if ps.Unresolved == 0 {
		return
	}
	fmt.Fprintf(out, "              %d unresolved skip(s); %d (%s) would be accepted by the current policy\n",
		ps.Unresolved, ps.NowAccepted, byteSize(ps.NowAcceptedBytes))
	if ps.NowAccepted > 0 {
		fmt.Fprintf(out, "              nothing is ever re-fetched automatically — run `comms refetch --dry-run` to see the plan\n")
	}
}

// printSkippedAttachments prints one instance's refused-attachment ledger,
// broken down by reason in the policy's canonical reason order.
func printSkippedAttachments(out io.Writer, s *skippedAttachmentStatus) {
	if s == nil || s.Total == 0 {
		return
	}
	fmt.Fprintf(out, "  refused:      %d attachment(s) by the attachment policy, %s not stored (%d unresolved, %s)\n",
		s.Total, byteSize(s.NotStoredBytes), s.Unresolved, byteSize(s.UnresolvedBytes))
	for _, reason := range policy.Reasons() {
		total := s.ByReason[reason]
		if total == 0 {
			continue
		}
		fmt.Fprintf(out, "    %-28s %d (%d unresolved)\n", reason, total, s.UnresolvedByReason[reason])
	}
}

// printTriageStatus prints one instance's noise-triage ledger, split by the
// deciding layer and rule.
func printTriageStatus(out io.Writer, s *triageStatus) {
	if s == nil {
		return
	}
	fmt.Fprintf(out, "  triage:       %d noise (filed under spam_root), %d signal, %d undecided\n", s.Noise, s.Signal, s.Undecided)
	for _, r := range s.ByRule {
		id := r.Layer
		if r.Rule != "" {
			id += ":" + r.Rule
		}
		fmt.Fprintf(out, "    %-9s %-36s %d\n", r.Verdict, id, r.Count)
	}
}

func age(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return time.Since(t).Round(time.Second).String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
