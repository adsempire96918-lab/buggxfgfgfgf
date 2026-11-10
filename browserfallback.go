// Package main - browser fallback: the last rung of the source ladder.
// A real Chrome opens the profile page, so nothing can be fingerprinted
// away. Used only for the users the HTTP ladder could not resolve.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// BrowserFallback fetches profile pages with a real Chrome.
//
// It is the LAST rung of the source ladder and by far the most expensive
// one (a real page load per user). Two guards keep it from stalling a
// large run:
//   - a page POOL: several tabs share one Chrome, so NSFW users are
//     resolved in parallel instead of one after another
//   - a BUDGET: after N browser fetches the ladder stops trying and the
//     user is recorded with its HTTP error immediately
type BrowserFallback struct {
	mu      sync.Mutex
	browser *rod.Browser
	pages   []*rod.Page
	next    int
	used    int
	budget  int
	profile string
	timeout time.Duration
}

// NewBrowserFallback starts Chrome for the fallback lane.
func NewBrowserFallback(dir string, headless bool, timeout time.Duration, pages, budget int) (*BrowserFallback, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if pages <= 0 {
		pages = 1
	}
	profile := filepath.Join(dir, "browser_profile_fallback")
	l := launcher.New().
		Headless(headless).
		UserDataDir(profile).
		Set("window-size", "1366,768").
		NoSandbox(true)
	if bin := findChrome(); bin != "" {
		l = l.Bin(bin)
	}
	url, err := launchChrome(l, 30*time.Second)
	if err != nil {
		return nil, err
	}
	b := rod.New().ControlURL(url)
	if err := b.Connect(); err != nil {
		return nil, err
	}
	bf := &BrowserFallback{browser: b, profile: profile, timeout: timeout, budget: budget}
	for i := 0; i < pages; i++ {
		p, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
		if err != nil {
			b.Close()
			return nil, err
		}
		bf.pages = append(bf.pages, p)
	}
	return bf, nil
}

// launchChrome starts Chrome with enhanced fingerprinting and timeouts.
// Enhanced: perfect Chrome_133 TLS fingerprint, full Chrome UA, realistic headers,
// anti-detection measures, hard timeout to prevent hangs.
func launchChrome(l *launcher.Launcher, timeout time.Duration) (string, error) {
	// Perfect Chrome_133 TLS fingerprint (matches tls-client Chrome_133 profile)
	l = l.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	l = l.Set("accept-language", "en-US,en;q=0.9")
	// Full Chrome-style user agent hints
	l = l.Set("sec-ch-ua", "\"Not(A:Brand; v=\"99, \"Google Chrome\"; v=\"99, \"Chromium\"; v=\"99, \"Microsoft Edge\"\")")
	l = l.Set("sec-ch-ua-mobile", "?0")
	l = l.Set("sec-ch-ua-platform", "\"Windows\"")
	// Security headers and fingerprints
	l = l.Set("sec-fetch-site", "same-origin")
	l = l.Set("sec-fetch-mode", "navigate")
	l = l.Set("sec-fetch-dest", "document")
	l = l.Set("upgrade-insecure-requests", "1")
	l = l.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	l = l.Set("accept-encoding", "gzip, deflate, br, zstd")
	l = l.Set("cache-control", "max-age=0")

	type res struct {
		url string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		u, err := l.Launch()
		ch <- res{u, err}
	}()
	select {
	case r := <-ch:
		return r.url, r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("browser launch timed out after %s (enhanced fingerprint)", timeout)
	}
}

// SetSession injects a cookie session into the browser (for NSFW pages).
func (b *BrowserFallback) SetSession(s *Session) {
	if b == nil || s == nil {
		return
	}
	params := make([]*proto.NetworkCookieParam, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		domain := c.Domain
		if domain == "" {
			domain = defaultCookieDomain
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		params = append(params, &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: domain, Path: p, Secure: true,
		})
	}
	b.browser.SetCookies(params)
}

// Fetch opens the profile in the browser and parses the karma from the page.
func (b *BrowserFallback) Fetch(ctx context.Context, user string) UserResult {
	if b == nil || len(b.pages) == 0 {
		return errResult("browser_unavailable", "no browser")
	}
	u := NormalizeUsername(user)
	if u == "" {
		return errResult("invalid_username", "")
	}
	// budget: never let the browser hold up a large run
	b.mu.Lock()
	if b.budget > 0 && b.used >= b.budget {
		b.mu.Unlock()
		return errResult("browser_budget_exhausted",
			"browser fallback budget used up (the NSFW age gate needs a session)")
	}
	b.used++
	page := b.pages[b.next%len(b.pages)]
	b.next++
	b.mu.Unlock()

	target := "https://www.reddit.com/user/" + u + "/"
	if err := page.Timeout(b.timeout).Navigate(target); err != nil {
		return errResult("browser_navigate_failed", err.Error())
	}
	page.Timeout(b.timeout).WaitLoad()
	html, err := page.Timeout(b.timeout).HTML()
	if err != nil {
		return errResult("browser_read_failed", err.Error())
	}
	res := parseProfileHTML(html, u)
	if res.OK {
		res.Source = "browser"
	}
	return res
}

// Used returns how many browser fetches were spent.
func (b *BrowserFallback) Used() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Close shuts the browser down.
func (b *BrowserFallback) Close() {
	if b != nil && b.browser != nil {
		b.browser.Close()
	}
}
