package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
)

func TestClassify(t *testing.T) {
	gerr := func(code int) *googleapi.Error { return &googleapi.Error{Code: code} }
	tests := []struct {
		name string
		err  error
		want Class
	}{
		{"nil", nil, Fatal},
		{"plain error", errors.New("boom"), Fatal},
		{"context canceled", context.Canceled, Fatal},
		{"wrapped canceled", fmt.Errorf("call: %w", context.Canceled), Fatal},
		{"deadline exceeded", context.DeadlineExceeded, Transient},
		{"wrapped deadline", &url.Error{Op: "Get", URL: "https://x", Err: context.DeadlineExceeded}, Transient},

		{"googleapi 401", gerr(401), AuthBroken},
		{"googleapi 429", gerr(429), RateLimited},
		{"googleapi 403 rateLimitExceeded", &googleapi.Error{
			Code:   403,
			Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}},
		}, RateLimited},
		{"googleapi 403 userRateLimitExceeded", &googleapi.Error{
			Code:   403,
			Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}},
		}, RateLimited},
		{"googleapi 403 reason only in body", &googleapi.Error{
			Code: 403,
			Body: `{"error":{"errors":[{"reason":"userRateLimitExceeded"}]}}`,
		}, RateLimited},
		{"googleapi 403 permission denied", &googleapi.Error{
			Code:   403,
			Errors: []googleapi.ErrorItem{{Reason: "forbidden"}},
		}, Fatal},
		{"googleapi 404", gerr(404), ItemGone},
		{"wrapped googleapi 404", fmt.Errorf("messages.get: %w", gerr(404)), ItemGone},
		{"googleapi 400", gerr(400), Fatal},
		{"googleapi 500", gerr(500), Transient},
		{"googleapi 503 wrapped", fmt.Errorf("history.list: %w", gerr(503)), Transient},

		{"oauth2 invalid_grant", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, AuthBroken},
		{"oauth2 invalid_grant in url.Error", &url.Error{
			Op:  "Post",
			URL: "https://oauth2.googleapis.com/token",
			Err: &oauth2.RetrieveError{ErrorCode: "invalid_grant"},
		}, AuthBroken},
		{"oauth2 token endpoint 500", &oauth2.RetrieveError{
			Response: &http.Response{StatusCode: 500},
		}, Transient},
		{"oauth2 other", &oauth2.RetrieveError{ErrorCode: "invalid_scope"}, Fatal},
		{"invalid_grant text only", errors.New(`oauth2: "invalid_grant" "Token has been revoked"`), AuthBroken},

		{"dns timeout", &net.DNSError{Err: "timeout", Name: "gmail.googleapis.com", IsTimeout: true}, Transient},
		{"op error", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}, Transient},
		{"url error transport", &url.Error{Op: "Get", URL: "https://x", Err: errors.New("EOF")}, Transient},
		{"econnreset", syscall.ECONNRESET, Transient},
		{"wrapped econnrefused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), Transient},
		{"unexpected EOF", io.ErrUnexpectedEOF, Transient},

		{"marked cursor gone", MarkCursorGone(gerr(404)), CursorGone},
		{"marked cursor gone rewrapped", fmt.Errorf("gmail: %w", MarkCursorGone(gerr(404))), CursorGone},
		{"bare cursor sentinel", MarkCursorGone(nil), CursorGone},
		{"marked auth broken", MarkAuthBroken(errors.New("token file corrupt")), AuthBroken},
		{"bare auth sentinel", ErrAuthBroken, AuthBroken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestMarkPreservesUnderlying(t *testing.T) {
	under := &googleapi.Error{Code: 404}
	marked := MarkCursorGone(fmt.Errorf("history.list: %w", under))

	var gerr *googleapi.Error
	if !errors.As(marked, &gerr) || gerr.Code != 404 {
		t.Error("underlying googleapi.Error not reachable through mark")
	}
	if !errors.Is(marked, ErrCursorGone) {
		t.Error("errors.Is(marked, ErrCursorGone) = false")
	}
	if errors.Is(marked, ErrAuthBroken) {
		t.Error("errors.Is(marked, ErrAuthBroken) = true, want false")
	}
}

func TestRetryAfter(t *testing.T) {
	hdr := func(v string) http.Header { return http.Header{"Retry-After": []string{v}} }

	t.Run("seconds", func(t *testing.T) {
		err := &googleapi.Error{Code: 429, Header: hdr("7")}
		d, ok := RetryAfter(err)
		if !ok || d != 7*time.Second {
			t.Errorf("RetryAfter = %v, %v; want 7s, true", d, ok)
		}
	})
	t.Run("wrapped", func(t *testing.T) {
		err := fmt.Errorf("send: %w", &googleapi.Error{Code: 429, Header: hdr("3")})
		d, ok := RetryAfter(err)
		if !ok || d != 3*time.Second {
			t.Errorf("RetryAfter = %v, %v; want 3s, true", d, ok)
		}
	})
	t.Run("http date future", func(t *testing.T) {
		at := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
		d, ok := RetryAfter(&googleapi.Error{Code: 429, Header: hdr(at)})
		if !ok || d <= 0 || d > 11*time.Second {
			t.Errorf("RetryAfter = %v, %v; want ~10s, true", d, ok)
		}
	})
	t.Run("http date past", func(t *testing.T) {
		at := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
		d, ok := RetryAfter(&googleapi.Error{Code: 429, Header: hdr(at)})
		if !ok || d != 0 {
			t.Errorf("RetryAfter = %v, %v; want 0, true", d, ok)
		}
	})
	t.Run("absent", func(t *testing.T) {
		if _, ok := RetryAfter(&googleapi.Error{Code: 429}); ok {
			t.Error("RetryAfter without header: ok = true")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, ok := RetryAfter(&googleapi.Error{Code: 429, Header: hdr("soon")}); ok {
			t.Error("RetryAfter with garbage header: ok = true")
		}
	})
	t.Run("non-google error", func(t *testing.T) {
		if _, ok := RetryAfter(errors.New("boom")); ok {
			t.Error("RetryAfter on plain error: ok = true")
		}
	})
}

// fakeClock drives Do deterministically: now returns simulated time and
// sleep records the requested delay while advancing it.
type fakeClock struct {
	t     time.Time
	slept []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
	return nil
}

// fakeOpts wires the clock and a fixed jitter factor into o.
func fakeOpts(c *fakeClock, jitter float64, o Options) Options {
	o.sleep = c.sleep
	o.now = c.now
	o.rnd = func() float64 { return jitter }
	return o
}

func TestDoSuccessFirstTry(t *testing.T) {
	c := &fakeClock{}
	calls := 0
	err := Do(context.Background(), fakeOpts(c, 1, Options{}), func() error {
		calls++
		return nil
	})
	if err != nil || calls != 1 || len(c.slept) != 0 {
		t.Errorf("err=%v calls=%d sleeps=%v; want nil, 1, none", err, calls, c.slept)
	}
}

func TestDoRetriesTransientThenSucceeds(t *testing.T) {
	c := &fakeClock{}
	calls := 0
	err := Do(context.Background(), fakeOpts(c, 1, Options{}), func() error {
		calls++
		if calls < 3 {
			return &googleapi.Error{Code: 503}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	// jitter factor 1 → full backoff: 1s, then 2s.
	want := []time.Duration{time.Second, 2 * time.Second}
	if len(c.slept) != len(want) || c.slept[0] != want[0] || c.slept[1] != want[1] {
		t.Errorf("sleeps = %v, want %v", c.slept, want)
	}
}

func TestDoStopsImmediately(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"fatal", errors.New("boom")},
		{"auth broken", &googleapi.Error{Code: 401}},
		{"item gone", &googleapi.Error{Code: 404}},
		{"cursor gone", MarkCursorGone(&googleapi.Error{Code: 404})},
		{"canceled", context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &fakeClock{}
			calls := 0
			err := Do(context.Background(), fakeOpts(c, 1, Options{}), func() error {
				calls++
				return tt.err
			})
			if !errors.Is(err, tt.err) {
				t.Errorf("Do = %v, want %v", err, tt.err)
			}
			if calls != 1 || len(c.slept) != 0 {
				t.Errorf("calls=%d sleeps=%v; want 1 call, no sleeps", calls, c.slept)
			}
		})
	}
}

func TestDoDefaultMaxAttempts(t *testing.T) {
	c := &fakeClock{}
	calls := 0
	last := &googleapi.Error{Code: 503}
	err := Do(context.Background(), fakeOpts(c, 1, Options{}), func() error {
		calls++
		return last
	})
	if !errors.Is(err, last) {
		t.Errorf("Do = %v, want last transient error", err)
	}
	if calls != 5 {
		t.Errorf("calls = %d, want default MaxAttempts 5", calls)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if len(c.slept) != len(want) {
		t.Fatalf("sleeps = %v, want %v", c.slept, want)
	}
	for i := range want {
		if c.slept[i] != want[i] {
			t.Errorf("sleep[%d] = %v, want %v", i, c.slept[i], want[i])
		}
	}
}

func TestDoHonorsRetryAfter(t *testing.T) {
	c := &fakeClock{}
	rl := &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{"7"}}}
	calls := 0
	// jitter 0 → backoff contributes nothing; Retry-After must set the wait.
	err := Do(context.Background(), fakeOpts(c, 0, Options{MaxAttempts: 2}), func() error {
		calls++
		return rl
	})
	if !errors.Is(err, rl) || calls != 2 {
		t.Fatalf("err=%v calls=%d; want the 429 after 2 calls", err, calls)
	}
	if len(c.slept) != 1 || c.slept[0] != 7*time.Second {
		t.Errorf("sleeps = %v, want [7s]", c.slept)
	}
}

func TestDoRetryAfterSmallerThanBackoff(t *testing.T) {
	c := &fakeClock{}
	rl := &googleapi.Error{Code: 429, Header: http.Header{"Retry-After": []string{"1"}}}
	calls := 0
	err := Do(context.Background(), fakeOpts(c, 1, Options{MaxAttempts: 2, BaseDelay: 4 * time.Second}), func() error {
		calls++
		return rl
	})
	if !errors.Is(err, rl) || calls != 2 {
		t.Fatalf("err=%v calls=%d; want the 429 after 2 calls", err, calls)
	}
	if len(c.slept) != 1 || c.slept[0] != 4*time.Second {
		t.Errorf("sleeps = %v, want [4s] (backoff wins over smaller Retry-After)", c.slept)
	}
}

func TestDoBudgetExceeded(t *testing.T) {
	c := &fakeClock{}
	calls := 0
	last := &googleapi.Error{Code: 503}
	// jitter 1, base 8s: attempt 1 sleeps 8s (within 10s budget); the
	// 16s wait before attempt 3 would cross it, so Do gives up after 2.
	err := Do(context.Background(), fakeOpts(c, 1, Options{
		MaxAttempts: 10,
		Budget:      10 * time.Second,
		BaseDelay:   8 * time.Second,
	}), func() error {
		calls++
		return last
	})
	if !errors.Is(err, last) {
		t.Errorf("Do = %v, want last transient error", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if len(c.slept) != 1 || c.slept[0] != 8*time.Second {
		t.Errorf("sleeps = %v, want [8s]", c.slept)
	}
}

func TestDoCapsBackoff(t *testing.T) {
	c := &fakeClock{}
	err := Do(context.Background(), fakeOpts(c, 1, Options{MaxAttempts: 10}), func() error {
		return &googleapi.Error{Code: 503}
	})
	if err == nil {
		t.Fatal("Do = nil, want error after exhausting attempts")
	}
	if len(c.slept) != 9 {
		t.Fatalf("sleeps = %v, want 9 entries", c.slept)
	}
	// 2^7 s = 128s and 2^8 s = 256s both cap at the default 2m.
	if c.slept[7] != 2*time.Minute || c.slept[8] != 2*time.Minute {
		t.Errorf("sleep[7]=%v sleep[8]=%v, want both capped at 2m", c.slept[7], c.slept[8])
	}
	for i, d := range c.slept {
		if d > 2*time.Minute {
			t.Errorf("sleep[%d] = %v exceeds 2m cap", i, d)
		}
	}
}

func TestDoContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := Do(ctx, Options{}, func() error {
		calls++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("fn called %d times on dead context, want 0", calls)
	}
}

func TestDoContextCanceledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opts := Options{}
	opts.now = time.Now
	opts.rnd = func() float64 { return 1 }
	opts.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	calls := 0
	err := Do(ctx, opts, func() error {
		calls++
		return &googleapi.Error{Code: 503}
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", o.MaxAttempts)
	}
	if o.Budget != 15*time.Minute {
		t.Errorf("Budget = %v, want 15m", o.Budget)
	}
	if o.BaseDelay != time.Second {
		t.Errorf("BaseDelay = %v, want 1s", o.BaseDelay)
	}
	if o.MaxDelay != 2*time.Minute {
		t.Errorf("MaxDelay = %v, want 2m", o.MaxDelay)
	}
	if o.sleep == nil || o.now == nil || o.rnd == nil {
		t.Error("seams not defaulted")
	}
	// Explicit values survive.
	o = Options{MaxAttempts: 3, Budget: time.Minute, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}.withDefaults()
	if o.MaxAttempts != 3 || o.Budget != time.Minute || o.BaseDelay != 100*time.Millisecond || o.MaxDelay != time.Second {
		t.Errorf("explicit options overridden: %+v", o)
	}
}

func TestClassString(t *testing.T) {
	for c, want := range map[Class]string{
		Transient:   "Transient",
		CursorGone:  "CursorGone",
		AuthBroken:  "AuthBroken",
		ItemGone:    "ItemGone",
		RateLimited: "RateLimited",
		Fatal:       "Fatal",
		Class(99):   "Class(99)",
	} {
		if got := c.String(); got != want {
			t.Errorf("Class(%d).String() = %q, want %q", int(c), got, want)
		}
	}
}
