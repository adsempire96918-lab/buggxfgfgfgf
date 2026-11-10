# T-04 — TokenBucket: exact wait, no thundering herd (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 30 min
**Plan card:** `EXECUTION_PLAN.md` → T-04

## Problem
`Wait` capped the sleep at 100 ms and re-checked under the lock. With 500
waiters that is up to ~5000 wakeups per second spent fighting over a token
that only becomes available once per 1/rate seconds.

## Files in scope
| File | Change |
|---|---|
| `ratelimit.go` | reservation-based `Wait` (GCRA), `Wakeups()`, `Debt()`, burst clamp in `SetRate` |
| `ratelimit_wait_test.go` | **NEW** regression tests (scope widened here, before the work) |

## What was done
The token is RESERVED at admission: the balance is allowed to go negative and
the caller sleeps exactly `debt/rate` seconds. Each waiter therefore owns its
own deadline, spaced by `1/rate`, so nobody fights over the same token and the
re-check loop disappears entirely (this is the GCRA formulation; it is
strictly stronger than "sleep once and loop rarely").

- `Wait` sleeps ONCE, `select` on `ctx.Done()` vs a timer.
- Cancellation returns the reserved slot, so a timeout never leaks budget.
- `Wakeups()` counts real sleeps; `Debt()` reports the queue at the bucket.
- `SetRate` clamps the balance to the new burst.

## Verification
```
go test -count=1 -run "TestTokenBucket" -v .
--- PASS: TestTokenBucketPacing (1.00s)
--- PASS: TestTokenBucketCancel (0.30s)
--- PASS: TestTokenBucketNoBusyLoop (2.00s)      # 200 waiters -> 200 wakeups
--- PASS: TestTokenBucketCancelReturnsReservedSlot (0.10s)
```

Full gate: `gofmt -l .` empty, `go vet .` clean, `go build` ok,
`go test -count=1 ./...` = only the pre-existing Windows-only
`TestMockCtlEndToEnd` failure.

## Acceptance
- [x] 200 waiters cause ~1 wakeup per token (measured 200 for 200 tokens; the
      old code produced thousands)
- [x] `go test -count=1 ./...` passes (same single platform failure as baseline)
