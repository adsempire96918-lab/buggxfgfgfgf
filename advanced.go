// Package main — advanced rate-limit avoidance: adaptive per-group rate
// control, source health tracking and the debug panel.
//
// The 5-group rotation (iplimit.go) spreads the traffic over 5 independent
// token buckets. This file adds the intelligence on top of it:
//
//   - GroupRateTracker: watches the outcome of every request per group and
//     raises/lowers that group's rate so a healthy group speeds up and a
//     throttled group backs off automatically.
//   - SourceHealthTracker: remembers which session/proxy actually returns
//     data so the checker can prefer the working ones.
//   - Debug panel: prints the live state of all of the above when
//     KARMA_DEBUG=1 (or --debug) is set.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================
// GroupRateTracker — adaptive rate control per rotating group
// ============================================================

// GroupRateInfo is the live state of one rotating group.
type GroupRateInfo struct {
	group          int
	reqsThisMinute int
	successCount   int
	errorCount     int
	lastAdjustment time.Time
	fillRatio      float64
	rate           float64
}

// GroupRateTracker adjusts the per-group request rate from the observed
// success rate. A group that keeps succeeding is sped up (up to maxRate);
// a group that keeps failing is slowed down (down to minRate).
type GroupRateTracker struct {
	mu              sync.Mutex
	groups          [5]*GroupRateInfo
	minRate         float64
	maxRate         float64
	adjustmentSpeed float64
	limiter         *IPLimiterGrouped
}

// NewGroupRateTracker creates a tracker for the 5 rotating groups.
func NewGroupRateTracker(minRate, maxRate, speed float64) *GroupRateTracker {
	if minRate <= 0 {
		minRate = 30
	}
	if maxRate <= 0 {
		maxRate = 120
	}
	if speed <= 0 {
		speed = 0.5
	}
	if speed > 1 {
		speed = 1
	}
	t := &GroupRateTracker{
		minRate:         minRate,
		maxRate:         maxRate,
		adjustmentSpeed: speed,
	}
	for i := 0; i < 5; i++ {
		t.groups[i] = &GroupRateInfo{group: i, rate: minRate}
	}
	return t
}

// Attach binds the tracker to the limiter it controls.
func (g *GroupRateTracker) Attach(l *IPLimiterGrouped) {
	g.mu.Lock()
	g.limiter = l
	g.mu.Unlock()
}

// Update records one request outcome for a group and (every 20s) adjusts
// that group's rate.
func (g *GroupRateTracker) Update(group int, success bool) {
	if group < 0 || group > 4 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	info := g.groups[group]
	info.reqsThisMinute++
	if success {
		info.successCount++
	} else {
		info.errorCount++
	}
	if info.rate <= 0 {
		info.rate = g.minRate
	}
	info.fillRatio = float64(info.reqsThisMinute) / 60.0

	// Adjust at most every 20 seconds.
	if time.Since(info.lastAdjustment) < 20*time.Second {
		return
	}

	total := info.successCount + info.errorCount
	successRate := 1.0
	if total > 0 {
		successRate = float64(info.successCount) / float64(total)
	}

	newRate := info.rate
	switch {
	case successRate > 0.95 && info.fillRatio < 0.8:
		// Healthy and not saturated: speed up a little.
		newRate = info.rate * (1 + g.adjustmentSpeed)
		if newRate > g.maxRate {
			newRate = g.maxRate
		}
	case successRate < 0.8 || info.fillRatio > 0.9:
		// Throttled or saturated: back off.
		newRate = info.rate * (1 - g.adjustmentSpeed*0.5)
		if newRate < g.minRate {
			newRate = g.minRate
		}
	}
	if newRate != info.rate {
		info.rate = newRate
		if g.limiter != nil {
			g.limiter.SetRate(newRate)
		}
	}
	info.lastAdjustment = time.Now()
}

// GetRates returns the current rate of every group.
func (g *GroupRateTracker) GetRates() [5]float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out [5]float64
	for i, info := range g.groups {
		out[i] = info.rate
	}
	return out
}

// GetFillRatios returns the bucket fill ratio (0..1) of every group.
func (g *GroupRateTracker) GetFillRatios() [5]float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out [5]float64
	for i, info := range g.groups {
		out[i] = info.fillRatio
	}
	return out
}

// GetSuccessRates returns the success ratio (0..1) of every group.
func (g *GroupRateTracker) GetSuccessRates() [5]float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out [5]float64
	for i, info := range g.groups {
		total := info.successCount + info.errorCount
		if total > 0 {
			out[i] = float64(info.successCount) / float64(total)
		} else {
			out[i] = 1
		}
	}
	return out
}

// GetCounts returns (successes, errors) per group.
func (g *GroupRateTracker) GetCounts() ([5]int64, [5]int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var okc, errc [5]int64
	for i, info := range g.groups {
		okc[i] = int64(info.successCount)
		errc[i] = int64(info.errorCount)
	}
	return okc, errc
}

// Reset clears the tracker state (used after a VPN rotation gives a new IP).
func (g *GroupRateTracker) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, info := range g.groups {
		info.reqsThisMinute = 0
		info.successCount = 0
		info.errorCount = 0
		info.fillRatio = 0
		info.lastAdjustment = time.Time{}
	}
}

// ============================================================
// SourceHealthTracker — which session/proxy actually works
// ============================================================

// SourceHealthInfo is the live state of one source (session file or proxy).
type SourceHealthInfo struct {
	checks      int64
	successes   int64
	errors      int64
	lastCheck   time.Time
	avgResponse time.Duration
	successRate float64
	isHealthy   bool
}

// SourceHealthTracker remembers the health of every source.
type SourceHealthTracker struct {
	mu             sync.Mutex
	sources        map[string]*SourceHealthInfo
	totalChecks    int64
	totalSuccesses int64
}

// NewSourceHealthTracker creates an empty tracker.
func NewSourceHealthTracker() *SourceHealthTracker {
	return &SourceHealthTracker{sources: make(map[string]*SourceHealthInfo)}
}

// RecordCheck records one outcome for a source.
func (h *SourceHealthTracker) RecordCheck(source string, success bool, responseTime time.Duration) {
	if source == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	info, ok := h.sources[source]
	if !ok {
		info = &SourceHealthInfo{isHealthy: true}
		h.sources[source] = info
	}
	info.checks++
	if success {
		info.successes++
	} else {
		info.errors++
	}
	info.lastCheck = time.Now()
	info.successRate = float64(info.successes) / float64(info.checks)
	if info.checks == 1 {
		info.avgResponse = responseTime
	} else {
		info.avgResponse = (info.avgResponse*time.Duration(info.checks-1) + responseTime) / time.Duration(info.checks)
	}
	h.totalChecks++
	if success {
		h.totalSuccesses++
	}
	if info.checks >= 10 {
		info.isHealthy = info.successRate >= 0.7
	}
}

// PathHealth summarises the tracker as "how much of the exit path works":
// the share of recorded sources that are still healthy (0..1). The VPN router
// uses it to describe WHY it is rotating - a path where most sources fail is
// the exit IP's problem, not the sessions' (T-09).
func (h *SourceHealthTracker) PathHealth() (healthy, total int, ratio float64) {
	if h == nil {
		return 0, 0, 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, info := range h.sources {
		total++
		if info.isHealthy {
			healthy++
		}
	}
	if total > 0 {
		ratio = float64(healthy) / float64(total)
	}
	return healthy, total, ratio
}

// GetBestSource returns the healthiest source name ("" when none).
func (h *SourceHealthTracker) GetBestSource() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	best, bestRate := "", -1.0
	for name, info := range h.sources {
		if info.isHealthy && info.successRate > bestRate {
			bestRate, best = info.successRate, name
		}
	}
	return best
}

// GetSourceStats returns the stats of one source.
func (h *SourceHealthTracker) GetSourceStats(source string) (checks int64, successRate float64, healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	info, ok := h.sources[source]
	if !ok {
		return 0, 0, false
	}
	return info.checks, info.successRate, info.isHealthy
}

// GetAllStats returns the stats of every source.
func (h *SourceHealthTracker) GetAllStats() map[string]SourceHealthInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]SourceHealthInfo, len(h.sources))
	for name, info := range h.sources {
		out[name] = *info
	}
	return out
}

// SourceCount returns how many sources are tracked.
func (h *SourceHealthTracker) SourceCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sources)
}

// ============================================================
// Debug helpers
// ============================================================

// debugEnabled reports whether KARMA_DEBUG=1 is set in the environment.
func debugEnabled() bool {
	return os.Getenv("KARMA_DEBUG") == "1"
}

// PrintGroupPanel prints the live state of the 5 rotating groups.
//
// IMPORTANT: since the per-IP budget fix (P0-1) the limiter keeps ONE bucket
// per EXIT IP, not one per group. The groups are still tracked (they drive
// the session rotation and the source health), but they do NOT have separate
// budgets any more - so this panel must not claim they do. It shows the
// per-group OUTCOME statistics and the ONE real per-IP rate.
func PrintGroupPanel(t *GroupRateTracker) {
	if t == nil || !debugEnabled() {
		return
	}
	rates := t.GetRates()
	fills := t.GetFillRatios()
	succ := t.GetSuccessRates()
	oks, errs := t.GetCounts()

	fmt.Println()
	fmt.Println(strings.Repeat("=", 58))
	fmt.Println("  ROTATING GROUPS (outcome per group - ONE shared per-IP budget)")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  %-6s %-9s %-8s %-9s %-9s %s\n", "Group", "Req/Min", "Fill%", "Success%", "OK/ERR", "Status")
	fmt.Println(strings.Repeat("-", 58))
	for i := 0; i < 5; i++ {
		status := "OK"
		switch {
		case fills[i] > 0.9:
			status = "HOT"
		case fills[i] > 0.75:
			status = "WARM"
		}
		fmt.Printf("  %-6d %-9.0f %-8.0f %-9.0f %-9s %s\n",
			i+1, rates[i], fills[i]*100, succ[i]*100,
			fmt.Sprintf("%d/%d", oks[i], errs[i]), status)
	}
	fmt.Println(strings.Repeat("-", 58))
	fmt.Println("  NOTE: the rate budget is per EXIT IP, not per group.")
	fmt.Println(strings.Repeat("=", 58))
}

// PrintSourcePanel prints the health of every tracked source.
func PrintSourcePanel(h *SourceHealthTracker) {
	if h == nil || !debugEnabled() {
		return
	}
	stats := h.GetAllStats()
	if len(stats) == 0 {
		return
	}
	fmt.Println()
	fmt.Println(strings.Repeat("=", 58))
	fmt.Println("  SOURCE HEALTH")
	fmt.Println(strings.Repeat("=", 58))
	fmt.Printf("  %-40s %-8s %-9s %s\n", "Source", "Checks", "Success%", "State")
	fmt.Println(strings.Repeat("-", 58))
	for name, info := range stats {
		state := "HEALTHY"
		if !info.isHealthy {
			state = "DEGRADED"
		}
		short := name
		if len(short) > 40 {
			short = short[len(short)-40:]
		}
		fmt.Printf("  %-40s %-8d %-9.0f %s\n", short, info.checks, info.successRate*100, state)
	}
	fmt.Println(strings.Repeat("=", 58))
}
