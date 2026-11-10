# T-10 — Multi-IP budget verification (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 30 min
**Plan card:** `EXECUTION_PLAN.md` → T-10

## Problem
The per-IP budget bug (one IP with 5 groups received 5× the rate) was fixed in
`iplimit.go`, but the MULTI-proxy case had no test: with 5 proxies each IP must
get its own full budget and the total must be 5×.

## Files in scope
| File | Change |
|---|---|
| `scaling_test.go` | `TestMultiIPBudgetIsIndependent`, `TestMultiIPTotalBudget` |
| `iplimit.go` | `IPLimiterGrouped.Len()` (used by the tests/panels) |
| `PROJECT_MAP.md` | the measured multi-IP result (plan step 3) |

## What is proven
1. **Independence** — 5 proxies → 5 DISTINCT buckets, each at the full
   configured rate; the 5 rotating groups of one IP all resolve to the SAME
   bucket (so the groups cannot multiply the budget).
2. **No starvation** — draining IP-1's burst makes IP-1 wait while IP-2 still
   answers instantly.
3. **Total budget** — Σ(rate over IPs) = N × per-IP rate (5 × 1200 = 6000/min),
   and an AIMD `SetRate` moves every IP by the same factor.

## Verification
```
go test -count=1 -run "TestMultiIP" -v .
--- PASS: TestMultiIPBudgetIsIndependent (0.00s)
--- PASS: TestMultiIPTotalBudget (0.00s)
```
Full gate clean apart from the pre-existing Windows-only `TestMockCtlEndToEnd`.

## Acceptance
- [x] Both tests pass
- [x] Result documented in `PROJECT_MAP.md`
