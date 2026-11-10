// Hardening — the browser fallback must be started EXACTLY ONCE, even when
// many workers hit the last ladder rung at the same moment.
package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestBrowserFallbackLazyInitIsSingleFlight exercises the race that used to
// launch several Chromes: 20 concurrent fallbacks, one start.
func TestBrowserFallbackLazyInitIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	prev := newBrowserFallback
	newBrowserFallback = func(dir string, headless bool, timeout time.Duration, pages, budget int) (*BrowserFallback, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window
		return nil, errors.New("no chrome in the test environment")
	}
	t.Cleanup(func() { newBrowserFallback = prev })

	m := &MultiChecker{cfg: MultiConfig{Timeout: 5 * time.Second}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := m.browserFetch(context.Background(), "someuser")
			if res.Error != "browser_unavailable" {
				t.Errorf("result = %+v, want browser_unavailable", res)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("Chrome was started %d times, want exactly 1", got)
	}
	if m.browserErr == nil {
		t.Fatal("the failed start was not remembered - every user would retry Chrome")
	}
}

// The single-session checker has the same lazy start and the same guard.
func TestCheckerBrowserFallbackLazyInitIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	prev := newBrowserFallback
	newBrowserFallback = func(dir string, headless bool, timeout time.Duration, pages, budget int) (*BrowserFallback, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return nil, errors.New("no chrome in the test environment")
	}
	t.Cleanup(func() { newBrowserFallback = prev })

	ch := &Checker{cfg: CheckConfig{Timeout: 5 * time.Second}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := ch.browserFetch(context.Background(), "someuser"); res.Error != "browser_unavailable" {
				t.Errorf("result = %+v, want browser_unavailable", res)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("Chrome was started %d times, want exactly 1", got)
	}
}
