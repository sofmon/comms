package daemon_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"save/internal/daemon"
	"save/internal/retry"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunLoopsAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var passes atomic.Int32
	r := daemon.Runner{
		Name:     "x",
		Interval: 2 * time.Millisecond,
		Pass: func(ctx context.Context) error {
			if passes.Add(1) >= 3 {
				cancel()
			}
			return nil
		},
		Check: func(ctx context.Context) error { return nil },
	}

	done := make(chan struct{})
	go func() {
		daemon.Run(ctx, []daemon.Runner{r}, quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after ctx cancellation")
	}
	if got := passes.Load(); got < 3 {
		t.Errorf("passes = %d, want >= 3", got)
	}
}

func TestDegradedSourceProbesUntilRecovered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var passes, checks atomic.Int32
	authErr := retry.MarkAuthBroken(errors.New("token revoked"))
	r := daemon.Runner{
		Name:     "x",
		Interval: time.Millisecond,
		Pass: func(ctx context.Context) error {
			switch passes.Add(1) {
			case 1:
				return authErr // enter degraded mode
			default:
				cancel() // recovered pass ran; stop the daemon
				return nil
			}
		},
		Check: func(ctx context.Context) error {
			if checks.Add(1) < 3 {
				return authErr // still broken for the first probes
			}
			return nil
		},
	}

	done := make(chan struct{})
	go func() {
		daemon.Run(ctx, []daemon.Runner{r}, quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop; degraded probing never recovered")
	}
	if got := checks.Load(); got < 3 {
		t.Errorf("checks = %d, want >= 3 (probing must repeat until Check succeeds)", got)
	}
	if got := passes.Load(); got < 2 {
		t.Errorf("passes = %d, want >= 2 (a pass must run immediately after recovery)", got)
	}
}

// TestInstancesOfOneKindRunIndependently: two accounts of the same source
// kind are two runners. One account's broken auth must degrade only its own
// loop — the sibling keeps its normal cadence.
func TestInstancesOfOneKindRunIndependently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var workPasses, personalPasses, personalChecks atomic.Int32
	authErr := retry.MarkAuthBroken(errors.New("token revoked"))
	runners := []daemon.Runner{
		{
			Name:     "gmail:work",
			Interval: time.Millisecond,
			Pass: func(ctx context.Context) error {
				// Stop once the healthy loop has clearly kept running while
				// its degraded sibling was stuck probing.
				if workPasses.Add(1) >= 5 && personalChecks.Load() >= 2 {
					cancel()
				}
				return nil
			},
			Check: func(ctx context.Context) error {
				t.Error("healthy instance must not be probed")
				return nil
			},
		},
		{
			Name:     "gmail:personal",
			Interval: time.Millisecond,
			Pass: func(ctx context.Context) error {
				personalPasses.Add(1)
				return authErr // permanently degraded
			},
			Check: func(ctx context.Context) error {
				personalChecks.Add(1)
				return authErr
			},
		},
	}

	done := make(chan struct{})
	go func() {
		daemon.Run(ctx, runners, quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop; the healthy instance was blocked by the degraded one")
	}
	if got := workPasses.Load(); got < 5 {
		t.Errorf("healthy instance passes = %d, want >= 5", got)
	}
	// The degraded instance dropped into probing after its first pass rather
	// than re-running full syncs against dead credentials.
	if got := personalPasses.Load(); got != 1 {
		t.Errorf("degraded instance passes = %d, want 1", got)
	}
	if personalChecks.Load() == 0 {
		t.Error("degraded instance was never probed")
	}
}

func TestNonAuthErrorKeepsNormalCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var passes atomic.Int32
	r := daemon.Runner{
		Name:     "x",
		Interval: time.Millisecond,
		Pass: func(ctx context.Context) error {
			if passes.Add(1) >= 3 {
				cancel()
			}
			return errors.New("transient-ish failure") // not AuthBroken
		},
		Check: func(ctx context.Context) error {
			t.Error("Check must not be called for non-auth errors")
			return nil
		},
	}

	done := make(chan struct{})
	go func() {
		daemon.Run(ctx, []daemon.Runner{r}, quiet())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not keep looping after a plain error")
	}
}
