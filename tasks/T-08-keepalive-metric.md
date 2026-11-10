# T-08 — Keep-alive / TLS-handshake metric (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 30 min
**Plan card:** `EXECUTION_PLAN.md` → T-08

## Problem
Unknown whether every request opened a new TLS connection. A handshake per
request burns CPU and shows Reddit a fresh-connection burst (a bot signal).

## Files in scope
| File | Change |
|---|---|
| `metrics.go` | `RecordConn`, `NewConns`/`ReusedConns`/`ConnReusePct` in the snapshot, `Connections` line in the METRICS block |
| `fetch.go` | `fhttp/httptrace` `GotConn` hook in `doRequest` (only when a collector exists) |
| `keepalive_metric_test.go` | **NEW** regression tests |

## The measurement (real, not estimated)
The plan allowed a heuristic ("first byte slower than the median") if the
transport was unreachable. It is not needed: fhttp's transport honours the
`GotConn` trace hook, and `tls-client` is built on it, so the `Reused` flag is
the honest answer.

**Important gotcha:** the hook must be `github.com/bogdanfinn/fhttp/httptrace`,
NOT `net/http/httptrace` — fhttp stores the trace under its own context key, so
the stdlib version never fires (measured: 0 events before the fix).

## Verification
```
go test -count=1 -run "TestKeepAlive" -v .
--- PASS: TestKeepAliveMetric (0.00s)
--- PASS: TestKeepAliveMetricNilSafe (0.00s)
```
Against a local TLS server, 4 sequential requests report
`Connections : 1 new | 3 reused | reuse 75.0% (1 new per 4.0 requests)` —
i.e. **keep-alive is working**, one handshake for four requests.

Full gate clean apart from the pre-existing Windows-only `TestMockCtlEndToEnd`.

## Acceptance
- [x] The METRICS block shows the connection ratio
- [x] Accounting covers every request (new + reused == requests)
