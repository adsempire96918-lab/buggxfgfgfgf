# Task files — the workflow

Every implementation task in this project starts with a **scope-locked task
file** in this folder and ends only when the targeted verification proves the
requested behaviour works.

## Naming

```
tasks/YYYY-MM-DD-<short-kebab-name>.md
```

One file per task. Copy `tasks/TEMPLATE.md` to start a new one.

## Lifecycle

```
IN PROGRESS  ──►  COMPLETE
      │
      └──────►  BLOCKED   (an external/live dependency or 2 failed attempts)
```

| State | Meaning |
|---|---|
| `IN PROGRESS` | The task is being worked on. Steps update as they finish. |
| `BLOCKED` | It cannot proceed: an external dependency is missing, or the same approach failed twice. The evidence log says exactly what was tried. |
| `COMPLETE` | Every acceptance criterion is checked, every step is `DONE`, the evidence is recorded and the final gate passed. |

## The rules

1. **Create the task file BEFORE writing any code.** The scope lock, the
   acceptance criteria and the verification plan are decided first.
2. **Only the target files may change.** If a step needs another file, stop,
   widen the scope in the task file with the reason, then continue.
3. **Update the task file at every meaningful checkpoint** — the step table
   and the evidence log, not just at the end.
4. **Targeted verification first.** Run the task's own check before the
   project-wide gate.
5. **The project-wide gate before completion:**
   `gofmt -l . && go vet . && go build -o userkarmacheck.exe . && go test -count=1 ./...`
6. **Loop guard.** No repeated broad repository scans, no identical failing
   command without a changed hypothesis, no silent scope expansion, and
   **max 2 attempts per approach** — after that, mark the task `BLOCKED`
   with the evidence instead of looping.
7. **Live traffic is a separate terminal.** A heavy run saturates the network
   and kills the agent's own API connection. The agent may run unit tests and
   a light live run (≤ 16 threads, ≤ 8 req/s). The VPN live test runs at
   most twice, ever.

## What lives where

| File | Purpose |
|---|---|
| `tasks/TEMPLATE.md` | The reusable task-file template. |
| `tasks/README.md` | This file — naming, lifecycle and the rules. |
| `tasks/<task>.md` | One active or finished task. |
| `EXECUTION_PLAN.md` (project root) | The engineering BACKLOG — which feature to build next, with its own file scope and timebox. A task file is created from a backlog entry. |
| `TASKS.md` (project root) | The historical development log. Kept for the record; the active work lives in `tasks/`. |
