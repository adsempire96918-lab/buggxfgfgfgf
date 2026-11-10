// Package main - opt-in REAL VPN rotation test. Skipped unless VPN=1 is set:
// it disconnects the real ExpressVPN, connects to the next pool region and
// proves the exit IP changed. The caller restores the previous location.
package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveVPNRotation rotates the real ExpressVPN once and checks the result.
func TestLiveVPNRotation(t *testing.T) {
	if os.Getenv("VPN") == "" {
		t.Skip("set VPN=1 to run the REAL VPN rotation test")
	}
	v := NewVPNController(VPNConfig{
		Path:     DefaultExpressVPN,
		Rotate:   true,
		Gap:      time.Second,
		VerifyIP: true,
	})
	if !v.Enabled() {
		t.Skip("expressvpnctl not installed at the default path")
	}
	before, err := v.ipNow(context.Background())
	if err != nil {
		t.Logf("warning: pre-rotation IP probe failed: %v", err)
	}
	t.Logf("IP before: %s", before)

	if !v.Trigger("live test") {
		t.Fatal("trigger refused")
	}
	deadline := time.Now().Add(3 * time.Minute)
	for v.IsPaused() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if v.IsPaused() {
		t.Fatal("rotation did not finish within 3 minutes")
	}
	if v.Rotations() != 1 {
		t.Fatalf("rotations = %d", v.Rotations())
	}
	hist := v.History()
	if len(hist) != 1 || !hist[0].OK {
		t.Fatalf("rotation failed: %+v", hist)
	}
	t.Logf("rotation: %+v", hist[0])

	after, err := v.ipNow(context.Background())
	if err != nil {
		t.Logf("warning: post-rotation IP probe failed: %v", err)
	}
	t.Logf("IP after: %s", after)
	if before != "" && after != "" && after == before {
		t.Fatal("exit IP did not change")
	}
	if hist[0].ToRegion == hist[0].FromRegion {
		t.Fatal("region did not change")
	}
}
