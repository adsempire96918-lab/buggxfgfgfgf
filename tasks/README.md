# Scope-locked task records

One file per `EXECUTION_PLAN.md` task. A task file is written **before** the
code, locks the file scope, and gets the verification output pasted in when the
task is done. If a task needs a file that is not in its scope, the scope is
widened in `EXECUTION_PLAN.md` first — never silently.

| Task | Record | Result |
|---|---|---|
| T-03 | `T-03-shared-breaker.md` | one circuit breaker per exit IP, single-flight half-open probe |
| T-04 | `T-04-tokenbucket-exact-wait.md` | `Wait` reserves the slot and sleeps once (GCRA) |
| T-05 | `T-05-jsc-single-flight.md` | one jsc solve per exit IP, waiters reuse the cookie |
| T-06 | `T-06-progress-throttle.md` | 20 lines/s, HIT/ERROR/SUSPENDED never dropped |
| T-07 | `T-07-per-session-rate.md` | session rate = base × score, 0.5x–1.5x |
| T-08 | `T-08-keepalive-metric.md` | real new-vs-reused connection metric + reuse ratio |
| T-09 | `T-09-region-health-routing.md` | healthiest region wins, burned region cooled down |
| T-10 | `T-10-multi-ip-budget.md` | N ips × full budget, independence + total proven |

T-01 and T-02 were finished before this work started; T-02's missing
documentation/verification is recorded in `EXECUTION_PLAN.md` itself.
