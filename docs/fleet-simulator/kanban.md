# The Kanban rig

A permanent certification fixture for the Kanban feature: `plants/kanban-press.yaml`, six runs and a walk, one driver. The runs go on the visible dev stack (compose project `shingo-dev`, Core `:8083`, Edge `:8081`), one at a time, so every run can be watched on the pages while it goes.

Two tiers:

- **On every gate:** R1 and R5's loop, in the docker shards. `shingo-core/cmd/seeddev/kanban_rig_docker_test.go` (`TestKanbanRig_R1_SteadyLoop`, `TestKanbanRig_R5_Repayment`, `TestKanbanRig_R5_SMIsNotRepaid`) seeds this plant file, applies the slots' allowed types and drives the real keeper, doors and walk with no compose stack, in seconds. R1's test also asserts the statement counts: keeper tick 9 with K short, as it runs here, and 4 with nothing short; snapshot build 10, intake door 8, parked arm 8. `TestKanbanRig_WalkCounts` logs them for the walk's states. They run plain moves in place of the press-index legs.
- **Before a merge to `main`:** `scripts/kanban-rig.sh R1` to `R6` on the stack is the certification pair, as `edge2.yaml`'s runs are.

## The plant

```
PRESS-KANBAN ──empties──→ KP-PRESS (4 positions) ──fulls──→ PRESS-UNLOAD (4 windows)
     ↑                                                           │
     └──────────── the unloader's CLEAR: PUSH AS the lit type ───┘

WELD-KANBAN  ──empties──→ KW-WELD (two-robot)    ──fulls──→ WELD-UNLOAD (2 windows)
     ↑                                                           │
     └────────────────────────── CLEAR ──────────────────────────┘

KP-SM              a plain supermarket: no level, seeded empty, nobody counts it
```

| Name | In the plant | What it is |
|---|---|---|
| K | `PRESS-KANBAN` | 16 flat children: `PKB_001..010` allow small, big and totes, `PKB_011..016` allow 32x30; levels small 2, big 2, totes 2, 32x30 2; cover 4; supports the press's 4 positions; no sources, no overflow |
| K2 | `WELD-KANBAN` | 4 flat children; small 1; cover 1; supports the weld; no sources, no overflow; its own 3 carts |
| SM | `KP-SM` | Lanes, no level, seeded empty. A keeper with no sources searches plant-wide, so an empty standing here is pulled into a short kanban at once. R5 puts one here by hand |

**The slots' allowed types are not in the plant file.** `plantspec.Slot` has a name and a depth and the seeder writes no `node_bin_types`, so the driver applies them right after the seed, through two of the Nodes page's doors (`/api/nodes/bin-types`, `/api/nodes/properties/set` with `bin_type_mode`): `apply_slot_types`, with the table at the top of the script. The docker tests apply the same through `SetNodeBinTypes` and `SetNodeProperty`. `simcalc` reads the plant file and so cannot see them either: it counts a floor's carts against a floor's wants.

**The pool.** 10 of 45x48: the 4 positions on small, and K's 2, 2 and 2 on `PKB_001..006`. 6 of 32x30, all on `PKB_011..016`. The weld's 3. So K's card reads small want 6, resident 2 on `KP-SMALL`: its cover is the carts on the positions, K is 4 short for as long as the press runs, nothing outside it can source one, and the line says "no empty SMALL-45 outside the group". `simcalc -carriers` reads this as BELOW WANT by 4 and exits 1: it is true of the fixture and left true.

The press runs five styles on the same positions: `KP-SMALL` (two parts, left and right), `KP-BIG`, `KP-TOTES`, `KP-32`, and `KP-UNTYPED`, whose part has no carrier row and is named on the kanban's card. The unloaders are the consumers: a full goes straight to a window, the parts come off, the empty goes back. The file's head has the pool arithmetic. `simcalc -carriers` prints the wants, what starves first and what each changeover leaves.

## The operator

The sim operator works an unloader's panel the way a person does when the sim config names its station: `sim.operators.unloader_taps` in `shingo-edge/shingoedge.dev.yaml`, by operator station code. It reads the same assignment the board lights (`service.AssignmentAt`) and posts to the panel's doors.

| Setting | A full at the window | An empty at the window |
|---|---|---|
| `lit`, something lit | CLEAR as the lit type | PUSH AS the lit type |
| `lit`, nothing lit | plain CLEAR | PUSH EMPTY |
| `empty` | plain CLEAR | PUSH EMPTY |
| station not listed | plain CLEAR | left standing |

Both rig stations ship as `lit`. To see the `empty` case (a cart parks against K until something is short), change the setting and rebuild the Edge image: the config is baked in.

## The runs

```sh
bash scripts/kanban-rig.sh R1      # R1..R6, W, W5
```

It refuses while `shingo-dev` is running, because it starts with `down -v`. `KANBAN_RIG_REPLACE=1` says that stack is yours to drop. `KANBAN_RIG_NO_BUILD=1` reuses the images. It takes the gate lock, so a gate and a run never overlap. It seeds with Core stopped, applies the slots' types, runs the injections, collects, prints the verdict and leaves the stack up. The exit code is the verdict's.

Each run writes under `../kanban-rig-runs/<run>-<stamp>/` (or `KANBAN_RIG_OUT`): `verdict.txt`; `long-orders.tsv`, every order over 10 sim-min by id, payload and cause; `stranded.txt` and `soakstat.txt` from `cmd/soakstat`; `no-door.txt`; samples every 30 s of the kanban cards, the windows and the unloader's board. Sim-minutes here are at the rig's 10x, so one is 6 wall-seconds.

A check that fails on this fixture stays failing. R3's and R5's say what the fixture leaves, below.

| Run | Length | Injection | Passes when | What it catches |
|---|---|---|---|---|
| **R1** steady on small | 20 wall-min | None | K wants small 6; K2 wants 2; every unloader cart lit small; no park at a level; K's small rests at resident 2 and says "no empty SMALL-45 outside the group"; no other lineside line held across two samples | Cover counted per position instead of per process; a loop that drifts; the statement counts belong here too, and they come from the cost tests, not this script |
| **R2** small → totes | To cutover + 10 sim-min | START `KP-TOTES` at 20 sim-min, CUTOVER at the gate | Cover moved at START (totes 6, small 2); K holds 2/2/2 and the 4 positions totes; K2 took nothing from K; nothing delivered to SM | Cover following the active style instead of the target; carts off the process not re-typed |
| **R3** totes → 32x30 → totes | Each to cutover + 10 sim-min | START and CUTOVER, three times | Under `KP-32`: K wants 32x30 6 and says what holds it; the 4 positions hold 32x30; no 45x48 parked at K's level on a window. Back on `KP-TOTES`: K holds 2/2/2, the positions totes, its 32x30 back at 6 and none parked | Crossing a floor with no overflow: what comes off the process when K is at its level, and whether the fulls keep a window |
| **R4** flip-flop | To cutover + 10 sim-min | START totes, CANCEL once two positions hold totes, START big, CUTOVER | Cover reverts within a tick; no leg left live; the next START lifts the totes | Cancel leaving orders or cover behind; a stranded order; a cart on a window with no door |
| **R5** what K does whole, and SM | About 10 wall-min | 4 carts registered on K's free slots until it is whole, one more stood on a window. Then one of K's small carried to SM with K short by 2 or more | The card says no group is short, the cart parks named at K and leaves when K is short. K takes SM's cart by its plant-wide search, nothing delivers to SM, and no ask of K stands queued on an empty that does not exist | A push with no source and no overflow; what a plain group gets back; asks minted past the carts that exist |
| **R6** raiding | About 5 wall-min | K2's two carts carried into K by hand | K holds carts; K2's keeper takes nothing from K, then or after settle; with nothing to source the keeper asks nothing, and K2's card says `held_by`: no empty SMALL-45 outside the group. The carts carried back: the weld's orders move | A lineside kanban sourcing from another lineside kanban |

Every run also fails on a stranded order with no releaser (`soakstat -stranded`), on an empty standing on a window with no live order for 20 sim-min, and on all four press windows held across two samples.

## The walk

`scripts/kanban-rig.sh W` and `W5` are for following the carts one by one, not for a verdict: a step that cannot proceed marks why and the walk goes on to collect. Every 3 wall-seconds from the run's start:

- `walk.tsv`: wall time, sim clock (Core `/api/sim/status`), and for every carrier but the weld's own 3: label, type, node, its group, claimed order.
- `walk-orders.tsv`: each order touching K, the 4 positions or the 4 windows, live or moved in the last sim-minute: id, type, payload, source, delivery, status, queue cause, age in seconds on the orders' clock.
- `walk-cards.jsonl`: K's card (`/api/nodes/kanban/levels`) and the unloader's board view, whose tiles carry each window's `assignment`.
- `walk-log.txt`: the steps' marker lines.

| Run | Steps |
|---|---|
| **W** | Settle on small 20 sim-min, `STEP1`. START `KP-32`, `STEP2-START`; gate open (15 wall-min at most, else `GATE NEVER OPENED`), `GATE-OPEN`; CUTOVER; 20 sim-min, `STEP3`. START `KP-BIG`, `STEP4-START`; gate open; CUTOVER; 20 sim-min, `END` |
| **W5** | Settle on small 20 sim-min. START `KP-32`; when two positions hold 32x30, CANCEL; 10 sim-min. START `KP-BIG`; gate open; CUTOVER; 10 sim-min, `END` |

## Leaving the floor

`simcalc -carriers` prints this as CHANGEOVER ROOM. A changeover within 45x48 conserves: K's cover moves from one type to another of the floor and the unloader re-types the carts. Leaving the floor does not: the 4 positions and K's cover both leave.

| Running | 45x48 carts | K's 45x48 wants + positions | 32x30 carts | K's 32x30 want + positions |
|---|---|---|---|---|
| `KP-SMALL`, `KP-BIG`, `KP-TOTES` | 10 | 10 + 4: K 4 short | 6 | 2: 4 past K's level |
| `KP-32` | 10 | 6: 4 past K's level | 6 | 6 + 4: K 4 short |

K refuses a store at its level, and it has no source and no overflow, so the 4 past its level have nowhere to land if they come off the process. On a window, each is lit nothing, tapped PUSH EMPTY and parks named at K, and 4 of them are all 4 windows, which leaves `KP-32`'s fulls none. As seeded, the 32x30 stand above their want in their own slots: nothing moves them there. Once they have left K, the same arithmetic holds on the way back.

simcalc counts a floor's carts against a floor's wants. It cannot see which slots allow which type, because the plant file cannot say.
