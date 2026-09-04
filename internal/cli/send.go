package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"

	chat "google.golang.org/api/chat/v1"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"comms/internal/config"
	"comms/internal/lock"
	"comms/internal/outbox"
	"comms/internal/paths"
	fastmailsender "comms/internal/sender/fastmail"
	gchatsender "comms/internal/sender/gchat"
	gmailsender "comms/internal/sender/gmail"
	"comms/internal/source"
	sourcefastmail "comms/internal/source/fastmail"
	"comms/internal/state"
)

const sendResponseBodyCap = 16 << 20

type sendApp struct {
	cfg     *config.Config
	db      *state.DB
	release func()
}

func openSendApp() (*sendApp, error) {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return nil, err
	}
	stateDir := paths.StateDir()
	if err := paths.EnsureDir(stateDir); err != nil {
		return nil, err
	}
	release, err := lock.AcquireNamed(stateDir, "send.lock")
	if err != nil {
		return nil, err
	}
	a := &sendApp{cfg: cfg, release: release}
	db, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		a.Close()
		return nil, err
	}
	a.db = db
	return a, nil
}

func (a *sendApp) Close() {
	if a.db != nil {
		_ = a.db.Close()
		a.db = nil
	}
	if a.release != nil {
		a.release()
	}
}

type outboundBuilder func(context.Context, *sendApp, config.Instance) (outbox.Sender, error)

func newSendCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send every valid Markdown draft in the filesystem outbox",
		Long: "Send every valid .md file under the configured outbox send/ directory.\n\n" +
			"Each successful file is atomically moved to archived/YYYY/MM/DD. Invalid\n" +
			"or failed files stay in send/. Sending is explicit: the daemon never runs it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSend(cmd, dryRun, buildOutboundSender)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate and list drafts without sending or moving them")
	return cmd
}

func runSend(cmd *cobra.Command, dryRun bool, build outboundBuilder) error {
	ctx, stop := signalContext()
	defer stop()

	a, err := openSendApp()
	if err != nil {
		return err
	}
	defer a.Close()

	sendDir := a.cfg.Sending.SendDir()
	archivedDir := a.cfg.Sending.ArchivedDir()
	for _, dir := range []string{a.cfg.Sending.Root, sendDir, archivedDir} {
		if err := paths.EnsureDir(dir); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	failed, recovered := 0, 0
	blocked := map[string]bool{}
	if !dryRun {
		rows, err := a.db.UnarchivedOutgoing()
		if err != nil {
			return err
		}
		for _, row := range rows {
			moved, err := outbox.FinishArchive(sendDir, archivedDir, row.DraftRelPath, row.ArchiveRelPath, row.ContentHash)
			if err != nil {
				fmt.Fprintf(out, "FAILED recovery %s: %v\n", row.DraftRelPath, err)
				blocked[row.DraftRelPath] = true
				failed++
				continue
			}
			if err := a.db.MarkOutgoingArchived(row.MessageKey); err != nil {
				fmt.Fprintf(out, "FAILED recovery %s: %v\n", row.DraftRelPath, err)
				blocked[row.DraftRelPath] = true
				failed++
				continue
			}
			verb := "confirmed"
			if moved {
				verb = "moved"
			}
			fmt.Fprintf(out, "recovered %s: %s -> archived/%s\n", row.DraftRelPath, verb, row.ArchiveRelPath)
			recovered++
		}
	}

	candidates, err := outbox.LoadDir(sendDir)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Fprintf(out, "no outgoing Markdown files in %s\n", sendDir)
		if failed > 0 {
			return fmt.Errorf("%d outgoing message operation(s) failed", failed)
		}
		return nil
	}

	type cachedSender struct {
		sender outbox.Sender
		err    error
	}
	cache := map[string]cachedSender{}
	sent := 0
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if blocked[candidate.RelPath] {
			continue
		}
		if candidate.Err != nil {
			fmt.Fprintf(out, "FAILED %s: %v\n", candidate.RelPath, candidate.Err)
			failed++
			continue
		}
		draft := candidate.Draft
		inst, err := resolveOutboundInstance(a.cfg, draft)
		if err != nil {
			fmt.Fprintf(out, "FAILED %s: %v\n", candidate.RelPath, err)
			failed++
			continue
		}
		if dryRun {
			fmt.Fprintf(out, "would send %s via %s (%s)\n", candidate.RelPath, inst.ID, describeDraft(draft))
			continue
		}

		row, err := a.db.PrepareOutgoing(state.Outgoing{
			MessageKey:   draft.MessageKey,
			Source:       inst.ID,
			Kind:         string(draft.Kind),
			DraftRelPath: draft.RelPath,
			ContentHash:  draft.ContentHash,
		})
		if err != nil {
			fmt.Fprintf(out, "FAILED %s: %v\n", candidate.RelPath, err)
			failed++
			continue
		}
		if row.Status == state.OutgoingArchived {
			fmt.Fprintf(out, "FAILED %s: this exact draft was already sent and archived; change its filename or content to send it again\n", candidate.RelPath)
			failed++
			continue
		}
		if row.Status == state.OutgoingSent {
			moved, err := outbox.FinishArchive(sendDir, archivedDir, row.DraftRelPath, row.ArchiveRelPath, row.ContentHash)
			if err == nil {
				err = a.db.MarkOutgoingArchived(row.MessageKey)
			}
			if err != nil {
				fmt.Fprintf(out, "FAILED %s: delivery was already recorded, but archival failed: %v\n", candidate.RelPath, err)
				failed++
				continue
			}
			fmt.Fprintf(out, "recovered %s: archived (moved=%t)\n", candidate.RelPath, moved)
			recovered++
			continue
		}

		cached, ok := cache[inst.ID]
		if !ok {
			cached.sender, cached.err = build(ctx, a, inst)
			cache[inst.ID] = cached
		}
		if cached.err != nil {
			fmt.Fprintf(out, "FAILED %s: %v\n", candidate.RelPath, cached.err)
			failed++
			continue
		}
		if err := a.db.BeginOutgoingAttempt(row.MessageKey); err != nil {
			fmt.Fprintf(out, "FAILED %s: %v\n", candidate.RelPath, err)
			failed++
			continue
		}
		receipt, err := cached.sender.Send(ctx, draft, row.PreparedAt)
		if err != nil {
			_ = a.db.FailOutgoing(row.MessageKey, err.Error())
			fmt.Fprintf(out, "FAILED %s via %s: %v\n", candidate.RelPath, inst.ID, err)
			failed++
			continue
		}
		if receipt.SentAt.IsZero() {
			receipt.SentAt = row.PreparedAt
		}
		archiveRel, err := outbox.ArchiveRel(receipt.SentAt, row.DraftRelPath, row.MessageKey)
		if err == nil {
			err = a.db.MarkOutgoingSent(row.MessageKey, receipt.ProviderID, archiveRel, receipt.SentAt)
		}
		if err == nil {
			_, err = outbox.FinishArchive(sendDir, archivedDir, row.DraftRelPath, archiveRel, row.ContentHash)
		}
		if err == nil {
			err = a.db.MarkOutgoingArchived(row.MessageKey)
		}
		if err != nil {
			fmt.Fprintf(out, "FAILED %s after provider acceptance: %v (the ledger prevents an automatic duplicate)\n", candidate.RelPath, err)
			failed++
			continue
		}
		reconciled := ""
		if receipt.Reconciled {
			reconciled = " (reconciled prior delivery)"
		}
		fmt.Fprintf(out, "sent %s via %s -> archived/%s%s\n", candidate.RelPath, inst.ID, archiveRel, reconciled)
		sent++
	}

	if dryRun {
		fmt.Fprintf(out, "dry run: %d valid, %d invalid\n", len(candidates)-failed, failed)
	} else {
		fmt.Fprintf(out, "send summary: %d sent, %d recovered, %d failed\n", sent, recovered, failed)
	}
	if failed > 0 {
		return fmt.Errorf("%d outgoing message operation(s) failed; successful messages were archived", failed)
	}
	return nil
}

func resolveOutboundInstance(cfg *config.Config, d outbox.Draft) (config.Instance, error) {
	switch d.Kind {
	case outbox.KindEmail:
		if acct, ok := cfg.GoogleByLabel(d.Account); ok {
			if !acct.SendEmail {
				return config.Instance{}, fmt.Errorf("account %q is Google but send_email is false; enable it and run `comms auth google %s`", d.Account, d.Account)
			}
			return config.Instance{ID: state.InstanceID(state.SourceGmail, acct.Label), Kind: state.SourceGmail, Label: acct.Label, Account: acct.Account}, nil
		}
		if acct, ok := cfg.FastMailByLabel(d.Account); ok {
			if !acct.SendEmail {
				return config.Instance{}, fmt.Errorf("account %q is FastMail but send_email is false; enable it and replace the read-only token with a write/send token", d.Account)
			}
			return config.Instance{ID: state.InstanceID(state.SourceFastmail, acct.Label), Kind: state.SourceFastmail, Label: acct.Label, Account: acct.Account}, nil
		}
	case outbox.KindChat:
		if acct, ok := cfg.GoogleByLabel(d.Account); ok {
			if !acct.SendChat {
				return config.Instance{}, fmt.Errorf("account %q has send_chat = false; enable it and run `comms auth google %s`", d.Account, d.Account)
			}
			return config.Instance{ID: state.InstanceID(state.SourceGChat, acct.Label), Kind: state.SourceGChat, Label: acct.Label, Account: acct.Account}, nil
		}
	}
	return config.Instance{}, fmt.Errorf("no send-enabled %s account has label %q", d.Kind, d.Account)
}

func buildOutboundSender(ctx context.Context, a *sendApp, inst config.Instance) (outbox.Sender, error) {
	switch inst.Kind {
	case state.SourceGmail:
		acct, _ := a.cfg.GoogleByLabel(inst.Label)
		ts, err := googleTokenSource(ctx, acct)
		if err != nil {
			return nil, err
		}
		svc, err := gmailapi.NewService(ctx, option.WithHTTPClient(cappedGoogleClient(ctx, ts)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", inst.ID, err)
		}
		return gmailsender.New(acct, svc), nil
	case state.SourceGChat:
		acct, _ := a.cfg.GoogleByLabel(inst.Label)
		ts, err := googleTokenSource(ctx, acct)
		if err != nil {
			return nil, err
		}
		svc, err := chat.NewService(ctx, option.WithHTTPClient(cappedGoogleClient(ctx, ts)))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", inst.ID, err)
		}
		return gchatsender.New(svc), nil
	case state.SourceFastmail:
		acct, _ := a.cfg.FastMailByLabel(inst.Label)
		if acct.Token == "" {
			if err := paths.CheckCredentialPerms(acct.TokenFilePath); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("%s: no FastMail token at %s — run `comms auth fastmail %s`", inst.ID, acct.TokenFilePath, acct.Label)
				}
				return nil, err
			}
		}
		tokens := sourcefastmail.FileTokenSource(acct.Token, acct.TokenFilePath)
		return fastmailsender.New(acct, tokens), nil
	default:
		return nil, fmt.Errorf("unsupported outbound provider %q", inst.Kind)
	}
}

func cappedGoogleClient(ctx context.Context, ts oauth2.TokenSource) *http.Client {
	hc := oauth2.NewClient(ctx, ts)
	hc.Transport = source.CapResponseBody(hc.Transport, sendResponseBodyCap)
	return hc
}

func describeDraft(d outbox.Draft) string {
	if d.Kind == outbox.KindChat {
		if d.Thread != "" {
			return "chat reply to " + d.Thread
		}
		return "chat to " + d.Space
	}
	return fmt.Sprintf("email %q to %d recipient(s)", d.Subject, len(d.To)+len(d.CC)+len(d.BCC))
}
