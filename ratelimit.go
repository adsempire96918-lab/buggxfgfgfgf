// Package main — token-bucket rate limiter (restored 2026-11-09).
//
// The file had been emptied at some point mid-refactor, which broke the
// build; the semantics below match the original Task 2 design and the
// engine_test.go expectations:
//   - rate is given in requests PER MINUTE (the CLI convention), refilled
//     continuously at rate/60 tokens per second;
//   - the first `burst` requests are instant, anything above paces;
//   - Wait is ctx-cancellable (TestTokenBucketCancel: a 60s-away token
//     returns the ctx error instead of sleeping blind);
//   - Wait RESERVES its token and sleeps ONCE for the exact time it becomes
//     available (T-04). The bucket may therefore carry a negative balance
//     ("debt") - that debt IS the queue. No 100 ms cap, no re-check loop, so
//     500 waiters cost 500 wakeups total instead of 5000 per second.
package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// TokenBucket paces requests: continuous refill, no timer goroutines.
// Every method is safe for concurrent use (150 workers share one IP bucket).
type TokenBucket struct {
	mu     sync.Mutex
	perSec float64 // refill rate (tokens per second)
	burst  float64
	tokens float64 // may go NEGATIVE: a negative balance is a reserved queue slot
	last   time.Time
	// wakeups counts how often a caller actually had to sleep. It exists so
	// the no-busy-loop property is measurable (TestTokenBucketNoBusyLoop).
	wakeups atomic.Int64
}

// NewTokenBucket creates a bucket. perMin = requests per minute,
// burst = how many requests may pass instantly before pacing starts.
func NewTokenBucket(perMin float64, burst int) *TokenBucket {
	if perMin <= 0 {
		perMin = 60
	}
	if burst <= 0 {
		burst = 1
	}
	return &TokenBucket{
		perSec: perMin / 60,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

// waitTime returns how long the caller must wait for the next token and
// refills the bucket up to "now" (caller holds the lock).
func (b *TokenBucket) now() {
	t := time.Now()
	if b.tokens < b.burst {
		b.tokens += t.Sub(b.last).Seconds() * b.perSec
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.last = t
}

// Wait consumes one token, blocking (ctx-cancellable) until one is available.
//
// The token is RESERVED here - the balance goes negative and the caller sleeps
// exactly as long as the debt takes to refill. That is what removes the
// thundering herd: every waiter has its own deadline, spaced by 1/rate, so
// they wake one after the other instead of all waking every 100 ms to fight
// over the same token.
func (b *TokenBucket) Wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.now()
	b.tokens--
	wait := time.Duration(0)
	if b.tokens < 0 {
		wait = time.Duration(-b.tokens / b.perSec * float64(time.Second))
	}
	b.mu.Unlock()
	if wait <= 0 {
		return nil
	}

	b.wakeups.Add(1)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// the request was abandoned: hand the reserved token back so the
		// cancelled waiter does not leave a hole in the budget
		b.mu.Lock()
		b.now()
		b.tokens++
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.mu.Unlock()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Wakeups reports how many callers had to sleep on this bucket. A busy-loop
// implementation grows this far faster than the consumed token count.
func (b *TokenBucket) Wakeups() int64 { return b.wakeups.Load() }

// Debt returns how many tokens are currently reserved ahead of "now"
// (0 when the bucket is in credit). It is the queue length at the bucket.
func (b *TokenBucket) Debt() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now()
	if b.tokens >= 0 {
		return 0
	}
	return -b.tokens
}

// TryWait takes a token if one is available right now (non blocking).
func (b *TokenBucket) TryWait() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now()
	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// SetRate changes the refill rate (requests per minute). AIMD uses this.
func (b *TokenBucket) SetRate(perMin float64) {
	if perMin <= 0 {
		perMin = 1 // never allow a zero/negative rate: it would stall forever
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now()
	b.perSec = perMin / 60
	if b.burst > b.perSec*2 && b.perSec*2 >= 1 {
		// keep the burst proportional to a fast rate, but never below 1
		b.burst = b.perSec * 2
	}
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}

// Rate returns the current rate in requests per minute.
func (b *TokenBucket) Rate() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.perSec * 60
}
