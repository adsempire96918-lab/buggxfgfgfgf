// Package main - error taxonomy: every fetch result is classified into a
// routing class so the runner can decide between "retry the same source",
// "escalate to another source" and "give up".
//
// The error CODES stay exactly as the Python project writes them (blocked /
// login_required / ...) so the output files remain byte-compatible.
package main

import "strings"

// Routing classes.
const (
	ClassOK           = "ok"
	ClassNotFound     = "notfound"
	ClassSuspended    = "suspended"
	ClassThrottled    = "throttled"     // transient block / rate limit / network
	ClassNeedsSession = "needs_session" // age gate - a session would unlock it
	ClassReal         = "real"          // genuine failure
)

// classifyResult maps a fetch result to its routing class.
func classifyResult(res UserResult) string {
	switch {
	case res.OK:
		return ClassOK
	case res.NotFound:
		return ClassNotFound
	case res.Banned:
		return ClassSuspended
	case res.Error == "login_required":
		return ClassNeedsSession
	case res.Error == "blocked", res.Error == "forbidden", res.Error == "http_403":
		return ClassThrottled
	case res.Error == "rate_limited":
		return ClassThrottled
	case res.Error == "challenge":
		// Reddit's jsc anti-bot gate: solvable, and a fresh IP clears it -
		// so it belongs to the IP-level bucket (the VPN rotation fixes it).
		return ClassThrottled
	case res.Error == "request_failed", res.Error == "timeout":
		return ClassThrottled
	}
	return ClassReal
}

// isBlockPage detects Reddit's block / bot-gate page (190KB style pages).
func isBlockPage(body string) bool {
	low := strings.ToLower(body)
	for _, m := range []string{
		"blocked by network security",
		"whoa there, pardner",
		"you have been blocked",
		"<title>blocked</title>",
		"network security",
		"whoa there",
	} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}
