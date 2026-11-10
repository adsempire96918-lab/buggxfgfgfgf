// Package main — session management (Task 3).
// 1:1 port of the Python session layer:
//   - reddit_fetch.load_cookie_file / parse_cookie_string / apply_cookies
//   - login_browser.cookie_string / write_status / health_check
//   - check_users.Checker._session_health (valid / dead / unknown)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// Cookie mirrors Python's cookie dict {"name","value","domain","path"}.
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path,omitempty"`
}

const defaultCookieDomain = ".reddit.com"

var cookieSplitRe = regexp.MustCompile(`[;\n]`)

// ParseCookieString parses a copied 'Cookie:' header — parity with
// reddit_fetch.parse_cookie_string (reserved attribute names are skipped).
func ParseCookieString(raw string) []Cookie {
	cookies := []Cookie{}
	for _, part := range cookieSplitRe.Split(raw, -1) {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		idx := strings.Index(part, "=")
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		value = strings.Trim(value, `"`)
		switch strings.ToLower(name) {
		case "", "path", "domain", "expires", "max-age", "samesite", "secure", "httponly":
			continue
		}
		cookies = append(cookies, Cookie{Name: name, Value: value, Domain: defaultCookieDomain, Path: "/"})
	}
	return cookies
}

// ParseSessionLine parses ONE line of a session file into cookies.
//
// Ported from the Python original (accounts.py :: _load_session_files). It
// accepts every shape a session file is stored in:
//
//	"reddit_session=abc"          -> the cookie pair
//	"abc; token_v2=def"           -> abc is reddit_session, def is token_v2
//	"Cookie: reddit_session=abc"  -> the "Cookie:" header prefix is stripped
//	"eyJhbGciOiJSUzI1NiIs..."     -> a BARE reddit_session value (a JWT)
//
// The bare form is the important one: a JWT has no "=" at all, so a plain
// cookie-header parser returns nothing for it and the session silently loads
// with zero cookies.
func ParseSessionLine(line string) []Cookie {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil
	}
	// strip a "Cookie:" header prefix
	if strings.HasPrefix(strings.ToLower(line), "cookie:") {
		line = strings.TrimSpace(line[7:])
		line = strings.TrimSuffix(line, ";")
		line = strings.TrimSpace(line)
	}
	if line == "" {
		return nil
	}

	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "reddit_session=") || strings.Contains(line, ";") {
		cookies := ParseCookieString(line)
		for _, c := range cookies {
			if strings.EqualFold(c.Name, "reddit_session") && c.Value != "" {
				return cookies
			}
		}
		// looks like "value; other=..." but no reddit_session inside:
		// the first field IS the reddit_session value.
		value, rest, _ := strings.Cut(line, ";")
		out := []Cookie{{
			Name:   "reddit_session",
			Value:  strings.TrimSpace(value),
			Domain: defaultCookieDomain,
			Path:   "/",
		}}
		return append(out, ParseCookieString(rest)...)
	}

	// A Netscape cookie line is TAB separated (domain \t path \t ... \t name
	// \t value). It is NOT a bare session value - treating it as one would
	// turn every cookie line of a session file into a separate "session".
	if strings.Contains(line, "\t") {
		return nil
	}

	// bare value: the whole line IS the reddit_session value. It must look
	// like a token (no spaces, no "=", no ";") - otherwise it is some other
	// kind of line that this parser must not swallow. A newline means the
	// caller passed a whole FILE, not a line.
	if strings.ContainsAny(line, " \t\r\n=\"") {
		return nil
	}
	return []Cookie{{
		Name:   "reddit_session",
		Value:  line,
		Domain: defaultCookieDomain,
		Path:   "/",
	}}
}

// LoadSessionValues loads a sessions.txt: ONE bare reddit_session value per
// line. It returns one cookie set per line (the Python sessions.txt format).
//
// IMPORTANT: a file that is NOT a bare-value list (a Netscape cookie file, a
// JSON cookie file, a raw "Cookie:" header) is a SINGLE session and is
// returned as exactly one cookie set. Splitting such a file per line would
// turn every cookie line of a session file into its own bogus "session" -
// 467 session files became 9288 "sessions" that way.
func LoadSessionValues(path string) ([][]Cookie, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return nil, nil
	}

	// Only a file where EVERY line is a bare token is a multi-session file.
	multi := true
	for _, l := range lines {
		if !isBareSessionValue(l) {
			multi = false
			break
		}
	}
	if !multi {
		// a single-session file (Netscape / JSON / raw header)
		cs, err := LoadCookies(path)
		if err != nil || len(cs) == 0 {
			return nil, err
		}
		return [][]Cookie{cs}, nil
	}

	var out [][]Cookie
	seen := map[string]bool{}
	for _, line := range lines {
		cs := ParseSessionLine(line)
		if len(cs) == 0 {
			continue
		}
		// dedup on the reddit_session value
		key := ""
		for _, c := range cs {
			if strings.EqualFold(c.Name, "reddit_session") {
				key = c.Value
			}
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, cs)
	}
	return out, nil
}

// isBareSessionValue reports whether a line is a bare reddit_session value
// (a token): no "=", no ";", no tab, no space.
func isBareSessionValue(line string) bool {
	if line == "" {
		return false
	}
	return !strings.ContainsAny(line, " \t=;\"")
}

// LoadCookies loads cookies from any of the 4 supported formats — parity
// with reddit_fetch.load_cookie_file: raw 'Cookie:' header, Playwright /
// Cookie-Editor JSON array, simple JSON object {name: value}, or Netscape
// cookies.txt. A missing/empty file yields an empty list (no error).
//
// A session file that holds a bare reddit_session value (a JWT) is also
// accepted - see ParseSessionLine.
func LoadCookies(path string) ([]Cookie, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Cookie{}, nil
		}
		return nil, err
	}
	s := strings.TrimSpace(string(raw))
	s = strings.TrimPrefix(s, "\ufeff") // BOM from editors
	if s == "" {
		return []Cookie{}, nil
	}

	// raw Cookie header (not JSON, no tabs)
	if !strings.ContainsAny(s[:1], "[{") && !strings.Contains(s, "\t") {
		if parsed := ParseCookieString(s); len(parsed) > 0 {
			return parsed, nil
		}
		// A bare reddit_session value (a JWT) has no "=" at all, so the
		// cookie-header parser above finds nothing. Try the session-line
		// parser before giving up - otherwise the file loads with ZERO
		// cookies and the session silently does not work.
		if parsed := ParseSessionLine(s); len(parsed) > 0 {
			return parsed, nil
		}
	}

	if s[0] == '[' || s[0] == '{' {
		var data interface{}
		if err := json.Unmarshal([]byte(s), &data); err != nil {
			return nil, fmt.Errorf("invalid cookie JSON: %w", err)
		}
		switch v := data.(type) {
		case map[string]interface{}:
			cs := []Cookie{}
			for name, value := range v {
				cs = append(cs, Cookie{Name: name, Value: fmt.Sprintf("%v", value), Domain: defaultCookieDomain, Path: "/"})
			}
			return cs, nil
		case []interface{}:
			cs := []Cookie{}
			for _, item := range v {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := m["name"].(string)
				if name == "" {
					continue
				}
				value, _ := m["value"].(string)
				domain, _ := m["domain"].(string)
				if domain == "" {
					domain = defaultCookieDomain
				}
				p, _ := m["path"].(string)
				if p == "" {
					p = "/"
				}
				cs = append(cs, Cookie{Name: name, Value: value, Domain: domain, Path: p})
			}
			return cs, nil
		}
		return []Cookie{}, nil
	}

	// Netscape cookies.txt (tab-separated, >= 7 columns)
	cs := []Cookie{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) >= 7 {
			domain := parts[0]
			if domain == "" {
				domain = defaultCookieDomain
			}
			p := parts[2]
			if p == "" {
				p = "/"
			}
			cs = append(cs, Cookie{Name: parts[5], Value: parts[6], Domain: domain, Path: p})
		}
	}
	return cs, nil
}

// ApplyCookies injects cookies into this client's jar — parity with
// reddit_fetch.apply_cookies. They are sent with every later request.
func (c *Client) ApplyCookies(cookies []Cookie) {
	jar := c.HTTP.GetCookieJar()
	if jar == nil {
		return
	}
	u, _ := url.Parse("https://www.reddit.com/")
	for _, ck := range cookies {
		domain := ck.Domain
		if domain == "" {
			domain = defaultCookieDomain
		}
		p := ck.Path
		if p == "" {
			p = "/"
		}
		jar.SetCookies(u, []*fhttp.Cookie{{
			Name: ck.Name, Value: ck.Value, Domain: domain, Path: p,
		}})
	}
}

// CookieString builds the raw 'Cookie:' header — parity with
// login_browser.cookie_string.
func CookieString(cookies []Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// WriteStatus writes login_status.txt — parity with login_browser.write_status.
func WriteStatus(dir, status, account, detail string, count int) error {
	if account == "" {
		account = "unknown"
	}
	ts := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	body := fmt.Sprintf("STATUS=%s\nACCOUNT=%s\nDETAIL=%s\nCOOKIES=%d\nTIME=%s\n",
		status, account, detail, count, ts)
	body = strings.ReplaceAll(body, "\n", lineEnding)
	return os.WriteFile(filepath.Join(dir, "login_status.txt"), []byte(body), 0644)
}

// SaveCookieFiles writes cookies.json + cookie_string.txt — parity with the
// login_browser.py save step. cookies.json stays byte-compatible with what
// the Python checker loads (name/value/domain/path only).
func SaveCookieFiles(dir string, cookies []Cookie) error {
	raw, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		return err
	}
	// Python's write_text() translates \n to CRLF on Windows - match it so the
	// file is byte-identical to the Python project's cookies.json.
	raw = []byte(strings.ReplaceAll(string(raw), "\n", lineEnding))
	raw = append(raw, []byte(lineEnding)...)
	if err := os.WriteFile(filepath.Join(dir, "cookies.json"), raw, 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "cookie_string.txt"), []byte(CookieString(cookies)+lineEnding), 0644)
}

// meData mirrors the /api/v1/me "data" object.
type meData struct {
	Name         string  `json:"name"`
	IsSuspended  bool    `json:"is_suspended"`
	LinkKarma    int64   `json:"link_karma"`
	CommentKarma int64   `json:"comment_karma"`
	CreatedUTC   float64 `json:"created_utc"`
}

// notJSONErr signals a 200 response that was not JSON (challenge page).
var notJSONErr = fmt.Errorf("non_json")

// probeMe performs ONE GET /api/v1/me with the given cookies and returns
// (httpCode, dataOrNil, err). Non-200 has data==nil, err==nil.
func (c *Client) probeMe(ctx context.Context, cookies []Cookie) (int, *meData, error) {
	c.ApplyCookies(cookies)
	code, body, err := c.Get(ctx, "https://www.reddit.com/api/v1/me", map[string]string{
		"Accept":  "application/json",
		"Referer": "https://www.reddit.com/",
	})
	if err != nil {
		return 0, nil, err
	}
	if code != 200 {
		return code, nil, nil
	}
	var top struct {
		Data *meData `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return 200, nil, notJSONErr
	}
	return 200, top.Data, nil
}

// SessionInfo mirrors login_browser.health_check's result dict.
type SessionInfo struct {
	OK        bool
	Account   string
	Karma     int64
	Created   string
	Suspended bool
	Detail    string
}

// HealthCheck verifies cookies against /api/v1/me — parity with
// login_browser.health_check.
func (c *Client) HealthCheck(ctx context.Context, cookies []Cookie) SessionInfo {
	info := SessionInfo{}
	code, data, err := c.probeMe(ctx, cookies)
	if err == notJSONErr {
		info.Detail = "HTTP 200 but response is not JSON (challenge page)"
		return info
	}
	if err != nil {
		info.Detail = fmt.Sprintf("request_failed: %v", err)
		return info
	}
	if code != 200 {
		if code == 401 || code == 403 {
			info.Detail = fmt.Sprintf("HTTP %d — session rejected (cookies incomplete or blocked)", code)
		} else {
			info.Detail = fmt.Sprintf("HTTP %d", code)
		}
		return info
	}
	if data == nil {
		info.Detail = "not logged in — session cookies were rejected"
		return info
	}
	if data.IsSuspended {
		info.Suspended = true
		info.Account = data.Name
		info.Detail = "account is suspended"
		return info
	}
	info.OK = true
	info.Account = data.Name
	info.Karma = data.LinkKarma + data.CommentKarma
	if data.CreatedUTC > 0 {
		info.Created = time.Unix(int64(data.CreatedUTC), 0).UTC().Format("2006-01-02")
	} else {
		info.Created = "unknown"
	}
	info.Detail = "session valid"
	return info
}

// Session guard states — parity with check_users.Checker._session_health.
const (
	SessionValid   = "valid"
	SessionDead    = "dead"
	SessionUnknown = "unknown"
)

// SessionState maps the /api/v1/me probe to the guard states used by the
// checker: valid / dead / unknown (with the exact Python messages).
func (c *Client) SessionState(ctx context.Context, cookies []Cookie) (state, account string, karma int64, detail string) {
	code, data, err := c.probeMe(ctx, cookies)
	if err == notJSONErr {
		return SessionUnknown, "", 0, "non-JSON response (challenge page)"
	}
	if err != nil {
		return SessionUnknown, "", 0, fmt.Sprintf("request_failed: %v", err)
	}
	switch {
	case code == 200 && data != nil && !data.IsSuspended:
		return SessionValid, data.Name, data.LinkKarma + data.CommentKarma, ""
	case code == 200:
		if data != nil && data.IsSuspended {
			return SessionDead, "", 0, fmt.Sprintf("u/%s is suspended", data.Name)
		}
		return SessionDead, "", 0, "session cookies rejected (expired or revoked)"
	case code == 401 || code == 403:
		return SessionDead, "", 0, fmt.Sprintf("HTTP %d — session rejected", code)
	default:
		return SessionUnknown, "", 0, fmt.Sprintf("HTTP %d", code)
	}
}
