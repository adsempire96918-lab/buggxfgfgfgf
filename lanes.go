// Package main - multi-session lanes.
//
// A lane is one (IP, session) pair: its own client, its own circuit breaker
// and its own adaptive governor, but the rate budget is SHARED per IP
// (measured: Reddit limits cookie sessions per IP, not per account).
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Lane = one (proxy IP, session) pair.
type Lane struct {
	ID       int
	Proxy    string
	Session  *Session
	Client   *Client
	Gate     *Gate
	Breaker  *CircuitBreaker
	Governor *AdaptiveGovernor

	mu           sync.Mutex
	blocked      bool
	blockedUntil time.Time
	rotated      int
	used         int
	maxUse       int
	pool         *SessionPool
	timeout      time.Duration

	// Group indicates which rotating group this lane belongs to (0-4).
	// Lanes are assigned to groups in round-robin fashion so that
	// the 5-rotating method distributes load across 5 independent IP buckets.
	Group int
}

// IsBlocked reports whether this lane hit a block-class failure. A blocked
// lane recovers automatically after the cooldown (otherwise a single block
// would kill the lane for the whole run).
func (l *Lane) IsBlocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.blocked {
		return false
	}
	if time.Now().After(l.blockedUntil) {
		l.blocked = false
		return false
	}
	return true
}

// MarkBlocked flags the lane (its session or the IP is refusing requests).
func (l *Lane) MarkBlocked() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.blocked = true
	l.blockedUntil = time.Now().Add(30 * time.Second)
}

// Unblock clears the lane's block flag immediately. Used after a VPN IP
// rotation: the block belonged to the old exit IP, not to this lane.
func (l *Lane) Unblock() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.blocked = false
	l.blockedUntil = time.Time{}
}

// Rotations counts how often this lane switched to another session.
func (l *Lane) Rotations() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rotated
}

// gateForSession derives a FRESH Gate for a new session from the lane's old
// gate: the new session's own rate budget, the same shared IP bucket and the
// same breaker (breaker state intentionally survives a session swap). The old
// gate object is left untouched - another worker may still be reading it
// through the old client, so mutating it would be a data race.
func gateForSession(old *Gate, s *Session) *Gate {
	if old == nil {
		return nil
	}
	bucket := old.Bucket
	if s != nil && s.Bucket != nil {
		bucket = s.Bucket
	}
	return &Gate{
		Bucket:   bucket,
		IPBucket: old.IPBucket,
		Breaker:  old.Breaker,
		Retries:  old.Retries,
		Backoff:  old.Backoff,
	}
}

// SetSession swaps the session of a lane (session rotation) and rebuilds the
// client so the new cookies are used.
func (l *Lane) SetSession(s *Session, timeout time.Duration) error {
	c, err := newClient(l.Proxy, timeout, 0)
	if err != nil {
		return err
	}
	c.ApplyCookies(s.Cookies)
	l.mu.Lock()
	c.Gate = gateForSession(l.Gate, s)
	l.Gate = c.Gate
	l.Session = s
	l.Client = c
	l.blocked = false
	l.rotated++
	l.mu.Unlock()
	return nil
}

// RotateIfNeeded swaps the lane to the next session from the pool when the
// current one has served enough users (session recycling). The swap is a
// single locked pointer update: the fresh client+gate are fully built before
// they become visible, and the old objects are never mutated (data-race free).
func (l *Lane) RotateIfNeeded() {
	l.mu.Lock()
	need := l.maxUse > 0 && l.used >= l.maxUse
	pool := l.pool
	timeout := l.timeout
	l.mu.Unlock()
	if !need || pool == nil {
		return
	}
	s := pool.Next()
	if s == nil {
		return
	}
	c, err := newClient(l.Proxy, timeout, 0)
	if err != nil {
		return
	}
	c.ApplyCookies(s.Cookies)
	l.mu.Lock()
	c.Gate = gateForSession(l.Gate, s)
	l.Gate = c.Gate
	old := l.Session
	l.Session = s
	l.Client = c
	l.used = 0
	l.rotated++
	l.mu.Unlock()
	// release the session we just left so the pool can hand it to another lane
	if old != nil && old != s {
		old.Release()
	}
}

// RotateToFreshSession swaps the lane to a FRESH session from the pool
// immediately. It is used when the current session gets blocked: with a big
// session pool (hundreds of accounts) there is always an unused session
// waiting, so parking the lane for the 30s block cooldown is pure dead time.
//
// The swap is the same single locked pointer update as RotateIfNeeded: the
// fresh client+gate are fully built before they become visible, and the old
// objects are never mutated (data-race free).
func (l *Lane) RotateToFreshSession() bool {
	l.mu.Lock()
	pool := l.pool
	timeout := l.timeout
	oldSession := l.Session
	l.mu.Unlock()

	if pool == nil {
		return false
	}

	// Pull a session that is not the one we are replacing and not quarantined.
	s := pool.Next()
	if s == nil || s == oldSession {
		// Try once more - the pool may have handed back the same session.
		s = pool.Next()
	}
	if s == nil || s == oldSession {
		return false
	}

	c, err := newClient(l.Proxy, timeout, 0)
	if err != nil {
		return false
	}
	c.ApplyCookies(s.Cookies)

	l.mu.Lock()
	c.Gate = gateForSession(l.Gate, s)
	l.Gate = c.Gate
	old := l.Session
	l.Session = s
	l.Client = c
	l.used = 0
	l.rotated++
	// the fresh session is not blocked - clear the flag so the lane is
	// usable again right away instead of waiting out the cooldown.
	l.blocked = false
	l.blockedUntil = time.Time{}
	l.mu.Unlock()
	// release the session we just left so the pool can hand it to another
	// lane (a big pool should never run out of accounts).
	if old != nil && old != s {
		old.Release()
	}
	return true
}

// Fetch runs one user check on this lane.
func (l *Lane) Fetch(ctx context.Context, user, token string) UserResult {
	l.mu.Lock()
	c := l.Client
	l.mu.Unlock()
	if c == nil {
		return errResult("no_client", "lane has no client")
	}
	res := c.FetchUser(ctx, user, token, true)
	blocked := res.Error == "blocked" || res.Error == "forbidden" || res.Error == "unauthorized"
	if blocked {
		l.MarkBlocked()
		// A blocked session is dead for this IP - do NOT wait out the 30s
		// cooldown when the pool still has fresh sessions. Swap immediately so
		// the lane keeps working. This is what makes a big session pool
		// ("sassion" with hundreds of accounts) genuinely more powerful:
		// every blocked session is replaced by an unused one at once.
		if l.RotateToFreshSession() {
			debugf("lane %d: session blocked - swapped to a fresh session", l.ID)
		}
	}
	l.Governor.Report(!blocked)
	l.mu.Lock()
	l.used++
	s := l.Session
	l.mu.Unlock()
	if s != nil {
		s.Report(!blocked)
	}
	if res.OK {
		l.mu.Lock()
		if l.Session != nil {
			l.Session.Touch()
		}
		l.mu.Unlock()
	}
	return res
}

// LaneManager owns the lanes and hands them out round-robin.
type LaneManager struct {
	mu    sync.Mutex
	lanes []*Lane
	next  int
	// breakers holds ONE circuit breaker per exit IP, shared by every lane
	// behind that IP (T-03). nil for hand-built managers in tests.
	breakers *BreakerRegistry
}

// NewLaneManager creates an empty manager.
func NewLaneManager() *LaneManager { return &LaneManager{} }

// Add appends a lane.
func (m *LaneManager) Add(l *Lane) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lanes = append(m.lanes, l)
}

// All returns every lane.
func (m *LaneManager) All() []*Lane {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Lane, len(m.lanes))
	copy(out, m.lanes)
	return out
}

// Len returns the lane count.
func (m *LaneManager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lanes)
}

// Next returns the next usable lane (round-robin, blocked lanes skipped).
// Returns nil when every lane is blocked.
func (m *LaneManager) Next() *Lane {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.lanes)
	if n == 0 {
		return nil
	}
	for i := 0; i < n; i++ {
		l := m.lanes[m.next%n]
		m.next = (m.next + 1) % n
		if !l.IsBlocked() {
			return l
		}
	}
	return nil
}

// Healthy counts the lanes that are not blocked.
func (m *LaneManager) Healthy() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, l := range m.lanes {
		if !l.IsBlocked() {
			n++
		}
	}
	return n
}

// ClientSnapshot returns the lane's CURRENT client (thread safe). The
// source ladder resolves this at request time so it always dials the
// session that is actually installed in the lane right now — a frozen
// snapshot would keep dialing old cookies after every rotation.
func (l *Lane) ClientSnapshot() *Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Client
}

// clientSnapshot returns the lane's CURRENT client (thread safe). The
// source ladder resolves this at request time so it always dials the
// session that is actually installed in the lane right now — a frozen
// snapshot would keep dialing old cookies after every rotation.
func (l *Lane) clientSnapshot() *Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Client
}
func (l *Lane) sessionSnapshot() *Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Session
}

// RotateLane swaps a blocked lane to another session that is not used by a
// lane on the same IP (session rotation on account-level blocks).
func (m *LaneManager) RotateLane(l *Lane, sessions []*Session, timeout time.Duration) bool {
	m.mu.Lock()
	used := map[*Session]bool{}
	for _, other := range m.lanes {
		if other != l && other.Proxy == l.Proxy {
			used[other.sessionSnapshot()] = true
		}
	}
	m.mu.Unlock()
	for i := range sessions {
		s := sessions[i]
		if used[s] || s.State == PoolRejected {
			continue
		}
		if err := l.SetSession(s, timeout); err == nil {
			return true
		}
	}
	return false
}

// buildLanes creates the lane POOL: a fixed number of lanes (clients), each
// taking sessions from the pool. Sessions are recycled - a lane switches to
// the next session after maxPerSession users, so 454 sessions can be used
// with only N clients.
func buildLanes(pool *SessionPool, proxies []string, lanesPerIP int, ipLim *IPLimiterGrouped, timeout time.Duration, maxBlocked, retries int, perSessionRate float64, burst int, maxPerSession int, metrics *Metrics) (*LaneManager, error) {
	if len(proxies) == 0 {
		proxies = []string{""}
	}
	mgr := NewLaneManager()
	// ONE breaker per exit IP (T-03). Every lane behind the same proxy shares
	// it, so a block wave pauses the whole IP instead of letting 500 lanes
	// retry at the same time.
	breakers := NewBreakerRegistry(maxBlocked, breakerCooldown)
	mgr.breakers = breakers
	id := 0
	// Per-lane IP counters for VPN rotation feedback (see lanes_test.go).
	for _, proxy := range proxies {
		made := 0
		// The group counter is PER EXIT IP: every IP gets an even spread
		// over the rotating groups, so no IP ends up with only one group
		// and the session rotation stays balanced.
		groupCounter := 0
		for pool.Len() > 0 && (lanesPerIP <= 0 || made < lanesPerIP) {
			s := pool.Next()
			if s == nil {
				break
			}
			c, err := newClient(proxy, timeout, id)
			if err != nil {
				return nil, err
			}
			c.Metrics = metrics
			g := groupCounter % ipLim.NumGroups()
			groupCounter++
			gate := &Gate{
				// per-session budget (the account's own ceiling) + the shared
				// per-IP budget on top.
				Bucket:   s.Bucket,
				IPBucket: ipLim.Bucket(proxy, g),
				Breaker:  breakers.For(breakerKey(proxy)),
				Retries:  retries,
				Backoff:  5 * time.Second,
			}
			c.Gate = gate
			c.ApplyCookies(s.Cookies)
			l := &Lane{
				ID:       id,
				Proxy:    proxy,
				Session:  s,
				Client:   c,
				Gate:     gate,
				Breaker:  gate.Breaker,
				Governor: NewAdaptiveGovernor(4, 1, 8),
				used:     0,
				maxUse:   maxPerSession,
				pool:     pool,
				timeout:  timeout,
				Group:    g,
			}
			mgr.Add(l)
			id++
			made++
		}
	}
	return mgr, nil
}

// MultiConfig configures a multi-session run.
type MultiConfig struct {
	Input           string
	Output          string
	Workers         int
	Proxies         []string
	Sessions        []*Session
	RatePerIP       float64
	RatePerSession  float64 // per account/session (measured ceiling ~1-1.5 req/s)
	Burst           int
	LanesPerIP      int
	Lanes           int            // lane pool size (0 = one lane per session)
	MaxPerSession   int            // users a session serves before the lane rotates
	AIMD            bool           // closed-loop rate control
	PreflightSample int            // sessions to probe at startup (0 = all)
	VPN             *VPNController // shared VPN rotation controller (nil = none)
	Warmup          int
	Timeout         time.Duration
	Retries         int
	MaxBlocked      int
	NoPreflight     bool
	NoResume        bool
	// Metrics is the shared run-wide collector (nil = no collection). Every
	// lane client writes its status and latency into it, so the block rate
	// and the latency percentiles can be reported at the end of the run.
	Metrics       *Metrics
	Token         string
	BrowserPages  int // parallel Chrome tabs for the fallback lane
	BrowserBudget int // max browser fetches per run (0 = unlimited)
	InputTotal    int64
	InputRejected int64

	// ---- advanced rate-limit avoidance (advanced.go) ----
	AdaptiveRate    bool    // auto-adjust the per-group rate from the observed success rate
	MinRatePerGroup float64 // floor for the adaptive rate (0 = derive from RatePerIP)
	MaxRatePerGroup float64 // ceiling for the adaptive rate (0 = derive from RatePerIP)
	RateAdjustment  float64 // how fast the rate moves (0..1)
	SourceTracking  bool    // remember which session/proxy actually works
	NoProxyMode     bool    // no proxy file -> single IP, conservative defaults
}

// MultiChecker runs a batch across the lane matrix.
type MultiChecker struct {
	cfg          MultiConfig
	out          *OutputWriter
	mgr          *LaneManager
	ipLim        *IPLimiterGrouped
	stats        Stats
	start        time.Time
	reqs         int64
	abort        atomic.Bool
	poolGovernor *AdaptiveGovernor
	sources      []Source
	srcRot       int64
	anon         *Client
	browser      *BrowserFallback
	// browserOnce guards the lazy Chrome start: without it concurrent ladder
	// fallbacks raced on m.browser and launched several Chromes (one leaked,
	// and the field itself was read/written without synchronization).
	browserOnce   sync.Once
	browserErr    error
	aimd          *AIMD
	aimdMu        sync.Mutex
	lastMult      float64
	cooldownUntil atomic.Int64
	netDead       int64 // consecutive network-fatal errors (watchdog)
	pool          *SessionPool
	pause         *VPNController
	metrics       *Metrics
	live          *LiveStatus

	// ---- advanced rate-limit avoidance (advanced.go) ----
	rateTracker  *GroupRateTracker    // adaptive per-group rate control
	sourceHealth *SourceHealthTracker // which session/proxy actually works
}

// NewMultiChecker builds the checker (lanes are built in Run).
func NewMultiChecker(cfg MultiConfig) *MultiChecker {
	if cfg.Workers < 1 {
		cfg.Workers = 4
	}
	if cfg.RatePerIP <= 0 {
		cfg.RatePerIP = 600
	}
	if cfg.RatePerSession <= 0 {
		cfg.RatePerSession = 72
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 2
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxBlocked <= 0 {
		cfg.MaxBlocked = 10
	}

	// ---- advanced rate-limit avoidance defaults ----
	// Without a proxy file every lane shares ONE exit IP, so the budget is
	// much tighter: keep the adaptive rate on and start conservative.
	if len(cfg.Proxies) == 0 {
		cfg.NoProxyMode = true
	}
	if cfg.MinRatePerGroup <= 0 {
		if cfg.NoProxyMode {
			cfg.MinRatePerGroup = 15
		} else {
			cfg.MinRatePerGroup = 30
		}
	}
	if cfg.MaxRatePerGroup <= 0 {
		if cfg.NoProxyMode {
			cfg.MaxRatePerGroup = 60
		} else {
			cfg.MaxRatePerGroup = 120
		}
	}
	if cfg.RateAdjustment <= 0 {
		if cfg.NoProxyMode {
			cfg.RateAdjustment = 0.15
		} else {
			cfg.RateAdjustment = 0.5
		}
	}

	lim := NewIPLimiterGrouped(cfg.RatePerIP, cfg.Burst, 5) // 5 groups for rotating method
	m := &MultiChecker{
		cfg:   cfg,
		out:   &OutputWriter{Dir: cfg.Output},
		ipLim: lim,
	}
	// the shared metrics collector: every lane client writes into it
	if m.metrics == nil {
		m.metrics = cfg.Metrics
	}
	if m.metrics == nil {
		m.metrics = NewMetrics()
	}
	m.cfg.Metrics = m.metrics
	if cfg.AdaptiveRate {
		m.rateTracker = NewGroupRateTracker(cfg.MinRatePerGroup, cfg.MaxRatePerGroup, cfg.RateAdjustment)
		m.rateTracker.Attach(lim)
	}
	if cfg.SourceTracking {
		m.sourceHealth = NewSourceHealthTracker()
	}
	return m
}

// PrepareWork builds the list of accounts to check: it removes duplicate
// "user:pass" rows from the input and skips everything the previous runs
// already saved (the resume map).
//
// It is shared by the single-engine and the multi-engine path so BOTH get
// the same protection - the multi-engine runner used to bypass the resume
// completely and could re-check finished accounts.
func PrepareWork(users []string, outputDir string, noResume bool) (todo []string, skipped int, dupes int) {
	// 1. exact duplicate rows inside the input file
	uniq, dupes := DedupLines(users)

	// 2. accounts already saved by an earlier run
	done := map[string]bool{}
	if !noResume {
		done = ReadDoneUsers(outputDir)
	}

	todo = make([]string, 0, len(uniq))
	for _, raw := range uniq {
		key := strings.ToLower(NormalizeUsername(raw))
		if key == "" {
			continue
		}
		if done[key] {
			skipped++
			continue
		}
		done[key] = true
		todo = append(todo, raw)
	}
	return todo, skipped, dupes
}

// Setup prepares the checker for work WITHOUT running the worker pool: it
// builds the session pool, the lanes, the source ladder and arms the VPN.
//
// The multi-engine runner uses this: every engine gets its own fully
// initialised checker, and the engines then pull from one shared queue.
func (m *MultiChecker) Setup(ctx context.Context) bool {
	// session pool (each session owns its rate bucket)
	pool := NewSessionPool(m.cfg.Sessions, m.cfg.RatePerSession, m.cfg.Burst)
	lanes := m.cfg.Lanes
	if lanes <= 0 || lanes > pool.Len() {
		lanes = pool.Len()
	}
	mgr, err := buildLanes(pool, m.cfg.Proxies, lanes, m.ipLim,
		m.cfg.Timeout, m.cfg.MaxBlocked, m.cfg.Retries, m.cfg.RatePerSession, m.cfg.Burst,
		m.cfg.MaxPerSession, m.metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lanes: %v\n", err)
		return false
	}
	m.mgr = mgr
	m.pool = pool
	if m.cfg.AIMD {
		m.aimd = NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	}
	if err := m.buildSources(); err != nil {
		fmt.Fprintf(os.Stderr, "sources: %v\n", err)
		return false
	}
	m.pause = m.cfg.VPN
	if m.pause != nil && m.pause.Enabled() {
		m.pause.Arm()
		m.pause.SetOnRotated(m.resetAfterRotation)
	}
	if !m.cfg.NoPreflight {
		m.preflight(ctx)
	}
	return true
}

// Run executes the batch across the lanes.
func (m *MultiChecker) Run(ctx context.Context, users []string) bool {
	if err := os.MkdirAll(m.cfg.Output, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "output dir: %v\n", err)
		return false
	}
	if err := m.out.Open(); err != nil {
		fmt.Fprintf(os.Stderr, "output: %v\n", err)
		return false
	}
	defer m.out.Close()

	// resume + duplicate protection (shared with the multi-engine path)
	todo, skipped, dupes := PrepareWork(users, m.cfg.Output, m.cfg.NoResume)
	if dupes > 0 {
		fmt.Printf("Removed %d duplicate user:pass row(s) from the input.\n", dupes)
	}
	atomic.StoreInt64(&m.stats.Skipped, int64(skipped))
	if skipped > 0 {
		fmt.Printf("Skipped %d account(s) already checked by an earlier run.\n", skipped)
	}
	atomic.StoreInt64(&m.stats.Total, int64(len(todo)))
	if m.cfg.InputTotal > 0 {
		atomic.StoreInt64(&m.stats.InputTotal, m.cfg.InputTotal)
	} else {
		atomic.StoreInt64(&m.stats.InputTotal, int64(len(users)))
	}
	if len(todo) == 0 {
		fmt.Println("Nothing to do - all usernames already checked.")
		if m.stats.Skipped > 0 {
			fmt.Printf("Skipped %d already-saved usernames.\n", m.stats.Skipped)
		}
		return true
	}

	// session pool (each session owns its rate bucket)
	pool := NewSessionPool(m.cfg.Sessions, m.cfg.RatePerSession, m.cfg.Burst)
	lanes := m.cfg.Lanes
	if lanes <= 0 || lanes > pool.Len() {
		lanes = pool.Len()
	}
	// lanes
	mgr, err := buildLanes(pool, m.cfg.Proxies, lanes, m.ipLim,
		m.cfg.Timeout, m.cfg.MaxBlocked, m.cfg.Retries, m.cfg.RatePerSession, m.cfg.Burst,
		m.cfg.MaxPerSession, m.metrics)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lanes: %v\n", err)
		return false
	}
	m.mgr = mgr
	m.pool = pool
	if m.cfg.AIMD {
		m.aimd = NewAIMD(0.5, 3.0, aimdDefaultUpEvery, 0.02)
	}
	laneSummary(mgr)
	if err := m.buildSources(); err != nil {
		fmt.Fprintf(os.Stderr, "sources: %v\n", err)
		return false
	}

	// VPN rotation: arm the shared controller and install the post-rotation
	// reset hook (a fresh exit IP means a clean slate for lanes/sessions).
	m.pause = m.cfg.VPN
	if m.pause != nil && m.pause.Enabled() {
		m.pause.Arm()
		m.pause.SetOnRotated(m.resetAfterRotation)
	}

	// preflight: probe every session once (cheap, and drops dead sessions
	// before we spend real requests on them)
	if !m.cfg.NoPreflight {
		m.preflight(ctx)
		if m.abort.Load() {
			return false
		}
	}

	fmt.Printf("\nMode: multi-session | Users: %d | Workers: %d | Lanes: %d | Rate: %.0f/min per IP | burst %d\n\n",
		len(todo), m.cfg.Workers, mgr.Len(), m.cfg.RatePerIP, m.cfg.Burst)

	m.start = time.Now()
	// live status panel (the Python script's rich.live panel)
	live := NewLiveStatus(int64(len(todo)))
	m.live = live
	// the panel shows the live rate-limit state: without it a run that is
	// quietly rate limited looks exactly like a run that is quietly slow
	live.SetProvider(m.rateStateRows)
	SetLiveActive(live.Enabled())
	defer func() {
		live.Close()
		SetLiveActive(false)
	}()
	// periodic debug snapshot: the panels at the end of the run are useless
	// when a long run is killed, so the same state is printed every 30 s
	stopSnapshot := make(chan struct{})
	defer close(stopSnapshot)
	go m.debugSnapshot(ctx, stopSnapshot)
	// One run-wide governor controls the actual worker count. The floor is
	// min(8, requested) so a small run stays small while a 200-500 thread
	// run can back off to a safe minimum during a block wave.
	poolMin := 8
	if poolMin > m.cfg.Workers {
		poolMin = m.cfg.Workers
	}
	m.poolGovernor = NewAdaptiveGovernor(m.cfg.Workers, poolMin, m.cfg.Workers)
	var next atomic.Int64
	workerPool := NewResizablePool(m.cfg.Workers, m.poolGovernor, func(ctx context.Context) (PoolTask, bool) {
		i := int(next.Add(1) - 1)
		if i >= len(todo) || m.abort.Load() {
			return nil, false
		}
		return func(ctx context.Context) AccountOutcome {
			return m.process(ctx, i, todo[i])
		}, true
	}, func(outcome AccountOutcome) {
		m.poolGovernor.Report(outcome == OutcomeClean)
	})
	workerPool.Run(ctx)
	// the fallback Chrome (if any) must not outlive the run
	defer m.closeBrowser()

	m.summary()
	m.writeSessionReport()
	return !m.abort.Load()
}

// preflight probes the sessions once and drops the rejected ones. With many
// sessions only a sample is probed (the rest are calibrated lazily).
//
// The probes run in PARALLEL. They used to run one after the other, and a
// probe is one HTTP round trip (~1.2 s to Reddit) - so 500 lanes meant a
// 10 MINUTE startup before a single account was checked. The probe
// concurrency is bounded so the preflight itself can never trip the rate
// limit.
func (m *MultiChecker) preflight(ctx context.Context) {
	if !Quiet() {
		fmt.Println("Preflight: checking sessions ...")
	}
	limit := m.cfg.PreflightSample
	if limit <= 0 || limit > len(m.cfg.Sessions) {
		limit = len(m.cfg.Sessions)
	}
	// the lanes that will be probed
	targets := []*Lane{}
	for _, l := range m.mgr.All() {
		if l.Session == nil || len(targets) >= limit {
			continue
		}
		targets = append(targets, l)
	}
	if len(targets) == 0 {
		healthy := m.mgr.Healthy()
		fmt.Printf("Sessions usable: %d/%d (probed 0)\n\n", healthy, m.mgr.Len())
		if healthy == 0 {
			fmt.Println("No usable session - run the session validator first.")
			m.abort.Store(true)
		}
		return
	}

	// bounded probe concurrency: enough to be fast, small enough to stay
	// inside the rate budget
	probeWorkers := 32
	if probeWorkers > len(targets) {
		probeWorkers = len(targets)
	}
	if probeWorkers < 1 {
		probeWorkers = 1
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, probeWorkers)
	var mu sync.Mutex
	checked := 0
	for _, l := range targets {
		wg.Add(1)
		go func(l *Lane) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			state, detail := ProbeSession(ctx, l.Proxy, m.cfg.Timeout, l.Session)
			l.Session.SetState(state, detail)
			if state == PoolRejected {
				l.MarkBlocked()
			}
			mu.Lock()
			checked++
			n := checked
			mu.Unlock()
			if !Quiet() && (n <= 20 || n%50 == 0) {
				fmt.Printf("  %-24s %-9s %s\n", l.Session.Name, state, detail)
			}
		}(l)
	}
	wg.Wait()

	healthy := m.mgr.Healthy()
	fmt.Printf("Sessions usable: %d/%d (probed %d)\n\n", healthy, m.mgr.Len(), checked)
	if healthy == 0 {
		fmt.Println("No usable session - run the session validator first.")
		m.abort.Store(true)
	}
}

func laneSummary(mgr *LaneManager) {
	fmt.Printf("Lanes: %d (healthy %d)\n", mgr.Len(), mgr.Healthy())
	if Quiet() {
		return // the per-lane list is noise in quiet mode
	}
	for _, l := range mgr.All() {
		proxy := l.Proxy
		if proxy == "" {
			proxy = "direct"
		}
		fmt.Printf("  lane %d  ip=%-22s session=%-24s blocked=%v\n",
			l.ID, proxy, l.Session.Name, l.IsBlocked())
	}
}

// buildSources creates the source ladder: every lane (session) first, then
// the anonymous fallback. Blocked lanes are skipped at request time.
func (m *MultiChecker) buildSources() error {
	m.sources = nil
	for _, l := range m.mgr.All() {
		if l.Client == nil {
			continue
		}
		m.sources = append(m.sources,
			Source{Client: l.Client, Session: l.Session, JSON: true, Lane: l,
				Label: "json+session(" + l.Session.Name + ")"},
			Source{Client: l.Client, Session: l.Session, JSON: false, Lane: l,
				Label: "html+session(" + l.Session.Name + ")"},
		)
	}
	// anonymous fallback (same IP budget)
	anon, err := newClient("", m.cfg.Timeout, 0)
	if err != nil {
		return err
	}
	anon.Gate = &Gate{
		Bucket:  m.ipLim.Bucket("", 0),
		Breaker: NewCircuitBreaker(m.cfg.MaxBlocked, breakerCooldown),
		Retries: m.cfg.Retries,
		Backoff: 5 * time.Second,
	}
	m.anon = anon
	m.sources = append(m.sources,
		Source{Client: anon, JSON: true, Label: "json+anonymous"},
		Source{Client: anon, JSON: false, Label: "html+anonymous"},
	)
	return nil
}

// process picks a lane, runs the check and records the result. It returns the
// terminal account outcome so the run-wide worker governor receives exactly
// one report per finished account.
func (m *MultiChecker) process(ctx context.Context, index int, raw string) AccountOutcome {
	user := NormalizeUsername(raw)
	if user == "" {
		return OutcomeNone
	}
	// global pause: if the VPN is rotating, park this worker (no crash,
	// no lost progress) until the new IP is ready
	if m.pause != nil && !m.pause.Wait(ctx) {
		return OutcomeNone
	}
	lane := m.mgr.Next()
	if lane == nil {
		// every lane is blocked -> one shared cooldown window (not one per
		// worker, otherwise the tail of the run crawls)
		now := time.Now().UnixNano()
		until := m.cooldownUntil.Load()
		if until <= now {
			until = time.Now().Add(15 * time.Second).UnixNano()
			m.cooldownUntil.Store(until)
		}
		wait := time.Duration(until - now)
		if wait < 0 {
			wait = 0
		}
		progressf("All lanes blocked - cooling down %s ...\n", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return OutcomeNone
		case <-time.After(wait):
		}
		lane = m.mgr.Next()
		if lane == nil {
			m.finishError(user, index, "all lanes blocked", "blocked")
			return OutcomeBlocked
		}
	}
	// warm-up: the first requests go slow so Reddit trusts the session
	if m.cfg.Warmup > 0 && atomic.LoadInt64(&m.reqs) < int64(m.cfg.Warmup) {
		time.Sleep(1500 * time.Millisecond)
	}
	atomic.AddInt64(&m.reqs, 1)
	atomic.AddInt64(&m.stats.Processed, 1)
	if lane != nil {
		lane.RotateIfNeeded()
	}

	// source ladder: session JSON -> session HTML -> anonymous JSON ->
	// anonymous HTML -> browser (last resort)
	// source ladder: session JSON -> session HTML -> anonymous JSON ->
	// anonymous HTML -> browser (last resort). The order is rotated per user
	// so the load spreads over all lanes instead of always starting at lane 0.
	srcs := m.sources
	if len(srcs) > 1 {
		off := int(atomic.AddInt64(&m.srcRot, 1)) % len(srcs)
		srcs = append(append([]Source{}, srcs[off:]...), srcs[:off]...)
	}
	// Reserve the AIMD ticket before any HTTP work starts. In recovery mode
	// exactly one concurrent account can own the trial probe.
	var aimdReq AIMDRequest
	if m.aimd != nil {
		aimdReq = m.aimd.BeginRequest()
	}
	lr := FetchWithLadderLimit(ctx, srcs, user, m.cfg.Timeout, 3*time.Second, 6)
	res := lr.Result
	// feed the VPN throttle detector: a rotation only fires when the IP is
	// genuinely rate limited (never on a single transient result)
	if m.pause != nil {
		if res.Error == "blocked" {
			// a Reddit block page is definitive - rotate the exit IP now.
			// The reason carries the measured path health, so the rotation
			// history explains whether the whole exit path failed (T-09).
			reason := "blocked page"
			if m.sourceHealth != nil {
				if healthy, total, ratio := m.sourceHealth.PathHealth(); total > 0 {
					reason = fmt.Sprintf("blocked page (path health %d/%d = %.0f%%)", healthy, total, ratio*100)
				}
			}
			m.pause.ReportIPBlocked(reason)
		} else if res.Error == "rate_limited" {
			// an explicit 429 means this exit IP is already over budget -
			// rotate IMMEDIATELY instead of waiting for the consecutive
			// throttle threshold (the rotation is debounced, so a burst of
			// 429s still produces only ONE rotation).
			m.pause.ReportThrottled("rate limited (429)")
		} else {
			m.pause.Report(classifyResult(res) != ClassThrottled, "throttled")
		}
	}
	// AIMD feedback: a blocked outcome shrinks the rate, a clean one grows it.
	// The request ticket is reserved before the ladder starts, so a recovery
	// probe is single-flight even when hundreds of workers are active.
	if m.aimd != nil {
		clean := res.OK || res.NotFound || res.Banned
		m.aimd.EndRequest(aimdReq, clean)
		m.applyAIMD()
	}
	// adaptive per-group rate control (advanced.go): the group that served
	// this user learns from the outcome and speeds up / backs off on its own.
	if m.rateTracker != nil && lane != nil {
		clean := res.OK || res.NotFound || res.Banned
		m.rateTracker.Update(lane.Group, clean)
	}
	// source health: remember which session/proxy actually returns data so
	// the report can show the working ones.
	if m.sourceHealth != nil && lane != nil && lane.Session != nil {
		clean := res.OK || res.NotFound || res.Banned
		m.sourceHealth.RecordCheck(lane.Session.Name, clean, 0)
	}
	if !res.OK && !res.NotFound && !res.Banned && classifyResult(res) != ClassThrottled {
		// last rung: a real Chrome (100% authentic). Only for a DEFINITIVE
		// failure - when the IP is throttled the browser is blocked too and
		// this would just slow the run down.
		if br := m.browserFetch(ctx, user); br.OK || br.NotFound || br.Banned {
			res = br
		}
	}
	if res.Error == "blocked" || res.Error == "forbidden" || res.Error == "unauthorized" {
		// try another session on the same IP before giving up on this user
		if lane := m.mgr.Next(); lane != nil {
			if m.mgr.RotateLane(lane, m.cfg.Sessions, m.cfg.Timeout) {
				fmt.Printf("  rotated lane %d to another session\n", lane.ID)
			}
		}
	}
	m.finish(user, index, raw, res)
	return accountOutcome(res)
}

// accountOutcome classifies the FINAL result of one account. A definitive
// Reddit answer (hit, 404 or suspended) is clean; block/throttle is unhealthy;
// every other terminal error is a failed account.
func accountOutcome(res UserResult) AccountOutcome {
	switch {
	case res.OK, res.NotFound, res.Banned:
		return OutcomeClean
	case res.Error == "blocked", res.Error == "forbidden", res.Error == "unauthorized", res.Error == "rate_limited":
		return OutcomeBlocked
	default:
		return OutcomeFailed
	}
}

// applyAIMD pushes the controller's multiplier into the shared per-IP
// bucket only. The per-session budgets stay at their measured rate - one
// blocked account must not slow down the other 453.
func (m *MultiChecker) applyAIMD() {
	if m.aimd == nil || m.ipLim == nil {
		return
	}
	m.aimdMu.Lock()
	defer m.aimdMu.Unlock()
	mult := m.aimd.Multiplier()
	if mult == m.lastMult {
		return
	}
	m.lastMult = mult
	m.ipLim.SetRate(m.cfg.RatePerIP * mult)
}

// resetAllResets every component that belongs to the previous exit IP.
// This is called while workers are paused (after the VPN rotated).
func (m *MultiChecker) resetAll() {
	// lanes: Unblock + shared per-IP breaker reset. The breakers are shared,
	// so the registry is reset once (resetting through the lanes would reset
	// the same object N times).
	for _, l := range m.mgr.All() {
		l.Unblock()
		if s := l.sessionSnapshot(); s != nil {
			s.ClearPenalty()
		}
	}
	if m.mgr.breakers != nil {
		m.mgr.breakers.ResetAll()
	} else {
		for _, l := range m.mgr.All() {
			if l.Breaker != nil {
				l.Breaker.Reset()
			}
		}
	}
	// session pool: ensure any pending quarantined session is cleared
	if m.pool != nil {
		m.pool.ClearPenalty()
	}
	if m.aimd != nil {
		// Start the fresh exit IP at the controller's configured floor. The
		// controller remembers its last known-good ceiling and recovers via
		// guarded probes; it never restores the old hot rate in one jump.
		m.aimd.ResetForRotation()
		m.aimdMu.Lock()
		m.lastMult = 0
		m.aimdMu.Unlock()
		m.applyAIMD()
	}
	// warm-up re-armed
	atomic.StoreInt64(&m.reqs, 0)
	// network dead streak cleared
	atomic.StoreInt64(&m.netDead, 0)
	fmt.Println("VPN: all lane breakers, session scores, AIMD and warm-up reset.")
}

// resetAfterRotation clears every penalty that the previous (rate-limited)
// IP caused: a fresh exit IP means a clean slate for lanes, breakers and
// sessions, plus a conservative AIMD restart and a re-armed warm-up.
func (m *MultiChecker) resetAfterRotation(rec RotationRecord) {
	m.resetAll()
	// the new exit IP starts with a clean slate: the adaptive rate tracker
	// must not carry the old IP's throttle streak over.
	if m.rateTracker != nil {
		m.rateTracker.Reset()
	}
	fmt.Println("VPN: lanes, breakers, sessions and rate reset for the fresh IP.")
}

// browserFetch lazily starts the browser fallback lane.
//
// The start is single-flight (sync.Once). Before that, two workers could both
// see m.browser == nil and launch Chrome twice - one browser leaked, its
// profile dir collided with the other, and the pointer itself was raced. A
// failed start is remembered instead of retried per user: launching Chrome is
// expensive and a failure here is environmental (no Chrome / no profile lock).
func (m *MultiChecker) browserFetch(ctx context.Context, user string) UserResult {
	m.browserOnce.Do(func() {
		// a profile page load must not eat the whole request timeout
		bt := m.cfg.Timeout
		if bt <= 0 || bt > 12*time.Second {
			bt = 12 * time.Second
		}
		b, err := newBrowserFallback(m.cfg.Output, true, bt,
			m.cfg.BrowserPages, m.cfg.BrowserBudget)
		if err != nil {
			m.browserErr = err
			return
		}
		if len(m.cfg.Sessions) > 0 {
			b.SetSession(m.cfg.Sessions[0])
		}
		m.browser = b
	})
	if m.browserErr != nil {
		return errResult("browser_unavailable", m.browserErr.Error())
	}
	if m.browser == nil {
		return errResult("browser_unavailable", "the browser lane did not start")
	}
	return m.browser.Fetch(ctx, user)
}

// closeBrowser shuts down the fallback Chrome if it was ever started.
func (m *MultiChecker) closeBrowser() {
	if m.browser != nil {
		m.browser.Close()
	}
}

// finish writes the result (same format as the single-session checker).
func (m *MultiChecker) finish(user string, index int, raw string, res UserResult) {
	switch {
	case res.OK:
		m.out.Append(KarmaBucketName(res.TotalKarma), strings.TrimSpace(raw))
		atomic.AddInt64(&m.stats.Hit, 1)
		atomic.AddInt64(&m.stats.Buckets[KarmaBucketIndex(res.TotalKarma)], 1)
		progressf("[%d/%d] HIT %s - Karma: %d | Link: %d | Comment: %d | Age: %dd | Premium: %v\n",
			index+1, m.stats.Total, user, res.TotalKarma, res.LinkKarma, res.CommentKarma, res.AgeDays, res.Premium)
	case res.NotFound:
		m.out.Append("not_found.txt", user+" | NOT_FOUND")
		atomic.AddInt64(&m.stats.NotFound, 1)
		progressf("[%d/%d] NOT_FOUND %s\n", index+1, m.stats.Total, user)
	case res.Banned:
		m.out.Append("banned.txt", user+" | SUSPENDED")
		atomic.AddInt64(&m.stats.Banned, 1)
		if res.Deleted {
			atomic.AddInt64(&m.stats.Deleted, 1)
		}
		progressf("[%d/%d] SUSPENDED %s\n", index+1, m.stats.Total, user)
	default:
		msg := res.Error
		if res.Detail != "" {
			msg = res.Error + ": " + res.Detail
		}
		m.finishError(user, index, msg, res.Error)
	}
	// refresh the live status panel
	if m.live != nil {
		m.live.Update(atomic.LoadInt64(&m.stats.Processed), atomic.LoadInt64(&m.stats.Hit),
			atomic.LoadInt64(&m.stats.NotFound)+atomic.LoadInt64(&m.stats.Banned),
			atomic.LoadInt64(&m.stats.Error))
	}
}

// rateStateRows returns the live rate-limit state for the status panel.
//
// These are the numbers that decide whether a 200-500 thread run is healthy:
//   - Blocked: how many requests Reddit refused (the detection signal)
//   - Rate:    the CURRENT per-IP request rate (the AIMD moves it)
//   - AIMD:    the controller multiplier (1.00 = full speed)
//   - Queue:   how many workers wait for a token (the rate-limit signal)
//   - VPN:     how many exit-IP rotations happened
func (m *MultiChecker) rateStateRows() []string {
	snap := m.metrics.Snapshot()
	rate := m.cfg.RatePerIP
	mult := 1.0
	if m.aimd != nil {
		mult = m.aimd.Multiplier()
		rate = m.cfg.RatePerIP * mult
	}
	rot := int64(0)
	if m.pause != nil {
		rot = m.pause.Rotations()
	}
	return []string{
		fmt.Sprintf("Blocked=%d", snap.Blocked),
		fmt.Sprintf("Block rate=%.1f%%", snap.BlockPct),
		fmt.Sprintf("Rate=%.0f/min", rate),
		fmt.Sprintf("AIMD=%.2fx", mult),
		fmt.Sprintf("Queue=%d", snap.QueueNow),
		fmt.Sprintf("VPN rotations=%d", rot),
	}
}

// debugSnapshot prints a compact state line every 30 s while the run is
// going.
//
// The end-of-run panels are useless when a long run is killed or crashes, so
// the same information is streamed during the run. It only prints when the
// debug flag is on, and it is ONE line per 30 s.
func (m *MultiChecker) debugSnapshot(ctx context.Context, stop <-chan struct{}) {
	if !debugEnabled() {
		return
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
			snap := m.metrics.Snapshot()
			mult := 1.0
			if m.aimd != nil {
				mult = m.aimd.Multiplier()
			}
			rot := int64(0)
			if m.pause != nil {
				rot = m.pause.Rotations()
			}
			fmt.Printf("[debug] checked=%d ok=%d notfound=%d blocked=%d failed=%d | %.0f/min | aimd=%.2fx | queue=%d | vpn=%d | goroutines=%d\n",
				atomic.LoadInt64(&m.stats.Processed), snap.OK, snap.NotFound, snap.Blocked, snap.Failed,
				m.cfg.RatePerIP*mult, mult, snap.QueueNow, rot, snap.Goroutine)
		}
	}
}

// finishError records an error (the lane breaker handles the block streak).
func (m *MultiChecker) finishError(user string, index int, message, err string) {
	m.out.Append("errors.txt", fmt.Sprintf("%s | ERROR: %s", user, message))
	atomic.AddInt64(&m.stats.Error, 1)
	if err == "blocked" {
		atomic.AddInt64(&m.stats.Blocked, 1)
	}
	if err == "login_required" {
		atomic.AddInt64(&m.stats.AgeGate, 1)
	}
	if err == "challenge" {
		atomic.AddInt64(&m.stats.Challenge, 1)
	}
	// the error-reason breakdown: without it every non-block failure lands
	// in one "other" bucket and the real cause stays invisible
	m.metrics.RecordReason(err)
	progressf("[%d/%d] ERROR %s - %s\n", index+1, m.stats.Total, user, message)
	m.watchdog(user, message)
}

// networkDeadPatterns are error signatures that mean the WHOLE network path
// is gone (VPN tunnel down, DNS dead, kill switch engaged) - no amount of
// pacing or rotation fixes them, and every further user just burns the full
// ladder deadline measured in minutes. This is what used to look like
// "after ~60k accounts the script just keeps loading": the tunnel died
// mid-run and the engine crawled every remaining user into errors.txt.
var networkDeadPatterns = []string{
	"context deadline exceeded",
	"dial tcp",
	"dial udp",
	"dial tcp4",
	"dial tcp6",
	"no such host",
	"network is unreachable",
	"connection refused",
	"i/o timeout",
	"tls:",
	"handshake failure",
	"certificate signed by unknown authority",
	"http2: parsed header fields exceed limit",
	"body too large",
	"request canceled (Client.Timeout exceeded while awaiting headers)",
	"request canceled while waiting for connection",
}

// watchdog detects a dead network path and aborts the run fast.
//
// Any clean outcome (hit/notfound/suspended) or a normal error resets the
// counter, so a flaky single timeout never triggers it. Only a STREAK of
// consecutive network-fatal failures does - measured evidence: your errors.txt
// ends with dozens of "context deadline exceeded" / "no such host" in a row
// while the live panel just kept loading.
func (m *MultiChecker) watchdog(user, message string) {
	low := strings.ToLower(message)
	fatal := false
	for _, p := range networkDeadPatterns {
		if strings.Contains(low, p) {
			fatal = true
			break
		}
	}
	if !fatal {
		atomic.StoreInt64(&m.netDead, 0)
		return
	}
	streak := atomic.AddInt64(&m.netDead, 1)
	if streak < 30 {
		return
	}
	if m.abort.CompareAndSwap(false, true) {
		fmt.Fprintf(os.Stderr, `
==========================================================================
  STOPPED EARLY: the network path (VPN / internet / DNS) is dead.
  The last %d users in a row ALL failed with network errors, e.g.:
      "%s"
  This is NOT a rate limit - Reddit is unreachable through this exit IP.
  Recovery steps:
    1. Check ExpressVPN is actually CONNECTED (reconnect manually).
    2. Re-run run.bat - resume skips everything already saved.
  Stopped at user: %s
==========================================================================
`, streak, firstN(message, 120), user)
	}
}

// summary prints the final block (same layout as the Python checker) plus
// the full run report.
func (m *MultiChecker) summary() {
	s := m.stats
	elapsed := time.Since(m.start)
	line := strings.Repeat("=", 60)
	fmt.Printf("\n%s\n  RESULTS\n%s\n", line, line)
	fmt.Printf("  Checked:   %d of %d\n", s.Processed, s.Total)
	fmt.Printf("  Hits:      %d\n", s.Hit)
	fmt.Printf("  Not found: %d\n", s.NotFound)
	fmt.Printf("  Suspended: %d\n", s.Banned)
	fmt.Printf("  Errors:    %d  (blocked: %d)\n", s.Error, s.Blocked)
	fmt.Printf("  Skipped:   %d\n", s.Skipped)
	fmt.Printf("  Time:      %.1fs\n", elapsed.Seconds())
	fmt.Println(line)
	if m.pause != nil && m.pause.Rotations() > 0 {
		fmt.Printf("  VPN rotations: %d (see session_usage.txt)\n", m.pause.Rotations())
	}
	fmt.Printf("  Saved to: %s\\0to500.txt ... 3000ormore.txt\n", m.cfg.Output)
	if dropped := ProgressDropped(); dropped > 0 {
		fmt.Printf("  Progress lines suppressed: %d (output throttled to %d/s; every HIT and ERROR is kept)\n",
			dropped, progressMaxLinesPerSecond)
	}

	// the full accounting report (console + run_report.txt)
	sessions, sessionsOK := 0, 0
	if m.mgr != nil {
		for _, l := range m.mgr.All() {
			sessions++
			if l.Session != nil && l.Session.State == PoolHealthy {
				sessionsOK++
			}
		}
	}
	rep := &RunReport{
		Mode:         fmt.Sprintf("multi-session (%d sessions, %d lanes)", len(m.cfg.Sessions), m.cfg.Lanes),
		InputTotal:   s.InputTotal,
		Checked:      s.Processed,
		Skipped:      s.Skipped,
		Rejected:     m.cfg.InputRejected,
		Valid:        s.Hit,
		NotFound:     s.NotFound,
		Suspended:    s.Banned - s.Deleted,
		Deleted:      s.Deleted,
		AgeGate:      s.AgeGate,
		Challenge:    s.Challenge,
		Blocked:      s.Blocked,
		Failed:       s.Error - s.Blocked - s.AgeGate - s.Challenge,
		Duration:     elapsed,
		Sessions:     sessions,
		SessionsOK:   sessionsOK,
		OutputDir:    m.cfg.Output,
		StoppedEarly: m.abort.Load(),
		StartedAt:    m.start,
	}
	if m.pause != nil {
		rep.VPNRotations = m.pause.Rotations()
	}
	for i, b := range KarmaBuckets {
		rep.Buckets = append(rep.Buckets, s.Buckets[i])
		rep.BucketNames = append(rep.BucketNames, b.Name)
	}
	rep.Print()
	// the HTTP metrics: the block rate and the latency percentiles are the
	// two numbers that decide whether the run was clean or the exit IP is
	// being poisoned
	if snap := m.metrics.Snapshot(); snap.Requests > 0 {
		fmt.Println()
		fmt.Println("  HTTP METRICS")
		fmt.Print(snap.Report())
	}

	// debug panels (only when KARMA_DEBUG=1)
	PrintGroupPanel(m.rateTracker)
	PrintSourcePanel(m.sourceHealth)
}

// writeSessionReport shows which session handled what.
func (m *MultiChecker) writeSessionReport() {
	var sb strings.Builder
	sb.WriteString("SESSION USAGE REPORT\n")
	sb.WriteString(strings.Repeat("=", 60) + "\n")
	for _, l := range m.mgr.All() {
		name := "?"
		uses := int64(0)
		state := ""
		if l.Session != nil {
			name = l.Session.Name
			uses = l.Session.Uses
			state = l.Session.State
		}
		fmt.Fprintf(&sb, "lane %-3d session=%-26s uses=%-6d state=%-9s blocked=%v rotations=%d\n",
			l.ID, name, uses, state, l.IsBlocked(), l.Rotations())
	}
	sb.WriteString(strings.Repeat("=", 60) + "\n")
	if m.pause != nil {
		if hist := m.pause.History(); len(hist) > 0 {
			sb.WriteString("VPN ROTATIONS\n")
			for _, r := range hist {
				fmt.Fprintf(&sb, "#%-3d %-20s %-24s -> %-24s ip %s -> %s ok=%v reason=%s\n",
					r.Num, r.Time.Format("15:04:05"), orDash(r.FromRegion), orDash(r.ToRegion),
					orDash(r.FromIP), orDash(r.ToIP), r.OK, r.Reason)
			}
			sb.WriteString(strings.Repeat("=", 60) + "\n")
		}
	}
	sb.WriteString("TIME=" + time.Now().UTC().Format("2006-01-02 15:04:05 UTC") + "\n")
	os.WriteFile(filepath.Join(m.cfg.Output, "session_usage.txt"), []byte(sb.String()), 0644)
	fmt.Printf("  Session report: %s\n", filepath.Join(m.cfg.Output, "session_usage.txt"))
}
