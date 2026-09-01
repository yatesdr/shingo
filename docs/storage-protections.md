# Storage / handoff protections — mechanism map

An INDEX, not an explanation. The code comments at each anchor are the source of
truth; this page only says which mechanism lives where, how the tiers compose,
and records the two plant-verified vendor facts they rest on. Keep anchors as
`file` + symbol (line numbers drift; the symbol is the durable handle).

## The tiers (dispatch-time → arrival-time)

Tiers 3 and 4 are reservation-backed. The reservation substrate that powers them
— soft holds on bins and slots, the reserve→claim→confirm lifecycle, and the
forbidigo-enforced seatbelts — is documented in [reservations.md](reservations.md).

| # | Mechanism | Catches | Anchor (symbol) |
|---|-----------|---------|-----------------|
| 1 | Advisory dropoff-capacity gate | a plain/complex order dispatching into a full concrete storage slot (child of a LANE/NGRP) or a saturated node group | `shingo-core/dispatch/capacity.go` — `CheckDropoffCapacity` (~:89) |
| 2 | Swap admission hold (unified, two faces) | a two-robot swap leg physically committing on the shared LINE node before its partner can do its part: an EVAC pulling the line bin before its supply sibling secured a replacement (strands the line, ALN_003), OR an INDEX/filler dropping a bin onto a position its evac sibling has not yet cleared (two bins on the line, HOP press-index 2026-07) | `shingo-core/dispatch/swap_hold.go` — `swapLegHoldVerdict` (~:105; `swapLegHeld` at ~:50 is the test-only boolean view of the same gate) |
| 3 | Slot reservation (reserve-only) | two stores resolving the same slot both dispatching into it (store-vs-store); the Dispatcher-surface entry also settles the destination (group→child, re-aim off a dug lane) and its return value is the only node a caller may use | `shingo-core/dispatch/store_slot.go` — `ReserveStorageDropoff` / `claimStoreSlot` (~:168) — a `resource_kind = 'slot'` reservation row, see [reservations.md](reservations.md) |
| 4 | Atomic slot claim (reservation-guarded CAS) | a slot claim racing occupancy / another claimant | `shingo-core/store/nodes/nodes.go` — `ClaimSlotTx` (~:92); the sanctioned path is `db.ConfirmSlotClaim` (claim + reservation `pending→confirmed` in one tx), enforced by forbidigo — see [reservations.md](reservations.md) §The claim seatbelt |
| 5 | Swap peer-death handler | a two-robot swap leg dying mid-flight (collide two bins on the line, or strand it) | `shingo-core/dispatch/swap_peer.go` — `HandleSwapPeerTerminal` (~:43) |
| 6 | Arrival reconciliation (stale-ghost eviction) | a delivery landing on a node shingo still records as occupied — the record is a stale ghost, evicted to `_TRANSIT` + `anomaly_at` | `shingo-core/service/bin_service.go` — `ApplyArrival` (~:584); shared helper `shingo-core/store/internal/helpers/helpers.go` — `EvictStaleGhostBinsTx` |

## The lane family (dispatch-time)

Lanes need their own protections because only the mouth is reachable, so two
orders can physically collide inside one lane. These compose with the tiers above
rather than replacing them; the full model is in [lanes.md](lanes.md).

> **Naming collision, read carefully.** Tier 2 above is the **swap** admission
> hold — a two-robot swap leg committing on a shared LINE node. It is unrelated
> to `dispatch/admission.go` in the table below, which answers lane safety. Two
> different mechanisms, one word.

| Mechanism | Catches | Anchor (symbol) |
|---|---|---|
| Mouth admission | two orders working one lane in incompatible directions; any other holder against a dig | `shingo-core/store/reservations/mouth.go` — `admitMouth` |
| Lane admission | a move into a lane that cannot take it — foreign dig holding, robot inside, target unreachable | `shingo-core/dispatch/admission.go` |
| Tiered lane entry | a safe move that would wall a deeper pending target (ordering, **not** safety) | `shingo-core/dispatch/lane_entry.go` — `classifyLaneEntry` |
| Reachability predicate | the one definition of "is this slot reachable", replacing seven disagreeing spellings | `shingo-core/store/internal/helpers/lane_reachability.go` — `LaneBlockerPredicate` |
| Lane occupancy | a second robot entering a lane one is already inside | `resource_kind='occupancy'` rows — see [reservations.md](reservations.md) |
| Bin claim in a dug lane | claiming a bin another order's dig has uncovered | `shingo-core/store/reservations/reservations.go` — `acquire` (dug-lane arm, `ErrLaneDugByAnother`; slots deliberately unguarded) |
| Dig standoff tripwire | digs waiting on each other in a closed loop — **reports only**, a human rules it | `shingo-core/dispatch/dig_standoff_tripwire.go` — `SweepMutualDigHolds` |
| Arrival guard | an arrival that cannot be applied, with a reason and context rather than a silent drop | `shingo-core/engine/arrival_guard.go` — `refuseArrival` |

Mouth admission is the load-bearing one: a `dig` excludes every other owner and
is excluded by every other owner — one carve-out: a dig-mode acquire yields to
a non-dig holder parked at the lane's mark whose order is in the "staged
outside" set (`admitMouth`, the `stagedOutside` exemption) — and any other pair
is admitted only on an exact same-mode share.

Note what the **gate** is not. A gate chooses only where an order waits —
parked pre-dispatch, or dwelling at a point. Wait points live on the node
group (`PropGroupWaitPoints`); `PropLaneGatePoint` on the lane is the legacy
fallback and still wins where it is set. The physical questions above are asked
on every lane-entry path unconditionally, gated or not. No lane-level marks are
set at either plant today (verified 2026-08-31); group-level wait points are in
use.

Tier 6 is the ONE reconciliation shared by every arrival-writer so they cannot
drift: `ApplyArrival` (single-bin), `ApplyMultiBinArrival`
(`store/order_bins.go`), and `RepairConfirmedOrderCompletion`
(`store/recovery/recovery.go`) all route through `helpers.PlaceBinTx`, which
runs `EvictStaleGhostBinsTx`.

## The deliberate LINE-drop exemption

The tier-1/tier-3 dropoff gate is applied ONLY to concrete storage slots
(children of a LANE or NGRP), **never to a LINE node and never to staging**.
Gating the coordinated swap's LINE drop deadlocked a plant and was reverted in
**`2b05dce`** — the exemption is load-bearing, not an oversight. A supply leg
delivers to a line node a sibling evac clears; the fleet's shared-node
sequencing (fact 1 below) + tiers 2 and 5 protect the line instead of the
gate. Staging was never covered by this predicate either — a staging node has
no parent and fails the gate's first test — so staging dropoffs are protected
by **declaration** instead (`protocol.ComplexOrderStep.ExclusiveSlot`,
enforced by the declared-exclusive dropoff loop in the complex gate).

Anchor: `shingo-core/dispatch/complex_dispatch.go` — the `isConcreteStorageDropoff`
gate (~:776) + the `2b05dce` regression comment (~:729) and the predicate's
own "IT DOES NOT COVER STAGING" header (~:26).

## Two plant-verified vendor facts (2026-07-08)

1. **The vendor fleet manager DOES sequence two robots at a shared node.** A
   two-robot swap can rely on R2's dropoff waiting for R1's pickup at the shared
   node. Anchor: `shingo-edge/engine/material_orders.go` — `BuildTwoRobotPressIndexSwapSteps`
   comment (~:165).
2. **A delivery physically CANNOT complete onto an occupied slot, but RDS emits
   no fault code and does not track occupancy.** The proof of emptiness is the
   physical completion itself, not a vendor error. So when tier 6 finds a
   different bin recorded at a completed delivery's destination, that record is a
   stale ghost (an untracked manual move) — evict it, keep the newcomer. There is
   NO vendor backstop for a bin-on-bin record; Core's dispatch-time tiers (1–5)
   plus the service-layer occupancy fail-close (`BinService.Move`, fenced by the
   raw-bin-move forbidigo guard in `.golangci.yml`) are the only protection.
