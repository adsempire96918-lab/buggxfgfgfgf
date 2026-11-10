// Package main - multi-engine runner.
//
// The single-engine checker stops making progress when its one source gets
// rate limited. The multi-engine runner solves that: several INDEPENDENT
// engines (each with its own sessions, lanes, rate budget and source ladder)
// pull from ONE shared job queue.
//
//   - engine A: the full cookie-session pool (fast JSON API)
//   - engine B: a smaller, gentler session pool (backup)
//   - engine C: anonymous / HTML scrape (no cookie budget at all)
//
// When an engine hits a rate limit it PARKS (stops pulling jobs) instead of
// failing the whole run. The other engines keep working, so the run never
// stops. When every engine is parked at once the coordinator triggers the VPN
// rotation; after the new exit IP is ready every engine resumes.
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// JobQueue - the shared work list
// ============================================================

// JobQueue hands the accounts to the engines. It is the only thing the
// engines share, so an engine that parks does not hold up the others.
type JobQueue struct {
	mu    sync.Mutex
	users []string
	idx   int
}

// NewJobQueue creates a queue over the accounts to check.
func NewJobQueue(users []string) *JobQueue {
	return &JobQueue{users: users}
}

// Next returns the next account, or false when the queue is empty.
func (q *JobQueue) Next() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.idx >= len(q.users) {
		return "", false
	}
	u := q.users[q.idx]
	q.idx++
	return u, true
}

// Remaining returns how many accounts are still queued.
func (q *JobQueue) Remaining() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.users) - q.idx
}

// ============================================================
// Engine - one independent checker
// ============================================================

// Engine is one independent checking method.
type Engine struct {
	Name    string
	Workers int
	Rate    float64 // per-IP rate for this engine (0 = inherit)

	checker *MultiChecker
	paused  atomic.Bool
	limited atomic.Int64 // how often this engine hit a limit
	done    atomic.Int64 // accounts this engine completed
	active  atomic.Int64 // tasks assigned to this engine right now
	started time.Time
}

// Pause parks the engine: its workers stop pulling new jobs.
func (e *Engine) Pause(reason string) {
	if e.paused.CompareAndSwap(false, true) {
		e.limited.Add(1)
		fmt.Printf("  engine %-12s PAUSED (%s)\n", e.Name, reason)
	}
}

// Resume wakes the engine up.
func (e *Engine) Resume() {
	if e.paused.CompareAndSwap(true, false) {
		fmt.Printf("  engine %-12s RESUMED\n", e.Name)
	}
}

// Paused reports whether the engine is parked.
func (e *Engine) Paused() bool { return e.paused.Load() }

// ============================================================
// MultiEngineRunner
// ============================================================

// MultiEngineRunner runs several engines against one shared queue.
type MultiEngineRunner struct {
	engines    []*Engine
	queue      *JobQueue
	vpn        *VPNController
	checked    atomic.Int64
	nextIndex  atomic.Int64
	nextEngine atomic.Int64
	governor   *AdaptiveGovernor
}

// NewMultiEngineRunner creates the runner over the given engines.
func NewMultiEngineRunner(engines []*Engine, users []string, vpn *VPNController) *MultiEngineRunner {
	maxWorkers := 0
	for _, e := range engines {
		if e.Workers > 0 {
			maxWorkers += e.Workers
		}
	}
	if maxWorkers < 1 {
		maxWorkers = 1
	}
	poolMin := 8
	if poolMin > maxWorkers {
		poolMin = maxWorkers
	}
	return &MultiEngineRunner{
		engines:  engines,
		queue:    NewJobQueue(users),
		vpn:      vpn,
		governor: NewAdaptiveGovernor(maxWorkers, poolMin, maxWorkers),
	}
}

// Run starts every engine and blocks until the queue is empty.
func (r *MultiEngineRunner) Run(ctx context.Context) {
	// Every engine needs its own fully initialised checker (lanes, sources,
	// session pool) before it can pull work.
	for _, e := range r.engines {
		if !e.checker.Setup(ctx) {
			fmt.Printf("  engine %-12s setup FAILED - skipping\n", e.Name)
			e.paused.Store(true)
		}
	}

	// ONE shared output writer for every engine.
	//
	// The engines write into the SAME 8 output files, so they must share one
	// writer: two writers would flush their buffers independently and the
	// lines of two engines could interleave inside a file. The writer also
	// has to be OPENED here - the multi-engine path never calls
	// MultiChecker.Run (that is where Open() normally happens), and an
	// unopened writer silently DROPS every result.
	shared, err := r.openSharedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "output: %v\n", err)
		return
	}
	if shared != nil {
		defer shared.Close()
		for _, e := range r.engines {
			if e.checker != nil {
				e.checker.out = shared
			}
		}
	}

	for _, e := range r.engines {
		e.started = time.Now()
	}
	// coordinator: when EVERY engine is parked, rotate the exit IP and wake
	// them all up. While at least one engine works, nothing happens.
	coordCtx, stopCoordinator := context.WithCancel(ctx)
	defer stopCoordinator()
	go r.coordinate(coordCtx)

	// One global pool owns the shared queue. Each task selects an available
	// engine, but the pool target is shared: the sum of engine caps is the
	// hard maximum, never one full pool per engine.
	pool := NewResizablePool(r.governor.Workers(), r.governor, r.nextTask, func(outcome AccountOutcome) {
		r.governor.Report(outcome == OutcomeClean)
	})
	pool.Run(ctx)
	r.report()
}

// openSharedOutput opens ONE output writer that every engine writes into.
//
// The multi-engine path never calls MultiChecker.Run (that is where the
// normal run opens the writer), so without this the writer stays closed and
// every result is silently dropped - the run "succeeds" but writes nothing.
// A nil writer means "no output dir configured" - the caller then skips it.
func (r *MultiEngineRunner) openSharedOutput() (*OutputWriter, error) {
	outDir := ""
	for _, e := range r.engines {
		if e.checker != nil && e.checker.cfg.Output != "" {
			outDir = e.checker.cfg.Output
			break
		}
	}
	if outDir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return nil, err
	}
	w := &OutputWriter{Dir: outDir}
	if err := w.Open(); err != nil {
		return nil, err
	}
	return w, nil
}

// nextTask selects an available engine and one shared-queue account. Engine
// selection is round-robin, and an engine's own Workers value remains a hard
// per-engine cap. If every engine is temporarily parked, wait for the VPN
// coordinator to resume one instead of declaring the queue exhausted.
func (r *MultiEngineRunner) nextTask(ctx context.Context) (PoolTask, bool) {
	for {
		if ctx.Err() != nil {
			return nil, false
		}
		if r.queue.Remaining() == 0 {
			return nil, false
		}
		usable := make([]*Engine, 0, len(r.engines))
		for _, e := range r.engines {
			if !e.Paused() && e.active.Load() < int64(e.Workers) {
				usable = append(usable, e)
			}
		}
		if len(usable) == 0 {
			select {
			case <-ctx.Done():
				return nil, false
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		start := int(r.nextEngine.Add(1)-1) % len(usable)
		e := usable[start]
		raw, ok := r.queue.Next()
		if !ok {
			return nil, false
		}
		e.active.Add(1)
		index := int(r.nextIndex.Add(1) - 1)
		return func(ctx context.Context) AccountOutcome {
			defer e.active.Add(-1)
			outcome := e.checker.process(ctx, index, raw)
			if outcome == OutcomeNone {
				return outcome
			}
			e.done.Add(1)
			r.checked.Add(1)
			if outcome == OutcomeBlocked {
				e.Pause("blocked")
			}
			return outcome
		}, true
	}
}

// coordinator watches the engines and rotates the VPN when all of them are
// parked at the same time.
func (r *MultiEngineRunner) coordinate(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if len(r.engines) == 0 || r.vpn == nil {
				continue
			}
			allPaused := true
			for _, e := range r.engines {
				if !e.Paused() {
					allPaused = false
					break
				}
			}
			if !allPaused {
				continue
			}
			fmt.Println("  ALL engines rate limited - rotating the exit IP ...")
			if r.vpn.Trigger("all engines rate limited") {
				// wait for the rotation to finish, then wake everyone
				for r.vpn.IsPaused() {
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
				for _, e := range r.engines {
					e.Resume()
				}
			} else {
				// the VPN is already rotating or capped - just wait it out
				time.Sleep(2 * time.Second)
			}
		}
	}
}

// report prints the per-engine summary.
func (r *MultiEngineRunner) report() {
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("  MULTI-ENGINE SUMMARY")
	fmt.Println("============================================================")
	fmt.Printf("  %-14s %-10s %-12s %-9s %s\n", "Engine", "Accounts", "Rate-limits", "Uptime", "State")
	fmt.Println("------------------------------------------------------------")
	for _, e := range r.engines {
		state := "active"
		if e.Paused() {
			state = "parked"
		}
		up := time.Since(e.started).Round(time.Second)
		fmt.Printf("  %-14s %-10d %-12d %-9s %s\n",
			e.Name, e.done.Load(), e.limited.Load(), up, state)
	}
	fmt.Printf("  Total checked: %d\n", r.checked.Load())
	fmt.Println("============================================================")
}

// ============================================================
// Engine builder
// ============================================================

// EngineSpec describes one engine to build.
type EngineSpec struct {
	Name        string
	SessionDir  string // "" = anonymous engine (no cookie budget)
	MaxLanes    int    // 0 = one lane per session
	Workers     int
	RatePerIP   float64
	RatePerSess float64
}

// BuildEngines creates the engines from their specs. Every engine gets its
// own session pool, its own lanes and its own rate budget, so one engine
// getting blocked never affects the others.
func BuildEngines(specs []EngineSpec, base MultiConfig) ([]*Engine, error) {
	out := make([]*Engine, 0, len(specs))
	for _, spec := range specs {
		cfg := base
		cfg.RatePerIP = spec.RatePerIP
		cfg.RatePerSession = spec.RatePerSess
		cfg.Lanes = spec.MaxLanes
		cfg.LanesPerIP = 0
		cfg.NoPreflight = true // the parent run already probed the sessions
		cfg.VPN = nil          // the coordinator owns the rotation
		cfg.BrowserPages = 1
		cfg.BrowserBudget = 20

		if spec.SessionDir != "" {
			sessions, err := LoadSessionPool(spec.SessionDir)
			if err != nil {
				fmt.Printf("  engine %-12s skipped (%v)\n", spec.Name, err)
				continue
			}
			cfg.Sessions = sessions
		} else {
			cfg.Sessions = nil // anonymous engine - no cookie budget
		}

		ch := NewMultiChecker(cfg)
		if ch == nil {
			continue
		}
		out = append(out, &Engine{
			Name:    spec.Name,
			Workers: spec.Workers,
			Rate:    spec.RatePerIP,
			checker: ch,
		})
	}
	return out, nil
}

// MustBuildEngines is BuildEngines but it exits on a fatal error.
func MustBuildEngines(specs []EngineSpec, base MultiConfig) []*Engine {
	engines, err := BuildEngines(specs, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "engines: %v\n", err)
		os.Exit(1)
	}
	return engines
}
