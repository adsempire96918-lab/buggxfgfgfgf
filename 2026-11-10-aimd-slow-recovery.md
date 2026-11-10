# Task: T-01 — AIMD slow recovery and guarded recovery probe

- **Status:** COMPLETE
- **Created:** 2026-11-10
- **Owner:** Cline
- **Goal:** Make the AIMD controller recover slowly after a block or VPN rotation, using a single guarded recovery probe so it cannot walk rapidly back into Reddit's rate limit.

## Scope lock

### Target files (only these may be changed)

| Absolute path | Planned change |
|---|---|
| `C:\Users\Rakib\Desktop\userkarmacheck-go\aimd.go` | Implement slow additive recovery, guarded single recovery probe, and rotation-aware reset state. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\lanes.go` | Use the new AIMD reset/recovery API after VPN rotation and apply probe state safely. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\engine_test.go` | Add deterministic AIMD recovery/probe regression tests required by the backlog card. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\EXECUTION_PLAN.md` | Mark T-01 DONE only after its acceptance criteria and verification pass. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\PROJECT_MAP.md` | Document the new AIMD recovery behavior. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\.cline\skills\reddit-checker\SKILL.md` | Update the project playbook with the verified AIMD behavior. |
| `C:\Users\Rakib\Desktop\userkarmacheck-go\userkarmacheck-go_source_backup.zip` | Recreate the source-only backup required by the project definition of done. |

### Explicitly out of scope

- `C:\Users\Rakib\Desktop\userkarmacheck-go\sassion\`, `C:\Users\Rakib\Desktop\userkarmacheck-go\full_out\`, and `C:\Users\Rakib\Desktop\userkarmacheck-go\input.txt` must not be changed.
- T-02 through T-10 implementation.
- Live Reddit traffic, VPN rotation, load tests, and credential/session changes.
- Source backup cleanup is not allowed; the source-only zip may only be recreated with the same file selection.

## Acceptance criteria

- [x] AIMD decrease is multiplicative and recovery is materially slower (planned `+0.05` per 40 clean samples).
- [x] A recovery probe is single-shot, rate-limited to one probe per 10 seconds, and a blocked probe causes an immediate further decrease.
- [x] VPN rotation resets to the configured minimum without losing the last known-good multiplier; recovery never jumps directly to that value.
- [x] Deterministic tests cover slower-than-shrink recovery, single-flight probe behavior, blocked-probe decrease, and gradual post-reset recovery.
- [x] Targeted verification passes: `go test -count=1 -run "TestAIMD" -v .`.
- [x] The project-standard gate passes: `gofmt -l .`, `go vet .`, `go build -o userkarmacheck.exe .`, `go test -count=1 ./...`.

## Ordered implementation steps

| # | Step | Target | Verification | Status |
|---|---|---|---|---|
| 1 | Capture baseline and inspect AIMD call sites | `aimd.go`, `lanes.go`, `engine_test.go` | Existing project gate was green before changes | DONE |
| 2 | Design and implement slow recovery + guarded probe | `aimd.go` | Focused unit tests | DONE |
| 3 | Integrate rotation reset without fixed 0.5 reset | `lanes.go` | Focused AIMD tests and race check | DONE |
| 4 | Add deterministic regression coverage | `engine_test.go` | `go test -count=1 -run "TestAIMD" -v .` | DONE |
| 5 | Run full project gate and update project records | `EXECUTION_PLAN.md`, `PROJECT_MAP.md` | Full gate | DONE |

## Verification plan

1. `go test -count=1 -run "TestAIMD" -v .`
2. `go test -race -count=1 -run "TestAIMD" -v .`
3. From `C:\Users\Rakib\Desktop\userkarmacheck-go`:
   - `gofmt -l .` (expected: empty output)
   - `go vet .`
   - `go build -o userkarmacheck.exe .`
   - `go test -count=1 ./...`
4. No live traffic is required for this controller-only task.

## Loop prevention / stop rules

- Read only the named target files plus minimum dependency context.
- If a check fails, record the exact failure, form one new hypothesis, make the smallest in-scope change, and rerun only the failing check.
- Do not run the same failing command twice without changed code or a changed hypothesis.
- Max 2 attempts per approach; after that mark the task `BLOCKED` with evidence.
- If a required change falls outside the target list, stop and widen this scope with rationale first.

## Risks and mitigations

- **Risk:** Concurrent workers could all consume the same recovery probe. **Mitigation:** probe admission is mutex-protected and returns at most one winner.
- **Risk:** A reset could restore a hot rate immediately. **Mitigation:** reset stores the known-good ceiling but keeps the live multiplier at the minimum and advances only through controlled probe/growth steps.
- **Risk:** Existing AIMD semantics could break lane rate control. **Mitigation:** preserve the public constructor/report/multiplier contract and run the complete existing test suite.

## Evidence log

- 2026-11-10: Baseline `gofmt -l .`, `go vet .`, `go build -o userkarmacheck.exe .`, and `go test -count=1 ./...` all passed before implementation.
- 2026-11-10: Implemented valid-only AIMD sample accounting (fixed unused ring slots being counted as blocks), slow `+0.05`/40-clean additive growth, mutex-protected single-flight 5% recovery probe, 10-second probe interval, epoch-safe stale outcomes, and rotation reset that preserves the known-good ceiling while starting at the floor.
- 2026-11-10: Integrated request tickets in `lanes.go`; serialized AIMD rate application with `aimdMu`; live/debug panels now derive the displayed rate from the controller instead of an unlocked limiter read.
- 2026-11-10: Targeted `go test -count=1 -run "TestAIMD" -v .` passed all 6 deterministic tests. `go test -race` could not build because this Windows environment has no `gcc`; this is an external toolchain limitation, not a test failure.
- 2026-11-10: Full gate passed: `gofmt -l .` empty, `go vet .` clean, `go build -o userkarmacheck.exe .` succeeded, and `go test -count=1 ./...` passed in 13.935s.
- 2026-11-10: Recreated `C:\Users\Rakib\Desktop\userkarmacheck-go_source_backup.zip` with 55 source/document/task files (171,803 bytes), excluding `sassion/`, `full_out/`, `input.txt`, binaries, and output data.

## Completion gate

- [x] All acceptance criteria are checked.
- [x] All implementation steps are `DONE`.
- [x] Verification evidence is recorded below.
- [x] Final status is changed from `IN PROGRESS` to `COMPLETE`.
