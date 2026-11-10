// Package main — userkarmacheck-go core types.
// Same field semantics as the Python version (reddit_fetch.py) so output
// files stay byte-compatible.
package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UserResult mirrors Python's fetch_user() dict.
type UserResult struct {
	OK           bool   `json:"ok"`
	NotFound     bool   `json:"notfound,omitempty"`
	Banned       bool   `json:"banned,omitempty"`
	Deleted      bool   `json:"deleted,omitempty"`
	Error        string `json:"error,omitempty"`
	Detail       string `json:"detail,omitempty"`
	Username     string `json:"username,omitempty"`
	TotalKarma   int64  `json:"totalKarma"`
	LinkKarma    int64  `json:"linkKarma"`
	CommentKarma int64  `json:"commentKarma"`
	AwarderKarma int64  `json:"awarderKarma"`
	AwardeeKarma int64  `json:"awardeeKarma"`
	Coins        int64  `json:"coins"`
	Premium      bool   `json:"premium"`
	Verified     bool   `json:"verified"`
	Over18       bool   `json:"over18"`
	AgeDays      int64  `json:"ageDays"`
	Created      string `json:"created"`
	Followers    int64  `json:"followers"`
	Source       string `json:"source,omitempty"`
}

func errResult(e, detail string) UserResult {
	return UserResult{OK: false, Error: e, Detail: detail}
}

var (
	profileURLRe = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.|old\.|new\.|np\.)?reddit\.com/(?:u|user)/([^/?#\s]+)`)
	prefixRe     = regexp.MustCompile(`(?i)^/?(?:u|user)/`)
)

// NormalizeUsername accepts name, name:pass, u/name, /user/name, full URL.
func NormalizeUsername(raw string) string {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `"'`))
	if s == "" {
		return ""
	}
	if m := profileURLRe.FindStringSubmatch(s); m != nil {
		return strings.Trim(m[1], "/")
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	s = prefixRe.ReplaceAllString(s, "")
	s = strings.SplitN(s, "?", 2)[0]
	s = strings.SplitN(s, "#", 2)[0]
	s = strings.SplitN(s, "/", 2)[0]
	return strings.Trim(strings.TrimSpace(s), "/")
}

// ParseProxy converts 'host:port' or 'host:port:user:pass' to http proxy url.
func ParseProxy(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	parts := strings.Split(line, ":")
	if len(parts) == 4 {
		return fmt.Sprintf("http://%s:%s@%s:%s", parts[2], parts[3], parts[0], parts[1])
	}
	if len(parts) == 2 {
		return fmt.Sprintf("http://%s:%s", parts[0], parts[1])
	}
	return ""
}

// ParseIntStr tolerates "1,234" / "1.2k" style numbers.
func parseNum(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "k") {
		if f, err := strconv.ParseFloat(strings.TrimSuffix(s, "k"), 64); err == nil {
			return int64(f * 1000)
		}
		return 0
	}
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSuffix(s, ".")
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// SortedKeys keeps header writes in a stable order (maps are random).
func sortedKeys(h map[string]string) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isoToEpoch parses an ISO-8601 date (or 2006-01-02) to unix seconds.
func isoToEpoch(value string) int64 {
	s := strings.TrimSpace(value)
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}
