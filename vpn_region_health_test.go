// T-09 — the VPN router prefers the HEALTHIEST region instead of the next one
// in the list, and a region that just burned us drops to the back of the queue.
package main

import (
	"strings"
	"testing"
	"time"
)

// TestVPNPrefersHealthyRegion is the acceptance test of T-09.
func TestVPNPrefersHealthyRegion(t *testing.T) {
	v := NewVPNController(VPNConfig{Rotate: true})
	regions := []string{"usa-east", "germany-1", "japan-tokyo"}

	// The naive pick would be "usa-east" (first in the pool). It just blocked,
	// so the router must take the region with the better record instead.
	v.RecordRegionOutcome("usa-east", false)
	v.RecordRegionOutcome("germany-1", true)
	v.RecordRegionOutcome("germany-1", true)

	got := v.nextRegion(regions, "singapore-jurong")
	if got != "germany-1" {
		t.Fatalf("nextRegion = %q, want the healthiest region germany-1", got)
	}
	if s := v.RegionScore("usa-east"); s >= v.RegionScore("japan-tokyo") {
		t.Fatalf("a blocked region scores %.2f, untried japan scores %.2f - it must rank lower",
			s, v.RegionScore("japan-tokyo"))
	}

	// A region that was blocked is not picked again while a healthy one exists.
	only := []string{"usa-east", "japan-tokyo"}
	if got := v.nextRegion(only, "germany-1"); got != "japan-tokyo" {
		t.Fatalf("nextRegion = %q, want japan-tokyo (usa-east just blocked)", got)
	}

	// ... and it becomes eligible again once the cooldown passed.
	v.mu.Lock()
	h := v.health["usa-east"]
	h.lastBlock = time.Now().Add(-2 * regionBlockCooldown)
	v.mu.Unlock()
	if s := v.RegionScore("usa-east"); s <= 0 {
		t.Fatalf("score after the cooldown = %.2f, want it back in the normal band", s)
	}

	// The different-country picker honours the same scores.
	multi := []string{"usa-new-york", "usa-boston", "germany-berlin", "japan-tokyo"}
	v.RecordRegionOutcome("germany-berlin", false)
	got = v.nextRegionDifferentCountry(multi, "usa-new-york")
	if got != "japan-tokyo" {
		t.Fatalf("nextRegionDifferentCountry = %q, want japan-tokyo (germany-berlin blocked)", got)
	}

	// An untried pool keeps the plain pool order (the old behaviour).
	fresh := NewVPNController(VPNConfig{Rotate: true})
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		seen[fresh.nextRegion([]string{"a", "b", "c"}, "b")] = true
	}
	if seen["b"] || len(seen) != 2 {
		t.Fatalf("fresh pool did not cycle: %v", seen)
	}
}

// The rotation itself must skip the burned region and score the winner up.
func TestVPNRotationSkipsBlockedRegion(t *testing.T) {
	v := testVPNController()
	f := &fakeCtl{
		state:     "Connected",
		regions:   []string{"usa-east", "germany-1", "japan-tokyo"},
		region:    "singapore-jurong",
		connectOK: true,
	}
	v.run = f.run
	v.ipNow = (&scriptedIP{vals: []string{"103.1.1.1", "134.2.2.2"}}).next

	// germany-1 served a block page a moment ago
	v.RecordRegionOutcome("germany-1", false)

	if !v.Trigger("test") {
		t.Fatal("trigger refused")
	}
	waitRotationDone(t, v)

	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "connect germany-1") {
			t.Fatalf("the rotation reconnected to the blocked region: %v", f.callLog())
		}
	}
	hist := v.History()
	if len(hist) != 1 || !hist[0].OK {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].ToRegion == "" || hist[0].ToRegion == "germany-1" {
		t.Fatalf("rotated to %q, want a healthy region", hist[0].ToRegion)
	}
	if s := v.RegionScore(hist[0].ToRegion); s <= 0.5 {
		t.Fatalf("the region that worked scores %.2f, want > 0.5 after a successful rotation", s)
	}
}

// TestVPNABlockPageScoresTheCurrentRegion: the block report must burn the
// region we are actually on, otherwise the next rotation walks right back.
func TestVPNABlockPageScoresTheCurrentRegion(t *testing.T) {
	v := testVPNController()
	f := &fakeCtl{
		state:     "Connected",
		regions:   []string{"usa-east", "germany-1"},
		region:    "usa-east", // the region that served the block page
		connectOK: true,
	}
	v.run = f.run
	v.ipNow = (&scriptedIP{vals: []string{"1.1.1.1", "2.2.2.2"}}).next

	if !v.ReportIPBlocked("blocked page") {
		t.Fatal("ReportIPBlocked refused")
	}
	waitRotationDone(t, v)

	if s := v.RegionScore("usa-east"); s >= v.RegionScore("germany-1") {
		t.Fatalf("blocked region usa-east scores %.2f, germany %.2f", s, v.RegionScore("usa-east"))
	}
	stats := v.RegionStats()
	if len(stats) < 1 {
		t.Fatalf("no region stats recorded: %+v", stats)
	}
}

// PathHealth is the read-only bridge the rotation reason uses.
func TestSourcePathHealth(t *testing.T) {
	h := NewSourceHealthTracker()
	h.RecordCheck("s1", true, 0)
	h.RecordCheck("s1", true, 0)
	h.RecordCheck("s2", false, 0)
	h.RecordCheck("s2", false, 0)

	healthy, total, ratio := h.PathHealth()
	if total != 2 || healthy < 1 {
		t.Fatalf("PathHealth = %d/%d (%.2f)", healthy, total, ratio)
	}
	if ratio <= 0 || ratio > 1 {
		t.Fatalf("ratio = %.2f, want 0..1", ratio)
	}
	var nilTracker *SourceHealthTracker
	if healthy, total, ratio := nilTracker.PathHealth(); healthy != 0 || total != 0 || ratio != 0 {
		t.Fatal("nil tracker must report 0")
	}
}
