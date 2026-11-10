// Package main — Reddit fetch layer (uTLS Chrome fingerprint).
// Port of reddit_fetch.py: JSON + HTML sources, jsc solver, NSFW handling.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttpjar "github.com/bogdanfinn/fhttp/cookiejar"
	fhttptrace "github.com/bogdanfinn/fhttp/httptrace"
	tls_client "github.com/bogdanfinn/tls-client"
)

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"

// chromeHeaders returns a full Chrome navigation header set (the same one
// curl_cffi sends when impersonating Chrome). Reddit treats requests without
// sec-ch-ua / sec-fetch-* / Accept-Encoding as bots, so these matter.
func chromeHeaders(accept string) map[string]string {
	h := map[string]string{
		"User-Agent":         defaultUA,
		"sec-ch-ua":          `"Chromium";v="133", "Not(A:Brand";v="24", "Google Chrome";v="133"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-site":     "none",
		"sec-fetch-mode":     "navigate",
		"sec-fetch-user":     "?1",
		"sec-fetch-dest":     "document",
		"Accept-Encoding":    "gzip, deflate, br, zstd",
		"Accept-Language":    "en-US,en;q=0.9",
	}
	if accept == "" {
		accept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"
	}
	h["Accept"] = accept
	return h
}

// Client is one browser-fingerprint HTTP session (cookie jar included —
// required because the jsc gate re-seeds session cookies each round).
// Gate is nil for the legacy path (Task 1 CLI) and set for the worker pool.
type Client struct {
	HTTP    tls_client.HttpClient
	Timeout time.Duration
	Gate    *Gate
	Headers *headerRotator
	Profile *ProfileRotator
	// Metrics is the shared run-wide collector. It is nil for the one-off
	// CLI commands (fetch/probe) - every method must tolerate a nil
	// collector, which is why Metrics.Record checks for nil itself.
	Metrics *Metrics
}

// newClient builds a Chrome-impersonating HTTP client (uTLS + real Chrome
// HTTP/2 fingerprint via tls-client). Reddit blocks plain Go TLS, and also
// blocks uTLS-only clients that speak HTTP/1.1 - the full Chrome stack
// (TLS hello + h2 SETTINGS + header order) is what passes.
//
// Advanced evasion:
//   - The TLS profile rotates between Chrome_125/Chrome_132/Chrome_133/Firefox
//     Each has a distinct JA3 hash, spreading automation detection risk.
//   - The header set rotates per request via headerRotator, distributing the
//     "bot" signal across UAs, Accept-Language and Accept variants.
func newClient(proxyLine string, timeout time.Duration, laneIdx int) (*Client, error) {
	jar, _ := fhttpjar.New(nil)
	profile := NewProfileRotator(laneIdx).Next()

	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutMilliseconds(int(timeout.Milliseconds())),
		tls_client.WithClientProfile(profile),
		tls_client.WithCookieJar(jar),
		tls_client.WithNotFollowRedirects(),
		// Reddit silently hangs HTTP/2 connections that carry the tls-client
		// h2 fingerprint; HTTP/1.1 over the Chrome TLS hello is accepted
		// (returns the jsc challenge page, which our solver handles).
		tls_client.WithForceHttp1(),
	}
	if p := ParseProxy(proxyLine); p != "" {
		opts = append(opts, tls_client.WithProxyUrl(p))
	}
	hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		HTTP:    hc,
		Timeout: timeout,
		Gate:    nil,
		Headers: NewHeaderRotator(laneIdx),
		Profile: NewProfileRotator(laneIdx),
	}, nil
}

// doRequest performs one GET; returns status, body, and Retry-After (0 = none).
// Uses the client's rotator for headers (anti-bot evasion) instead of fixed values.
func (c *Client) doRequest(ctx context.Context, reqURL string, extra map[string]string) (int, string, time.Duration, error) {
	start := time.Now()
	// Connection accounting (T-08). tls-client does not expose a connection
	// counter, but fhttp's transport honours net/http's GotConn trace hook, so
	// the Reused flag is the honest source for "did this request pay for a
	// TLS handshake?". Only installed when a collector is present.
	if c.Metrics != nil {
		// The hook must come from fhttp's httptrace fork: fhttp stores the
		// trace under its own context key, so net/http's httptrace would
		// silently never fire.
		trace := &fhttptrace.ClientTrace{
			GotConn: func(info fhttptrace.GotConnInfo) {
				c.Metrics.RecordConn(info.Reused)
			},
		}
		ctx = fhttptrace.WithClientTrace(ctx, trace)
	}
	if c.Headers != nil {
		h := c.Headers.Current()
		req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, reqURL, nil)
		if err != nil {
			return 0, "", 0, err
		}
		for k, v := range h {
			req.Header.Set(k, v)
		}
		if extra != nil {
			// Extra headers override rotating defaults so per-request needs
			// (Authorization, Accept, Referer) always win.
			for k, v := range extra {
				req.Header.Set(k, v)
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			c.Metrics.RecordError()
			return 0, "", 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, "", 0, err
		}
		// the metrics collector needs the STATUS and the latency of every
		// request - that is how the block rate is measured
		c.Metrics.Record(resp.StatusCode, time.Since(start), isChallengeBody(string(body)))
		ra := RetryAfter(resp.Header.Get("Retry-After"))
		return resp.StatusCode, string(body), ra, nil
	}
	// Legacy path: no rotator (Task 1 CLI / old behavior) uses static headers.
	accept := ""
	if extra != nil {
		accept = extra["Accept"]
	}
	h := chromeHeaders(accept)
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, reqURL, nil)
	if err != nil {
		return 0, "", 0, err
	}
	req.Header.Set("User-Agent", defaultUA)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.Metrics.RecordError()
		return 0, "", 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", 0, err
	}
	c.Metrics.Record(resp.StatusCode, time.Since(start), isChallengeBody(string(body)))
	ra := RetryAfter(resp.Header.Get("Retry-After"))
	return resp.StatusCode, string(body), ra, nil
}

// isChallengeBody reports whether a response body is Reddit's jsc challenge
// page. A challenge is NOT a block - it is solvable, and a fresh exit IP
// clears it - so the metrics collector counts it separately.
//
// It reuses the SAME marker as the breaker (challengeInputRe) so the metrics
// and the breaker can never disagree. A loose "contains the word challenge"
// check would count ordinary profile pages as challenges and inflate the
// number.
func isChallengeBody(body string) bool {
	if body == "" {
		return false
	}
	return challengeInputRe.MatchString(body)
}

// Get is the universal request path. When the client has a Gate (worker
// pool), it routes through GetResilient so JSON, HTML and every jsc round
// is rate-limited and breaker-protected. Without a Gate (Task 1 CLI) it is
// a plain request — legacy behavior unchanged.
func (c *Client) Get(ctx context.Context, reqURL string, extra map[string]string) (int, string, error) {
	if c.Gate != nil {
		return c.GetResilient(ctx, reqURL, extra)
	}
	code, body, _, err := c.doRequest(ctx, reqURL, extra)
	return code, body, err
}

// GetEx is Get plus the server's Retry-After duration (0 if absent).
func (c *Client) GetEx(ctx context.Context, reqURL string, extra map[string]string) (int, string, time.Duration, error) {
	return c.doRequest(ctx, reqURL, extra)
}

// queueWaitCap bounds how long a worker may sit in the rate-limiter queue.
//
// It is deliberately much larger than the request timeout: the queue wait is
// a SCHEDULING artifact (how many workers share one per-IP bucket), not a
// network problem. Cutting the queue wait short is what turned a slow queue
// into a request timeout.
const queueWaitCap = 5 * time.Minute

// GetResilient is the worker-pool path: per-IP token bucket + circuit
// breaker + 429 retry with exponential backoff (Retry-After honored).
// With a nil Gate it degrades to GetEx (legacy behavior).
//
// The queue wait and the request get SEPARATE deadlines. They used to share
// one context deadline, so with many workers on a single per-IP bucket the
// queue wait (tens of seconds) consumed the request's time and the request
// died with "context deadline exceeded" even though the network was fine.
// The account was then only saved by the HTML fallback - which is why the
// transport-failure count was ~1 per account while the account-level error
// count stayed low.
func (c *Client) GetResilient(ctx context.Context, reqURL string, extra map[string]string) (int, string, error) {
	retries := 0
	if c.Gate != nil {
		retries = c.Gate.Retries
	}
	if retries < 0 {
		retries = 0
	}
	attempt := 0
	for {
		if c.Gate != nil {
			// the queue wait gets its own generous budget
			waitCtx, cancelWait := ctx, func() {}
			if c.Timeout > 0 {
				waitCtx, cancelWait = context.WithTimeout(ctx, queueWaitCap)
			}
			// the queue depth is the clearest sign that the run is RATE
			// limited rather than network limited
			waitStart := time.Now()
			c.Metrics.QueueEnter()
			err := c.Gate.Admit(waitCtx)
			c.Metrics.QueueLeave(time.Since(waitStart))
			cancelWait()
			if err != nil {
				return 0, "", err
			}
		}
		// the request gets its OWN full timeout
		reqCtx, cancelReq := ctx, func() {}
		if c.Timeout > 0 {
			reqCtx, cancelReq = context.WithTimeout(ctx, c.Timeout)
		}
		code, body, ra, err := c.GetEx(reqCtx, reqURL, extra)
		cancelReq()
		if err != nil {
			return 0, "", err
		}
		if c.Gate != nil {
			c.Gate.Report(code, body)
		}
		if code == 429 && attempt < retries {
			delay := ra
			if d := c.Gate.BackoffDelay(attempt); d > delay {
				delay = d
			}
			if delay <= 0 {
				delay = 5 * time.Second
			}
			debugf("429 retry attempt=%d/%d delay=%s (retry-after=%s)", attempt+1, retries, delay, ra)
			select {
			case <-ctx.Done():
				return 429, "", ctx.Err()
			case <-time.After(delay):
			}
			attempt++
			continue
		}
		return code, body, nil
	}
}

var (
	jscXRe     = regexp.MustCompile(`e\+e\)\("([0-9a-f]{8,32})"\)`)
	jscTokenRe = regexp.MustCompile(`jsc_token[^>]*value="([0-9a-f]+)"`)
)

// debugf prints a diagnostic line when KARMA_DEBUG=1 is set. It is the
// step-by-step debug switch for the fetch layer (challenge rounds, status
// codes, solved pages) without touching the normal output.
func debugf(format string, args ...any) {
	if os.Getenv("KARMA_DEBUG") == "1" {
		fmt.Printf("[debug] "+format+"\n", args...)
	}
}

// SolveJSC handles Reddit's 2026 "jsc" anti-bot gate. The page computes the
// solution client-side as X+X; we submit it as query params.
//
// IMPORTANT (measured 2026-11-09): a CORRECT solve is answered by Reddit
// with **302 + a fresh `loid` cookie**, NOT with 200. The old code only
// accepted 200, so every successful solve was thrown away and the profile
// was reported as "blocked" - which stalled the startup check and made the
// VPN rotate for the wrong reason. After the 302 we re-request the original
// URL (the cookie jar now carries the solved session) and get the real page.
//
// extra carries the request headers of the original request so the refetch
// asks for the same representation (HTML page vs JSON payload).
func (c *Client) SolveJSC(ctx context.Context, reqURL, html string, maxRounds int, extra map[string]string) string {
	current := html
	for i := 0; i < maxRounds; i++ {
		if !challengeInputRe.MatchString(current) {
			return current // real page (challenge input gone)
		}
		m := jscXRe.FindStringSubmatch(current)
		if m == nil {
			return ""
		}
		x := m[1]
		token := ""
		if t := jscTokenRe.FindStringSubmatch(current); t != nil {
			token = t[1]
		}
		sep := "&"
		if !strings.Contains(reqURL, "?") {
			sep = "?"
		}
		url2 := reqURL + sep + "solution=" + x + x + "&js_challenge=1&jsc_token=" + token + "&jsc_orig_r="
		// The solve request can hit a transient 403/429 (Reddit throttles
		// bursts) - retry it a few times instead of giving up.
		for attempt := 0; attempt < 4; attempt++ {
			code, body, err := c.Get(ctx, url2, extra)
			debugf("jsc solve round=%d attempt=%d code=%d len=%d err=%v", i, attempt, code, len(body), err)
			if err == nil && code == 200 {
				current = body
				break
			}
			if err == nil && isRedirectCode(code) {
				// The solution was ACCEPTED: Reddit answers 302 and sets the
				// solved-session cookie. The real page comes from a refetch.
				current = body
				if c2, b2, e2 := c.Get(ctx, reqURL, extra); e2 == nil && c2 == 200 {
					current = b2
					debugf("jsc refetch ok: code=%d len=%d challenge=%v", c2, len(b2), challengeInputRe.MatchString(b2))
					break
				}
				debugf("jsc refetch failed - the solve did not stick")
				break
			}
			if attempt == 3 {
				return ""
			}
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(time.Duration(attempt+1) * 1500 * time.Millisecond):
			}
		}
	}
	if !challengeInputRe.MatchString(current) {
		return current
	}
	return ""
}

// ---------------------------------------------------------------------------
// JSC single-flight (T-05)
// ---------------------------------------------------------------------------
//
// A challenge wave is IP-wide: every lane behind the same exit IP gets the
// challenge form. Solving it per lane costs 2-4 extra requests each, so a
// 500-lane wave burns 2000+ requests against an IP that is already suspicious.
//
// The solved state is a COOKIE (jsc sets `loid`), and the cookie is bound to
// the client/IP, so one solve is enough for the whole exit IP:
//   - the first caller ("leader") solves the challenge for its own URL;
//   - the solved cookies are snapshotted and every waiter imports them into
//     its own jar, then re-fetches ITS OWN url (the leader's page belongs to
//     another account, so it must never be handed over).
//
// The guard is released on every exit path (defer), so a failed or timed-out
// solve never wedges the other lanes.

// jscCall is one in-flight solve shared by a group of lanes.
type jscCall struct {
	done    chan struct{}
	ok      bool
	cookies []*fhttp.Cookie
}

// jscSolver coalesces concurrent challenges per (exit IP, host). The three
// function seams default to the real client calls; tests replace them so the
// coalescing logic can be checked without network access.
type jscSolver struct {
	mu       sync.Mutex
	inflight map[string]*jscCall

	solveFn   func(ctx context.Context, c *Client, reqURL, html string, rounds int, extra map[string]string) string
	exportFn  func(c *Client) []*fhttp.Cookie
	importFn  func(c *Client, cookies []*fhttp.Cookie)
	refetchFn func(ctx context.Context, c *Client, reqURL string, extra map[string]string) string
}

func newJSCSolver() *jscSolver {
	return &jscSolver{
		inflight: make(map[string]*jscCall),
		solveFn: func(ctx context.Context, c *Client, reqURL, html string, rounds int, extra map[string]string) string {
			return c.SolveJSC(ctx, reqURL, html, rounds, extra)
		},
		exportFn: exportRedditCookies,
		importFn: importRedditCookies,
		refetchFn: func(ctx context.Context, c *Client, reqURL string, extra map[string]string) string {
			code, body, err := c.Get(ctx, reqURL, extra)
			if err != nil || code != 200 || challengeInputRe.MatchString(body) {
				return ""
			}
			return body
		},
	}
}

// jscFlights is the run-wide guard every lane client uses.
var jscFlights = newJSCSolver()

// redditCookieURLs are the jars the solved cookie can live in.
func redditCookieURLs() []*url.URL {
	urls := make([]*url.URL, 0, 3)
	for _, raw := range []string{
		"https://www.reddit.com/",
		"https://reddit.com/",
		"https://oauth.reddit.com/",
	} {
		if u, err := url.Parse(raw); err == nil {
			urls = append(urls, u)
		}
	}
	return urls
}

// exportRedditCookies snapshots the reddit cookies of a solved client.
func exportRedditCookies(c *Client) []*fhttp.Cookie {
	if c == nil || c.HTTP == nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []*fhttp.Cookie
	for _, u := range redditCookieURLs() {
		for _, ck := range c.HTTP.GetCookies(u) {
			if ck == nil || seen[ck.Name] {
				continue
			}
			seen[ck.Name] = true
			out = append(out, ck)
		}
	}
	return out
}

// importRedditCookies copies the solved cookies into another lane's jar.
func importRedditCookies(c *Client, cookies []*fhttp.Cookie) {
	if c == nil || c.HTTP == nil || len(cookies) == 0 {
		return
	}
	for _, u := range redditCookieURLs() {
		c.HTTP.SetCookies(u, cookies)
	}
}

// flightKey identifies "the same challenge" for the single-flight: same exit
// IP (proxy) and same host.
func (s *jscSolver) flightKey(c *Client, reqURL string) string {
	proxy := ""
	if c != nil && c.HTTP != nil {
		proxy = c.HTTP.GetProxy()
	}
	host := ""
	if u, err := url.Parse(reqURL); err == nil {
		host = u.Host
	}
	return proxy + "|" + host
}

// solve runs (or joins) one challenge solve.
func (s *jscSolver) solve(ctx context.Context, c *Client, reqURL, html string, rounds int, extra map[string]string) string {
	key := s.flightKey(c, reqURL)

	s.mu.Lock()
	if call, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ""
		case <-call.done:
		}
		if !call.ok {
			return ""
		}
		// reuse the solved session: import the cookies, then fetch OUR page
		s.importFn(c, call.cookies)
		return s.refetchFn(ctx, c, reqURL, extra)
	}
	call := &jscCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
		close(call.done)
	}()

	page := s.solveFn(ctx, c, reqURL, html, rounds, extra)
	if page != "" && !challengeInputRe.MatchString(page) {
		call.cookies = s.exportFn(c)
		call.ok = true
	}
	return page
}

// SolveJSCShared is the lane-facing entry point: one challenge wave costs one
// solve, and every lane ends up with the solved session in its own jar.
func (c *Client) SolveJSCShared(ctx context.Context, reqURL, html string, maxRounds int, extra map[string]string) string {
	if jscFlights == nil {
		return c.SolveJSC(ctx, reqURL, html, maxRounds, extra)
	}
	return jscFlights.solve(ctx, c, reqURL, html, maxRounds, extra)
}

// isRedirectCode reports the redirect status codes Reddit uses to accept a
// challenge solution (302 with an empty Location + a fresh session cookie).
func isRedirectCode(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

// challengeInputRe detects the jsc challenge form specifically — real pages
// may contain the bare string "jsc_token" in JS bundles, so a plain
// contains-check loops forever.
var challengeInputRe = regexp.MustCompile(`name="jsc_token"`)

// --------------------------------------------------------------------------
// JSON source
// --------------------------------------------------------------------------

type jsonUser struct {
	Name         string  `json:"name"`
	TotalKarma   int64   `json:"total_karma"`
	LinkKarma    int64   `json:"link_karma"`
	CommentKarma int64   `json:"comment_karma"`
	AwarderKarma int64   `json:"awarder_karma"`
	AwardeeKarma int64   `json:"awardee_karma"`
	Coins        int64   `json:"coins"`
	IsGold       bool    `json:"is_gold"`
	HasVerified  bool    `json:"has_verified_email"`
	Over18       bool    `json:"over_18"`
	IsSuspended  bool    `json:"is_suspended"`
	CreatedUTC   float64 `json:"created_utc"`
	Subreddit    struct {
		Subscribers int64 `json:"subscribers"`
	} `json:"subreddit"`
}

func parseUserPayload(payload json.RawMessage, username string) UserResult {
	var top struct {
		Error int      `json:"error"`
		Data  jsonUser `json:"data"`
	}
	if err := json.Unmarshal(payload, &top); err != nil {
		// payload may be the data object itself
		var d jsonUser
		if err2 := json.Unmarshal(payload, &d); err2 != nil {
			return errResult("parse_fail", firstN(strings.TrimSpace(string(payload)), 200))
		}
		return finishJSON(d, username)
	}
	if top.Error == 404 {
		return UserResult{NotFound: true}
	}
	return finishJSON(top.Data, username)
}

func finishJSON(d jsonUser, username string) UserResult {
	if d.IsSuspended {
		return UserResult{Banned: true}
	}
	total := d.TotalKarma
	if total == 0 {
		total = d.LinkKarma + d.CommentKarma
	}
	var created string
	var ageDays int64
	if d.CreatedUTC > 0 {
		created = time.Unix(int64(d.CreatedUTC), 0).UTC().Format("2006-01-02")
		ageDays = (time.Now().Unix() - int64(d.CreatedUTC)) / 86400
		if ageDays < 0 {
			ageDays = 0
		}
	} else {
		created = "unknown"
	}
	if d.Name == "" {
		d.Name = username
	}
	return UserResult{
		OK: true, Username: d.Name, TotalKarma: total,
		LinkKarma: d.LinkKarma, CommentKarma: d.CommentKarma,
		AwarderKarma: d.AwarderKarma, AwardeeKarma: d.AwardeeKarma,
		Coins: d.Coins, Premium: d.IsGold, Verified: d.HasVerified,
		Over18: d.Over18, AgeDays: ageDays, Created: created,
		Followers: d.Subreddit.Subscribers, Source: "json",
	}
}

var blockedMarkers = []string{
	"blocked by network security",
	"developer token",
	"developer credentials",
	"whoa there",
	"prove your humanity",
	"<title>blocked</title>",
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// FetchUser tries JSON first (exact fields), falls back to HTML.
// token == "" means cookie/anonymous session; non-empty means OAuth bearer.
func (c *Client) FetchUser(ctx context.Context, username, token string, allowHTML bool) UserResult {
	user := NormalizeUsername(username)
	if user == "" {
		return errResult("invalid_username", "")
	}
	q := url.PathEscape(user)

	var code int
	var body string
	var err error
	var reqURL string
	extra := map[string]string{}
	if token != "" {
		reqURL = "https://oauth.reddit.com/user/" + q + "/about?raw_json=1"
		extra["Authorization"] = "Bearer " + token
		extra["Accept"] = "application/json"
	} else {
		reqURL = "https://www.reddit.com/user/" + q + "/about.json?raw_json=1"
		extra["Accept"] = "application/json"
		extra["Referer"] = "https://www.reddit.com/user/" + q + "/"
		// retry transient 403/429 (Reddit throttles bursts)
		for attempt := 0; attempt < 3; attempt++ {
			code, body, err = c.Get(ctx, reqURL, extra)
			debugf("json fetch user=%s attempt=%d code=%d len=%d err=%v",
				user, attempt, code, len(body), err)
			if err != nil {
				break
			}
			if code != 403 && code != 429 {
				break
			}
			if attempt == 2 {
				break
			}
			select {
			case <-ctx.Done():
				return errResult("request_failed", ctx.Err().Error())
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
		}
		if err == nil &&
			(code == 401 || code == 403) && challengeInputRe.MatchString(body) {
			if solved := c.SolveJSCShared(ctx, reqURL, body, 4, extra); solved != "" {
				if code2, body2, err2 := c.Get(ctx, reqURL, extra); err2 == nil {
					code, body = code2, body2
				}
			}
		}
	}
	if err != nil {
		return errResult("request_failed", err.Error())
	}

	switch {
	case code == 404:
		// A deleted/suspended account returns the SAME 404 JSON as a
		// username that never existed, so the JSON answer alone cannot tell
		// them apart. The profile page can: a deleted account still renders
		// (with the "deleted" notice) while a non-existent one does not.
		// Without this the deleted accounts are silently counted as
		// NOT_FOUND and never land in banned.txt.
		if allowHTML {
			return c.resolveNotFound(ctx, user)
		}
		return UserResult{NotFound: true}
	case code == 429:
		// The JSON endpoint is rate limited but the profile page usually
		// still answers - try it before giving the account up.
		if allowHTML {
			hr := c.FetchUserHTML(ctx, user)
			if hr.OK || hr.NotFound || hr.Banned || hr.Deleted || hr.Error == "login_required" {
				return hr
			}
		}
		return errResult("rate_limited", "HTTP 429")
	case code == 401 || code == 403:
		low := strings.ToLower(body)
		if token != "" {
			return errResult("unauthorized", fmt.Sprintf("HTTP %d — token invalid/expired", code))
		}
		blocked := false
		for _, m := range blockedMarkers {
			if strings.Contains(low, m) {
				blocked = true
				break
			}
		}
		if blocked && allowHTML {
			hr := c.FetchUserHTML(ctx, user)
			if hr.OK || hr.NotFound || hr.Banned || hr.Error == "login_required" {
				return hr
			}
		}
		if blocked {
			return errResult("blocked", "Reddit blocked the request (cookies missing/expired, or IP blocked)")
		}
		// never call a 403 "suspended" — real suspension comes as 200 + is_suspended
		return errResult("forbidden", "HTTP 403 — "+firstN(body, 120))
	case code != 200:
		return errResult(fmt.Sprintf("http_%d", code), firstN(body, 200))
	}
	if strings.TrimSpace(body) == "" {
		return errResult("empty_body", "")
	}
	return parseUserPayload([]byte(body), user)
}

// --------------------------------------------------------------------------
// HTML source — no login, no browser, no cookies needed
// --------------------------------------------------------------------------

var (
	karmaRe        = regexp.MustCompile(`data-testid="karma-number"[^>]*>\s*([\d,.]+)`)
	linkCommentRe  = regexp.MustCompile(`([\d,]+)\s*post karma,\s*([\d,]+)\s*comment karma`)
	cakeDayRe      = regexp.MustCompile(`<time\b[^>]*data-testid="cake-day"[^>]*>`)
	datetimeRe     = regexp.MustCompile(`datetime="([^"]+)"`)
	prefixedNameRe = regexp.MustCompile(`data-testid="profile-prefixed-name"[^>]*>\s*<span[^>]*>\s*u/([^\s<]+)`)
	anyUNameRe     = regexp.MustCompile(`\bu/([A-Za-z0-9_-]{2,32})\b`)
	// profilePageRe matches the profile components Reddit renders for a real
	// account. A missing account gets the home feed instead - no profile
	// component at all.
	profilePageRe = regexp.MustCompile(`shreddit-profile|data-testid="profile-prefixed-name"`)
)

func parseProfileHTML(html, username string) UserResult {
	low := strings.ToLower(html)
	// Reddit's jsc anti-bot gate is NOT an IP rate limit - it is a solvable
	// challenge. Reporting it as "blocked" made the VPN detector rotate the
	// exit IP for the wrong reason and stalled the startup check.
	if challengeInputRe.MatchString(html) {
		return errResult("challenge", "Reddit's anti-bot challenge did not clear")
	}
	switch {
	case strings.Contains(low, "nobody on reddit goes by that name"):
		return UserResult{NotFound: true}
	case strings.Contains(low, "this account has been suspended"),
		strings.Contains(low, "account has been suspended"):
		return UserResult{Banned: true}
	case strings.Contains(low, "this user has deleted their account"),
		strings.Contains(low, "has deleted their account"):
		return UserResult{Banned: true, Deleted: true}
	}

	km := karmaRe.FindStringSubmatch(html)
	lm := linkCommentRe.FindStringSubmatch(html)
	if km == nil && lm == nil {
		if strings.Contains(low, "log in to confirm your age") ||
			(strings.Contains(low, "mature content") && strings.Contains(low, "log in")) {
			return errResult("login_required",
				"mature/NSFW profile — Reddit requires login to view (run login.bat to save a session)")
		}
		// Reddit serves the HOME FEED for an account that does not exist -
		// there is no profile component on the page at all. Reporting that
		// as "blocked" stalled the startup check on a perfectly healthy IP.
		if !profilePageRe.MatchString(html) {
			return UserResult{NotFound: true}
		}
		return errResult("blocked", "profile page did not render (challenge or rate limit)")
	}

	// A real profile can legitimately have NO karma-number element (a user
	// with 0 post + 0 comment karma). Treating that as "blocked" was a
	// false positive that stalled the startup check on a healthy IP.
	var total int64
	if km != nil {
		total = parseNum(km[1])
	}
	var link, comment int64
	if lm != nil {
		link = parseNum(lm[1])
		comment = parseNum(lm[2])
	}
	if km == nil {
		total = link + comment
	}

	created := "unknown"
	var ageDays int64
	if tag := cakeDayRe.FindString(html); tag != "" {
		if dtm := datetimeRe.FindStringSubmatch(tag); dtm != nil {
			if epoch := isoToEpoch(dtm[1]); epoch > 0 {
				created = time.Unix(epoch, 0).UTC().Format("2006-01-02")
				ageDays = (time.Now().Unix() - epoch) / 86400
				if ageDays < 0 {
					ageDays = 0
				}
			}
		}
	}

	name := username
	if nm := prefixedNameRe.FindStringSubmatch(html); nm != nil {
		name = strings.TrimSpace(nm[1])
	} else if nm := anyUNameRe.FindStringSubmatch(html); nm != nil {
		name = nm[1]
	}

	return UserResult{
		OK: true, Username: name, TotalKarma: total,
		LinkKarma: link, CommentKarma: comment,
		Premium: strings.Contains(low, "has reddit premium"),
		Created: created, AgeDays: ageDays, Source: "html",
	}
}

// resolveNotFound resolves a JSON 404 by looking at the profile page.
//
// Reddit answers {"message": "Not Found", "error": 404} for BOTH a username
// that never existed AND a deleted/suspended account. The profile page is
// the only way to tell them apart: a deleted account still renders (with the
// "this account was deleted" notice), a non-existent one does not.
//
// Ported from the Python original (reddit_fetch.py :: _resolve_notfound).
// Without it every deleted account is misfiled as NOT_FOUND.
func (c *Client) resolveNotFound(ctx context.Context, user string) UserResult {
	hr := c.FetchUserHTML(ctx, user)
	if hr.OK || hr.NotFound || hr.Banned || hr.Deleted || hr.Error == "login_required" {
		return hr
	}
	// The page did not answer either - keep the JSON verdict.
	return UserResult{NotFound: true}
}

// FetchUserHTML scrapes the profile page over plain HTTP (uTLS, no browser).
func (c *Client) FetchUserHTML(ctx context.Context, username string) UserResult {
	user := NormalizeUsername(username)
	if user == "" {
		return errResult("invalid_username", "")
	}
	q := url.PathEscape(user)
	reqURL := "https://www.reddit.com/user/" + q + "/"
	var code int
	var body string
	var err error
	accept := map[string]string{
		"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	}
	// Reddit throttles bursts: a 403 here is often transient, so retry a few
	// times with a short backoff before giving up.
	for attempt := 0; attempt < 4; attempt++ {
		code, body, err = c.Get(ctx, reqURL, accept)
		debugf("html fetch attempt=%d code=%d len=%d err=%v", attempt, code, len(body), err)
		if err != nil {
			return errResult("request_failed", err.Error())
		}
		if code == 404 {
			return UserResult{NotFound: true}
		}
		// The jsc gate can arrive with 200 OR 403. Solve it right away -
		// retrying the same request against the same IP never clears it.
		if (code == 200 || code == 403) && challengeInputRe.MatchString(body) {
			if solved := c.SolveJSCShared(ctx, reqURL, body, 4, accept); solved != "" {
				body = solved
				code = 200
			}
		}
		if code == 403 {
			// A 403 WITHOUT a challenge form is Reddit's real block page:
			// this exit IP is blocked. Retrying it only deepens the block
			// (and used to open the circuit breaker before the VPN rotation
			// could ever fire) - report it immediately so the IP rotates.
			return errResult("blocked", "Reddit blocked this IP (HTTP 403 block page)")
		}
		if code == 429 {
			return errResult("rate_limited", "HTTP 429")
		}
		if code != 200 {
			break
		}
		break
	}
	if code == 404 {
		return UserResult{NotFound: true}
	}
	if code != 200 {
		if code == 429 {
			return errResult("rate_limited", "HTTP 429")
		}
		return errResult(fmt.Sprintf("http_%d", code), firstN(body, 200))
	}
	// A real block page (200 + "whoa there, pardner") IS an IP-level block.
	if isBlockPage(body) {
		return errResult("blocked", "Reddit served a block page (this IP is blocked)")
	}
	return parseProfileHTML(body, user)
}
