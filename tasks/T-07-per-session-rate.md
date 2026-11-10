# T-07 — Per-session adaptive rate (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 45 min
**Plan card:** `EXECUTION_PLAN.md` → T-07

## Problem
Every session got exactly the same `--rate-per-session`. Some accounts
tolerate more, some less — the health score existed but nothing used it.

## Files in scope
| File | Change |
|---|---|
| `pool.go` | `Session.baseRate`, `sessionRateFactor`, `EffectiveRate`/`BaseRate`/`SetBaseRate`, score re-applied in `Report`/`ClearPenalty` |
| `session_rate_test.go` | **NEW** regression tests |
| `lanes.go` | *(no change needed — lanes already take `s.Bucket` through `gateForSession`, so the scaled bucket is used automatically)* |

## Rule
`rate = baseRate × factor(score)`, clamped to the `0.5x … 1.5x` band:

| score | factor |
|---|---|
| 10 (clean streak) | 1.50x |
| 5 | 1.25x |
| 0 (neutral / after a VPN reset) | 1.00x |
| −1 | 0.83x |
| −3 (quarantined) | 0.50x |

Only the session's own bucket moves. The shared per-IP bucket sits in front of
it (`Gate.IPBucket`) and still caps the total for the exit IP.

## Verification
```
go test -count=1 -run "TestSessionAdaptiveRate|TestSessionRateFactorBand" -v .
--- PASS: TestSessionAdaptiveRate (0.00s)
--- PASS: TestSessionRateFactorBand (0.00s)
```
Full gate clean apart from the pre-existing Windows-only `TestMockCtlEndToEnd`.

## Acceptance
- [x] A clean session's bucket rate (108/min) is higher than a bad one's (36/min) at base 72
- [x] The per-IP bucket still caps the total (separate object, rate unchanged)
- [x] One session's rate change never affects another session
