// Package main — per-proxy IP health tracker for proactive rotation.
//
// The core insight from Reddit's anti-bot system: when an exit IP starts
// serving 403/rate-limited responses, that IP is doomed - requests keep
// failing until you switch IPs. The old approach relied solely on per-lane
// block flags + VPN timer cooldowns, which meant a bad exit IP burned
// minutes before being swapped. This tracker builds a fast reputation
// score (0-100) per proxy and proactively demotes/isolates unhealthy IPs.
package main

import (
	"fmt"
	"sync"
	"time"
)

// ipHealthScore tracks success/failure ratios and signals for each proxy.
type ipHealthScore struct {
	mu       sync.Mutex
	success  int64 // successful request counts
	blocked  int64 // 403/block page responses
	failures int64 // network errors / context dead
	score    int   // current score 0..100
	lastFail time.Time
}

// NewIPReputation creates a tracker keyed by index into the proxy list.
// Returns an index into a proxy health map for quick lookup during lane build.
func newIPReputation(proxyCount int) map[string]*ipHealthScore {
	m := make(map[string]*ipHealthScore, proxyCount)
	for i := 0; i < proxyCount; i++ {
		m[fmt.Sprintf("ip_%d", i)] = &ipHealthScore{}
	}
	return m
}

// NewIPReputationTracker mirrors newIPReputation but adds Update/IsHealthy/etc.
// Actually, let me just use newIPReputation + direct methods on the returned struct.
// The lanes.go currently calls newIPReputationTracker() — we need to rename it.
func newIPReputationTracker(proxyCount int) map[string]*ipHealthScore {
	return newIPReputation(proxyCount)
}

// Update records a single request outcome and adjusts the health score.
func (i *ipHealthScore) Update(blocked bool, fail bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if blocked {
		i.blocked++
		i.lastFail = time.Now()
		if i.score > 20 {
			i.score -= 8
		}
	} else if fail {
		i.failures++
		if i.score > 5 {
			i.score -= 2
		}
	} else {
		i.success++
	}
	total := i.success + i.blocked + i.failures
	if total > 0 {
		earned := float64(i.success)*0.8 + float64(i.failures)*(-0.3) - float64(i.blocked)*10.0
		outcome := earned / float64(total) * 100.0
		i.score = clampScore(outcome)
	}
}

// IsHealthy returns true if this IP passes the health threshold.
// Threshold: score >= 50 after at least 10 samples.
func (i *ipHealthScore) IsHealthy() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.score >= 50 && (i.success+i.blocked+i.failures) >= 10
}

// IsDoomed returns true if this IP should be excluded from lane allocation.
// A doomed IP drops below score 25.
func (i *ipHealthScore) IsDoomed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.score < 25
}

// SoftThreshold: score >= 70 means "active but weakening".
func (i *ipHealthScore) IsWeak() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.score >= 70 && i.score < 85
}

// clampScore clamps a float64 score to the 0..100 integer range.
func clampScore(v float64) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	default:
		return int(v + 0.5)
	}
}
