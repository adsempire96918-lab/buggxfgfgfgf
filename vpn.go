// Package main - ExpressVPN IP rotation.
//
// When the throttle detector decides the exit IP is rate limited, every
// worker parks itself (no crash, no lost progress), the VPN is disconnected,
// reconnected to the NEXT region (which forces a fresh exit IP), the new IP
// is verified, lane/session state is reset, and work resumes.
//
// Every expressvpnctl interaction below was verified against the installed
// client (2026-11-09):
//
//	get connectionstate  -> Disconnected / Connecting / Connected / ...
//	get regions          -> one region id per line ("smart" is first)
//	get region           -> the selected region ("smart" = Smart Location)
//	get smart            -> the resolved smart region id
//	connect "<region>"   -> set the region and connect
//	disconnect           -> drop the tunnel
//	set networklock true -> kill switch (no leaks to the real IP)
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultExpressVPN is the standard Windows install path.
const DefaultExpressVPN = `C:\Program Files\ExpressVPN\expressvpnctl.exe`

// VPNConfig tunes the VPNController. Zero values get safe defaults.
type VPNConfig struct {
	Path           string        // expressvpnctl path ("" = rotation disabled)
	Rotate         bool          // master switch
	Gap            time.Duration // pause between disconnect and connect
	ConnectTimeout time.Duration // cap while polling for Connected
	DiscTimeout    time.Duration // cap while polling for Disconnected
	Poll           time.Duration // state poll interval
	Country        string        // only rotate regions containing this ("" = all)
	RegionsFile    string        // custom region list (one id per line)
	Threshold      int           // consecutive throttled results before a rotation
	WindowMin      int           // sliding-window samples before the ratio can fire
	WindowRatio    float64       // throttled ratio of the window that fires
	MaxRotations   int           // cap per run (0 = unlimited)
	MinInterval    time.Duration // debounce between rotations
	NetworkLock    bool          // enable the kill switch for the run
	VerifyIP       bool          // verify the exit IP really changed (ipify)
	Settle         time.Duration // pause after "Connected" before the IP probe
	VerifyTimeout  time.Duration // how long to keep polling for the new exit IP
}

// RotationRecord is one rotation attempt (success or failure).
type RotationRecord struct {
	Num        int64
	Time       time.Time
	Reason     string
	FromRegion string
	ToRegion   string
	FromIP     string
	ToIP       string
	OK         bool
	Detail     string
}

// VPNController owns the pause flag, the throttle detector and the rotation.
type VPNController struct {
	cfg      VPNConfig
	statOK   bool // expressvpnctl binary found
	paused   atomic.Bool
	rotating atomic.Bool
	capped   atomic.Bool
	detector *ThrottleDetector

	run   func(args ...string) (string, error)      // expressvpnctl runner (injectable)
	ipNow func(ctx context.Context) (string, error) // exit IP probe (injectable)

	mu         sync.Mutex
	regions    []string
	regionIdx  int
	lastRotate time.Time
	rotations  int64
	onRotated  func(rec RotationRecord)
	history    []RotationRecord
}

// NewVPNController builds the controller with defaults applied.
func NewVPNController(cfg VPNConfig) *VPNController {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 45 * time.Second
	}
	if cfg.DiscTimeout <= 0 {
		cfg.DiscTimeout = 10 * time.Second
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 500 * time.Millisecond
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 6
	}
	if cfg.WindowMin <= 0 {
		cfg.WindowMin = 20
	}
	if cfg.WindowRatio <= 0 {
		cfg.WindowRatio = 0.30
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = 30 * time.Second
	}
	if cfg.Settle <= 0 {
		// ExpressVPN reports "Connected" BEFORE the data tunnel has fully
		// switched to the new location. Probing the exit IP in that window
		// returns the OLD IP and the rotation is wrongly judged a failure.
		cfg.Settle = 4 * time.Second
	}
	if cfg.VerifyTimeout <= 0 {
		// keep polling for the new exit IP instead of one single shot
		cfg.VerifyTimeout = 20 * time.Second
	}
	v := &VPNController{
		cfg:      cfg,
		detector: NewThrottleDetector(cfg.Threshold, cfg.WindowMin, cfg.WindowRatio),
	}
	v.run = v.execCtl
	v.ipNow = probeExitIP
	_, err := os.Stat(cfg.Path)
	v.statOK = err == nil
	return v
}

// Enabled reports whether rotation can actually happen.
func (v *VPNController) Enabled() bool {
	return v != nil && v.cfg.Rotate && v.cfg.Path != "" && v.statOK
}

// IsPaused reports the global pause state (workers park while it is set).
// It also covers the tiny window between winning the rotation slot and
// raising the pause flag, so no worker slips a request through the old IP.
func (v *VPNController) IsPaused() bool { return v.paused.Load() || v.rotating.Load() }

// Rotations returns how many rotation attempts happened.
func (v *VPNController) Rotations() int64 { return atomic.LoadInt64(&v.rotations) }

// Threshold returns the consecutive-throttle rotation threshold (for UX).
func (v *VPNController) Threshold() int { return v.cfg.Threshold }

// SetOnRotated installs the post-rotation reset hook (called while the
// workers are still parked, so they resume onto a clean state).
func (v *VPNController) SetOnRotated(f func(rec RotationRecord)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.onRotated = f
}

// History returns the rotation attempts of this run (for the report files).
func (v *VPNController) History() []RotationRecord {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]RotationRecord{}, v.history...)
}

// execCtl runs expressvpnctl with a bounded context.
func (v *VPNController) execCtl(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.cfg.Path, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Wait blocks while a rotation is in progress. It returns false if the
// context was cancelled (the worker should stop cleanly).
func (v *VPNController) Wait(ctx context.Context) bool {
	for v.paused.Load() || v.rotating.Load() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true
}

// probeExitIP resolves the current public IP via api.ipify.org with a FRESH
// client: no cookies are attached, so nothing about the reddit sessions
// leaks to the third-party echo service.
func probeExitIP(ctx context.Context) (string, error) {
	c, err := newClient("", 8*time.Second, 0)
	if err != nil {
		return "", err
	}
	code, body, err := c.Get(ctx, "https://api.ipify.org", map[string]string{
		"Accept": "text/plain",
	})
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("ipify http_%d", code)
	}
	ip := strings.TrimSpace(body)
	if !strings.ContainsAny(ip, ".:") || len(ip) > 64 {
		return "", fmt.Errorf("unexpected ipify body %q", firstN(ip, 64))
	}
	return ip, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.TrimRight(s, "\r"))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// Arm applies the one-time run setup (Network Lock) and preloads the region
// pool so a misconfiguration is reported before the run, not mid-rotation.
func (v *VPNController) Arm() {
	if v == nil || !v.cfg.Rotate {
		return
	}
	if v.cfg.Path != "" && !v.statOK {
		fmt.Printf("VPN: expressvpnctl not found at %s - rotation DISABLED.\n", v.cfg.Path)
		return
	}
	if !v.Enabled() {
		return
	}
	if v.cfg.NetworkLock {
		if _, err := v.run("set", "networklock", "true"); err != nil {
			// The kill switch needs administrator rights on Windows. It is a
			// hardening extra, not a requirement - the rotation still works
			// without it, so the run continues.
			fmt.Printf("VPN: network lock not enabled (%v) - run as Administrator for the kill switch.\n", err)
		} else {
			fmt.Println("VPN: Network Lock ON (no traffic can leak to the real IP).")
		}
	}
	ctx := context.Background()
	country := v.cfg.Country
	if country == "" {
		country = "all"
	}
	if regions := v.regionPool(ctx); len(regions) > 0 {
		fmt.Printf("VPN rotation armed: %d regions (country filter %q), threshold %d, cap %d.\n",
			len(regions), country, v.cfg.Threshold, v.cfg.MaxRotations)
	} else {
		fmt.Println("VPN: region list is empty - rotation DISABLED.")
	}
}

// regionPool returns the rotation pool, loading it once (region file or the
// daemon's own `get regions` list). "smart" is excluded: Smart Location can
// resolve to the same server again, which would keep the same exit IP.
func (v *VPNController) regionPool(ctx context.Context) []string {
	v.mu.Lock()
	cached := v.regions
	v.mu.Unlock()
	if cached != nil {
		return cached
	}
	var lines []string
	if v.cfg.RegionsFile != "" {
		raw, err := os.ReadFile(v.cfg.RegionsFile)
		if err != nil {
			fmt.Printf("VPN: cannot read %s: %v\n", v.cfg.RegionsFile, err)
			return nil
		}
		lines = strings.Split(string(raw), "\n")
	} else {
		out, err := v.run("get", "regions")
		if err != nil {
			fmt.Printf("VPN: get regions failed: %v\n", err)
			return nil
		}
		lines = strings.Split(out, "\n")
	}
	all := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" || l == "smart" || strings.HasPrefix(l, "#") {
			continue
		}
		all = append(all, l)
	}
	regions := all
	if v.cfg.Country != "" {
		filtered := make([]string, 0, len(all))
		for _, r := range all {
			if strings.Contains(r, strings.ToLower(v.cfg.Country)) {
				filtered = append(filtered, r)
			}
		}
		if len(filtered) == 0 {
			fmt.Printf("VPN: no region matches %q - using the full pool.\n", v.cfg.Country)
		} else {
			regions = filtered
		}
	}
	v.mu.Lock()
	if v.regions == nil {
		v.regions = regions
	}
	out := v.regions
	v.mu.Unlock()
	return out
}

// nextRegion returns the next pool region (skipping the current one).
func (v *VPNController) nextRegion(regions []string, current string) string {
	if len(regions) == 0 {
		return ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := 0; i < len(regions); i++ {
		r := regions[v.regionIdx%len(regions)]
		v.regionIdx = (v.regionIdx + 1) % len(regions)
		if r != current {
			return r
		}
	}
	return ""
}

// regionCountry extracts the country prefix of an ExpressVPN region id.
// ExpressVPN names regions "<country>-<city>" (e.g. "usa-new-york",
// "germany-frankfurt-1", "india-(via-singapore)"), so the part before the
// first dash is the country.
func regionCountry(region string) string {
	r := strings.ToLower(strings.TrimSpace(region))
	if r == "" {
		return ""
	}
	if i := strings.Index(r, "-"); i > 0 {
		return r[:i]
	}
	return r
}

// nextRegionDifferentCountry prefers a region in a DIFFERENT country than the
// one we are on right now. Reddit's rate-limit budget is per exit IP, but a
// different country also gives a different network path and a different
// reputation score, so rotating across countries is strictly better than
// rotating between two cities of the same country.
//
// It falls back to the plain round-robin when every region in the pool shares
// the current country (e.g. the user pinned --vpn-country).
func (v *VPNController) nextRegionDifferentCountry(regions []string, current string) string {
	if len(regions) == 0 {
		return ""
	}
	curCountry := regionCountry(current)

	// Pass 1: a region from another country.
	v.mu.Lock()
	start := v.regionIdx
	for i := 0; i < len(regions); i++ {
		idx := (start + i) % len(regions)
		r := regions[idx]
		if r == current {
			continue
		}
		if curCountry != "" && regionCountry(r) != curCountry {
			v.regionIdx = (idx + 1) % len(regions)
			v.mu.Unlock()
			return r
		}
	}
	// Pass 2: any other region (same country is still a new IP).
	for i := 0; i < len(regions); i++ {
		idx := (start + i) % len(regions)
		r := regions[idx]
		if r != current {
			v.regionIdx = (idx + 1) % len(regions)
			v.mu.Unlock()
			return r
		}
	}
	v.mu.Unlock()
	return ""
}

// currentRegion resolves the region the daemon is connected to right now.
func (v *VPNController) currentRegion() string {
	out, err := v.run("get", "region")
	r := firstLine(out)
	if err != nil || r == "" {
		return ""
	}
	if strings.EqualFold(r, "smart") {
		if out2, err2 := v.run("get", "smart"); err2 == nil {
			if s := firstLine(out2); s != "" {
				return s
			}
		}
		return "smart"
	}
	return r
}

// Report feeds one user-check outcome into the throttle detector and starts
// a rotation when the detector fires. clean=true for every outcome that is
// NOT an IP-level throttle (including notfound/suspended and real errors).
func (v *VPNController) Report(clean bool, reason string) bool {
	if v == nil || !v.Enabled() || v.capped.Load() {
		return false
	}
	if !v.detector.Report(clean) {
		return false
	}
	return v.Trigger(reason)
}

// ReportIPBlocked triggers a rotation immediately. A Reddit block page is
// DEFINITIVE proof that this exit IP is blocked - waiting for the
// consecutive-throttle threshold would keep hammering a dead IP (and the
// circuit breaker would open before the detector ever fired).
func (v *VPNController) ReportIPBlocked(reason string) bool {
	if v == nil || !v.Enabled() || v.capped.Load() {
		return false
	}
	return v.Trigger(reason)
}

// ReportThrottled reacts to an explicit rate-limit response (HTTP 429 /
// "rate_limited"). Unlike the consecutive-throttle detector this fires on the
// FIRST rate-limit answer: the exit IP is already over budget, so every
// further request on it is wasted. The rotation is debounced by MinInterval,
// so a burst of 429s still produces ONE rotation, not one per response.
func (v *VPNController) ReportThrottled(reason string) bool {
	if v == nil || !v.Enabled() || v.capped.Load() {
		return false
	}
	return v.Trigger(reason)
}

// Trigger pauses every worker and starts ONE rotation goroutine. It returns
// true only for the caller that actually performs the rotation.
func (v *VPNController) Trigger(reason string) bool {
	if v == nil || !v.Enabled() || v.capped.Load() {
		return false
	}
	v.mu.Lock()
	if !v.lastRotate.IsZero() && time.Since(v.lastRotate) < v.cfg.MinInterval {
		v.mu.Unlock()
		return false // debounce: rotation just happened
	}
	v.mu.Unlock()
	// Claim the rotation slot FIRST - only the winner pauses the workers.
	if !v.rotating.CompareAndSwap(false, true) {
		return false // another worker is already rotating
	}
	v.paused.Store(true)
	fmt.Printf("Rate limit sustained (%s) - pausing all workers and rotating the VPN IP ...\n", reason)
	go v.rotate(reason)
	return true
}

// waitState polls `get connectionstate` until the wanted state or timeout.
func (v *VPNController) waitState(ctx context.Context, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var state string
	for {
		out, err := v.run("get", "connectionstate")
		state = firstLine(out)
		if err == nil && strings.EqualFold(state, want) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s (state=%q)", want, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(v.cfg.Poll):
		}
	}
}

// rotate performs disconnect -> gap -> connect(next region) -> verify, then
// always resumes the workers (even on failure - they continue governed).
func (v *VPNController) rotate(reason string) {
	defer v.rotating.Store(false)
	defer v.paused.Store(false)

	ctx := context.Background()
	rec := RotationRecord{Time: time.Now(), Reason: reason}
	rec.Num = atomic.AddInt64(&v.rotations, 1)
	if v.cfg.MaxRotations > 0 && int(rec.Num) > v.cfg.MaxRotations {
		v.capped.Store(true)
		fmt.Println("VPN rotation cap reached - continuing at the governed rate.")
		return
	}
	defer func() {
		v.mu.Lock()
		v.history = append(v.history, rec)
		onRotated := v.onRotated
		v.mu.Unlock()
		if rec.OK && onRotated != nil {
			onRotated(rec)
		}
	}()

	// 1. snapshot the current exit IP + region
	if v.cfg.VerifyIP {
		rec.FromIP, _ = v.ipNow(ctx)
	}
	rec.FromRegion = v.currentRegion()

	// 2. disconnect and confirm the state
	if out, err := v.run("disconnect"); err != nil {
		fmt.Printf("expressvpnctl disconnect failed: %v (%s)\n", err, firstLine(out))
	}
	if err := v.waitState(ctx, "Disconnected", v.cfg.DiscTimeout); err != nil {
		fmt.Printf("VPN: disconnect not confirmed (%v) - continuing anyway.\n", err)
	}

	// 3. the requested gap between disconnect and connect
	if v.cfg.Gap > 0 {
		time.Sleep(v.cfg.Gap)
	}

	// 4. connect to the NEXT region (up to 3 candidates until the IP changes).
	// The first candidate is in a DIFFERENT COUNTRY than the current one, so a
	// rate-limited exit IP is replaced by a fresh network path, not just a
	// different city of the same country.
	regions := v.regionPool(ctx)
	for attempt := 0; attempt < 3; attempt++ {
		region := v.nextRegionDifferentCountry(regions, rec.FromRegion)
		if region == "" {
			region = v.nextRegion(regions, rec.FromRegion)
		}
		if region == "" {
			rec.Detail = "region pool empty"
			break
		}
		fmt.Printf("VPN: connecting to %s ...\n", region)
		if out, err := v.run("connect", region); err != nil {
			rec.Detail = firstLine(out)
			fmt.Printf("expressvpnctl connect %s failed: %v (%s)\n", region, err, firstLine(out))
			continue
		}
		if err := v.waitState(ctx, "Connected", v.cfg.ConnectTimeout); err != nil {
			rec.Detail = err.Error()
			fmt.Printf("VPN: %s did not reach Connected (%v) - trying the next region.\n", region, err)
			continue
		}
		rec.ToRegion = region
		if !v.cfg.VerifyIP {
			rec.OK = true
			break
		}
		// ExpressVPN reports "Connected" before the data tunnel has really
		// switched to the new location, and it also rotates the exit IP on
		// its own inside a location. A single IP probe right after the
		// connect therefore often sees the OLD IP and the whole rotation is
		// wrongly judged a failure. So: wait for the tunnel to settle, then
		// KEEP POLLING until the exit IP differs from the old one.
		newIP, ok := v.waitNewExitIP(ctx, rec.FromIP)
		if ok {
			rec.ToIP, rec.OK = newIP, true
			break
		}
		rec.Detail = "exit IP unchanged"
		fmt.Printf("VPN: %s exit IP did not change - trying the next region.\n", region)
		if out, derr := v.run("disconnect"); derr != nil {
			fmt.Printf("expressvpnctl disconnect failed: %v (%s)\n", derr, firstLine(out))
		}
		_ = v.waitState(ctx, "Disconnected", v.cfg.DiscTimeout)
		if v.cfg.Gap > 0 {
			time.Sleep(v.cfg.Gap)
		}
	}

	v.mu.Lock()
	v.lastRotate = time.Now()
	v.mu.Unlock()

	if rec.OK {
		// The fresh exit IP starts with a CLEAN detector: the old IP's
		// throttle streak must not carry over (it would re-rotate at once).
		v.detector.Reset()
		fmt.Printf("VPN rotation #%d done: %s -> %s | IP %s -> %s (reason: %s)\n",
			rec.Num, orDash(rec.FromRegion), rec.ToRegion, orDash(rec.FromIP), orDash(rec.ToIP), reason)
	} else {
		fmt.Printf("VPN rotation #%d FAILED (%s) - resuming at the governed rate.\n",
			rec.Num, orDash(rec.Detail))
	}
}

// waitNewExitIP waits until the public exit IP differs from oldIP.
//
// Two things make a single probe unreliable:
//   - ExpressVPN reports "Connected" before the data tunnel has really
//     switched to the new location, so the first probes still return the
//     OLD exit IP;
//   - ExpressVPN rotates the exit IP inside a location on its own, so two
//     probes a few seconds apart can legitimately differ.
//
// The fix is a settle pause followed by polling: the rotation counts as
// successful as soon as ANY probe returns a different IP.
func (v *VPNController) waitNewExitIP(ctx context.Context, oldIP string) (string, bool) {
	if v.cfg.Settle > 0 {
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(v.cfg.Settle):
		}
	}
	deadline := time.Now().Add(v.cfg.VerifyTimeout)
	// the poll interval is 1s in production, but a short VerifyTimeout (the
	// unit tests use it) has to poll faster or the test would sleep for a
	// whole second per probe.
	poll := time.Second
	if v.cfg.VerifyTimeout > 0 && v.cfg.VerifyTimeout < 4*time.Second {
		poll = v.cfg.VerifyTimeout / 4
		if poll < 10*time.Millisecond {
			poll = 10 * time.Millisecond
		}
	}
	for {
		ip, err := v.ipNow(ctx)
		if err == nil && ip != "" && ip != oldIP {
			return ip, true
		}
		if !time.Now().Before(deadline) {
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(poll):
		}
	}
}

// ThrottleDetector decides when the exit IP is genuinely rate limited: N
// consecutive throttled results, or a throttled ratio over a sliding window.
// A single transient 429 never fires it (a rotation costs seconds).
type ThrottleDetector struct {
	mu          sync.Mutex
	consecutive int
	window      []bool // true = clean
	maxConsec   int
	minSamples  int
	maxRatio    float64
}

// NewThrottleDetector with defaults on zero values.
func NewThrottleDetector(maxConsec, minSamples int, maxRatio float64) *ThrottleDetector {
	if maxConsec <= 0 {
		maxConsec = 6
	}
	if minSamples <= 0 {
		minSamples = 20
	}
	if maxRatio <= 0 {
		maxRatio = 0.30
	}
	return &ThrottleDetector{maxConsec: maxConsec, minSamples: minSamples, maxRatio: maxRatio}
}

// Reset clears the detector state. Called after a successful rotation: the
// new exit IP must not inherit the previous IP's throttle streak.
func (d *ThrottleDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.consecutive = 0
	d.window = d.window[:0]
}

// Report records one outcome; returns true exactly once per incident, when
// a rotation should fire (the window resets after firing).
func (d *ThrottleDetector) Report(clean bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.window = append(d.window, clean)
	if len(d.window) > 100 {
		d.window = d.window[len(d.window)-100:]
	}
	if clean {
		d.consecutive = 0
	} else {
		d.consecutive++
	}
	fire := false
	switch {
	case d.consecutive >= d.maxConsec:
		fire = true
	case len(d.window) >= d.minSamples:
		bad := 0
		for _, c := range d.window {
			if !c {
				bad++
			}
		}
		if float64(bad)/float64(len(d.window)) >= d.maxRatio {
			fire = true
		}
	}
	if fire {
		d.consecutive = 0
		d.window = d.window[:0]
	}
	return fire
}
