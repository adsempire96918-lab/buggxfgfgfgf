// Package main - end-of-run report.
//
// Every run produces a full accounting of the accounts it touched:
// total / checked / valid / invalid / failed, plus the speed, the VPN
// rotations and the karma-bucket breakdown. The report is printed to the
// console AND saved as <output>/run_report.txt so it can be inspected
// later or parsed by another tool.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// quietMode suppresses the per-user progress lines. With --quiet the run
// prints only the important events (VPN rotations, lane cooldowns) and the
// final RUN REPORT - no "[123/3972] HIT ..." spam.
var quietMode atomic.Bool

// SetQuiet turns the per-user progress output on or off.
func SetQuiet(on bool) { quietMode.Store(on) }

// Quiet reports whether the progress output is suppressed.
func Quiet() bool { return quietMode.Load() }

// progressMaxLinesPerSecond caps the per-user progress output. At 500 threads
// a line per result is ~1000 lines/second and the console - not the network -
// becomes the bottleneck (T-06).
const progressMaxLinesPerSecond = 20

var (
	// progressOut is a seam so the throttle can be measured without a console.
	progressOut io.Writer = os.Stdout

	progressMu       sync.Mutex
	progressWindow   int64 // unix second of the current window
	progressInWindow int
	progressPrinted  atomic.Int64
	progressDropped  atomic.Int64
)

// progressMustKeep reports whether a line may never be dropped: every HIT,
// every ERROR and every SUSPENDED is a real verdict the operator must see.
// The repetitive NOT_FOUND stream - the one that floods the console - is what
// gets throttled.
func progressMustKeep(msg string) bool {
	return strings.Contains(msg, " HIT ") ||
		strings.Contains(msg, " ERROR ") ||
		strings.Contains(msg, " SUSPENDED ")
}

// progressAllowed implements the 20-lines-per-second window.
func progressAllowed(now time.Time) bool {
	sec := now.Unix()
	progressMu.Lock()
	defer progressMu.Unlock()
	if sec != progressWindow {
		progressWindow = sec
		progressInWindow = 0
	}
	if progressInWindow >= progressMaxLinesPerSecond {
		return false
	}
	progressInWindow++
	return true
}

// progressf prints a per-user progress line unless --quiet is set or the
// live status panel is on screen (the panel replaces the per-user lines).
//
// HIT/ERROR/SUSPENDED lines are always printed; the repetitive NOT_FOUND lines
// are throttled to progressMaxLinesPerSecond and counted.
func progressf(format string, args ...any) {
	if quietMode.Load() || LiveActive() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if !progressMustKeep(msg) && !progressAllowed(time.Now()) {
		progressDropped.Add(1)
		return
	}
	progressPrinted.Add(1)
	if progressOut == nil {
		fmt.Print(msg)
		return
	}
	fmt.Fprint(progressOut, msg)
}

// ProgressDropped returns how many progress lines the throttle suppressed.
func ProgressDropped() int64 { return progressDropped.Load() }

// ProgressPrinted returns how many progress lines were actually written.
func ProgressPrinted() int64 { return progressPrinted.Load() }

// resetProgressThrottle clears the window and the counters (tests).
func resetProgressThrottle() {
	progressMu.Lock()
	progressWindow = 0
	progressInWindow = 0
	progressMu.Unlock()
	progressPrinted.Store(0)
	progressDropped.Store(0)
}

// RunReport is the complete accounting of one run.
type RunReport struct {
	Mode         string
	InputTotal   int64 // accounts found in the input file
	Checked      int64 // accounts actually looked up this run
	Skipped      int64 // already saved earlier (resume)
	Rejected     int64 // input lines rejected by the credential filter
	Valid        int64 // karma found
	NotFound     int64 // account does not exist
	Suspended    int64 // account suspended
	Deleted      int64 // account deleted
	AgeGate      int64 // NSFW age gate (login_required)
	Challenge    int64 // Reddit's jsc challenge did not clear
	Blocked      int64 // the exit IP was blocked
	Failed       int64 // every other failure
	Duration     time.Duration
	VPNRotations int64
	Sessions     int // sessions in the pool
	SessionsOK   int // sessions that passed the health check
	Buckets      []int64
	BucketNames  []string
	OutputDir    string
	StoppedEarly bool
	StartedAt    time.Time
}

// pct returns the percentage of checked accounts (0 when nothing was checked).
func (r *RunReport) pct(n int64) string {
	if r.Checked <= 0 {
		return "  -"
	}
	return fmt.Sprintf("%5.1f%%", float64(n)*100/float64(r.Checked))
}

// Invalid is the number of accounts that exist but cannot be used
// (suspended or deleted).
func (r *RunReport) Invalid() int64 { return r.Suspended + r.Deleted }

// Unusable is every account that produced no karma value.
func (r *RunReport) Unusable() int64 { return r.Invalid() + r.NotFound }

// Accounted is checked + skipped + rejected (every input line has a verdict).
func (r *RunReport) Accounted() int64 { return r.Checked + r.Skipped + r.Rejected }

// Render builds the human-readable report.
func (r *RunReport) Render() string {
	var b strings.Builder
	line := strings.Repeat("=", 62)
	thin := strings.Repeat("-", 62)
	b.WriteString("\n" + line + "\n")
	b.WriteString(fmt.Sprintf("  RUN REPORT   %s\n", r.StartedAt.Format("2006-01-02 15:04:05")))
	b.WriteString(line + "\n")
	b.WriteString(fmt.Sprintf("  Mode              : %s\n", r.Mode))
	b.WriteString(fmt.Sprintf("  Accounts in input : %d\n", r.InputTotal))
	b.WriteString(fmt.Sprintf("  Checked this run  : %d\n", r.Checked))
	b.WriteString(fmt.Sprintf("  Skipped (resume)  : %d\n", r.Skipped))
	if r.Rejected > 0 {
		b.WriteString(fmt.Sprintf("  Rejected (input)  : %d\n", r.Rejected))
	}
	b.WriteString(thin + "\n")
	b.WriteString(fmt.Sprintf("  VALID             : %-6d %s   karma found\n", r.Valid, r.pct(r.Valid)))
	b.WriteString(fmt.Sprintf("  INVALID           : %-6d %s   %d not found + %d suspended + %d deleted\n",
		r.Unusable(), r.pct(r.Unusable()), r.NotFound, r.Suspended, r.Deleted))
	b.WriteString(fmt.Sprintf("  FAILED / ERROR    : %-6d %s\n", r.Failed+r.Blocked, r.pct(r.Failed+r.Blocked)))
	b.WriteString(thin + "\n")
	b.WriteString(fmt.Sprintf("  Accounted for     : %d / %d  (%.1f%% of the input)\n",
		r.Accounted(), r.InputTotal, ratio(r.Accounted(), r.InputTotal)))
	if r.Checked > 0 && r.Duration > 0 {
		perMin := float64(r.Checked) / r.Duration.Minutes()
		b.WriteString(fmt.Sprintf("  Speed             : %.0f accounts/minute  (%.1fs)\n", perMin, r.Duration.Seconds()))
	} else {
		b.WriteString(fmt.Sprintf("  Time              : %.1fs\n", r.Duration.Seconds()))
	}
	b.WriteString(fmt.Sprintf("  VPN rotations     : %d\n", r.VPNRotations))
	if r.Sessions > 0 {
		b.WriteString(fmt.Sprintf("  Sessions          : %d / %d usable\n", r.SessionsOK, r.Sessions))
	}
	if r.Failed+r.Blocked+r.AgeGate+r.Challenge > 0 {
		b.WriteString(thin + "\n")
		b.WriteString("  Failure breakdown:\n")
		b.WriteString(fmt.Sprintf("    blocked (exit IP)   : %d\n", r.Blocked))
		b.WriteString(fmt.Sprintf("    NSFW age gate       : %d\n", r.AgeGate))
		b.WriteString(fmt.Sprintf("    jsc challenge       : %d\n", r.Challenge))
		b.WriteString(fmt.Sprintf("    other               : %d\n", r.Failed))
	}
	if len(r.Buckets) > 0 {
		b.WriteString(thin + "\n")
		b.WriteString("  Karma buckets (this run):\n")
		for i, n := range r.Buckets {
			name := ""
			if i < len(r.BucketNames) {
				name = r.BucketNames[i]
			}
			b.WriteString(fmt.Sprintf("    %-18s: %d\n", name, n))
		}
	}
	if r.StoppedEarly {
		b.WriteString(thin + "\n")
		b.WriteString("  STOPPED EARLY - fix the cause and run again (progress is saved).\n")
	}
	b.WriteString(line + "\n")
	b.WriteString(fmt.Sprintf("  Saved to: %s\n", r.OutputDir))
	return b.String()
}

// ratio returns a/b as a percentage (0 when b is 0).
func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

// Save writes the report to <dir>/run_report.txt.
func (r *RunReport) Save(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "run_report.txt")
	return os.WriteFile(path, []byte(r.Render()), 0644)
}

// Print shows the report on the console and saves it next to the results.
func (r *RunReport) Print() {
	fmt.Print(r.Render())
	if err := r.Save(r.OutputDir); err != nil {
		fmt.Printf("  (report could not be saved: %v)\n", err)
		return
	}
	fmt.Printf("  Report saved: %s\n", filepath.Join(r.OutputDir, "run_report.txt"))
}
