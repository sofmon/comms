package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/google/renameio/v2"
	"github.com/spf13/cobra"

	"comms/internal/config"
	"comms/internal/googleauth"
	"comms/internal/paths"
	"comms/internal/source/fastmail"
	"comms/internal/state"
)

// Setup walkthroughs, printed by auth failures and `comms doctor`. Render
// them with googleSetup / fastmailSetup so the account's own paths, label
// and env variable appear in the text.
const googleSetupHintFmt = `Google Cloud Console setup for account %[1]q (%[2]s):
  1. console.cloud.google.com → create (or pick) a project
  2. APIs & Services → Library → enable the "Gmail API" and the "Google Chat API"
  3. OAuth consent screen → audience "Internal" (Workspace account; no
     verification and no 7-day token expiry). IMPORTANT for multiple
     accounts: an "Internal" client only accepts users of ITS OWN Workspace
     organization, so a second [[google]] account in a different org cannot
     reuse this client. Give that account its own Cloud project + OAuth
     client and point its client_file at the downloaded JSON (or publish an
     "External" client, which brings verification and 7-day refresh-token
     expiry back). Note: a Workspace admin can block unlisted OAuth clients
     from the Chat scopes.
  4. Credentials → Create credentials → OAuth client ID → type "Desktop app"
  5. Download the client JSON, save it as %[3]s, chmod 600 it
  Then run: comms auth google %[1]s`

// googleSetup renders the walkthrough for one account.
func googleSetup(acct config.GoogleAccount) string {
	return fmt.Sprintf(googleSetupHintFmt, acct.Label, acct.Account, acct.ClientFilePath)
}

const fastmailSetupHintFmt = `FastMail token setup for account %[1]q (%[2]s):
  1. FastMail web → Settings → Privacy & Security → API tokens → New token
  2. Type: JMAP; scope: read-only. The token is shown exactly once — paste it
     here right away. (API tokens are not available on Basic plans; there is
     no JMAP fallback for those.)
  Then run: comms auth fastmail %[1]s`

func fastmailSetup(acct config.FastMailAccount) string {
	return fmt.Sprintf(fastmailSetupHintFmt, acct.Label, acct.Account)
}

func newAuthCmd() *cobra.Command {
	auth := &cobra.Command{
		Use:   "auth",
		Short: "Authorize comms with one configured account",
	}
	auth.AddCommand(newAuthGoogleCmd(), newAuthFastmailCmd())
	return auth
}

// resolveAuthLabels picks which configured accounts an `comms auth` run
// applies to. block is the config block name ("google"/"fastmail"), labels
// every configured label of that kind in config order, arg the optional
// positional label, and all the --all switch (google only; pass hasAll=false
// for commands that do not offer it). Omitting the label is allowed only
// when exactly one account of that kind exists.
func resolveAuthLabels(block string, labels []string, arg string, all, hasAll bool) ([]string, error) {
	switch {
	case len(labels) == 0:
		return nil, fmt.Errorf("no [[%s]] account is configured in %s", block, config.DefaultPath())
	case all && arg != "":
		return nil, fmt.Errorf("pass either a label or --all to `comms auth %s`, not both", block)
	case all:
		return labels, nil
	case arg != "":
		for _, l := range labels {
			if l == arg {
				return []string{l}, nil
			}
		}
		return nil, fmt.Errorf("no [[%s]] account is labelled %q — configured labels: %s",
			block, arg, strings.Join(labels, ", "))
	case len(labels) == 1:
		return labels, nil
	}
	allHint := ""
	if hasAll {
		allHint = " or pass --all to do every one in turn"
	}
	return nil, fmt.Errorf("several [[%s]] accounts are configured — name the one to authorize (`comms auth %s <label>`)%s; configured labels: %s",
		block, block, allHint, strings.Join(labels, ", "))
}

func googleLabels(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Google))
	for _, g := range cfg.Google {
		out = append(out, g.Label)
	}
	return out
}

func fastmailLabels(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.FastMail))
	for _, f := range cfg.FastMail {
		out = append(out, f.Label)
	}
	return out
}

func newAuthGoogleCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "google [label]",
		Short: "Run the Google OAuth desktop flow for one [[google]] account (Gmail + Google Chat)",
		Long: "Run the Google OAuth desktop flow for one [[google]] account.\n\n" +
			"One consent covers that account's Gmail and Chat. The label may be omitted\n" +
			"only when a single [[google]] account is configured; --all authorizes every\n" +
			"configured account in turn.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			return runAuthGoogle(cmd, arg, all)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "authorize every configured [[google]] account in turn")
	return c
}

func runAuthGoogle(cmd *cobra.Command, arg string, all bool) error {
	out := cmd.OutOrStdout()
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	labels, err := resolveAuthLabels("google", googleLabels(cfg), arg, all, true)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	for i, label := range labels {
		acct, ok := cfg.GoogleByLabel(label)
		if !ok {
			return fmt.Errorf("no [[google]] block labelled %q", label)
		}
		if i > 0 {
			fmt.Fprintln(out)
		}
		if err := authorizeGoogle(ctx, out, acct); err != nil {
			return fmt.Errorf("account %q (%s): %w", acct.Label, acct.Account, err)
		}
	}
	return nil
}

// authorizeGoogle runs one account's consent flow. Scope drift is evaluated
// against that account's own token file, so two accounts never confuse each
// other's grants.
func authorizeGoogle(ctx context.Context, out io.Writer, acct config.GoogleAccount) error {
	fmt.Fprintf(out, "Authorizing [[google]] %q — account %s\n", acct.Label, acct.Account)
	fmt.Fprintf(out, "  OAuth client: %s\n", acct.ClientFilePath)
	fmt.Fprintf(out, "  Token file:   %s\n", acct.TokenFilePath)
	if _, err := os.Stat(acct.ClientFilePath); err != nil {
		return fmt.Errorf("no OAuth client file at %s\n\n%s", acct.ClientFilePath, googleSetup(acct))
	}
	scopes := googleScopes(acct)
	if len(scopes) == 0 {
		// Validate rejects this, so it can only mean the config changed
		// under us; refuse rather than store a scopeless grant.
		return errors.New("this account archives neither gmail nor chat — nothing to authorize")
	}
	fmt.Fprintf(out, "Requesting scopes:\n")
	for _, s := range scopes {
		fmt.Fprintf(out, "  %s\n", s)
	}

	// Tell the user why re-auth is (or isn't) needed before the browser opens.
	switch missing, derr := googleauth.ScopeDrift(acct.TokenFilePath, scopes); {
	case derr == nil && len(missing) == 0:
		fmt.Fprintln(out, "An existing grant already covers these scopes; re-authorizing refreshes it.")
	case derr == nil:
		fmt.Fprintf(out, "The stored grant is missing %v — re-consent is required.\n", missing)
	case errors.Is(derr, googleauth.ErrNeedsAuth):
		fmt.Fprintln(out, "No usable stored token; starting a fresh authorization.")
	default:
		return derr
	}
	fmt.Fprintf(out, "Sign in as %s on the consent screen — signing in as a different\n"+
		"account would archive the wrong mailbox under label %q (`comms doctor` catches it later).\n",
		acct.Account, acct.Label)

	if err := googleauth.Authenticate(ctx, acct.ClientFilePath, acct.TokenFilePath, scopes); err != nil {
		return err
	}
	// The consent screen lets users decline individual scopes; catch that
	// now instead of failing on the first Chat call.
	if missing, derr := googleauth.ScopeDrift(acct.TokenFilePath, scopes); derr == nil && len(missing) > 0 {
		return fmt.Errorf("authorization completed but the grant is missing %v — did you uncheck a permission on the consent screen? Re-run `comms auth google %s` and approve everything",
			missing, acct.Label)
	}
	fmt.Fprintf(out, "Google authorization for %s stored at %s (0600).\n", acct.Account, acct.TokenFilePath)
	return nil
}

func newAuthFastmailCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fastmail [label]",
		Short: "Store and verify a FastMail JMAP API token for one [[fastmail]] account",
		Long: "Store and verify a FastMail JMAP API token for one [[fastmail]] account.\n\n" +
			"The label may be omitted only when a single [[fastmail]] account is configured.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			return runAuthFastmail(cmd, arg)
		},
	}
}

func runAuthFastmail(cmd *cobra.Command, arg string) error {
	out := cmd.OutOrStdout()
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return err
	}
	labels, err := resolveAuthLabels("fastmail", fastmailLabels(cfg), arg, false, false)
	if err != nil {
		return err
	}
	acct, ok := cfg.FastMailByLabel(labels[0])
	if !ok {
		return fmt.Errorf("no [[fastmail]] block labelled %q", labels[0])
	}

	fmt.Fprintf(out, "Authorizing [[fastmail]] %q — account %s\n", acct.Label, acct.Account)
	fmt.Fprintln(out, "Create a read-only JMAP API token in the FastMail settings")
	fmt.Fprintln(out, "(Settings → Privacy & Security → API tokens → New token, type JMAP, read-only).")
	// golang.org/x/term is not among the pinned dependencies, so the prompt
	// cannot disable echo; say so instead of pretending. The env variable is
	// per label; the unsuffixed name works only with a single account.
	envVar := "COMMS_FASTMAIL_TOKEN_" + config.EnvSuffix(acct.Label)
	if len(cfg.FastMail) == 1 {
		envVar += " (or COMMS_FASTMAIL_TOKEN)"
	}
	fmt.Fprintln(out, "Note: input is NOT hidden here — paste in a private terminal, or export")
	fmt.Fprintf(out, "%s instead if you prefer to keep it out of this prompt.\n", envVar)
	fmt.Fprintf(out, "FastMail API token for %s: ", acct.Account)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return errors.New("empty token")
	}

	// Verify before storing: a single session GET with the token.
	src := fastmail.New(state.InstanceID(state.SourceFastmail, acct.Label), acct, nil, nil, nil, logger(),
		func() (string, error) { return token, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fmt.Fprintln(out, "Verifying the token against the FastMail JMAP session endpoint...")
	if err := src.Check(ctx); err != nil {
		return fmt.Errorf("token verification failed: %w\n\n%s", err, fastmailSetup(acct))
	}

	if err := paths.EnsureDir(paths.ConfigDir()); err != nil {
		return err
	}
	if err := writeToken(acct.TokenFilePath, token); err != nil {
		return err
	}
	fmt.Fprintf(out, "FastMail token for %s verified and stored at %s (0600).\n", acct.Account, acct.TokenFilePath)
	return nil
}

// writeToken persists the token atomically with static 0600 permissions.
func writeToken(path, token string) error {
	pf, err := renameio.NewPendingFile(path, renameio.WithStaticPermissions(0o600))
	if err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	defer pf.Cleanup() //nolint:errcheck // no-op after successful replace
	if _, err := pf.WriteString(token + "\n"); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	if err := pf.CloseAtomicallyReplace(); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

// credFileState classifies a credential file for doctor: present and
// locked down, present but too open, or absent.
func credFileState(path string) (ok bool, detail string) {
	err := paths.CheckCredentialPerms(path)
	switch {
	case err == nil:
		return true, "present, permissions ok"
	case errors.Is(err, fs.ErrNotExist):
		return false, "missing"
	default:
		return false, err.Error()
	}
}
