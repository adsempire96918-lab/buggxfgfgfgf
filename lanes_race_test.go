// Package main - race regression test for lane session rotation.
// Run with: go test -race -run TestLaneRotationRace
//
// With several workers and fewer lanes, multiple goroutines share one lane:
// one runs Fetch (reading Gate.Bucket through Gate.Admit) while another calls
// RotateIfNeeded/SetSession (which used to write Gate.Bucket unsynchronized)
// and a third calls LaneManager.RotateLane (which read lane fields directly).
package main

import (
	"sync"
	"testing"
	"time"
)

// TestLaneRotationRace pounds a lane with concurrent fetch-side gate reads,
// session rotations and manager-driven rotations. The race detector proves
// the locking; this must PASS with -race (and is deadlock-free by design:
// readers stop only after the BOUNDED rotation loops finish).
func TestLaneRotationRace(t *testing.T) {
	sessions := make([]*Session, 8)
	for i := range sessions {
		sessions[i] = &Session{Name: "s", Bucket: NewTokenBucket(12000, 2)}
	}
	pool := NewSessionPool(sessions, 12000, 2)
	ipLim := NewIPLimiterGrouped(12000, 8, 5)
	mgr, err := buildLanes(pool, nil, 2, ipLim, 5*time.Second, 5, 0, 12000, 2, 1, nil)
	if err != nil {
		t.Fatalf("buildLanes: %v", err)
	}
	lanes := mgr.All()
	if len(lanes) < 2 {
		t.Fatalf("lanes = %d, want >= 2", len(lanes))
	}
	l1, l2 := lanes[0], lanes[1]

	stop := make(chan struct{})
	var readers, rotators sync.WaitGroup

	// reader (the "fetch" side): take the lane's current gate like
	// Gate.Admit would and read its bucket pointers - continuously until stop.
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			l1.mu.Lock()
			g := l1.Gate
			l1.mu.Unlock()
			if g != nil {
				_ = g.Bucket // this read raced with RotateIfNeeded's write
				_ = g.IPBucket
			}
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	// rotator 1: recycle lane 1 sessions as fast as possible (bounded).
	rotators.Add(1)
	go func() {
		defer rotators.Done()
		for i := 0; i < 200; i++ {
			l1.RotateIfNeeded()
			if s := pool.Next(); s != nil {
				_ = l1.SetSession(s, 5*time.Second)
			}
		}
	}()

	// rotator 2: manager-driven rotation of lane 2 (reads every lane's
	// session under the mgr mutex - including lane 1 while it rotates).
	rotators.Add(1)
	go func() {
		defer rotators.Done()
		for i := 0; i < 200; i++ {
			mgr.RotateLane(l2, pool.sessionsSnapshot(), 5*time.Second)
		}
	}()

	rotators.Wait() // bounded loops always finish
	close(stop)     // then release the reader
	readers.Wait()

	if l1.IsBlocked() {
		t.Log("lane 1 blocked (acceptable on a crowded mock pool)")
	}
}

// TestLaneRotationDoesNotMutateSharedGate is the deterministic proof of the
// rotation data race fix: the OLD client+gate of a lane must be completely
// untouched by RotateIfNeeded, and the new client must draw its budget from
// the NEW session's bucket.
func TestLaneRotationDoesNotMutateSharedGate(t *testing.T) {
	sessions := []*Session{
		{Name: "old", Bucket: NewTokenBucket(600, 2)},
		{Name: "new", Bucket: NewTokenBucket(900, 2)},
	}
	pool := NewSessionPool(sessions, 600, 2)
	ipLim := NewIPLimiterGrouped(600, 2, 5)
	mgr, err := buildLanes(pool, nil, 1, ipLim, 5*time.Second, 5, 0, 600, 2, 1, nil)
	if err != nil {
		t.Fatalf("buildLanes: %v", err)
	}
	lane := mgr.All()[0]

	oldSession := lane.Session
	oldClient := lane.Client
	oldGate := lane.Gate
	oldBucket := oldGate.Bucket

	// maxPerSession=1: simulate one served user so the lane must rotate
	lane.mu.Lock()
	lane.used = lane.maxUse
	lane.mu.Unlock()
	lane.RotateIfNeeded()

	if lane.Session == oldSession {
		t.Fatal("lane did not rotate")
	}
	if lane.Client == oldClient {
		t.Fatal("lane client was not swapped")
	}
	if oldGate.Bucket != oldBucket {
		t.Fatal("SHARED GATE MUTATED during rotation (the data race is back)")
	}
	if lane.Gate == oldGate {
		t.Fatal("lane kept the old shared gate instead of a fresh one")
	}
	if lane.Gate.Bucket != lane.Session.Bucket {
		t.Fatal("new gate does not use the NEW session's bucket")
	}
	if oldClient.Gate != oldGate {
		t.Fatal("old client's gate pointer was touched")
	}
}

// TestLaneSetSessionUsesNewSessionBucket: a manager-driven swap must also
// move the lane onto the new session's own rate budget without mutating the
// old gate.
func TestLaneSetSessionUsesNewSessionBucket(t *testing.T) {
	lane := &Lane{}
	c, err := newClient("", 5*time.Second, 0)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	oldGate := &Gate{Bucket: NewTokenBucket(600, 2), Breaker: NewCircuitBreaker(5, time.Minute)}
	c.Gate = oldGate
	lane.Client = c
	lane.Gate = oldGate

	fresh := &Session{Name: "fresh", Bucket: NewTokenBucket(900, 2)}
	if err := lane.SetSession(fresh, 5*time.Second); err != nil {
		t.Fatalf("SetSession: %v", err)
	}
	if lane.Gate.Bucket != fresh.Bucket {
		t.Fatal("lane did not adopt the new session's bucket")
	}
	if lane.Gate.Breaker != oldGate.Breaker {
		t.Fatal("breaker must survive a session swap")
	}
}

// sessionsSnapshot returns the pool's sessions (test helper).
func (p *SessionPool) sessionsSnapshot() []*Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Session{}, p.sessions...)
}
