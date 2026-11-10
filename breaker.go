// Package main — per-IP circuit breaker.
// N consecutive blocked/unauthorized responses open the circuit; after a
// cooldown exactly ONE half-open probe decides recovery.
//
// The breaker state belongs to an EXIT IP, never to a lane: 500 lanes behind
// one VPN IP are one bot as far as Reddit is concerned, so a block wave must
// pause all of them at once (T-03). BreakerRegistry below is what lanes use;
// a standalone CircuitBreaker is still available for tests and one-off gates.
package main

import (
	"context"
	"sync"
	"time"
)

type BreakerState int

const (
	StateClosed BreakerState = iota
	StateOpen
	StateHalfOpen
)

// CircuitBreaker opens after maxConsecutive block-class failures.
//
// The half-open probe is a single-flight: the first caller that reaches the
// breaker after the cooldown becomes the probe, every other caller waits on
// probeDone (ctx-cancellable) instead of polling. That is what keeps a
// recovered exit IP from being hit by 500 probes at the same millisecond.
type CircuitBreaker struct {
	mu             sync.Mutex
	state          BreakerState
	consecutive    int
	maxConsecutive int
	cooldown       time.Duration
	openedAt       time.Time
	probing        bool
	probeDone      chan struct{} // closed when the in-flight probe resolves
}

func NewCircuitBreaker(maxConsecutive int, cooldown time.Duration) *CircuitBreaker {
	if maxConsecutive <= 0 {
		maxConsecutive = 10
	}
	if cooldown <= 0 {
		cooldown = 15 * time.Minute
	}
	return &CircuitBreaker{state: StateClosed, maxConsecutive: maxConsecutive, cooldown: cooldown}
}

// beginProbeLocked marks this caller as the single half-open probe and
// returns the channel the waiting callers block on. Caller holds the lock.
func (c *CircuitBreaker) beginProbeLocked() {
	c.state = StateHalfOpen
	c.probing = true
	c.probeDone = make(chan struct{})
}

// finishProbeLocked releases every waiter of the current probe. Caller holds
// the lock.
func (c *CircuitBreaker) finishProbeLocked() {
	c.probing = false
	if c.probeDone != nil {
		close(c.probeDone)
		c.probeDone = nil
	}
}

// Allow blocks while the circuit is open (up to ctx). A half-open probe
// lets exactly one request through; the rest wait for its verdict.
func (c *CircuitBreaker) Allow(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		switch c.state {
		case StateClosed:
			c.mu.Unlock()
			return nil
		case StateOpen:
			remaining := c.cooldown - time.Since(c.openedAt)
			if remaining <= 0 {
				// cooldown is over: this caller is THE probe
				c.beginProbeLocked()
				c.mu.Unlock()
				return nil
			}
			c.mu.Unlock()
			if err := breakerWait(ctx, remaining); err != nil {
				return err
			}
			// re-evaluate: the cooldown may have expired while we slept
		case StateHalfOpen:
			if !c.probing {
				// nothing in flight (defensive): take the probe slot
				c.beginProbeLocked()
				c.mu.Unlock()
				return nil
			}
			done := c.probeDone
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
				// the probe reported; loop and look at the new state
			}
		}
	}
}

// Report records one request outcome. blocked = 401/403 block-class.
func (c *CircuitBreaker) Report(blocked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case StateHalfOpen:
		c.finishProbeLocked()
		if blocked {
			c.state = StateOpen
			c.openedAt = time.Now()
			debugf("breaker: probe blocked - OPEN again (cooldown %s)", c.cooldown)
		} else {
			c.state = StateClosed
			c.consecutive = 0
			debugf("breaker: probe clean - CLOSED")
		}
	case StateOpen:
		// stay open; Allow() re-evaluates the cooldown
	case StateClosed:
		if blocked {
			c.consecutive++
			if c.consecutive >= c.maxConsecutive {
				c.state = StateOpen
				c.openedAt = time.Now()
				debugf("breaker: OPEN after %d consecutive blocks (cooldown %s)", c.consecutive, c.cooldown)
			}
		} else {
			c.consecutive = 0
		}
	}
}

// State returns the current breaker state.
func (c *CircuitBreaker) State() BreakerState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// IsBlocked reports whether requests would wait right now.
func (c *CircuitBreaker) IsBlocked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == StateOpen && time.Since(c.openedAt) < c.cooldown
}

// Reset forces the breaker closed and clears the consecutive counter. Used
// after a VPN IP rotation: the blocks belonged to the old exit IP, so the
// fresh IP starts with a clean breaker.
func (c *CircuitBreaker) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = StateClosed
	c.consecutive = 0
	c.finishProbeLocked()
}

// Consecutive returns the current consecutive-block counter (reporting/tests).
func (c *CircuitBreaker) Consecutive() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.consecutive
}

// breakerWait sleeps for d but returns early when ctx is cancelled.
func breakerWait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Shared per-exit-IP breakers (T-03)
// ---------------------------------------------------------------------------

// BreakerRegistry keeps ONE breaker per exit IP. Every lane that runs through
// the same proxy string gets the same breaker object, so a block wave opens
// the circuit once and pauses all of them — instead of every lane retrying
// independently (500 simultaneous retries is a textbook bot pattern).
//
// The key is the proxy/exit-IP string as configured ("" = direct connection).
type BreakerRegistry struct {
	mu             sync.Mutex
	breakers       map[string]*CircuitBreaker
	maxConsecutive int
	cooldown       time.Duration
}

// NewBreakerRegistry creates an empty registry.
func NewBreakerRegistry(maxConsecutive int, cooldown time.Duration) *BreakerRegistry {
	if maxConsecutive <= 0 {
		maxConsecutive = 10
	}
	if cooldown <= 0 {
		cooldown = 15 * time.Minute
	}
	return &BreakerRegistry{
		breakers:       make(map[string]*CircuitBreaker),
		maxConsecutive: maxConsecutive,
		cooldown:       cooldown,
	}
}

// For returns the breaker of one exit IP, creating it on first use. The same
// key always yields the same object.
func (r *BreakerRegistry) For(key string) *CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[key]
	if !ok {
		b = NewCircuitBreaker(r.maxConsecutive, r.cooldown)
		r.breakers[key] = b
	}
	return b
}

// Len returns how many exit IPs have a breaker.
func (r *BreakerRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.breakers)
}

// Keys returns the registered exit-IP keys (debug panels).
func (r *BreakerRegistry) Keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.breakers))
	for k := range r.breakers {
		out = append(out, k)
	}
	return out
}

// ResetAll closes every breaker. Used after a VPN rotation: the blocks
// belonged to the old exit IP.
func (r *BreakerRegistry) ResetAll() {
	r.mu.Lock()
	breakers := make([]*CircuitBreaker, 0, len(r.breakers))
	for _, b := range r.breakers {
		breakers = append(breakers, b)
	}
	r.mu.Unlock()
	for _, b := range breakers {
		b.Reset()
	}
}

// breakerKey normalises the proxy string into the registry key. An empty
// proxy means the direct connection of this machine, which is one exit IP.
func breakerKey(proxy string) string {
	if proxy == "" {
		return "direct"
	}
	return proxy
}
