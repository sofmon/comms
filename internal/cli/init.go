package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"save/internal/config"
	"save/internal/paths"
	"save/internal/triage"
)

func newInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the config and state directories and a skeleton config.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd)
		},
	}
}

func runInit(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	cfgDir := paths.ConfigDir()
	stateDir := paths.StateDir()
	for _, d := range []string{cfgDir, stateDir} {
		if err := paths.EnsureDir(d); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "config dir: %s (0700)\n", cfgDir)
	fmt.Fprintf(out, "state dir:  %s (0700)\n", stateDir)

	cfgPath := config.DefaultPath()
	switch _, err := os.Stat(cfgPath); {
	case err == nil:
		fmt.Fprintf(out, "config:     %s already exists — left untouched\n", cfgPath)
	case errors.Is(err, fs.ErrNotExist):
		if err := config.WriteSkeleton(cfgPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "config:     wrote skeleton %s (0600)\n", cfgPath)
	default:
		return err
	}

	// The triage rules live in their own file so they can be edited without
	// touching credentials paths and account blocks.
	rulesPath := filepath.Join(cfgDir, "triage.toml")
	switch _, err := os.Stat(rulesPath); {
	case err == nil:
		fmt.Fprintf(out, "triage:     %s already exists — left untouched\n", rulesPath)
	case errors.Is(err, fs.ErrNotExist):
		if err := triage.WriteDefaultRules(rulesPath); err != nil {
			return err
		}
		fmt.Fprintf(out, "triage:     wrote starter rules %s (0600)\n", rulesPath)
	default:
		return err
	}

	fmt.Fprintf(out, `
Next steps:
  1. Edit %s: set archive_root, then keep one [[google]] block per Google
     identity (label + account, gmail/chat as needed) and one [[fastmail]]
     block per FastMail account; delete the example blocks you don't need.
     A label is permanent — it names every file that account writes and
     keys its sync state, so renaming one orphans that account's archive.
  2. Put your Google OAuth Desktop-app client JSON at
     %s/google-client.json and chmod 600 it
     (Cloud Console: enable the Gmail API and the Google Chat API, set the
     OAuth consent screen audience to "Internal", create a client of type
     "Desktop app" — `+"`save doctor`"+` re-prints this walkthrough).
     An "Internal" client only accepts accounts from its OWN Workspace
     organization: an account in a different org needs its own Cloud
     project and client JSON — point that block's client_file at it.
  3. save auth google <label>     # once per [[google]] account (or --all)
  4. save auth fastmail <label>   # once per [[fastmail]] account
  5. save sync        # first backfill; Gmail can take hours and is resumable
  6. save run         # daemon — see docs/launchd/com.user.save.plist for autostart
  7. save triage --dry-run   # later: see which notes the rules in triage.toml
                             # would file under spam_root, then run it for real
`, cfgPath, cfgDir)
	return nil
}
