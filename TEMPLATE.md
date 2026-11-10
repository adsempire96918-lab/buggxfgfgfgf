# Task: <one-line title>

- **Status:** IN PROGRESS
- **Created:** YYYY-MM-DD
- **Owner:** <agent / human>
- **Goal:** <one sentence: what must be true when this task is done>

## Scope lock

### Target files (only these may be changed)

| Absolute path | Planned change |
|---|---|
| `C:\...` | <what changes here> |

### Explicitly out of scope

- <files, folders and behaviours that must NOT be touched>
- `C:\Users\Rakib\Desktop\userkarmacheck-go\sassion\`, `...\full_out\`, `...\input.txt`
- Live Reddit traffic, VPN rotations and load tests (unless the task is
  explicitly about them)

## Acceptance criteria

- [ ] <observable, testable statement>
- [ ] <another one>
- [ ] The project-standard gate passes: `gofmt -l .`, `go vet .`,
      `go build -o userkarmacheck.exe .`, `go test -count=1 ./...`

## Ordered implementation steps

| # | Step | Target | Verification | Status |
|---|---|---|---|---|
| 1 | <step> | `<file>` | <how to prove it> | PENDING |

## Verification plan

1. <the targeted check for this task>
2. From `C:\Users\Rakib\Desktop\userkarmacheck-go`:
   - `gofmt -l .` (expected: empty output)
   - `go vet .`
   - `go build -o userkarmacheck.exe .`
   - `go test -count=1 ./...`
3. <any live check — only if the task demands it, and only in a separate
   terminal that the USER starts>

## Loop prevention / stop rules

- Read only the named target files plus the minimum dependency context; do
  not repeatedly scan the whole repository.
- If a check fails, record the exact failure, form ONE new hypothesis, make
  the smallest in-scope change, and rerun only the failing check.
- **Do not run the same failing command twice without changed code or a
  changed hypothesis.**
- **Max 2 attempts per approach.** After that, mark the task `BLOCKED` with
  the evidence and move on.
- If a required change falls outside the target list, stop, update this scope
  and the rationale first, then continue.
- If an external/live dependency is required, mark the task `BLOCKED` with
  evidence instead of looping or claiming completion.

## Risks and mitigations

- **Risk:** <what could go wrong> **Mitigation:** <how it is contained>

## Evidence log

- YYYY-MM-DD: <what was done and what it proved>

## Completion gate

- [ ] All acceptance criteria are checked.
- [ ] All implementation steps are `DONE`.
- [ ] Verification evidence is recorded below.
- [ ] Final status is changed from `IN PROGRESS` to `COMPLETE`.
