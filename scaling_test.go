// Package main - tests for the session-pool scaling behaviour.
//
// A big session pool ("sassion" with hundreds of accounts) is what makes the
// checker resilient: every lane starts on its own account, a blocked session
// is replaced by an unused one IMMEDIATELY (no 30s dead time), and the pool
// never runs out of accounts.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolHandsOutUnleasedSessionsFirst proves that with a large pool every
// lane gets its OWN session instead of all lanes sharing the first few.
func TestPoolHandsOutUnleasedSessionsFirst(t *testing.T) {
	sessions := make([]*Session, 50)
	for i := range sessions {
		sessions[i] = &Session{Name: "s", Bucket: NewTokenBucket(12000, 2)}
	}
	pool := NewSessionPool(sessions, 12000, 2)

	seen := map[*Session]bool{}
	for i := 0; i < 50; i++ {
		s := pool.Next()
		if s == nil {
			t.Fatalf("pool returned nil at %d", i)
		}
		if seen[s] {
			t.Fatalf("session %s handed out twice while free sessions remained", s.Name)
		}
		seen[s] = true
	}
}

// TestPoolReleaseMakesSessionReusable proves a released session can be
// handed out again (the pool never permanently loses an account).
func TestPoolReleaseMakesSessionReusable(t *testing.T) {
	sessions := make([]*Session, 3)
	for i := range sessions {
		sessions[i] = &Session{Name: "s", Bucket: NewTokenBucket(12000, 2)}
	}
	pool := NewSessionPool(sessions, 12000, 2)

	first := pool.Next()
	if first == nil {
		t.Fatal("no session")
	}
	first.Release()

	got := false
	for i := 0; i < 3; i++ {
		if pool.Next() == first {
			got = true
			break
		}
	}
	if !got {
		t.Fatal("released session was never handed out again")
	}
}

// TestLaneBlockedSwapsToFreshSession proves the key scaling behaviour: a
// blocked session is replaced by a fresh one right away instead of parking
// the lane for the 30s block cooldown.
func TestLaneBlockedSwapsToFreshSession(t *testing.T) {
	sessions := make([]*Session, 4)
	for i := range sessions {
		sessions[i] = &Session{Name: "s", Bucket: NewTokenBucket(12000, 2)}
	}
	pool := NewSessionPool(sessions, 12000, 2)

	first := pool.Next()
	l := &Lane{
		ID:      0,
		Session: first,
		pool:    pool,
		timeout: 5 * time.Second,
		maxUse:  25,
	}

	if !l.RotateToFreshSession() {
		t.Fatal("RotateToFreshSession returned false with a fresh session available")
	}
	if l.Session == first {
		t.Fatal("lane still holds the old session")
	}
	if l.IsBlocked() {
		t.Fatal("lane is still flagged blocked after the swap")
	}
	if l.rotated != 1 {
		t.Fatalf("rotated = %d, want 1", l.rotated)
	}
}

// TestBlockedSessionIsReleased proves the old session goes back to the pool
// when a lane rotates away from it.
func TestBlockedSessionIsReleased(t *testing.T) {
	sessions := make([]*Session, 2)
	for i := range sessions {
		sessions[i] = &Session{Name: "s", Bucket: NewTokenBucket(12000, 2)}
	}
	pool := NewSessionPool(sessions, 12000, 2)

	old := pool.Next()
	l := &Lane{ID: 0, Session: old, pool: pool, timeout: 5 * time.Second, maxUse: 25}

	l.RotateToFreshSession()

	old.mu.Lock()
	leased := old.leased
	old.mu.Unlock()
	if leased {
		t.Fatal("the old session is still leased after the lane rotated away")
	}
}

// TestRegionCountryParsing proves the country prefix extraction used by the
// country-diverse VPN rotation.
func TestRegionCountryParsing(t *testing.T) {
	cases := map[string]string{
		"usa-new-york":          "usa",
		"germany-frankfurt-1":   "germany",
		"india-(via-singapore)": "india",
		"bangladesh":            "bangladesh",
		"singapore-cbd":         "singapore",
		"":                      "",
	}
	for in, want := range cases {
		if got := regionCountry(in); got != want {
			t.Errorf("regionCountry(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNextRegionDifferentCountry proves the rotation prefers another country
// over another city of the same country.
func TestNextRegionDifferentCountry(t *testing.T) {
	v := NewVPNController(VPNConfig{Rotate: true})
	regions := []string{"usa-new-york", "usa-boston", "germany-berlin", "japan-tokyo"}

	got := v.nextRegionDifferentCountry(regions, "usa-new-york")
	if got == "" {
		t.Fatal("no region returned")
	}
	if regionCountry(got) == "usa" {
		t.Fatalf("returned %q - expected a different country than usa", got)
	}
}

// TestAdaptiveRateTrackerMovesRate proves the adaptive per-group rate reacts
// to the observed success rate and never leaves the configured band.
func TestAdaptiveRateTrackerMovesRate(t *testing.T) {
	lim := NewIPLimiterGrouped(60, 2, 5)
	tr := NewGroupRateTracker(15, 60, 0.5)
	tr.Attach(lim)

	before := tr.GetRates()[0]
	for i := 0; i < 50; i++ {
		tr.Update(0, true)
	}
	if tr.GetRates()[0] < before {
		t.Fatalf("rate dropped on a success streak: %v -> %v", before, tr.GetRates()[0])
	}

	after := tr.GetRates()[0]
	for i := 0; i < 50; i++ {
		tr.Update(0, false)
	}
	if tr.GetRates()[0] > after {
		t.Fatalf("rate rose on a failure streak: %v -> %v", after, tr.GetRates()[0])
	}

	for i := 0; i < 5; i++ {
		r := tr.GetRates()[i]
		if r < 15 || r > 60 {
			t.Fatalf("group %d rate %v outside [15,60]", i, r)
		}
	}
}

// TestSourceHealthTracker proves the source health bookkeeping works.
func TestSourceHealthTracker(t *testing.T) {
	h := NewSourceHealthTracker()
	for i := 0; i < 20; i++ {
		h.RecordCheck("s1", true, 100*time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		h.RecordCheck("s2", false, 100*time.Millisecond)
	}
	if best := h.GetBestSource(); best != "s1" {
		t.Fatalf("best source = %q, want s1", best)
	}
	checks, rate, healthy := h.GetSourceStats("s1")
	if checks != 20 || rate != 1.0 || !healthy {
		t.Fatalf("s1 stats = %d/%v/%v", checks, rate, healthy)
	}
	_, _, healthy2 := h.GetSourceStats("s2")
	if healthy2 {
		t.Fatal("a 0% success source is reported as healthy")
	}
}

// TestDedupLinesKeepsFirstOccurrence proves duplicate "user:pass" rows are
// removed and the FIRST one is kept.
func TestDedupLinesKeepsFirstOccurrence(t *testing.T) {
	in := []string{
		"alpha:pass1",
		"beta:pass2",
		"alpha:pass1", // exact duplicate
		"ALPHA:PASS1", // same row, different case
		"alpha:pass9", // SAME user, different password -> NOT a duplicate
	}
	out, dupes := DedupLines(in)
	if dupes != 2 {
		t.Fatalf("dupes = %d, want 2", dupes)
	}
	if len(out) != 3 {
		t.Fatalf("kept %d rows, want 3: %v", len(out), out)
	}
	if out[0] != "alpha:pass1" {
		t.Fatalf("first row = %q, want alpha:pass1", out[0])
	}
	if out[2] != "alpha:pass9" {
		t.Fatalf("third row = %q, want alpha:pass9 (different password)", out[2])
	}
}

// TestAdaptiveGovernorResize proves that a run-wide governor publishes its
// latest target and reason when it changes.
func TestAdaptiveGovernorResize(t *testing.T) {
	g := NewAdaptiveGovernor(16, 8, 16)
	g.moveGap = 0
	for i := 0; i < 10; i++ {
		g.Report(false)
	}
	if got := g.Workers(); got != 8 {
		t.Fatalf("workers after error window = %d, want 8", got)
	}
	select {
	case target := <-g.Changed():
		if target.Workers != 8 || target.Reason == "" {
			t.Fatalf("resize target = %+v, want 8 workers with a reason", target)
		}
	case <-time.After(time.Second):
		t.Fatal("no resize notification")
	}
}

// TestWorkerPoolResizes proves that the pool can shrink and grow at runtime
// while every accepted job completes exactly once.
func TestWorkerPoolResizes(t *testing.T) {
	g := NewAdaptiveGovernor(8, 2, 16)
	g.moveGap = 0
	var next atomic.Int64
	var completed atomic.Int64
	seen := make([]atomic.Int32, 200)
	firstBatch := make(chan struct{})
	secondBatch := make(chan struct{})
	pool := NewResizablePool(16, g, func(ctx context.Context) (PoolTask, bool) {
		i := int(next.Add(1) - 1)
		if i >= len(seen) {
			return nil, false
		}
		return func(ctx context.Context) AccountOutcome {
			if seen[i].Add(1) != 1 {
				t.Errorf("job %d ran more than once", i)
			}
			completed.Add(1)
			if i < 8 {
				select {
				case <-firstBatch:
				case <-ctx.Done():
					return OutcomeNone
				}
			} else if i < 16 {
				select {
				case <-secondBatch:
				case <-ctx.Done():
					return OutcomeNone
				}
			}
			return OutcomeClean
		}, true
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(done)
	}()

	// Ten failures make the governor halve 8 -> 4. The first eight jobs are
	// held at a barrier so the resize is observable deterministically.
	for i := 0; i < 10; i++ {
		g.Report(false)
	}
	close(firstBatch)
	waitPoolLive(t, pool, 4)

	// Ten clean outcomes make it grow 4 -> 8. Hold the next batch until the
	// growth has been applied.
	for i := 0; i < 10; i++ {
		g.Report(true)
	}
	waitPoolLive(t, pool, 8)
	close(secondBatch)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not stop")
	}
	if got := completed.Load(); got != int64(len(seen)) {
		t.Fatalf("completed jobs = %d, want %d", got, len(seen))
	}
	for i := range seen {
		if got := seen[i].Load(); got != 1 {
			t.Fatalf("job %d completed %d times, want 1", i, got)
		}
	}
}

// TestAccountOutcomeReportedExactlyOnce proves that the terminal account
// result is classified correctly and reported once by the pool.
func TestAccountOutcomeReportedExactlyOnce(t *testing.T) {
	cases := []struct {
		name string
		res  UserResult
		want AccountOutcome
	}{
		{"hit", UserResult{OK: true}, OutcomeClean},
		{"not found", UserResult{NotFound: true}, OutcomeClean},
		{"suspended", UserResult{Banned: true}, OutcomeClean},
		{"blocked", UserResult{Error: "blocked"}, OutcomeBlocked},
		{"throttled", UserResult{Error: "rate_limited"}, OutcomeBlocked},
		{"failed", UserResult{Error: "request_failed"}, OutcomeFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountOutcome(tc.res); got != tc.want {
				t.Fatalf("outcome = %v, want %v", got, tc.want)
			}
		})
	}

	g := NewAdaptiveGovernor(4, 1, 4)
	var reports atomic.Int64
	var next atomic.Int64
	pool := NewResizablePool(4, g, func(ctx context.Context) (PoolTask, bool) {
		i := int(next.Add(1) - 1)
		if i >= len(cases) {
			return nil, false
		}
		return func(ctx context.Context) AccountOutcome {
			return accountOutcome(cases[i].res)
		}, true
	}, func(outcome AccountOutcome) {
		reports.Add(1)
	})
	pool.Run(context.Background())
	if got := reports.Load(); got != int64(len(cases)) {
		t.Fatalf("outcome reports = %d, want %d", got, len(cases))
	}
}

func waitPoolLive(t *testing.T, pool *ResizablePool, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pool.Snapshot().Live == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	got := pool.Snapshot()
	t.Fatalf("pool live workers = %d, want %d (target=%d busy=%d)", got.Live, want, got.Target, got.Busy)
}

// TestPlanThreads proves the thread count is validated against the lanes and
// the session count, so a wrong number can never waste resources.
func TestPlanThreads(t *testing.T) {
	cases := []struct {
		threads, lanes, sessions, want int
	}{
		{50, 20, 100, 20},       // more threads than lanes -> capped at lanes
		{500, 100, 454, 100},    // more threads than lanes -> capped
		{10, 0, 0, 10},          // nothing known -> 1
		{0, 30, 100, 30},        // 0 = auto -> lane count
		{8, 100, 2, 8},          // 8 <= 2*4 -> kept
		{100, 100, 2, 8},        // 100 > 2*4 -> capped at 8
		{9999, 9999, 9999, 512}, // hard ceiling
		{500, 500, 5561, 500},   // 500 lanes on 5561 sessions -> 500 threads
	}
	for _, c := range cases {
		got := planThreads(c.threads, c.lanes, c.sessions)
		if got != c.want {
			t.Errorf("planThreads(%d,%d,%d) = %d, want %d", c.threads, c.lanes, c.sessions, got, c.want)
		}
	}
}

// TestParseSessionLineFormats proves every session-file shape from the
// Python original (accounts.py) is understood, especially the BARE JWT value
// that the plain cookie-header parser cannot read.
func TestParseSessionLineFormats(t *testing.T) {
	// 1. reddit_session=abc
	cs := ParseSessionLine("reddit_session=abc")
	if len(cs) != 1 || cs[0].Name != "reddit_session" || cs[0].Value != "abc" {
		t.Fatalf("reddit_session=abc -> %+v", cs)
	}

	// 2. bare JWT value (no "=" at all) - the case the Go version missed
	cs = ParseSessionLine("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig")
	if len(cs) != 1 || cs[0].Name != "reddit_session" {
		t.Fatalf("bare JWT -> %+v", cs)
	}
	if cs[0].Value != "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig" {
		t.Fatalf("bare JWT value = %q", cs[0].Value)
	}

	// 3. "value; token_v2=def" - the first field is reddit_session
	cs = ParseSessionLine("abc123; token_v2=def456")
	if len(cs) < 2 {
		t.Fatalf("value; token_v2=def -> %+v", cs)
	}
	if cs[0].Name != "reddit_session" || cs[0].Value != "abc123" {
		t.Fatalf("first cookie = %+v", cs[0])
	}

	// 4. "Cookie: reddit_session=abc; token_v2=def" - the header prefix
	cs = ParseSessionLine("Cookie: reddit_session=abc; token_v2=def")
	if len(cs) < 2 {
		t.Fatalf("Cookie: header -> %+v", cs)
	}
	for _, c := range cs {
		if c.Name == "Cookie" {
			t.Fatal("the 'Cookie:' prefix leaked into a cookie name")
		}
	}
}

// TestLoadCookiesBareJWT proves a session file holding a bare reddit_session
// value loads with cookies instead of silently loading nothing.
func TestLoadCookiesBareJWT(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.txt")
	if err := os.WriteFile(path, []byte("eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cs, err := LoadCookies(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("loaded %d cookies, want 1", len(cs))
	}
	if cs[0].Name != "reddit_session" || cs[0].Value == "" {
		t.Fatalf("cookie = %+v", cs[0])
	}
}

// TestQueueWaitDoesNotEatRequestTimeout is a REGRESSION test.
//
// The queue wait (how many workers share one per-IP bucket) and the HTTP
// request used to share ONE context deadline. With many workers the queue
// wait consumed the request's time and the request died with "context
// deadline exceeded" even though the network was fine - the account was
// then only saved by the HTML fallback.
//
// The request must get its OWN full timeout, measured from the moment the
// token was actually acquired.
func TestQueueWaitDoesNotEatRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := newClient("", 5*time.Second, 0)
	if err != nil {
		t.Skipf("no client: %v", err)
	}
	// a bucket that hands out ONE token every 3 seconds -> the queue wait
	// is ~3s, far longer than the 400ms the server needs
	c.Gate = &Gate{
		IPBucket: NewTokenBucket(20, 1),
		Bucket:   NewTokenBucket(6000, 1),
		Breaker:  NewCircuitBreaker(5, time.Minute),
	}
	ctx := context.Background()
	// take the first token so the next wait is a full 3 seconds
	if err := c.Gate.IPBucket.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	code, body, err := c.GetResilient(ctx, srv.URL, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the request failed after the queue wait: %v (elapsed %s)", err, elapsed)
	}
	if code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("code=%d body=%q", code, body)
	}
	// the queue wait really happened
	if elapsed < 2*time.Second {
		t.Fatalf("the queue wait did not happen (elapsed %s) - the test proves nothing", elapsed)
	}
	// and the request still got its full timeout: the server slept 400ms
	// AFTER the wait, so the total must be clearly more than the wait
	if elapsed < 3*time.Second {
		t.Fatalf("elapsed %s - the request did not get its own time after the wait", elapsed)
	}
}

// TestPerIPBudgetIsNotMultipliedByGroups is a REGRESSION test.
//
// The limiter used to keep one bucket per ROTATING GROUP with a global
// group counter, so a single exit IP with 5 groups received 5 x the
// configured per-IP rate - the fastest way to get the IP blocked.
//
// The budget must be per EXIT IP: every group of that IP shares ONE bucket.
func TestPerIPBudgetIsNotMultipliedByGroups(t *testing.T) {
	lim := NewIPLimiterGrouped(60, 2, 5)

	// all 5 groups of the SAME exit IP must share ONE bucket
	b0 := lim.Bucket("1.2.3.4", 0)
	for g := 1; g < 5; g++ {
		if lim.Bucket("1.2.3.4", g) != b0 {
			t.Fatalf("group %d of the same IP got its own bucket - the budget is multiplied", g)
		}
	}
	// a DIFFERENT exit IP must have its own bucket
	if lim.Bucket("5.6.7.8", 0) == b0 {
		t.Fatal("two different IPs share one bucket")
	}
	// the rate of the bucket is the configured per-IP rate
	if got := b0.Rate(); got != 60 {
		t.Fatalf("bucket rate = %v, want 60", got)
	}
}

// TestPerIPBudgetPacing proves the shared bucket really paces the TOTAL
// traffic of an IP: 5 groups pulling together must not get 5x the tokens.
func TestPerIPBudgetPacing(t *testing.T) {
	// 60/min = 1 per second, burst 1
	lim := NewIPLimiterGrouped(60, 1, 5)
	b := lim.Bucket("9.9.9.9", 0)

	start := time.Now()
	// 6 tokens at 1/s -> the first is instant, the rest are paced
	for i := 0; i < 6; i++ {
		if err := b.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	// 5 paced tokens at 1/s -> at least ~4s (the 6th is the 5th paced one)
	if elapsed < 4*time.Second {
		t.Fatalf("6 tokens took %s - the bucket is not pacing", elapsed)
	}
}

// TestMetricsErrorReasons proves the error-reason breakdown works: the run
// report used to show every non-block failure as one "other" bucket, which
// made the real cause invisible.
func TestMetricsErrorReasons(t *testing.T) {
	m := NewMetrics()
	m.RecordReason("request_failed")
	m.RecordReason("request_failed")
	m.RecordReason("timeout")
	m.RecordReason("blocked")
	m.RecordReason("rate_limited")
	m.RecordReason("login_required")
	m.RecordReason("challenge")
	m.RecordReason("browser_budget_exhausted")
	m.RecordReason("something_odd")

	r := m.ErrorReasons()
	want := map[string]int64{
		"request_failed":      2,
		"timeout":             1,
		"blocked":             1,
		"rate_limited":        1,
		"login_required":      1,
		"challenge":           1,
		"browser_unavailable": 1,
		"other":               1,
	}
	for i, name := range ErrorReasonNames {
		if r[i] != want[name] {
			t.Errorf("%s = %d, want %d", name, r[i], want[name])
		}
	}
	// the report must actually show the breakdown
	s := m.Snapshot()
	out := s.Report()
	for _, name := range []string{"request_failed", "timeout", "other"} {
		if !strings.Contains(out, name) {
			t.Errorf("the report does not mention %q:\n%s", name, out)
		}
	}
}

// TestMetricsQueueDepth proves the queue depth counter tracks the workers
// that wait for a rate-limit token - the clearest sign that a run is rate
// limited rather than network limited.
func TestMetricsQueueDepth(t *testing.T) {
	m := NewMetrics()
	if m.QueueDepth() != 0 {
		t.Fatalf("queue depth = %d, want 0", m.QueueDepth())
	}
	m.QueueEnter()
	m.QueueEnter()
	if m.QueueDepth() != 2 {
		t.Fatalf("queue depth = %d, want 2", m.QueueDepth())
	}
	m.QueueLeave(50 * time.Millisecond)
	if m.QueueDepth() != 1 {
		t.Fatalf("queue depth = %d, want 1", m.QueueDepth())
	}
	m.QueueLeave(0)
	if m.QueueDepth() != 0 {
		t.Fatalf("queue depth = %d, want 0", m.QueueDepth())
	}
}

// TestLivePanelShowsRateState proves the live panel carries the rate-limit
// rows. Without them a run that is quietly rate limited looks exactly like a
// run that is quietly slow.
func TestLivePanelShowsRateState(t *testing.T) {
	l := NewLiveStatus(100)
	l.SetProvider(func() []string {
		return []string{"Blocked=7", "Rate=1200/min", "AIMD=0.85x", "Queue=42"}
	})
	p := l.panel()
	for _, want := range []string{"Blocked", "Rate", "AIMD", "Queue", "7", "1200/min", "0.85x", "42"} {
		if !strings.Contains(p, want) {
			t.Errorf("the panel is missing %q:\n%s", want, p)
		}
	}
}

// TestLadderVerdictAndReason proves the ladder trace helpers produce a
// readable verdict and the right reason for each decision.
func TestLadderVerdictAndReason(t *testing.T) {
	if got := ladderVerdict(UserResult{OK: true}); got != "hit" {
		t.Errorf("verdict(hit) = %q", got)
	}
	if got := ladderVerdict(UserResult{NotFound: true}); got != "not_found" {
		t.Errorf("verdict(notfound) = %q", got)
	}
	if got := ladderVerdict(UserResult{Banned: true, Deleted: true}); got != "deleted" {
		t.Errorf("verdict(deleted) = %q", got)
	}
	if got := ladderVerdict(UserResult{Error: "blocked"}); got != "blocked" {
		t.Errorf("verdict(blocked) = %q", got)
	}
	// a JSON 404 must say WHY it escalates
	if got := ladderReason(UserResult{NotFound: true}, true); got != "json_404_needs_profile_page" {
		t.Errorf("reason(json 404) = %q", got)
	}
	if got := ladderReason(UserResult{NotFound: true}, false); got != "profile_page_confirms_missing" {
		t.Errorf("reason(html 404) = %q", got)
	}
}

// TestMetricsCollector proves the run-wide collector counts the block rate
// and the latency percentiles correctly.
//
// The block rate is THE number that decides whether a run poisoned the exit
// IP: a run can be fast and still be 15% blocked.
func TestMetricsCollector(t *testing.T) {
	m := NewMetrics()
	m.Record(200, 100*time.Millisecond, false) // ok
	m.Record(404, 120*time.Millisecond, false) // not found
	m.Record(403, 200*time.Millisecond, false) // blocked
	m.Record(429, 300*time.Millisecond, false) // blocked
	m.Record(200, 50*time.Millisecond, true)   // challenge (solvable, not a block)
	m.RecordError()                            // transport failure
	m.RecordRetry()

	s := m.Snapshot()
	if s.Requests != 5 {
		t.Fatalf("requests = %d, want 5", s.Requests)
	}
	if s.OK != 1 || s.NotFound != 1 || s.Blocked != 2 || s.Challenge != 1 {
		t.Fatalf("counters wrong: %+v", s)
	}
	if s.Failed != 1 || s.Retries != 1 {
		t.Fatalf("failed/retries wrong: %+v", s)
	}
	// the block rate is measured over the ANSWERED requests (a 404 is a
	// clean answer, not a failure): 2 blocked of 4 answered = 50%
	if s.BlockPct < 49 || s.BlockPct > 51 {
		t.Fatalf("block pct = %.2f, want 50", s.BlockPct)
	}
	// the latency histogram: 50ms buckets, upper edge
	if s.P50MS < 50 || s.P50MS > 150 {
		t.Fatalf("p50 = %.0fms", s.P50MS)
	}
	if s.P95MS < s.P50MS || s.P99MS < s.P95MS {
		t.Fatalf("percentiles not monotonic: p50 %.0f p95 %.0f p99 %.0f", s.P50MS, s.P95MS, s.P99MS)
	}
	if s.MaxMS < 300 {
		t.Fatalf("max = %.0fms, want >= 300", s.MaxMS)
	}
	if s.Goroutine <= 0 {
		t.Fatalf("goroutines = %d", s.Goroutine)
	}
}

// TestMetricsNilSafe proves a nil collector never panics - the one-off CLI
// commands (fetch/probe) have no collector at all.
func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.Record(200, time.Second, false)
	m.RecordError()
	m.RecordRetry()
	s := m.Snapshot()
	if s.Requests != 0 {
		t.Fatalf("nil collector recorded %d requests", s.Requests)
	}
}

// TestLadderResolvesDeletedVsNotFound is a REGRESSION test.
//
// Reddit answers JSON 404 for BOTH a missing user and a DELETED account.
// Only the profile page can tell them apart. The ladder must therefore
// escalate a JSON NOT_FOUND to the HTML source, so a deleted account ends up
// in banned.txt (SUSPENDED) instead of not_found.txt (NOT_FOUND).
func TestLadderResolvesDeletedVsNotFound(t *testing.T) {
	// JSON 404 -> escalate to the HTML source
	if got := ladderActionFor(UserResult{NotFound: true}, true); got != ladderEscalate {
		t.Fatalf("JSON notfound -> %v, want escalate", got)
	}
	// HTML NOT_FOUND -> the account really does not exist -> stop
	if got := ladderActionFor(UserResult{NotFound: true}, false); got != ladderStop {
		t.Fatalf("HTML notfound -> %v, want stop", got)
	}
	// the HTML page of a deleted account -> SUSPENDED/DELETED -> stop
	if got := ladderActionFor(UserResult{Banned: true, Deleted: true}, false); got != ladderStop {
		t.Fatalf("deleted -> %v, want stop", got)
	}
	// a hit stops, a session-less error escalates, a block retries
	if got := ladderActionFor(UserResult{OK: true}, true); got != ladderStop {
		t.Fatalf("hit -> %v, want stop", got)
	}
	if got := ladderActionFor(UserResult{Error: "login_required"}, true); got != ladderEscalate {
		t.Fatalf("login_required -> %v, want escalate", got)
	}
	if got := ladderActionFor(UserResult{Error: "rate_limited"}, true); got != ladderRetry {
		t.Fatalf("rate_limited -> %v, want retry", got)
	}
}

// TestMultiEngineOpensSharedOutput is a REGRESSION test.
//
// The multi-engine path never calls MultiChecker.Run (that is where the
// normal run opens the output writer), so the writer stayed CLOSED and every
// result was silently dropped - the run reported success but wrote no files.
// The runner must open ONE shared writer for all engines.
func TestMultiEngineOpensSharedOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	ch := NewMultiChecker(MultiConfig{Output: out, Workers: 1})
	r := &MultiEngineRunner{engines: []*Engine{{Name: "e1", checker: ch}}}

	w, err := r.openSharedOutput()
	if err != nil {
		t.Fatalf("openSharedOutput: %v", err)
	}
	if w == nil {
		t.Fatal("no shared output writer was opened")
	}
	defer w.Close()

	// the writer must really write (an unopened writer drops everything)
	w.Append("not_found.txt", "someuser | NOT_FOUND")
	w.Flush()
	data, err := os.ReadFile(filepath.Join(out, "not_found.txt"))
	if err != nil {
		t.Fatalf("output file not created: %v", err)
	}
	if !strings.Contains(string(data), "someuser | NOT_FOUND") {
		t.Fatalf("output file = %q", string(data))
	}
}

// TestLoadSessionValuesNetscapeIsOneSession is a REGRESSION test.
//
// A Netscape session file has one TAB separated line per cookie. It is ONE
// session, not many. Splitting it per line once turned 467 session files
// into 9288 bogus "sessions" (every cookie line became its own session).
func TestLoadSessionValuesNetscapeIsOneSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.txt")
	ns := "# Netscape HTTP Cookie File\n" +
		"Valid Cookie / Every day!\n" +
		"\n" +
		".reddit.com\tTRUE\t/\tTRUE\t1799806940\tcsv\t2\n" +
		".reddit.com\tTRUE\t/\tTRUE\t1799806940\tloid\tabc123\n" +
		".reddit.com\tTRUE\t/\tTRUE\t1799806940\treddit_session\teyJhbGciOiJSUzI1NiJ9\n"
	if err := os.WriteFile(path, []byte(ns), 0644); err != nil {
		t.Fatal(err)
	}
	sets, err := LoadSessionValues(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 {
		t.Fatalf("loaded %d session sets, want 1 (a Netscape file is ONE session)", len(sets))
	}
	// the whole cookie set must be there
	names := map[string]string{}
	for _, c := range sets[0] {
		names[c.Name] = c.Value
	}
	if names["reddit_session"] != "eyJhbGciOiJSUzI1NiJ9" {
		t.Fatalf("reddit_session = %q", names["reddit_session"])
	}
	if names["loid"] != "abc123" || names["csv"] != "2" {
		t.Fatalf("cookies = %v", names)
	}
}

// TestLoadSessionPoolCountsFilesAsSessions proves the pool creates ONE
// session per session FILE (the real sassion/ folder has 467 files).
func TestLoadSessionPoolCountsFilesAsSessions(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		ns := "Valid Cookie / Every day!\n\n" +
			".reddit.com\tTRUE\t/\tTRUE\t1799806940\tcsv\t2\n" +
			fmt.Sprintf(".reddit.com\tTRUE\t/\tTRUE\t1799806940\treddit_session\ttoken%d\n", i)
		name := filepath.Join(dir, fmt.Sprintf("[0 friends] [1 karma] [user%d].txt", i))
		if err := os.WriteFile(name, []byte(ns), 0644); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := LoadSessionPool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 3 {
		t.Fatalf("pool has %d sessions, want 3 (one per file)", len(sessions))
	}
	for _, s := range sessions {
		if len(s.Cookies) != 3 { // csv + reddit_session + over18
			t.Fatalf("session %s has %d cookies, want 3", s.Name, len(s.Cookies))
		}
	}
}

// TestLoadSessionValues proves a sessions.txt (one bare value per line)
// loads as one cookie set per line, with duplicates removed.
func TestLoadSessionValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.txt")
	body := "value-one\nvalue-two\nvalue-one\n\n# comment\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	sets, err := LoadSessionValues(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 2 {
		t.Fatalf("loaded %d session sets, want 2", len(sets))
	}
	if sets[0][0].Value != "value-one" || sets[1][0].Value != "value-two" {
		t.Fatalf("sets = %+v", sets)
	}
}

func TestPrepareWorkResumesAndDedups(t *testing.T) {
	dir := t.TempDir()
	// an earlier run saved "alpha" and "beta"
	if err := os.WriteFile(filepath.Join(dir, "not_found.txt"), []byte("alpha | NOT_FOUND\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "banned.txt"), []byte("beta | SUSPENDED\r\n"), 0644); err != nil {
		t.Fatal(err)
	}

	users := []string{
		"alpha:pass",
		"beta:pass",
		"gamma:pass",
		"gamma:pass", // duplicate row
	}
	todo, skipped, dupes := PrepareWork(users, dir, false)
	if dupes != 1 {
		t.Fatalf("dupes = %d, want 1", dupes)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2 (alpha + beta)", skipped)
	}
	if len(todo) != 1 || todo[0] != "gamma:pass" {
		t.Fatalf("todo = %v, want [gamma:pass]", todo)
	}
}

// ---------------------------------------------------------------------------
// T-10 — multi-IP budget independence and the total budget
// ---------------------------------------------------------------------------

// multiIPProxies is the 5-proxy topology of the real run.bat.
var multiIPProxies = []string{
	"http://10.0.0.1:8080",
	"http://10.0.0.2:8080",
	"http://10.0.0.3:8080",
	"http://10.0.0.4:8080",
	"http://10.0.0.5:8080",
}

// TestMultiIPBudgetIsIndependent: with 5 proxies every exit IP gets its OWN
// bucket at the FULL configured rate, and the rotating groups never create a
// second budget for the same IP (the bug that gave one IP 5x its budget).
func TestMultiIPBudgetIsIndependent(t *testing.T) {
	const rate = 600.0
	const groups = 5
	lim := NewIPLimiterGrouped(rate, 2, groups)

	perIP := make(map[string]*TokenBucket, len(multiIPProxies))
	for _, proxy := range multiIPProxies {
		var first *TokenBucket
		for g := 0; g < groups; g++ {
			b := lim.Bucket(proxy, g)
			if b == nil {
				t.Fatalf("%s group %d: nil bucket", proxy, g)
			}
			if first == nil {
				first = b
				continue
			}
			if b != first {
				t.Fatalf("%s: group %d got a DIFFERENT bucket - the per-IP budget is multiplied by the groups", proxy, g)
			}
		}
		if got := first.Rate(); got != rate {
			t.Fatalf("%s: rate = %.1f, want %.1f (the full per-IP budget)", proxy, got, rate)
		}
		perIP[proxy] = first
	}

	if got := lim.Len(); got != len(multiIPProxies) {
		t.Fatalf("limiter holds %d buckets, want %d (one per exit IP)", got, len(multiIPProxies))
	}
	seen := map[*TokenBucket]string{}
	for proxy, b := range perIP {
		if other, ok := seen[b]; ok {
			t.Fatalf("%s and %s share one bucket", proxy, other)
		}
		seen[b] = proxy
	}

	// A drained IP must not take tokens away from another IP.
	b1, b2 := perIP[multiIPProxies[0]], perIP[multiIPProxies[1]]
	for i := 0; i < 2; i++ { // burst = 2
		if !b1.TryWait() {
			t.Fatalf("burst token %d not available on %s", i, multiIPProxies[0])
		}
	}
	if b1.TryWait() {
		t.Fatalf("%s served a third instant request - the burst is not bounded", multiIPProxies[0])
	}
	if !b2.TryWait() {
		t.Fatalf("%s was starved by the drained IP - the budgets are not independent", multiIPProxies[1])
	}
}

// TestMultiIPTotalBudget: the sum over the exit IPs is N x the per-IP rate,
// and an AIMD change moves every IP by the same factor.
func TestMultiIPTotalBudget(t *testing.T) {
	const rate = 1200.0
	const burst = 2
	lim := NewIPLimiterGrouped(rate, burst, 5)
	for _, proxy := range multiIPProxies {
		lim.Bucket(proxy, 0)
	}

	ips := lim.IPs()
	if len(ips) != len(multiIPProxies) {
		t.Fatalf("IPs() = %v, want %d entries", ips, len(multiIPProxies))
	}
	total := 0.0
	for _, ip := range ips {
		total += lim.Bucket(ip, 0).Rate()
	}
	if want := float64(len(multiIPProxies)) * rate; total != want {
		t.Fatalf("total budget = %.0f/min, want %.0f/min (%d x %.0f)", total, want, len(multiIPProxies), rate)
	}

	// AIMD: every IP follows the same multiplier, the total stays N x rate.
	lim.SetRate(rate * 0.85)
	total = 0
	for _, ip := range ips {
		if got := lim.Bucket(ip, 0).Rate(); got != rate*0.85 {
			t.Fatalf("%s rate = %.1f after SetRate, want %.1f", ip, got, rate*0.85)
		}
		total += lim.Bucket(ip, 0).Rate()
	}
	if want := float64(len(multiIPProxies)) * rate * 0.85; total != want {
		t.Fatalf("total after the AIMD shrink = %.0f, want %.0f", total, want)
	}
	if got := lim.Rate(); got != rate*0.85 {
		t.Fatalf("limiter Rate() = %.1f, want %.1f", got, rate*0.85)
	}
}
