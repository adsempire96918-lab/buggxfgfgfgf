// Package main - browser login (Task 5): rod -> real Chrome, human-like
// flow, parity with login_browser.py.
//
// Flow: reddit.com homepage -> wait 1-3s -> hover "Log in" -> click ->
// user logs in manually -> poll cookies -> health check -> save 3 files.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const (
	requiredCookieA = "reddit_session"
	requiredCookieB = "token_v2"
)

var bannedMarkers = []string{
	"account has been suspended",
	"account is suspended",
	"your account is suspended",
	"suspended for violation",
}

// LoginFlags for the login command.
type LoginFlags struct {
	Timeout  time.Duration
	Profile  string // profile dir ("" = auto unique)
	Headless bool
}

// findChrome locates the installed Google Chrome ("" = let rod decide).
func findChrome() string {
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates, filepath.Join(la, `Google`, `Chrome`, `Application`, `chrome.exe`))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// runLogin drives the browser login flow. Returns process exit code:
// 0 valid, 2 banned, 1 failed/timeout.
func runLogin(f LoginFlags) int {
	profile := f.Profile
	if profile == "" {
		profile = freshLoginProfileDir(".")
	}
	ua := uniqueChromeUA()
	fmt.Printf("Engine: real Chrome\nProfile: %s\nUA: %s\n\n", profile, ua)

	l := launcher.New().
		Headless(f.Headless).
		UserDataDir(profile).
		Set("user-agent", ua).
		Set("window-size", "1366,768").
		NoSandbox(true)
	if bin := findChrome(); bin != "" {
		l = l.Bin(bin)
	}
	url, err := l.Launch()
	if err != nil {
		fmt.Fprintf(os.Stderr, "chrome launch failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Make sure Google Chrome is installed.")
		return 1
	}
	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		l.Kill()
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		return 1
	}
	defer browser.Close()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "page: %v\n", err)
		return 1
	}

	// 1. homepage
	if err := page.Timeout(30 * time.Second).Navigate("https://www.reddit.com/"); err != nil {
		fmt.Fprintf(os.Stderr, "navigate: %v\n", err)
		return 1
	}
	page.Timeout(30 * time.Second).MustWaitLoad()
	fmt.Println("reddit.com opened - waiting like a human...")
	waitJitter(1000, 2000)

	// 2. find + hover + click "Log in"
	deadline := time.Now().Add(20 * time.Second)
	clicked := false
	for time.Now().Before(deadline) {
		if el := firstLoginLink(page); el != nil {
			el.Hover()
			waitJitter(300, 900)
			if el.Click(proto.InputMouseButtonLeft, 1) == nil {
				clicked = true
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !clicked {
		fmt.Println("Could not find the Log in link - opening the login page directly.")
		page.Timeout(30 * time.Second).Navigate("https://www.reddit.com/login/")
	}
	// wait for the login form (bounded - Reddit's page never becomes "stable")
	if _, err := page.Timeout(15 * time.Second).Element(`input[type="password"]`); err != nil {
		fmt.Println("Login form not detected yet - you can still log in manually in the window.")
	}
	fmt.Println("Login form ready - log in with your account (2FA/captcha: solve it in the window).")
	fmt.Println("Waiting for the session...")

	// 3. poll cookies until login or timeout
	ctx, cancel := context.WithTimeout(context.Background(), f.Timeout)
	defer cancel()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nTimeout - no login detected. Nothing was saved.")
			_ = WriteStatus(".", "FAILED", "", "timeout - no login detected", 0)
			return 1
		case <-tick.C:
			if html, err := page.Timeout(5 * time.Second).HTML(); err == nil {
				low := strings.ToLower(html)
				for _, m := range bannedMarkers {
					if strings.Contains(low, m) {
						fmt.Println("\nAccount is BANNED / suspended - nothing saved.")
						_ = WriteStatus(".", "BANNED", "", "suspended page shown by Reddit", 0)
						return 2
					}
				}
			}
			cookies, err := browser.GetCookies()
			if err != nil {
				continue
			}
			if hasRequiredCookies(cookies) {
				return saveSession(".", cookies)
			}
		}
	}
}

// firstLoginLink finds the header "Log in" link (new and old UI).
func firstLoginLink(page *rod.Page) *rod.Element {
	for _, sel := range []string{
		`a[href$="/login/"]`,
		`a:has-text("Log in")`,
		`a:has-text("Log In")`,
	} {
		if el, err := page.Timeout(2 * time.Second).Element(sel); err == nil && el != nil {
			return el
		}
	}
	return nil
}

// hasRequiredCookies - parity with login_browser.logged_in.
func hasRequiredCookies(cookies []*proto.NetworkCookie) bool {
	names := map[string]bool{}
	for _, c := range cookies {
		names[c.Name] = true
	}
	return names[requiredCookieA] && names[requiredCookieB]
}

// saveSession health-checks the fresh cookies (uTLS, same engine as the
// checker) and saves the 3 files - parity with login_browser.py.
func saveSession(dir string, cookies []*proto.NetworkCookie) int {
	cs := make([]Cookie, 0, len(cookies))
	for _, c := range cookies {
		domain := c.Domain
		if domain == "" {
			domain = defaultCookieDomain
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		cs = append(cs, Cookie{Name: c.Name, Value: c.Value, Domain: domain, Path: p})
	}
	c, err := newClient("", 30*time.Second, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session check client: %v\n", err)
		return 1
	}
	info := c.HealthCheck(context.Background(), cs)
	if info.Suspended {
		_ = WriteStatus(dir, "BANNED", info.Account, "account is suspended (API)", len(cs))
		fmt.Println("\nAccount is SUSPENDED (API) - nothing saved.")
		return 2
	}
	if !info.OK {
		_ = WriteStatus(dir, "FAILED", info.Account, info.Detail, len(cs))
		fmt.Printf("\nSession check failed: %s\nCookies were NOT saved. Try again.\n", info.Detail)
		return 1
	}
	if err := SaveCookieFiles(dir, cs); err != nil {
		fmt.Fprintf(os.Stderr, "save: %v\n", err)
		return 1
	}
	detail := fmt.Sprintf("session valid - karma %d, created %s", info.Karma, info.Created)
	_ = WriteStatus(dir, "VALID", info.Account, detail, len(cs))
	fmt.Printf("\n[OK] u/%s - session VALID\n", info.Account)
	fmt.Printf("     Account karma: %d | Created: %s\n", info.Karma, info.Created)
	fmt.Println("     Saved: cookies.json, cookie_string.txt, login_status.txt")
	fmt.Println("\nNow run:  userkarmacheck check   (cookies.json is auto-loaded)")
	return 0
}

// freshLoginProfileDir - unique per run, old run profiles swept (parity).
func freshLoginProfileDir(base string) string {
	name := "browser_profile_" + strconv.FormatInt(time.Now().Unix(), 10)
	re := regexp.MustCompile(`^browser_profile_\d+$`)
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if e.IsDir() && re.MatchString(e.Name()) && e.Name() != name {
				os.RemoveAll(filepath.Join(base, e.Name()))
			}
		}
	}
	return filepath.Join(base, name)
}

// uniqueChromeUA - realistic Windows Chrome UA, randomized per run (parity).
func uniqueChromeUA() string {
	major := 124 + rand.Intn(10)
	build := rand.Intn(4001)
	return fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.0.%d Safari/537.36", major, build)
}

// waitJitter sleeps a random duration in [minMs, maxMs] ms.
func waitJitter(minMs, maxMs int) {
	d := time.Duration(minMs) + time.Duration(rand.Intn(maxMs-minMs+1))*time.Millisecond
	time.Sleep(d)
}
