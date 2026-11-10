// Package main — worker pool + check runner (Task 4).
// Parity with check_users.Checker: retry policy, blocked-streak abort,
// preflight, resume, per-result lines and the final RESULTS summary.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CheckConfig mirrors the relevant check_users args.
type CheckConfig struct {
	Input         string
	Output        string
	Workers       int
	Proxies       []string
	Cookies       []Cookie
	Token         string         // static OAuth bearer ("" = cookie/anonymous)
	TokenProvider *TokenProvider // app-only OAuth (client id/secret)
	HTMLMode      bool
	Delay         time.Duration
	Rate          float64 // per-minute bucket (0 = unlimited pacing)
	Burst         int     // burst size of the IP bucket (keep small: 2)
	Timeout       time.Duration
	Retries       int
	MaxBlocked    int
	NoPreflight   bool
	NoResume      bool
	VPN           *VPNController // shared VPN rotation controller (nil = none)
	BrowserPages  int            // parallel Chrome tabs for the fallback lane
	BrowserBudget int            // max browser fetches per run (0 = unlimited)
	InputTotal    int64          // accounts in the input file (before filtering)
	InputRejected int64          // input lines rejected by the credential filter
}

// Stats mirrors check_users.stats.
type Stats struct {
	Total, Hit, NotFound, Banned, Error, Blocked, Skipped, Processed int64
	InputTotal                                                       int64    // accounts found in the input file
	Deleted                                                          int64    // suspended + deleted (invalid accounts)
	AgeGate                                                          int64    // login_required (NSFW age gate)
	Challenge                                                        int64    // Reddit's jsc challenge did not clear
	Buckets                                                          [8]int64 // hits per karma bucket (same order as KarmaBuckets)
}

// breakerCooldown is how long an IP stays parked after a real block burst.
// It used to be 5 minutes - far too punishing: a single bad minute froze
// the whole run. With VPN rotation armed the rotation is the real remedy,
// so the cooldown is only a short safety net.
const breakerCooldown = 45 * time.Second

// maxPreflightRotations caps how many exit IPs the startup probe may burn
// before giving up on the sample (each rotation takes a few seconds).
const maxPreflightRotations = 3

// Checker runs one batch of usernames.
type Checker struct {
	cfg           CheckConfig
	out           *OutputWriter
	stats         Stats
	start         time.Time
	abort         atomic.Bool
	announced     atomic.Bool
	blockedStreak int64
	governor      *AdaptiveGovernor
	clients       map[string]*Client // per-proxy (or "" = direct)
	clientsMu     sync.Mutex
	ipLim         *IPLimiter       // shared per-IP budget (every mode)
	anon          *Client          // anonymous client (no cookies) for the ladder
	sources       []Source         // the source ladder
	browser       *BrowserFallback // last rung of the ladder
	live          *LiveStatus      // in-place status panel
}

// Run executes the whole batch. Returns false if aborted by auth failure.
func (ch *Checker) Run(ctx context.Context, users []string) bool {
	if err := os.MkdirAll(ch.cfg.Output, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "output dir: %v\n", err)
		return false
	}
	if err := ch.out.Open(); err != nil {
		fmt.Fprintf(os.Stderr, "output: %v\n", err)
		return false
	}
	defer ch.out.Close()

	// resume
	done := map[string]bool{}
	if !ch.cfg.NoResume {
		done = ReadDoneUsers(ch.cfg.Output)
	}
	todo := make([]string, 0, len(users))
	for _, raw := range users {
		key := strings.ToLower(NormalizeUsername(raw))
		if key == "" {
			continue
		}
		if done[key] {
			atomic.AddInt64(&ch.stats.Skipped, 1)
			continue
		}
		done[key] = true
		todo = append(todo, raw)
	}
	atomic.StoreInt64(&ch.stats.Total, int64(len(todo)))
	if ch.cfg.InputTotal > 0 {
		atomic.StoreInt64(&ch.stats.InputTotal, ch.cfg.InputTotal)
	} else {
		atomic.StoreInt64(&ch.stats.InputTotal, int64(len(users)))
	}
	if len(todo) == 0 {
		fmt.Println("Nothing to do — all usernames already checked.")
		if ch.stats.Skipped > 0 {
			fmt.Printf("Skipped %d already-saved usernames.\n", ch.stats.Skipped)
		}
		return true
	}

	mode := "anonymous"
	switch {
	case ch.cfg.HTMLMode:
		mode = "HTML scrape (no login)"
	case ch.cfg.Token != "":
		mode = "OAuth token"
	case len(ch.cfg.Cookies) > 0:
		mode = "cookies"
	}
	rate := ""
	if ch.cfg.Rate > 0 {
		rate = fmt.Sprintf(" | %.0f/min", ch.cfg.Rate)
	}
	fmt.Printf("Mode: %s | Users: %d | Workers: %d | Proxies: %d%s\n\n",
		mode, len(todo), ch.cfg.Workers, len(ch.cfg.Proxies), rate)

	if err := ch.buildSources(); err != nil {
		fmt.Fprintf(os.Stderr, "sources: %v\n", err)
		return false
	}
	// VPN rotation: arm the shared controller and install the reset hook
	// (a fresh exit IP clears the source breakers and the blocked streak).
	if ch.cfg.VPN != nil && ch.cfg.VPN.Enabled() {
		ch.cfg.VPN.Arm()
		ch.cfg.VPN.SetOnRotated(func(rec RotationRecord) { ch.resetAfterRotation(rec) })
	}
	if !ch.cfg.NoPreflight && !ch.preflight(ctx, todo[:min(3, len(todo))]...) {
		return false
	}

	ch.start = time.Now()
	// live status panel (the Python script's rich.live panel)
	live := NewLiveStatus(int64(len(todo)))
	ch.live = live
	SetLiveActive(live.Enabled())
	defer func() {
		live.Close()
		SetLiveActive(false)
	}()
	jobs := make(chan int, 64)
	var wg sync.WaitGroup
	for w := 0; w < ch.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if ch.abort.Load() {
					continue
				}
				// park while the VPN is rotating (no crash, no lost progress)
				if ch.cfg.VPN != nil && !ch.cfg.VPN.Wait(ctx) {
					return
				}
				ch.checkOne(ctx, i, todo[i])
				if ch.cfg.Delay > 0 {
					time.Sleep(ch.cfg.Delay)
				}
			}
		}()
	}
	for i := range todo {
		if ch.abort.Load() {
			break
		}
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	ch.summary()
	return !ch.abort.Load()
}

// fetch — parity with check_users._fetch: HTML mode = direct HTML scrape,
// JSON mode = JSON first with internal HTML fallback on block.
func (ch *Checker) fetch(ctx context.Context, c *Client, user, token string) UserResult {
	// Anti-bot jitter: small random delays between requests per client
	// prevent burst detection that Reddit's pattern scorer flags as bot.
	// This only applies when a Gate (multi-session mode) is active, since
	// the single-user command has no other clients to coordinate with.
	if ch.cfg.Rate > 0 && c.Gate != nil {
		j := time.Duration((rand.Int63n(7) - 3) * int64(time.Millisecond)) // -3ms..+3ms
		select {
		case <-ctx.Done():
			return errResult("context_expired", ctx.Err().Error())
		case <-time.After(j):
		}
	}
	if ch.cfg.HTMLMode {
		return c.FetchUserHTML(ctx, user)
	}
	return c.FetchUser(ctx, user, token, true)
}

// reportVPN feeds one fetch outcome into the VPN throttle detector.
func (ch *Checker) reportVPN(res UserResult) {
	if ch.cfg.VPN == nil {
		return
	}
	if res.Error == "blocked" {
		// A Reddit block page is definitive: this exit IP is blocked.
		// Rotate NOW instead of waiting for the 6-consecutive threshold.
		ch.cfg.VPN.ReportIPBlocked("blocked page")
		return
	}
	ch.cfg.VPN.Report(classifyResult(res) != ClassThrottled, "throttled")
}

// sleepCtx sleeps d or returns false when the context ends.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// preflight — parity with check_users._preflight, but resilient:
//   - a dead SESSION aborts (that is a real setup problem)
//   - a throttled sample is retried
//   - an age-gated / missing sample only warns (it is a per-user condition,
//     the source ladder handles it during the run)
func (ch *Checker) preflight(ctx context.Context, samples ...string) bool {
	c, err := ch.clientFor("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "preflight: %v\n", err)
		return false
	}
	if len(ch.cfg.Cookies) > 0 {
		state, account, karma, detail := c.SessionState(ctx, ch.cfg.Cookies)
		switch state {
		case SessionValid:
			fmt.Printf("Session OK — u/%s (karma %d)\n\n", account, karma)
		case SessionDead:
			fmt.Printf("Session check FAILED: %s\n", detail)
			fmt.Println("Your login cookies are expired or revoked. Run the login")
			fmt.Println("command to log in again, then run again — progress is saved.")
			return false
		default:
			fmt.Printf("Preflight warning: %s — continuing.\n\n", detail)
		}
	}
	token := ch.cfg.Token
	if ch.cfg.TokenProvider.Enabled() {
		if t, err := ch.cfg.TokenProvider.Get(); err == nil {
			token = t
		}
	}
	vpnArmed := ch.cfg.VPN != nil && ch.cfg.VPN.Enabled()
	preflightRotations := 0
	for _, raw := range samples {
		user := NormalizeUsername(raw)
		if user == "" {
			continue
		}
		// The sample fetch must not kill the run because of a TRANSIENT block:
		// a 403 block page is rate limiting, not a broken setup.
		var res UserResult
		for attempt := 0; attempt < 4; attempt++ {
			// a fresh shallow reader cannot see a silent in-flight fetch; show
			// that it is happening (a slow VPN exit can make one fetch take
			// tens of seconds and look dead)
			progressf("Preflight: fetching u/%s ...\n", user)
			res = ch.fetch(ctx, c, user, token)
			ch.reportVPN(res) // feed the rotation detector
			if res.OK || res.NotFound || res.Banned {
				return true
			}
			if classifyResult(res) != ClassThrottled {
				break
			}
			if attempt == 3 {
				break
			}
			// With VPN rotation armed a blocked startup IP is fixed by a
			// rotation in seconds - park while it happens and retry fast
			// instead of burning up to a minute per sample on sleeps.
			if vpnArmed {
				if ch.cfg.VPN.IsPaused() {
					progressf("Preflight: %s throttled (%s) - waiting for the VPN rotation ...\n", user, res.Error)
					if !ch.cfg.VPN.Wait(ctx) {
						return false
					}
					continue // fresh IP: retry right away
				}
				// A blocked sample on a FRESH exit IP is proof that this IP
				// is bad. Rotate immediately instead of waiting for the
				// 6-consecutive threshold (which preflight can never reach
				// fast enough - it would just stall the startup).
				if preflightRotations < maxPreflightRotations {
					preflightRotations++
					fmt.Printf("Preflight: %s is blocked (%s) - rotating the VPN IP now ...\n", user, res.Error)
					if ch.cfg.VPN.Trigger("preflight blocked") {
						if !ch.cfg.VPN.Wait(ctx) {
							return false
						}
						continue // retry the SAME sample on the fresh IP
					}
				}
				progressf("Preflight: %s throttled (%s) - retry %d/3 ...\n", user, res.Error, attempt+1)
				if !sleepCtx(ctx, time.Duration(attempt+1)*2*time.Second) {
					return false
				}
				continue
			}
			progressf("Preflight: %s throttled (%s) - retry %d/3 ...\n", user, res.Error, attempt+1)
			if !sleepCtx(ctx, time.Duration(attempt+1)*10*time.Second) {
				return false
			}
		}
		if res.OK || res.NotFound || res.Banned {
			return true
		}
		class := classifyResult(res)
		if class == ClassNeedsSession {
			fmt.Printf("Preflight: %s needs a session (age gate) - continuing.\n", user)
			fmt.Println("Use --sessions <folder> to check with your saved sessions.")
			return true
		}
		fmt.Printf("Preflight: %s did not resolve (%s) - trying the next sample ...\n", user, res.Error)
	}
	fmt.Println("Preflight: no sample could be resolved - continuing anyway.")
	return true
}

// clientFor returns (creating if needed) the uTLS client for a proxy line.
func (ch *Checker) clientFor(proxyLine string) (*Client, error) {
	ch.clientsMu.Lock()
	defer ch.clientsMu.Unlock()
	if c, ok := ch.clients[proxyLine]; ok {
		return c, nil
	}
	c, err := newClient(proxyLine, ch.cfg.Timeout, 0)
	if err != nil {
		return nil, err
	}
	// Shared per-IP budget for every lane on this IP (burst is kept small:
	// a burst is what makes Reddit hand out 403 block pages).
	if ch.cfg.Rate > 0 {
		c.Gate = &Gate{
			// per-session budget (the account's own ceiling) + the shared
			// per-IP budget on top of it.
			Bucket:   NewTokenBucket(ch.cfg.Rate, ch.cfg.Burst),
			IPBucket: ch.ipLim.Bucket(proxyLine),
			Breaker:  NewCircuitBreaker(ch.cfg.MaxBlocked, breakerCooldown),
			Retries:  ch.cfg.Retries,
			Backoff:  5 * time.Second,
		}
	}
	if len(ch.cfg.Cookies) > 0 {
		c.ApplyCookies(ch.cfg.Cookies)
	}
	ch.clients[proxyLine] = c
	return c, nil
}

// NewChecker builds a checker; the output dir must exist.
func NewChecker(cfg CheckConfig) *Checker {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	return &Checker{
		cfg:      cfg,
		out:      &OutputWriter{Dir: cfg.Output},
		governor: NewAdaptiveGovernor(cfg.Workers, 2, cfg.Workers*4),
		clients:  map[string]*Client{},
		ipLim:    NewIPLimiter(cfg.Rate, cfg.Burst),
	}
}

// buildSources creates the source ladder for the single-session mode:
// session JSON -> session HTML -> anonymous JSON -> anonymous HTML.
func (ch *Checker) buildSources() error {
	ch.sources = nil
	if len(ch.cfg.Cookies) > 0 {
		c, err := ch.clientFor("")
		if err != nil {
			return err
		}
		ch.sources = append(ch.sources,
			Source{Client: c, JSON: true, Label: "json+session"},
			Source{Client: c, JSON: false, Label: "html+session"},
		)
	}
	if ch.anon == nil {
		anon, err := newClient("", ch.cfg.Timeout, 0)
		if err != nil {
			return err
		}
		if ch.cfg.Rate > 0 {
			anon.Gate = &Gate{
				Bucket:   NewTokenBucket(ch.cfg.Rate, ch.cfg.Burst),
				IPBucket: ch.ipLim.Bucket(""),
				Breaker:  NewCircuitBreaker(ch.cfg.MaxBlocked, breakerCooldown),
				Retries:  ch.cfg.Retries,
				Backoff:  5 * time.Second,
			}
		}
		ch.anon = anon
	}
	ch.sources = append(ch.sources,
		Source{Client: ch.anon, JSON: true, Label: "json+anonymous"},
		Source{Client: ch.anon, JSON: false, Label: "html+anonymous"},
	)
	return nil
}

// browserFetch lazily starts the browser fallback lane.
func (ch *Checker) browserFetch(ctx context.Context, user string) UserResult {
	if ch.browser == nil {
		// a profile page load must not eat the whole request timeout
		bt := ch.cfg.Timeout
		if bt <= 0 || bt > 12*time.Second {
			bt = 12 * time.Second
		}
		b, err := NewBrowserFallback(ch.cfg.Output, true, bt,
			ch.cfg.BrowserPages, ch.cfg.BrowserBudget)
		if err != nil {
			return errResult("browser_unavailable", err.Error())
		}
		ch.browser = b
	}
	return ch.browser.Fetch(ctx, user)
}

// checkOne — parity with check_users.check_one (retry + output + stats).
func (ch *Checker) checkOne(ctx context.Context, index int, raw string) {
	if ch.abort.Load() {
		return
	}
	user := NormalizeUsername(raw)
	if user == "" {
		return
	}
	atomic.AddInt64(&ch.stats.Processed, 1)

	proxyLine := ""
	if len(ch.cfg.Proxies) > 0 {
		proxyLine = ch.cfg.Proxies[index%len(ch.cfg.Proxies)]
	}
	c, err := ch.clientFor(proxyLine)
	if err != nil {
		ch.finishError(user, index, "client_failed: "+err.Error(), "error")
		return
	}

	var res UserResult
	token := ch.cfg.Token
	if ch.cfg.TokenProvider.Enabled() {
		t, err := ch.cfg.TokenProvider.Get()
		if err != nil {
			ch.finishError(user, index, "oauth_token_failed: "+err.Error(), "error")
			return
		}
		token = t
	}
	if token == "" && !ch.cfg.TokenProvider.Enabled() {
		// cookie / anonymous mode: walk the source ladder
		// (session JSON -> session HTML -> anon JSON -> anon HTML)
		lr := FetchWithLadder(ctx, ch.sources, user, ch.cfg.Timeout, 3*time.Second)
		res := lr.Result
		if !res.OK && !res.NotFound && !res.Banned && classifyResult(res) != ClassThrottled {
			// last rung: a real Chrome (only for a definitive failure)
			if br := ch.browserFetch(ctx, user); br.OK || br.NotFound || br.Banned {
				res = br
			}
		}
		ch.finish(ctx, user, index, raw, res)
		return
	}
	for attempt := 0; attempt <= ch.cfg.Retries; attempt++ {
		if ch.abort.Load() {
			return
		}
		if len(ch.cfg.Proxies) > 0 {
			proxyLine = ch.cfg.Proxies[(index+attempt)%len(ch.cfg.Proxies)]
			if c2, err := ch.clientFor(proxyLine); err == nil {
				c = c2
			}
		}
		res = ch.fetch(ctx, c, user, token)
		e := res.Error
		class := classifyResult(res)
		switch {
		case e == "unauthorized" && ch.cfg.TokenProvider.Enabled() && attempt < ch.cfg.Retries:
			ch.cfg.TokenProvider.Invalidate()
			if t, err := ch.cfg.TokenProvider.Get(); err == nil {
				token = t
			}
		case class == ClassThrottled && attempt < ch.cfg.Retries:
			// transient block / rate limit: wait for the IP budget and retry
			// the SAME source (a different source would not help here).
			time.Sleep(time.Duration(attempt+1) * 3 * time.Second)
		case class == ClassNeedsSession && attempt < ch.cfg.Retries:
			// age gate: the JSON endpoint sometimes works where the HTML is
			// gated, so try it before giving up.
			jr := c.FetchUser(ctx, user, token, true)
			if classifyResult(jr) != ClassNeedsSession {
				res = jr
				goto done
			}
		default:
			goto done
		}
	}
done:
	ch.finish(ctx, user, index, raw, res)
}

// finish writes the result to the right file and updates stats.
func (ch *Checker) finish(ctx context.Context, user string, index int, raw string, res UserResult) {
	// feed the VPN throttle detector: a rotation only fires when the IP is
	// genuinely rate limited (never on a single transient result)
	ch.reportVPN(res)
	switch {
	case res.OK:
		ch.out.Append(KarmaBucketName(res.TotalKarma), strings.TrimSpace(raw))
		atomic.AddInt64(&ch.stats.Hit, 1)
		atomic.AddInt64(&ch.stats.Buckets[KarmaBucketIndex(res.TotalKarma)], 1)
		atomic.StoreInt64(&ch.blockedStreak, 0)
		ch.governor.Report(true)
		progressf("[%d/%d] HIT %s - Karma: %d | Link: %d | Comment: %d | Age: %dd | Premium: %v\n",
			index+1, ch.stats.Total, user, res.TotalKarma, res.LinkKarma, res.CommentKarma, res.AgeDays, res.Premium)
	case res.NotFound:
		ch.out.Append("not_found.txt", user+" | NOT_FOUND")
		atomic.AddInt64(&ch.stats.NotFound, 1)
		atomic.StoreInt64(&ch.blockedStreak, 0)
		ch.governor.Report(true)
		progressf("[%d/%d] NOT_FOUND %s\n", index+1, ch.stats.Total, user)
	case res.Banned:
		ch.out.Append("banned.txt", user+" | SUSPENDED")
		atomic.AddInt64(&ch.stats.Banned, 1)
		if res.Deleted {
			atomic.AddInt64(&ch.stats.Deleted, 1)
		}
		atomic.StoreInt64(&ch.blockedStreak, 0)
		ch.governor.Report(true)
		progressf("[%d/%d] SUSPENDED %s\n", index+1, ch.stats.Total, user)
	default:
		msg := res.Error
		if res.Detail != "" {
			msg = res.Error + ": " + res.Detail
		}
		ch.finishError(user, index, msg, res.Error)
	}
	// refresh the live status panel
	if ch.live != nil {
		ch.live.Update(atomic.LoadInt64(&ch.stats.Processed), atomic.LoadInt64(&ch.stats.Hit),
			atomic.LoadInt64(&ch.stats.NotFound)+atomic.LoadInt64(&ch.stats.Banned),
			atomic.LoadInt64(&ch.stats.Error))
	}
}

// finishError — parity with check_users._finish_error (streak + abort).
func (ch *Checker) finishError(user string, index int, message, err string) {
	ch.out.Append("errors.txt", fmt.Sprintf("%s | ERROR: %s", user, message))
	atomic.AddInt64(&ch.stats.Error, 1)
	fatal := err == "blocked" || err == "unauthorized"
	if err == "blocked" {
		atomic.AddInt64(&ch.stats.Blocked, 1)
	}
	if err == "login_required" {
		atomic.AddInt64(&ch.stats.AgeGate, 1)
	}
	if err == "challenge" {
		atomic.AddInt64(&ch.stats.Challenge, 1)
	}
	ch.governor.Report(false)
	if fatal {
		n := atomic.AddInt64(&ch.blockedStreak, 1)
		if n >= int64(ch.cfg.MaxBlocked) && ch.announced.CompareAndSwap(false, true) {
			ch.abort.Store(true)
			ch.blockedHelp(err)
			fmt.Printf("Stopping after %d blocked/unauthorized users in a row — fix auth before wasting more requests.\n", ch.cfg.MaxBlocked)
		}
	} else {
		atomic.StoreInt64(&ch.blockedStreak, 0)
	}
	progressf("[%d/%d] ERROR %s - %s\n", index+1, ch.stats.Total, user, message)
}

// resetAfterRotation clears every penalty the previous (rate-limited) IP
// caused: source breakers and the blocked streak start clean on a fresh IP.
func (ch *Checker) resetAfterRotation(rec RotationRecord) {
	atomic.StoreInt64(&ch.blockedStreak, 0)
	ch.clientsMu.Lock()
	for _, c := range ch.clients {
		if c == nil {
			continue
		}
		if c.Gate != nil && c.Gate.Breaker != nil {
			c.Gate.Breaker.Reset()
		}
		// the old TCP/TLS sessions point at the previous exit IP - drop them
		// so the next request dials through the new tunnel
		if c.HTTP != nil {
			c.HTTP.CloseIdleConnections()
		}
	}
	ch.clientsMu.Unlock()
	if ch.anon != nil {
		if ch.anon.Gate != nil && ch.anon.Gate.Breaker != nil {
			ch.anon.Gate.Breaker.Reset()
		}
		if ch.anon.HTTP != nil {
			ch.anon.HTTP.CloseIdleConnections()
		}
	}
	fmt.Println("VPN: source breakers and blocked streak reset for the fresh IP.")
}

// blockedHelp — parity with check_users._print_blocked_help.
func (ch *Checker) blockedHelp(err string) {
	if err == "unauthorized" {
		fmt.Println("Your cookies/token were rejected (expired or invalid).")
	} else if len(ch.cfg.Cookies) > 0 {
		fmt.Println("Your session cookies stopped being accepted — the session expired or Reddit blocked it.")
	} else {
		fmt.Println("Reddit blocks anonymous profile requests.")
	}
	if len(ch.cfg.Cookies) > 0 && (err == "unauthorized" || err == "blocked") {
		fmt.Println("Fix: run the login command to log in again (it re-saves cookies.json), then run again — progress is saved.")
		return
	}
	fmt.Println("Usually this is temporary rate limiting. Wait a minute and run again — progress is saved.")
}

// summary — parity with check_users.run() final block + the full run report.
func (ch *Checker) summary() {
	s := ch.stats
	elapsed := time.Since(ch.start)
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
	if ch.cfg.VPN != nil && ch.cfg.VPN.Rotations() > 0 {
		fmt.Printf("  VPN rotations: %d\n", ch.cfg.VPN.Rotations())
	}
	if ch.abort.Load() {
		fmt.Println("  STOPPED EARLY — run the login command to log in again. Progress is saved.")
	}
	fmt.Printf("  Saved to: %s\\0to500.txt ... 3000ormore.txt\n", ch.cfg.Output)

	// the full accounting report (console + run_report.txt)
	mode := "single session"
	if ch.cfg.HTMLMode {
		mode = "anonymous HTML scrape"
	} else if len(ch.cfg.Cookies) > 0 {
		mode = "cookie session"
	}
	rep := &RunReport{
		Mode:         mode,
		InputTotal:   s.InputTotal,
		Checked:      s.Processed,
		Skipped:      s.Skipped,
		Rejected:     ch.cfg.InputRejected,
		Valid:        s.Hit,
		NotFound:     s.NotFound,
		Suspended:    s.Banned - s.Deleted,
		Deleted:      s.Deleted,
		AgeGate:      s.AgeGate,
		Challenge:    s.Challenge,
		Blocked:      s.Blocked,
		Failed:       s.Error - s.Blocked - s.AgeGate - s.Challenge,
		Duration:     elapsed,
		OutputDir:    ch.cfg.Output,
		StoppedEarly: ch.abort.Load(),
		StartedAt:    ch.start,
	}
	if ch.cfg.VPN != nil {
		rep.VPNRotations = ch.cfg.VPN.Rotations()
	}
	for i, b := range KarmaBuckets {
		rep.Buckets = append(rep.Buckets, s.Buckets[i])
		rep.BucketNames = append(rep.BucketNames, b.Name)
	}
	rep.Print()
}
