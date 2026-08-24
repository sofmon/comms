package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestUnitsBurstAndCost(t *testing.T) {
	// 50 units/s, burst 20: a cost-20 call drains the bucket instantly;
	// the next cost-5 call must wait ~100ms for refill.
	u := NewUnits(50, 20)
	ctx := context.Background()

	start := time.Now()
	if err := u.Wait(ctx, 20); err != nil {
		t.Fatalf("Wait(20): %v", err)
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Errorf("burst-covered Wait(20) took %v, want ~immediate", el)
	}

	start = time.Now()
	if err := u.Wait(ctx, 5); err != nil {
		t.Fatalf("Wait(5): %v", err)
	}
	el := time.Since(start)
	if el < 60*time.Millisecond {
		t.Errorf("post-burst Wait(5) took %v, want >= ~100ms refill wait", el)
	}
	if el > 3*time.Second {
		t.Errorf("post-burst Wait(5) took %v, want ~100ms", el)
	}
}

func TestUnitsCostExceedsBurst(t *testing.T) {
	u := NewUnits(50, 10)
	start := time.Now()
	err := u.Wait(context.Background(), 11)
	if err == nil {
		t.Fatal("Wait(cost > burst) = nil, want error")
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Errorf("Wait(cost > burst) took %v, want immediate failure", el)
	}
}

func TestUnitsContextCanceled(t *testing.T) {
	u := NewUnits(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := u.Wait(ctx, 1); err == nil {
		t.Fatal("Wait with canceled ctx = nil, want error")
	}
}

func TestKeyedIsolatesKeys(t *testing.T) {
	// Per-key: burst 1, essentially no refill. Distinct keys each get
	// their own bucket, so the first call per key is instant; a second
	// call on the same key blocks past the deadline.
	k := NewKeyed(0.001, 1, 0, 0)
	ctx := context.Background()

	start := time.Now()
	if err := k.Wait(ctx, "space-a"); err != nil {
		t.Fatalf(`Wait("space-a"): %v`, err)
	}
	if err := k.Wait(ctx, "space-b"); err != nil {
		t.Fatalf(`Wait("space-b"): %v`, err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Errorf("first Wait per key took %v, want ~immediate", el)
	}

	short, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if err := k.Wait(short, "space-a"); err == nil {
		t.Fatal(`second Wait("space-a") = nil, want deadline error`)
	}
}

func TestKeyedOverallCap(t *testing.T) {
	// Per-key effectively unlimited; overall burst 2 with no refill:
	// the third call across any keys blocks.
	k := NewKeyed(1000, 100, 0.001, 2)
	ctx := context.Background()

	if err := k.Wait(ctx, "a"); err != nil {
		t.Fatalf(`Wait("a"): %v`, err)
	}
	if err := k.Wait(ctx, "b"); err != nil {
		t.Fatalf(`Wait("b"): %v`, err)
	}
	short, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if err := k.Wait(short, "c"); err == nil {
		t.Fatal(`Wait("c") past overall burst = nil, want deadline error`)
	}
}

func TestKeyedBurstClamped(t *testing.T) {
	// Burst 0 would make every Wait fail; NewKeyed clamps it to 1.
	k := NewKeyed(5, 0, 5, 0)
	if err := k.Wait(context.Background(), "x"); err != nil {
		t.Fatalf("Wait with clamped burst: %v", err)
	}
}

func TestKeyedLazyCreationReuses(t *testing.T) {
	k := NewKeyed(10, 5, 0, 0)
	if a, b := k.limiter("a"), k.limiter("a"); a != b {
		t.Error("limiter(\"a\") returned distinct limiters across calls")
	}
	if a, b := k.limiter("a"), k.limiter("b"); a == b {
		t.Error("distinct keys share one limiter")
	}
}

func TestKeyedConcurrent(t *testing.T) {
	// Concurrent first-use of the same and distinct keys must be safe
	// (meaningful under -race) and must not error at generous rates.
	k := NewKeyed(1000, 100, 1000, 100)
	ctx := context.Background()
	keys := []string{"a", "b", "c", "d"}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			if err := k.Wait(ctx, key); err != nil {
				errs <- err
			}
		}(keys[i%len(keys)])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Wait: %v", err)
	}
}
