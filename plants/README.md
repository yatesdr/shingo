# plants/ — the one plant

Verified against: 74c3eb77

`plants/master.yaml` is the plant spec the dev stack seeds (`docker-compose.dev.yml`
mounts this directory at `/plants`), the rig scripts drive, the planner golden is cut
on and the scenario families seed. **LOCAL DEV / SIMULATION ONLY**: no name, part or
number in it is a real plant's, and the live plants are configured through the Nodes
page, not from a spec. The eight specs it replaced (`demo`, `demo-sim`, `edge2`,
`lane-stress`, `lane-stress-packed`, `kanban-press`, `kanban-press5`, `order-carts`)
are in git history.

## The sections

A YAML key appears once, so each list in the file is composed of the sections' runs,
each opened by a banner `# ══ [tag] ...`; search for the tag to read a section whole.
The file's header is the full map. Every section is a closed loop on its own carrier
types, so no section's keeper or loader draws another's carts.

| Tag | What it is | Edge | Carriers |
|---|---|---|---|
| `[demo]` | The demo loop: PRESS-1/2 into `SYN_MARKET`, WELD-1..5, the BRKT/CLIP/STUD/SHIM loaders (multi-window, single-window, dedicated positions), UNLOADER-A into the `SYN_PRESS_EMPTIES` kanban, all four swap modes, the PRESS-1 tooling and carry-over changeovers | EDGE 1 | STANDARD, STANDARD-SM |
| `[multi-cart]` | `[demo]`'s PRESS-1 (press-index: R2 carries two carts) and WELD-2 (the single-robot relay: two carts, four lifts) | EDGE 1 | (demo's) |
| `[4x2]` | WELD-6: a two-robot weld, five consume positions fed by five loaders; RUN and ALT share COMMON-1..3 and run MIRROR-L / MIRROR-R at the two mirror positions; CATID pins 1001 / 1002; production counter `MES_WELD_6.Prod_Counter_01` (identity `CATID_01` beside it); load-station counter `STA_W6_LOAD.Load_Count` over all five positions; `line_depth` 16 in `shingoedge.dev.yaml` | EDGE 1 | CORRIDOR |
| `[corridor]` | The lane-stress storage, slot for slot: `SYN_STAMP` and `SYN_COMP`, single-file lanes of depth 1-5 behind their zones' wait points, the oldest carts buried at depth 5 and 4; two lanes' worth of headroom per group (`headroom: free_lanes: 2`). It is `[4x2]`'s market | (Core) | CORRIDOR |
| `[kanban]` | KP-PRESS (two-off, four positions) on `PRESS-KANBAN` (the 45x48 family and CART-32, pools 10 and 6), KW-WELD on `WELD-KANBAN`, their unloaders pushing back, and `KP-SM`, the plain supermarket a family deficit raids | EDGE 1 | SMALL-45, BIG-45, TOTES-45, CART-32 |
| `[kanban5]` | Five presses of KP-PRESS's shape on one `P5-KANBAN` (pools 26 and 22) through one unloader of ten windows; its raid supermarket is `KP-SM` | EDGE 1 | (kanban's) |
| `[window]` | Line 2, the observable-window line: LINE-2 (A/B direct and staged consume, a weld producing), its two loaders and its drain. Its processes name `edge: edge2.line1`: EDGE 2's operator pacing (60 s release, 30 s loads, `shingoedge.edge2.yaml`) is the window under test, and that pacing is per Edge | EDGE 2 | STANDARD-L2 |

Core is seeded from the whole file and every Edge it names is enrolled; each Edge is
seeded with its own processes (`seeddev --edge`, `plantspec.Plant.ForEdge`).
`make dev-seed` runs `seed` (Core, EDGE 1) and `seed-edge2` (EDGE 2).

## Which scenario family proves what

| Property | Family | Section |
|---|---|---|
| Changeover and counting: every reduction on the right cart, the holds, the production record, the deliveries | `integration/scenarios/catid_4x2_*_test.go` (S1-S9); `scripts/sim-4x2.sh` on the stack; the section's shape: plantspec `TestMaster4x2_*` | `[4x2]` |
| Lanes: the mouth gate, the dig walk, no air bubbles, two digs in one group | `shingo-core/cmd/seeddev` `TestLanesFamily_*` (docker); plantspec `TestCensus_*` (the frozen baseline's two bubbles replayed on the corridor), `TestMasterCorridor_*` | `[corridor]` |
| Kanban: the loop, the families, the pools, a family never loses a cart while whole, the costs | `cmd/seeddev` `TestKanbanRig_*`, `TestKanbanPin_*`, `TestSeedKanbanPress_*`, `TestKanbanPressFixture_*`; plantspec `TestKanbanPress*Plant_*`; simcalc `TestKanbanPress_*`; `scripts/kanban-rig.sh` R1-R6, W, P1-P6 on the stack (`docs/dev-env/kanban-rig.md`) | `[kanban]`, `[kanban5]` |
| Multi-cart: several carts on one order, the invariant after every injected state | `cmd/seeddev` `TestOrderCartsFixture_*` (docker) | `[multi-cart]` |
| The observable window: a changeover whose material lands minutes before it completes | EDGE 2 on the stack (its scheduled changeovers); the line's sizing: `cmd/seeddev` `TestEdge2Fixture_*` | `[window]` |
| The deciders on the demo loop | `cmd/seeddev` `TestPlannerGolden_*` (the golden), `TestSeedDemo_*`; plantspec `TestShippedDemoPlant*` | `[demo]` |
| The file as a whole: valid, clean at birth, every section marked, each on its own carriers | plantspec `TestMasterPlant_*`, `TestCommittedSpecsValidate` | all |

## Sized for the run

A sim run measures the code only while the plant is healthy, so `make dev-rates` and
`make dev-carriers` (simcalc, both Edge configs, `-run 400m`: a 40-minute run at 10x)
must show the plant lasting the run before a number taken on it counts.

- Every part is balanced, drained, loader-fed or IDLE (a loader whose part only a style
  not running consumes) but `[demo]`'s PANEL-A and PANEL-B; every pool holds its floor
  (`SYN_STAMP` 9 empties against 5, `SYN_COMP` 16 against 7, `SMN_L2` 7 against 7) but
  `SYN_MARKET`; every kanban stands at its want and every family at its pool.
- `[demo]`'s press-empties kanban is seeded to its family pools: the level is 4 STANDARD
  and 2 STANDARD-SM, and each pool adds the most positions each supported press runs
  in the family (8 and 6). Ten carts stand in its twelve positions and four on the
  presses, so the keeper is born quiet with two positions spare. Seeded at its level, as
  before, both families were born two short and the first tick borrowed.
- Two `[demo]` lines stay red, both demo-sim's and both nameplate: the rate check's
  PANEL-A/B STARVE, and the carrier check's `SYN_MARKET RATE DEFICIT 0.20/min — drains
  regardless of size`. simcalc fails any pool whose spends exceed its frees, whatever
  its stock; it costs WELD-1/2's ASSY carriers at their 6/min nameplate (0.30 bins/min
  each out of `SYN_MARKET`) where their swap cadence draws about a third of it (the
  PRESS-1 block of `shingoedge.dev.yaml`), and those carriers return through UNLOADER-A
  into the press empties, a kanban with no source to send its surplus back by. The
  measured tick rates are not retuned to the model. Re-measure before retuning.

## The seed checks itself

`plantspec.Validate` runs the census at birth (`census.go`): partially filled lanes
packed from the back (no air bubble) and two lanes' worth of free slots per group,
and the A/B two-container rule. A spec that fails does not seed. Read what it says
before trusting a number taken on a new section.

## Figures that moved with the master

- The planner golden was re-cut once on the master. Its moves are `[demo]`'s seed: the
  +5 PANEL-B stock (demo-sim's, which the golden's old plant lacked) and the
  press-empties kanban born at its pools.
- The kanban family's statement counts are the rig's own plant's (keeper tick 3), the
  master's kanbans being born at their pools.
- `[kanban5]` shares `[kanban]`'s raid supermarket: the five-press rig plant had its own,
  the same four carts.

## Tooling

| Tool | What it does |
|---|---|
| `shingo-core/cmd/simcalc` | Plant-spec arithmetic: rates, carrier pools, kanbans, the fleet. |
| `shingo-core/cmd/soakstat` | Reads a soak run and reports on it. |
| `scripts/soak-watch.sh` | Watches a running soak. |

A rig measurement taken before 2026-08-16 (`169e37c5`) ran on a clock with two speeds;
check when an old figure was taken before comparing against it. Core's loader switches
(an unloader's `auto_push`) live in the file's `loader_settings`, never on a claim: a
loader claim seeds no Edge claim row.
