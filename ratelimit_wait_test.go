// T-04 — TokenBucket.Wait reserves the token and sleeps ONCE for the exact
// time it becomes available, so a large waiter set cannot spin.
package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestTokenBucketNoBusyLoop is the acceptance test of T-04.
//
// The old Wait capped the sleep at 100 ms and re-checked under the lock, so a
// 200-waiter set produced thousands of wakeups per second for nothing. With
// the reservation scheme every waiter sleeps exactly once, for its own slot.
func TestTokenBucketNoBusyLoop(t *testing.T) {
	// 6000/min = 100 tokens/s: 200 waiters drain in ~2 s, which keeps the
	// test fast while exercising exactly the same code path as 60/min.
	const perMin = 6000
	const waiters = 200
	b := NewTokenBucket(perMin, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// drain the burst token so every waiter below really has to sleep
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("burst token: %v", err)
	}
	wakeupsBefore := b.Wakeups()

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Wait(ctx); err != nil {
				t.Errorf("wait: %v", err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	wakeups := b.Wakeups() - wakeupsBefore
	// one wakeup per token; allow a small slack for scheduling jitter
	if wakeups > waiters+int64(waiters/10) {
		t.Fatalf("%d wakeups for %d tokens - the waiters are spinning", wakeups, waiters)
	}
	// the burst token is gone, so ~199 tokens at 100/s must really be paced
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("200 waiters drained in %s - the bucket did not pace them", elapsed)
	}
	if debt := b.Debt(); debt > 1 {
		t.Fatalf("debt after the drain = %.2f tokens, want ~0", debt)
	}
}

// A cancelled waiter must hand its reserved token back, otherwise every
// timeout permanently shrinks the budget.
func TestTokenBucketCancelReturnsReservedSlot(t *testing.T) {
	b := NewTokenBucket(600, 1) // 10/s
	ctx := context.Background()
	if !b.TryWait() {
		t.Fatal("burst token not available")
	}
	// reserve a slot 100 ms in the future, then cancel it
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := b.Wait(cctx); err == nil {
		t.Fatal("expected ctx error")
	}
	if debt := b.Debt(); debt > 0.05 {
		t.Fatalf("debt = %.3f after a cancelled wait, want 0 (the slot leaked)", debt)
	}
	// the bucket still paces normally afterwards
	start := time.Now()
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("wait after cancel: %v", err)
	}
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("waited only %s - the cancelled reservation was not honoured", d)
	}
}
