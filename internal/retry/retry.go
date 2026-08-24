// Package retry classifies connector errors into retry classes and runs
// retriable operations with jittered exponential backoff. Classification
// covers Google API errors (googleapi.Error), OAuth token-endpoint errors,
// network failures, and context errors; connectors reinterpret ambiguous
// statuses (e.g. history.list 404 means the cursor expired, not that an
// item vanished) by wrapping with MarkCursorGone/MarkAuthBroken.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

// Class is the retry class of an error.
type Class int

const (
	// Transient errors are worth retrying with backoff: 5xx, network
	// failures, timeouts.
	Transient Class = iota
	// CursorGone means a sync cursor/anchor is no longer valid; the
	// caller must re-enumerate from scratch. Only produced via
	// MarkCursorGone.
	CursorGone
	// AuthBroken means credentials were rejected; retrying without
	// re-authentication is pointless.
	AuthBroken
	// ItemGone means the individual item vanished (404); skip it —
	// never run-fatal.
	ItemGone
	// RateLimited means quota or rate exceeded; retry after backing
	// off, honoring Retry-After when present.
	RateLimited
	// Fatal errors are not retriable: cancellation, programming
	// errors, and anything unrecognized.
	Fatal
)

var classNames = [...]string{"Transient", "CursorGone", "AuthBroken", "ItemGone", "RateLimited", "Fatal"}

func (c Class) String() string {
	if c < 0 || int(c) >= len(classNames) {
		return fmt.Sprintf("Class(%d)", int(c))
	}
	return classNames[c]
}

// Sentinels detectable with errors.Is. Connectors attach them to an
// underlying error via MarkCursorGone/MarkAuthBroken; the underlying
// error stays reachable through errors.As/Is.
var (
	ErrCursorGone = errors.New("sync cursor gone")
	ErrAuthBroken = errors.New("auth broken")
)

// MarkCursorGone wraps err so Classify returns CursorGone. Used by
// connectors to reinterpret responses (Gmail history.list 404, JMAP
// anchor destroyed). MarkCursorGone(nil) returns the bare sentinel.
func MarkCursorGone(err error) error { return mark(ErrCursorGone, err) }

// MarkAuthBroken wraps err so Classify returns AuthBroken.
// MarkAuthBroken(nil) returns the bare sentinel.
func MarkAuthBroken(err error) error { return mark(ErrAuthBroken, err) }

func mark(sentinel, err error) error {
	if err == nil {
		return sentinel
	}
	return &markedError{sentinel: sentinel, err: err}
}

type markedError struct {
	sentinel error
	err      error
}

func (m *markedError) Error() string   { return m.sentinel.Error() + ": " + m.err.Error() }
func (m *markedError) Unwrap() []error { return []error{m.sentinel, m.err} }

// Classify maps err to its retry class. Explicit sentinel marks win over
// everything else; context.Canceled is Fatal (propagate shutdown) while
// context.DeadlineExceeded is Transient (a per-call timeout). Unknown
// errors are Fatal: retrying what we don't understand hides bugs.
// Classify(nil) returns Fatal — there is nothing to retry.
func Classify(err error) Class {
	if err == nil {
		return Fatal
	}
	if errors.Is(err, ErrCursorGone) {
		return CursorGone
	}
	if errors.Is(err, ErrAuthBroken) {
		return AuthBroken
	}
	if errors.Is(err, context.Canceled) {
		return Fatal
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Transient
	}

	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		switch {
		case gerr.Code == 401:
			return AuthBroken
		case gerr.Code == 429:
			return RateLimited
		case gerr.Code == 403 && hasRateReason(gerr):
			return RateLimited
		case gerr.Code == 404:
			return ItemGone
		case gerr.Code >= 500:
			return Transient
		}
		return Fatal
	}

	var rerr *oauth2.RetrieveError
	if errors.As(err, &rerr) {
		switch {
		case rerr.ErrorCode == "invalid_grant":
			return AuthBroken
		case rerr.Response != nil && rerr.Response.StatusCode == 401:
			return AuthBroken
		case rerr.Response != nil && rerr.Response.StatusCode == 429:
			return RateLimited
		case rerr.Response != nil && rerr.Response.StatusCode >= 500:
			return Transient
		}
		return Fatal
	}
	// Token errors sometimes surface only as text inside a wrapper that
	// drops the typed error.
	if strings.Contains(err.Error(), "invalid_grant") {
		return AuthBroken
	}

	var nerr net.Error
	if errors.As(err, &nerr) {
		return Transient
	}
	if errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return Transient
	}

	return Fatal
}

// hasRateReason reports whether a 403 is a rate/quota rejection rather
// than a permission denial. Legacy responses carry the reason in
// Errors[].Reason; newer ones only in the JSON body.
func hasRateReason(e *googleapi.Error) bool {
	for _, item := range e.Errors {
		switch item.Reason {
		case "rateLimitExceeded", "userRateLimitExceeded":
			return true
		}
	}
	return strings.Contains(e.Body, "rateLimitExceeded") ||
		strings.Contains(e.Body, "userRateLimitExceeded")
}

// RetryAfter extracts a server-requested wait from the error's
// Retry-After header (integer seconds, or an HTTP-date). ok is false when
// the error carries no usable header.
func RetryAfter(err error) (d time.Duration, ok bool) {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Header == nil {
		return 0, false
	}
	v := gerr.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	if secs, perr := strconv.Atoi(v); perr == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, perr := http.ParseTime(v); perr == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// Options controls Do. The zero value selects the defaults noted on each
// field.
type Options struct {
	// MaxAttempts is the total number of fn calls, including the first
	// (default 5).
	MaxAttempts int
	// Budget bounds total elapsed time including waits; Do gives up
	// rather than start a sleep that would cross it (default 15m).
	Budget time.Duration
	// BaseDelay is the first backoff ceiling; each attempt doubles it
	// (default 1s).
	BaseDelay time.Duration
	// MaxDelay caps the exponential term (default 2m). A larger
	// Retry-After is still honored.
	MaxDelay time.Duration

	// Test seams; nil selects the real clock/timer/PRNG.
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
	rnd   func() float64
}

func (o Options) withDefaults() Options {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.Budget <= 0 {
		o.Budget = 15 * time.Minute
	}
	if o.BaseDelay <= 0 {
		o.BaseDelay = time.Second
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = 2 * time.Minute
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.rnd == nil {
		o.rnd = rand.Float64
	}
	return o
}

// Do runs fn, retrying Transient and RateLimited failures with
// full-jitter exponential backoff (delay uniform in [0, min(MaxDelay,
// BaseDelay*2^(attempt-1))]), waiting at least any server-provided
// Retry-After. It returns fn's last error unchanged when attempts or
// budget run out, immediately on any other class, and ctx's error if the
// context ends first.
func Do(ctx context.Context, opts Options, fn func() error) error {
	opts = opts.withDefaults()
	start := opts.now()
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		switch Classify(err) {
		case Transient, RateLimited:
		default:
			return err
		}
		if attempt >= opts.MaxAttempts {
			return err
		}
		delay := backoffDelay(attempt, opts.BaseDelay, opts.MaxDelay, opts.rnd)
		if ra, ok := RetryAfter(err); ok && ra > delay {
			delay = ra
		}
		if opts.now().Sub(start)+delay > opts.Budget {
			return err
		}
		if serr := opts.sleep(ctx, delay); serr != nil {
			return serr
		}
	}
}

// backoffDelay returns a full-jitter delay for the given 1-based attempt.
func backoffDelay(attempt int, base, max time.Duration, rnd func() float64) time.Duration {
	ceil := float64(base) * math.Pow(2, float64(attempt-1))
	if m := float64(max); ceil > m {
		ceil = m
	}
	return time.Duration(rnd() * ceil)
}

// sleepCtx waits d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
