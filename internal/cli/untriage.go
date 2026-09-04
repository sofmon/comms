package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"comms/internal/state"
)

func newUntriageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "untriage PATH|ID",
		Short: "Move a note noise triage filed under spam_root back into the archive",
		Long: `Move one note back from spam_root into the archive, with its attachment
folder, and record that you did: a note moved back by hand is never filed
again by a later ` + "`comms triage`" + ` pass, whatever the rules say (re-evaluate it
deliberately with ` + "`comms triage --reclassify --only manual`" + `).

The note is named by its path — in either tree — or by its message id, as
"<instance>/<id>" (e.g. gmail:work/18f2a...) or a bare id when only one
account archived it. A note already in the archive is recorded as kept by
hand without moving anything.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUntriage(cmd.OutOrStdout(), args[0])
		},
	}
}

func runUntriage(out io.Writer, target string) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.Close()

	m, err := a.findNote(target)
	if err != nil {
		return err
	}
	// MoveNote finds the note wherever it is: under spam_root it moves it
	// (with its attachment folder); already under archive_root it reports
	// AlreadyThere and only brings a lagging attachment folder across.
	res, err := a.writer.MoveNote(m.RelPath, state.DispositionArchive)
	if err != nil {
		return err
	}
	const reason = "moved back by comms untriage"
	if err := a.db.UpsertTriageDecision(state.TriageDecision{
		Source: m.Source, StableID: m.StableID, Verdict: state.TriageSignal,
		Layer: state.TriageLayerManual, Rule: "untriage", Reason: reason, Digest: manualRule,
	}); err != nil {
		return err
	}
	if err := a.db.SetDisposition(m.Source, m.StableID, state.DispositionArchive, reason, manualRule, ""); err != nil {
		return err
	}
	switch {
	case res.AlreadyThere && m.Disposition == state.DispositionArchive:
		fmt.Fprintf(out, "%s was already in the archive; recorded as kept by hand so triage leaves it alone\n", m.RelPath)
	case res.AlreadyThere:
		fmt.Fprintf(out, "%s was already in the archive tree (an interrupted move); its row now agrees, and it is kept by hand\n", m.RelPath)
	default:
		fmt.Fprintf(out, "moved %s back to the archive (attachment folder: %v); kept by hand from now on\n", m.RelPath, res.MovedAttachDir)
	}
	return nil
}

// findNote resolves the argument to one messages row: a path inside either
// tree, "<instance>/<stable id>", or a bare stable id that only one account
// holds.
func (a *app) findNote(target string) (state.Message, error) {
	if strings.HasSuffix(target, ".md") || strings.ContainsAny(target, `/\`) && !strings.Contains(target, ":") {
		abs, err := filepath.Abs(target)
		if err != nil {
			return state.Message{}, err
		}
		rel, _, ok := relInTree(a.cfg, abs)
		if !ok {
			return state.Message{}, fmt.Errorf("%s is under neither archive_root (%s) nor spam_root (%s)", abs, a.cfg.ArchiveRoot, a.cfg.SpamRoot)
		}
		rows, err := a.db.MessagesByRelPath(rel)
		if err != nil {
			return state.Message{}, err
		}
		return onlyOne(rows, "path "+rel)
	}
	if inst, id, ok := strings.Cut(target, "/"); ok && strings.Contains(inst, ":") {
		if _, ok := a.cfg.InstanceByID(inst); !ok {
			return state.Message{}, fmt.Errorf("%q is not a configured instance (%s)", inst, selectorHelp(a.cfg))
		}
		m, found, err := a.db.GetMessage(inst, id)
		if err != nil {
			return state.Message{}, err
		}
		if !found {
			return state.Message{}, fmt.Errorf("no archived message %s in %s", id, inst)
		}
		return m, nil
	}
	rows, err := a.db.MessagesByStableID(target)
	if err != nil {
		return state.Message{}, err
	}
	return onlyOne(rows, "message id "+target)
}

func onlyOne(rows []state.Message, what string) (state.Message, error) {
	switch len(rows) {
	case 0:
		return state.Message{}, fmt.Errorf("no archived message matches %s", what)
	case 1:
		return rows[0], nil
	}
	var ids []string
	for _, m := range rows {
		ids = append(ids, m.Source+"/"+m.StableID)
	}
	return state.Message{}, fmt.Errorf("%s matches %d messages (%s) — name one as <instance>/<id>", what, len(rows), strings.Join(ids, ", "))
}
