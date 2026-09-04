// Package cli wires the cobra command tree: init, auth google|fastmail,
// sync, run, status, refetch, triage, doctor, verify, version. Shared construction (config,
// lock, state DB, timezone pin, writer, sources) lives in app.go; the
// reporting commands read the state database read-only and take no lock, so
// they work beside a running daemon.
//
// Everything below the config is keyed by INSTANCE — one account's one
// source kind, e.g. "gmail:work". buildSources turns cfg.Instances() into
// one connector each, and sync, run, status and doctor all iterate over
// instances rather than over the three source kinds.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

const version = "0.1.0-dev"

func newRoot() *cobra.Command {
	var verbose bool
	root := &cobra.Command{
		Use:           "comms",
		Short:         "comms archives Gmail, Google Chat, and FastMail locally as Markdown",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if verbose {
				logLevel.Set(slog.LevelDebug)
			}
		},
	}
	root.PersistentFlags().BoolVar(&verbose, "verbose", false, "enable debug logging")
	root.AddCommand(
		newInitCmd(),
		newAuthCmd(),
		newSyncCmd(),
		newRunCmd(),
		newStatusCmd(),
		newRefetchCmd(),
		newTriageCmd(),
		newUntriageCmd(),
		newDoctorCmd(),
		newVerifyCmd(),
		newVersionCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the comms version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "comms", version)
		},
	}
}

// signalContext returns a context canceled by SIGINT or SIGTERM; the write
// ordering (files first, DB second, cursor last) makes cancellation safe at
// any point.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
