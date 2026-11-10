# T-06 — Progress-line throttle (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 20 min
**Plan card:** `EXECUTION_PLAN.md` → T-06

## Problem
`progressf` printed one line per result when the live panel is off. At 500
threads that is ~1000 lines/second and the console (not the network) becomes
the bottleneck.

## Files in scope (widened before the work)
| File | Change |
|---|---|
| `report.go` | 20-lines/second window, verdict-aware classification, counters, `progressOut` seam |
| `lanes.go` | ONE summary line reporting the suppressed count (the plan asks for the counter to reach the final summary) |
| `progress_throttle_test.go` | **NEW** regression tests |

## Rule
- ` HIT `, ` ERROR ` and ` SUSPENDED ` lines are **never** dropped — they are
  the verdicts an operator must see.
- Everything else (the repetitive `NOT_FOUND` stream, "All lanes blocked"
  chatter) is limited to `progressMaxLinesPerSecond = 20`.
- Dropped lines are counted (`ProgressDropped()`), printed lines are counted
  (`ProgressPrinted()`), and the run summary prints
  `Progress lines suppressed: N (output throttled to 20/s; every HIT and ERROR is kept)`.

## Verification
```
go test -count=1 -run "TestProgressThrottle" -v .
--- PASS: TestProgressThrottle (0.00s)
--- PASS: TestProgressThrottleKeepsLowVolume (0.00s)
```
Full gate clean apart from the pre-existing Windows-only `TestMockCtlEndToEnd`.

## Acceptance
- [x] 1000 calls produce at most 20 lines in one second
- [x] No HIT or ERROR line is ever dropped
