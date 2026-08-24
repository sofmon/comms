// Package ratelimit provides the two limiter shapes the connectors need:
// a quota-cost limiter for Gmail, where API calls cost different quota
// units against one per-user budget, and a keyed limiter for Google Chat,
// where each space has its own read cap plus an overall ceiling.
package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// Units is a quota-unit limiter. Gmail enforces a per-user quota of 6,000
// units/min; callers construct this with a safety-margined per-second
// budget (~83 units/s) and pass each call's documented cost
// (messages.get 20, messages.list 5, history.list 2, getProfile 1).
type Units struct {
	l *rate.Limiter
}

// NewUnits returns a limiter that refills unitsPerSecond with the given
// burst capacity. burst must be at least the largest single cost callers
// will pass, or those calls will always fail.
func NewUnits(unitsPerSecond float64, burst int) *Units {
	return &Units{l: rate.NewLimiter(rate.Limit(unitsPerSecond), burst)}
}

// Wait blocks until cost units are available or ctx is done. It fails
// immediately (without consuming quota) when cost exceeds the burst
// capacity or ctx is already done.
func (u *Units) Wait(ctx context.Context, cost int) error {
	return u.l.WaitN(ctx, cost)
}

// Keyed is a set of lazily created per-key limiters plus one overall
// limiter; Wait blocks on both. Chat uses one key per space (~10 req/s
// per space under the documented 15/s per-space cap) with an overall
// ceiling across all spaces.
type Keyed struct {
	perKeyRate  rate.Limit
	perKeyBurst int
	overall     *rate.Limiter

	mu  sync.Mutex
	per map[string]*rate.Limiter
}

// NewKeyed returns a keyed limiter. Each key gets its own limiter at
// perKeyPerSecond/perKeyBurst, created on first use. overallPerSecond <= 0
// disables the overall cap. Bursts below 1 are clamped to 1 so a single
// Wait can always succeed.
func NewKeyed(perKeyPerSecond float64, perKeyBurst int, overallPerSecond float64, overallBurst int) *Keyed {
	if perKeyBurst < 1 {
		perKeyBurst = 1
	}
	ovRate := rate.Inf
	if overallPerSecond > 0 {
		ovRate = rate.Limit(overallPerSecond)
		if overallBurst < 1 {
			overallBurst = 1
		}
	}
	return &Keyed{
		perKeyRate:  rate.Limit(perKeyPerSecond),
		perKeyBurst: perKeyBurst,
		overall:     rate.NewLimiter(ovRate, overallBurst),
		per:         make(map[string]*rate.Limiter),
	}
}

// Wait blocks until both the key's limiter and the overall limiter grant
// one token, or ctx is done.
func (k *Keyed) Wait(ctx context.Context, key string) error {
	if err := k.limiter(key).Wait(ctx); err != nil {
		return err
	}
	return k.overall.Wait(ctx)
}

// limiter returns the limiter for key, creating it on first use.
func (k *Keyed) limiter(key string) *rate.Limiter {
	k.mu.Lock()
	defer k.mu.Unlock()
	l, ok := k.per[key]
	if !ok {
		l = rate.NewLimiter(k.perKeyRate, k.perKeyBurst)
		k.per[key] = l
	}
	return l
}
