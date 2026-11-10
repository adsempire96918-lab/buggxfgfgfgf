// Package main - shared per-IP rate limiting.
//
// Measured fact (2026-11-08): Reddit's limit for cookie sessions is per-IP,
// NOT per-account - 3 sessions on one IP at 2.9 req/s produced 29% blocks,
// while 1 session at 0.9 req/s was 100% clean. So every lane that shares an
// IP must share ONE token bucket, otherwise the lanes burst past the budget.
package main

import (
	"context"
	"sync"
)

// IPLimiter keeps one token bucket per IP/proxy and shares it between all
// lanes on that IP.
type IPLimiter struct {
	mu      sync.Mutex
	buckets map[string]*TokenBucket
	rate    float64
	burst   int
}

// NewIPLimiter creates a limiter. rate = requests per minute per IP,
// burst = maximum burst (keep it small: 2).
func NewIPLimiter(rate float64, burst int) *IPLimiter {
	if rate <= 0 {
		rate = 60
	}
	if burst <= 0 {
		burst = 2
	}
	return &IPLimiter{
		buckets: make(map[string]*TokenBucket),
		rate:    rate,
		burst:   burst,
	}
}

// Bucket returns (creating if needed) the token bucket of an IP. All lanes
// on the same IP must use this same bucket.
func (l *IPLimiter) Bucket(ip string) *TokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = NewTokenBucket(l.rate, l.burst)
		l.buckets[ip] = b
	}
	return b
}

// Wait blocks until this IP has a token.
func (l *IPLimiter) Wait(ctx context.Context, ip string) error {
	return l.Bucket(ip).Wait(ctx)
}

// SetRate changes the rate of every bucket of this limiter (AIMD control).
func (l *IPLimiter) SetRate(perMin float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if perMin <= 0 {
		perMin = 1
	}
	for _, b := range l.buckets {
		b.SetRate(perMin)
	}
	l.rate = perMin
}

// Rate returns the configured per-IP rate.
func (l *IPLimiter) Rate() float64 { return l.rate }

// IPLimiterGrouped keeps ONE token bucket per EXIT IP.
//
// HISTORY (the bug this fixes): it used to keep one bucket per ROTATING
// GROUP, and the group counter was global - so a single exit IP with 5
// groups received 5 x the configured per-IP rate. That is the fastest way
// to get the exit IP blocked, and it is why a 500-lane run on one IP showed
// 8-15% blocks while the configured rate looked conservative.
//
// The groups are still tracked - they drive the session rotation, the
// adaptive group-rate tracker and the debug panels - but the BUDGET is per
// IP, exactly the way Reddit applies it. The group argument is kept in the
// API so the callers (and the group concept) stay intact.
//
// The `group` argument is accepted for API compatibility and for the
// per-group reporting; it does NOT create a separate budget.
type IPLimiterGrouped struct {
	mu        sync.Mutex
	buckets   map[string]*TokenBucket // one bucket per exit IP ("" = direct)
	rate      float64                 // requests per minute per exit IP
	burst     int
	numGroups int
}

// NewIPLimiterGrouped creates a limiter.
// rate = requests per minute PER EXIT IP (the whole budget, not per group).
// burst = maximum burst per exit IP.
// numGroups = number of rotating groups (use 5 for the 5-rotating method).
func NewIPLimiterGrouped(rate float64, burst int, numGroups int) *IPLimiterGrouped {
	if rate <= 0 {
		rate = 60
	}
	if burst <= 0 {
		burst = 2
	}
	if numGroups <= 0 {
		numGroups = 5
	}
	return &IPLimiterGrouped{
		buckets:   make(map[string]*TokenBucket),
		rate:      rate,
		burst:     burst,
		numGroups: numGroups,
	}
}

// Bucket returns the token bucket of an exit IP. Every lane on that IP -
// whatever group it belongs to - shares this ONE bucket, so the total
// request rate of an IP can never exceed the configured budget.
//
// The group is used only to keep the API and the reporting intact.
func (l *IPLimiterGrouped) Bucket(ip string, group int) *TokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		b = NewTokenBucket(l.rate, l.burst)
		l.buckets[ip] = b
	}
	return b
}

// Wait blocks until the exit IP has a token.
func (l *IPLimiterGrouped) Wait(ctx context.Context, ip string, group int) error {
	return l.Bucket(ip, group).Wait(ctx)
}

// SetRate changes the rate of every exit IP (AIMD control).
func (l *IPLimiterGrouped) SetRate(perMin float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if perMin <= 0 {
		perMin = 1
	}
	for _, b := range l.buckets {
		b.SetRate(perMin)
	}
	l.rate = perMin
}

// Rate returns the configured per-IP rate.
func (l *IPLimiterGrouped) Rate() float64 { return l.rate }

// Burst returns the configured burst size.
func (l *IPLimiterGrouped) Burst() int { return l.burst }

// NumGroups returns the number of rotating groups.
func (l *IPLimiterGrouped) NumGroups() int { return l.numGroups }

// IPs returns the exit IPs that have a bucket (for the debug panels).
func (l *IPLimiterGrouped) IPs() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.buckets))
	for ip := range l.buckets {
		out = append(out, ip)
	}
	return out
}
