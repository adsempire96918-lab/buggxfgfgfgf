// Package main - session validator: checks whether saved cookie sessions are
// still valid by injecting them into a REAL Chrome and opening reddit.com.
//
// No user:pass is ever submitted - only cookie injection. The browser sends
// the cookies itself, so Reddit sees a genuine logged-in session.
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// Session states (same vocabulary as the checker guard).
const (
	SessValid   = "VALID"
	SessDead    = "DEAD"
	SessBanned  = "BANNED"
	SessUnknown = "UNKNOWN"
)

// SessionResult is the validation outcome of one cookie file.
type SessionResult struct {
	File    string
	Account string
	Karma   int64
	State   string
	Detail  string
	Cookies int
	Over18  bool
	Home    string // homepage signal: "logged-in" / "logged-out" / "unknown"
}

// FindSessionFiles lists cookie files in a folder (top level + one level of
// sub-folders, so one folder can hold many accounts).
func FindSessionFiles(dir string) []string {
	out := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, f := range sub {
				if !f.IsDir() && isCookieFile(f.Name()) {
					out = append(out, filepath.Join(dir, e.Name(), f.Name()))
				}
			}
			continue
		}
		if isCookieFile(e.Name()) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// SessionMeta is the optional header block some cookie files carry
// (e.g. "Username: X", "Suspended: true", "Karma: 3").
type SessionMeta struct {
	Username  string
	Suspended bool
	Karma     int64
	Verified  bool
	Found     bool
}

// ParseSessionMeta reads the header block of a session file (best effort).
func ParseSessionMeta(path string) SessionMeta {
	m := SessionMeta{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(raw), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.Contains(l, "\t") {
			continue // skip cookie lines
		}
		low := strings.ToLower(l)
		switch {
		case strings.Contains(low, "username:"):
			if v := afterColon(l); v != "" {
				m.Username = v
				m.Found = true
			}
		case strings.Contains(low, "suspended:"):
			m.Suspended = strings.Contains(afterColon(l), "true")
			m.Found = true
		case strings.Contains(low, "karma:"):
			if v := afterColon(l); v != "" {
				m.Karma = parseNum(v)
				m.Found = true
			}
		case strings.Contains(low, "verified:"):
			m.Verified = strings.Contains(afterColon(l), "true")
			m.Found = true
		}
	}
	return m
}

// afterColon returns the trimmed text after the first colon.
func afterColon(line string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(line[i+1:]), "-–"))
}

// profileVerdict opens the account's own profile page and reports what the
// page says: suspended / logged-in-with-karma / login-gate.
func profileVerdict(page *rod.Page, username string, timeout time.Duration) (state, detail string, karma int64) {
	if username == "" {
		return "", "", 0
	}
	url := "https://www.reddit.com/user/" + url.PathEscape(username) + "/"
	if err := page.Timeout(timeout).Navigate(url); err != nil {
		return "", "profile_navigate_failed: " + err.Error(), 0
	}
	page.Timeout(timeout).WaitLoad()
	body, _ := page.Timeout(timeout).HTML()
	low := strings.ToLower(body)
	switch {
	case strings.Contains(low, "this account has been suspended"),
		strings.Contains(low, "account has been suspended"):
		return SessBanned, "u/" + username + " is suspended (profile page)", 0
	case strings.Contains(low, "log in to confirm your age"):
		return SessDead, "profile needs a login session (age gate)", 0
	case strings.Contains(low, "nobody on reddit goes by that name"):
		return SessDead, "profile does not exist", 0
	}
	if m := karmaRe.FindStringSubmatch(body); m != nil {
		return SessValid, "profile page shows karma", parseNum(m[1])
	}
	if isBlockPage(body) {
		return SessUnknown, "Reddit blocked the profile request", 0
	}
	return SessUnknown, "profile page gave no verdict", 0
}

// usernameFromFileName pulls "User-Name" out of names like
// "[0 friends] ... [Some-User-123].txt".
func usernameFromFileName(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if i := strings.LastIndex(base, "["); i >= 0 {
		base = base[i+1:]
	}
	base = strings.Trim(base, "[]")
	if base == "" {
		return ""
	}
	return NormalizeUsername(base)
}

// isCookieFile accepts the cookie formats we can read (json / raw header).
func isCookieFile(name string) bool {
	n := strings.ToLower(name)
	if strings.HasSuffix(n, ".status.txt") || strings.HasSuffix(n, ".report.txt") ||
		strings.Contains(n, "report") || strings.Contains(n, "readme") {
		return false
	}
	return strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".txt")
}

// ValidateSessionInBrowser injects the cookies into a real Chrome, opens
// reddit.com and reports whether the account is really logged in.
func ValidateSessionInBrowser(path string, headless bool, timeout time.Duration) SessionResult {
	res := SessionResult{File: path, State: SessUnknown, Home: "unknown"}

	cookies, err := LoadCookies(path)
	if err != nil {
		res.Detail = "read_error: " + err.Error()
		return res
	}
	res.Cookies = len(cookies)
	if len(cookies) == 0 {
		res.Detail = "no cookies in file"
		return res
	}

	profile := freshLoginProfileDir(filepath.Dir(path))
	ua := uniqueChromeUA()
	l := launcher.New().
		Headless(headless).
		UserDataDir(profile).
		Set("user-agent", ua).
		Set("window-size", "1366,768").
		NoSandbox(true)
	if bin := findChrome(); bin != "" {
		l = l.Bin(bin)
	}
	url, err := l.Launch()
	if err != nil {
		res.Detail = "chrome_launch_failed: " + err.Error()
		return res
	}
	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		l.Kill()
		res.Detail = "connect_failed: " + err.Error()
		return res
	}
	defer browser.Close()

	// inject the cookies - this is the only auth mechanism (no credentials)
	params := make([]*proto.NetworkCookieParam, 0, len(cookies))
	for _, c := range cookies {
		domain := c.Domain
		if domain == "" {
			domain = defaultCookieDomain
		}
		p := c.Path
		if p == "" {
			p = "/"
		}
		params = append(params, &proto.NetworkCookieParam{
			Name: c.Name, Value: c.Value, Domain: domain, Path: p,
		})
	}
	if err := browser.SetCookies(params); err != nil {
		res.Detail = "cookie_inject_failed: " + err.Error()
		return res
	}

	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		res.Detail = "page_failed: " + err.Error()
		return res
	}

	// 1) open reddit.com like a human
	if err := page.Timeout(timeout).Navigate("https://www.reddit.com/"); err != nil {
		res.Detail = "navigate_failed: " + err.Error()
		return res
	}
	page.Timeout(timeout).WaitLoad()
	waitJitter(800, 1800)
	home, _ := page.Timeout(timeout).HTML()

	// 2) ask Reddit who we are - from inside the browser (cookies sent by it)
	//    retry: a transient 403/block page must not be reported as "dead"
	var body string
	blocked := false
	for attempt := 0; attempt < 3; attempt++ {
		if err := page.Timeout(timeout).Navigate("https://www.reddit.com/api/v1/me"); err != nil {
			res.Detail = "me_failed: " + err.Error()
			res.Home = homeSignal(home, "")
			return res
		}
		page.Timeout(timeout).WaitLoad()
		body, _ = page.Timeout(timeout).HTML()
		if isBlockPage(body) {
			blocked = true
			waitJitter(1500, 3000)
			continue
		}
		break
	}

	me := parseMePayload(body)
	switch {
	case blocked && me == nil:
		res.State = SessUnknown
		res.Detail = "Reddit blocked the request (IP throttled / bot gate) - not a session verdict"
		res.Home = homeSignal(home, "")
	case me == nil:
		// no API data -> ask the profile page (suspended / age gate / karma)
		meta := ParseSessionMeta(path)
		username := meta.Username
		if username == "" {
			username = usernameFromFileName(path)
		}
		st, detail, karma := profileVerdict(page, username, timeout)
		res.State = st
		res.Detail = detail
		res.Account = username
		res.Karma = karma
		res.Home = homeSignal(home, username)
	case me.IsSuspended:
		res.State = SessBanned
		res.Account = me.Name
		res.Detail = "u/" + me.Name + " is suspended"
		res.Home = homeSignal(home, me.Name)
	default:
		res.State = SessValid
		res.Account = me.Name
		res.Karma = me.LinkKarma + me.CommentKarma
		res.Over18 = me.Over18
		res.Detail = "session valid"
		res.Home = homeSignal(home, me.Name)
	}
	return res
}

// isBlockPage lives in classify.go (shared with the checker).

// meInfo mirrors the /api/v1/me "data" object.
type meInfo struct {
	Name         string  `json:"name"`
	IsSuspended  bool    `json:"is_suspended"`
	LinkKarma    int64   `json:"link_karma"`
	CommentKarma int64   `json:"comment_karma"`
	Over18       bool    `json:"over_18"`
	CreatedUTC   float64 `json:"created_utc"`
}

// parseMePayload pulls the JSON out of the browser-rendered /api/v1/me page
// (Chrome wraps plain JSON in <pre>).
func parseMePayload(body string) *meInfo {
	candidates := []string{body}
	if i := strings.Index(body, "<pre>"); i >= 0 {
		if j := strings.Index(body[i:], "</pre>"); j > 0 {
			candidates = append(candidates, body[i+5:i+j])
		}
	}
	if i := strings.Index(body, "{"); i >= 0 {
		if j := strings.LastIndex(body, "}"); j > i {
			candidates = append(candidates, body[i:j+1])
		}
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if !strings.HasPrefix(c, "{") {
			continue
		}
		var top struct {
			Data *meInfo `json:"data"`
		}
		if err := json.Unmarshal([]byte(c), &top); err != nil {
			continue
		}
		if top.Data != nil {
			return top.Data
		}
	}
	return nil
}

// homeSignal reports what the homepage says about the login state.
func homeSignal(home, account string) string {
	if home == "" {
		return "unknown"
	}
	low := strings.ToLower(home)
	if account != "" {
		if strings.Contains(low, "/user/"+strings.ToLower(account)) ||
			strings.Contains(low, "u/"+strings.ToLower(account)) {
			return "logged-in"
		}
	}
	if strings.Contains(low, "log out") || strings.Contains(low, "logout") {
		return "logged-in"
	}
	if strings.Contains(low, "log in") {
		return "logged-out"
	}
	return "unknown"
}

// ValidateSessions validates every session file in a folder and writes a
// summary report next to them.
func ValidateSessions(dir string, headless bool, timeout time.Duration) []SessionResult {
	files := FindSessionFiles(dir)
	results := make([]SessionResult, 0, len(files))
	for _, f := range files {
		fmt.Printf("checking %s ...\n", f)
		r := ValidateSessionInBrowser(f, headless, timeout)
		results = append(results, r)
		fmt.Printf("  -> %-7s %s\n", r.State, r.Detail)
	}
	WriteSessionReport(dir, results)
	return results
}

// WriteSessionReport writes sassion_report.txt (and one status file per
// session) so the state is visible without re-running the browser.
func WriteSessionReport(dir string, results []SessionResult) {
	var sb strings.Builder
	sb.WriteString("SESSION VALIDATION REPORT (browser cookie-injection check)\n")
	sb.WriteString(strings.Repeat("=", 60) + "\n")
	valid, dead, banned, unknown := 0, 0, 0, 0
	for _, r := range results {
		fmt.Fprintf(&sb, "%-46s %-7s account=%-18s karma=%-8d over18=%-5v home=%-10s cookies=%d\n",
			r.File, r.State, r.Account, r.Karma, r.Over18, r.Home, r.Cookies)
		switch r.State {
		case SessValid:
			valid++
		case SessDead:
			dead++
		case SessBanned:
			banned++
		default:
			unknown++
		}
	}
	sb.WriteString(strings.Repeat("=", 60) + "\n")
	fmt.Fprintf(&sb, "valid=%d dead=%d banned=%d unknown=%d  total=%d\n",
		valid, dead, banned, unknown, len(results))
	sb.WriteString("TIME=" + time.Now().UTC().Format("2006-01-02 15:04:05 UTC") + "\n")
	os.WriteFile(filepath.Join(dir, "sassion_report.txt"), []byte(sb.String()), 0644)

	for _, r := range results {
		status := fmt.Sprintf("STATE=%s\nACCOUNT=%s\nKARMA=%d\nOVER18=%v\nHOME=%s\nCOOKIES=%d\nDETAIL=%s\nTIME=%s\n",
			r.State, r.Account, r.Karma, r.Over18, r.Home, r.Cookies, r.Detail,
			time.Now().UTC().Format("2006-01-02 15:04:05 UTC"))
		name := strings.TrimSuffix(filepath.Base(r.File), filepath.Ext(r.File)) + ".status.txt"
		os.WriteFile(filepath.Join(filepath.Dir(r.File), name), []byte(status), 0644)
	}
}
