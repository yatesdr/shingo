# Terminology Reference

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

### Bin

A physical container that holds materials and is tracked as it moves between nodes. Each bin has a type, a status, and optionally a payload assignment (payload code, manifest, UOP remaining). The bin is the primary physical entity in Shingo.

| System | Term |
|---|---|
| Shingo | **Bin** |
| Seer RDS | Goods / Container (`goodsId`, `containerName`) |
| MiR | Payload |
| Locus Robotics | Tote / Cart |
| VDA 5050 | Load |

Bin statuses: `available`, `staged`, `flagged`, `maintenance`, `quality_hold`, `retired`.

### Payload

A template defining what a bin should contain — its expected parts, quantities, and UOP capacity. When a payload is assigned to a bin, the bin receives the payload code, its manifest is populated from the template, and UOP remaining is set to capacity.

| System | Term |
|---|---|
| Shingo | **Payload** |
| Warehouse systems | SKU / Item master |

A bin assigned a payload is only eligible for dispatch when its manifest is confirmed (`manifest_confirmed = true`), its status is `available`, and it is not claimed by another order.

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

Order statuses: `pending`, `sourcing`, `queued`, `dispatched`, `in_transit`, `delivered`, `confirmed`, `failed`, `cancelled`. (There is no `completed` status — terminal completion is `confirmed`.)

### Reservation

A soft, revocable hold recording that an order intends to use a resource — a source **bin** or a destination **slot** — set *before* the bin is claimed. Reservations are what let Core hold an unfulfillable order (`queued`/`sourcing`) and retry as inventory appears, rather than failing the order. Reclaimed when the owning order ends; never time-based. See [reservations.md](reservations.md).

### Slot

A physical destination node a bin can be delivered to (a storage slot, staging location, or line-side position). A **slot reservation** is the destination-side mirror of a bin reservation: a soft hold on a node, confirmed atomically with the slot claim.

### Sourcing

The order status while Core is locating a source bin or destination. An order in `sourcing` holds its sources as reservations and is retried each scanner tick; `sourcing` is not "stuck" — demand is operator-driven. Together `queued` and `sourcing` are the **acquiring** set.

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
not move. See [edge-identity-rollout.md](edge-identity-rollout.md).

### Process

A production area monitored by a station. Stored in the database as `production_lines`. Each process can produce one active job style at a time.

### Style

An end-item type that a process produces. Stored in the database as `job_styles`. Switching the active style on a process is handled through the changeover workflow.

### Bin Loader / Unloader

A staging point where an operator manually fills empty bins (a **loader**, role `produce`) or empties full bins (an **unloader**, role `consume`) to feed/drain the kanban loop. Core owns the loader aggregate (`bin_loaders`); the Edge runtime consumes it as a first-class `domain.Loader`. A loader has one of two **layouts**:

| Term | Meaning |
|---|---|
| **Shared window** | A loader with **N window nodes** that all present the **same** shared payload set and draw on **one** budget. An empty may be delivered to **any free window**. The invariant: one demand of N → exactly N empties in flight across all windows, never 2N. |
| **Window** | One load point of a shared-window loader. Carries **no** per-position payload (the shared set is loader-level). Marked explicitly as `kind = "window"` — *not* inferred from an empty payload. |
| **Dedicated positions** (a.k.a. *home-location*) | A loader with **N independent** positions, each its own node bound to one payload, each its own one-bin slot. Positions do **not** share a budget. |
| **Position** | One dedicated home: one node, one payload (which may be unassigned/empty until the operator picks one). Marked `kind = "dedicated"`. |
| **Anchor** | The loader's identity node (`core_node_name`). Historically also the shared demand key; being retired in favor of an explicit `loader_id` (see the multi-window refactor). |

**Budget** = a loader's total physical bin slots (`SlotCount`). For a shared-window loader it is the shared empty-in ceiling across all windows.

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

### Supermarket

An automated storage area consisting of lanes and a shuffle row, represented as a node group (`NGRP`).

### Lane

A linear sequence of storage slots within a supermarket. The front slot (depth 1) is robot-accessible; deeper slots are blocked by those in front. Because only the mouth is reachable, a lane needs arbitration between orders that want it at the same time — see [lanes.md](lanes.md).

### Shuffle Row

Holding slots used during retrieval reshuffles. When a target bin is blocked, the blocking bins are parked elsewhere and the target is retrieved. The blockers are **not** moved back — see [material-flow.md](material-flow.md). A blocker may be parked in another lane in the same group, not only in the shuffle row.

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

### Changeover

The workflow for switching a production line from one job style to another. Progresses through a fixed sequence: stopping, counting out, storing, delivering, counting in, and ready.

### Departed

A swap leg that has finished with its **cell** but is not finished as an order: the fleet has confirmed the last step of the leg's plan whose node belongs to the cell, and the robot is now carrying a bin away from it.

A departed leg is still live — Core, telemetry, the orders page and swap-peer handling all still own it. What it stops being is the *cell's*: the two admission guards, the station card's action button and the auto-relief tick all treat the cell as free. Rendered on the card as **TO MARKET**, the opposite number of ROBOT IN TRANSIT (a cell *waiting on* a delivery).

Derived from `steps_json` against the claim's cell set, never from the swap mode. See [order-lifecycle.md](order-lifecycle.md#departed-legs-and-cell-done).

## Naming Conventions

- **Go structs** use Shingo terms: `store.Bin`, `store.Payload`, `store.Node`
- **JSON API fields** use `snake_case`: `bin_type_id`, `payload_code`, `manifest_confirmed`
- **HTML/CSS classes** use `kebab-case`: `tile-loc`, `occupancy-modal`
- **Vendor-specific code** (inside `rds/`, Fleet Explorer) uses the vendor's own terminology
