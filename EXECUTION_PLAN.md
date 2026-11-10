# EXECUTION PLAN — userkarmacheck-go

**Purpose:** every future change to this project runs through this file. One
task at a time, each with an exact file scope, an acceptance criterion, a
verify command and a timebox. That is what keeps the work from looping.

Read `PROJECT_MAP.md` for the architecture and
`~/.agents/skills/reddit-checker/SKILL.md` for the hard rules and the
measured numbers.

---

## THE PROTOCOL (read before every task)

1. **One task at a time.** Finish T-nn, verify it, update its status, then
   start T-(nn+1). Never work on two at once.
2. **Only the listed files.** If a task needs a file that is not in its
   scope, STOP and widen the scope in this file first. No drive-by edits.
3. **The verify command must pass** before the task is marked DONE. A task
   without a passing verify command is NOT done.
4. **Timebox.** If a task is not verified within its timebox, move it to
   BLOCKED and go to the next task. Come back later.
5. **LOOP BREAKER — max 2 attempts.** If the same approach fails twice
   (same test fails, same error twice), STOP trying that approach. Write
   down what was tried, switch to a different approach, or mark BLOCKED.
   Never attempt the same fix a third time.
6. **No heavy network tests from the agent session.** A 500-thread run
   saturates the network and kills the agent's own API connection
   (`ECONNRESET`). Heavy runs belong in a separate terminal that the USER
   starts. The agent may run unit tests and a LIGHT live run
   (<= 16 threads, <= 8 req/s).
7. **VPN live test: max 2 runs, ever.** It disconnects the real ExpressVPN.
   Only when the VPN code itself changed.
8. **Never touch `sassion/` or `full_out/`.** They hold the user's sessions
   and the run results.
9. **Backup before any cleanup:** re-create
   `C:\Users\Rakib\Desktop\userkarmacheck-go_source_backup.zip`.
10. **Every task ends with the full gate:**
    `gofmt -l . && go vet . && go build -o userkarmacheck.exe . && go test -count=1 ./...`

---

## TASK INDEX

| ID | Task | Files | Timebox | Status |
|----|------|-------|---------|--------|
| T-01 | AIMD: slow recovery + no walk-back into the limit | `aimd.go`, `lanes.go` | 45 min | DONE |
| T-02 | Resizable worker pool (apply the governor) | `adaptive.go`, `lanes.go` | 60 min | DONE |
| T-03 | Shared per-IP circuit breaker | `breaker.go`, `resilient.go`, `lanes.go`, `breaker_shared_test.go` | 45 min | DONE |
| T-04 | TokenBucket: exact wait, no thundering herd | `ratelimit.go`, `ratelimit_wait_test.go` | 30 min | DONE |
| T-05 | JSC single-flight solver | `fetch.go`, `jsc_singleflight_test.go` | 45 min | DONE |
| T-06 | Progress-line throttle | `report.go`, `lanes.go`, `progress_throttle_test.go` | 20 min | DONE |
| T-07 | Per-session adaptive rate | `pool.go`, `session_rate_test.go` | 45 min | DONE |
| T-08 | Keep-alive / TLS-handshake metric | `fetch.go`, `metrics.go`, `keepalive_metric_test.go` | 30 min | DONE |
| T-09 | Health-based exit-IP routing | `advanced.go`, `vpn.go`, `lanes.go`, `vpn_region_health_test.go` | 60 min | DONE |
| T-10 | Multi-IP budget independence + total budget verification | `iplimit.go`, `scaling_test.go` | 30 min | TODO |

---

## TASK CARDS

### T-01 — AIMD: slow recovery, no walk-back into the limit
**Problem (measured):** after a VPN rotation the AIMD resets to 0.5x and then
ramps up by +0.15 per 20 clean samples. That is fast enough to walk straight
back into the rate limit — one measured run rotated the VPN **6 times**.

**Files (only these):** `aimd.go`, `lanes.go`

**Steps**
1. In `aimd.go` split the two directions: keep the multiplicative decrease
   (0.85x) but make the additive increase much slower (e.g. +0.05 per 40
   clean samples) and require a longer clean streak before growing.
2. Add a `RecoveryProbe` mode: after a shrink, allow ONE trial request at
   1.05x every 10 s. If it is clean, resume growing; if it is blocked, shrink
   again immediately.
3. In `lanes.go` `resetAfterRotation`: do NOT reset to a fixed 0.5x. Reset to
   the minimum but remember the last known-good multiplier, and let the probe
   climb back to it instead of from zero.
4. Add `TestAIMDRecoveryIsSlowerThanShrink` in `engine_test.go`.

**Acceptance**
- The AIMD never grows faster than it shrinks.
- After a reset the multiplier climbs back gradually (no jump).
- `go test -count=1 ./...` passes.

**Verify:** `go test -count=1 -run "TestAIMD" -v .`

**If blocked (loop-breaker):** after 2 attempts, leave the AIMD as it is and
note it here. Do NOT attempt a third rewrite.

---

### T-02 — Resizable worker pool
**Problem:** `AdaptiveGovernor.Workers()` is never read. The pool is a fixed
`for w := 0; w < m.cfg.Workers` loop, so the thread count can never react to
a block wave. The per-lane governor is `NewAdaptiveGovernor(4, 1, 8)` — capped
at 8, meaningless at 500 lanes.

**Files (only these):** `adaptive.go`, `lanes.go`

**Steps**
1. In `lanes.go` replace the fixed worker loop with a pool that can grow and
   shrink: keep a `targetWorkers` atomic int, spawn workers up to it, and let
   idle workers exit when the target drops.
2. Feed the pool target from a controller goroutine that reads a NEW
   **run-wide** `AdaptiveGovernor` (min 8, max the requested worker count).
   The per-lane governor stays as it is.
3. Report every change: `fmt.Printf("pool: %d -> %d workers (%s)\n", ...)`.
4. The governor's `Report(ok)` must be called once per finished account with
   the account-level outcome (a 404 is a clean outcome, a block is not).
5. Add `TestWorkerPoolResizes` in `scaling_test.go`.

**Acceptance**
- The worker count visibly changes when the error rate rises.
- The pool never exceeds the requested maximum.
- `go test -count=1 ./...` passes.

**Verify:** `go test -count=1 -run "TestWorkerPool" -v .`

**If blocked:** after 2 attempts, keep the fixed pool and mark T-02 BLOCKED.

**Verified (2026-11-09, Linux gate — closes the open documentation item):**
`go test -count=1 -run "TestWorkerPool" -v .` → `PASS`, and the log prints
`pool: 4 -> 8 workers (healthy (error rate 0.0%))`, i.e. the governor really
moves the live worker count. The fixed
`for w := 0; w < m.cfg.Workers` loop is gone: `lanes.go` (single engine) and
`multiengine.go` (multi engine) both drive one `ResizablePool`, and the
account outcome reaches the governor exactly once per finished account
(`TestAccountOutcomeReportedExactlyOnce`).

---

### T-03 — Shared per-IP circuit breaker
**Problem:** the breaker is per LANE. When the exit IP blocks, all 500 lanes
retry at once — a textbook bot pattern.

**Files (only these):** `breaker.go`, `resilient.go`, `lanes.go`,
`breaker_shared_test.go` (scope widened before the work — the tests need their own file)

**Steps**
1. Add a shared breaker per exit IP (a map keyed by the proxy string).
2. The lane's `Gate.Breaker` points at the shared per-IP breaker instead of a
   private one.
3. The shared breaker gets a half-open probe: after the cooldown, exactly ONE
   lane is allowed through; the others keep waiting.
4. The per-session health score (`pool.go`) stays per session — that is a
   different signal and must not be merged into the IP breaker.
5. Add `TestSharedBreakerOpensOnceForAllLanes`.

**Acceptance**
- 100 lanes on one IP share one breaker state.
- After the cooldown only one request is let through.
- `go test -count=1 ./...` passes.

**Verify:** `go test -count=1 -run "TestSharedBreaker" -v .`

**Done (2026-11-09):** `BreakerRegistry` + channel-based half-open probe.
- 100 lanes on one IP share one `*CircuitBreaker` (`TestSharedBreakerOpensOnceForAllLanes`):
  after the cooldown exactly **1** caller is let through, the other 99 wait on
  `probeDone`; a clean probe releases all of them, a blocked probe re-opens the
  circuit and they keep waiting.
- `buildLanes` wires every lane through the registry (`TestLanesShareOneBreakerPerExitIP`),
  and a session swap keeps the shared breaker (`gateForSession`).
- `resetAll` (VPN rotation) resets the registry once instead of N times.
- The per-session health score stays in `pool.go` — deliberately NOT merged.
Task record: `tasks/T-03-shared-breaker.md`.

---

### T-04 — TokenBucket: exact wait, no thundering herd
**Problem:** `Wait` caps the sleep at 100 ms and then re-checks under the
lock. With 500 waiters that is 5000 wakeups per second for nothing.

**Files (only these):** `ratelimit.go`, `ratelimit_wait_test.go`
(scope widened before the work — the tests need their own file)

**Steps**
1. Compute the exact wait time from the deficit and sleep ONCE (no 100 ms
   cap, no re-check loop).
2. Keep the ctx cancellation: use `select` on `ctx.Done()` and a timer.
3. After the sleep, take the token under the lock; if another waiter won it,
   loop (but the sleep is now exact, so the loop runs rarely).
4. Add `TestTokenBucketNoBusyLoop`: 200 waiters on a 60/min bucket must not
   spin — count the wakeups with a counter in the bucket.

**Acceptance**
- 200 waiters cause no more than ~1 wakeup per token.
- `go test -count=1 ./...` passes.
**Done (2026-11-09):** `Wait` now RESERVES the token (the balance may go
negative) and sleeps exactly once for `debt/rate` — GCRA, so every waiter has
its own deadline instead of all of them waking every 100 ms. Cancellation
returns the reserved slot. Measured: 200 waiters / 200 tokens = **200 wakeups**
(`TestTokenBucketNoBusyLoop`). Task record: `tasks/T-04-tokenbucket-exact-wait.md`.


---

### T-05 — JSC single-flight solver
**Problem:** every lane solves its own jsc challenge (2-4 extra requests).
At 500 threads a challenge wave costs 2000+ requests.

**Files (only these):** `fetch.go`, `jsc_singleflight_test.go`
(scope widened before the work; `lanes.go` needed no change — the lanes reach the
solver through `Client`, so the guard lives in `fetch.go`)

**Steps**
1. Add a package-level single-flight guard around `SolveJSC`: one goroutine
   solves, the others wait for the result and reuse the solved cookie.
2. The guard must be released on every exit path (error, timeout, success).
3. The solved `loid` cookie must reach the waiting lanes — the cookie jar is
   per client, so the solved cookie has to be copied to the waiting clients.
4. Add `TestJSCSingleFlight`: N goroutines call the solver, the underlying
   HTTP call must happen ONCE.

**Acceptance**
- One challenge = one solve round, not N.
- The waiting lanes get the solved cookie.
- `go test -count=1 ./...` passes.

**Verify:** `go test -count=1 -run "TestJSCSingleFlight" -v .`
**Done (2026-11-09):** `jscSolver` keyed by **exit IP + host** (not by lane).
The leader solves, snapshots the reddit cookies and every waiter imports them
and re-fetches its OWN url — a lane never receives another lane's page. Measured
by `TestJSCSingleFlight`: 25 lanes → **1** solve, 24 cookie imports. The guard
is released with `defer`, so failure/timeout cannot wedge the wave.
Task record: `tasks/T-05-jsc-single-flight.md`.


---

### T-06 — Progress-line throttle
**Problem:** `progressf` prints one line per result when the live panel is
off. At 500 threads that is 1000 lines/second and the console becomes the
bottleneck.

**Files (only these):** `report.go`, `lanes.go` (one summary line only),
`progress_throttle_test.go` — scope widened before the work, because the plan asks
for the dropped count to reach the final summary, which lives in `lanes.go`

**Steps**
1. Rate-limit `progressf` to at most 20 lines per second (keep every HIT and
   every error, drop the repetitive NOT_FOUND lines).
2. Add a counter so the dropped lines are reported in the final summary.
3. Add `TestProgressThrottle`.

**Acceptance**
- 1000 calls produce at most ~20 lines in one second.
- No HIT or ERROR line is ever dropped.

**Verify:** `go test -count=1 -run "TestProgressThrottle" -v .`
**Done (2026-11-09):** `progressf` renders the line, keeps every ` HIT `/` ERROR `/
` SUSPENDED ` line unconditionally and pushes everything else through a
20-lines-per-second window. Suppressed lines are counted and the run summary
prints the total. `progressOut` is a seam so the throttle is measurable.
Task record: `tasks/T-06-progress-throttle.md`.


---

### T-07 — Per-session adaptive rate
**Problem:** every session gets the same `--rate-per-session`. Some accounts
tolerate more, some less.

**Files (only these):** `pool.go`, `session_rate_test.go`
(scope widened before the work; `lanes.go` needed no change — the lane gate already
uses `s.Bucket`)

**Steps**
1. Give each `Session` a small health score that already exists (`Score`).
2. Scale the session's bucket rate by the score: a clean session may use up
   to 1.5x the base rate, a bad one 0.5x — clamped to the configured band.
3. Never let one session's rate change affect another.

**Acceptance**
- A clean session's bucket rate is higher than a bad one's.
- The per-IP bucket still caps the total.

**Verify:** `go test -count=1 -run "TestSessionAdaptiveRate" -v .`
**Done (2026-11-09):** `Session.baseRate` stores the configured rate and every
`Report`/`ClearPenalty` re-applies `baseRate × factor(score)` to the session's
OWN bucket, clamped to 0.5x…1.5x (score 10 → 1.5x, 0 → 1.0x, −3 → 0.5x).
`TestSessionAdaptiveRate`: at base 72 a clean session runs at **108/min** and a
blocked one at **36/min**, the other sessions do not move, and the per-IP
bucket stays at 1200/min. Task record: `tasks/T-07-per-session-rate.md`.


---

### T-08 — Keep-alive / TLS-handshake metric
**Problem:** unknown. If every request opens a new TLS connection, the CPU
burns and Reddit sees a burst of new connections (a bot signal).

**Files (only these):** `fetch.go`, `metrics.go`, `keepalive_metric_test.go`
(scope widened before the work — the tests need their own file)

**Steps**
1. Count the requests and the NEW connections. tls-client does not expose the
   connection count directly — count a "new connection" when a request had to
   wait longer than the median for its first byte, OR use the client's
   `Transport` idle-connection count if it is reachable.
2. Report `requests` and `new connections` in the HTTP METRICS block.
3. Add `TestKeepAliveMetric`.

**Acceptance**
- The METRICS block shows the connection ratio.
- `go test -count=1 ./...` passes.
**Done (2026-11-09):** the "first byte slower than the median" heuristic was NOT
needed — fhttp's transport honours the `GotConn` trace hook, so `doRequest`
records the real `Reused` flag (`github.com/bogdanfinn/fhttp/httptrace`, not
stdlib `net/http/httptrace`: the stdlib hook never fires under fhttp, measured
0 events). The METRICS block now prints
`Connections : N new | M reused | reuse X% (1 new per K requests)`.
Measured against a local TLS server: 4 requests → **1 handshake**, 75% reuse,
so keep-alive is working. Task record: `tasks/T-08-keepalive-metric.md`.


---

### T-09 — Health-based exit-IP routing
**Problem:** `SourceHealthTracker` records which session/proxy works but
nothing routes on it. The VPN rotates to the next region blindly.

**Files (only these):** `advanced.go`, `vpn.go`, `lanes.go`, `vpn_region_health_test.go`
(scope widened before the work — the tests need their own file)

**Steps**
1. Keep a per-region success score in the VPN controller.
2. `nextRegion` prefers the healthiest region that is not the current one,
   instead of the next one in the list.
3. The region list stays as the fallback order.
4. Add `TestVPNPrefersHealthyRegion`.

**Acceptance**
- A region that was blocked is not picked again immediately.
- `go test -count=1 ./...` passes.
**Done (2026-11-09):** `VPNController` keeps a per-region record
(`ok`/`bad`/`lastBlock`) and ranks with the Laplace-smoothed success ratio minus
0.75 while the region is inside its 10-minute block cooldown, so a burned region
sinks below every healthy one. `nextRegion` and `nextRegionDifferentCountry`
both pick the best score and use the pool order only as the tie-break, so a
fresh controller behaves exactly like before. Every rotation attempt records its
outcome, and `ReportIPBlocked` burns the region we are connected to before the
rotation chooses (measured in `TestVPNABlockPageScoresTheCurrentRegion`:
usa-east blocked → next connect is germany-1). `advanced.go` gained
`SourceHealthTracker.PathHealth()` and the rotation reason now carries it.
Task record: `tasks/T-09-region-health-routing.md`.


---

### T-10 — Multi-IP budget verification
**Problem:** the per-IP budget was fixed (P0-1) but the MULTI-proxy case has
no test: with 5 proxies, each IP must get its own full budget and the total
must be 5x.

**Files (only these):** `iplimit.go`, `scaling_test.go`

**Steps**
1. Add `TestMultiIPBudgetIsIndependent`: 5 proxies, each gets a bucket with
   the full configured rate; the buckets are different objects.
2. Add `TestMultiIPTotalBudget`: the sum over the IPs is N x the per-IP rate.
3. Document the result in `PROJECT_MAP.md`.

**Acceptance**
- Both tests pass.

**Verify:** `go test -count=1 -run "TestMultiIP" -v .`

---

## DEFINITION OF DONE (a task is only DONE when ALL are true)

- [ ] The code compiles: `go build -o userkarmacheck.exe .`
- [ ] `gofmt -l .` prints nothing
- [ ] `go vet .` is clean
- [ ] `go test -count=1 ./...` passes
- [ ] The task's own verify command passes
- [ ] A regression test exists for the change
- [ ] `PROJECT_MAP.md` and the skill are updated with the finding
- [ ] The status in the task index above is set to DONE
- [ ] The backup zip was re-created
- [ ] No file outside the task's scope was modified


