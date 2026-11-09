# Terminology Reference

Verified against: b9210bce

This document defines the vendor-neutral terminology used throughout Shingo and maps each term to its equivalent in common fleet management systems.

## Core Concepts

### Node

A physical location in the facility where bins can be stored, picked up, or delivered. Every node has a name, a vendor location identifier (the name known to the fleet backend), a type, a zone, and a capacity.

| System | Term |
|---|---|
| Shingo | **Node** |
| Seer RDS | Bin Location (`GeneralLocation` class in scene data) |
| MiR | Position / Station |
| Locus Robotics | Location |
| 6 River Systems | Destination |
| VDA 5050 | Node (same) |

Node types include `NGRP` (node group), `LANE` (lane), plus physical nodes for storage, line-side, and staging. (The old `SHF` shuffle-row type was deleted — its nodes were reassigned to `LANE`; reshuffle parking is described by behavior, children of the group, not by a node type.)

**Core Node** — A physical, robot-addressable location in the cell (lane, slot, station). Owned by Core, identified by a stable name string (e.g. `LANE_03_SLOT_2`). Exists whether any Edge process uses it or not. Edge receives the list via sync from Core. Code reference: referenced everywhere as `CoreNodeName string`.

### Bin

A physical container that holds materials and is tracked as it moves between nodes. Each bin has a type, a status, and optionally a payload assignment (payload code, manifest, UOP remaining). The bin is the primary physical entity in Shingo.

| System | Term |
|---|---|
| Shingo | **Bin** |
| Seer RDS | Goods / Container (`goodsId`, `containerName`) |
| MiR | Payload |
| Locus Robotics | Tote / Cart |
| VDA 5050 | Load |

Bin statuses: `available`, `staged`, `flagged`, `maintenance`, `retired`.

A bin carries its payload assignment, manifest, and consumption state directly.

Fields:
- **Label**: unique identifier (e.g., `SHG:0042`)
- **Bin Type**: physical container class (determines size and compatibility)
- **Node**: current floor location
- **Status**: `available`, `staged`, `flagged`, `maintenance`, `retired`
- **Payload Code**: assigned payload template (empty if unloaded)
- **Manifest**: JSON parts list (actual contents)
- **Manifest Confirmed**: whether the operator has verified contents
- **UOP Remaining**: production cycles of material left
- **Loaded At**: timestamp for FIFO ordering (set when manifest is confirmed)
- **Claimed By**: order ID if reserved for transport

A bin without a payload assignment (or with an unconfirmed manifest) is treated as empty by the dispatch system.

### Label

Labels follow the format `SHG:NNNN` (e.g., `SHG:0042`). The `SHG:` prefix distinguishes a Shingo label from other identifiers a plant may already put on a container. The number is unique across all bins regardless of type.

The label is an **identifier**, not a verification gate. Nothing in the running
system reads it off the physical bin.

### Flagging and Maintenance

#### Flagging

A flagged bin:
- Remains at its current location
- Is excluded from retrieve orders
- Is highlighted in the supermarket view
- Can be retrieved for inspection when convenient

#### Maintenance Flow

1. Operator flags the bin with a reason — status: `flagged`
2. Operator triggers "Retrieve for Maintenance" — system delivers to accessible location — status: `maintenance`
3. After repair, operator activates the bin — status: `available`, returned to storage

#### Retiring

Set status to `retired` to permanently remove a bin from operations. The record is retained for historical reference.

### Payload

A template defining what a bin should contain — its expected parts, quantities, and UOP capacity. When a payload is assigned to a bin, the bin receives the payload code, its manifest is populated from the template, and UOP remaining is set to capacity.

| System | Term |
|---|---|
| Shingo | **Payload** |
| Warehouse systems | SKU / Item master |

A bin assigned a payload is only eligible for dispatch when its manifest is confirmed (`manifest_confirmed = true`), its status is `available`, and it is not claimed by another order.

A payload defines a bin's expected contents and production capacity.

Fields:
- **Code**: short identifier (e.g., `BRK-ROTOR-KIT`)
- **Description**: descriptive label (e.g., "Brake Rotor Kit")
- **UOP Capacity**: production cycles supported by a full load (e.g., 24)
- **Template Manifest**: expected part numbers and quantities for a full load
- **Compatible Bin Types**: which physical bin types can carry this payload

If the same physical bin frame carries different parts for different processes, define separate payloads. The payload represents the combination of container configuration and contents.

### Manifest

The list of items (parts, materials) inside a bin. Each manifest item has a part number, quantity, and optional notes. A **payload manifest** defines the template; the bin's **manifest** is the actual contents as confirmed by the operator.

| System | Term |
|---|---|
| Shingo | **Manifest** |
| Seer RDS | No direct equivalent (goods are tracked by ID only) |
| Warehouse systems | Pick list / Packing list |

### Order

A transport request to move a bin between nodes. Orders flow from Shingo Edge to Shingo Core, which dispatches them to the fleet backend.

| System | Term |
|---|---|
| Shingo | **Order** |
| Seer RDS | Order (block-based or join order) |
| MiR | Mission |
| Locus Robotics | Job |
| 6 River Systems | Task |
| VDA 5050 | Order (same) |

Order statuses: `pending`, `queued`, `dispatched`, `in_transit`, `delivered`, `confirmed`, `failed`, `cancelled` (plus `submitted`, `acknowledged`, `staged`, `faulted`, `reshuffling`, `skipped`; the full set is in [order-lifecycle.md](order-lifecycle.md)). (There is no `completed` status — terminal completion is `confirmed`.) Every order is issued `pending`, goes to `dispatched` when its whole hand is had, or rests in `queued` with a cause until it is.

**Order** — A material-movement request between nodes. The canonical noun across the system. Edge produces orders driven by demand wiring; Core receives and dispatches them. Edge URL is `/orders`. Code reference: `shingo-edge/domain/order.go`; Edge handler `handleOrders` calls `OrderService().ListActiveByProcess()`.

### Manual Order

An admin-created one-off order. On Edge, submitted via the `/manual-order` page (types: `move`, `retrieve`, `store`, `complex`). On Core, submitted via Core's `/orders` admin modal (subtypes: `transport`, `staged`, `swap`, `send_to_location`), historically called "Spot Order" — Core rename to "Manual Order" is outstanding. Flows to Core via the protocol like any other Edge-originated order. Code reference: Edge: `shingo-edge/www/handlers_manual_order.go`; Core: `shingo-core/www/handlers_orders.go:203` (`apiSpotOrderSubmit` — rename pending).

### Test Order

A developer/QA tool on Core's `/test-orders` page for exercising order paths during development. Not an operator-facing concept. Code reference: `shingo-core/www/handlers_test_orders.go`; don't use this term in operator UI.

**Spot Order vs Manual Order are not the same thing** even though they cover the same admin-need category. Different surfaces, different forms, different type vocabularies. See reconciliation table below for the proposed unified term.

### Cart (of an order)

A bin an order carries, recorded as one `order_bins` row per **lift**: the plan step that picks it up and the plan step that sets it down. Every order that carries a bin has its rows, kept after the order ends; an order has no single "bin" column. Where a lift happens is read from the order's plan, never stored on the row. See [data-model.md](data-model.md#order-carts-order_bins).

### Cart (two-stage unloader)

The operator-facing word for the carrier of a two-stage unloader. HMI copy on the two-stage panels says cart, and so do the Core station box slots about it (*Carts with an empty bin go to*, *Carts wait at*); code and other admin surfaces say carrier. Code reference: `operator-render.js` `confirmFullOff`, `confirmSendOn`; `loaders.js` `stationSlots`.

### Reservation

A soft, revocable hold recording that an order intends to use a resource — a source **bin** or a destination **slot** — set *before* the bin is claimed. Reservations are what let Core hold an unfulfillable order (`queued`) and retry as inventory appears, rather than failing the order. Reclaimed when the owning order ends; never time-based. See [reservations.md](reservations.md).

### Slot

A physical destination node a bin can be delivered to (a storage slot, staging location, or line-side position). A **slot reservation** is the destination-side mirror of a bin reservation: a soft hold on a node, confirmed atomically with the slot claim.

A single storage position within a lane, holding exactly one bin. Each slot has a **depth** value: 1 = front (accessible), higher = deeper (blocked). Slots are physical nodes with a `depth` property.

### Acquiring

The act between `pending` and `dispatched`: asking for an order's whole hand (source bins, destination slot, lanes) and taking it in one transaction, or parking the order in `queued` with the cause on its row. It is never a stored status. The **acquiring set** — the orders the fulfillment scanner may ask — is `pending` and `queued` (`protocol.IsAcquiring`); a `queued` order holds its sources as reservations and is asked again each scanner tick, and is not "stuck" — demand is operator-driven.

### Sourcing

Two meanings, only one live. As an **order status**, `sourcing` is history: since C1 no order can be in it, no transition enters it, and the constant stays only so old `order_history` rows render (migration v148 moved any live `sourcing` order to `queued`). As the **forecast's** word — the Sourcing page, `sourcing_state`, `StrictSourcing`, the sourcing verdicts and feed — it is current: whether a (process, style) can be supplied.

### Zone

A logical grouping of nodes, typically corresponding to a physical area of the facility (a floor, a warehouse section, a production line area).

| System | Term |
|---|---|
| Shingo | **Zone** |
| Seer RDS | Area (scene area) |
| MiR | Zone / Map group |
| VDA 5050 | Zone (same) |

### Station

A Shingo Edge instance identity. **Three things that used to be one string**, and
separating them is what stopped a rename from being a plant stop:

| | What it is | Owner | Mutable? |
|---|---|---|---|
| `station_uid` | the identity Core correlates all history to | **Core**, minted at enrollment | **never** |
| `display_name` | `SPRINGFIELD / EDGE-2` | operator, edited on Core | freely |
| `Address.Station` | the transport routing selector | derived — its VALUE *is* the `station_uid` | never |

Before v66 the uid was DERIVED from namespace + line id (`plant-a.line-1`) and
asserted by the Edge, which meant every unenrolled edge in the fleet shared one
identity and a relabel rewrote a key six tables and a backup manifest were built
on. Now Core mints it, and a station is REGISTERED only after it has been
ENROLLED — a register carrying a uid Core has never issued is refused and writes
nothing.

The uid being re-issuable is the whole reason Core mints it rather than the Pi:
replace the hardware for an existing station and Core hands the new box the
EXISTING uid, so the station's history does not move because its identity did
not move. Enrollment is a deliberate act on Core — `POST /api/edges/enroll`
(`shingo-core/www/router.go:415`) — and the registration message it gates is
[EdgeRegister](wire-protocol.md#edgeregister).

This is the worst overloading in the codebase today — the word "Station" means two different things at two different scales. The [reconciliation table](#cross-surface-terminology-to-reconcile) lists the rename targets.

**Edge Cell** — One Edge installation — a physical production cell with its own Edge instance, controllers, HMIs, and Core sync. Identified by `StationID` in `Config.Messaging`. Core's `NodeType` code `EDGE` and `Order.StationID` refer to this concept. **The term "Edge Cell" is the proposed unified name** — code currently uses "Station" (Edge config), "edge-station" (Core docstrings), and `StationID` (both). Code reference: `shingo-edge/config/...` `StationID`; `shingo-core/domain/order.go:23` `StationID`; `shingo-core/domain/node_type.go:6` "EDGE (edge station)".

### Operator Station

A specific HMI screen inside an Edge Cell. Configured to claim a subset of the cell's Process Nodes; renders an operator-facing UI for those nodes. Multiple Operator Stations exist per cell. **In code, "Operator Station" wins** — code currently mixes this with "Station" (the domain type). **In the UI it is a screen** (*Operator screens*, *Add operator screen*, *Screen* as a column head), which keeps "station" for the cell and the loader station. Code reference: `shingo-edge/domain/station.go:8` `type Station struct`; API at `/api/operator-stations`; URL `/operator/station/{id}`.

### Process

A production area monitored by a station. Stored in the database as `production_lines`. Each process can produce one active job style at a time.

**Process** — A production sequence configured for a cell (e.g. "Front Rail"). Has one ActiveStyleID and many Styles. Owns the production counter config. **One Process is active per cell at a time** (Process has `ActiveStyleID`; cell switches active style via changeover). Code reference: `shingo-edge/domain/process.go:32` `type Process struct`.

### Style

An end-item type that a process produces. Stored in the database as `job_styles`. Switching the active style on a process is handled through the changeover workflow.

**Style** — A variant produced under a Process (e.g. "Style A", "Style B"). Belongs to one Process via `ProcessID`. The active Style drives which NodeClaims are in effect. Also written **"Job Style"** in UI labels and changeover docs — both names are acceptable; `Style` is the code identifier, "Job Style" is fine in operator-facing text. Code reference: `shingo-edge/domain/process.go:41` `type Style struct`.

### NodeClaim

A per-Style binding to a Core Node — declares the payload, capacity, reorder behaviour, swap mode, staging. The active Style's NodeClaims drive material orders. **One Claim type exists** (the verb "claims" is used in unrelated relationships — see Claim disambiguation below). Code reference: `shingo-edge/domain/process.go:116` `type NodeClaim struct`.

### Claim disambiguation (one noun, two unrelated verb uses)

The word "claim" appears in three places. Only one of them is a data type:

1. **NodeClaim** (data type) — per-Style binding to a Core Node. The configured "Style X wants payload Y at node Z."
2. **Operator Station claims nodes** (verb / many-to-many relationship) — operator-station → claimed-nodes (`apiSetStationClaimedNodes`). Says "this HMI is responsible for these physical nodes." No `Claim` table; just an assignment.
3. **Robot claims bin** (runtime fleet concept) — a robot taking ownership of a bin for transport. Not in the process domain at all.

When writing about "claims," qualify which one. "NodeClaim" for the noun; "station node assignment" or "robot-bin ownership" for the verbs.

### Role (Claim Role)

Direction of material flow — consume (full in, empty out) or produce (empty in, full out).

**Claim Role** — What a node does for a payload under a NodeClaim. Two live values: `consume` (node consumes upstream material), `produce` (node produces material for downstream). **Deprecated:** `changeover` — present in `protocol/types.go:235` and referenced in `engine/changeover.go` and `operator_node_changeover.go`, but does **not** reflect how changeovers actually work (its third reference, the admin page's `processes.js`, went with the claim editor in U9d). Actual changeover mechanic: operator selects a new Style → active NodeClaims change → each claim's `swap_mode` drives add/drop commands per node. No separate "changeover role" needed. Slated for removal — see [deprecations tracker](ui-style-guide/33-settings-pages.md#deprecations-tracker). Code reference: `protocol/types.go:230-235`.

### Swap Mode

The step-list shape for a material handling cycle. Four are configurable — `sequential`, `single_robot`, `two_robot`, `two_robot_press_index`. `manual_swap` names a forklift-worked loader window and is never stored; `simple` is retired to a runtime descriptor. The field is spelled `cycle_mode` on the dispatched order. See [order-lifecycle.md](order-lifecycle.md#swap-modes).

**Swap Mode** — How a node's bin gets replaced. Active values: `sequential`, `single_robot`, `two_robot`, `two_robot_press_index`, `manual_swap`. **Deprecated:** `simple` (hidden in UI, legacy data still has it — see [deprecations tracker](ui-style-guide/33-settings-pages.md#deprecations-tracker)). Code reference: `protocol/swap_mode.go:17-22`.

### Process Node

An Edge-side record that says "Process X uses Core Node Y in role Z." Has its own ID, references a Core Node by name (`CoreNodeName`), carries process-scoped config (owning operator station, sequence, display name) plus a separate `RuntimeState` row (active bin, remaining UOP, active orders). Many Process Nodes can reference one Core Node (different processes sharing the same physical slot). Code reference: `shingo-edge/domain/process.go:53` `type Node struct` (the comment on line 49 explicitly says "process node").

Person/Employee analogy: Core Node is the human (one per body), Process Node is the employment record (many possible per person, each carrying per-employer context).

### Bin Loader / Unloader

A staging point where an operator manually fills empty bins (a **loader**, role `produce`) or empties full bins (an **unloader**, role `consume`) to feed/drain the material loop. Core owns the loader aggregate (`bin_loaders`); the Edge runtime consumes it as a first-class `domain.Loader`. A loader has one of two **layouts**:

| Term | Meaning |
|---|---|
| **Shared window** | A loader with **N window nodes** that all present the **same** shared payload set and draw on **one** budget. An empty may be delivered to **any free window**. The invariant: one demand of N → exactly N empties in flight across all windows, never 2N. |
| **Window** | One load point of a shared-window loader. Carries **no** per-position payload (the shared set is loader-level). Marked explicitly as `kind = "window"` — *not* inferred from an empty payload. |
| **Dedicated positions** (a.k.a. *home-location*) | A loader with **N independent** positions, each its own node bound to one payload, each its own one-bin slot. Positions do **not** share a budget. |
| **Position** | One dedicated home: one node, one payload (which may be unassigned/empty until the operator picks one). Marked `kind = "dedicated"`. |
| **Anchor** | The loader's identity node (`core_node_name`). Historically also the shared demand key; being retired in favor of an explicit `loader_id` (see the multi-window refactor). |

**Budget** = a loader's total physical bin slots (`SlotCount`). For a shared-window loader it is the shared empty-in ceiling across all windows.

**Loader station** — The Core Nodes page's word for a bin loader or unloader (one `bin_loaders` row; a two-stage unloader is one station made of two loaders). UI text says **station** only on that page's box and card (**+ Station**, **New station**, **Delete station**); code says loader. A third meaning of "station" — see the reconciliation table. Code reference: `shingo-core/store/loaders` `Loader`; `shingo-core/www/static/pages/loaders.js`.

### Bare bin type

A bin type with `bare_of` set: the marker Core stamps on a CLEAR at a stage-1 window of a two-stage unloader. Derived from the cart's own type, one marker per cart type, created the first time that type comes through. It means "this cart holds no bin yet". A bare cart is never handed out as an empty, and a bare type is never in a payload's carrier rules or offered in any picker. At stage 2 the one-tap **Send on** posts a blank code and Core stamps the cart back to its `bare_of` type. No operator surface shows a marker code; the Edge carries only `leaves_bare` (stage 1) and `takes_bare` (stage 2, a produce loader whose windows the Edge draws as an unloader's; its bare-cart card offers EMPTY ON or PARTIAL ON). UI: a **bare** badge on the bin types table, read-only. Code reference: `shingo-core/domain/bin_type.go` `BinType.Bare`; `bin_types.bare_of`; `ensureBareMarker`; `store/bins/bins.go` `EmptyCarrierWhere`.

## Robot and Fleet Concepts

### Available

Whether a robot is accepting new orders from the dispatch system. A robot that is not available will finish its current task but will not be assigned new work.

| System | Term | Values |
|---|---|---|
| Shingo | **Available** (bool) | `true` / `false` |
| Seer RDS | Dispatchable | `dispatchable`, `undispatchable_unignore`, `undispatchable_ignore` |
| MiR | State (Ready) | Ready / Paused |
| VDA 5050 | Operating mode | Automatic / Semi-automatic / Manual |

### Connected

Whether the fleet backend can communicate with the robot.

| System | Term |
|---|---|
| Shingo | **Connected** (bool) |
| Seer RDS | `connection_status` (int, 1 = connected) |
| MiR | Status (online/offline) |

### Busy

Whether the robot is currently executing an order or task.

| System | Term |
|---|---|
| Shingo | **Busy** (bool) |
| Seer RDS | `procBusiness` (bool) |
| MiR | Mission status (executing) |

**Busy means in use and unable to take other work: moving, held or faulted.**
A robot is busy from the order's first acknowledged or in-transit row until it
puts the load down (delivered, or the order's failure), and on an open order
until now. Faulted time counts: the robot is still on its order. The wait
before a robot takes the order is not busy, and nor is the wait for the
station's confirmation after delivery.

Run time (see [Mission detail — stages](ui-style-guide/31-mission-detail-stages.md)) is not busy time: it measures the work,
so it leaves fault time out.

A robot on two orders at once is busy once, for the union of the two. Every
fleet figure reads this one interval set: per-robot busy and utilization,
fleet utilization and average load, the daily average, the hourly curve and
the peak. `TestOverviewPin_OverlapIsCountedOnce` pins it, including that a
day's average load equals that day's daily average.

### Fleet Backend

The vendor-specific robot fleet management system that Shingo communicates with. Shingo's `fleet.Backend` interface abstracts over vendor differences.

| System | Term |
|---|---|
| Shingo | **Fleet Backend** |
| Seer RDS | RDS (Robot Dispatch System) |
| MiR | MiR Fleet |
| Locus Robotics | LocusServer |
| VDA 5050 | Master control |

## Operations

### Retry Failed

Re-attempt the current failed operation on a robot. Used after the physical issue causing the failure has been resolved.

### Force Complete

Manually mark the robot's current task as finished, skipping whatever operation was in progress. Used when material has been moved by hand or the task is stuck.

### Set Availability

Control whether a robot accepts new dispatch orders.

## Material Concepts

### UOP (Unit of Production)

One cycle of the manufacturing process supported by the bin's parts. A bin with UOP capacity 24 contains enough parts for 24 production cycles. UOP remaining drives reorder decisions.

**UoP** — Units of Production — the count of finished parts a bin/payload carries, or that a cell has consumed. The atomic quantity the threshold monitor sums and reorder thresholds fire on. Casing rule: always **"UoP"** in UI text — labels, headers, table columns, prose, toasts, tooltips. Never "UOP" or "uop". **Display text only:** code identifiers, JSON keys, struct fields, and `data-*` attributes keep their existing casing (`UOPRemaining`, `uop_remaining`, `data-uop`) — renaming a serialized field is out of scope and would break the protocol. See [Units and casing](#units-and-casing).

### Totes on Cart

How many totes stand on a tote cart, filled or empty: `bins.totes_on_cart`. **NULL is unknown** (a cart nobody has declared, or one emptied since); **0 is tops & bottoms** (below), a cart with no totes on it. It is a count, not a bin type: there is no tops & bottoms type. Every statement that empties a carrier writes it NULL; a person declares a height with the CLEAR (layers × the cart's own type's totes per layer, 0 declaring tops & bottoms); the admin bin edit sets it; an admin re-type blanks it; Core derives it at a tote press when the cart outside the press is lifted (what the press's build drew from it): an outside known at 0 stays 0, and one the count overdrew is written unknown, never 0. "Bare" is the marker only (a carrier type's bare marker: a cart with no bin on it) and never means tops & bottoms or a vacant position.

### Totes per Layer

How many totes make one layer on a cart of a bin type: `bin_types.totes_per_layer`, NULL when the type is not a tote cart. A count of totes is a height only on an exact multiple of it.

### Tote Layers

How many layers of totes a payload's full stack stands: `payloads.tote_layers`, NULL when the part states none. It is the height an empty is wanted at when it is set down anywhere but the press of a cell fed from behind (there, 0: tops & bottoms first, else the shortest stack). An empty is ranked by it and never refused for it, but for tops & bottoms, which a stack is never offered.

### Tops & Bottoms

A tote cart at a known 0 totes with no part on it: the cart's frame alone (`protocol.CarrierTopsAndBottoms`). It is nobody's ordinary empty: a want of no height, or of a stack, never takes it; a want of 0 (the press of a cell fed from behind) takes it first; a want of it alone (a kanban's tops & bottoms row) takes nothing else. A kanban keeps it on a row of its own (`kanban_layer_levels` at 0 layers), a cap beside the type's level that neither takes the other's room; the keeper never fetches it (it is made at the unloader, and the card asks the crew for it), and a group that keeps the type with no such row refuses one. On a cell's own position it belongs to that cell, so a press's index leg lifts it whatever height the press would ask for. An unknown count is not tops & bottoms: it is unverified.

### Vacant

A position with nothing standing on it. A press's on-deck (its outside or second paired position) that is vacant when a steady pair reserves has its carrier fetched from the claim's inbound source, unless another live order is already bringing one there, when the pair waits ("Waiting for outside material"). A changeover's legs are never fetched for (the changeover brings a vacant position its own carrier), nor is a leg whose demand episode Core cannot name; they wait. Never "bare", which is the marker.

**Bare position** — **Not the same thing** as a bare bin type. An Edge word for a position with no carrier standing on it at all. Nothing to do with a bin type; do not shorten "bare bin type" to "bare" where the two could be confused. Code reference: `shingo-edge/engine/consume_plan.go`, `operator_produce.go`.

### UOP per Tote

The cycles (UOP) that fill one tote of a payload: `payloads.uop_per_tote`, NULL when the part states none. With it, Core turns a press's count into the totes it filled. Without it, no tote count is derived.

### Supermarket

An automated storage area consisting of lanes and a shuffle row, represented as a node group (`NGRP`).

### Lane

A linear sequence of storage slots within a supermarket. The front slot (depth 1) is robot-accessible; deeper slots are blocked by those in front. Because only the mouth is reachable, a lane needs arbitration between orders that want it at the same time — see [lanes.md](lanes.md).

Lane depth varies. Each lane is a synthetic node of type `LANE`.

### Shuffle Row

Holding slots used during retrieval reshuffles. When a target bin is blocked, the blocking bins are parked elsewhere and the target is retrieved. The blockers are **not** moved back — see [lanes.md](lanes.md#retrieve--reshuffle). A blocker may be parked in another lane in the same group, not only in the shuffle row.

The shuffle row must have at least as many slots as the deepest lane minus one. Shuffle slots are fully accessible (no blocking between them). (The synthetic `SHF` node type was deleted — its nodes were reassigned to `LANE`; blockers park in any child slot of the group, per the next paragraph.)

A blocker is not restricted to the shuffle row. Parking searches the children of the whole node group (`findShuffleSlots`), so a blocker may be parked in another lane in the same supermarket if that is the better slot.

### Reachable / Buried

A slot is **reachable** iff no occupied slot sits strictly shallower in the same lane; otherwise the bin in it is **buried**. One definition, `LaneBlockerPredicate` — do not write an eighth spelling.

### Mouth

The right to work a lane, held as a reservation row (`resource_kind='mouth'`) keyed on the lane. Carries a **mode**.

### Mode (of a mouth hold)

The work direction: `inbound` (the owner drops into the lane), `outbound` (the owner picks from it), or `dig` (exclusive — both directions at once). A dig excludes every other owner; anything else shares only on an exact same-mode match. An order holds one mode per lane.

### Dig

An excavation: moving blockers out of a lane so a buried bin can be reached. The `mode='dig'` row **is** the lock — there is no separate lock table. A dig dwells in the lane it is digging and yields to the dig already there.

Note that `mode='dig'` no longer implies an excavation is running — every demand's source hold is a dig. Read `reserved_by` to tell an excavation from ordinary sourcing.

### Occupancy

A robot is physically inside a lane right now (`resource_kind='occupancy'`). A different fact from holding the claim on the work.

### Admission

The decision "may this move happen at all", asked in one place (`dispatch/admission.go`). Distinct from **ordering** — admission says the lane cannot take the move; ordering says it could, but somebody else should go first. `lane-target-buried` is admission; `lane-deeper-pending` is ordering.

### Gate / Mark

A lane is **gated** iff its node group carries a dwell point (`PropGroupWaitPoints` on the NGRP) — the **mark**. The per-lane `PropLaneGatePoint` is a legacy key that still wins when set, but no plant uses it. The mark chooses where an order waits: parked pre-dispatch, or dwelling at a point. It is not a safety setting, and no lane-level marks are set at either plant today (group-level wait points are in use). See [lanes.md](lanes.md).

**Waiting spot** — A map point beside a group of lanes where a robot stands while Core decides whether it may enter a lane. A group's waiting spots are its `group_wait_points`. Every lane has one: a lane cannot be created, or moved, into a group without a waiting spot, and a group with lanes cannot lose its last one. UI: the node modal's *Waiting spots* section is the one editor; a group without any shows *Give this group its waiting spots to add a lane* in place of *+ Add lane*. Code reference: `shingo-core/store/nodes/marks.go`; `dispatch.PropGroupWaitPoints`.

### Chapter

One generation of a compound. A superseded generation is a **closed chapter**; `orders.open_for_children` records sealedness explicitly.

### Headroom

Free slots a group keeps in reserve so a dig has somewhere to put blockers. A group filled to the brim cannot excavate at all. Distinct from the shuffle-row minimum, which sizes the deepest single dig.

### Queue Cause

The named reason an order is waiting, recorded on its row. Core vocabulary that never crosses the wire. Every cause declares what releases it (the releaser inventory) and is backed by a periodic floor.

### Floor

A periodic pass that re-evaluates a wait an event should have released. A floor is a **backstop**, not a poll — the interval is a maximum wait. See [sweeps-and-monitors.md](sweeps-and-monitors.md).

### Staging (declared)

A node whose destination gates deliberately stand down — reserved by nothing, capacity-checked by nothing. A staging node is a station with **no parent**, declared in the Edge's cell config; Core cannot infer it.

### Inbound Staging

The node named by a claim's `inbound_staging`: where a single-robot or two-robot swap parks the incoming bin on its way to the line, and where a press's tooling changeover holds the incoming style's bins until tooling is done. It means only that. A spare kept for the line is the **keep-staged node**, a field of its own.

**Staging**: Intermediate holding node used in hot-swap modes — Inbound for the arriving bin, Outbound for the departing one.

### Keep-Staged Node (the spot)

The node named by a claim's `keep_staged_node`: one more node the line's full or empty comes from, instead of the inbound source, with one spare kept standing on it. Naming one is what turns keep-staged on, in every swap mode (sequential, single-robot, two-robot, press-index); blank is off. A REQUEST's swap fetches its carrier from the spot when a right spare stands there, and from the inbound source when none does, as a line with no spot would; the REQUEST orders nothing for the spot. The keep-staged keeper refills the spot from the inbound source by plain retrieves when it reads it vacant: on its sweep, at a lift from the spot, and at the end of an order of the line. A bin on the spot that is not a spare is left where it stands: the keeper orders nothing onto it and the board names it, and a swap that stages on the spot is held by Core (waiting for slot, dropoff occupied) until a person moves it. Only a changeover's start and cancel, and the save that clears the spot, send a spare back. It may be the same node as the claim's inbound staging (the robot waits under the spare); no other claim may name it, and a lane is refused on it as on every other leg of a claim. The flag it replaced (`keep_staged`) armed a spot on `inbound_staging`, which gave that field a second meaning.

### Changeover

The workflow for switching a production line from one job style to another. The changeover **row** has three states — `active`, `completed`, `cancelled` — and moves once, to one of the two terminal ones. The sequencing is per **node task**, not per changeover: `swap_required → staging_requested → staged → empty_requested → line_cleared → release_requested → released`, plus `unchanged`, `switched` and the dispositions `error` / `capacity_blocked` / `awaiting_material` / `abandoned` / `cancelled`. Cutover is gated on all of them being terminal.

### Paired Position

The node named by a claim's `paired_core_node`. The field means two different things depending on the swap mode: at a **sequential** (A/B) cell it is the partner position the line draws from while this one is swapped; at a **press-index** cell it is the back press position in a queue. One field, two meanings — read the mode before reading the field.

### Departed

A swap leg that has finished with its **cell** but is not finished as an order: the fleet has confirmed the last step of the leg's plan whose node belongs to the cell, and the robot is now carrying a bin away from it.

A departed leg is still live — Core, telemetry, the orders page and swap-peer handling all still own it. What it stops being is the *cell's*: the two admission guards, the station card's action button and the auto-relief tick all treat the cell as free. Rendered on the card as **TO MARKET**, the opposite number of ROBOT IN TRANSIT (a cell *waiting on* a delivery).

Derived from `steps_json` against the claim's cell set, never from the swap mode. See [order-lifecycle.md](order-lifecycle.md#departed-legs-and-cell-done).

### Other material-flow terms

| Term | Definition |
|------|-----------|
| **Bin Type** | Physical container classification (size, form factor) |
| **Manifest Confirmed** | Whether an operator has verified the bin's contents match the payload template |
| **Depth** | Slot position in its lane (1 = front/accessible, higher = deeper/blocked) |
| **Reshuffle** | Moving blocking bins to the shuffle row to access a target bin |
| **FIFO** | First In, First Out — oldest material is retrieved first |
| **Contiguous Packing** | Occupied slots form an unbroken block from the back of a lane |
| **Reorder Point** | UOP threshold below which a material handling cycle is triggered (when AutoReorder is ON) |
| **AutoReorder** | Setting that controls whether cycles are triggered automatically by PLC counters or manually by the operator |
| **Outbound Destination** | Node where spent or finished bins are sent after removal from the station |
| **Inbound Source** | Node or node group where replacement bins are retrieved from |
| **Operator Canvas** | Real-time visual display on Edge showing payload states, order progress, and operator action buttons |
| **Payload Catalog** | List of payloads available to an edge station, synced from Core |

## Kanban

A kanban is the card loop of empty carriers around a process: the card is a cart, the pool is the number of cards in the loop, the level is the mix held ready, the empty is the signal, the sources are where the loop borrows from. It holds EMPTY carriers; a part's stock at a line is threshold replenishment.

### Kanban
A node group that declares a **level** per bin type and a **pool** per **carrier family**. Core counts its **loop**, admits every cart of the loop, fetches only what the loop has lost, and caps stores from outside the loop at the level. It holds EMPTY carriers; a part's stock at a line is threshold replenishment.

### Carrier Family
The physical cart a bin type is one setting of (`carrier_families`, `bin_types.family_id`). Every type is of one family, by default its own; a bare marker is of its carrier's. A CLEAR may re-stamp a bin within its family, never across. **Footprint** is the label and the check, never the key.

### Loop
Where a kanban's carts may be: its slots, its supports' positions, the windows of every unloader whose outbound it is, and the flights between them.

### Pool
How many carts of a family a kanban's loop is meant to hold (`kanban_families.pool`). Declared; **found** is counted against it.

### Found
The loop's carts of a family by place: slot, line, window, flight. Below the pool a cart has left the loop; above it a stranger is in it.

### Room
The kanban's slots whose bin types admit the family. Below the pool a return cannot always land.

### Level
The ready mix of one bin type a kanban holds at all times (`kanban_levels.want`); not the pool. Want is level + **cover**, with no clamp.

### Cover
The positions the running style's claims put on the line, per bin type. Counted from the claims, never declared. A process runs its target style while a changeover is open, otherwise its active style.

### Minimum Pool
The sum of a family's levels plus, summed over the supported processes, the most positions any of that process's styles puts on the line in the family. Below it the line eats the mix.

### Floaters
Pool less mix less the line: the family's carts standing in the kanban as whatever they came off as, re-stamped only when a type is short.

### Footprint
A bin type's width by length, either way round (`domain.BinType.Footprint`). Members of a family must agree. Not the backstop **Floor**.

### Supports
The process nodes a kanban serves (`kanban_supports`). A kanban with supports needs a station, the board its keeper's orders show on.

### Sources
The ordered groups a kanban's keeper may pull from (`kanban_sources`, in rank order). An empty list searches the plant. A lineside kanban's keeper never pulls from another kanban that has supports. An unloader repays the kanban, then the first source short of the cart.

Sources and strict fence a kanban's owned carts. A cart above a kanban's pool (found, flights included) is owned by nobody: any keeper's deficit ask takes it first, whatever its own sources. A kanban never gives a cart it owns.


### Strict
Nobody else may pull this kanban's owned carts (`kanbans.strict`); a cart above its pool is anyone's.


## UI words

Names matter — drift in component naming follows drift in domain naming.
This glossary is the source of truth; use these names in code, templates,
and UI labels.

Each entry was verified against the code (citations below). Where the codebase
uses inconsistent names for the same concept today, the entry says which name
wins and the inconsistency is flagged in **Cross-surface terminology to
reconcile** below.

### Cross-surface terminology to reconcile

These are same-concept-different-name drifts where the system should pick one
name and migrate. Listed in rough order of impact × ease.

| Concept | Names today | Proposed unified name | Rename mechanics |
|---|---|---|---|
| **Edge installation / cell** | "Station" (Edge config UI, `StationID`), "edge-station" (Core docstrings), `EDGE` (Core NodeType code) | **"Edge Cell"** in UI labels and new docs. `StationID` field name stays in code (too disruptive to rename a serialized field across the protocol), but its meaning is "Edge Cell ID" | Update Edge config UI labels: "Station ID" → "Edge Cell ID". Update Core docstrings. Don't rename `StationID` in JSON/structs |
| **Loader or unloader on Core's Nodes page** | "station" (the station box and card: **+ Station**, **New station**), "loader" (code, API, every other page) | Keep **station** on that page only; a plant person thinks "the press 6 unload station", and "loader" reads as the thing that fills. Qualify it (**loader station**) anywhere it could meet the two meanings above | Nothing to rename; don't spread "station" for a loader beyond the Nodes page |
| **HMI screen inside a cell** | "Station" (domain type `shingo-edge/domain/station.go`), "OperatorStation" (API endpoint, JSON field), "screen" (Edge UI) | **"Operator Station"** in code (matches existing API). In UI labels, **screen** — *Operator screens* is the Processes page's tab and editor | Data types and APIs already match; UI strings that still say "station" for an HMI move to "screen" as their files are touched |
| **Order list page on Edge** | **Done.** URL `/orders`, page identifier `"orders"`, handler `handleOrders`. HTMX targets use `/orders/partial`. | — (completed) | — |
| **Admin-created one-off order** | Edge: "Manual Order" (types move/retrieve/store/complex). Core: still "Spot Order" (subtypes transport/staged/swap/send_to_location) — rename to "Manual Order" is outstanding. | **"Manual Order"** — clearer than "Spot," and Edge's term is the broader one. Core's `/orders` admin modal should be renamed to "Manual Order." Subtype vocabularies stay distinct because they represent genuinely different operations | Rename Core's `apiSpotOrderSubmit` to `apiManualOrderSubmit`. Rename `.spot-tabs` CSS to `.manual-order-tabs`. Update Core nav label "Spot Order" → "Manual Order" |
| **What Core calls "edge-station" in NodeType** | `EDGE` NodeType code described as "edge station" | Keep the code as `EDGE` (short codes are intentional). Rename the human description to "edge cell" | One docstring change on `shingo-core/domain/node_type.go:6` |

Reconciliation is opportunistic adjacency — bundle these into the consistency refactor PRs as files get touched. They're not blockers; they're cleanup.

### Units and casing

**Casing history — and a warning about how it was mis-measured twice.**

`752dec99` (2026-07-22) swept nine files and **held perfectly**: not one of them
has regressed. `8272aac0`'s follow-up (2026-07-26) finished the job across the
files that sweep never covered. What went wrong in between was the *measurement*,
not the code, and it went wrong the same way twice. (This paragraph first cited
`2c0d3c48` — the `--sub-*` substrate-ramp commit from the same merge. A wrong
SHA inside the passage about getting the record wrong; corrected here.)

**Raw `UOP` counts are meaningless here.** Most occurrences in the tree are
supposed to be uppercase — `{{.UOPRemaining}}`, `UOPCapacity`, `remainingUOP`,
`lsUOP`, `data-uop`, `bin_uop_ledger`. An unfiltered grep returns ~199 and reads
as catastrophic drift; a differently-scoped one returned "34 vs 7" and then
"44 vs 30", which reads as a rule actively decaying. **Neither number described
a real problem**, and the second was used to justify re-opening an item that had
in fact held. Never quote an unfiltered count for this term.

The only question that means anything is: **which rendered text says `UOP`?**
Sort every hit into a bucket before touching anything:

| Bucket | Verdict |
|---|---|
| Identifier / field ref / `data-*` attribute | Correct. Leave it. Renaming a serialized field breaks the protocol |
| Rendered text in a file a previous sweep covered | A real regression |
| Rendered text in a file no sweep ever covered | Not drift — an unswept surface |
| Prose in a comment | Not the product, but it is where the next implementer copies the casing from |

Applied to the 2026-07-26 pass, all 19 rendered hits were in the **third**
bucket. `752dec99` enumerated templates plus three Core page scripts; the
**entire Operator HMI** (`shingo-edge/www/static/operator-station/`) and Edge's
`static/js/pages/` were never in its scope. The highest-visibility surface in
the system — what an operator reads all shift — had simply never been swept.
Comments were swept in the second pass on purpose, reversing the first pass's
call, on the grounds above.

**The discriminator, if you write a guard.** A test that bans the string `UOP`
fails against a *correct* tree, because it cannot tell a label from a field ref.
The predicate that can is a word-boundary match:

```
(?<![A-Za-z0-9_])UOP(?![A-Za-z0-9_])
```

Verify it **both ways** before trusting it — it must fire on `>UOP Remaining<`
and `' UOP)'`, and stay silent on `{{.UOPRemaining}}`, `data-uop-capacity`,
`remainingUOP` and `MinHysteresisUOP`. A guard that cannot separate those two
lists is not a guard. No such test exists yet.

**The checkable end state:** that pattern returns nothing in any `.js` / `.css` /
`.html` under `shingo-core/www`, `shingo-edge/www` or `shared/`. Verifiable in
ten seconds, unlike a count in a table.

### Words on the setup screens

One word per thing on the Edge Processes page and the station composer. A
field keeps the name its claim gives it ([Desktop composer patterns](ui-style-guide/16-desktop-composer-patterns-u9.md) › "A field
is called what the claim calls it"); these are the nouns around the fields.

| Say | Never | Why |
|---|---|---|
| **part** | payload (in UI text) | the plant's word on every surface, from the picker to the finding pill. `payload_code` stays the field name in code and on the wire; field labels and refusals say *part* (*Auto request part*; a claim with none is asked for a part) |
| **flow** | — | what one part does on this process: which positions it works, how each swaps, where its bins come from and go |
| **preset** | — | a named **shape** of flow. A preset is never a part and never names one; parts are applied to it |
| **screen** | station, for an HMI | see Operator Station above |
| **1‑Robot Swap**, **2‑Robot Swap**, **2‑Robot Index**, **Sequential A/B** | Single-robot swap, Two-robot swap, 2-Robot Press Index | the chip's word, everywhere, server refusals included. Go's `SwapModeWord` and `composer-model.js`'s `MODES` are the one table (pinned, see [Drift detection](ui-style-guide/24-drift-detection.md)) and keep its non-breaking hyphen |
| **Add a position** | — | one phrase on every door that turns a position on in a flow (the table's *+ Add a position*, the free-position menu, the empty bar's *Add a position below, or use a preset*) |
| a node name, in full | `PLN_01` → `P01` | the plant named its nodes; a shortened name is a second name nobody can search for. Too long for its box, it is cut in the middle with the full name in the tooltip |

The changeover picker's red pill says what Core answered — **No bin at
Core** (at least one part has no available bin), with Core's own note
(`missing PANEL-A`) on the row and as the pill's title — and never "No
parts". A style Core has not checked reads **Not checked at Core**. A pill
that says more than its source said is a pill that sends someone looking for
the wrong problem.

### State words

What a process or a station is doing is **three words, derived, never
stored**:

| Words | When |
|---|---|
| **Changing over** | the process's `target_style_id` is set and differs from its `active_style_id` |
| **Running `<style>`** | otherwise, when a style is running (the station view's `current_style`; the process list's running style) |
| **No part running** | neither |

Changing over is tested first: during a changeover the old style is still the
running one, and "Running Style A" would hide the changeover. Both readers —
the station's `stationState` (`operator-render.js`: header, style chip,
footer badge, empty grid) and the Processes list's State column
(`processes-desktop.js`) — derive from fields their payloads already carry,
and each surface reads its one derivation so its places cannot disagree.

**Never store a state you can derive.** `processes.production_state` held the
same fact as the two style pointers, written at changeover start, cutover and
cancel; a Settings save could write a stale copy back over the live one. It
was dropped (Edge migration v16) along with `operator_stations.device_mode`,
which had no reader and two defaults that disagreed. Before adding a state
column or payload field, check whether the words can be computed from what is
already sent; if they can, compute them, in one function per surface.

### Working principle

The glossary is the source of truth. Where the UI uses an inconsistent name
today, the inconsistency is a defect to fix during migration. **When the
system as a whole means one thing, both surfaces should call it the same
thing** — there's no legitimate reason for Core to say "spot order" while Edge
says "manual order," or for "Station" to mean two different things at two different
scales. Each row in the reconciliation table is a small consistency win
available to anyone touching the relevant file.

## Naming Conventions

- **Go structs** use Shingo terms: `store.Bin`, `store.Payload`, `store.Node`
- **JSON API fields** use `snake_case`: `bin_type_id`, `payload_code`, `manifest_confirmed`
- **HTML/CSS classes** use `kebab-case`: `tile-loc`, `occupancy-modal`
- **Vendor-specific code** (inside `rds/`, Fleet Explorer) uses the vendor's own terminology
