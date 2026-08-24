package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"save/internal/daemon"
)

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the daemon: continuous per-account sync loops until SIGINT/SIGTERM",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemon()
		},
	}
}

func runDaemon() error {
	ctx, stop := signalContext()
	defer stop()

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.Close()

	if err := a.heal(ctx); err != nil {
		return err
	}
	srcs, err := a.buildSources(ctx, nil)
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		return fmt.Errorf("no accounts configured in %s", a.cfgPath)
	}

	// One goroutine per INSTANCE, each on its kind's global interval: a
	// multi-hour Gmail backfill on one account must not delay any other
	// account, nor the 2-minute Chat cadence.
	runners := make([]daemon.Runner, 0, len(srcs))
	for _, s := range srcs {
		conn := s.conn
		interval := a.cfg.Interval(s.inst.Kind)
		// A non-positive interval would spin the loop; config validation
		// rules it out, so refuse loudly rather than burn a core.
		if interval <= 0 {
			return fmt.Errorf("%s: no daemon interval is configured for source kind %q", s.inst.ID, s.inst.Kind)
		}
		runners = append(runners, daemon.Runner{
			Name:     s.inst.ID,
			Interval: interval,
			// Each loop iteration is the exact single pass `save sync` runs.
			Pass:  func(ctx context.Context) error { return a.runSourcePass(ctx, conn) },
			Check: conn.Check,
		})
		a.log.Info("scheduling source", "source", s.inst.ID, "account", s.inst.Account, "interval", interval)
	}
	a.log.Info("daemon running; stop with Ctrl-C or SIGTERM")
	daemon.Run(ctx, runners, a.log)
	a.log.Info("daemon stopped")
	return nil
}
