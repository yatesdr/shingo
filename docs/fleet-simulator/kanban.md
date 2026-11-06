# The Kanban rig

A permanent certification fixture for the Kanban feature: the `[kanban]` and `[kanban5]` sections of `plants/master.yaml`, six runs and a walk, one driver. The other sections of the master run beside the loop, and every count a run checks is the loop's own (`kanban-rig.sh` `orders_count`). The runs go on the visible dev stack (compose project `shingo-dev`, Core `:8083`, Edge `:8081`), one at a time, so every run can be watched on the pages while it goes.

Two tiers:

- **On every gate:** R1 and R5's loop, in the docker shards. `shingo-core/cmd/seeddev/kanban_rig_docker_test.go` (`TestKanbanRig_R1_SteadyLoop`, `TestKanbanRig_R5_Repayment`, `TestKanbanRig_R5_SMIsNotRepaid`) seeds the master whole, applies the slots' allowed types and drives the real keeper, doors and walk with no compose stack, in seconds. R1's test also asserts the statement counts, on the master as on the rig's own plant before it: keeper tick 3 with nothing short, snapshot build 9, intake door 8, parked arm 8. They hold on the master because every kanban in it is born at its pools. `TestKanbanRig_WalkCounts` logs them for the walk's states. They run plain moves in place of the press-index legs.
- **Before a merge to `main`:** `scripts/kanban-rig.sh R1` to `R6` on the stack is the certification pair, as line 2's (`[window]`) runs are.

## The plant

```
PRESS-KANBAN ──empties──→ KP-PRESS (4 positions) ──fulls──→ PRESS-UNLOAD (4 windows)
     ↑                                                           │
     └──────────── the unloader's CLEAR: PUSH AS the lit type ───┘

WELD-KANBAN  ──empties──→ KW-WELD (two-robot)    ──fulls──→ WELD-UNLOAD (2 windows)
     ↑                                                           │
     └────────────────────────── CLEAR ──────────────────────────┘

KP-SM              a plain supermarket: no level, stocked with 45x48 empties, nobody counts it
```

| Name | In the plant | What it is |
|---|---|---|
| K | `PRESS-KANBAN` | 16 flat children: `PKB_001..010` admit the 45x48 family, `PKB_011..016` admit `CART-32`; mix 2/2/2/2; pools 10 and 6; supports the press's 4 positions; no sources |
| K2 | `WELD-KANBAN` | 4 flat children; small 2; pool 3; supports the weld; no sources; of the same family as K and on no support of K's |
| SM | `KP-SM` | Lanes, no level, stocked with 4 of 45x48. The raid's target: a keeper asking per type would empty it through a changeover. A keeper asks only for its family's deficit, so it stands untouched while a family is whole |

**A slot says what it admits in the plant file:** `admits: [45x48]` takes a type code or a family code, and the seeder writes per-type rows.

**The pool.** 10 of 45x48: the 4 positions on small, and K's 2, 2 and 2 on `PKB_001..006`. 6 of 32x30, all on `PKB_011..016`. The weld's 3. A running type's want is its level plus its cover, the positions the style puts on the line: under `KP-SMALL` K wants small 6, and the 4 on the line are held, so small reads short 0.

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
bash scripts/kanban-rig.sh R1      # R1..R6, W, W5, W6, P1..P6
```

It refuses while `shingo-dev` is running, because it starts with `down -v`. `KANBAN_RIG_REPLACE=1` says that stack is yours to drop. `KANBAN_RIG_NO_BUILD=1` reuses the images. It takes the gate lock, so a gate and a run never overlap. It seeds with Core stopped, applies the slots' types, runs the injections, collects, prints the verdict and leaves the stack up. The exit code is the verdict's.

Each run writes under `../kanban-rig-runs/<run>-<stamp>/` (or `KANBAN_RIG_OUT`): `verdict.txt`; `long-orders.tsv`, every order over 10 sim-min by id, payload and cause; `stranded.txt` and `soakstat.txt` from `cmd/soakstat`; `no-door.txt`; samples every 30 s of the kanban cards, the windows and the unloader's board. Sim-minutes here are at the rig's 10x, so one is 6 wall-seconds. `family-watch.tsv` and `cart-watch.tsv` read found by place and every cart every 3 s; `orders.tsv` and `order-history.tsv` keep the orders past the next run.


| Run | Length | Injection | Passes when | What it catches |
|---|---|---|---|---|
| **R1** steady on small | 20 wall-min | None | K wants small 6; K2 wants 3; every unloader cart lit small; no park at a level; small reads short 0 with its 4 held on the line; found equals pool at every sample; `KP-SM` unchanged | Cover not read from the claims; the keeper raiding a whole family; a loop that drifts; the statement counts belong here too, and they come from the cost tests, not this script |
| **R2** small → totes | To cutover + 10 sim-min | START `KP-TOTES` at 20 sim-min, CUTOVER at the gate | Cover moved at START (totes 6, small 2); K holds 2/2/2 and the 4 positions totes; K2 took nothing from K; nothing delivered to SM | Cover following the active style instead of the target; carts off the process not re-typed |
| **R3** totes → 32x30 → totes | Each to cutover + 10 sim-min | START and CUTOVER, three times | Under `KP-32`: K wants 32x30 6; the 4 positions hold 32x30; the 4 of 45x48 land and none stands on a window past its unload. Back on `KP-TOTES`: K holds its mix, the positions totes, its 32x30 back at 6 and none parked | Crossing a family: what comes off the process when K is at its level, and whether the fulls keep a window |
| **R4** flip-flop | To cutover + 10 sim-min | START totes, CANCEL once two positions hold totes, START big, CUTOVER | Cover reverts within a tick; no leg left live; the next START lifts the totes | Cancel leaving orders or cover behind; a stranded order; a cart on a window with no door |
| **R5** what K does whole, and SM | About 10 wall-min | 4 carts registered on K's free slots until it is whole, one more stood on a window. Then one of K's small carried to SM with K short by 2 or more | The card says no group is short and the cart lands while a slot admits it; with none it parks named at K as full, naming the family, and leaves when a slot frees. K takes SM's cart for its family's deficit only, nothing delivers to SM, and no ask of K stands queued on an empty that does not exist | A loop cart refused at its level; what a plain group gets back; asks minted past the carts that exist |
| **R6** raiding | About 5 wall-min | K2's two carried into K by hand | K stands above its pool and K2 below; K2's keeper takes K's carts above its pool before any other and none K owns; K ends at 10 of 10 and SM gives none; the weld's orders move | A kanban giving a cart it owns; a stranger with no way home; K read at its pool while its carts are on robots |

Every run also fails on a stranded order with no releaser (`soakstat -stranded`), on an empty standing on a window with no live order for 20 sim-min, and on all four press windows held across two samples.

## The walk

`scripts/kanban-rig.sh W`, `W5` and `W6` follow the carts one by one and hold the acceptance states: a step that cannot proceed fails, marks why, and the walk goes on to collect. Each adds samples every 3 wall-seconds.

- `walk.tsv`: wall time, sim clock (Core `/api/sim/status`), and for every carrier but the weld's own 3: label, type, node, its group, claimed order.
- `walk-orders.tsv`: each order touching K, the 4 positions or the 4 windows, live or moved in the last sim-minute: id, type, payload, source, delivery, status, queue cause, age in seconds on the orders' clock.
- `walk-cards.jsonl`: K's card (`/api/nodes/kanban/levels`) and the unloader's board view, whose tiles carry each window's `assignment`.
- `walk-log.txt`: the steps' marker lines.

| Run | Steps |
|---|---|
| **W** | Settle on small 20 sim-min, `STEP1`. START `KP-32`, `STEP2-START`; gate open (15 wall-min at most, else `GATE NEVER OPENED`), `GATE-OPEN`; CUTOVER; 20 sim-min, `STEP3`. START `KP-BIG`, `STEP4-START`; gate open; CUTOVER; 20 sim-min, `END` |
| **W5** | Settle on small 20 sim-min. START `KP-32`; when two positions hold 32x30, CANCEL; 10 sim-min. START `KP-BIG`; gate open; CUTOVER; 10 sim-min, `END` |
| **W6** | START `KP-BIG`, CUTOVER, 10 sim-min. Every cart of the family in K set big on the bin page: K B6, C6. START `KP-SMALL`; gate open; CUTOVER; 20 sim-min. K ends S2 B2 T2 C6: 8 re-types, the line's 4 at the windows on their way back, K's 4 through the arm |
| **P1** | Five presses steady on small, 20 wall-min, on the master's `[kanban5]` |
| **P2** | Press 3 onto 32x30; then presses 1 and 2 onto 32x30 in one cycle as press 3 returns to small |
| **P3** | Press 1 onto big, every standing cart set big, back to small |
| **P4** | A cart carried out by hand: the family one short, one ask brings it home |
| **P5** | Presses 1 and 2 onto 32x30, press 2 cancels with two positions held. `KANBAN_RIG_P5_CANCEL_ON_ROBOT=1` holds the CANCEL for a 32x30 on a robot bound for press 2 |
| **P6** | Presses 1 and 2 onto big in one cycle |

## Leaving the family

A return always lands: the level governs the keeper's asks, never a cart of the loop. `simcalc -carriers` prints each family's pool, minimum, room and carts, and fails a file that does not hold its pool or whose pool will not fit.

| Step | What moves | K ends |
|---|---|---|
| Steady on small | Line S4 | S2 B2 T2 C6, short 0 everywhere |
| START small → 32x30 | The 4 smalls land; 4 32x30 out in parallel | S6 B2 T2 C2 |
| Steady on 32x30 | Nothing lights, nothing moves; floaters 4 small | S6 B2 T2 C2 |
| START 32x30 → big | The 4 32x30 land; 2 bigs straight; the 4 floaters cycle through the windows lit BIG | S2 B2 T2 C6, floaters 0 |
| START big → small, all ten set big | 8 re-types: the line's 4 at the windows on the way back, K's 4 extras through the arm | S2 B2 T2 C6 |
| CANCEL mid-way | Cover reverts, placed carts stay, nothing parks, the next START proceeds | |

## The five presses

The master's `[kanban5]`: five presses of this shape, one shared unloader `P5-UNLOAD` of 10 windows, one kanban `P5-KANBAN` over all 20 positions, mix 2/2/2/2, pools 26 and 22, 48 slots admitting by family. Its plain supermarket is `[kanban]`'s `KP-SM` (the five-press rig plant had its own `P5-SM`, the same four carts; one raid target serves both loops). One name in `bin_loaders.outbound_dest` is why sharing the unloader means one kanban.

Runs: R1 on all five, steady, 20 wall-min; two presses leaving 45x48 in one cycle while a third returns; the all-bigs case on one press; a hand move between positions and the kanban; CANCEL on one press while another changes over; two presses STARTing onto big in one cycle.

Both rigs, every run: found equals pool per family at every sample; the supermarket unchanged; no loop cart parked at a level; no window held past its unload and a tick; every START's gate opens; a keeper tick of at most 4 statements whatever the press count.
