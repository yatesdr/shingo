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
| 1 | Advisory dropoff-capacity gate | a plain/complex order dispatching into a full concrete storage/staging slot | `shingo-core/dispatch/capacity.go` — `CheckDropoffCapacity` (~:89) |
| 2 | Swap admission hold (unified, two faces) | a two-robot swap leg physically committing on the shared LINE node before its partner can do its part: an EVAC pulling the line bin before its supply sibling secured a replacement (strands the line, ALN_003), OR an INDEX/filler dropping a bin onto a position its evac sibling has not yet cleared (two bins on the line, HOP press-index 2026-07) | `shingo-core/dispatch/complex_dispatch.go` — `swapLegHeld` (~:262) |
| 3 | Slot reservation (reserve-only) | two stores resolving the same slot both dispatching into it (store-vs-store) | `shingo-core/dispatch/store_slot.go` — `ReserveStorageDropoff` / `claimStoreSlot` (~:122) — a `resource_kind = 'slot'` reservation row, see [reservations.md](reservations.md) |
| 4 | Atomic slot claim (reservation-guarded CAS) | a slot claim racing occupancy / another claimant | `shingo-core/store/nodes/nodes.go` — `ClaimSlotTx` (~:92); the sanctioned path is `db.ConfirmSlotClaim` (claim + reservation `pending→confirmed` in one tx), enforced by forbidigo — see [reservations.md](reservations.md) §The claim seatbelt |
| 5 | Swap peer-death handler | a two-robot swap leg dying mid-flight (collide two bins on the line, or strand it) | `shingo-core/dispatch/swap_peer.go` — `HandleSwapPeerTerminal` (~:43) |
| 6 | Arrival reconciliation (stale-ghost eviction) | a delivery landing on a node shingo still records as occupied — the record is a stale ghost, evicted to `_TRANSIT` + `anomaly_at` | `shingo-core/service/bin_service.go` — `ApplyArrival` (~:584); shared helper `shingo-core/store/internal/helpers/helpers.go` — `EvictStaleGhostBinsTx` |

Tier 6 is the ONE reconciliation shared by every arrival-writer so they cannot
drift: `ApplyArrival` (single-bin), `ApplyMultiBinArrival`
(`store/order_bins.go`), and `RepairConfirmedOrderCompletion`
(`store/recovery/recovery.go`) all route through `EvictStaleGhostBinsTx`.

## The deliberate LINE-drop exemption

The tier-1/tier-3 dropoff gate is applied ONLY to concrete storage/staging
slots, **never to a LINE node**. Gating the coordinated swap's LINE drop
deadlocked a plant and was reverted in **`2b05dce`** — the exemption is
load-bearing, not an oversight. A supply leg delivers to a line node a sibling
evac clears; the fleet's shared-node sequencing (fact 1 below) + tiers 2 and 5
protect the line instead of the gate.

Anchor: `shingo-core/dispatch/complex_dispatch.go` — the `isConcreteStorageDropoff`
gate + the `2b05dce` regression comment (~:381).

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
