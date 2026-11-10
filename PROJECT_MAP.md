# userkarmacheck-go — PROJECT MAP (2026-11-09)

Reddit account karma checker. Native Go rewrite of the Python `userkarmacheck`
project with uTLS/tls-client Chrome fingerprinting, multi-session lanes, VPN
rotation and resume. Python original (safety net, untouched):
`C:\Users\Rakib\Desktop\userkarmacheck`.

## Quick commands

| What | How |
|---|---|
| **START HERE — the task list** | `EXECUTION_PLAN.md` — every future change runs through it (protocol, per-task file scope, acceptance criteria, verify command, timebox, loop-breaker) |
| Full run | `run.bat` → `userkarmacheck check -i input.txt -o full_out --sessions sassion --lanes 466 --threads 150 --rate-per-ip 1200 --rate-per-session 72 --warmup 40 --browser-pages 4 --browser-budget 60 --adaptive-rate --source-tracking --vpn-rotate --vpn-network-lock` |
| **No-proxy run** | `run_noproxy.bat` → single exit IP + ExpressVPN auto-rotation, conservative adaptive rate (30 lanes / 30 threads) |
| Debug panels | `KARMA_DEBUG=1` env var **or** `--debug` flag → rate-bucket + source-health tables at the end of the run |
| Login an account | `login.bat` / `userkarmacheck login` (real Chrome via rod, saves sassion/N/cookies.json) |
| Validate session folder | `userkarmacheck sessions -d sassion` → sassion_report.txt |
| One fetch / one probe | `check\|fetch\|probe` subcommands (see main.go) |
| Debug trace | env `KARMA_DEBUG=1` |
| Tests | `go test ./...` |

## Pipeline (what happens on a run)

```
input.txt (545,735 lines) ── FilterCredentials? ── LoadLines
        └─ resume: ReadDoneUsers(full_out) skips already-saved users
MultiChecker.Run (lanes.go) ─ buildLanes (lane = client+session+gate)
        ├─ buildSources  → ladder: session JSON → session HTML → anon JSON → anon HTML → Chrome
        ├─ VPN controller armed (rotations reset lanes/breakers/AIMD/warm-up)
        ├─ preflight: /api/v1/me probe per session (dead sessions dropped)
        └─ adaptive resizable worker pool → process() → finish() → 8 output files
Rate path per request:  AimedGate.Admit = IPBucket.Wait (shared 1200/min) →
        per-session Bucket.Wait (72/min) → Breaker.Allow (45s cooldown)
Protective loop: AIMD shrinks/grows IP rate · ThrottleDetector → VPN rotate ·
        watchDog aborts the run when the NETWORK PATH (VPN/DNS) is dead
```

## File map

### Core engine
| File | Contents |
|---|---|
| `main.go` | Subcommand router (`check` default, `login`, `fetch`, `probe`, `sessions`); all CLI flags; credential filter; MultiConfig assembly |
| `lanes.go` | **The account-checking engine.** `Lane` (client+session+gate, block flag, `RotateIfNeeded` session recycling), `LaneManager.Next` (round-robin, skips blocked), `buildLanes` (lane pool), `MultiChecker.Run` (resume → worker pool → summary), `process` (per-user pipeline), `finish`/`finishError` (output + counters), `watchdog` (dead-network abort), `resetAfterRotation`, session report |
| `worker.go` | Single-session checker (`--cookies`/`--proxies` mode): same output format, ladder integration, blocked-streak abort. Also the fallback path when no `--sessions` |
| `pool.go` | `SessionPool` (loads sassion/*/cookies.json), session health score (+1 clean / −1 blocked, ≤ −3 → 10-min quarantine) **that scales the session bucket rate 0.5x–1.5x**, `ProbeSession` |
| `ladder.go` | Source ladder + per-user deadline `FetchWithLadderLimit` (max 6 sources, 60s budget). Sources resolve the lane's CURRENT client at request time (rotation-safe) |
| `fetch.go` | HTTP layer: tls-client Chrome_133 fingerprint, HTTP/1.1, `FetchUser` (JSON), `FetchUserHTML` (+jsc solver `SolveJSC`, accepts 302+loid), `SolveJSCShared` — single-flight per exit IP + host so one challenge wave costs one solve, `GetResilient` |
| `browserfallback.go` | Last ladder rung: real Chrome through rod, page pool (`--browser-pages`) + budget (`--browser-budget`), 12s cap |

### Rate control / resilience
| File | Contents |
|---|---|
| `ratelimit.go` | TokenBucket (refill rate-min/60 per sec, burst; ctx-cancellable `Wait` that RESERVES the slot and sleeps exactly once — GCRA, no 100 ms poll; `SetRate`, `Wakeups`, `Debt`) |
| `iplimit.go` | IPLimiter — ONE shared bucket per exit IP (Reddit limits cookie sessions per IP, measured) **+ IPLimiterGrouped — 5 INDEPENDENT buckets (one per rotating group)** |
| `advanced.go` | **NEW**: GroupRateTracker (adaptive per-group rate from the observed success rate), SourceHealthTracker (which session/proxy works), debug panels (`KARMA_DEBUG=1`) |
| `breaker.go` | CircuitBreaker: open after N consecutive 401/403, 45s cooldown, half-open probe; `Reset()` used by VPN rotation |
| `resilient.go` | Gate = session bucket + IP bucket + breaker; `Report` ignores jsc-challenge 403; `RetryAfter`/backoff |
| `aimd.go` | AIMD: blocked→×0.85; clean→+0.05 only after 40 clean samples; single-flight 5% recovery probe every 10s; VPN reset starts at floor and preserves the known-good ceiling; applies to IP bucket only |
| `adaptive.go` | AdaptiveGovernor + reusable ResizablePool: account outcomes halve/double the live worker count; both single-engine and multi-engine CLI paths use one shared, runtime-resizable pool with no job loss |
| `classify.go` | Error taxonomy (ok / notfound / suspended / throttled / needs_session / real) |
| `vpn.go` | ExpressVPN controller: ThrottleDetector (6 consecutive throttles or ≥30% of window), pause-all, region pool, rotation verify via ipify, max-rotations cap |

### I/O & presentation
| File | Contents |
|---|---|
| `output.go` | 8 output files (`0to500.txt … 3000ormore.txt`, `not_found.txt`, `banned.txt` = actually errors.txt holds errors — Python-format parity), `ReadDoneUsers` resume, `LoadLines`, thread-safe `Append` |
| `report.go` | RunReport console + `full_out/run_report.txt` + `session_usage.txt`; `progressf` throttled to 20 lines/s (HIT/ERROR/SUSPENDED always kept, dropped count in the summary) |
| `live.go` | In-place live status panel (rich.live equivalent; ASCII fallback on non-UTF8 console) |

### Observability (added 2026-11-09, extended 2026-11-10)
| File | Contents |
|---|---|
| `metrics.go` | Run-wide collector: request/status counters, **block rate**, latency histogram (p50/p95/p99), goroutine + heap, **queue depth**, **error-reason breakdown**, **new-vs-reused TLS connections + reuse ratio** (T-08). Hot path is allocation-free (atomics + fixed histogram). Reported at the end of every run as "HTTP METRICS" |
| `profiling.go` | `--pprof :6060` starts the pprof server (loopback only): `/debug/pprof/profile?seconds=30` for the CPU profile, `/debug/pprof/heap`, `/debug/pprof/goroutine?debug=2` |
| `live.go` | The live panel carries the **rate-limit state**: Blocked, Block rate, Rate, AIMD multiplier, Queue depth, VPN rotations. A run that is quietly rate limited must be visible, not guessed |
| `ladder.go` | Every escalate / retry / stop is traced with the user, the source label, the verdict and the reason (`ladderVerdict`, `ladderReason`) |
| `lanes.go` | `rateStateRows` feeds the panel; `debugSnapshot` prints one state line every 30 s **while the run is going** (the end-of-run panels are useless when a long run is killed) |

**How to read the live numbers:**
| Signal | Meaning |
|---|---|
| `Blocked` rising | Reddit is refusing requests — the detection signal |
| `Block rate` | blocked / answered. Above ~2% means the rate is too hot |
| `Rate` | the CURRENT per-IP rate (the AIMD moves it) |
| `AIMD` | the controller multiplier. `1.00x` = full speed, `< 1.00x` = the AIMD shrank the rate |
| `Queue` | workers waiting for a token. A deep queue with a low block rate means the **bucket** is the bottleneck, not the network |
| `queue wait avg` | average time a request spent waiting for a token. A measured run showed **17.5 s** — that is the real cost of a rate limit |

**Measured on this machine:** network RTT to Reddit ≈ **1.2 s/request**
(the checker's own uTLS client; `hey` cannot measure it — Reddit 403s plain
Go TLS). Therefore
`throughput = concurrency / latency` → 500 threads ≈ **416 req/s ceiling**,
and the per-IP limit (~162 req/s) bites first on a single exit IP.

### Sessions & auth
| File | Contents |
|---|---|
| `session.go` | Cookie loading (Netscape/JSON array/object/raw header), `HealthCheck`, status files |
| `sessions.go` | `sessions` subcommand: validate every session in a real Chrome → sassion_report.txt |
| `login.go` | rod flow: homepage → Log in button → poll cookies → health check → save 3 files |
| `oauth.go` | app-only OAuth token (cached, invalidated on 401) |
| `credentials.go` | username/password filter (Total/Valid/InvalidUsername/… counters) |

## Output files (`full_out\`)
- `0to500.txt … 3000ormore.txt` — hits, RAW input line, bucketed by total karma
- `not_found.txt` — `user | NOT_FOUND`
- `banned.txt` — `user | SUSPENDED`
- `errors.txt` — `user | ERROR: err: detail` (re-checked on the next run, NOT in resume)
- `run_report.txt`, `session_usage.txt`

## State on disk
- `input.txt` — 545,735 account lines (13.7 MB)
- `sassion\` — the session folder (one Netscape cookie file per account; 5561 files at the last count — the folder grows as new accounts are added, the checker picks them up automatically)
- `full_out\` — output state; resume map is scanned from here each run

## Known history / pitfalls (do not re-break)
1. Reddit limits **per IP**, not per account → every lane on an IP shares ONE bucket (iplimit.go).
2. A jsc-challenge 403 is SOLVABLE → never count it against a breaker (resilient.go Report).
3. jsc challenge success = **302 + loid cookie**, not 200 (fetch.go SolveJSC).
4. A non-existent account gets Reddit's HOME feed → NOT_FOUND, not "blocked".
5. Ladder sources must resolve the lane client at request time — a snapshot
   replayed old cookies after rotation (fixed 2026-11-09).
6. `ratelimit.go` was accidentally emptied once (build broke) — TokenBucket restored 2026-11-09.
7. Deadnetwork watchdog: a streak of 30 consecutive network-fatal errors
   (`context deadline exceeded`, `no such host`, `dial tcp`, …) aborts the run
   with a clean message instead of crawling forever (fixed 2026-11-09).
8. **A Netscape session file is ONE session, not one per cookie line.** Splitting
   it per line turned 467 session files into 9288 bogus "sessions"
   (`ParseSessionLine` now rejects tab-separated lines; `LoadSessionValues`
   only splits a file when EVERY line is a bare token) — fixed 2026-11-09.
9. **The multi-engine path must open its OWN output writer.** It never calls
   `MultiChecker.Run` (that is where the normal run opens the writer), so an
   unopened writer silently DROPPED every result — the run reported success
   but wrote no files. All engines now share ONE writer
   (`MultiEngineRunner.openSharedOutput`) — fixed 2026-11-09.
10. **ExpressVPN reports "Connected" BEFORE the tunnel has switched.** A single
    exit-IP probe right after the connect still sees the OLD IP and the whole
    rotation is judged a failure. The rotation now waits for the tunnel to
    settle and KEEPS POLLING until the IP really changed
    (`VPNController.waitNewExitIP`) — fixed 2026-11-09.
11. **The session folder holds FILES, not sub-folders.** `dir /b /ad` counts
    directories only, so `run.bat` / `run_noproxy.bat` always found 0 sessions
    and silently fell back to 10 lanes. They now use `dir /b /a-d` — fixed
    2026-11-09.
12. **A batch file must not pass a subcommand twice.** `run.bat check -i x`
    used to become `userkarmacheck.exe check check -i x`; the extra "check"
    was read as a positional argument, the flag parser stopped there and the
    run used the DEFAULT input.txt. `run.bat` now forwards the subcommand
    unchanged — fixed 2026-11-09.
13. **Inside a batch `if (...)` block every `%VAR%` is expanded when the block
    is PARSED.** The session counters must therefore be set OUTSIDE the block,
    or the `if %LANES% GTR 500` test sees an empty value and the script dies
    with "500 was unexpected at this time" — fixed 2026-11-09.
14. **The per-IP budget is per EXIT IP, not per rotating group.** The limiter used to
    keep one bucket per group with a global group counter, so a single IP with 5
    groups received **5 x** the configured `--rate-per-ip` — the fastest way to get
    the exit IP blocked. `IPLimiterGrouped.Bucket(ip, group)` now returns ONE bucket
    per IP; the group stays for the session rotation and the panels only — fixed
    2026-11-09.
15. **The preflight probes in PARALLEL (32 at a time).** It used to probe one
    session after the other and a probe is one HTTP round trip (~1.2 s), so 500
    lanes meant a 10 MINUTE startup — fixed 2026-11-09.
16. **`hey` cannot measure this project.** Reddit answers plain Go TLS with 403
    and a 20 s hang, so `hey` only proves that the plain-TLS path is blocked. Use
    the checker's own `HTTP METRICS` block for the real numbers — fixed 2026-11-09.
17. **The per-user deadline must not be eaten by the rate-limiter wait.** The queue
    wait and the HTTP request shared ONE context deadline, so with many workers
    on ONE per-IP bucket the queue wait (tens of seconds) consumed the
    request's time and the request died with "context deadline exceeded" even
    though the network was fine. The account was then only saved by the HTML
    fallback — which is why the transport-failure count was ~1 per account
    while the account-level error count stayed low.
    FIXED 2026-11-09: `GetResilient` gives the queue wait its own budget
    (`queueWaitCap` = 5 min) and the REQUEST its own full timeout
    (`Client.Timeout`); the ladder's per-user budget is a wall-clock deadline
    instead of a context deadline. Measured: transport failures went from
    **~56% of the HTTP attempts to 3.1%**. Regression test:
    `TestQueueWaitDoesNotEatRequestTimeout`.
18. **The AIMD shrinks fast and recovers slow.** After a VPN rotation it resets
    to 0.5x and then ramps up by +0.15 per 20 clean samples — fast enough to
    walk straight back into the rate limit (one measured run rotated 6 times).
    The growth must be much slower than the shrink — known open issue (P1).

19. **One breaker per EXIT IP, never per lane.** A per-lane breaker let all 500
    lanes run their own cooldown and fire their own probe at the same time —
    the block wave then continued by itself. `breaker.go:BreakerRegistry` now
    keeps ONE `CircuitBreaker` per proxy/exit IP and the half-open probe is a
    single-flight (`probeDone` channel), so exactly one request tests the
    recovered IP while the rest wait. The per-session `Score` in `pool.go`
    stays a separate signal — fixed 2026-11-09 (T-03), test
    `TestSharedBreakerOpensOnceForAllLanes`.

## Measured load-test results (500 lanes, one exit IP)
| Rate | Block rate | Transport failures | Verdict |
|---|---|---|---|
| 6000/min (100 req/s) | **4.57%** | 177 | run aborted early — TOO HOT |
| 2400/min (40 req/s) | **1.75%** | 443 | better, but the ladder's first source still times out |
| 1200/min (20 req/s) | 0% | 0 | the safe zone |

**Safe configuration on ONE exit IP:** `--lanes 500 --threads 120 --rate-per-ip 1200`
(120 threads x 1.2 s RTT ~ 100 req/s in flight, matching the measured ~162 req/s
per-IP ceiling with headroom). More speed needs MORE exit IPs, not more threads.

## Session file formats (all supported)
| Format | Example | Notes |
|---|---|---|
| Netscape | `.reddit.com<TAB>TRUE<TAB>/<TAB>TRUE<TAB>exp<TAB>name<TAB>value` | ONE session per file |
| JSON array | `[{"name":"reddit_session","value":"..."}]` | ONE session per file |
| JSON object | `{"reddit_session":"..."}` | ONE session per file |
| Raw header | `Cookie: reddit_session=abc; token_v2=def` | ONE session per file |
| Bare value | `eyJhbGciOiJSUzI1NiIs...` | ONE session per line → many sessions |
| Mixed | `abc; token_v2=def` | first field = reddit_session |

`LoadSessionValues` tells the two file KINDS apart: only a file where EVERY
line is a bare token is split into many sessions; everything else is one
session.
