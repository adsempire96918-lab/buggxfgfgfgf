// T-05 — one jsc challenge wave costs ONE solve per exit IP; the waiting
// lanes reuse the solved session and fetch their own page.
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// TestJSCSingleFlight is the acceptance test of T-05. The fake solver counts
// the solves; the fake cookie seams record what reaches the waiting lanes.
func TestJSCSingleFlight(t *testing.T) {
	const lanes = 25
	s := newJSCSolver()

	var solves atomic.Int32
	var imports atomic.Int32
	var refetches atomic.Int32
	gate := make(chan struct{})
	solvedCookies := []*fhttp.Cookie{{Name: "loid", Value: "solved-session"}}

	s.solveFn = func(ctx context.Context, c *Client, reqURL, html string, rounds int, extra map[string]string) string {
		solves.Add(1)
		<-gate // stay in flight until every lane has joined the call
		return "solved-page:" + reqURL
	}
	s.exportFn = func(c *Client) []*fhttp.Cookie { return solvedCookies }
	s.importFn = func(c *Client, cks []*fhttp.Cookie) {
		if len(cks) == 1 && cks[0].Name == "loid" && cks[0].Value == "solved-session" {
			imports.Add(1)
		}
	}
	s.refetchFn = func(ctx context.Context, c *Client, reqURL string, extra map[string]string) string {
		refetches.Add(1)
		return "own-page:" + reqURL
	}

	got := make([]string, lanes)
	var wg sync.WaitGroup
	for i := 0; i < lanes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reqURL := fmt.Sprintf("https://www.reddit.com/user/user%02d/", i)
			// every lane has its own client/jar (that is the real topology)
			got[i] = s.solve(context.Background(), &Client{}, reqURL, "<form name=\"jsc_token\">1+1</form>", 4, nil)
		}(i)
	}

	// wait until the leader is in flight, then let it finish
	deadline := time.Now().Add(2 * time.Second)
	for solves.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(75 * time.Millisecond) // let the waiters join the same call
	close(gate)
	wg.Wait()

	if n := solves.Load(); n != 1 {
		t.Fatalf("%d challenge solves for one wave, want exactly 1", n)
	}
	if n := imports.Load(); n != lanes-1 {
		t.Fatalf("%d lanes imported the solved cookie, want %d", n, lanes-1)
	}
	if n := refetches.Load(); n != lanes-1 {
		t.Fatalf("%d lanes refetched their own page, want %d", n, lanes-1)
	}
	for i, page := range got {
		// leader -> its own solved page, waiter -> its own refetched page;
		// either way it must carry THIS lane's url and never another lane's
		if !strings.Contains(page, fmt.Sprintf("user%02d/", i)) {
			t.Fatalf("lane %d got %q - a lane must never receive another lane's page", i, page)
		}
	}

	// the guard must be released: a new wave solves again
	if page := s.solve(context.Background(), &Client{}, "https://www.reddit.com/user/x/", "<form name=\"jsc_token\">2+2</form>", 4, nil); page == "" {
		t.Fatal("the second wave returned no page (the guard was not released)")
	}
	if n := solves.Load(); n != 2 {
		t.Fatalf("solves = %d after a second wave, want 2 (the guard was not released)", n)
	}
	if len(s.inflight) != 0 {
		t.Fatalf("inflight = %d, want 0", len(s.inflight))
	}
}

// A failed solve must not leak the guard and must not hand a page to anybody.
func TestJSCSingleFlightReleasesOnFailure(t *testing.T) {
	s := newJSCSolver()
	var solves atomic.Int32
	s.solveFn = func(ctx context.Context, c *Client, reqURL, html string, rounds int, extra map[string]string) string {
		solves.Add(1)
		return "" // solve failed (blocked / timeout)
	}
	s.importFn = func(c *Client, cks []*fhttp.Cookie) { t.Error("a failed solve imported cookies") }
	s.refetchFn = func(ctx context.Context, c *Client, reqURL string, extra map[string]string) string {
		t.Error("a failed solve triggered a refetch")
		return ""
	}

	if page := s.solve(context.Background(), &Client{}, "https://www.reddit.com/user/a/", "challenge", 4, nil); page != "" {
		t.Fatalf("page = %q, want empty", page)
	}
	if len(s.inflight) != 0 {
		t.Fatalf("inflight = %d after a failure, want 0", len(s.inflight))
	}
	// the next call must be able to solve again
	_ = s.solve(context.Background(), &Client{}, "https://www.reddit.com/user/a/", "challenge", 4, nil)
	if n := solves.Load(); n != 2 {
		t.Fatalf("solves = %d, want 2", n)
	}
}

// A cancelled waiter must return instead of blocking forever on the leader.
func TestJSCSingleFlightWaiterHonoursContext(t *testing.T) {
	s := newJSCSolver()
	gate := make(chan struct{})
	started := make(chan struct{})
	s.solveFn = func(ctx context.Context, c *Client, reqURL, html string, rounds int, extra map[string]string) string {
		close(started)
		<-gate
		return "page"
	}
	go func() { _ = s.solve(context.Background(), &Client{}, "https://www.reddit.com/user/lead/", "c", 4, nil) }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if page := s.solve(ctx, &Client{}, "https://www.reddit.com/user/waiting/", "c", 4, nil); page != "" {
		t.Fatalf("cancelled waiter returned %q, want empty", page)
	}
	close(gate)
}
