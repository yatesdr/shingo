# Changeover Automation & A/B Cycling

## Overview

Changeover automation handles production line tooling changes (style A → style B). Edge wiring advances node task states on order completion: `staging_requested → staged → line_cleared → released`. Auto-staging (Phase 2) fires at changeover start for all swap/add positions. Keep-staged modes handle pre-staged bins with either a single robot (combined) or two robots (split). A/B cycling manages paired consume nodes with `active_pull` switching — only the active node decrements UOP, the inactive holds staged material as buffer.

Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `engine/changeover_test.go` — full changeover lifecycle, auto-staging, error/retry, cancel, keep-staged (TC-74, 76, 61, 73)
- `engine/wiring_test.go` — A/B cycling and FlipABNode (TC-72)
- `engine/operator_stations_test.go` — order acceptance guards (TC-61)
- `engine/changeover_diff_test.go` — DiffStyleClaims pure function unit tests (TC-86 through TC-89)
- `engine/step_builders_test.go` — Build*Steps pure function unit tests (TC-90)

Run this domain's tests:

```bash
cd shingo-edge
go test -v -run "TestChangeover|TestWiring_ABCycling|TestWiring_FlipABNode|TestCanAcceptOrders|TestDiffStyleClaims|TestBuildSwapChangeover|TestBuildEvacuateChangeover|TestBuildKeepStaged|TestBuildStage|TestBuildRelease|TestBuildRestore" ./engine/ -timeout 60s
```

The TC-61…TC-108 case ledger that used to sit below this line was a test report —
what was run on a date and what it found — not reference, so it now lives outside
the repo with the other reports. The tests themselves are the live record; the
files above are where to read them.
