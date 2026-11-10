# T-09 — Health-based exit-IP routing (SCOPE LOCK)

**Status:** DONE — 2026-11-09
**Timebox:** 60 min
**Plan card:** `EXECUTION_PLAN.md` → T-09

## Problem
`SourceHealthTracker` recorded which session/proxy works, but nothing routed on
it. The VPN rotated to the NEXT region in the list — so a region that had just
served a block page could be picked again straight away.

## Files in scope
| File | Change |
|---|---|
| `vpn.go` | `regionHealth` per region, `RecordRegionOutcome`, `RegionScore`, `RegionStats`, health-aware `nextRegion` + `nextRegionDifferentCountry`, outcomes recorded for every rotation attempt, `ReportIPBlocked` burns the current region |
| `advanced.go` | `SourceHealthTracker.PathHealth()` — the measured "how much of the exit path works" share |
| `lanes.go` | the rotation reason now carries the path health (`blocked page (path health 3/8 = 38%)`), so the history says WHY |
| `vpn_region_health_test.go` | **NEW** regression tests |

## Scoring
`score = (ok+1)/(ok+bad+2)` (Laplace), **minus 0.75** while the region is inside
its 10-minute block cooldown.

| region state | score |
|---|---|
| untried | 0.50 (neutral → pool order decides) |
| 2 clean rotations | 0.75 |
| blocked < 10 min ago | negative (below every healthy region) |
| blocked, cooldown over | 0.33 (eligible again, still behind a clean one) |

The pool list stays the fallback order: equal scores are resolved by walking the
list from the round-robin cursor, which is exactly the old behaviour on a fresh
controller.

## Verification
```
go test -count=1 -run "TestVPNPrefersHealthyRegion|TestVPNRotationSkipsBlockedRegion|TestVPNABlockPageScoresTheCurrentRegion|TestSourcePathHealth"
--- PASS: TestVPNPrefersHealthyRegion
--- PASS: TestVPNRotationSkipsBlockedRegion   # connect germany-1 never appears
--- PASS: TestVPNABlockPageScoresTheCurrentRegion  # usa-east blocked -> germany-1
--- PASS: TestSourcePathHealth
```
Full gate clean apart from the pre-existing Windows-only `TestMockCtlEndToEnd`.

## Acceptance
- [x] A region that was blocked is not picked again immediately (10-min cooldown + score)
- [x] The healthiest region wins; the pool order only breaks ties
- [x] `go test -count=1 ./...` passes (same single platform failure as baseline)
