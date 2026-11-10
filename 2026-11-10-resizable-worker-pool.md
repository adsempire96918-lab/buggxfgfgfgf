# Task: T-02 — Resizable adaptive worker pool

- **Status:** COMPLETE
- **Created:** 2026-11-10
- **Owner:** Cline
- **Goal:** Make single-engine and multi-engine CLI paths use a runtime-resizable worker pool that reacts to account-level outcomes without losing jobs or exceeding requested concurrency.

## Scope lock

### Target files (only these may be changed)

| Absolute path | Planned change |
|---|---|
| `C:\Users\Rakib\Desktop\userkarmacheck-go\adaptive.go` | Governor resize signal/reason and reusable race-free resizable pool. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\lanes.go` | Replace fixed loop; report one final account outcome. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\multiengine.go` | Use the pool in the real `--multi-engine` path; remove racy completion accounting. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\scaling_test.go` | Deterministic resize, no-job-loss and outcome tests. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\EXECUTION_PLAN.md` | Mark T-02 DONE after verification. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\PROJECT_MAP.md` | Document adaptive pool behavior. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\userkarmacheck-go_source_backup.zip` | Recreate source-only backup. |

### Explicitly out of scope

- `C:\Users\Rakib\Desktop\userkarmacheck-go\sassion\`, `C:\Users\Rakib\Desktop\userkarmacheck-go\full_out\`, and `C:\Users\Rakib\Desktop\userkarmacheck-go\input.txt` must not be changed.
- T-03 through T-10 implementation.
- Live Reddit traffic, VPN rotation, load tests, and credential/session changes.
- Source backup cleanup; the zip may only be recreated with the same file selection.

## Acceptance criteria

- [x] Pool grows and shrinks at runtime without losing or duplicating a job.
- [x] In-flight jobs finish; idle workers retire after shrink; requested maximum is never exceeded.
- [x] Run-wide governor starts at requested count, floor `min(8, requested)`, and reacts to account outcomes.
- [x] Exactly one outcome per finished account: hit/404/suspended clean; block/throttle/error not clean.
- [x] Both `MultiChecker.Run` and `MultiEngineRunner.Run` use the pool.
- [x] Every target change logs `pool: <old> -> <new> workers (<reason>)`.
- [x] Deterministic resize, no-job-loss and exactly-once reporting tests pass.
- [x] Targeted test passes: `go test -count=1 -run "TestWorkerPool|TestAccountOutcome|TestAdaptiveGovernorResize" -v .`.
- [x] Full gate passes: `gofmt -l .`, `go vet .`, `go build -o userkarmacheck.exe .`, `go test -count=1 ./...`.

## Ordered implementation steps

| # | Step | Target | Verification | Status |
|---|---|---|---|---|
| 1 | Inspect execution paths and baseline | `adaptive.go`, `lanes.go`, `multiengine.go`, `scaling_test.go` | Baseline green; fixed pools and multi-engine bypass confirmed | DONE |
| 2 | Implement governor signal and resizable pool | `adaptive.go` | Focused pool tests | DONE |
| 3 | Integrate single-engine pool and outcomes | `lanes.go` | Focused tests | DONE |
| 4 | Integrate multi-engine pool | `multiengine.go` | Focused/full tests | DONE |
| 5 | Add deterministic regression coverage | `scaling_test.go` | Targeted verification | DONE |
| 6 | Full gate and project records | `EXECUTION_PLAN.md`, `PROJECT_MAP.md` | Full gate | DONE |

## Verification plan

1. `go test -count=1 -run "TestWorkerPool|TestAccountOutcome|TestAdaptiveGovernorResize" -v .`
2. `go test -race -count=1 -run "TestWorkerPool|TestAccountOutcome" -v .` (record missing-C-compiler limitation if unavailable).
3. Full project gate listed in acceptance criteria.
4. No live traffic is required for this local concurrency task.

## Loop prevention / stop rules

- Read only named targets plus minimum dependency context.
- On failure, record it, form one new hypothesis, make the smallest in-scope change, and rerun only the failing check.
- Do not repeat an identical failing command without changed code/hypothesis.
- Max 2 attempts per approach; then mark BLOCKED with evidence.
- Widen scope with rationale before touching another file.

## Risks and mitigations

- **Risk:** Worker exits after taking a job. **Mitigation:** one worker owns a job until its callback returns; resize retires idle workers.
- **Risk:** Per-engine pools multiply global concurrency. **Mitigation:** multi-engine uses one global pool and per-engine caps.
- **Risk:** Duplicate outcome reports. **Mitigation:** one terminal account-outcome function is the sole reporter.
- **Risk:** Lost resize notification. **Mitigation:** coalesced changed channel plus latest-value read.

## Evidence log

- 2026-11-10: Baseline full gate green. Review confirmed both CLI paths use fixed loops and multi-engine bypasses `MultiChecker.Run`.
- 2026-11-10: Implemented `AdaptiveGovernor` resize notifications plus a reusable `ResizablePool`; both single-engine and multi-engine paths now use it. The pool never drops an accepted job, caps live workers at the requested maximum, and reports exactly one terminal account outcome.
- 2026-11-10: Added deterministic `TestWorkerPoolResizes`, `TestAccountOutcomeReportedExactlyOnce`, and `TestAdaptiveGovernorResize`; targeted verification passed.
- 2026-11-10: Full suite passed in 13.911s. The Windows race build remains unavailable because no `gcc`/C compiler is installed; this is recorded as an environment limitation rather than a code failure.
- 2026-11-10: Removed the obsolete local `.cline/skills/reddit-checker` directory at the user's request; no source, session, input, or output data was removed.

## Completion gate

- [x] All acceptance criteria are checked.
- [x] All implementation steps are `DONE`.
- [x] Verification evidence is recorded below.
- [x] Final status changed from `IN PROGRESS` to `COMPLETE`.
