# Designing a Sim Plant

Verified against: 2cbdbd5b

Reference for building a section of `plants/master.yaml`, the one plant spec, so it **flows end-to-end in the
local-dev simulator** (the dockerized `shingo-dev` stack + `//go:build sim`
fleet/operator). [plants/README.md](../../plants/README.md) maps the file's sections and the scenario
family that proves each; this doc is how a section is written. Read it before authoring or editing a sim
plant: most of it is non-obvious.

The simulator stands in for everything a real plant's operators + RDS fleet do.
A plant spec that omits a variable a swap mode requires can *seed* and still
**stall at runtime**: orders reach `delivered` but bins never reach
their destinations. The single biggest source of that is getting the **swap-mode
↔ staging/pairing contract** wrong.

---

## The spec, the seed and the tick

### From spec to plant

`shingo-core/plantspec` loads the file and `plantspec.Validate` refuses a spec it finds wrong, listing every
problem (dangling references, missing swap staging, storage outside a LANE/NGRP hierarchy, the census at
birth). The seed tool, `shingo-core/cmd/seeddev`, writes what passes to both stores. A process may name the
Edge it runs on (`edge: edge2.line1`); Core is seeded from the whole file and each Edge from its own
processes (`seeddev --edge`, `plantspec.Plant.ForEdge`):

- **`seedCore`** → Postgres, through the store accessors: node types, bin types (families, sizes, robot
  groups) and payloads, the storage hierarchy (zone → lane → slot) and stations, bins (+ manifests),
  node↔payload links, the demand registry, the bin loaders, the kanbans, and every Edge it names enrolled.
- **`seedEdge`** → SQLite, in one transaction: processes, styles, operator stations, claims, reporting
  points, payload catalog, and the initial runtime states, each produce/consume node bound to the bin
  standing on it (so the loops bootstrap).
- **`crossValidate`** → every node an Edge row names resolves to a Core node.

Everything is existence-checked, so re-seeding is idempotent.

### Building blocks

Every key, with its rules, is a field of `plantspec.Plant` (its yaml tags and field comments).

| Block | Spec key | What it is |
|---|---|---|
| **Carrier types** | `bin_types`, `carrier_families`, `carrier_sizes`, `carrier_robot_groups` | The carrier types (the first is the default for a bin that names none), which types are one physical cart (a family), their footprints, and the robot group a type requires. |
| **Zone / lane / slot** | `zones[].lanes[].slots[]` | A supermarket: a zone holds lanes, a lane holds depth-ordered slots. `retrieve_algorithm` (e.g. FIFO) + `store_algorithm` (e.g. DPTH) govern access; a part buried at depth > 1 forces a reshuffle. A slot's `admits` names the types or families it takes. `headroom.free_lanes` is the slack the census asserts at birth (default one lane's worth per group). |
| **Station** | `stations[]` | A non-storage node + its `kind` (line_in, line_out, press, weld, loader, unloader, staging, dest). |
| **Process** | `processes[]` | An independently-counting unit with one `active_style`, on the Edge its `edge` names. A counter tick applies to the nodes of its `(process, style)`, so independently-counting nodes must be separate processes. |
| **Style** | `styles[]` | A `(process, payload)` the process can run. The repertoire a process changes over between; one is active at a time. |
| **Claim** | `claims[]` | A style→node binding: `role` (produce/consume), `swap_mode`, `payload`, `uop_capacity`, `reorder_point`, `auto_reorder`, sources and staging, `keep_staged_node`. A/B pairs share a process+style via `paired_core_node` + `active_pull` (the parked side's ticks are skipped); `window_of` makes a loader claim a window of a shared loader, `home_of` a position of a dedicated-positions loader. |
| **Operator station / cell** | `operator_stations[]`, `cell_configs[]` | The Edge's operator stations, and which one each process's cell uses. |
| **Payload / Bin** | `payloads[]`, `bins[]` | A part (`uop_capacity`, carrier type, robot group) and a physical container holding some UOP of a payload at a slot/node. |
| **Demand** | `demands[]` | A demand-registry entry (payload wanted at a node). `replenish_uop_threshold` is a loader's C-push trigger: unset follows the claim's `reorder_point`, 0 is informational only. |
| **Reporting point / station counter** | `reporting_points[]`, `station_counters[]` | Ties a PLC counter tag to a node; a station counter counts its listed positions in place of the production counter. `plc_name`/`tag_name` MUST match the Edge sim process entries in `shingoedge.dev.yaml`. |
| **Kanban** | `kanbans[]` | A zone Core holds an empty-carrier level in: its levels, pools, sources and the positions it supports. |
| **Loader settings** | `loader_settings` | The per-loader switches a claim cannot express (`auto_push`, `accept_partials`), keyed by loader name. A loader claim seeds no Edge claim row, so a claim's own `auto_push` reaches nothing. |

### How a tick moves material

- Each process has an **`active_style`**; the seeder sets the Edge's `active_style_id` from it.
- A **counter tick** carries the style it was made under and is charged to each of the process's nodes that
  style claims (`(*Engine).walkTick` in `shingo-edge/engine`).
- **Produce** nodes fill toward `uop_capacity`, then swap (remove the full, deliver an empty). A produce
  stroke with no bound bin is held for the next.
- **Consume** nodes draw down toward `reorder_point`; a claim with `auto_reorder` asks for its next full when
  it reaches it (`(*Engine).sweepNodeLevel`; the rule is in
  [uop-threshold-replenishment.md](../uop-threshold-replenishment.md)).
- **A/B pairs** share one process+style; `active_pull` says which side counts.
- In the sim, the fake PLC counts a process only while its machine can cycle (§4): a starved, full or
  empty-handed seat stops it.

---

## 1. Node naming — follow [node-naming-standards.md](../node-naming-standards.md)

Physical floor nodes use `{TYPE}_{NNN}` with **underscores** (not hyphens), from that doc's type codes.
Synthetic nodes: NGRP groups are `SYN_{Descriptive}`, lanes are `Lane_NN`. Names are
convention only — Shingo dispatches on `node_type_id`, never the prefix — but
consistency keeps logs/orders/audit readable. Loader→`PLK`, unloader→`FGN` are
judgment calls (the standard has no loader/unloader code). PLC/process/style names
(`PRESS-1`, `WELD-1-RUN`) and bin labels are **separate namespaces**, not floor nodes.

---

## 2. The swap-mode contract (THE table)

A claim's `swap_mode` determines which staging/pairing fields it **requires**. `plantspec.Validate`
refuses a spec without them (✅), mirroring the runtime checks of `BuildSwapDispatch`
(`shingo-edge/engine/swap_dispatch.go`); the step choreographies are in `material_orders.go`. The four
swap modes are `protocol.ConfigurableSwapModes`; `simple` is refused.

| `swap_mode` | inbound_staging | outbound_staging | paired_core_node | keep_staged_node | index_robot_supplies | also requires | robots | sim drives? |
|---|:---:|:---:|:---:|:---:|:---:|---|:---:|:---:|
| `sequential` (A/B) | – | – | the A/B partner | optional | – | `active_pull` arbitration | 1/leg | yes (release) |
| `single_robot` | ✅ **required** | ✅ **required**, distinct | – | optional | – | — | 1 | yes (release) |
| `two_robot` | ✅ **required** | – | – | optional | – | `outbound_destination` (refused at dispatch) | 2 | yes (release ×2) |
| `two_robot_press_index` | – | – | ✅ **required** | optional | optional | `outbound_destination` ✅ | 2 | yes (release ×2) |
| `manual_swap` | – | – | – | – | – | `outbound_destination` ✅ (not on a produce `home_of` position); a demand threshold (loaders) | 0 | yes (LOAD/CLEAR) |

> **The #1 pitfall:** `single_robot` requires inbound **and** outbound staging, and
> they must be **DISTINCT nodes**: the new bin parks at inbound (step 2) and the old bin at outbound
> (step 5), so one node for both strands the swap. The validator refuses the same node for both.

`keep_staged_node` is one more place a line's carrier comes from instead of `inbound_source`, with a spare
kept standing on it; the spot may be the claim's own `inbound_staging` and is otherwise dedicated to the
line (`plantspec.Validate` restates the Edge's `CheckKeepStagedSpots` rule over the spec). `index_robot_supplies`
belongs to `two_robot_press_index` only (§3).

---

## 3. Choreographies (what each mode actually does)

`InboundSource`/`OutboundDestination` are normally **groups** (NGRP) so the resolver
picks a slot. A produce claim's `InboundSource` pickup is auto-flagged `Empty` so
Core fetches an empty carrier (not a full bin). A claim with a `keep_staged_node` fetches from its spot
instead of `InboundSource` in every mode (`refillPickup`).

**`single_robot`** — 1 robot, 9 steps (`BuildSingleSwapSteps`):
1. pickup(InboundSource) → 2. dropoff(**InboundStaging**) → 3. wait(node) → 4. pickup(node)
→ 5. dropoff(**OutboundStaging**) → 6. pickup(InboundStaging) → 7. dropoff(node)
→ 8. pickup(OutboundStaging) → 9. dropoff(OutboundDestination).
*Inbound staging parks the NEW bin; outbound staging parks the OLD bin — hence both, distinct.* A
keep-staged claim whose spot is its inbound staging skips steps 1-2: the spare already stands there.

**`two_robot`** — 2 robots (`BuildTwoRobotSwapSteps`), needs inbound staging:
- Order A (resupply): pickup(InboundSource)→dropoff(InboundStaging)→wait(InboundStaging)→pickup(InboundStaging)→dropoff(node)
- Order B (removal): wait(node)→pickup(node)→dropoff(OutboundDestination)
- Edge releases B (remove old) then A (deliver new).

**`two_robot_press_index`** — 2 robots, paired positions, **no staging** (`BuildTwoRobotPressIndexSwapSteps`):
- 2-position (front=CoreNode, back=`PairedCoreNode`):
  - R1: wait(front)→pickup(front)→dropoff(OutboundDestination)→pickup(InboundSource)→dropoff(back)
  - R2: wait(back)→pickup(back)→dropoff(front)
- 3-position adds `SecondPairedCoreNode` (C→B→A index). One front-node claim drives it;
  the paired node(s) are **not** separate claims.
- `index_robot_supplies` flips the supply trip: R1 stops at OutboundDestination, and R2 indexes forward and
  then fetches the replacement to the rearmost position.

**`sequential`** (A/B) — the **two-bins-at-the-line, rotate** pattern (`BuildSequential{Removal,Backfill}Steps`):
- Two positions (A, B) share one process+style; `paired_core_node` points each at the other;
  `active_pull` marks the bin the line is currently filling. When it fills, the operator
  releases it, and the release flips `active_pull` to the other bin (the release trunk,
  `commitFlip`); the level sweep then reorders the side that went dark, which swaps
  it out:
- Order A (removal): wait(node)→pickup(node)→dropoff(OutboundDestination)
- Order B (backfill, auto-created when A goes in_transit): pickup(InboundSource)→dropoff(node)
- One position, one live swap, as in every mode: a request on a side whose removal or
  backfill is still working it is refused; the partner side is its own position.
- This is the realistic press/line model — a press is never a single bin filling alone; it's
  A/B (or the in-line `two_robot_press_index`). The cutover is the operator's release of the
  finished side (see §4), and the sim operator releases, so an A/B cell rotates headlessly.

---

## 4. What the simulator drives

`shingo-edge/engine/sim_operator.go` (`//go:build sim`) is the headless operator. It does what a person at
the station does, through the same doors:
- **Release** on every swap that reaches `staged`, once the carrier it replaces is done (the same facts
  that stop the machine, below), after `sim.operators.swap_release` — the operator
  pushing RELEASE. Covers `single_robot`, `two_robot`, `two_robot_press_index`,
  `sequential` (any mode that stages). With `pair_release` it presses the station's RELEASE for the pair
  instead of releasing each leg.
- **Confirm** a delivered swap leg that serves a line node (the Edge receipt), so the Edge and Core orders
  both reach `confirmed`.
- **LOAD** (empty→produce) / **CLEAR** (full→consume) — **only for `manual_swap`**
  loader/unloader nodes, after `loader_auto_load` / `unloader_auto_clear`. Binds/manifests the bin a human
  would scan. An unloader station named in `sim.operators.unloader_taps` has its panel worked as lit
  ([kanban-rig.md](kanban-rig.md), "The operator").
- **Changeover** — each one listed under `sim.operators.changeovers` (`sim_changeover.go`): START at its
  time, each RELEASE the screen offers, CUTOVER once the changeover's gate says it can complete with no
  blockers. Nothing else in the sim starts or cuts over a changeover.

The fake PLC (`shingo-edge/plc/simwarlink`) counts a process only while its machine can cycle
(`engine.SimMachineReady`): a consume seat at 0 with nothing on its pile, a produce seat at its pack, or an
active seat holding no carrier stops it, and the swap the operator releases is what restarts it.

`shingo-core/fleet/simulator` is multi-robot capable (a robot per active order, from a pool of
`sim.fleet_size` named robots when set), so two-robot choreographies drive. A delivered bin is bound to its
node automatically on delivery.

**The A/B cutover is the release.** Releasing the finished side is what flips the pair (there is no flip
button or flip endpoint), and the sim operator releases like any operator, so an A/B cell rotates
headlessly.

---

## 5. Other must-haves for a flowing loop

- **Auto-confirm.** `delivered` is not terminal — an Edge receipt (or the core
  reconciliation sweep) confirms it. The sim sets
  `staging.auto_confirm_delivered` (`2s`) + a short `sweep_interval` in
  `shingocore.dev.yaml`. The orders that bring a loader its empty or an unloader its full carry
  `skip_auto_confirm` and stay operator-confirmed (the sim LOADs/CLEARs them). Without fast auto-confirm
  the loop cycles only every 5 min (the default timeout).
- **Groups, not concrete slots.** Point `inbound_source`/`outbound_destination` at
  `SYN_*` NGRP groups so the resolver picks a slot. Same group for in and out is fine. A source is a group
  or a loader position, never a lane: the validator refuses a lane.
- **Deep lanes / ASRS.** Supermarket lanes should be multi-slot (`depth` 1..N) with
  `retrieve_algorithm: FIFO` + `store_algorithm: LKND` (or `DPTH`) to exercise the
  reshuffle path. Single-depth never reshuffles. Keep empty carriers in a dedicated
  return pool (`SYN_*`/`UTN_*`, or a kanban), not mixed into supermarket lanes.
- **Composite cells.** A weld/assembly cell is ONE process with MULTIPLE claims (e.g.
  2 consume + 1 produce); a counter tick fans out to all its nodes. Each claim carries
  its own `swap_mode` + staging.
- **reporting_points** `plc_name`/`tag_name` must match the sim processes in
  `shingoedge.dev.yaml`. `manual_swap` nodes don't tick (no reporting point).

---

## 6. Authoring checklist

1. Name nodes per §1; group source/dest with `SYN_*` (§5).
2. Pick a `swap_mode` per claim and provide **exactly** the fields §2 requires —
   especially **distinct** inbound/outbound staging for `single_robot`, an inbound
   staging node and an outbound destination for `two_robot`, `paired_core_node` (+`outbound_destination`) for
   `two_robot_press_index`, `paired_core_node`+`active_pull` for `sequential` A/B.
3. Set `auto_confirm_delivered` short.
4. Deep supermarket lanes; empties in a return pool.
5. Seed source lanes full, leave store-target lanes open; the census at birth refuses air bubbles and a
   group without its headroom.
6. Size it: `make dev-rates` and `make dev-carriers` must show the plant lasting a run
   ([plants/README.md](../../plants/README.md), "Sized for the run").
7. Run: `seed` → watch `curl localhost:8083/api/orders` and core logs. A flowing loop
   keeps creating + confirming orders; a stalled one shows bins stuck in `SLN`/`_TR`
   and producers holding their ticks with no bound bin.

## Reference: a working press setup

`[demo]`'s PRESS-1 in `plants/master.yaml` is the two-position press shape: `swap_mode:
two_robot_press_index`, front node `PLN_001` with `paired_core_node: PLN_002`, **no staging** for the swap,
`inbound_source` the press-empties kanban `SYN_PRESS_EMPTIES`, `outbound_destination` the market
`SYN_MARKET`. Its alternate style `PRESS-1-ALT` sets `index_robot_supplies`, so one press runs both
choreographies. The unload side of such a loop is a `manual_swap` consume loader (pull full → return
empty).
