package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var (
		only        []string
		full        bool
		retryFailed bool
	)
	c := &cobra.Command{
		Use:   "sync",
		Short: "Run one sync pass over the configured accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(only, full, retryFailed)
		},
	}
	c.Flags().StringArrayVar(&only, "source", nil,
		"sync only this source: a kind (gmail), an instance id (gmail:work), or an account label (work); repeatable")
	c.Flags().BoolVar(&full, "full", false, "clear the selected instances' cursors first: full re-enumeration (dedup prevents duplicate files)")
	c.Flags().BoolVar(&retryFailed, "retry-failed", false, "forget recorded per-item failures so previously skipped items are retried")
	return c
}

func runSync(only []string, full, retryFailed bool) error {
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
	srcs, err := a.buildSources(ctx, only)
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		return fmt.Errorf("no accounts configured in %s", a.cfgPath)
	}

	if full {
		for _, s := range srcs {
			if err := a.clearCursors(s.inst.ID); err != nil {
				return err
			}
		}
	}
	if retryFailed {
		ids := make([]string, len(srcs))
		for i, s := range srcs {
			ids[i] = s.inst.ID
		}
		if err := a.clearFailures(ids); err != nil {
			return err
		}
	}

	var errs []error
	for _, s := range srcs {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		a.log.Info("sync starting", "source", s.inst.ID, "account", s.inst.Account)
		if err := a.runSourcePass(ctx, s.conn); err != nil {
			a.log.Error("sync failed", "source", s.inst.ID, "account", s.inst.Account, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", s.inst.ID, err))
			continue
		}
		a.log.Info("sync finished", "source", s.inst.ID, "account", s.inst.Account)
	}
	return errors.Join(errs...)
}
