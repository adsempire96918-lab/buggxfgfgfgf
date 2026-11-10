// Package main — adaptive concurrency.
// Watches a sliding window of per-user outcomes and recommends a worker
// count: halve when errors dominate, double when healthy (rate-limited moves).
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// AdaptiveGovernor tunes the worker pool from observed success rates.
type AdaptiveGovernor struct {
	mu       sync.Mutex
	window   []bool // true = ok, recent N
	size     int
	maxW     int
	minW     int
	errHi    float64 // above this → shrink
	errLo    float64 // below this → grow
	lastMove time.Time
	moveGap  time.Duration
	cur      int
	reason   string
	changed  chan AdaptiveTarget
}

func NewAdaptiveGovernor(start, minW, maxW int) *AdaptiveGovernor {
	if minW <= 0 {
		minW = 2
	}
	if maxW < minW {
		maxW = minW
	}
	if start < minW {
		start = minW
	}
	if start > maxW {
		start = maxW
	}
	return &AdaptiveGovernor{
		window:  make([]bool, 0, 64),
		size:    64,
		maxW:    maxW,
		minW:    minW,
		errHi:   0.30,
		errLo:   0.10,
		moveGap: 10 * time.Second,
		cur:     start,
		reason:  "initial",
		changed: make(chan AdaptiveTarget, 1),
	}
}

// Report records one completed user check (per task, not per HTTP call).
func (g *AdaptiveGovernor) Report(ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.window = append(g.window, ok)
	if len(g.window) > g.size {
		g.window = g.window[len(g.window)-g.size:]
	}
	if time.Since(g.lastMove) < g.moveGap || len(g.window) < 10 {
		return
	}
	errs := 0
	for _, ok := range g.window {
		if !ok {
			errs++
		}
	}
	rate := float64(errs) / float64(len(g.window))
	if rate > g.errHi && g.cur > g.minW {
		g.cur /= 2
		if g.cur < g.minW {
			g.cur = g.minW
		}
		g.lastMove = time.Now()
		g.reason = fmt.Sprintf("error rate %.1f%%", rate*100)
		g.window = g.window[:0]
		g.notifyChangedLocked()
	} else if rate < g.errLo && g.cur < g.maxW {
		g.cur *= 2
		if g.cur > g.maxW {
			g.cur = g.maxW
		}
		g.lastMove = time.Now()
		g.reason = fmt.Sprintf("healthy (error rate %.1f%%)", rate*100)
		g.window = g.window[:0]
		g.notifyChangedLocked()
	}
}

// Workers returns the recommended worker count.
func (g *AdaptiveGovernor) Workers() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cur
}

// Reason returns the reason for the current worker recommendation.
func (g *AdaptiveGovernor) Reason() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reason
}

// AdaptiveTarget is one immutable worker recommendation.
type AdaptiveTarget struct {
	Workers int
	Reason  string
}

// Changed returns a coalesced notification channel. The channel carries the
// latest target value, so a coalesced notification can never lose the new
// worker count or its reason.
func (g *AdaptiveGovernor) Changed() <-chan AdaptiveTarget {
	return g.changed
}

func (g *AdaptiveGovernor) notifyChangedLocked() {
	target := AdaptiveTarget{Workers: g.cur, Reason: g.reason}
	select {
	case g.changed <- target:
		return
	default:
	}
	// Replace a pending stale notification with the newest target. The
	// controller may have consumed the previous value concurrently, so the
	// drain is best-effort and the final send is non-blocking.
	select {
	case <-g.changed:
	default:
	}
	select {
	case g.changed <- target:
	default:
	}
}

// AccountOutcome is the terminal result of one account check.
type AccountOutcome uint8

const (
	// OutcomeNone means no finished account (invalid input or cancellation).
	OutcomeNone AccountOutcome = iota
	// OutcomeClean means Reddit answered definitively: hit, 404 or suspended.
	OutcomeClean
	// OutcomeBlocked means the exit IP or source was refused/throttled.
	OutcomeBlocked
	// OutcomeFailed means the account ended in an ordinary transport/parse error.
	OutcomeFailed
)

// PoolTargetSource supplies a live worker target and a coalesced change signal.
type PoolTargetSource interface {
	Workers() int
	Changed() <-chan AdaptiveTarget
	Reason() string
}

// PoolTask owns one job until it returns. A task is never dropped during a
// resize: the worker that accepted it always runs it to completion.
type PoolTask func(context.Context) AccountOutcome

// PoolNextFunc returns the next task. false means the source is permanently
// exhausted; temporary lack of work must be handled inside the function.
type PoolNextFunc func(context.Context) (PoolTask, bool)

type poolWorker struct {
	id         uint64
	retire     chan struct{}
	retireOnce sync.Once
	retiring   atomic.Bool
	busy       atomic.Bool
}

// PoolSnapshot is a point-in-time view of the pool.
type PoolSnapshot struct {
	Target int
	Live   int
	Busy   int
}

// ResizablePool is a runtime-resizable worker pool. The governor changes the
// target; idle workers retire on shrink and new workers spawn on growth.
type ResizablePool struct {
	maxWorkers int
	target     PoolTargetSource
	next       PoolNextFunc
	reporter   func(AccountOutcome)
	jobs       chan PoolTask
	ctx        context.Context

	mu      sync.Mutex
	workers map[uint64]*poolWorker
	nextID  uint64
	targetN int
	closing bool

	busy  atomic.Int64
	wg    sync.WaitGroup
	exits chan uint64
}

// NewResizablePool creates a pool bounded by maxWorkers.
func NewResizablePool(maxWorkers int, target PoolTargetSource, next PoolNextFunc, reporter func(AccountOutcome)) *ResizablePool {
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	return &ResizablePool{
		maxWorkers: maxWorkers,
		target:     target,
		next:       next,
		reporter:   reporter,
		jobs:       make(chan PoolTask),
		workers:    make(map[uint64]*poolWorker),
		exits:      make(chan uint64, maxWorkers+1),
	}
}

// Run starts the pool and blocks until the source is exhausted and all
// accepted jobs have finished.
func (p *ResizablePool) Run(ctx context.Context) {
	p.ctx = ctx
	initial := p.maxWorkers
	if p.target != nil {
		initial = p.target.Workers()
	}
	p.mu.Lock()
	p.targetN = initial
	p.mu.Unlock()
	p.setTarget(initial, "initial")

	controllerCtx, stopController := context.WithCancel(ctx)
	defer stopController()
	controllerDone := make(chan struct{})
	go func() {
		defer close(controllerDone)
		for {
			var changed <-chan AdaptiveTarget
			if p.target != nil {
				changed = p.target.Changed()
			}
			select {
			case <-controllerCtx.Done():
				return
			case target := <-changed:
				p.setTarget(target.Workers, target.Reason)
			case <-p.exits:
				p.setTarget(p.target.Workers(), p.target.Reason())
			}
		}
	}()

	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for {
			task, ok := p.next(ctx)
			if !ok {
				break
			}
			select {
			case p.jobs <- task:
			case <-ctx.Done():
				p.mu.Lock()
				p.closing = true
				p.mu.Unlock()
				close(p.jobs)
				return
			}
		}
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		close(p.jobs)
	}()

	<-producerDone
	// The source is exhausted. Mark the pool closing BEFORE stopping the
	// controller so worker-exit events cannot trigger replacement workers
	// against the already-closed jobs channel.
	p.mu.Lock()
	p.closing = true
	p.mu.Unlock()
	stopController()
	<-controllerDone
	p.mu.Lock()
	for _, w := range p.workers {
		w.retireOnce.Do(func() { close(w.retire) })
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// Snapshot returns the current target, live worker count and busy count.
func (p *ResizablePool) Snapshot() PoolSnapshot {
	p.mu.Lock()
	live := len(p.workers)
	target := p.targetN
	p.mu.Unlock()
	return PoolSnapshot{
		Target: target,
		Live:   live,
		Busy:   int(p.busy.Load()),
	}
}

func (p *ResizablePool) setTarget(target int, reason string) {
	if target < 1 {
		target = 1
	}
	if target > p.maxWorkers {
		target = p.maxWorkers
	}

	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	old := p.targetN
	p.targetN = target
	workers := make([]*poolWorker, 0, len(p.workers))
	for _, w := range p.workers {
		workers = append(workers, w)
	}
	p.mu.Unlock()

	if old != target && old != 0 {
		fmt.Printf("pool: %d -> %d workers (%s)\n", old, target, reason)
	}

	// Count only workers that have not been told to retire. A retiring busy
	// worker still owns its current task and exits when that task completes.
	active := 0
	for _, w := range workers {
		if !w.retiring.Load() {
			active++
		}
	}
	if active < target {
		// Retiring busy workers still count toward the hard maximum until
		// their current task finishes. Spawn only into genuinely free slots;
		// the worker-exit event will reconcile the remaining growth later.
		slots := p.maxWorkers - len(workers)
		if slots > target-active {
			slots = target - active
		}
		for i := 0; i < slots; i++ {
			p.spawnWorker()
		}
		return
	}
	if active <= target {
		return
	}

	// Retire the excess workers. Busy workers finish their current task and
	// then exit; idle workers exit immediately. A task is never interrupted.
	excess := active - target
	p.mu.Lock()
	for _, w := range workers {
		if excess == 0 {
			break
		}
		if w.retiring.Load() {
			continue
		}
		w.retiring.Store(true)
		w.retireOnce.Do(func() { close(w.retire) })
		excess--
	}
	p.mu.Unlock()
}

func (p *ResizablePool) spawnWorker() {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return
	}
	p.nextID++
	w := &poolWorker{id: p.nextID, retire: make(chan struct{})}
	p.workers[w.id] = w
	p.wg.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			delete(p.workers, w.id)
			p.mu.Unlock()
			select {
			case p.exits <- w.id:
			default:
			}
		}()
		for {
			select {
			case <-w.retire:
				return
			case task, ok := <-p.jobs:
				if !ok {
					return
				}
				w.busy.Store(true)
				p.busy.Add(1)
				outcome := task(p.ctx)
				p.busy.Add(-1)
				if p.reporter != nil && outcome != OutcomeNone {
					p.reporter(outcome)
				}
				w.busy.Store(false)
				if w.retiring.Load() {
					return
				}
			}
		}
	}()
}
