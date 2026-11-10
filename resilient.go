// Package main — resilience gate: per-IP rate + breaker + 429 backoff.
package main

import (
	"context"
	"errors"
	"strconv"
	"time"
)

var errNoTime = errors.New("unparseable retry-after")

// Gate bundles the per-IP controls around a Client. A nil Gate on a Client
// keeps the legacy (ungated) behavior — Task 1 CLI stays untouched.
type Gate struct {
	Bucket   *TokenBucket // per-lane / per-session budget
	IPBucket *TokenBucket // shared per-IP budget (optional, higher limit)
	Breaker  *CircuitBreaker
	Retries  int           // 429 retries with backoff
	Backoff  time.Duration // base for exponential 429 backoff
}

func NewGate(perMin float64, maxBlocked int, cooldown, backoff time.Duration, retries int) *Gate {
	if backoff <= 0 {
		backoff = 5 * time.Second
	}
	return NewGateWithBreaker(perMin, NewCircuitBreaker(maxBlocked, cooldown), backoff, retries)
}

// NewGateWithBreaker builds a gate around an EXISTING breaker. Lanes use this
// to share one breaker per exit IP (BreakerRegistry): a block wave then opens
// the circuit once for the whole IP instead of once per lane (T-03).
func NewGateWithBreaker(perMin float64, breaker *CircuitBreaker, backoff time.Duration, retries int) *Gate {
	if backoff <= 0 {
		backoff = 5 * time.Second
	}
	return &Gate{
		Bucket:  NewTokenBucket(perMin, 10),
		Breaker: breaker,
		Retries: retries,
		Backoff: backoff,
	}
}

// Admit passes the request through rate limit + breaker.
func (g *Gate) Admit(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if g.IPBucket != nil {
		if err := g.IPBucket.Wait(ctx); err != nil {
			return err
		}
	}
	if err := g.Bucket.Wait(ctx); err != nil {
		return err
	}
	if g.Breaker != nil && g.Breaker.IsBlocked() {
		debugf("gate: circuit breaker OPEN - waiting for the cooldown")
	}
	if g.Breaker == nil {
		return nil
	}
	return g.Breaker.Allow(ctx)
}

// Report feeds one HTTP outcome back into the breaker.
//
// A 401/403 that carries Reddit's jsc challenge is SOLVABLE - it is not an
// IP-level block. Counting it against the circuit breaker opened the
// circuit after a handful of challenge pages and froze the whole run for
// the full cooldown (5 minutes) even though the IP was perfectly fine.
func (g *Gate) Report(code int, body string) {
	if g == nil {
		return
	}
	blocked := code == 401 || code == 403
	if blocked && challengeInputRe.MatchString(body) {
		blocked = false
	}
	if blocked {
		debugf("gate: block-class response code=%d body=%q", code, firstN(body, 140))
	}
	if g.Breaker == nil {
		return
	}
	g.Breaker.Report(blocked)
}

// RetryAfter parses a Retry-After header value (seconds or date).
func RetryAfter(v string) time.Duration {
	v = trimSpace(v)
	if v == "" {
		return 0
	}
	if sec, err := strconv.ParseFloat(v, 64); err == nil && sec > 0 {
		d := time.Duration(sec * float64(time.Second))
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		return d
	}
	if t, err := httpParseTime(v); err == nil {
		d := time.Until(t)
		if d > 2*time.Minute {
			d = 2 * time.Minute
		}
		if d < 0 {
			d = 0
		}
		return d
	}
	return 0
}

// Backoff returns the exponential delay for attempt n (0-based): base, 2x, 4x...
func (g *Gate) BackoffDelay(attempt int) time.Duration {
	base := g.Backoff
	for i := 0; i < attempt; i++ {
		base *= 2
	}
	if base > 60*time.Second {
		base = 60 * time.Second
	}
	return base
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func httpParseTime(v string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC1123,
		time.RFC1123Z,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errNoTime
}
