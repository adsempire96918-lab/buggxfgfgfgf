// T-06 — the progress stream is throttled to 20 lines/second, but a HIT and
// an ERROR can never be dropped.
package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// safeBuffer is a bytes.Buffer usable as the progress writer.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func withProgressBuffer(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	prevOut, prevQuiet := progressOut, Quiet()
	progressOut = buf
	SetQuiet(false)
	resetProgressThrottle()
	t.Cleanup(func() {
		progressOut = prevOut
		SetQuiet(prevQuiet)
		resetProgressThrottle()
	})
	return buf
}

// TestProgressThrottle is the acceptance test of T-06: 1000 NOT_FOUND lines in
// one second produce at most ~20 lines, while HIT and ERROR always land.
func TestProgressThrottle(t *testing.T) {
	buf := withProgressBuffer(t)

	// 1000 repetitive NOT_FOUND lines inside one window
	for i := 0; i < 1000; i++ {
		progressf("[%d/1000] NOT_FOUND user%d\n", i+1, i)
	}
	if got := ProgressPrinted(); got > progressMaxLinesPerSecond {
		t.Fatalf("%d NOT_FOUND lines printed in one second, want <= %d", got, progressMaxLinesPerSecond)
	}
	if got := ProgressDropped(); got < 900 {
		t.Fatalf("dropped = %d, want ~980", got)
	}
	if n := strings.Count(buf.String(), "NOT_FOUND"); n > progressMaxLinesPerSecond {
		t.Fatalf("console received %d NOT_FOUND lines, want <= %d", n, progressMaxLinesPerSecond)
	}

	// HIT / ERROR / SUSPENDED must survive the same saturated window
	resetProgressThrottle()
	for i := 0; i < progressMaxLinesPerSecond+5; i++ {
		progressf("[%d/50] NOT_FOUND filler%d\n", i+1, i)
	}
	for i := 0; i < 50; i++ {
		progressf("[%d/50] HIT hit%d - Karma: %d | Link: 1 | Comment: 2 | Age: 3d | Premium: false\n", i+1, i, i)
		progressf("[%d/50] ERROR err%d - request_failed\n", i+1, i)
		progressf("[%d/50] SUSPENDED sus%d\n", i+1, i)
	}
	out := buf.String()
	if n := strings.Count(out, " HIT "); n != 50 {
		t.Fatalf("%d HIT lines reached the console, want 50", n)
	}
	if n := strings.Count(out, " ERROR "); n != 50 {
		t.Fatalf("%d ERROR lines reached the console, want 50", n)
	}
	if n := strings.Count(out, " SUSPENDED "); n != 50 {
		t.Fatalf("%d SUSPENDED lines reached the console, want 50", n)
	}
	if got := ProgressDropped(); got < 5 {
		t.Fatalf("dropped = %d, want the filler NOT_FOUND lines dropped", got)
	}
}

// The throttle must not kick in at low volume.
func TestProgressThrottleKeepsLowVolume(t *testing.T) {
	buf := withProgressBuffer(t)
	for i := 0; i < 5; i++ {
		progressf("[%d/5] NOT_FOUND u%d\n", i+1, i)
	}
	if got := strings.Count(buf.String(), "NOT_FOUND"); got != 5 {
		t.Fatalf("%d lines printed, want all 5", got)
	}
	if ProgressDropped() != 0 {
		t.Fatalf("dropped = %d at low volume, want 0", ProgressDropped())
	}
}
