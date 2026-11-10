// T-03 — one circuit breaker per EXIT IP, shared by every lane behind it.
package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSharedBreakerOpensOnceForAllLanes is the acceptance test of T-03:
// 100 lanes on one IP share one breaker state, and after the cooldown exactly
// ONE of them is let through as the half-open probe.
func TestSharedBreakerOpensOnceForAllLanes(t *testing.T) {
	reg := NewBreakerRegistry(3, 60*time.Millisecond)
	if got := reg.Len(); got != 0 {
		t.Fatalf("new registry Len = %d, want 0", got)
	}

	const lanes = 100
	breakers := make([]*CircuitBreaker, lanes)
	for i := range breakers {
		breakers[i] = reg.For("http://10.0.0.1:8080")
	}
	for i := range breakers {
		if breakers[i] != breakers[0] {
			t.Fatalf("lane %d has its own breaker - the IP state is not shared", i)
		}
	}
	if reg.Len() != 1 {
		t.Fatalf("registry Len = %d, want 1 exit IP", reg.Len())
	}
	if other := reg.For("http://10.0.0.2:8080"); other == breakers[0] {
		t.Fatal("a different exit IP got the same breaker")
	}

	// A block wave on ONE lane must open the circuit for all of them.
	for i := 0; i < 3; i++ {
		breakers[0].Report(true)
	}
	if breakers[0].State() != StateOpen {
		t.Fatalf("state = %v, want open", breakers[0].State())
	}
	for i := range breakers {
		if !breakers[i].IsBlocked() {
			t.Fatalf("lane %d does not see the shared OPEN state", i)
		}
	}

	// After the cooldown exactly one lane may probe; the other 99 wait.
	time.Sleep(70 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var allowed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < lanes; i++ {
		wg.Add(1)
		go func(b *CircuitBreaker) {
			defer wg.Done()
			<-start
			if err := b.Allow(ctx); err == nil {
				allowed.Add(1)
			}
		}(breakers[i])
	}
	close(start)

	// give every waiter time to reach the breaker
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if allowed.Load() > 0 {
			time.Sleep(50 * time.Millisecond)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := allowed.Load(); got != 1 {
		t.Fatalf("%d lanes were let through the half-open state, want exactly 1", got)
	}
	if st := breakers[0].State(); st != StateHalfOpen {
		t.Fatalf("state = %v, want half-open while the probe is in flight", st)
	}

	// A clean probe closes the circuit and releases every waiter.
	breakers[0].Report(false)
	wg.Wait()
	if got := allowed.Load(); got != lanes {
		t.Fatalf("%d lanes passed after the clean probe, want %d", got, lanes)
	}
	if st := breakers[0].State(); st != StateClosed {
		t.Fatalf("state = %v, want closed", st)
	}
}

// A blocked probe must re-open the circuit and keep the waiters waiting.
func TestSharedBreakerFailedProbeReopensForAll(t *testing.T) {
	// the cooldown is deliberately much longer than the waiter's budget, so
	// the waiter cannot legitimately become the next probe while it waits
	reg := NewBreakerRegistry(1, 300*time.Millisecond)
	b := reg.For("direct")
	b.Report(true)
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	time.Sleep(320 * time.Millisecond)
	if err := b.Allow(context.Background()); err != nil {
		t.Fatalf("probe allow: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	waiting := make(chan error, 1)
	go func() { waiting <- b.Allow(ctx) }()
	time.Sleep(20 * time.Millisecond)

	// the probe is blocked -> OPEN again; the waiter must time out, not pass
	b.Report(true)
	if st := b.State(); st != StateOpen {
		t.Fatalf("state = %v, want open after a blocked probe", st)
	}
	if err := <-waiting; err == nil {
		t.Fatal("a waiter passed through while the circuit was re-opened")
	}
}

// TestLanesShareOneBreakerPerExitIP checks the actual wiring: buildLanes hands
// every lane behind the same proxy the same breaker object.
func TestLanesShareOneBreakerPerExitIP(t *testing.T) {
	sessions := []*Session{{Name: "s1"}, {Name: "s2"}, {Name: "s3"}}
	pool := NewSessionPool(sessions, 72, 2)
	lim := NewIPLimiterGrouped(1200, 2, 5)
	mgr, err := buildLanes(pool,
		[]string{"http://10.0.0.1:8080", "http://10.0.0.2:8080"},
		2, lim, 5*time.Second, 3, 1, 72, 2, 0, NewMetrics())
	if err != nil {
		t.Fatalf("buildLanes: %v", err)
	}
	all := mgr.All()
	if len(all) != 4 {
		t.Fatalf("lanes = %d, want 4 (2 per exit IP)", len(all))
	}
	if all[0].Breaker != all[1].Breaker {
		t.Fatal("two lanes on the same exit IP do not share a breaker")
	}
	if all[0].Gate.Breaker != all[0].Breaker {
		t.Fatal("the lane gate does not use the shared breaker")
	}
	if all[2].Breaker != all[3].Breaker {
		t.Fatal("the two lanes of the second exit IP do not share a breaker")
	}
	if all[2].Breaker == all[0].Breaker {
		t.Fatal("a lane on another exit IP shares the breaker of the first IP")
	}
	// a session swap must keep the shared per-IP breaker
	swapped := gateForSession(all[0].Gate, &Session{Name: "fresh", Bucket: NewTokenBucket(60, 2)})
	if swapped.Breaker != all[0].Breaker {
		t.Fatal("gateForSession dropped the shared breaker")
	}
	if swapped.Bucket == all[0].Gate.Bucket {
		t.Fatal("gateForSession did not take the new session bucket")
	}
}
