// Package main - live status panel.
//
// This is the Go version of the Python script's rich.live panel: a bordered
// table that redraws itself in place while the run is going.
//
//	╭─ Live Status ─────────────────────────────╮
//	│ Status                    │ Value          │
//	├───────────────────────────┼────────────────┤
//	│ Total To Check            │ 3972           │
//	│ Checked                   │ 1184           │
//	│ Valid                     │ 683            │
//	│ Invalid (Banned/Suspended)│ 261            │
//	│ Failed (Connection/Timeout)│ 0             │
//	│ Checks per minute         │ 1184           │
//	╰───────────────────────────────────────────╯
//
// On a terminal it redraws in place (ANSI cursor-up + erase). When the
// output is redirected to a file it prints one plain status line every few
// seconds instead, so a log file stays readable.
package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// boxChars are the border glyphs. Windows consoles usually run a legacy
// code page and would print the Unicode box drawing characters as garbage,
// so the ASCII set is used when the terminal cannot render them (the same
// fallback the Python rich library does).
type boxChars struct {
	tl, tr, bl, br, h, v, ml, mr string
}

var unicodeBox = boxChars{"╭", "╮", "╰", "╯", "─", "│", "├", "┤"}
var asciiBox = boxChars{"+", "+", "+", "+", "-", "|", "+", "+"}

// LiveStatus is the in-place status panel.
//
// PERFORMANCE: the panel used to redraw on EVERY checked account - that is
// one full-screen repaint (ANSI cursor-up + erase + 10 lines) per user, which
// dominates the CPU when 80 workers finish an account every few
// milliseconds. The counters are now atomic (no mutex in the hot path) and
// the redraw is throttled to redrawInterval, so the panel refreshes several
// times per second no matter how fast the workers go.
type LiveStatus struct {
	enabled bool
	tty     bool
	started time.Time
	total   int64
	checked int64
	valid   int64
	invalid int64
	failed  int64
	blocked int64

	// provider returns the live rate-limit state for the extra panel rows.
	// It is a function so the panel does not need to know about the checker:
	// the checker passes a closure that reads its own AIMD / limiter state.
	provider func() []string

	mu        sync.Mutex
	lines     int
	lastDraw  time.Time
	lastFile  time.Time
	lastCount int64
}

// redrawInterval is the minimum gap between two panel repaints.
const redrawInterval = 250 * time.Millisecond

// liveActive is true while a panel is on screen. The per-user progress
// lines then stay quiet so they cannot fight with the redraw.
var liveActive atomic.Bool

// SetLiveActive marks the panel as visible (or not).
func SetLiveActive(on bool) { liveActive.Store(on) }

// LiveActive reports whether the panel is currently on screen.
func LiveActive() bool { return liveActive.Load() }

// isTerminal reports whether stdout is an interactive terminal.
func isTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// NewLiveStatus creates the panel for a run of `total` accounts.
func NewLiveStatus(total int64) *LiveStatus {
	return &LiveStatus{enabled: !Quiet(), tty: isTerminal(), started: time.Now(), total: total}
}

// Enabled reports whether the panel is active (it replaces the per-user
// progress lines).
func (l *LiveStatus) Enabled() bool { return l.enabled }

// SetProvider installs the callback that supplies the live rate-limit rows
// (blocked count, the current per-IP rate, the AIMD multiplier, the queue
// depth and the VPN rotation count).
//
// This is what makes a 200-500 thread run observable: without it a run that
// is quietly rate limited looks exactly like a run that is quietly slow.
func (l *LiveStatus) SetProvider(f func() []string) {
	l.mu.Lock()
	l.provider = f
	l.mu.Unlock()
}

// cpm is the checks-per-minute rate.
func (l *LiveStatus) cpm() int64 {
	el := time.Since(l.started).Minutes()
	if el <= 0 {
		return 0
	}
	return int64(float64(l.checked) / el)
}

// elapsed returns the run time so far.
func (l *LiveStatus) elapsed() time.Duration { return time.Since(l.started) }

// eta returns the estimated time to finish (0 when it cannot be estimated).
func (l *LiveStatus) eta() time.Duration {
	c := l.checked
	if c < 10 || l.total <= c {
		return 0
	}
	per := time.Since(l.started) / time.Duration(c)
	return per * time.Duration(l.total-c)
}

// Update refreshes the counters and redraws the panel (throttled).
func (l *LiveStatus) Update(checked, valid, invalid, failed int64) {
	// Atomic stores: the hot path never takes the panel mutex.
	atomic.StoreInt64(&l.checked, checked)
	atomic.StoreInt64(&l.valid, valid)
	atomic.StoreInt64(&l.invalid, invalid)
	atomic.StoreInt64(&l.failed, failed)

	if !l.tty {
		// redirected output: one plain status line every 2 seconds
		l.mu.Lock()
		due := time.Since(l.lastFile) >= 2*time.Second
		if due {
			l.lastFile = time.Now()
		}
		l.mu.Unlock()
		if due {
			// the redirected line carries the rate-limit state too - a run
			// that is quietly rate limited must be visible in the log too
			extra := ""
			if l.provider != nil {
				parts := l.provider()
				if len(parts) > 0 {
					extra = " | " + strings.Join(parts, " | ")
				}
			}
			fmt.Printf("... checked %d/%d | valid %d | invalid %d | failed %d | %d/min | elapsed %s | eta %s%s\n",
				l.checked, l.total, l.valid, l.invalid, l.failed, l.cpm(),
				formatDuration(l.elapsed()), formatDuration(l.eta()), extra)
		}
		return
	}

	// TTY: throttle the repaint. The throttle check and the draw happen
	// under ONE lock so two workers finishing at the same time can never
	// draw two frames on top of each other.
	l.mu.Lock()
	if time.Since(l.lastDraw) >= redrawInterval {
		l.lastDraw = time.Now()
		l.drawLocked()
	}
	l.mu.Unlock()
}

// Close finishes the panel (final frame + newline).
func (l *LiveStatus) Close() {
	if !l.tty {
		return
	}
	l.draw()
	fmt.Println()
}

// draw renders the panel in place.
//
// The cursor-up escape MUST be followed by a carriage return: the panel
// lines have different widths, so without "\r" the cursor keeps the column
// of the previous (longer) line and the next frame is drawn NEXT TO the
// previous one instead of over it.
func (l *LiveStatus) draw() {
	l.mu.Lock()
	l.drawLocked()
	l.mu.Unlock()
}

// drawLocked renders the panel; the caller must hold l.mu.
func (l *LiveStatus) drawLocked() {
	panel := l.panel()
	if l.lines > 0 {
		// move the cursor up over the previous frame and back to column 0
		fmt.Printf("\x1b[%dA\r", l.lines)
	}
	// erase from the cursor down, then draw the new frame
	fmt.Print("\x1b[J")
	fmt.Print(panel)
	l.lines = strings.Count(panel, "\n")
}

// formatDuration renders a duration as H:MM:SS (or M:SS when under an hour).
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "--:--:--"
	}
	s := int(d.Seconds())
	h := s / 3600
	m := (s % 3600) / 60
	sec := s % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d", m, sec)
}

// panel builds the bordered table.
func (l *LiveStatus) panel() string {
	box := asciiBox
	if consoleUTF8() {
		box = unicodeBox
	}
	rows := [][2]string{
		{"Total To Check", fmt.Sprint(l.total)},
		{"Checked", fmt.Sprint(l.checked)},
		{"Valid", fmt.Sprint(l.valid)},
		{"Invalid (Banned/Suspended)", fmt.Sprint(l.invalid)},
		{"Failed (Connection/Timeout)", fmt.Sprint(l.failed)},
		{"Checks per minute", fmt.Sprint(l.cpm())},
		{"Elapsed", formatDuration(l.elapsed())},
		{"ETA", formatDuration(l.eta())},
	}
	// the live rate-limit state (blocked count, rate, AIMD, queue, rotations)
	if l.provider != nil {
		for _, s := range l.provider() {
			if s == "" {
				continue
			}
			name, value, ok := strings.Cut(s, "=")
			if !ok {
				continue
			}
			rows = append(rows, [2]string{name, value})
		}
	}
	leftW, rightW := len("Status"), len("Value")
	for _, r := range rows {
		if len(r[0]) > leftW {
			leftW = len(r[0])
		}
		if len(r[1]) > rightW {
			rightW = len(r[1])
		}
	}
	inner := leftW + 3 + rightW
	title := " Live Status "
	var b strings.Builder
	b.WriteString(box.tl + box.h + title + strings.Repeat(box.h, inner-len(title)) + box.tr + "\n")
	b.WriteString(box.v + " " + pad("Status", leftW) + " " + box.v + " " + pad("Value", rightW) + " " + box.v + "\n")
	b.WriteString(box.ml + strings.Repeat(box.h, inner+2) + box.mr + "\n")
	for _, r := range rows {
		b.WriteString(box.v + " " + pad(r[0], leftW) + " " + box.v + " " + pad(r[1], rightW) + " " + box.v + "\n")
	}
	b.WriteString(box.bl + strings.Repeat(box.h, inner+2) + box.br)
	return b.String()
}

// pad left-aligns s in a field of width w.
func pad(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
