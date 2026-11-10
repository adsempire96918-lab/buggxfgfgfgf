// T-07 — each session scales its own rate by its health score; sessions never
// affect each other and the per-IP bucket still caps the total.
package main

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestSessionAdaptiveRate is the acceptance test of T-07.
func TestSessionAdaptiveRate(t *testing.T) {
	const base = 72.0
	clean := &Session{Name: "clean"}
	bad := &Session{Name: "bad"}
	pool := NewSessionPool([]*Session{clean, bad}, base, 2)

	if clean.Bucket == bad.Bucket {
		t.Fatal("the two sessions share one bucket")
	}
	if got := clean.EffectiveRate(); !almostEqual(got, base) {
		t.Fatalf("fresh session rate = %.2f, want %.2f", got, base)
	}

	// a clean streak raises the rate, capped at 1.5x
	for i := 0; i < 20; i++ {
		clean.Report(true)
	}
	if got, want := clean.EffectiveRate(), base*sessionRateMaxFactor; !almostEqual(got, want) {
		t.Fatalf("clean session rate = %.2f, want %.2f (1.5x cap)", got, want)
	}
	// the OTHER session must be untouched: no cross-talk between sessions
	if got := bad.EffectiveRate(); !almostEqual(got, base) {
		t.Fatalf("the blocked session rate moved to %.2f - a session changed another session", got)
	}

	// a block streak lowers the rate, floored at 0.5x
	for i := 0; i < 5; i++ {
		bad.Report(false)
	}
	if got, want := bad.EffectiveRate(), base*sessionRateMinFactor; !almostEqual(got, want) {
		t.Fatalf("blocked session rate = %.2f, want %.2f (0.5x floor)", got, want)
	}
	if got := clean.EffectiveRate(); !almostEqual(got, base*sessionRateMaxFactor) {
		t.Fatalf("the clean session changed to %.2f while the other was blocked", got)
	}
	if bad.ScoreValue() != -3 {
		t.Fatalf("score = %d, want the -3 floor", bad.ScoreValue())
	}

	// a clean session really is faster than a bad one
	if clean.EffectiveRate() <= bad.EffectiveRate() {
		t.Fatalf("clean rate %.2f <= blocked rate %.2f", clean.EffectiveRate(), bad.EffectiveRate())
	}

	// the per-IP budget is a different object and stays at its own rate
	lim := NewIPLimiterGrouped(1200, 2, 5)
	ipBucket := lim.Bucket("direct", 0)
	if ipBucket == clean.Bucket || ipBucket == bad.Bucket {
		t.Fatal("the per-IP bucket is the same object as a session bucket")
	}
	if got := ipBucket.Rate(); !almostEqual(got, 1200) {
		t.Fatalf("per-IP rate = %.2f, want 1200 (session scoring must not move it)", got)
	}

	// a VPN rotation lifts the penalty -> the rate returns to the base rate
	pool.ClearPenalty()
	if got := bad.EffectiveRate(); !almostEqual(got, base) {
		t.Fatalf("rate after ClearPenalty = %.2f, want %.2f", got, base)
	}
}

// The multiplier band must hold for every reachable score.
func TestSessionRateFactorBand(t *testing.T) {
	cases := map[int64]float64{
		-3: 0.5, -2: 1.0 + 0.5*(-2)/3.0, -1: 1.0 + 0.5*(-1)/3.0,
		0: 1.0, 5: 1.25, 10: 1.5, 99: 1.5,
	}
	for score, want := range cases {
		if got := sessionRateFactor(score); !almostEqual(got, want) {
			t.Fatalf("factor(%d) = %.3f, want %.3f", score, got, want)
		}
		if got := sessionRateFactor(score); got < sessionRateMinFactor || got > sessionRateMaxFactor {
			t.Fatalf("factor(%d) = %.3f is outside the %.1fx-%.1fx band", score, got, sessionRateMinFactor, sessionRateMaxFactor)
		}
	}
}
