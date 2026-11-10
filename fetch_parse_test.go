// Package main - HTML profile parsing regression tests.
//
// These cover the three bugs that made the startup preflight stall with
// "Preflight: <user> throttled (blocked) - retry 1/3 ..." on a healthy IP:
//
//  1. an unsolved jsc challenge was reported as "blocked" (an IP rate limit)
//  2. a real profile with zero karma was reported as "blocked"
//  3. a genuine Reddit block page was not detected as a block
package main

import (
	"strings"
	"testing"
)

// A jsc challenge page must be reported as "challenge", never as "blocked":
// the challenge is solvable and a fresh exit IP clears it.
func TestParseProfileHTMLChallengeIsNotBlocked(t *testing.T) {
	html := `<html><body><form><input name="jsc_token" value="abc123"/>` +
		`<script>e+e)("deadbeefdeadbeef")</script></form></body></html>`
	res := parseProfileHTML(html, "someone")
	if res.OK {
		t.Fatal("challenge page parsed as a successful profile")
	}
	if res.Error != "challenge" {
		t.Fatalf("error = %q, want %q", res.Error, "challenge")
	}
	if classifyResult(res) != ClassThrottled {
		t.Fatalf("class = %q, want %q", classifyResult(res), ClassThrottled)
	}
}

// A real profile with 0 post + 0 comment karma has no karma-number element.
// It must be a HIT with 0 karma, not a false "blocked".
func TestParseProfileHTMLZeroKarmaIsAHit(t *testing.T) {
	html := `<html><body><h1><span data-testid="profile-prefixed-name"><span>u/newbie</span></span></h1>` +
		`<div>0 post karma, 0 comment karma</div>` +
		`<time data-testid="cake-day" datetime="2020-01-02T00:00:00+00:00">January 2, 2020</time>` +
		`</body></html>`
	res := parseProfileHTML(html, "newbie")
	if !res.OK {
		t.Fatalf("zero-karma profile not parsed: ok=%v err=%q detail=%q", res.OK, res.Error, res.Detail)
	}
	if res.TotalKarma != 0 {
		t.Fatalf("totalKarma = %d, want 0", res.TotalKarma)
	}
	if res.Username != "newbie" {
		t.Fatalf("username = %q, want newbie", res.Username)
	}
}

// A normal profile with karma must still parse exactly as before.
func TestParseProfileHTMLNormalProfile(t *testing.T) {
	html := `<html><body><h1><span data-testid="profile-prefixed-name"><span>u/spez</span></span></h1>` +
		`<div data-testid="karma-number">941,389</div>` +
		`<div>184,766 post karma, 756,623 comment karma</div>` +
		`<time data-testid="cake-day" datetime="2005-06-06T00:00:00+00:00">June 6, 2005</time>` +
		`</body></html>`
	res := parseProfileHTML(html, "spez")
	if !res.OK {
		t.Fatalf("profile not parsed: err=%q detail=%q", res.Error, res.Detail)
	}
	if res.TotalKarma != 941389 {
		t.Fatalf("totalKarma = %d, want 941389", res.TotalKarma)
	}
	if res.LinkKarma != 184766 || res.CommentKarma != 756623 {
		t.Fatalf("link/comment = %d/%d, want 184766/756623", res.LinkKarma, res.CommentKarma)
	}
	if res.Created != "2005-06-06" {
		t.Fatalf("created = %q, want 2005-06-06", res.Created)
	}
}

// A genuine Reddit block page must be detected as a block (IP-level).
func TestIsBlockPageDetectsRedditBlock(t *testing.T) {
	html := `<html><head><title>Blocked</title></head><body>` +
		`<p>Whoa there, pardner! You've been blocked by network security.</p></body></html>`
	if !isBlockPage(html) {
		t.Fatal("block page not detected")
	}
	if isBlockPage(`<html><body><div data-testid="karma-number">10</div></body></html>`) {
		t.Fatal("normal profile flagged as a block page")
	}
}

// A page with no profile component at all is NOT a profile: Reddit serves
// the home feed for an account that does not exist. It must be NOT_FOUND,
// never "blocked" (that stalled the startup check on a healthy IP).
func TestParseProfileHTMLMissingAccountIsNotFound(t *testing.T) {
	res := parseProfileHTML(`<html><body><div id="canonical-url-updater"></div></body></html>`, "ghost")
	if res.OK {
		t.Fatal("missing account parsed as a hit")
	}
	if !res.NotFound {
		t.Fatalf("missing account not reported as NOT_FOUND: err=%q detail=%q", res.Error, res.Detail)
	}
}

// A page that DOES carry the profile component but no karma is a real
// profile whose karma did not render - that stays an IP-level block.
func TestParseProfileHTMLProfileWithoutKarmaIsBlocked(t *testing.T) {
	html := `<html><body><shreddit-profile><span data-testid="profile-prefixed-name">` +
		`<span>u/someone</span></span></shreddit-profile></body></html>`
	res := parseProfileHTML(html, "someone")
	if res.OK {
		t.Fatal("profile without karma parsed as a hit")
	}
	if res.NotFound {
		t.Fatal("real profile reported as NOT_FOUND")
	}
	if res.Error != "blocked" {
		t.Fatalf("error = %q, want blocked", res.Error)
	}
}

// The challenge solver must accept the 302 that Reddit uses to CONFIRM a
// correct solution (it sets the solved-session cookie and returns an empty
// body). Rejecting that 302 was the reason every solved profile was
// reported as "blocked".
func TestIsRedirectCodeAcceptsChallengeAcceptance(t *testing.T) {
	for _, code := range []int{301, 302, 303, 307, 308} {
		if !isRedirectCode(code) {
			t.Errorf("isRedirectCode(%d) = false, want true", code)
		}
	}
	for _, code := range []int{200, 403, 404, 429, 500} {
		if isRedirectCode(code) {
			t.Errorf("isRedirectCode(%d) = true, want false", code)
		}
	}
}

// The profile-page marker must match a real profile and not the home feed.
func TestProfilePageMarker(t *testing.T) {
	real := `<html><body><shreddit-profile><span data-testid="profile-prefixed-name">` +
		`<span>u/spez</span></span></shreddit-profile></body></html>`
	if !profilePageRe.MatchString(real) {
		t.Fatal("real profile not matched")
	}
	home := `<html><body><div class="feed"></div></body></html>`
	if profilePageRe.MatchString(home) {
		t.Fatal("home feed matched as a profile page")
	}
}

// The age gate must stay "login_required" (it is NOT an IP problem).
func TestParseProfileHTMLAgeGateIsLoginRequired(t *testing.T) {
	html := `<html><body><p>Log in to confirm your age</p><p>mature content</p></body></html>`
	res := parseProfileHTML(html, "nsfwuser")
	if res.Error != "login_required" {
		t.Fatalf("error = %q, want login_required", res.Error)
	}
	if classifyResult(res) != ClassNeedsSession {
		t.Fatalf("class = %q, want %q", classifyResult(res), ClassNeedsSession)
	}
}

// The error taxonomy: every IP-level outcome maps to ClassThrottled so the
// VPN rotation reacts to it, and user-level outcomes do not.
func TestClassifyResultTaxonomy(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"blocked", ClassThrottled},
		{"forbidden", ClassThrottled},
		{"http_403", ClassThrottled},
		{"rate_limited", ClassThrottled},
		{"challenge", ClassThrottled},
		{"request_failed", ClassThrottled},
		{"timeout", ClassThrottled},
		{"login_required", ClassNeedsSession},
		{"parse_fail", ClassReal},
	}
	for _, c := range cases {
		got := classifyResult(errResult(c.err, ""))
		if got != c.want {
			t.Errorf("classifyResult(%q) = %q, want %q", c.err, got, c.want)
		}
	}
	// a successful result and a missing user are neither throttled nor real
	if got := classifyResult(UserResult{OK: true}); got != ClassOK {
		t.Errorf("classifyResult(ok) = %q, want %q", got, ClassOK)
	}
	if got := classifyResult(UserResult{NotFound: true}); got != ClassNotFound {
		t.Errorf("classifyResult(notfound) = %q, want %q", got, ClassNotFound)
	}
}

// The challenge page must not be mistaken for a profile by the block-page
// detector either (it is a challenge, not a block).
func TestChallengePageIsNotABlockPage(t *testing.T) {
	html := `<html><body><input name="jsc_token" value="deadbeef"/></body></html>`
	if isBlockPage(html) {
		t.Fatal("challenge page flagged as a block page")
	}
	if !strings.Contains(html, "jsc_token") {
		t.Fatal("test fixture is broken")
	}
}
