package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSessionScaling measures how the clean request rate scales with the
// number of sessions (this is the decisive number for "how fast can we go").
func TestSessionScaling(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1")
	}
	files := FindSessionFiles("sassion")
	t.Logf("sessions available: %d", len(files))
	users := []string{"spez", "Misa_misa"}

	burst := func(nSessions, total int) (ok, blocked int, elapsed float64) {
		if nSessions > len(files) {
			nSessions = len(files)
		}
		clients := make([]*Client, 0, nSessions)
		for i := 0; i < nSessions; i++ {
			cs, err := LoadCookies(files[i])
			if err != nil || len(cs) == 0 {
				continue
			}
			c, err := newClient("", 30*time.Second, 0)
			if err != nil {
				continue
			}
			c.ApplyCookies(cs)
			clients = append(clients, c)
		}
		if len(clients) == 0 {
			return 0, 0, 0
		}
		type r struct{ ok, blocked bool }
		ch := make(chan r, total)
		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < len(clients); w++ {
			wg.Add(1)
			go func(c *Client) {
				defer wg.Done()
				for i := 0; i < total/len(clients)+1; i++ {
					u := users[i%len(users)]
					code, body, err := c.Get(context.Background(),
						"https://www.reddit.com/user/"+u+"/about.json?raw_json=1",
						map[string]string{"Accept": "application/json", "Referer": "https://www.reddit.com/user/" + u + "/"})
					if err != nil {
						ch <- r{}
						continue
					}
					if code == 200 && strings.Contains(body, "total_karma") {
						ch <- r{ok: true}
					} else {
						ch <- r{blocked: true}
					}
				}
			}(clients[w])
		}
		go func() { wg.Wait(); close(ch) }()
		for x := range ch {
			if x.ok {
				ok++
			} else if x.blocked {
				blocked++
			}
		}
		return ok, blocked, time.Since(start).Seconds()
	}

	for _, cfg := range []struct{ sessions, total int }{
		{1, 20}, {10, 60}, {50, 150}, {150, 300}, {454, 400},
	} {
		ok, bl, el := burst(cfg.sessions, cfg.total)
		t.Logf("%3d sessions, %3d reqs: ok=%-4d blocked=%-4d in %5.1fs -> %6.2f req/s (clean %5.2f/s)",
			cfg.sessions, cfg.total, ok, bl, el, float64(ok+bl)/el, float64(ok)/el)
		time.Sleep(10 * time.Second)
	}
}

// TestConcurrencyVsRate answers the key question: do MORE workers/sessions
// give more throughput, or is the limit per-IP?
//
//	A = 11 workers, shared 60/min bucket (paced)
//	B = 11 workers, no pacing at all (burst)
func TestConcurrencyVsRate(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1")
	}
	files := FindSessionFiles("sassion")
	if len(files) < 2 {
		t.Skip("need sessions")
	}
	users := []string{"spez", "Misa_misa"}

	run := func(workers int, perMin float64, total int) (ok, blocked, errs int, elapsed float64) {
		lim := NewIPLimiter(perMin, 2)
		if perMin <= 0 {
			lim = NewIPLimiter(100000, 2) // effectively no pacing
		}
		clients := make([]*Client, 0, workers)
		for i := 0; i < workers && i < len(files); i++ {
			cs, err := LoadCookies(files[i])
			if err != nil || len(cs) == 0 {
				continue
			}
			c, err := newClient("", 30*time.Second, 0)
			if err != nil {
				continue
			}
			c.ApplyCookies(cs)
			c.Gate = &Gate{
				Bucket:  lim.Bucket(""),
				Breaker: NewCircuitBreaker(50, time.Minute),
				Retries: 0, Backoff: time.Second,
			}
			clients = append(clients, c)
		}
		if len(clients) == 0 {
			return 0, 0, 0, 0
		}
		type r struct{ ok, blocked bool }
		ch := make(chan r, total)
		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < len(clients); w++ {
			wg.Add(1)
			go func(c *Client) {
				defer wg.Done()
				for i := 0; i < total/len(clients)+1; i++ {
					u := users[i%len(users)]
					code, body, err := c.Get(context.Background(),
						"https://www.reddit.com/user/"+u+"/about.json?raw_json=1",
						map[string]string{"Accept": "application/json", "Referer": "https://www.reddit.com/user/" + u + "/"})
					if err != nil {
						ch <- r{}
						continue
					}
					if code == 200 && strings.Contains(body, "total_karma") {
						ch <- r{ok: true}
					} else {
						ch <- r{blocked: true}
					}
				}
			}(clients[w])
		}
		go func() { wg.Wait(); close(ch) }()
		for x := range ch {
			if x.ok {
				ok++
			} else if x.blocked {
				blocked++
			} else {
				errs++
			}
		}
		return ok, blocked, errs, time.Since(start).Seconds()
	}

	ok, bl, er, el := run(11, 60, 30)
	t.Logf("A 11 workers + shared 60/min bucket : ok=%d blocked=%d err=%d in %.1fs (%.2f req/s)",
		ok, bl, er, el, float64(ok+bl+er)/el)
	time.Sleep(20 * time.Second)
	ok, bl, er, el = run(11, 0, 30)
	t.Logf("B 11 workers + NO pacing (burst)   : ok=%d blocked=%d err=%d in %.1fs (%.2f req/s)",
		ok, bl, er, el, float64(ok+bl+er)/el)
	// C: burst again immediately (bucket should be drained)
	ok, bl, er, el = run(11, 0, 30)
	t.Logf("C burst right after B (drained)     : ok=%d blocked=%d err=%d in %.1fs (%.2f req/s)",
		ok, bl, er, el, float64(ok+bl+er)/el)
	// D: after a 60s cooldown (bucket refilled?)
	time.Sleep(60 * time.Second)
	ok, bl, er, el = run(11, 0, 30)
	t.Logf("D burst after 60s cooldown          : ok=%d blocked=%d err=%d in %.1fs (%.2f req/s)",
		ok, bl, er, el, float64(ok+bl+er)/el)
	// E: THE decisive test - 1 session vs 11 sessions at the same burst rate
	ok, bl, er, el = run(1, 0, 30)
	t.Logf("E 1 session, no pacing (burst)      : ok=%d blocked=%d err=%d in %.1fs (%.2f req/s)",
		ok, bl, er, el, float64(ok+bl+er)/el)
	time.Sleep(30 * time.Second)
	ok, bl, er, el = run(11, 0, 30)
	t.Logf("F 11 sessions, no pacing (burst)    : ok=%d blocked=%d err=%d in %.1fs (%.2f req/s)",
		ok, bl, er, el, float64(ok+bl+er)/el)
}

// TestBrowserFallback checks the last rung of the ladder (real Chrome).
func TestBrowserFallback(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1")
	}
	b, err := NewBrowserFallback(".", true, 30*time.Second, 2, 10)
	if err != nil {
		t.Fatalf("browser: %v", err)
	}
	defer b.Close()
	for _, u := range []string{"spez", "nonexistentuser_xyz123"} {
		res := b.Fetch(context.Background(), u)
		t.Logf("browser %-24s ok=%v karma=%d notfound=%v err=%s source=%s",
			u, res.OK, res.TotalKarma, res.NotFound, res.Error, res.Source)
	}
}

// TestRateScale measures how many requests succeed with 1 session vs with
// N sessions in parallel (this decides the lane architecture).
func TestRateScale(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1")
	}
	files := FindSessionFiles("sassion")
	if len(files) < 3 {
		t.Skip("need 3 sessions")
	}
	users := []string{"spez", "Misa_misa", "spez", "Misa_misa"}

	run := func(nSessions, perSession int, delayMs ...int) (ok, blocked, errs int, elapsed float64) {
		start := time.Now()
		type res struct{ ok, blocked bool }
		ch := make(chan res, nSessions*perSession)
		for s := 0; s < nSessions; s++ {
			go func(s int) {
				cs, err := LoadCookies(files[s])
				if err != nil || len(cs) == 0 {
					return
				}
				c, err := newClient("", 30*time.Second, 0)
				if err != nil {
					return
				}
				c.ApplyCookies(cs)
				for i := 0; i < perSession; i++ {
					u := users[i%len(users)]
					code, body, err := c.Get(context.Background(),
						"https://www.reddit.com/user/"+u+"/about.json?raw_json=1",
						map[string]string{"Accept": "application/json", "Referer": "https://www.reddit.com/user/" + u + "/"})
					if err != nil {
						ch <- res{}
						continue
					}
					if code == 200 && strings.Contains(body, "total_karma") {
						ch <- res{ok: true}
					} else {
						ch <- res{blocked: true}
					}
					d := 700
					if len(delayMs) > 0 {
						d = delayMs[0]
					}
					time.Sleep(time.Duration(d) * time.Millisecond)
				}
			}(s)
		}
		for i := 0; i < nSessions*perSession; i++ {
			r := <-ch
			if r.ok {
				ok++
			} else if r.blocked {
				blocked++
			} else {
				errs++
			}
		}
		return ok, blocked, errs, time.Since(start).Seconds()
	}

	ok, bl, er, el := run(1, 12)
	t.Logf("1 session  x12: ok=%d blocked=%d err=%d in %.1fs (%.1f req/s)", ok, bl, er, el, float64(12)/el)
	time.Sleep(3 * time.Second)
	ok, bl, er, el = run(3, 8)
	t.Logf("3 sessions x8 : ok=%d blocked=%d err=%d in %.1fs (%.1f req/s)", ok, bl, er, el, float64(24)/el)
	time.Sleep(5 * time.Second)
	ok, bl, er, el = run(1, 20, 500)
	t.Logf("1 session  x20 @1.25rps: ok=%d blocked=%d err=%d in %.1fs (%.1f req/s)", ok, bl, er, el, float64(20)/el)
}

// 1. does /about.json work with a cookie session?
// 2. does an NSFW profile return data?
// 3. does adding over18=1 unlock it?
// 4. what is the practical rate limit?
func TestNSFWDiag(t *testing.T) {
	if os.Getenv("DIAG") == "" {
		t.Skip("set DIAG=1")
	}
	files := FindSessionFiles("sassion")
	var target string
	for _, f := range files {
		if strings.Contains(f, "Admirable") {
			target = f
			break
		}
	}
	if target == "" && len(files) > 0 {
		target = files[0]
	}
	base, err := LoadCookies(target)
	if err != nil || len(base) == 0 {
		t.Fatalf("load: %v", err)
	}
	t.Logf("session=%s cookies=%d", filepath.Base(target), len(base))

	probe := func(label string, extra []Cookie) {
		c, err := newClient("", 30*time.Second, 0)
		if err != nil {
			t.Fatalf("client: %v", err)
		}
		cs := append(append([]Cookie{}, base...), extra...)
		c.ApplyCookies(cs)
		ctx := context.Background()
		for _, u := range []string{"spez", "puthurjibin", "goesmanya", "Misa_misa"} {
			code, body, err := c.Get(ctx, "https://www.reddit.com/user/"+u+"/about.json?raw_json=1",
				map[string]string{"Accept": "application/json", "Referer": "https://www.reddit.com/user/" + u + "/"})
			if err != nil {
				t.Logf("%s %s: ERR %v", label, u, err)
				continue
			}
			var top struct {
				Error int `json:"error"`
				Data  struct {
					Name        string  `json:"name"`
					TotalKarma  int64   `json:"total_karma"`
					Over18      bool    `json:"over_18"`
					IsSuspended bool    `json:"is_suspended"`
					CreatedUTC  float64 `json:"created_utc"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(body), &top)
			t.Logf("%s %-12s code=%d len=%d err=%d name=%q karma=%d over18=%v suspended=%v",
				label, u, code, len(body), top.Error, top.Data.Name, top.Data.TotalKarma,
				top.Data.Over18, top.Data.IsSuspended)
		}
		// HTML path for an NSFW user (age gate check)
		c2, _ := newClient("", 30*time.Second, 0)
		c2.ApplyCookies(append(append([]Cookie{}, base...), extra...))
		code, body, _ := c2.Get(ctx, "https://www.reddit.com/user/goesmanya/",
			map[string]string{"Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"})
		low := strings.ToLower(body)
		t.Logf("%s HTML goesmanya: code=%d len=%d agegate=%v karma=%v suspended_marker=%v",
			label, code, len(body),
			strings.Contains(low, "log in to confirm your age"),
			strings.Contains(body, "karma-number"),
			strings.Contains(low, "account has been suspended"))
	}

	probe("plain    ", nil)
	probe("over18=1 ", []Cookie{{Name: "over18", Value: "1", Domain: defaultCookieDomain, Path: "/"}})
}
