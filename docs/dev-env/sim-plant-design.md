# Designing a Sim Plant

Reference for building a `plants/*.yaml` plant spec that **flows end-to-end in the
local-dev simulator** (the dockerized `shingo-dev` stack + `//go:build sim`
fleet/operator). Read this before authoring or editing a sim plant — most of it is
non-obvious and was paid for in debugging.

The simulator stands in for everything a real plant's operators + RDS fleet do.
A plant spec that omits a variable a swap mode requires will *seed and validate
fine* but **stall at runtime** — orders reach `delivered` but bins never reach
their destinations. The single biggest source of that is getting the **swap-mode
↔ staging/pairing contract** wrong.

---

## 1. Node naming — follow `docs/node-naming-standards.md`

Physical floor nodes use `{TYPE}_{NNN}` with **underscores** (not hyphens):

| Code | Node | Code | Node |
|---|---|---|---|
| `PLN` | press line | `SMN` | supermarket slot |
| `ALN` | assembly/weld line | `SLN` | staging lane |
| `FGN` | finished-goods (exit) | `DZN` | drop zone |
| `UTN` | utility / empty-tote bank | `PLK` | pick / decant |

Synthetic nodes: NGRP groups are `SYN_{Descriptive}`, lanes are `Lane_NN`. Names are
convention only — Shingo dispatches on `node_type_id`, never the prefix — but
consistency keeps logs/orders/audit readable. Loader→`PLK`, unloader→`FGN` are
judgment calls (the standard has no loader/unloader code). PLC/process/style names
(`PRESS-1`, `WELD-1-RUN`) and bin labels are **separate namespaces**, not floor nodes.

---

## 2. The swap-mode contract (THE table)

A claim's `swap_mode` determines which staging/pairing fields it **requires**.
Enforced in `shingo-edge/engine/swap_dispatch.go::BuildSwapDispatch`; the step
choreographies are in `material_orders.go`.

| `swap_mode` | inbound_staging | outbound_staging | paired_core_node | also requires | robots | sim drives? |
|---|:---:|:---:|:---:|---|:---:|:---:|
| `simple` | – | – | – | — (no complex order) | 0–1 | yes |
| `sequential` (A/B) | – | – | ✅ (the A/B partner) | `active_pull` arbitration | 1/leg | yes (release) |
| `single_robot` | ✅ **required** | ✅ **required** | – | — | 1 | yes (release) |
| `two_robot` | ✅ **required** | – | – | — | 2 | yes (release ×2) |
| `two_robot_press_index` | – | – | ✅ **required** | `outbound_destination` | 2 | yes (release ×2) |
| `manual_swap` | – | – | – (a/b uses it) | demand threshold (loaders) | 0 | yes (LOAD/CLEAR) |

> **The #1 pitfall:** `single_robot` requires inbound **and** outbound staging, and
> they must be **DISTINCT nodes**. Validation only checks both are non-empty, so
> pointing both at the same `SLN_xxx` passes — then the new bin (parked at inbound,
> step 2) and the old bin (parked at outbound, step 5) collide on one node and the
> swap strands. Use two separate staging lanes.

---

## 3. Choreographies (what each mode actually does)

`InboundSource`/`OutboundDestination` are normally **groups** (NGRP) so the resolver
picks a slot. A produce claim's `InboundSource` pickup is auto-flagged `Empty` so
Core fetches an empty carrier (not a full bin).

**`single_robot`** — 1 robot, 9 steps (`BuildSingleSwapSteps`):
1. pickup(InboundSource) → 2. dropoff(**InboundStaging**) → 3. wait(node) → 4. pickup(node)
→ 5. dropoff(**OutboundStaging**) → 6. pickup(InboundStaging) → 7. dropoff(node)
→ 8. pickup(OutboundStaging) → 9. dropoff(OutboundDestination).
*Inbound staging parks the NEW bin; outbound staging parks the OLD bin — hence both, distinct.*

**`two_robot`** — 2 robots (`BuildTwoRobotSwapSteps`), needs inbound staging:
- Order A (resupply): pickup(InboundSource)→dropoff(InboundStaging)→wait(InboundStaging)→pickup(InboundStaging)→dropoff(node)
- Order B (removal): wait(node)→pickup(node)→dropoff(OutboundDestination)
- Edge releases B (remove old) then A (deliver new). `RequiresActiveSwapGuard`.

**`two_robot_press_index`** — 2 robots, paired positions, **no staging** (`BuildTwoRobotPressIndexSwapSteps`):
- 2-position (front=CoreNode, back=`PairedCoreNode`):
  - R1: wait(front)→pickup(front)→dropoff(OutboundDestination)→pickup(InboundSource)→dropoff(back)
  - R2: wait(back)→pickup(back)→dropoff(front)
- 3-position adds `SecondPairedCoreNode` (C→B→A index). One front-node claim drives it;
  the paired node(s) are **not** separate claims.

**`sequential`** (A/B) — the **two-bins-at-the-line, rotate** pattern (`BuildSequential{Removal,Backfill}Steps`):
- Two positions (A, B) share one process+style; `paired_core_node` points each at the other;
  `active_pull` marks the bin the line is currently filling. When it fills, the **PLC**
  cuts over — `Engine.FlipABNode` (exposed as `POST /process-nodes/{id}/flip-ab`) flips
  `active_pull` to the other bin and fires auto-reorder on the now-full one, which swaps it out:
- Order A (removal): wait(node)→pickup(node)→dropoff(OutboundDestination)
- Order B (backfill, auto-created when A goes in_transit): pickup(InboundSource)→dropoff(node)
- This is the realistic press/line model — a press is never a single bin filling alone; it's
  A/B (or the in-line `two_robot_press_index`). The cutover is **automated by a PLC bit**, not
  a manual operator action. **Catch (see §4): the sim's PLC stand-in doesn't yet fire that
  flip**, so an A/B cell won't rotate headlessly until the sim simulates the bit.

---

## 4. What the simulator drives (and what it doesn't)

`shingo-edge/engine/sim_operator.go` (`//go:build sim`) is the headless operator:
- **Release** on every order that reaches `staged` — the equivalent of the operator
  pushing RELEASE. Covers `single_robot`, `two_robot`, `two_robot_press_index`,
  `sequential` (any mode that stages).
- **LOAD** (empty→produce) / **CLEAR** (full→consume) — **only for `manual_swap`**
  loader/unloader nodes. Binds/manifests the bin a human would scan.

`shingo-core/fleet/simulator` is multi-robot capable (assigns `SIM-ROBOT-N` per active
order), so two-robot choreographies drive. Binding of a delivered bin to a node is
automatic on delivery.

**Not auto-driven (today):** the **A/B cutover**. In production this is automated — a PLC
bit calls `Engine.FlipABNode` (the `/flip-ab` endpoint) when the active bin fills, flipping
`active_pull` and swapping out the full bin. It is **not** a manual operator action. The
sim's PLC stand-in (`plc/simwarlink`) only generates counter ticks; it doesn't yet fire the
flip, so an A/B cell won't rotate headlessly until the sim simulates that PLC bit (auto-flip
on active-bin-full). Also not auto-driven: changeover style cutover, partial/empty-release
accounting.

---

## 5. Other must-haves for a flowing loop

- **Auto-confirm.** `delivered` is not terminal — an Edge receipt (or the core
  reconciliation sweep) confirms it, placing the bin + releasing claims. The sim sets
  `staging.auto_confirm_delivered` (e.g. `2s`) + a short `sweep_interval` in
  `shingocore.dev.yaml`. Loaders/unloaders carry `skip_auto_confirm` and stay
  operator-driven (the sim CLEARs them). Without fast auto-confirm the loop cycles
  only every 5 min (the default timeout).
- **Groups, not concrete slots.** Point `inbound_source`/`outbound_destination` at
  `SYN_*` NGRP groups so the resolver picks a slot. Same group for in and out is fine.
- **Deep lanes / ASRS.** Supermarket lanes should be multi-slot (`depth` 1..N) with
  `retrieve_algorithm: FIFO` + `store_algorithm: LKND` (or `DPTH`) to exercise the
  reshuffle path. Single-depth never reshuffles. Keep empty carriers in a dedicated
  return pool (`SYN_*`/`UTN_*`), not mixed into supermarket lanes.
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
   staging node for `two_robot`, `paired_core_node` (+`outbound_destination`) for
   `two_robot_press_index`, `paired_core_node`+`active_pull` for `sequential` A/B.
3. Set `auto_confirm_delivered` short; mark loaders/unloaders `skip_auto_confirm`.
4. Deep supermarket lanes; empties in a return pool.
5. Seed source lanes full, leave store-target lanes open.
6. Run: `seed` → watch `curl localhost:8083/api/orders` and core logs. A flowing loop
   keeps creating + confirming orders; a stalled one shows bins stuck in `SLN`/`_TR`
   and producers `no_bin_bound`.

## Reference: a working press setup (Hopkinsville)

A two-position press in production: `swap_mode: two_robot_press_index`, front node
`PLN_01` with `paired_core_node: PLN_02`, **no staging**, `inbound_source` =
"Supermarket Empty Totes" group, `outbound_destination` = "Supermarket Area" group.
The supermarket unload side is `manual_swap` consume (pull full → return empty).
