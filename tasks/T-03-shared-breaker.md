# T-03 — Shared per-IP circuit breaker (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 45 min
**Plan card:** `EXECUTION_PLAN.md` → T-03

## Problem
The breaker was per LANE. When the exit IP got blocked, all 500 lanes ran
their own cooldown and fired their own half-open probe — 500 simultaneous
retries is a textbook bot pattern and it deepened the block.

## Files in scope (nothing else may be touched)
| File | Change |
|---|---|
| `breaker.go` | `BreakerRegistry` (one breaker per exit IP) + channel-based half-open probe (no 200 ms polling) |
| `resilient.go` | `NewGateWithBreaker`; `Report`/`Admit` nil-breaker safe |
| `lanes.go` | `buildLanes` takes the breaker from the registry; `LaneManager.breakers`; `resetAll` resets the registry once |
| `breaker_shared_test.go` | **NEW** regression tests (scope widened here, before the work) |

## What was done
1. `BreakerRegistry.For(proxy)` returns the SAME `*CircuitBreaker` for every
   lane behind the same exit IP (`breakerKey("")` = `direct`).
2. Half-open is a single-flight: after the cooldown exactly one caller becomes
   the probe (`beginProbeLocked`), every other caller blocks on `probeDone`
   (ctx-cancellable). A blocked probe re-opens the circuit, a clean probe
   closes it and releases the waiters. The old `time.Sleep(200ms)` re-check
   loop is gone.
3. The per-session health score (`pool.go`) is untouched — it is a different
   signal and must not be merged into the IP breaker.
4. VPN rotation resets the registry once (`resetAll`) instead of N lanes
   resetting the same object.

## Verification
```
go test -count=1 -run "TestSharedBreaker|TestLanesShareOneBreaker" -v .
--- PASS: TestSharedBreakerOpensOnceForAllLanes (0.13s)
--- PASS: TestSharedBreakerFailedProbeReopensForAll (0.44s)
--- PASS: TestLanesShareOneBreakerPerExitIP (0.00s)
```

Full gate (`gofmt -l .`, `go vet .`, `go build`, `go test -count=1 ./...`):
clean. Only the pre-existing Windows-only `TestMockCtlEndToEnd` fails on Linux
(it executes a `.bat`), unchanged from the baseline.

## Acceptance
- [x] 100 lanes on one IP share one breaker state
- [x] after the cooldown only one request is let through
- [x] `go test -count=1 ./...` passes (same single platform failure as baseline)
