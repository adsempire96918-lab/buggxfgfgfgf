// Package main - VPN rotation tests (no real VPN touched): throttle detector,
// region pool, full rotate() against a scripted fake expressvpnctl, and one
// end-to-end run through a mock .bat executed by the real exec path.
package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testVPNController builds a controller with fast polling for tests.
func testVPNController() *VPNController {
	v := NewVPNController(VPNConfig{
		Path:           "fake-ctl",
		Rotate:         true,
		Poll:           10 * time.Millisecond,
		ConnectTimeout: 2 * time.Second,
		DiscTimeout:    500 * time.Millisecond,
		Gap:            10 * time.Millisecond,
		MinInterval:    0, // no debounce in flow tests
		Threshold:      3,
		VerifyIP:       true,
		Settle:         10 * time.Millisecond,  // no tunnel wait in tests
		VerifyTimeout:  400 * time.Millisecond, // fast IP polling in tests
	})
	v.statOK = true // pretend the binary exists (runner is replaced)
	return v
}

// fakeCtl is a scripted expressvpnctl.
type fakeCtl struct {
	mu        sync.Mutex
	calls     []string
	state     string
	regions   []string
	region    string // current (smart-resolved) region
	connectOK bool   // whether connect leads to Connected
}

func (f *fakeCtl) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "get":
		switch args[1] {
		case "connectionstate":
			return f.state + "\n", nil
		case "regions":
			return strings.Join(append([]string{"smart"}, f.regions...), "\n") + "\n", nil
		case "region":
			return "smart\n", nil
		case "smart":
			return f.region + "\n", nil
		}
	case "disconnect":
		f.state = "Disconnected"
		return "", nil
	case "connect":
		f.region = args[1]
		if f.connectOK {
			f.state = "Connected"
		}
		return "", nil
	}
	return "", nil
}

func (f *fakeCtl) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

// scriptedIP returns queued IPs, then repeats the last one.
type scriptedIP struct {
	mu   sync.Mutex
	vals []string
	i    int
}

func (s *scriptedIP) next(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.vals[min(s.i, len(s.vals)-1)]
	if s.i < len(s.vals) {
		s.i++
	}
	return v, nil
}

func firstCallIdx(calls []string, prefix string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestThrottleDetectorConsecutive(t *testing.T) {
	d := NewThrottleDetector(3, 20, 0.99)
	if d.Report(false) || d.Report(false) {
		t.Fatal("fired before the threshold")
	}
	if !d.Report(false) {
		t.Fatal("did not fire at 3 consecutive")
	}
	// fired: the incident reset -> next failures count from zero again
	if d.Report(false) {
		t.Fatal("fired again without a full streak")
	}
}

func TestThrottleDetectorCleanResetsStreak(t *testing.T) {
	d := NewThrottleDetector(3, 20, 0.99)
	d.Report(false)
	d.Report(false)
	d.Report(true) // streak broken
	if d.Report(false) || d.Report(false) {
		t.Fatal("streak survived a clean result")
	}
}

// A rotation gives the new exit IP a clean slate: the previous IP's streak
// must not carry over (it would fire another rotation almost immediately).
func TestThrottleDetectorResetClearsStreak(t *testing.T) {
	d := NewThrottleDetector(3, 20, 0.99)
	d.Report(false)
	d.Report(false) // 2 of 3 - one more would fire
	d.Reset()
	if d.Report(false) || d.Report(false) {
		t.Fatal("streak survived the reset")
	}
	if !d.Report(false) {
		t.Fatal("did not fire at 3 consecutive after the reset")
	}
}

// The detector must not fire on a single transient result.
func TestThrottleDetectorSingleFailureNeverFires(t *testing.T) {
	d := NewThrottleDetector(6, 20, 0.99)
	if d.Report(false) {
		t.Fatal("fired on a single throttled result")
	}
}

func TestThrottleDetectorWindowRatio(t *testing.T) {
	// 25% bad must NOT fire; 30% must fire (consecutive threshold kept high).
	d := NewThrottleDetector(10, 20, 0.30)
	fired := false
	for i := 0; i < 18; i++ {
		if d.Report(true) {
			t.Fatal("fired on clean results")
		}
	}
	for i := 0; i < 6; i++ { // 18 clean + 6 bad = 24 samples, 25% bad
		if d.Report(false) {
			fired = true
		}
	}
	if fired {
		t.Fatal("fired below the window ratio")
	}
	d2 := NewThrottleDetector(10, 20, 0.30)
	fired = false
	for i := 0; i < 14; i++ {
		if d2.Report(true) {
			t.Fatal("fired on clean results")
		}
	}
	for i := 0; i < 6; i++ { // 14 clean + 6 bad = 20 samples, 30% bad
		if d2.Report(false) {
			fired = true
		}
	}
	if !fired {
		t.Fatal("did not fire at exactly 30% bad over 20 samples")
	}
}

func TestRegionPoolExcludesSmartAndFilters(t *testing.T) {
	v := testVPNController()
	f := &fakeCtl{state: "Connected", regions: []string{"usa-east", "usa-west", "germany-1"}, region: "usa-east", connectOK: true}
	v.run = f.run
	pool := v.regionPool(context.Background())
	if len(pool) != 3 {
		t.Fatalf("pool = %v, want 3 regions", pool)
	}
	for _, r := range pool {
		if r == "smart" {
			t.Fatal("smart leaked into the pool")
		}
	}
	// cached: a second call must not hit the daemon again
	c1 := len(f.callLog())
	v.regionPool(context.Background())
	if len(f.callLog()) != c1 {
		t.Fatal("region pool not cached")
	}
	// country filter
	v2 := testVPNController()
	v2.cfg.Country = "usa"
	v2.run = f.run
	pool = v2.regionPool(context.Background())
	if len(pool) != 2 || pool[0] != "usa-east" || pool[1] != "usa-west" {
		t.Fatalf("country filter = %v", pool)
	}
}

func TestNextRegionCycleSkipsCurrent(t *testing.T) {
	regions := []string{"a", "b", "c"}
	v2 := testVPNController()
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		seen[v2.nextRegion(regions, "b")] = true
	}
	if seen["b"] || len(seen) != 2 {
		t.Fatalf("seen = %v, want {a,c}", seen)
	}
	if r := v2.nextRegion(nil, "a"); r != "" {
		t.Fatalf("empty pool next = %q", r)
	}
}

func waitRotationDone(t *testing.T, v *VPNController) {
	t.Helper()
	for i := 0; i < 500 && (v.IsPaused() || v.Rotations() == 0); i++ {
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRotateHappyFlow(t *testing.T) {
	v := testVPNController()
	f := &fakeCtl{state: "Connected", regions: []string{"usa-east", "usa-west", "germany-1"}, region: "singapore-jurong", connectOK: true}
	v.run = f.run
	v.ipNow = (&scriptedIP{vals: []string{"103.1.1.1", "134.2.2.2"}}).next
	var got RotationRecord
	done := make(chan struct{})
	v.SetOnRotated(func(rec RotationRecord) { got = rec; close(done) })

	if !v.Trigger("test") {
		t.Fatal("trigger refused")
	}
	waitRotationDone(t, v)
	if v.Rotations() != 1 {
		t.Fatalf("rotations = %d", v.Rotations())
	}
	calls := f.callLog()
	di, ci := firstCallIdx(calls, "disconnect"), firstCallIdx(calls, "connect")
	if di < 0 || ci < 0 || di > ci {
		t.Fatalf("disconnect/connect order wrong: %v", calls)
	}
	hist := v.History()
	if len(hist) != 1 || !hist[0].OK {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].ToRegion == hist[0].FromRegion || hist[0].ToRegion == "" {
		t.Fatalf("region did not change: %+v", hist[0])
	}
	if hist[0].FromIP != "103.1.1.1" || hist[0].ToIP != "134.2.2.2" {
		t.Fatalf("IPs wrong: %+v", hist[0])
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onRotated never fired")
	}
	if got.ToRegion != hist[0].ToRegion {
		t.Fatal("onRotated got a different record than the history")
	}
	if v.IsPaused() {
		t.Fatal("workers still parked after the rotation")
	}
}

// TestRotatePollsUntilNewIP proves the rotation KEEPS POLLING the exit IP
// after "Connected" instead of taking a single shot.
//
// ExpressVPN reports "Connected" before the data tunnel has really switched
// to the new location, so the first probe still returns the OLD IP. The
// rotation must wait for the tunnel to settle and keep probing until the IP
// really changed - otherwise every rotation is judged a failure.
func TestRotatePollsUntilNewIP(t *testing.T) {
	v := testVPNController()
	f := &fakeCtl{state: "Connected", regions: []string{"usa-east", "usa-west", "germany-1"}, region: "singapore-jurong", connectOK: true}
	v.run = f.run
	// old IP, then the SAME old IP (tunnel not switched yet), then the new one
	v.ipNow = (&scriptedIP{vals: []string{"103.1.1.1", "103.1.1.1", "134.2.2.2"}}).next
	v.Trigger("test")
	waitRotationDone(t, v)
	hist := v.History()
	if len(hist) != 1 || !hist[0].OK {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].ToIP != "134.2.2.2" {
		t.Fatalf("ToIP = %q, want 134.2.2.2", hist[0].ToIP)
	}
	// the polling kept the FIRST region - no needless second connect
	connects := 0
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "connect") {
			connects++
		}
	}
	if connects != 1 {
		t.Fatalf("connects = %d, want 1 (the IP changed while polling): %v", connects, f.callLog())
	}
}

// TestRotateSameIPNextRegion proves that when the exit IP NEVER changes the
// rotation moves on to the next region instead of giving up.
func TestRotateSameIPNextRegion(t *testing.T) {
	v := testVPNController()
	f := &fakeCtl{state: "Connected", regions: []string{"usa-east", "usa-west", "germany-1"}, region: "singapore-jurong", connectOK: true}
	v.run = f.run
	// the IP never changes - every probe returns the old one
	v.ipNow = (&scriptedIP{vals: []string{"103.1.1.1"}}).next
	v.Trigger("test")
	waitRotationDone(t, v)
	hist := v.History()
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].OK {
		t.Fatalf("rotation reported OK although the IP never changed: %+v", hist[0])
	}
	connects := 0
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "connect") {
			connects++
		}
	}
	if connects != 3 {
		t.Fatalf("connects = %d, want 3 (tries all candidates): %v", connects, f.callLog())
	}
	if v.IsPaused() {
		t.Fatal("workers parked forever after a failed rotation")
	}
}

func TestRotateNeverConnects(t *testing.T) {
	v := testVPNController()
	v.cfg.ConnectTimeout = 300 * time.Millisecond // fast failure path
	// three regions, current not in the pool; connect never reaches Connected
	f := &fakeCtl{state: "Connected", regions: []string{"usa-east", "usa-west", "germany-1"}, region: "singapore-jurong", connectOK: false}
	v.run = f.run
	v.ipNow = (&scriptedIP{vals: []string{"103.1.1.1", "103.1.1.1"}}).next
	v.Trigger("test")
	waitRotationDone(t, v)
	hist := v.History()
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	if hist[0].OK {
		t.Fatalf("rotation reported OK while never Connected: %+v", hist[0])
	}
	connects := 0
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "connect") {
			connects++
		}
	}
	if connects != 3 {
		t.Fatalf("connects = %d, want 3 (tries all candidates): %v", connects, f.callLog())
	}
	if v.IsPaused() {
		t.Fatal("workers parked forever after a failed rotation")
	}
}

func TestTriggerDebounceAndCap(t *testing.T) {
	v := testVPNController()
	v.cfg.MinInterval = time.Hour // debounce active
	v.cfg.MaxRotations = 1
	f := &fakeCtl{state: "Connected", regions: []string{"a", "b"}, region: "x", connectOK: true}
	v.run = f.run
	v.ipNow = (&scriptedIP{vals: []string{"1.1.1.1", "2.2.2.2"}}).next
	if !v.Trigger("first") {
		t.Fatal("first trigger refused")
	}
	waitRotationDone(t, v)
	if v.Rotations() != 1 {
		t.Fatalf("rotations = %d", v.Rotations())
	}
	if v.Trigger("debounced") {
		t.Fatal("debounced trigger accepted")
	}
	// travel past the debounce window and hit the rotation cap instead
	v.mu.Lock()
	v.lastRotate = time.Now().Add(-2 * time.Hour)
	v.mu.Unlock()
	v.Trigger("over cap")
	for i := 0; i < 200 && v.IsPaused(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if v.Rotations() != 2 {
		t.Fatalf("rotations = %d, want 2 (the capped attempt still counts)", v.Rotations())
	}
	if !v.capped.Load() {
		t.Fatal("cap not latched")
	}
	if v.Trigger("capped") {
		t.Fatal("trigger accepted after the cap")
	}
}

func TestCircuitBreakerReset(t *testing.T) {
	c := NewCircuitBreaker(2, 15*time.Minute)
	c.Report(true)
	c.Report(true)
	if c.State() != StateOpen {
		t.Fatal("breaker not open")
	}
	c.Reset()
	if c.State() != StateClosed {
		t.Fatal("breaker not closed after reset")
	}
	// counter cleared: two fresh blocks open it again (not one)
	c.Report(true)
	if c.State() != StateClosed {
		t.Fatal("consecutive counter survived the reset")
	}
}

// mockCtlBat is a full expressvpnctl simulation: connection state lives in a
// file, connects are logged. Written by the end-to-end test into a temp dir.
const mockCtlBat = "@echo off\r\n" +
	"setlocal\r\n" +
	"set S=%~dp0mockvpn_state.txt\r\n" +
	"if \"%~1\"==\"get\" goto GET\r\n" +
	"if \"%~1\"==\"disconnect\" ( >\"%S%\" echo Disconnected & goto END )\r\n" +
	"if \"%~1\"==\"connect\" ( >\"%S%\" echo Connected & >>\"%~dp0mockvpn_log.txt\" echo connect %~2 & goto END )\r\n" +
	"goto END\r\n" +
	":GET\r\n" +
	"if \"%~2\"==\"connectionstate\" ( if exist \"%S%\" ( type \"%S%\" ) else ( echo Disconnected ) & goto END )\r\n" +
	"if \"%~2\"==\"regions\" ( echo usa-east& echo usa-west& echo germany-1& goto END )\r\n" +
	"if \"%~2\"==\"region\" ( echo smart & goto END )\r\n" +
	"if \"%~2\"==\"smart\" ( echo singapore-jurong & goto END )\r\n" +
	"goto END\r\n" +
	":END\r\n" +
	"endlocal\r\n"

// TestMockCtlEndToEnd drives the REAL exec path against the mock .bat: the
// controller is not touched at all, only the ipify probe is faked.
func TestMockCtlEndToEnd(t *testing.T) {
	dir := t.TempDir()
	bat := filepath.Join(dir, "mock-expressvpnctl.bat")
	if err := os.WriteFile(bat, []byte(mockCtlBat), 0644); err != nil {
		t.Fatalf("write mock: %v", err)
	}
	v := NewVPNController(VPNConfig{
		Path:        bat,
		Rotate:      true,
		Poll:        30 * time.Millisecond,
		Gap:         20 * time.Millisecond,
		MinInterval: 0,
	})
	if !v.Enabled() {
		t.Skip("controller not enabled (bat not executable here)")
	}
	v.ipNow = (&scriptedIP{vals: []string{"103.1.1.1", "134.2.2.2"}}).next
	if !v.Trigger("mock") {
		t.Fatal("trigger refused")
	}
	waitRotationDone(t, v)
	if v.Rotations() != 1 {
		t.Fatalf("rotations = %d", v.Rotations())
	}
	hist := v.History()
	if len(hist) != 1 || !hist[0].OK {
		log, _ := os.ReadFile(filepath.Join(dir, "mockvpn_log.txt"))
		t.Fatalf("history = %+v log=%s", hist, log)
	}
	if hist[0].ToRegion != "usa-east" {
		t.Fatalf("ToRegion = %q, want usa-east (first pool entry)", hist[0].ToRegion)
	}
	state, _ := os.ReadFile(filepath.Join(dir, "mockvpn_state.txt"))
	if !strings.Contains(string(state), "Connected") {
		t.Fatalf("end state = %q", state)
	}
}
