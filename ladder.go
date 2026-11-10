// Package main - source ladder: every user is fetched through an ordered
// list of strategies, so a failure escalates instead of becoming an error.
//
//  1. JSON  + session   fastest, exact fields, NSFW unlocked
//  2. HTML  + session   jsc solver fallback
//  3. JSON  anonymous   no session needed
//  4. HTML  anonymous   last HTTP option
//  5. Browser (rod)     100% authentic, only for the stragglers
//
// Routing rules (measured 2026-11-08):
//
//	throttled    -> wait for the IP budget, retry the SAME source
//	needs_session-> escalate to the next source immediately (no sleep)
//	notfound / suspended -> done
package main

import (
	"context"
	"fmt"
	"time"
)

// Source is one way to fetch a user.
type Source struct {
	Client  *Client
	Session *Session
	JSON    bool
	Label   string
	Lane    *Lane // optional: skipped when the lane is blocked
}

// LadderResult reports what happened while walking the ladder.
type LadderResult struct {
	Result    UserResult
	Source    string
	Attempts  int
	Escalated bool
}

// ladderAction says what the ladder does with a result.
type ladderAction int

const (
	ladderStop     ladderAction = iota // the result is final - the walk ends
	ladderEscalate                     // try the NEXT source
	ladderRetry                        // wait for the IP budget, retry THIS source
)

// ladderActionFor decides how the ladder reacts to a result.
//
// The important rule: a JSON 404 means "no such user" OR "deleted account" -
// only the profile page can tell them apart. So a JSON NOT_FOUND ESCALATES to
// the next source (the HTML one), which reports the deleted account as
// SUSPENDED/DELETED (banned.txt) instead of NOT_FOUND (not_found.txt). An
// HTML NOT_FOUND ends the walk - the account really does not exist.
//
// A missing account therefore costs exactly one extra request.
func ladderActionFor(res UserResult, srcJSON bool) ladderAction {
	switch classifyResult(res) {
	case ClassOK, ClassSuspended:
		return ladderStop
	case ClassNotFound:
		if srcJSON {
			return ladderEscalate
		}
		return ladderStop
	case ClassNeedsSession:
		return ladderEscalate
	case ClassThrottled:
		return ladderRetry
	}
	return ladderStop
}

// FetchWithLadder walks the sources until one of them answers. maxSources
// caps how many sources are tried per user (the ladder must not walk every
// lane when the IP is throttled - that would take minutes per user).
func FetchWithLadder(ctx context.Context, sources []Source, user string,
	timeout time.Duration, throttleWait time.Duration) LadderResult {
	return FetchWithLadderLimit(ctx, sources, user, timeout, throttleWait, 6)
}

// FetchWithLadderLimit is FetchWithLadder with an explicit source cap.
//
// The whole walk gets a PER-USER DEADLINE. Without it a blocked exit IP
// makes every ladder source retry with its own 429 backoff (5s+10s+20s)
// and a single user can eat minutes - the tail of a big run then crawls.
//
// The deadline is a WALL-CLOCK budget, not a context deadline. A context
// deadline would also cut the rate-limiter queue wait, and with many workers
// sharing one per-IP bucket that queue wait is exactly what eats the
// request's time - the request then died with "context deadline exceeded"
// even though the network was fine. The request carries its own timeout
// (Client.Timeout), so the two budgets no longer fight each other.
func FetchWithLadderLimit(ctx context.Context, sources []Source, user string,
	timeout time.Duration, throttleWait time.Duration, maxSources int) LadderResult {
	out := LadderResult{}
	var last UserResult
	tried := 0
	budget := timeout * 2
	if budget < 45*time.Second {
		budget = 45 * time.Second
	}
	walkDeadline := time.Now().Add(budget)
	for _, src := range sources {
		if maxSources > 0 && tried >= maxSources {
			break
		}
		// the walk budget: stop escalating once it is spent
		if time.Now().After(walkDeadline) {
			break
		}
		if src.Client == nil {
			continue
		}
		if src.Lane != nil && src.Lane.IsBlocked() {
			continue
		}
		// resolve the client AT REQUEST TIME: a lane may have rotated to a
		// new session/client since the sources were built. Dialing a stale
		// snapshot replayed dead cookies and silently ignored the rotation.
		srcClient := src.Client
		if src.Lane != nil {
			srcClient = src.Lane.ClientSnapshot()
			if srcClient == nil {
				continue
			}
		}
		tried++
		out.Attempts++
		out.Source = src.Label
		var res UserResult
		if src.JSON {
			res = srcClient.FetchUser(ctx, user, "", false)
		} else {
			res = srcClient.FetchUserHTML(ctx, user)
		}
		last = res
		switch ladderActionFor(res, src.JSON) {
		case ladderStop:
			debugf("ladder: STOP user=%s source=%s result=%s attempts=%d",
				user, src.Label, ladderVerdict(res), out.Attempts)
			out.Result = res
			return out
		case ladderEscalate:
			// a session did not help, or a JSON 404 needs the profile page
			// to tell "missing" from "deleted" -> try the next source
			debugf("ladder: ESCALATE user=%s source=%s reason=%s result=%s",
				user, src.Label, ladderReason(res, src.JSON), ladderVerdict(res))
			out.Escalated = true
			continue
		case ladderRetry:
			debugf("ladder: RETRY user=%s source=%s reason=%s result=%s",
				user, src.Label, ladderReason(res, src.JSON), ladderVerdict(res))
			// transient: wait for the IP budget and retry the same source
			if throttleWait <= 0 {
				throttleWait = 3 * time.Second
			}
			select {
			case <-ctx.Done():
				out.Result = last
				return out
			case <-time.After(throttleWait):
			}
			// retry this source once more before escalating (re-resolve the
			// lane's current client — the throttle wait may have seen a
			// session rotation)
			if src.Lane != nil {
				if c := src.Lane.ClientSnapshot(); c != nil {
					srcClient = c
				}
			}
			if src.JSON {
				res = srcClient.FetchUser(ctx, user, "", false)
			} else {
				res = srcClient.FetchUserHTML(ctx, user)
			}
			last = res
			switch ladderActionFor(res, src.JSON) {
			case ladderStop:
				debugf("ladder: STOP after retry user=%s source=%s result=%s attempts=%d",
					user, src.Label, ladderVerdict(res), out.Attempts)
				out.Result = res
				return out
			case ladderEscalate:
				debugf("ladder: ESCALATE after retry user=%s source=%s reason=%s",
					user, src.Label, ladderReason(res, src.JSON))
				out.Escalated = true
				continue
			}
		}
	}
	out.Result = last
	debugf("ladder: EXHAUSTED user=%s attempts=%d result=%s", user, out.Attempts, ladderVerdict(last))
	return out
}

// ladderVerdict renders a result as a short, readable verdict for the trace.
func ladderVerdict(res UserResult) string {
	switch {
	case res.OK:
		return "hit"
	case res.NotFound:
		return "not_found"
	case res.Deleted:
		return "deleted"
	case res.Banned:
		return "suspended"
	case res.Error != "":
		return res.Error
	}
	return "unknown"
}

// ladderReason explains WHY the ladder made its decision. This is the
// difference between "the account does not exist" and "the JSON endpoint said
// 404 but the profile page has to decide whether it is deleted".
func ladderReason(res UserResult, srcJSON bool) string {
	switch classifyResult(res) {
	case ClassNotFound:
		if srcJSON {
			return "json_404_needs_profile_page"
		}
		return "profile_page_confirms_missing"
	case ClassNeedsSession:
		return "session_did_not_help"
	case ClassThrottled:
		return "throttled"
	}
	return "final"
}

// BuildSessionSources creates the JSON+HTML source pair for one session.
func BuildSessionSources(c *Client, s *Session, proxy string) []Source {
	return []Source{
		{Client: c, Session: s, JSON: true, Label: fmt.Sprintf("json+session(%s)", s.Name)},
		{Client: c, Session: s, JSON: false, Label: fmt.Sprintf("html+session(%s)", s.Name)},
	}
}

// BuildAnonymousSources creates the JSON+HTML source pair without a session.
func BuildAnonymousSources(c *Client, proxy string) []Source {
	return []Source{
		{Client: c, JSON: true, Label: "json+anonymous"},
		{Client: c, JSON: false, Label: "html+anonymous"},
	}
}
