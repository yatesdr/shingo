# Changeover Automation & A/B Cycling

## Overview

Changeover automation handles production line tooling changes (style A → style B). Edge wiring advances node task states on order completion: `staging_requested → staged → line_cleared → released`. Auto-staging (Phase 2) fires at changeover start for all swap/add positions. A claim that names a keep-staged node keeps one spare standing there, and its changeover legs fetch the incoming carrier from it instead of the market, in every swap mode; a running line's swap takes it when a right spare stands there and fetches from the market otherwise. A/B cycling manages paired consume nodes with `active_pull` switching — only the active node decrements UOP, the inactive holds staged material as buffer.

Edge tests use SQLite in a temp directory — no Docker required.

## Test files

- `engine/changeover_test.go` — full changeover lifecycle, auto-staging, error/retry, cancel, keep-staged (TC-74, 76, 61, 73)
- `engine/wiring_test.go` — A/B cycling (TC-72); the release flip is `engine/sequential_release_flip_test.go`
- `engine/operator_stations_test.go` — order acceptance guards (TC-61)
- `engine/changeover_diff_test.go` — DiffStyleClaims pure function unit tests (TC-86 through TC-89)
- `engine/step_builders_test.go` — Build*Steps pure function unit tests (TC-90)

Run this domain's tests:

```bash
cd shingo-edge
go test -v -run "TestChangeover|TestWiring_ABCycling|TestSequential|TestCanAcceptOrders|TestDiffStyleClaims|TestBuildSwapChangeover|TestBuildEvacuateChangeover|TestBuildStage|TestBuildRelease|TestBuildRestore" ./engine/ -timeout 60s
```

## The 4x2-shaped cell, end to end

`integration/scenarios/catid_4x2_*_test.go` (`-tags docker`) run Core and an Edge in one process on a
five-position, two-style weld (RUN and ALT share three parts and differ at two mirror positions, CATIDs
1001/1002 pinned) whose PLC is `simwarlink.Line`, turned one machine cycle and one Edge poll pass at a
time. Each shape checks the carts against the line's loads, the holds ledger, and the production record
(counter_snapshots, hourly, what Core received), and that every order lands confirmed: S1 a steady run on
the load station's counter and on the cell counter, S2 the late cutover (the declaration on a green gate,
the counted-part force on a red one), S3 the early cutover, S4 a jump with no changeover (nothing moves; the
wrong-part item offers the changeover, untapped and tapped), S5 an unmapped part (Charge and Write off), S6
a mixed cell, S7 a restart, S8 the PLC moving its count to a sibling MES slot (the slot watch's item, the
click that re-points and counts the strokes since the move), S9 the hand-placed cart; and
`TestPinChangeover_ACommonPositionIsResuppliedWhileOpen` a common position supplied through an open
changeover. `make dev-sim-4x2 SHAPE=S3` (`scripts/sim-4x2.sh`; `VARIANT=tap` taps S4's and S9's item) runs
one shape on the dev stack for a person to watch and prints the same ledger line.

```bash
cd integration
go test -tags docker -count=1 -run Catid4x2 ./scenarios/
```

The TC-61…TC-108 case ledger that used to sit below this line was a test report —
what was run on a date and what it found — not reference, so it now lives outside
the repo with the other reports. The tests themselves are the live record; the
files above are where to read them.
