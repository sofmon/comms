// Package daemon schedules the sync loops for `save run`: one goroutine per
// source INSTANCE — one account's one source kind, e.g. "gmail:work" — each
// with an independent jittered interval (a multi-hour Gmail backfill on one
// account must never delay another account, nor the 2-minute Chat cadence,
// since history-off spaces retain messages only 24h), context-driven
// shutdown that waits for in-flight passes, and slow probing for instances
// whose auth is broken. Runners are opaque to the daemon: it never groups or
// compares them by kind, so two accounts of one kind are simply two runners.
package daemon

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"save/internal/retry"
)

// DegradedMultiplier scales a runner's interval while its auth is broken:
// re-running a full sync pass against dead credentials is pointless, so the
// instance is probed with its cheap Check every Interval×10 instead. One
// account's broken token degrades only that account's runner.
const DegradedMultiplier = 10

// Runner is one scheduled source instance. Name is its instance id
// ("gmail:work"), which every log line of this loop carries. Pass is the
// shared single-pass sync closure (the same code path `save sync` runs
// once); Check is the cheap auth probe used while the instance is degraded.
type Runner struct {
	Name     string
	Interval time.Duration
	Pass     func(ctx context.Context) error
	Check    func(ctx context.Context) error
}

// Run drives every runner in its own goroutine and blocks until ctx is
// canceled and every in-flight pass has returned. The caller owns startup
// healing and signal wiring; canceling ctx is the only stop mechanism.
func Run(ctx context.Context, runners []Runner, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	var wg sync.WaitGroup
	for _, r := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx, r, log.With("source", r.Name))
		}()
	}
	wg.Wait()
}

func loop(ctx context.Context, r Runner, log *slog.Logger) {
	// Initial jitter (up to 10% of the interval) desynchronizes sources
	// started together so their first passes don't hit the network at once.
	if !sleepCtx(ctx, time.Duration(rand.Float64()*0.1*float64(r.Interval))) {
		return
	}
	for {
		start := time.Now()
		err := r.Pass(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
			log.Debug("sync pass ok", "took", time.Since(start).Round(time.Millisecond))
		case retry.Classify(err) == retry.AuthBroken && r.Check != nil:
			// Log the degradation once at Error; the per-probe failures
			// below log at Debug so a dead token doesn't flood the log.
			log.Error("auth is broken for this account; probing slowly until it recovers — re-authorize this instance's account to fix it",
				"probe_interval", r.Interval*DegradedMultiplier, "err", err)
			if !probeUntilHealthy(ctx, r, log) {
				return
			}
			log.Info("auth recovered; resuming normal syncs")
			continue // run a pass immediately after recovery
		default:
			log.Error("sync pass failed; will retry next interval", "err", err)
		}
		if !sleepCtx(ctx, jitter(r.Interval)) {
			return
		}
	}
}

// probeUntilHealthy runs Check every jittered Interval×DegradedMultiplier
// until it succeeds; it reports false when ctx ended first.
func probeUntilHealthy(ctx context.Context, r Runner, log *slog.Logger) bool {
	for {
		if !sleepCtx(ctx, jitter(r.Interval*DegradedMultiplier)) {
			return false
		}
		err := r.Check(ctx)
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			return true
		}
		log.Debug("degraded probe still failing", "err", err)
	}
}

// jitter returns d ±10%, so loops with equal intervals drift apart instead
// of thundering together.
func jitter(d time.Duration) time.Duration {
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

// sleepCtx waits d (or returns immediately for d <= 0) and reports whether
// the caller should keep running (false: ctx ended during the wait).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
