// Package main - AIMD rate controller.
//
// Like TCP congestion control: when Reddit is clean the rate grows, when
// blocks appear the rate shrinks. Recovery is deliberately much slower than
// the decrease, and a recovery probe is single-flight so hundreds of workers
// cannot all probe Reddit at the same instant.
package main

import (
	"sync"
	"time"
)

const (
	aimdDecreaseFactor = 0.85
	aimdIncreaseStep   = 0.05
	aimdProbeFactor    = 1.05
	aimdProbeInterval  = 10 * time.Second
	aimdDefaultUpEvery = 40
	aimdWindowSize     = 100
)

// AIMDMode describes how the controller is currently recovering.
type AIMDMode uint8

const (
	// AIMDSteady is the normal state: clean samples may grow the rate.
	AIMDSteady AIMDMode = iota
	// AIMDRecovery means the controller was shrunk. Only a guarded probe may
	// grow it until it reaches the last known-good multiplier.
	AIMDRecovery
)

// AIMDRequest is an opaque request ticket. BeginRequest reserves at most one
// recovery probe; EndRequest applies that request's outcome to the controller.
type AIMDRequest struct {
	epoch     uint64
	probeID   uint64
	probe     bool
	probeMult float64
}

// IsProbe reports whether this request owns the single recovery probe.
func (r AIMDRequest) IsProbe() bool { return r.probe }

// ProbeMultiplier returns the trial multiplier reserved for a probe request.
func (r AIMDRequest) ProbeMultiplier() float64 { return r.probeMult }

// AIMD watches request outcomes and produces a rate multiplier.
type AIMD struct {
	mu          sync.Mutex
	mult        float64
	min         float64
	max         float64
	samples     []bool
	cleanStreak int
	lastAdj     time.Time
	upEvery     int
	downAt      float64

	mode            AIMDMode
	recoveryCeiling float64
	nextProbeAt     time.Time
	probeInFlight   bool
	probeStartedAt  time.Time
	probeSeq        uint64
	currentProbeID  uint64
	epoch           uint64
	now             func() time.Time
}

// NewAIMD creates a controller. upEvery is the number of clean samples
// required before an additive increase; downAt is the blocked ratio that
// triggers a multiplicative decrease.
func NewAIMD(min, max float64, upEvery int, downAt float64) *AIMD {
	if min <= 0 {
		min = 0.5
	}
	if max <= min {
		max = 3.0
		if max <= min {
			max = min * 2
		}
	}
	if upEvery <= 0 {
		upEvery = aimdDefaultUpEvery
	}
	if downAt <= 0 {
		downAt = 0.02
	}
	return &AIMD{
		mult:            1.0,
		min:             min,
		max:             max,
		upEvery:         upEvery,
		downAt:          downAt,
		recoveryCeiling: 1.0,
		now:             time.Now,
	}
}

// BeginRequest reserves a recovery probe when one is due. In steady state it
// returns an ordinary ticket. The mutex makes the reservation single-flight:
// among any number of concurrent callers, exactly one can own a probe.
func (a *AIMD) BeginRequest() AIMDRequest {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.clock()
	if a.mode == AIMDRecovery && !now.Before(a.nextProbeAt) &&
		(!a.probeInFlight || now.Sub(a.probeStartedAt) >= aimdProbeInterval) {
		a.probeSeq++
		a.currentProbeID = a.probeSeq
		a.probeInFlight = true
		a.probeStartedAt = now
		a.nextProbeAt = now.Add(aimdProbeInterval)

		target := a.mult * aimdProbeFactor
		if target > a.recoveryCeiling {
			target = a.recoveryCeiling
		}
		if target > a.max {
			target = a.max
		}
		if target < a.mult {
			target = a.mult
		}
		return AIMDRequest{
			epoch:     a.epoch,
			probeID:   a.currentProbeID,
			probe:     true,
			probeMult: target,
		}
	}
	return AIMDRequest{epoch: a.epoch}
}

// EndRequest applies one request outcome. A ticket from before a VPN
// rotation is ignored, so an old in-flight request cannot contaminate the
// fresh exit IP's controller state.
func (a *AIMD) EndRequest(req AIMDRequest, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.epoch != a.epoch {
		return
	}
	now := a.clock()

	if req.probe {
		if req.probeID == 0 || req.probeID != a.currentProbeID {
			return
		}
		a.currentProbeID = 0
		a.probeInFlight = false
		if !ok {
			a.decreaseLocked(now, req.probeMult, true)
			return
		}
		a.mult = req.probeMult
		// A clean probe proves that this small step is acceptable. Resume
		// the normal, much slower additive growth path. The probe target was
		// capped by the last known-good ceiling, so this cannot jump past a
		// previously hot rate.
		a.mode = AIMDSteady
		a.nextProbeAt = now.Add(aimdProbeInterval)
		a.lastAdj = now
		a.clearSamplesLocked()
		return
	}

	if a.mode == AIMDRecovery {
		// While recovering, only the guarded probe may move the rate. If
		// every ordinary worker reacted to the same block wave, hundreds of
		// goroutines would multiply-decrease the controller in milliseconds.
		return
	}

	if !ok {
		a.addSampleLocked(false)
		a.cleanStreak = 0
		if a.shouldDecreaseLocked() {
			a.decreaseLocked(now, a.mult, false)
		}
		return
	}

	a.addSampleLocked(true)
	a.cleanStreak++
	if a.shouldGrowLocked(now) {
		a.mult += aimdIncreaseStep
		if a.mult > a.max {
			a.mult = a.max
		}
		if a.mult > a.recoveryCeiling {
			a.recoveryCeiling = a.mult
		}
		a.lastAdj = now
		a.clearSamplesLocked()
		a.cleanStreak = 0
	}
}

// Report is the compatibility helper for callers that do not need a probe
// ticket. New concurrent code should use BeginRequest and EndRequest.
func (a *AIMD) Report(ok bool) {
	a.EndRequest(a.BeginRequest(), ok)
}

// Multiplier returns the current rate multiplier.
func (a *AIMD) Multiplier() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mult
}

// Mode returns the current controller mode.
func (a *AIMD) Mode() AIMDMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// RecoveryCeiling returns the last known-good multiplier. A VPN rotation
// preserves this value, but never restores it in one jump.
func (a *AIMD) RecoveryCeiling() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recoveryCeiling
}

// BlockRatio returns the blocked ratio of the current valid sample window.
func (a *AIMD) BlockRatio() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.samples) == 0 {
		return 0
	}
	blocked := 0
	for _, v := range a.samples {
		if !v {
			blocked++
		}
	}
	return float64(blocked) / float64(len(a.samples))
}

// ResetForRotation starts a fresh exit IP at the conservative floor while
// preserving the last known-good ceiling. Recovery then climbs one guarded
// probe at a time instead of jumping straight back to a hot rate.
func (a *AIMD) ResetForRotation() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.epoch++
	a.mult = a.min
	a.mode = AIMDRecovery
	a.nextProbeAt = a.clock()
	a.probeInFlight = false
	a.probeStartedAt = time.Time{}
	a.currentProbeID = 0
	a.clearSamplesLocked()
	a.lastAdj = a.clock()
	if a.recoveryCeiling < a.min {
		a.recoveryCeiling = a.min
	}
}

// Reset sets the multiplier directly and clears the measurement window.
// It is retained for API compatibility; VPN rotation uses ResetForRotation.
func (a *AIMD) Reset(mult float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.epoch++
	if mult <= 0 {
		mult = a.min
	}
	if mult > a.max {
		mult = a.max
	}
	a.mult = mult
	a.mode = AIMDSteady
	a.nextProbeAt = time.Time{}
	a.probeInFlight = false
	a.probeStartedAt = time.Time{}
	a.currentProbeID = 0
	a.clearSamplesLocked()
	a.lastAdj = a.clock()
}

func (a *AIMD) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *AIMD) addSampleLocked(ok bool) {
	a.samples = append(a.samples, ok)
	if len(a.samples) > aimdWindowSize {
		copy(a.samples, a.samples[len(a.samples)-aimdWindowSize:])
		a.samples = a.samples[:aimdWindowSize]
	}
}

func (a *AIMD) clearSamplesLocked() {
	a.samples = a.samples[:0]
	a.cleanStreak = 0
}

func (a *AIMD) shouldDecreaseLocked() bool {
	if len(a.samples) < 20 {
		return false
	}
	blocked := 0
	for _, v := range a.samples {
		if !v {
			blocked++
		}
	}
	return float64(blocked)/float64(len(a.samples)) > a.downAt
}

func (a *AIMD) shouldGrowLocked(now time.Time) bool {
	if len(a.samples) < a.upEvery || a.cleanStreak < a.upEvery {
		return false
	}
	for _, v := range a.samples {
		if !v {
			return false
		}
	}
	return now.Sub(a.lastAdj) > 2*time.Second
}

func (a *AIMD) decreaseLocked(now time.Time, attempted float64, probeFailed bool) {
	previous := a.mult
	base := previous
	if attempted > base {
		base = attempted
	}
	a.mult = base * aimdDecreaseFactor
	if a.mult < a.min {
		a.mult = a.min
	}
	// The failed attempt itself is not a known-good ceiling. Keep the last
	// known-good ceiling across an ordinary decrease. If the guarded probe
	// itself failed, lower the ceiling to the rate that was clean before
	// that trial so the failed step is not retried immediately.
	if probeFailed {
		a.recoveryCeiling = previous
	}
	a.mode = AIMDRecovery
	a.nextProbeAt = now.Add(aimdProbeInterval)
	a.lastAdj = now
	a.clearSamplesLocked()
	a.currentProbeID = 0
	a.probeInFlight = false
}
