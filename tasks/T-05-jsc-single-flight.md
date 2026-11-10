# T-05 — JSC single-flight solver (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 45 min
**Plan card:** `EXECUTION_PLAN.md` → T-05

## Problem
Every lane solved its own jsc challenge (2-4 extra requests each). A challenge
wave is IP-wide, so 500 lanes on one exit IP cost 2000+ requests against an IP
that is already being probed.

## Files in scope
| File | Change |
|---|---|
| `fetch.go` | `jscSolver` (per exit-IP + host single-flight), cookie export/import, `SolveJSCShared`; both call sites switched |
| `jsc_singleflight_test.go` | **NEW** regression tests (scope widened here, before the work) |
| `lanes.go` | *(no change needed — the lanes reach the solver through `Client`, so nothing in the engine had to move)* |

## Design
Key = `exit IP (client proxy) + host`. The solved state is the `loid` cookie.
- the first caller is the **leader**: it solves for its own URL;
- the solved cookies are snapshotted (`exportRedditCookies`);
- every **waiter** waits on `call.done` (ctx-cancellable), imports the cookies
  into its own jar (`importRedditCookies`) and re-fetches **its own** URL.

A lane NEVER receives another lane's page — that would file one account's
result under another account's name.

The guard is released with `defer` (delete from the map + close(done)), so
success, failure and timeout all release it.

## Verification
```
go test -count=1 -run "TestJSC" -v .
--- PASS: TestJSCSingleFlight (0.08s)              # 25 lanes -> 1 solve
--- PASS: TestJSCSingleFlightReleasesOnFailure (0.00s)
--- PASS: TestJSCSingleFlightWaiterHonoursContext (0.05s)
```
Full gate clean apart from the pre-existing Windows-only `TestMockCtlEndToEnd`.

## Acceptance
- [x] One challenge = one solve round, not N (25 lanes → 1 solve, counted)
- [x] The waiting lanes get the solved cookie (24 imports, one per waiter)
- [x] No lane ever gets another lane's page
