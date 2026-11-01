# The Kanban rig

A permanent certification fixture for the Kanban feature: `plants/kanban-press.yaml`, six runs, one driver. The runs go on the visible dev stack (compose project `shingo-dev`, Core `:8083`, Edge `:8081`), one at a time, so every run can be watched on the pages while it goes.

Two tiers:

- **On every gate:** R1 and R5's loop, in the docker shards. `shingo-core/cmd/seeddev/kanban_rig_docker_test.go` (`TestKanbanRig_R1_SteadyLoop`, `TestKanbanRig_R5_Repayment`) seeds this plant file and drives the real keeper, doors and walk with no compose stack, in seconds. R1's test also asserts the statement counts: steady tick 4, snapshot build 10, intake door 8, parked arm 8. It runs plain moves in place of the press-index legs.
- **Before a merge to `main`:** `scripts/kanban-rig.sh R1` to `R6` on the stack is the certification pair, as `edge2.yaml`'s runs are.

## The plant

```
PRESS-KANBAN ──empties──→ KP-PRESS (4 positions) ──fulls──→ PRESS-UNLOAD (4 windows)
     ↑                                                           │
     └──────────── the unloader's CLEAR: PUSH AS the lit type ───┘

WELD-KANBAN  ──empties──→ KW-WELD (two-robot)    ──fulls──→ WELD-UNLOAD (2 windows)
     ↑                                                           │
     └────────────────────────── CLEAR ──────────────────────────┘

ZONE-45, ZONE-32   the lenders: levels, no supports, no keeper
ZONE-45-SPARE      plain and flat, no level: K's overflow and its last source
KP-SM              a plain supermarket: no level, spare carts, nobody counts it
```

| Name in the brief | In the plant | What it is |
|---|---|---|
| K | `PRESS-KANBAN` | 8 flat children; levels small 1, big 1, totes 1, 32x30 0; cover 4; supports the press's 4 positions; sources `ZONE-45`, `ZONE-32`, `ZONE-45-SPARE`; overflow `ZONE-45-SPARE`, 32x30 to `ZONE-32` |
| K2 | `WELD-KANBAN` | 4 flat children; small 1; cover 1; supports the weld; source `ZONE-45` |
| Z45 | `ZONE-45` | 8 flat children; small 2, big 2, totes 2; no supports |
| Z32 | `ZONE-32` | 8 flat children; 32x30 6; no supports |
| | `ZONE-45-SPARE` | 10 flat children, no level, seeded empty: the room for the family's carts while the press runs off it |
| SM | `KP-SM` | Lanes, no level, 3 spare 45x48 carts |

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
bash scripts/kanban-rig.sh R1      # R1..R6
```

It refuses while `shingo-dev` is running, because it starts with `down -v`. `KANBAN_RIG_REPLACE=1` says that stack is yours to drop. `KANBAN_RIG_NO_BUILD=1` reuses the images. It takes the gate lock, so a gate and a run never overlap. It seeds with Core stopped, runs the injections, collects, prints the verdict and leaves the stack up. The exit code is the verdict's.

Each run writes under `../kanban-rig-runs/<run>-<stamp>/` (or `KANBAN_RIG_OUT`): `verdict.txt`; `long-orders.tsv`, every order over 10 sim-min by id, payload and cause; `stranded.txt` and `soakstat.txt` from `cmd/soakstat`; `no-door.txt`; samples every 30 s of the kanban cards, the windows and the unloader's board. Sim-minutes here are at the rig's 10x, so one is 6 wall-seconds.

| Run | Length | Injection | Passes when | What it catches |
|---|---|---|---|---|
| **R1** steady on small | 20 wall-min | None | K wants small 5; K2 wants 2; every unloader cart lit small; no park at a level; `held_by` empty | Cover counted per position instead of per process; a loop that drifts; the statement counts belong here too, and they come from the cost tests, not this script |
| **R2** small → totes | To cutover + 10 sim-min | START `KP-TOTES` at 20 sim-min, CUTOVER at the gate | Cover moved at START (totes 5, small 1); K holds 5 totes; nothing delivered to SM | Cover following the active style instead of the target; carts off the process not re-typed |
| **R3** totes → 32x30 → totes | Each to cutover + 10 sim-min | START and CUTOVER, three times | 32x30 comes from Z32; 45x48 off the process goes to K, then Z45 if short, then the spare zone, never SM. Under `KP-32` with Z45 whole, K keeps its surplus 45x48 with `surplus_cause` on the card and the positions' carts go to the spare: 7 in K, 6 in Z45, 4 in the spare, all 17 accounted for. K's 32x30 holds what its free child lets it, the positions fill one at a time, and nothing wedges. K ends with at most one 32x30 and its totes want; the windows never all held | Crossing a floor: the keeper's sources, per-type overflow, the surplus arm, R10 |
| **R4** flip-flop | To cutover + 10 sim-min | START totes, CANCEL once two positions hold totes, START big, CUTOVER | Cover reverts within a tick; no leg left live; the next START lifts the totes | Cancel leaving orders or cover behind; a stranded order; a cart on a window with no door |
| **R5** repayment | About 10 wall-min | K's cart out by hand until K borrows from Z45, then onto a window. The same for K2. Then K borrows from SM by a hand move | Z45 repaid both times; the next cart after SM's goes to overflow (the spare zone), and the card says no group is short; nothing to SM | Repayment by count: K whole, the next source short, across kanbans, and what a lender with no level cannot get back |
| **R6** raiding | About 5 wall-min | Z45's two smalls out by hand, then K2's two carts | K holds spares; K2's keeper takes nothing from K, then or after settle; with nothing to source the keeper asks nothing, and K2's card says `held_by`: no empty at any source | A lineside kanban sourcing from another lineside kanban |

Every run also fails on a stranded order with no releaser (`soakstat -stranded`), on an empty standing on a window with no live order for 20 sim-min, and on all four press windows held across two samples.

The carts R5 and R6 take out stand on `KP-SM`'s mouths. R5's carts are out of the loop only until K has borrowed: putting them on a window puts the count back, which is what makes "Z45 whole" reachable.

## Leaving the floor

`simcalc -carriers` prints this as CHANGEOVER ROOM, and it counts a plain overflow's slots as room. A changeover within 45x48 conserves. Leaving the floor does not: the 4 positions and K's cover both leave, so 8 carts have no level to land on, whichever pool is seeded.

| Running | 45x48 carts in K's loop | 45x48 levels + wants | Spare zone | 32x30 carts | K's want + positions |
|---|---|---|---|---|---|
| `KP-SMALL`, `KP-BIG`, `KP-TOTES` | 17 | 17 | unused | 6 | 0 |
| `KP-32` | 17 | 9 | 8 of its 10 slots | 6 | 8 |

K and Z45 both refuse a store at level and overflow is one hop, so without the spare zone those carts would park and take all 4 windows from `KP-32`'s fulls. With it they land, and K's last source brings them back when Z45 has none. The save's "repaid only as T's overflow" names the spare zone: R17a, on the rig. A real plant's family needs the same room.

**The table's 8 in the spare is the settled picture, once K's surplus has somewhere to go. It is not what R3 ends on.** A resident moves only to its unloader's window or to a group with a level for its type and room, never to a plain group. So under `KP-32` with Z45 whole, K keeps its 4 surplus 45x48 (7 in K, one child free), Z45 holds its 6, and only the positions' 4 carts reach the spare. K's 32x30 fills through that one free child, so K holds 1 and the positions fill one at a time. The card says why (`surplus_cause`) and nothing wedges.

**32x30 under `KP-32` stays 2 short, and that is not a defect.** K's 4 of cover and the 4 positions want 8 from 6 carts: cover over the pool, which the card shows as "cannot be covered", with `held_by`. R3 expects it.

