// Package main — engine tests (no network): rate limit, breaker, adaptive.
package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestTokenBucketPacing(t *testing.T) {
	// 60/min = 1/sec, burst 3: first 3 are instant, 4th waits ~1s.
	b := NewTokenBucket(60, 3)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := b.Wait(ctx); err != nil {
			t.Fatalf("burst token %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("4th token: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Fatalf("expected ~1s wait, got %v", elapsed)
	}
}

func TestTokenBucketCancel(t *testing.T) {
	b := NewTokenBucket(1, 1) // 1/min — next token is 60s away
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("first token: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); err == nil {
		t.Fatal("expected ctx deadline error")
	}
}

func TestCircuitBreakerTripAndRecover(t *testing.T) {
	c := NewCircuitBreaker(3, 500*time.Millisecond)
	// 2 blocks: still closed
	c.Report(true)
	c.Report(true)
	if c.State() != StateClosed {
		t.Fatalf("state = %v, want closed", c.State())
	}
	// 3rd block: open
	c.Report(true)
	if c.State() != StateOpen {
		t.Fatalf("state = %v, want open", c.State())
	}
	if !c.IsBlocked() {
		t.Fatal("IsBlocked = false, want true")
	}
	// Allow must block until cooldown expires
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := c.Allow(ctx); err != nil {
		t.Fatalf("allow: %v", err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("allow returned before cooldown")
	}
	// half-open probe fails → open again
	c.Report(true)
	if c.State() != StateOpen {
		t.Fatalf("state = %v, want open after failed probe", c.State())
	}
	// wait out cooldown, probe succeeds → closed
	time.Sleep(500 * time.Millisecond)
	if err := c.Allow(ctx); err != nil {
		t.Fatalf("allow: %v", err)
	}
	c.Report(false)
	if c.State() != StateClosed {
		t.Fatalf("state = %v, want closed after good probe", c.State())
	}
}

func TestCircuitBreakerResetsOnSuccess(t *testing.T) {
	c := NewCircuitBreaker(3, time.Minute)
	c.Report(true)
	c.Report(true)
	c.Report(false) // success resets the counter
	c.Report(true)
	c.Report(true)
	if c.State() != StateClosed {
		t.Fatalf("state = %v, want closed (counter reset)", c.State())
	}
}

func TestAdaptiveShrinkAndGrow(t *testing.T) {
	g := NewAdaptiveGovernor(8, 2, 16)
	g.moveGap = 0 // disable the move cooldown for deterministic multi-step moves
	// flood with errors → shrink 8 → 4 → 2
	for i := 0; i < 40; i++ {
		g.Report(false)
	}
	if w := g.Workers(); w != 2 {
		t.Fatalf("workers = %d, want 2 after error flood", w)
	}
	// flood with success → grow 2 → 4 → 8 → 16
	for i := 0; i < 80; i++ {
		g.Report(true)
	}
	if w := g.Workers(); w != 16 {
		t.Fatalf("workers = %d, want 16 after success flood", w)
	}
}

func TestRetryAfterParse(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"0.5", 500 * time.Millisecond},
		{"999999", 2 * time.Minute}, // capped
		{"", 0},
		{"garbage", 0},
	}
	for _, c := range cases {
		if got := RetryAfter(c.in); got != c.want {
			t.Fatalf("RetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestGateBackoffExponential(t *testing.T) {
	g := NewGate(90, 10, 15*time.Minute, 5*time.Second, 3)
	want := []time.Duration{5, 10, 20, 40, 60} // capped at 60
	for i, w := range want {
		if got := g.BackoffDelay(i); got != w*time.Second {
			t.Fatalf("BackoffDelay(%d) = %v, want %v", i, got, w*time.Second)
		}
	}
}

func TestNilGateLegacy(t *testing.T) {
	g := (*Gate)(nil)
	if err := g.Admit(context.Background()); err != nil {
		t.Fatalf("nil gate admit: %v", err)
	}
	g.Report(429, "") // must not panic
	// a 403 that carries a solvable jsc challenge must NOT count against
	// the breaker (it would freeze the run for the whole cooldown)
	g2 := NewGate(90, 2, time.Minute, 5*time.Second, 3)
	g2.Report(403, `<input name="jsc_token" value="abc">`)
	g2.Report(403, `<input name="jsc_token" value="abc">`)
	if g2.Breaker.State() != StateClosed {
		t.Fatal("a solvable challenge opened the circuit breaker")
	}
	// a real 403 (no challenge) must still count
	g2.Report(403, "whoa there, pardner")
	g2.Report(403, "whoa there, pardner")
	if g2.Breaker.State() != StateOpen {
		t.Fatal("real blocks did not open the circuit breaker")
	}
}

// TestAIMDRecoveryIsSlowerThanShrink proves that a block can reduce the
// multiplier faster than a clean window can restore it.
func TestAIMDRecoveryIsSlowerThanShrink(t *testing.T) {
	a := NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }

	// One blocked sample in a valid 20-sample window triggers the
	// multiplicative decrease.
	for i := 0; i < 19; i++ {
		a.Report(true)
	}
	a.Report(false)
	if got := a.Multiplier(); got != 0.85 {
		t.Fatalf("multiplier after block = %.4f, want 0.85", got)
	}

	// A clean probe advances by at most 5%, while the decrease was 15%.
	now = now.Add(aimdProbeInterval)
	probe := a.BeginRequest()
	if !probe.IsProbe() {
		t.Fatal("expected a recovery probe")
	}
	a.EndRequest(probe, true)
	got := a.Multiplier()
	if got <= 0.85 || got > 0.85*aimdProbeFactor+1e-9 {
		t.Fatalf("multiplier after clean probe = %.4f, want a small recovery step", got)
	}
	if 0.85-got >= 1.0-0.85 {
		t.Fatal("recovery grew at least as fast as the shrink")
	}

	// Normal additive recovery is deliberately slower: 39 clean samples do
	// not move the rate, and the 40th adds exactly 0.05.
	now = now.Add(3 * time.Second)
	for i := 0; i < 39; i++ {
		a.Report(true)
	}
	if got := a.Multiplier(); got != 0.8925 {
		t.Fatalf("multiplier after 39 clean samples = %.4f, want 0.8925", got)
	}
	a.Report(true)
	if got := a.Multiplier(); got != 0.9425 {
		t.Fatalf("multiplier after 40 clean samples = %.4f, want 0.9425", got)
	}
}

// TestAIMDRecoveryProbeSingleFlight proves that hundreds of concurrent
// workers cannot all claim the same recovery probe.
func TestAIMDRecoveryProbeSingleFlight(t *testing.T) {
	a := NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	for i := 0; i < 19; i++ {
		a.Report(true)
	}
	a.Report(false)

	now = now.Add(aimdProbeInterval)
	const callers = 128
	tickets := make(chan AIMDRequest, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tickets <- a.BeginRequest()
		}()
	}
	wg.Wait()
	close(tickets)

	probes := 0
	for ticket := range tickets {
		if ticket.IsProbe() {
			probes++
		}
	}
	if probes != 1 {
		t.Fatalf("recovery probes = %d, want exactly 1", probes)
	}
}

// TestAIMDBlockedRecoveryProbeShrinksImmediately proves that a failed trial
// immediately backs the rate off instead of waiting for another sample window.
func TestAIMDBlockedRecoveryProbeShrinksImmediately(t *testing.T) {
	a := NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	for i := 0; i < 19; i++ {
		a.Report(true)
	}
	a.Report(false)
	before := a.Multiplier()

	now = now.Add(aimdProbeInterval)
	probe := a.BeginRequest()
	if !probe.IsProbe() {
		t.Fatal("expected a recovery probe")
	}
	a.EndRequest(probe, false)
	if got := a.Multiplier(); got >= before {
		t.Fatalf("multiplier after blocked probe = %.4f, want < %.4f", got, before)
	}
}

// TestAIMDRotationResetIsGradual proves that a VPN rotation starts at the
// floor, preserves the known-good ceiling, and never restores the old rate
// in one jump.
func TestAIMDRotationResetIsGradual(t *testing.T) {
	a := NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	for i := 0; i < 19; i++ {
		a.Report(true)
	}
	a.Report(false)
	ceiling := a.RecoveryCeiling()

	a.ResetForRotation()
	if got := a.Multiplier(); got != 0.5 {
		t.Fatalf("multiplier after rotation = %.4f, want 0.5", got)
	}
	if got := a.RecoveryCeiling(); got != ceiling {
		t.Fatalf("recovery ceiling after rotation = %.4f, want %.4f", got, ceiling)
	}

	probe := a.BeginRequest()
	if !probe.IsProbe() {
		t.Fatal("expected the first post-rotation request to be the probe")
	}
	if probe.ProbeMultiplier() >= ceiling {
		t.Fatalf("probe multiplier %.4f jumped to ceiling %.4f", probe.ProbeMultiplier(), ceiling)
	}
	a.EndRequest(probe, true)
	if got := a.Multiplier(); got >= ceiling {
		t.Fatalf("post-rotation multiplier %.4f jumped to ceiling %.4f", got, ceiling)
	}
}

// TestAIMDStaleOutcomeIgnoredAfterRotation proves that an in-flight request
// from the previous exit IP cannot contaminate the fresh controller state.
func TestAIMDStaleOutcomeIgnoredAfterRotation(t *testing.T) {
	a := NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	old := a.BeginRequest()
	a.ResetForRotation()
	a.EndRequest(old, false)
	if got := a.Multiplier(); got != 0.5 {
		t.Fatalf("multiplier after stale blocked outcome = %.4f, want 0.5", got)
	}
}

// TestAIMDWindowCountsOnlyValidSamples proves that unused ring-buffer slots
// are not counted as blocks on a fresh controller.
func TestAIMDWindowCountsOnlyValidSamples(t *testing.T) {
	a := NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	for i := 0; i < 20; i++ {
		a.Report(true)
	}
	if got := a.Multiplier(); got != 1.0 {
		t.Fatalf("multiplier after 20 clean samples = %.4f, want 1.0", got)
	}
	if got := a.BlockRatio(); got != 0 {
		t.Fatalf("block ratio after 20 clean samples = %.4f, want 0", got)
	}
}
