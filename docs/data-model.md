# Data Model

Verified against: 51489c78

This document describes the core data entities in Shingo, their relationships, and the key concepts that drive dispatch and storage logic.

---

## Entity Overview

```
Payload (template)
    |
    v
Bin (physical container)  -->  BinType (container class)
    |
    v
Node (floor location)     -->  NodeType (location class)
    |
    v
Order (transport request)
```

A **Payload** defines what a container should hold. A **Bin** is the physical container; it carries its payload assignment, manifest, and UOP remaining directly. An **Order** moves bins between **Nodes**: its carts, one `order_bins` row per lift.

---

## Entities

### Payload

A template defining a container's expected contents and capacity. Analogous to a product SKU for containers.

Columns: [`payloads`](#core-payloads). `uop_capacity` is the production cycles a full load supports; the template
manifest (parts and quantities for a full load) is its rows in [`payload_manifest`](#core-payload_manifest).

**Relationships:**
- Has many compatible **bin_types** (via `payload_bin_types` junction)
- Can be assigned to **nodes** (via `node_payloads` junction) — controls which payloads are accepted at delivery nodes

### Bin

A physical container that can be tracked, moved, and stored. The bin is the primary physical entity. It carries its payload assignment, manifest, and consumption state directly.

Columns: [`bins`](#core-bins). Notes the schema does not say:

- `label` is not scanned — see [terminology.md](terminology.md#label).
- `node_id` is where the bin stands. A lifted bin stands on `_TRANSIT` or on a robot's carrier node; a retired
  bin stands nowhere (NULL).
- `status` is the lifecycle state ([Bin Statuses](#bin-statuses)).
- `claimed_by` is the order that has claimed the bin for transport, set with a `confirmed` **reservation** (see
  [Reservations](#reservations)). They can drift briefly: `ReleaseOrphanedClaims` clears the claim and leaves the
  reservation row for `ReapOrphanedReservations`.
- `manifest` is the actual contents; `uop_remaining` the production cycles of material left.
- `loaded_at` is when the manifest was confirmed, and drives FIFO ordering.
- `staged_expires_at` is written NULL by every staging writer (`helpers.StagedSetSQL`): staging does not expire
  (`resolveNodeStaging` stamps no expiry). A non-NULL value is an old stamp, which reconciliation reports and
  never acts on.

**Relationships:**
- Belongs to one **BinType**
- Sits at one **Node** (nullable)
- Can be claimed by one **Order**

**The UOP ledger and its two permanent tables.** Every count change on a bin
writes a row to `bin_uop_ledger`: every applied delta with its before and after
totals. Its raw delta rows (`op = 'bin_uop_delta'`) are kept 90 days
(`binUOPDeltaRetentionDays`) and deleted by the daily Core sweep
(`audit.PurgeOldBinUOPDelta`), after that day's roll-up; every other op is kept.
Two permanent tables carry what outlives the raw rows:

- `bin_uop_exception` — one row per exception-worthy event, never deleted:
  `negative_crossing` (a bin taken below zero; `deepest_uop`/`recovered_at` carry the
  excursion's shape), `stale_epoch` and `payload_mismatch` (deltas the applier
  dropped), and `boundary` (one row per epoch bump — the per-bin binding ordinal,
  `epoch_seq`). This is the durable answer to "where are the negatives".
- `bin_uop_delta_daily` — one row per (day, bin, epoch, payload, reason, actor):
  ticks, consumed, added, first/last/min UOP, crossings. The durable "how parts
  moved" answer once the raw rows age out.

### BinType

Classification for physical containers. Controls compatibility with nodes and payloads.

Columns: [`bin_types`](#core-bin_types). Notes the schema does not say:

- `width_in`, `length_in`, `height_in` are inches; width and length are the footprint.
- `family_id` is the carrier family ([`carrier_families`](#core-carrier_families)): the physical cart this type is
  one setting of. Every type has one, by default its own.
- `required_robot_group` is the robot group an empty of this type needs; empty is any.
- `bare_of`, on a bare marker, is the carrier it stands for.
- `totes_per_layer` is how many totes one layer holds; null is not a tote carrier, or not said.

**Relationships:**
- Has many **Bins**
- Compatible with many **Payloads** (via `payload_bin_types`)
- Accepted at many **Nodes** (via `node_bin_types`) — used for lane bin type restrictions

### Node

A physical floor location where bins can sit.

Columns: [`nodes`](#core-nodes). `is_synthetic` marks a virtual grouping node (not a physical slot); `enabled` is
whether the node accepts orders; `depth` is a lane slot's position in its lane.

**Relationships:**
- Belongs to one **NodeType** (optional)
- Has one parent **Node** (optional, for hierarchy)
- Has many child **Nodes**
- Has many **Bins** at this location
- Has many **node_properties** (key-value pairs)
- Accepts specific **BinTypes** (via `node_bin_types`)
- Accepts specific **Payloads** (via `node_payloads`)
- Associated with **Stations** (via `node_stations`)

### NodeType

Classification for nodes. Controls dispatch behavior. The codes dispatch branches on are the `NodeClass*` constants
in `protocol/constants.go`:

| Code | Name | Synthetic | Purpose |
|------|------|-----------|---------|
| `NGRP` | Node Group | Yes | Groups lanes and/or direct children for dispatch resolution |
| `LANE` | Lane | Yes | Linear sequence of slots in a supermarket |
| `STOR` | Storage Slot | No | A standalone storage node: a storage dropoff by its type alone (`storageDropoffOf`) |

The migrations seed `NGRP` and `LANE`. The plant seeder (`shingo-core/cmd/seeddev`) also creates `STOR` and
`STATION`, the one type every station node carries; no rule reads `STATION`. A physical node may have no type.

### Order

A transport request to move a bin between nodes.

Columns: [`orders`](#core-orders). Notes the schema does not say:

- `edge_uuid` is the UUID the requesting edge station assigned; `station_id` is that station.
- `order_type` is an [Order Type](#order-types); `status` a [status](#order-statuses).
- `delivery_node` — see the caveat below. `source_node` and `delivery_node` are node names, not keys: the table
  has no reference to `nodes`.
- `vendor_order_id` and `vendor_state` are the fleet backend's order id and current state string.
- `named_bin_id` is the one bin the asker named the order for, if any — a constraint, not a record.
- `parent_order_id` is the parent compound order (for reshuffle child orders); `sequence` the step sequence within
  it.

The bins an order carries are not a column of the order: they are its rows in
`order_bins` ([Order carts](#order-carts-order_bins)).

#### `delivery_node`

For a plain order it is the destination: one bin, one place it rests. For a complex order (anything with a
`steps_json`) it is a display value, and two facts follow:

- **Edge and Core hold different values under one name.** `ComplexOrderRequest` carries no delivery node; Core
  derives its own at intake from the steps (`extractEndpoints`: the last pickup-or-dropoff). Never reason from one
  side's column about the other's.
- **Neither names where a leg's bin rests.** A complex leg has several dropoffs, auto-confirmed legs store `''`, and
  a leg names where the robot finished — a 3-position press-index R2 sets the fresh bin on the press and ends at the
  index node.

So "does this order leave a bin at node N" has one rule, `protocol.OrderPlacesBinAt`: a complex order by
`protocol.PlacesBinAt` over its steps, any other type by its delivery node. A leg's role is read the same way, off
its steps (`legTakesLineBin` on Core for the evac shape; `ReleaseStagedOrders` on the Edge through
`classifySwapLegsBySteps`). The Edge's column comment on `orders.delivery_node` ([Edge: `orders`](#edge-orders))
says which column is authoritative for which shape.

Core's column is load-bearing for routing: `patchRedirectSegments` rewrites a staged order's final dropoff to it so
a redirect reaches the robot, which holds because every leg the Edge builds ends on a dropoff
(`TestSwapBuilders_EveryLegEndsOnADropoff`).

### Order carts (`order_bins`)

An order's carts: one row per **lift** — the cart, the plan step that picks it
up and the plan step that sets it down. Every order that carries a cart has its
rows, written where the order comes to hold the cart and in the same statement
(the plain hold, a named issue, a dig leg's issue, the complex allocation, a
recovery, a re-point, the claim exchange). The rows are **never deleted when the
order ends**: "the carts of this order" and "the last order of this cart" read
the same table, live or ended.

Columns: [`order_bins`](#core-order_bins). `pick_step` is the plan position that lifts the cart; `-1` when it is
already in the robot's hands as the plan starts (a recovery's unload-only plan). `drop_step` is the plan position
that sets it down; NULL on an open lift (a dwelling dig leg, whose set-down its release binds).

The rows hold **plan positions, never nodes**. Where a step happens is the
plan's: `domain.StepNode` in Go, `StepNodeSQL` in SQL, pinned to each other. A
plain order has no stored plan; its plan is the two steps its endpoints imply
(the lift at `source_node`, the set-down at `delivery_node`), and its rows point
into that. A relayed cart (set down at staging and lifted again) has a row per
lift. Which cart is in the robot's forks is a fact about the plant, never a
column: the order's rows whose cart the order has claimed and that stands on
`_TRANSIT` or a robot's carrier node.

Unique on `(order_id, pick_step)` and on `(order_id, drop_step)`; checked
`(pick_step IS NOT NULL OR drop_step IS NULL) AND (drop_step IS NULL OR pick_step < drop_step)`;
indexed on `(bin_id, order_id DESC)` for the cart → order direction (the
stranded sweep, the orphan guard, cancel-return, the pages).

Who may delete a row: only a release of a cart not yet lifted, before the order
ends — a new allocation replacing the set (`ReplaceOrderBins`), a taker, the
pair release, the scanner's forget. Who may change one: a transform that moves
plan positions moves the rows with it (`ShiftOrderBinSteps`); the claim
exchange rewrites a row whose cart is not lifted yet.

A row with both steps NULL is a link whose steps are not known: the backfill
for orders that ended before the table existed. The link is true.

### Reservations

A soft, revocable hold recording that an order intends to use a resource, set *before* any hard column
(`claimed_by`) is written. The mechanism is [reservations.md](reservations.md)'s; the lane kinds are
[lanes.md](lanes.md)'s.

Columns: [`reservations`](#core-reservations). Notes the schema does not say:

- `resource_kind` is `bin` (a source bin), `slot` (a destination node), `mouth` (the right to work a lane), or
  `occupancy` (a robot is inside the lane now). `bin_id` is set for `bin` alone; `node_id`, the target node, for
  every other kind, enforced by a `CHECK`.
- `mode` is `inbound` \| `outbound` \| `dig` — the work direction, on **`mouth` rows only**; NULL on every other kind.
- `state` is `pending` (held, not yet shipped) or `confirmed` (shipped — the claim moved with it).
- `reserved_by` is an actor tag for forensics — also what identifies a `dig` row raised by an excavation from one
  raised by ordinary sourcing.

The partial unique indexes `uq_reservations_bin_active` (on `bin_id`) and `uq_reservations_slot_active` (on
`node_id`) make an acquire exactly-one-winner for `bin` and `slot` rows; `mouth` and `occupancy` rows have no such
index. A release is a hard `DELETE`, and rows are reaped when their owning order is terminal or gone, never by age.

---

## Statuses

### Bin Statuses

The five values of `domain.BinStatus` (`domain.AllBinStatuses`). The column carries no CHECK, so sourcing reads an
allow-list: only `available` and `staged` may be picked (`BinStatus.Sourceable`, twinned in SQL by
`SourceableStatusSQL`), and a value outside the enum is refused.

| Status | Description |
|--------|-------------|
| `available` | Normal operating state — sourceable |
| `staged` | At a non-storage node, where an operator is working at it (every arrival there stages, `resolveNodeStaging`); sourceable as a pickup, never counted as stock |
| `flagged` | Marked for attention — stays where it is, not sourceable |
| `maintenance` | Out for repair — not sourceable |
| `retired` | Out of operations for good; retiring also clears its node (`BinService.Retire`). The record is kept |

### Order Statuses

<!-- generated: protocol/status.go AllStatuses -->

| Status | Constant | Terminal | Moves to |
|---|---|---|---|
| `pending` | `StatusPending` | no | `submitted`, `queued`, `dispatched`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `sourcing` | `StatusSourcing` | no | `acknowledged`, `dispatched`, `in_transit`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `queued` | `StatusQueued` | no | `acknowledged`, `dispatched`, `in_transit`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `submitted` | `StatusSubmitted` | no | `acknowledged`, `queued`, `cancelled`, `failed`, `skipped` |
| `dispatched` | `StatusDispatched` | no | `in_transit`, `staged`, `delivered`, `queued`, `cancelled`, `faulted`, `failed` |
| `acknowledged` | `StatusAcknowledged` | no | `dispatched`, `in_transit`, `staged`, `cancelled`, `faulted`, `failed` |
| `in_transit` | `StatusInTransit` | no | `delivered`, `staged`, `cancelled`, `faulted`, `failed` |
| `staged` | `StatusStaged` | no | `in_transit`, `delivered`, `cancelled`, `faulted`, `failed` |
| `delivered` | `StatusDelivered` | no | `confirmed`, `cancelled`, `failed` |
| `confirmed` | `StatusConfirmed` | **yes** |  |
| `faulted` | `StatusFaulted` | no | `in_transit`, `delivered`, `failed`, `cancelled` |
| `failed` | `StatusFailed` | **yes** |  |
| `cancelled` | `StatusCancelled` | **yes** |  |
| `reshuffling` | `StatusReshuffling` | no | `confirmed`, `pending`, `cancelled`, `failed` |
| `skipped` | `StatusSkipped` | **yes** |  |

<!-- /generated -->

Core and Edge share the one vocabulary; what each status means, who writes it and how it crosses the wire is
[order-lifecycle.md](order-lifecycle.md)'s table. `queued` always carries its cause on the row (`queue_code`,
`queue_reason`), holds its sources as soft **reservations**, and is asked again each scanner tick.

`pending` and `queued` together form the **acquiring** set — the orders the
fulfillment scanner may ask (`IsAcquiring` in `protocol/status.go`). Acquiring
is the act between `pending` and `dispatched`, not a stored status.

### Order Types

`orders.order_type` on both sides holds an `OrderType`:

<!-- generated: protocol/types.go OrderType -->

| Order type | Constant | Meaning |
|---|---|---|
| `retrieve` | `OrderTypeRetrieve` | pull a loaded bin matching a payload to a destination |
| `retrieve_empty` | `OrderTypeRetrieveEmpty` | pull an empty bin compatible with a payload to a destination |
| `store` | `OrderTypeStore` | never a kind of order (the bin resolver's put mode); nothing mints it, kept unread |
| `move` | `OrderTypeMove` | generic move; no manifest semantics |
| `complex` | `OrderTypeComplex` | multi-step order composed of sub-steps |
| `ingest` | `OrderTypeIngest` | edge-legacy: no longer minted (manifest-only ingest write); kept for historical order rows |
| `reshuffle_restore` | `OrderTypeReshuffleRestore` | OrderTypeReshuffleRestore is the RETIRED restore-blockers housekeeping type. The subsystem that minted it is deleted (blockers lie now) and no order is ever created with it again — the constant is kept ONLY so historical rows (and the one-shot retirement sweep) display and match a name rather than a raw string. |

<!-- /generated -->

### Swap Modes

A style node claim's `swap_mode` (Core `style_claims`, Edge `style_node_claims`) holds one of the configurable
swap modes; what each mode does is [order-lifecycle.md](order-lifecycle.md)'s.

<!-- generated: protocol/swap_mode.go ConfigurableSwapModes -->

| Swap mode | Constant | Two robots |
|---|---|---|
| `single_robot` | `SwapModeSingleRobot` | no |
| `two_robot` | `SwapModeTwoRobot` | yes |
| `two_robot_press_index` | `SwapModeTwoRobotPressIndex` | yes |
| `sequential` | `SwapModeSequential` | no |

In `AllSwapModes` but not configurable (never stored on a claim): `simple` (`SwapModeSimple`), `manual_swap` (`SwapModeManualSwap`).

<!-- /generated -->

### Manifest Confirmation

Bins track whether their contents have been verified via `manifest_confirmed`:

| State | Meaning |
|-------|---------|
| `false` | Contents not verified — not sourceable as stock. Nor is it an empty: an empty carrier is a bin with no `payload_code` (`EmptyCarrierWhere`) |
| `true` | Operator has confirmed the manifest — bin is eligible for retrieval |

When `manifest_confirmed` is set to `true`, `loaded_at` is also set — this timestamp drives FIFO ordering.

---

## Key Concepts

### Bin-Centric Model

The system is bin-centric: the **bin** is the primary physical entity. Bins carry their payload assignment, manifest, and consumption state directly. Users think in bins and payloads. Dispatch checks `manifest_confirmed` and `status` on the bin itself.

### One Payload Per Bin

A bin has at most one payload assignment at a time (determined by `payload_code`). To change what a bin contains:
1. Clear the current payload (reset `payload_code`, `manifest`, `uop_remaining`)
2. Assign the new payload code
3. Confirm the manifest

### FIFO Ordering

When multiple bins match a retrieve request, the system picks the one with the oldest `loaded_at` timestamp. This ensures first-in-first-out material rotation.

### Dispatch Eligibility

The rule has one definition, `BinSourceableSQL` in
`shingo-core/store/internal/helpers/bin_sourceable.go`, and every sourcing reader
composes it. A bin is eligible for retrieval when:
1. it carries sourceable stock (`BinCarriesSourceableStockSQL`): the manifest is
   confirmed, the count is above zero (`StockSQL`), it is not on quality hold, and
   its status is `available` — `staged` passes the status allow-list but is
   excluded as stock, because an operator is working at it
2. it stands at an enabled, non-synthetic node (not a robot deck or `_TRANSIT`)
3. nobody holds it: no `claimed_by`, not `locked`, and no pending **reservation**
   (see [reservations.md](reservations.md))
4. where `payload_bin_types` declares carriers for the payload, the bin's type is
   one of them (`PayloadBinTypeRuleArm`)

### Node Hierarchy

Nodes can form hierarchies using `parent_id`:

```
NGRP (Node Group)
  |-- LANE-1
  |     |-- SLOT-1 (depth=1, front)
  |     |-- SLOT-2 (depth=2)
  |     +-- SLOT-3 (depth=3, back)
  |-- LANE-2
  |     |-- SLOT-1 (depth=1, front)
  |     +-- SLOT-2 (depth=2, back)
  +-- DIRECT-CHILD (non-lane physical node)
```

When an order targets a synthetic node (NGRP), the group resolver (`binresolver.GroupResolver`) walks the hierarchy
to a physical slot, by the group's `retrieve_algorithm` and `store_algorithm` properties; the codes are in
[node-naming-standards.md](node-naming-standards.md).

### Lane Storage

Lanes are linear sequences of slots where only the front slot (depth 1) is physically accessible. Storage is packed from the back (deepest first). See [lanes.md](lanes.md) for storage and reshuffle logic.

### Bin Type Restrictions

Lanes can restrict which bin types they accept (via `node_bin_types`). During store resolution, lanes that do not accept the incoming bin's type are skipped.

### Bin Loader Aggregate

Core owns bin loaders as an aggregate (`bin_loaders` + `bin_loader_homes` + `bin_loader_payloads`), synced to the
Edge cache (`core_loaders` + `core_loader_positions` + `core_loader_payloads`) and consumed by the Edge runtime as a
`domain.Loader`. The model is [bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md)'s; the
column facts that live here:

- A loader's `layout` (`shared_window` | `dedicated_positions`) is the one discriminator for how its homes behave,
  never inferred from "does it have positions" or "is the payload empty". A window of a shared loader carries no
  payload of its own (the set is `bin_loader_payloads`); a dedicated position carries one, blank until assigned.
- `home_kind` (`home` | `buffer`) says what a dedicated home with a blank `payload_code` is: a **home** not yet
  assigned a part, or a kept-partial **buffer** slot. It is `bin_loader_homes.home_kind` on Core,
  `LoaderPosition.HomeKind` on the wire and `core_loader_positions.home_kind` in the Edge cache, where blank is a
  Core that predates the field.
- `min_stock` on `bin_loader_homes` and `bin_loader_payloads` (and the Edge cache) is dormant: written 0, read by
  nothing.
- `no_clear_partials` is a dedicated loader's Clear partials switch stated as its restriction, so false (the
  default, and what a Core that predates it leaves off `LoaderInfo`) is the clear every loader has. It is
  `bin_loaders.no_clear_partials` on Core (v176) and `core_loaders.no_clear_partials` in the Edge cache (v46); what
  it turns off is [bin-loader-unloader-architecture.md](bin-loader-unloader-architecture.md#clear-partials)'s.

### Typed Domain Identifiers (Edge runtime)

The Edge `domain.Loader` and the surfaces built on it use newtypes — `LoaderID`, `NodeID`, `PayloadCode` — instead of bare strings, so the compiler rejects the bug class where an in-flight empty count is keyed by the wrong node string (process_node vs core_node). These are adopted on new surfaces only; legacy string call sites convert at the boundary (this is not a repo-wide rename).

---

## Schema Reference

Every table and column, generated from the committed schema snapshots that `make schema-snapshot` writes in each
module: Core's is `pg_dump --schema-only` of a fully migrated Postgres, the Edge's the DDL of a fully migrated
SQLite database. Core's schema declares no column comments; the Edge's DDL keeps the comments its migrations wrote,
shown in the Comment column. Each index's References column is the relationship map: a reference there is a
foreign key, and a column that names a node or an order by name (`orders.delivery_node`) is not one.

### Core tables (Postgres)

<!-- generated: shingo-core/store/schema/schema.snapshot.sql -->

86 tables.

| Table | Columns | Primary key | References |
|---|---|---|---|
| [`admin_users`](#core-admin_users) | 4 | `id` |  |
| [`area_confidence_daily`](#core-area_confidence_daily) | 17 | `day`, `area_name` |  |
| [`audit_log`](#core-audit_log) | 8 | `id` |  |
| [`bin_loader_home_bin_types`](#core-bin_loader_home_bin_types) | 2 | `position_node_id`, `bin_type_id` | [`bin_loader_homes`](#core-bin_loader_homes), [`bin_types`](#core-bin_types) |
| [`bin_loader_homes`](#core-bin_loader_homes) | 7 |  | [`bin_loaders`](#core-bin_loaders), [`nodes`](#core-nodes) |
| [`bin_loader_payloads`](#core-bin_loader_payloads) | 4 | `loader_id`, `payload_code` | [`bin_loaders`](#core-bin_loaders) |
| [`bin_loader_quotas`](#core-bin_loader_quotas) | 3 | `loader_id`, `bin_type_id` | [`bin_loaders`](#core-bin_loaders), [`bin_types`](#core-bin_types) |
| [`bin_loaders`](#core-bin_loaders) | 24 | `id` | `bin_loaders` |
| [`bin_types`](#core-bin_types) | 13 | `id` | [`carrier_families`](#core-carrier_families), `bin_types` |
| [`bin_uop_delta_daily`](#core-bin_uop_delta_daily) | 13 | `day`, `bin_id`, `epoch_seq`, `payload_code`, `reason`, `actor` |  |
| [`bin_uop_exception`](#core-bin_uop_exception) | 13 | `id` |  |
| [`bin_uop_ledger`](#core-bin_uop_ledger) | 16 | `id` |  |
| [`bins`](#core-bins) | 33 | `id` | [`bin_types`](#core-bin_types), [`nodes`](#core-nodes) |
| [`carrier_families`](#core-carrier_families) | 5 | `id` |  |
| [`cell_config`](#core-cell_config) | 6 | `cell_id` |  |
| [`cell_part_events`](#core-cell_part_events) | 9 |  |  |
| [`cell_targets`](#core-cell_targets) | 5 | `cell_id`, `payload_code` |  |
| [`cleaning_mission_setup`](#core-cleaning_mission_setup) | 7 | `id` |  |
| [`cleaning_missions`](#core-cleaning_missions) | 7 | `id` |  |
| [`cleaning_opt_outs`](#core-cleaning_opt_outs) | 1 | `vehicle_id` |  |
| [`cms_postings`](#core-cms_postings) | 13 | `id` |  |
| [`cms_transactions`](#core-cms_transactions) | 17 | `id` | [`bins`](#core-bins), [`nodes`](#core-nodes), [`orders`](#core-orders) |
| [`dashboards`](#core-dashboards) | 9 | `id` |  |
| [`demand_origins`](#core-demand_origins) | 29 | `origin_id` |  |
| [`demand_registry`](#core-demand_registry) | 9 | `id` |  |
| [`downtime_event_dedup`](#core-downtime_event_dedup) | 3 | `station`, `edge_event_id` |  |
| [`downtime_events`](#core-downtime_events) | 8 |  |  |
| [`edge_cells`](#core-edge_cells) | 6 | `station`, `cell_label` |  |
| [`edge_lineside_reports`](#core-edge_lineside_reports) | 5 | `station`, `core_node_name`, `payload_code` |  |
| [`edge_registry`](#core-edge_registry) | 24 | `id` |  |
| [`inbox`](#core-inbox) | 5 | `msg_id` |  |
| [`inventory_delta_dedup`](#core-inventory_delta_dedup) | 8 | `station`, `scope_kind`, `scope_key`, `epoch` |  |
| [`kanban_families`](#core-kanban_families) | 5 | `kanban_node_id`, `family_id` | [`carrier_families`](#core-carrier_families), [`kanbans`](#core-kanbans) |
| [`kanban_layer_levels`](#core-kanban_layer_levels) | 4 | `kanban_node_id`, `bin_type_id`, `layers` | [`bin_types`](#core-bin_types), [`kanbans`](#core-kanbans) |
| [`kanban_levels`](#core-kanban_levels) | 5 | `kanban_node_id`, `bin_type_id` | [`bin_types`](#core-bin_types), [`kanbans`](#core-kanbans) |
| [`kanban_sources`](#core-kanban_sources) | 3 | `kanban_node_id`, `rank` | [`kanbans`](#core-kanbans), [`nodes`](#core-nodes) |
| [`kanban_supports`](#core-kanban_supports) | 3 | `kanban_node_id`, `process_node_id` | [`kanbans`](#core-kanbans), [`nodes`](#core-nodes) |
| [`kanbans`](#core-kanbans) | 5 | `node_id` | [`nodes`](#core-nodes) |
| [`lane_confidence_daily`](#core-lane_confidence_daily) | 21 | `day`, `area_name`, `lane`, `version_id` | [`scene_lane_versions`](#core-scene_lane_versions) |
| [`lane_robot_confidence_daily`](#core-lane_robot_confidence_daily) | 16 | `day`, `area_name`, `lane`, `version_id`, `vehicle_id` | [`scene_lane_versions`](#core-scene_lane_versions) |
| [`lineside_buckets`](#core-lineside_buckets) | 8 | `id` |  |
| [`lineside_drain_ledger`](#core-lineside_drain_ledger) | 6 | `id` |  |
| [`load_sequences`](#core-load_sequences) | 3 | `name` |  |
| [`mission_events`](#core-mission_events) | 15 | `id` |  |
| [`mission_telemetry`](#core-mission_telemetry) | 21 | `id` |  |
| [`node_bin_types`](#core-node_bin_types) | 3 | `node_id`, `bin_type_id` | [`bin_types`](#core-bin_types), [`nodes`](#core-nodes) |
| [`node_payloads`](#core-node_payloads) | 3 | `node_id`, `payload_id` | [`nodes`](#core-nodes), [`payloads`](#core-payloads) |
| [`node_properties`](#core-node_properties) | 6 | `id` | [`nodes`](#core-nodes) |
| [`node_stations`](#core-node_stations) | 3 | `node_id`, `station_id` | [`nodes`](#core-nodes) |
| [`node_types`](#core-node_types) | 7 | `id` |  |
| [`nodes`](#core-nodes) | 12 | `id` | [`orders`](#core-orders), `nodes` |
| [`order_bins`](#core-order_bins) | 6 | `id` | [`bins`](#core-bins), [`orders`](#core-orders) |
| [`order_history`](#core-order_history) | 8 | `id` | [`orders`](#core-orders) |
| [`order_intake_refusals`](#core-order_intake_refusals) | 5 | `edge_uuid` |  |
| [`orders`](#core-orders) | 45 | `id` | `orders` |
| [`outbox`](#core-outbox) | 8 | `id` |  |
| [`parts`](#core-parts) | 6 | `id` |  |
| [`payload_bin_types`](#core-payload_bin_types) | 2 | `payload_id`, `bin_type_id` | [`bin_types`](#core-bin_types), [`payloads`](#core-payloads) |
| [`payload_containment`](#core-payload_containment) | 8 | `payload_code` |  |
| [`payload_manifest`](#core-payload_manifest) | 7 | `id` | [`parts`](#core-parts), [`payloads`](#core-payloads) |
| [`payloads`](#core-payloads) | 13 | `id` |  |
| [`plant_claims_reports`](#core-plant_claims_reports) | 4 | `process_id` |  |
| [`plant_confidence_daily`](#core-plant_confidence_daily) | 14 | `day` |  |
| [`process_positions`](#core-process_positions) | 2 | `process_id`, `core_node_name` |  |
| [`process_styles`](#core-process_styles) | 6 | `process_id`, `style_id` |  |
| [`recovery_actions`](#core-recovery_actions) | 7 | `id` |  |
| [`reservations`](#core-reservations) | 10 | `id` | [`bins`](#core-bins), [`nodes`](#core-nodes), [`orders`](#core-orders) |
| [`robot_alarm_onsets`](#core-robot_alarm_onsets) | 5 | `hour`, `vehicle_id`, `code` |  |
| [`robot_confidence_daily`](#core-robot_confidence_daily) | 8 | `day`, `vehicle_id` |  |
| [`robot_confidence_low`](#core-robot_confidence_low) | 16 |  |  |
| [`robot_confidence_samples`](#core-robot_confidence_samples) | 16 |  |  |
| [`robot_hours`](#core-robot_hours) | 9 | `hour`, `vehicle_id` |  |
| [`robot_lidar_hours`](#core-robot_lidar_hours) | 11 | `hour`, `vehicle_id`, `lidar` |  |
| [`scene_areas`](#core-scene_areas) | 16 | `id` | [`scene_diffs`](#core-scene_diffs), [`scene_map_versions`](#core-scene_map_versions), `scene_areas` |
| [`scene_diffs`](#core-scene_diffs) | 11 | `id` |  |
| [`scene_edges`](#core-scene_edges) | 15 | `id` |  |
| [`scene_lane_versions`](#core-scene_lane_versions) | 14 | `id` | [`scene_diffs`](#core-scene_diffs), `scene_lane_versions` |
| [`scene_map_versions`](#core-scene_map_versions) | 12 | `id` | [`scene_diffs`](#core-scene_diffs) |
| [`scene_points`](#core-scene_points) | 13 | `id` |  |
| [`scene_reflectors`](#core-scene_reflectors) | 11 | `id` | [`scene_diffs`](#core-scene_diffs), [`scene_map_versions`](#core-scene_map_versions), `scene_reflectors` |
| [`schema_migrations`](#core-schema_migrations) | 2 | `version` |  |
| [`sourceability_events`](#core-sourceability_events) | 13 | `id` |  |
| [`style_claims`](#core-style_claims) | 16 |  |  |
| [`supply_refusals`](#core-supply_refusals) | 11 | `id` |  |
| [`test_commands`](#core-test_commands) | 11 | `id` |  |
| [`tte_samples`](#core-tte_samples) | 13 | `id` |  |

#### Core: `admin_users`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('admin_users_id_seq')` | PK |
| `username` | `text` | yes |  | unique |
| `password_hash` | `text` | yes |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `area_confidence_daily`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `day` | `date` | yes |  |  |
| `area_name` | `text` | yes |  |  |
| `class_name` | `text` | yes | `''` |  |
| `p05` | `double precision` |  |  |  |
| `p25` | `double precision` |  |  |  |
| `p50` | `double precision` |  |  |  |
| `p75` | `double precision` |  |  |  |
| `p95` | `double precision` |  |  |  |
| `samples` | `integer` | yes |  |  |
| `mean_good` | `double precision` |  |  |  |
| `samples_good` | `integer` | yes | `0` |  |
| `min_conf` | `double precision` |  |  |  |
| `robots` | `integer` | yes |  |  |
| `robots_seen` | `text[]` |  |  |  |
| `sentinel_samples` | `integer` | yes | `0` |  |
| `sentinel_robots` | `integer` | yes | `0` |  |
| `conf_hist` | `integer[]` |  |  |  |

- Primary key (day, area_name)

#### Core: `audit_log`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('audit_log_id_seq')` | PK |
| `entity_type` | `text` | yes |  |  |
| `entity_id` | `bigint` | yes | `0` |  |
| `action` | `text` | yes |  |  |
| `old_value` | `text` | yes | `''` |  |
| `new_value` | `text` | yes | `''` |  |
| `actor` | `text` | yes | `'system'` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `bin_loader_home_bin_types`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `position_node_id` | `bigint` | yes |  | → `bin_loader_homes(position_node_id) ON DELETE CASCADE` |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id) ON DELETE CASCADE` |

- Primary key (position_node_id, bin_type_id)

#### Core: `bin_loader_homes`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `loader_id` | `bigint` | yes |  | → `bin_loaders(id) ON DELETE CASCADE` |
| `position_node_id` | `bigint` | yes |  | unique, → `nodes(id)` |
| `payload_code` | `text` | yes |  |  |
| `min_stock` | `integer` | yes | `0` |  |
| `uop_threshold` | `integer` | yes | `0` |  |
| `sort_order` | `integer` | yes | `0` |  |
| `home_kind` | `text` | yes | `'home'` |  |

- Check `(home_kind = ANY (ARRAY['home', 'buffer']))`

#### Core: `bin_loader_payloads`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `loader_id` | `bigint` | yes |  | → `bin_loaders(id) ON DELETE CASCADE` |
| `payload_code` | `text` | yes |  |  |
| `min_stock` | `integer` | yes | `0` |  |
| `uop_threshold` | `integer` | yes | `0` |  |

- Primary key (loader_id, payload_code)

#### Core: `bin_loader_quotas`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `loader_id` | `bigint` | yes |  | → `bin_loaders(id) ON DELETE CASCADE` |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id) ON DELETE CASCADE` |
| `want` | `integer` | yes | `0` |  |

- Check `(want >= 0)`
- Primary key (loader_id, bin_type_id)

#### Core: `bin_loaders`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('bin_loaders_id_seq')` | PK |
| `name` | `text` | yes |  |  |
| `role` | `text` | yes |  |  |
| `layout` | `text` | yes |  |  |
| `replenishment` | `text` | yes |  |  |
| `outbound_dest` | `text` | yes | `''` |  |
| `inbound_source` | `text` | yes | `''` |  |
| `config_gen` | `bigint` | yes | `1` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `archived_at` | `timestamp with time zone` |  |  |  |
| `funnel_windows` | `boolean` | yes | `false` |  |
| `accept_partials` | `boolean` | yes | `false` |  |
| `auto_push` | `boolean` | yes | `false` |  |
| `second_stage_loader_id` | `bigint` |  |  | → `bin_loaders(id)` |
| `fed_directly` | `boolean` | yes | `false` |  |
| `pulled_directly` | `boolean` | yes | `false` |  |
| `board_hides_idle` | `boolean` | yes | `false` |  |
| `board_shows_all_parts` | `boolean` | yes | `false` |  |
| `drains_empty` | `boolean` | yes | `false` |  |
| `drains_tops_bottoms` | `boolean` | yes | `false` |  |
| `drains_leftover` | `boolean` | yes | `false` |  |
| `drains_unverified` | `boolean` | yes | `false` |  |
| `no_clear_partials` | `boolean` | yes | `false` |  |

- Check `(layout = ANY (ARRAY['shared_window', 'dedicated_positions']))`
- Check `(replenishment = ANY (ARRAY['operator', 'threshold']))`
- Check `(role = ANY (ARRAY['produce', 'consume']))`

#### Core: `bin_types`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('bin_types_id_seq')` | PK |
| `code` | `text` | yes |  | unique |
| `description` | `text` | yes | `''` |  |
| `width_in` | `double precision` | yes | `0` |  |
| `height_in` | `double precision` | yes | `0` |  |
| `length_in` | `double precision` | yes | `0` |  |
| `required_robot_group` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `totes_per_layer` | `integer` |  |  |  |
| `bare_of` | `bigint` |  |  | unique, → `bin_types(id)` |
| `bare` | `boolean` | yes | `generated: ((bare_of IS NOT NULL))` |  |
| `family_id` | `bigint` | yes |  | → `carrier_families(id) ON DELETE RESTRICT` |

- Check `(bare_of <> id)`
- Check `(totes_per_layer > 0)`

#### Core: `bin_uop_delta_daily`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `day` | `date` | yes |  |  |
| `bin_id` | `bigint` | yes |  |  |
| `epoch_seq` | `bigint` | yes |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `reason` | `text` | yes | `''` |  |
| `actor` | `text` | yes | `''` |  |
| `ticks` | `integer` | yes |  |  |
| `consumed` | `integer` | yes |  |  |
| `added` | `integer` | yes |  |  |
| `first_uop` | `integer` |  |  |  |
| `last_uop` | `integer` |  |  |  |
| `min_uop` | `integer` |  |  |  |
| `crossings` | `integer` | yes | `0` |  |

- Primary key (day, bin_id, epoch_seq, payload_code, reason, actor)

#### Core: `bin_uop_exception`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('bin_uop_exception_id_seq')` | PK |
| `kind` | `text` | yes |  |  |
| `bin_id` | `bigint` |  |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `actor` | `text` | yes | `''` |  |
| `epoch_seq` | `bigint` |  |  |  |
| `occurred_at` | `timestamp with time zone` | yes |  |  |
| `before_uop` | `integer` |  |  |  |
| `after_uop` | `integer` |  |  |  |
| `deepest_uop` | `integer` |  |  |  |
| `recovered_at` | `timestamp with time zone` |  |  |  |
| `op` | `text` | yes |  |  |
| `detail` | `jsonb` |  |  |  |

#### Core: `bin_uop_ledger`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('bin_uop_ledger_id_seq')` | PK |
| `bin_id` | `bigint` | yes |  |  |
| `before_uop` | `integer` |  |  |  |
| `after_uop` | `integer` | yes |  |  |
| `op` | `text` | yes |  |  |
| `source` | `text` | yes | `''` |  |
| `order_id` | `bigint` |  |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `actor` | `text` | yes | `''` |  |
| `metadata` | `jsonb` |  |  |  |
| `applied_at` | `timestamp with time zone` | yes | `now()` |  |
| `node_id` | `bigint` |  |  |  |
| `station` | `text` | yes | `''` |  |
| `detail` | `jsonb` |  |  |  |
| `loader_id` | `bigint` |  |  |  |
| `reason` | `text` | yes | `''` |  |

#### Core: `bins`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('bins_id_seq')` | PK |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id)` |
| `label` | `text` | yes | `''` |  |
| `description` | `text` | yes | `''` |  |
| `node_id` | `bigint` |  |  | → `nodes(id)` |
| `status` | `text` | yes | `'available'` |  |
| `claimed_by` | `bigint` |  |  |  |
| `staged_at` | `timestamp with time zone` |  |  |  |
| `staged_expires_at` | `timestamp with time zone` |  |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `manifest` | `jsonb` |  |  |  |
| `uop_remaining` | `integer` | yes | `0` |  |
| `delta_epoch` | `bigint` | yes | `1` |  |
| `manifest_confirmed` | `boolean` | yes | `false` |  |
| `locked` | `boolean` | yes | `false` |  |
| `locked_by` | `text` | yes | `''` |  |
| `locked_at` | `timestamp with time zone` |  |  |  |
| `last_counted_at` | `timestamp with time zone` |  |  |  |
| `last_counted_by` | `text` | yes | `''` |  |
| `loaded_at` | `timestamp with time zone` |  |  |  |
| `anomaly_at` | `timestamp with time zone` |  |  |  |
| `quality_hold` | `boolean` | yes | `false` |  |
| `hold_by` | `text` | yes | `''` |  |
| `hold_at` | `timestamp with time zone` |  |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `lifted_at` | `timestamp with time zone` |  |  |  |
| `totes_on_cart` | `integer` |  |  |  |
| `last_lift_at` | `timestamp with time zone` |  |  |  |
| `lifted_from_node_id` | `bigint` |  |  | → `nodes(id) ON DELETE SET NULL` |
| `anomaly_note` | `text` | yes | `''` |  |
| `undeclared_carrier_at` | `timestamp with time zone` |  |  |  |
| `held_since` | `timestamp with time zone` |  |  |  |

- Check `(totes_on_cart >= 0)`
- Unique (label) where `label <> ''`

#### Core: `carrier_families`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('carrier_families_id_seq')` | PK |
| `code` | `text` | yes |  | unique |
| `description` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `cell_config`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `cell_id` | `text` | yes |  | PK |
| `station` | `text` | yes |  |  |
| `primary_process_id` | `bigint` | yes |  |  |
| `sub_process_ids` | `jsonb` | yes | `'[]'::jsonb` |  |
| `display_name` | `text` | yes | `''` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `cell_part_events`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('cell_part_events_id_seq')` |  |
| `cell_id` | `text` | yes |  |  |
| `recorded_at` | `timestamp with time zone` | yes |  |  |
| `edge_snapshot_id` | `bigint` | yes |  |  |
| `count_value` | `bigint` | yes | `0` |  |
| `delta` | `bigint` | yes | `0` |  |
| `anomaly` | `text` | yes | `''` |  |
| `process_id` | `bigint` | yes | `0` |  |
| `style_id` | `bigint` | yes | `0` |  |

- Partitioned: `RANGE (recorded_at)`

#### Core: `cell_targets`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `cell_id` | `text` | yes |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `target_cycle_ms` | `bigint` | yes | `0` |  |
| `owner` | `text` | yes | `''` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (cell_id, payload_code)

#### Core: `cleaning_mission_setup`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `smallint` | yes | `1` | PK |
| `points` | `text[]` | yes |  |  |
| `frequency` | `interval` | yes |  |  |
| `active_from` | `time without time zone` | yes |  |  |
| `active_to` | `time without time zone` | yes |  |  |
| `min_battery` | `smallint` | yes | `25` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Check `(id = 1)`

#### Core: `cleaning_missions`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('cleaning_missions_id_seq')` | PK |
| `vehicle_id` | `text` | yes |  |  |
| `vendor_order_id` | `text` | yes |  |  |
| `state` | `text` | yes |  |  |
| `reason` | `text` | yes |  |  |
| `dispatched_at` | `timestamp with time zone` | yes | `now()` |  |
| `finished_at` | `timestamp with time zone` |  |  |  |

#### Core: `cleaning_opt_outs`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `vehicle_id` | `text` | yes |  | PK |

#### Core: `cms_postings`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('cms_postings_id_seq')` | PK |
| `body_sha` | `text` | yes |  |  |
| `status` | `text` | yes | `'pending'` |  |
| `transaction_id` | `text` | yes | `''` |  |
| `http_status` | `integer` |  |  |  |
| `attempts` | `integer` | yes | `0` |  |
| `requeue_count` | `integer` | yes | `0` |  |
| `next_retry_at` | `timestamp with time zone` |  |  |  |
| `last_error` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `inflight_at` | `timestamp with time zone` |  |  |  |
| `posted_at` | `timestamp with time zone` |  |  |  |
| `settled_at` | `timestamp with time zone` |  |  |  |

#### Core: `cms_transactions`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('cms_transactions_id_seq')` | PK |
| `node_id` | `bigint` | yes |  | → `nodes(id)` |
| `node_name` | `text` | yes | `''` |  |
| `cat_id` | `text` | yes |  |  |
| `delta` | `bigint` | yes | `0` |  |
| `bin_id` | `bigint` |  |  | → `bins(id)` |
| `bin_label` | `text` | yes | `''` |  |
| `payload_code` | `text` | yes | `''` |  |
| `source_type` | `text` | yes |  |  |
| `order_id` | `bigint` |  |  | → `orders(id)` |
| `notes` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `posting_id` | `bigint` |  |  |  |
| `robot_id` | `text` | yes | `''` |  |
| `storeroom` | `text` | yes | `''` |  |
| `location_node_id` | `bigint` |  |  |  |
| `location_node_name` | `text` | yes | `''` |  |

- Unique (order_id, bin_id, node_id, cat_id, delta) NULLS NOT DISTINCT WHERE (order_id IS NOT NULL)

#### Core: `dashboards`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('dashboards_id_seq')` | PK |
| `name` | `text` | yes |  |  |
| `kind` | `text` | yes | `'task-board'` |  |
| `stations_json` | `text` | yes | `'[]'` |  |
| `config_json` | `text` | yes | `'{}'` |  |
| `enabled` | `boolean` | yes | `true` |  |
| `sort_order` | `integer` | yes | `0` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `demand_origins`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `origin_id` | `uuid` | yes |  | PK |
| `episode_key` | `text` | yes |  |  |
| `kind` | `text` | yes |  |  |
| `direction` | `text` | yes | `''` |  |
| `trigger_kind` | `text` | yes | `''` |  |
| `trigger_ref` | `text` | yes | `''` |  |
| `parent_origin_id` | `uuid` |  |  |  |
| `station_id` | `text` | yes | `''` |  |
| `process_id` | `text` | yes | `''` |  |
| `core_node_name` | `text` | yes | `''` |  |
| `payload_code` | `text` | yes | `''` |  |
| `opened_at` | `timestamp with time zone` |  |  |  |
| `opened_total` | `integer` | yes | `0` |  |
| `threshold` | `integer` | yes | `0` |  |
| `revision` | `bigint` | yes | `1` |  |
| `expected_orders` | `integer` |  |  |  |
| `expected_reason` | `text` | yes | `''` |  |
| `uop_delivered` | `integer` | yes | `0` |  |
| `rerequest_count` | `integer` | yes | `0` |  |
| `signal_count` | `integer` | yes | `0` |  |
| `discretionary` | `boolean` | yes | `false` |  |
| `closed_at` | `timestamp with time zone` |  |  |  |
| `close_reason` | `text` | yes | `''` |  |
| `closed_by` | `text` |  |  |  |
| `plan` | `text` | yes | `''` |  |
| `plan_verdict` | `text` | yes | `''` |  |
| `need_at` | `timestamp with time zone` |  |  |  |
| `predicted_need_at` | `timestamp with time zone` |  |  |  |
| `provenance` | `text` | yes | `''` |  |

- Unique (episode_key) where `closed_at IS NULL`

#### Core: `demand_registry`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('demand_registry_id_seq')` | PK |
| `station_id` | `text` | yes |  |  |
| `core_node_name` | `text` | yes |  |  |
| `role` | `text` | yes |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `outbound_dest` | `text` | yes | `''` |  |
| `replenish_uop_threshold` | `integer` | yes | `0` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `loader_id` | `bigint` |  |  |  |

- Unique (station_id, core_node_name, payload_code)

#### Core: `downtime_event_dedup`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `station` | `text` | yes |  |  |
| `edge_event_id` | `bigint` | yes |  |  |
| `applied_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (station, edge_event_id)

#### Core: `downtime_events`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('downtime_events_id_seq')` |  |
| `station` | `text` | yes |  |  |
| `plc_name` | `text` | yes |  |  |
| `reason` | `text` | yes | `''` |  |
| `started_at` | `timestamp with time zone` | yes |  |  |
| `ended_at` | `timestamp with time zone` |  |  |  |
| `duration_ms` | `bigint` | yes | `0` |  |
| `edge_event_id` | `bigint` | yes | `0` |  |

- Partitioned: `RANGE (started_at)`

#### Core: `edge_cells`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `station` | `text` | yes |  |  |
| `cell_label` | `text` | yes |  |  |
| `bindings` | `jsonb` | yes | `'[]'::jsonb` |  |
| `first_seen` | `timestamp with time zone` | yes | `now()` |  |
| `last_seen` | `timestamp with time zone` | yes | `now()` |  |
| `stale` | `boolean` | yes | `false` |  |

- Primary key (station, cell_label)

#### Core: `edge_lineside_reports`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `station` | `text` | yes |  |  |
| `core_node_name` | `text` | yes |  |  |
| `payload_code` | `text` | yes |  |  |
| `reported_at` | `timestamp with time zone` | yes |  |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (station, core_node_name, payload_code)

#### Core: `edge_registry`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('edge_registry_id_seq')` | PK |
| `station_uid` | `text` | yes | `''` |  |
| `display_name` | `text` | yes | `''` |  |
| `station_id` | `text` | yes |  | unique |
| `hostname` | `text` | yes | `''` |  |
| `version` | `text` | yes | `''` |  |
| `registered_at` | `timestamp with time zone` | yes | `now()` |  |
| `last_heartbeat` | `timestamp with time zone` |  |  |  |
| `status` | `text` | yes | `'active'` |  |
| `bound_hostname` | `text` | yes | `''` |  |
| `bound_instance` | `text` | yes | `''` |  |
| `prev_instance` | `text` | yes | `''` |  |
| `bound_at` | `timestamp with time zone` |  |  |  |
| `claimed_at` | `timestamp with time zone` |  |  |  |
| `conflict_hostname` | `text` | yes | `''` |  |
| `conflict_count` | `bigint` | yes | `0` |  |
| `conflict_at` | `timestamp with time zone` |  |  |  |
| `schema_version` | `integer` |  |  |  |
| `clock_offset_ms` | `bigint` |  |  |  |
| `timezone` | `text` | yes | `''` |  |
| `tick_pending` | `bigint` |  |  |  |
| `tick_oldest_unsent_age_ms` | `bigint` |  |  |  |
| `tick_reported_at` | `timestamp with time zone` |  |  |  |
| `tick_rejected` | `bigint` | yes | `0` |  |

- Unique (station_uid) where `station_uid <> ''`

#### Core: `inbox`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `msg_id` | `text` | yes |  | PK |
| `msg_type` | `text` | yes | `''` |  |
| `station_id` | `text` | yes | `''` |  |
| `processed_at` | `timestamp with time zone` | yes | `now()` |  |
| `reply_outbox_id` | `bigint` |  |  |  |

#### Core: `inventory_delta_dedup`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `station` | `text` | yes |  |  |
| `scope_kind` | `text` | yes |  |  |
| `scope_key` | `text` | yes |  |  |
| `epoch` | `bigint` | yes | `0` |  |
| `last_seq` | `bigint` | yes |  |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `applied_net` | `bigint` |  |  |  |
| `applied_window_end` | `timestamp with time zone` |  |  |  |

- Primary key (station, scope_kind, scope_key, epoch)

#### Core: `kanban_families`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `kanban_node_id` | `bigint` | yes |  | → `kanbans(node_id) ON DELETE CASCADE` |
| `family_id` | `bigint` | yes |  | → `carrier_families(id) ON DELETE RESTRICT` |
| `pool` | `integer` | yes |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Check `(pool >= 0)`
- Primary key (kanban_node_id, family_id)

#### Core: `kanban_layer_levels`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `kanban_node_id` | `bigint` | yes |  | → `kanbans(node_id) ON DELETE CASCADE` |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id) ON DELETE CASCADE` |
| `layers` | `integer` | yes |  |  |
| `want` | `integer` | yes |  |  |

- Check `(layers >= 0)`
- Check `(want >= 0)`
- Primary key (kanban_node_id, bin_type_id, layers)

#### Core: `kanban_levels`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `kanban_node_id` | `bigint` | yes |  | → `kanbans(node_id) ON DELETE CASCADE` |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id) ON DELETE CASCADE` |
| `want` | `integer` | yes |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Check `(want >= 0)`
- Primary key (kanban_node_id, bin_type_id)

#### Core: `kanban_sources`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `kanban_node_id` | `bigint` | yes |  | → `kanbans(node_id) ON DELETE CASCADE` |
| `rank` | `integer` | yes |  |  |
| `source_node_id` | `bigint` | yes |  | → `nodes(id) ON DELETE CASCADE` |

- Check `(source_node_id <> kanban_node_id)`
- Check `(rank >= 1)`
- Unique (kanban_node_id, source_node_id)
- Primary key (kanban_node_id, rank)

#### Core: `kanban_supports`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `kanban_node_id` | `bigint` | yes |  | → `kanbans(node_id) ON DELETE CASCADE` |
| `process_node_id` | `bigint` | yes |  | → `nodes(id) ON DELETE CASCADE` |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (kanban_node_id, process_node_id)

#### Core: `kanbans`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `node_id` | `bigint` | yes |  | PK, → `nodes(id) ON DELETE CASCADE` |
| `station_id` | `text` |  |  |  |
| `strict` | `boolean` | yes | `false` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `lane_confidence_daily`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `day` | `date` | yes |  |  |
| `area_name` | `text` | yes |  |  |
| `lane` | `text` | yes |  |  |
| `p05` | `double precision` |  |  |  |
| `p25` | `double precision` |  |  |  |
| `p50` | `double precision` |  |  |  |
| `p75` | `double precision` |  |  |  |
| `p95` | `double precision` |  |  |  |
| `samples` | `integer` | yes |  |  |
| `mean_good` | `double precision` |  |  |  |
| `samples_good` | `integer` | yes | `0` |  |
| `min_conf` | `double precision` |  |  |  |
| `robots` | `integer` | yes |  |  |
| `robots_seen` | `text[]` |  |  |  |
| `sentinel_samples` | `integer` | yes | `0` |  |
| `sentinel_robots` | `integer` | yes | `0` |  |
| `reloc_failed_samples` | `integer` | yes | `0` |  |
| `reloc_failed_robots` | `integer` | yes | `0` |  |
| `map_mismatch_samples` | `integer` | yes | `0` |  |
| `version_id` | `bigint` | yes |  | → `scene_lane_versions(id)` |
| `conf_hist` | `integer[]` |  |  |  |

- Primary key (day, area_name, lane, version_id)

#### Core: `lane_robot_confidence_daily`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `day` | `date` | yes |  |  |
| `area_name` | `text` | yes |  |  |
| `lane` | `text` | yes |  |  |
| `vehicle_id` | `text` | yes |  |  |
| `p05` | `double precision` |  |  |  |
| `p25` | `double precision` |  |  |  |
| `p50` | `double precision` |  |  |  |
| `p75` | `double precision` |  |  |  |
| `p95` | `double precision` |  |  |  |
| `samples` | `integer` | yes |  |  |
| `mean_good` | `double precision` |  |  |  |
| `samples_good` | `integer` | yes | `0` |  |
| `min_conf` | `double precision` |  |  |  |
| `sentinel_samples` | `integer` | yes | `0` |  |
| `version_id` | `bigint` | yes |  | → `scene_lane_versions(id)` |
| `conf_hist` | `integer[]` |  |  |  |

- Primary key (day, area_name, lane, version_id, vehicle_id)

#### Core: `lineside_buckets`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('lineside_buckets_id_seq')` | PK |
| `station` | `text` | yes |  |  |
| `core_node_name` | `text` | yes |  |  |
| `payload_code` | `text` | yes |  |  |
| `qty` | `integer` | yes |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `state` | `text` | yes |  |  |

- Check `(qty >= 0)`
- Check `(state = ANY (ARRAY['active', 'stranded']))`
- Unique (core_node_name, payload_code, state)

#### Core: `lineside_drain_ledger`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('lineside_drain_ledger_id_seq')` | PK |
| `node_id` | `bigint` | yes |  |  |
| `payload_code` | `text` | yes |  |  |
| `before_qty` | `integer` | yes |  |  |
| `after_qty` | `integer` | yes |  |  |
| `applied_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `load_sequences`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `name` | `text` | yes |  | PK |
| `task_names` | `text` | yes | `'[]'` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `mission_events`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('mission_events_id_seq')` | PK |
| `order_id` | `bigint` | yes |  |  |
| `vendor_order_id` | `text` | yes | `''` |  |
| `old_state` | `text` | yes |  |  |
| `new_state` | `text` | yes |  |  |
| `robot_id` | `text` | yes | `''` |  |
| `robot_x` | `double precision` |  |  |  |
| `robot_y` | `double precision` |  |  |  |
| `robot_angle` | `double precision` |  |  |  |
| `robot_battery` | `double precision` |  |  |  |
| `robot_station` | `text` | yes | `''` |  |
| `blocks_json` | `jsonb` | yes | `'[]'::jsonb` |  |
| `errors_json` | `jsonb` | yes | `'[]'::jsonb` |  |
| `detail` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `mission_telemetry`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('mission_telemetry_id_seq')` | PK |
| `order_id` | `bigint` | yes |  | unique |
| `vendor_order_id` | `text` | yes | `''` |  |
| `robot_id` | `text` | yes | `''` |  |
| `station_id` | `text` | yes | `''` |  |
| `order_type` | `text` | yes | `''` |  |
| `source_node` | `text` | yes | `''` |  |
| `delivery_node` | `text` | yes | `''` |  |
| `terminal_state` | `text` | yes | `''` |  |
| `vendor_created` | `timestamp with time zone` |  |  |  |
| `vendor_completed` | `timestamp with time zone` |  |  |  |
| `core_created` | `timestamp with time zone` |  |  |  |
| `core_completed` | `timestamp with time zone` |  |  |  |
| `duration_ms` | `bigint` | yes | `0` |  |
| `vendor_duration_ms` | `bigint` | yes | `0` |  |
| `blocks_json` | `jsonb` | yes | `'[]'::jsonb` |  |
| `errors_json` | `jsonb` | yes | `'[]'::jsonb` |  |
| `warnings_json` | `jsonb` | yes | `'[]'::jsonb` |  |
| `notices_json` | `jsonb` | yes | `'[]'::jsonb` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `robot_alarms_json` | `jsonb` |  |  |  |

#### Core: `node_bin_types`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `node_id` | `bigint` | yes |  | → `nodes(id) ON DELETE CASCADE` |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id) ON DELETE CASCADE` |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (node_id, bin_type_id)

#### Core: `node_payloads`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `node_id` | `bigint` | yes |  | → `nodes(id) ON DELETE CASCADE` |
| `payload_id` | `bigint` | yes |  | → `payloads(id) ON DELETE CASCADE` |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (node_id, payload_id)

#### Core: `node_properties`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('node_properties_id_seq')` | PK |
| `node_id` | `bigint` | yes |  | → `nodes(id) ON DELETE CASCADE` |
| `key` | `text` | yes |  |  |
| `value` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Unique (node_id, key)

#### Core: `node_stations`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `node_id` | `bigint` | yes |  | → `nodes(id) ON DELETE CASCADE` |
| `station_id` | `text` | yes |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

- Primary key (node_id, station_id)

#### Core: `node_types`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('node_types_id_seq')` | PK |
| `code` | `text` | yes |  | unique |
| `name` | `text` | yes |  |  |
| `description` | `text` | yes | `''` |  |
| `is_synthetic` | `boolean` | yes | `false` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `nodes`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('nodes_id_seq')` | PK |
| `name` | `text` | yes |  | unique |
| `display_name` | `text` | yes | `''` |  |
| `is_synthetic` | `boolean` | yes | `false` |  |
| `zone` | `text` | yes | `''` |  |
| `enabled` | `boolean` | yes | `true` |  |
| `depth` | `integer` |  |  |  |
| `node_type_id` | `bigint` |  |  |  |
| `parent_id` | `bigint` |  |  | → `nodes(id)` |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `claimed_by` | `bigint` |  |  | → `orders(id)` |

#### Core: `order_bins`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('order_bins_id_seq')` | PK |
| `order_id` | `bigint` | yes |  | → `orders(id)` |
| `bin_id` | `bigint` | yes |  | → `bins(id)` |
| `pick_step` | `integer` |  |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `drop_step` | `integer` |  |  |  |

- Check `(((pick_step IS NOT NULL) OR (drop_step IS NULL)) AND ((drop_step IS NULL) OR (pick_step < drop_step)))`
- Unique (order_id, drop_step)
- Unique (order_id, pick_step)

#### Core: `order_history`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('order_history_id_seq')` | PK |
| `order_id` | `bigint` | yes |  | → `orders(id)` |
| `status` | `text` | yes |  |  |
| `detail` | `text` | yes | `''` |  |
| `code` | `text` |  |  |  |
| `actor` | `text` |  |  |  |
| `ref` | `jsonb` |  |  |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `order_intake_refusals`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `edge_uuid` | `text` | yes |  | PK |
| `station_id` | `text` | yes | `''` |  |
| `error_code` | `text` | yes |  |  |
| `detail` | `text` | yes | `''` |  |
| `refused_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `orders`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('orders_id_seq')` | PK |
| `edge_uuid` | `text` | yes |  |  |
| `station_id` | `text` | yes | `''` |  |
| `order_type` | `text` | yes | `'retrieve'` |  |
| `status` | `text` | yes | `'pending'` |  |
| `quantity` | `bigint` | yes | `1` |  |
| `source_node` | `text` | yes | `''` |  |
| `delivery_node` | `text` | yes | `''` |  |
| `process_node` | `text` | yes | `''` |  |
| `vendor_order_id` | `text` | yes | `''` |  |
| `vendor_state` | `text` | yes | `''` |  |
| `robot_id` | `text` | yes | `''` |  |
| `priority` | `integer` | yes | `0` |  |
| `payload_desc` | `text` | yes | `''` |  |
| `error_detail` | `text` | yes | `''` |  |
| `steps_json` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `completed_at` | `timestamp with time zone` |  |  |  |
| `parent_order_id` | `bigint` |  |  | → `orders(id)` |
| `sequence` | `integer` | yes | `0` |  |
| `payload_code` | `text` | yes | `''` |  |
| `wait_index` | `integer` | yes | `0` |  |
| `queue_reason` | `text` | yes | `''` |  |
| `queue_code` | `text` |  |  |  |
| `queue_cause` | `text` |  |  |  |
| `skip_auto_confirm` | `boolean` | yes | `false` |  |
| `sibling_order_uuid` | `text` | yes | `''` |  |
| `key_route` | `text` | yes | `''` |  |
| `key_task` | `text` | yes | `''` |  |
| `source_intent` | `text` | yes | `''` |  |
| `coordinated` | `boolean` | yes | `false` |  |
| `remaining_uop` | `integer` |  |  |  |
| `origin_id` | `uuid` |  |  |  |
| `origin_class` | `text` | yes | `''` |  |
| `open_for_children` | `boolean` | yes | `false` |  |
| `station_unaware` | `boolean` | yes | `false` |  |
| `projects_to` | `text` | yes | `''` |  |
| `plan` | `text` | yes | `''` |  |
| `orphan_aged_at` | `timestamp with time zone` |  |  |  |
| `destination_resolved_at` | `timestamp with time zone` |  |  |  |
| `recovers_order_id` | `bigint` |  |  | → `orders(id)` |
| `named_bin_id` | `bigint` |  |  |  |
| `want` | `text` | yes | `''` |  |
| `steps_done` | `integer` | yes | `0` |  |

- Unique (edge_uuid) where `edge_uuid <> ''`

#### Core: `outbox`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('outbox_id_seq')` | PK |
| `topic` | `text` | yes |  |  |
| `payload` | `bytea` | yes |  |  |
| `msg_type` | `text` | yes | `''` |  |
| `station_id` | `text` | yes | `''` |  |
| `retries` | `integer` | yes | `0` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `sent_at` | `timestamp with time zone` |  |  |  |

#### Core: `parts`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('parts_id_seq')` | PK |
| `part_number` | `text` | yes |  | unique |
| `catid` | `text` | yes | `''` |  |
| `description` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `payload_bin_types`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `payload_id` | `bigint` | yes |  | → `payloads(id) ON DELETE CASCADE` |
| `bin_type_id` | `bigint` | yes |  | → `bin_types(id) ON DELETE CASCADE` |

- Primary key (payload_id, bin_type_id)

#### Core: `payload_containment`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `payload_code` | `text` | yes |  | PK |
| `active` | `boolean` | yes | `false` |  |
| `reason` | `text` | yes | `''` |  |
| `activated_by` | `text` | yes | `''` |  |
| `activated_at` | `timestamp with time zone` |  |  |  |
| `deactivated_by` | `text` | yes | `''` |  |
| `deactivated_at` | `timestamp with time zone` |  |  |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `payload_manifest`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('payload_manifest_id_seq')` | PK |
| `payload_id` | `bigint` | yes |  | → `payloads(id) ON DELETE CASCADE` |
| `part_number` | `text` | yes | `''` |  |
| `parts_per_cycle` | `bigint` | yes | `1` |  |
| `description` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `part_id` | `bigint` |  |  | → `parts(id)` |

- Check `(part_number <> '')`
- Unique (payload_id, part_number)

#### Core: `payloads`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('payloads_id_seq')` | PK |
| `code` | `text` | yes |  | unique |
| `description` | `text` | yes | `''` |  |
| `uop_capacity` | `integer` | yes | `0` |  |
| `robot_group` | `text` | yes | `''` |  |
| `advanced_load_sequence` | `text` | yes | `''` |  |
| `near_empty_enabled` | `boolean` | yes | `false` |  |
| `near_empty_robot_group` | `text` | yes | `''` |  |
| `near_empty_threshold_pct` | `integer` | yes | `0` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `tote_layers` | `integer` |  |  |  |
| `uop_per_tote` | `integer` |  |  |  |

- Check `(tote_layers > 0)`
- Check `(uop_per_tote > 0)`

#### Core: `plant_claims_reports`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `process_id` | `text` | yes |  | PK |
| `station_id` | `text` | yes |  |  |
| `digest` | `text` | yes |  |  |
| `received_at` | `timestamp with time zone` | yes |  |  |

#### Core: `plant_confidence_daily`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `day` | `date` | yes |  | PK |
| `samples_read` | `bigint` | yes | `0` |  |
| `orphan_samples` | `bigint` | yes | `0` |  |
| `unkeyable_edges` | `integer` | yes | `0` |  |
| `unkeyable_samples` | `bigint` | yes | `0` |  |
| `unversioned_samples` | `bigint` | yes | `0` |  |
| `map_mismatch_samples` | `bigint` | yes | `0` |  |
| `unattributed_samples` | `bigint` | yes | `0` |  |
| `robot_rows` | `integer` | yes | `0` |  |
| `lane_rows` | `integer` | yes | `0` |  |
| `area_rows` | `integer` | yes | `0` |  |
| `residuals_null` | `integer` | yes | `0` |  |
| `lanes_fail_only` | `integer` | yes | `0` |  |
| `lane_robot_rows` | `integer` | yes | `0` |  |

#### Core: `process_positions`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `process_id` | `text` | yes |  |  |
| `core_node_name` | `text` | yes |  |  |

- Primary key (process_id, core_node_name)

#### Core: `process_styles`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `process_id` | `text` | yes |  |  |
| `style_id` | `text` | yes |  |  |
| `config_gen` | `bigint` | yes | `0` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `is_active` | `boolean` | yes | `false` |  |
| `station_id` | `text` |  |  |  |

- Primary key (process_id, style_id)

#### Core: `recovery_actions`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('recovery_actions_id_seq')` | PK |
| `action` | `text` | yes |  |  |
| `target_type` | `text` | yes |  |  |
| `target_id` | `bigint` | yes | `0` |  |
| `detail` | `text` | yes | `''` |  |
| `actor` | `text` | yes | `'system'` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `reservations`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('reservations_id_seq')` | PK |
| `order_id` | `bigint` | yes |  | → `orders(id)` |
| `bin_id` | `bigint` |  |  | → `bins(id)` |
| `state` | `text` | yes | `'pending'` |  |
| `reserved_by` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `expires_at` | `timestamp with time zone` |  |  |  |
| `resource_kind` | `text` | yes | `'bin'` |  |
| `node_id` | `bigint` |  |  | → `nodes(id)` |
| `mode` | `text` |  |  |  |

- Check `(((resource_kind = 'bin') AND (bin_id IS NOT NULL) AND (node_id IS NULL)) OR ((resource_kind = ANY (ARRAY['slot', 'mouth', 'occupancy'])) AND (node_id IS NOT NULL) AND (bin_id IS NULL)))`
- Check `((mode IS NULL) OR (mode = ANY (ARRAY['inbound', 'outbound', 'dig'])))`
- Check `(resource_kind = ANY (ARRAY['bin', 'slot', 'mouth', 'occupancy']))`
- Check `(state = ANY (ARRAY['pending', 'confirmed']))`
- Unique (bin_id) where `(resource_kind = 'bin') AND (state = ANY (ARRAY['pending', 'confirmed']))`
- Unique (node_id) where `(resource_kind = 'slot') AND (state = ANY (ARRAY['pending', 'confirmed']))`

#### Core: `robot_alarm_onsets`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `hour` | `timestamp with time zone` | yes |  |  |
| `vehicle_id` | `text` | yes |  |  |
| `code` | `integer` | yes |  |  |
| `onsets` | `integer` | yes |  |  |
| `first_at` | `timestamp with time zone` | yes |  |  |

- Primary key (hour, vehicle_id, code)

#### Core: `robot_confidence_daily`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `day` | `date` | yes |  |  |
| `vehicle_id` | `text` | yes |  |  |
| `residual` | `double precision` |  |  |  |
| `cells` | `integer` | yes |  |  |
| `samples` | `integer` | yes |  |  |
| `mean` | `double precision` |  |  |  |
| `p05` | `double precision` |  |  |  |
| `map_mismatch_samples` | `integer` | yes | `0` |  |

- Primary key (day, vehicle_id)

#### Core: `robot_confidence_low`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('robot_confidence_low_id_seq')` |  |
| `vehicle_id` | `text` | yes |  |  |
| `sampled_at` | `timestamp with time zone` | yes |  |  |
| `confidence` | `double precision` | yes |  |  |
| `x` | `double precision` | yes |  |  |
| `y` | `double precision` | yes |  |  |
| `angle` | `double precision` | yes |  |  |
| `station` | `text` | yes | `''` |  |
| `last_station` | `text` | yes | `''` |  |
| `order_id` | `bigint` | yes | `0` |  |
| `on_task` | `boolean` | yes | `false` |  |
| `blocked` | `boolean` | yes | `false` |  |
| `reloc_status` | `smallint` | yes |  |  |
| `area_ids` | `text[]` |  |  |  |
| `map_md5` | `text` |  |  |  |
| `alarm_codes` | `integer[]` |  |  |  |

- Partitioned: `RANGE (sampled_at)`

#### Core: `robot_confidence_samples`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('robot_confidence_samples_id_seq')` |  |
| `vehicle_id` | `text` | yes |  |  |
| `sampled_at` | `timestamp with time zone` | yes |  |  |
| `confidence` | `double precision` | yes |  |  |
| `x` | `double precision` | yes |  |  |
| `y` | `double precision` | yes |  |  |
| `angle` | `double precision` | yes |  |  |
| `station` | `text` | yes | `''` |  |
| `last_station` | `text` | yes | `''` |  |
| `order_id` | `bigint` | yes | `0` |  |
| `on_task` | `boolean` | yes | `false` |  |
| `blocked` | `boolean` | yes | `false` |  |
| `reloc_status` | `smallint` | yes |  |  |
| `area_ids` | `text[]` |  |  |  |
| `map_md5` | `text` |  |  |  |
| `alarm_codes` | `integer[]` |  |  |  |

- Partitioned: `RANGE (sampled_at)`

#### Core: `robot_hours`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `hour` | `timestamp with time zone` | yes |  |  |
| `vehicle_id` | `text` | yes |  |  |
| `connected_s` | `integer` | yes | `0` |  |
| `charging_s` | `integer` | yes | `0` |  |
| `blocked_s` | `integer` | yes | `0` |  |
| `metres` | `double precision` | yes | `0` |  |
| `charge_starts` | `integer` | yes | `0` |  |
| `battery_min` | `double precision` |  |  |  |
| `lift_count` | `integer` |  |  |  |

- Primary key (hour, vehicle_id)

#### Core: `robot_lidar_hours`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `hour` | `timestamp with time zone` | yes |  |  |
| `vehicle_id` | `text` | yes |  |  |
| `lidar` | `text` | yes |  |  |
| `station` | `text` | yes |  |  |
| `use_for_loc` | `boolean` | yes |  |  |
| `beams` | `integer` | yes |  |  |
| `valid` | `integer` | yes |  |  |
| `rssi_med_valid` | `double precision` |  |  |  |
| `rssi_p25_valid` | `double precision` |  |  |  |
| `rssi_p75_valid` | `double precision` |  |  |  |
| `near_valid` | `integer` | yes |  |  |

- Primary key (hour, vehicle_id, lidar)

#### Core: `scene_areas`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_areas_id_seq')` | PK |
| `area_name` | `text` | yes |  |  |
| `class_name` | `text` | yes |  |  |
| `polygon` | `jsonb` | yes |  |  |
| `reflector_count` | `integer` | yes | `0` |  |
| `color_pen` | `bigint` |  |  |  |
| `color_brush` | `bigint` |  |  |  |
| `properties` | `jsonb` |  |  |  |
| `shape_hash` | `text` | yes |  |  |
| `def_hash` | `text` | yes |  |  |
| `max_vertex_delta_m` | `double precision` |  |  |  |
| `supersedes_id` | `bigint` |  |  | → `scene_areas(id)` |
| `diff_id` | `bigint` | yes |  | → `scene_diffs(id)` |
| `map_version_id` | `bigint` |  |  | → `scene_map_versions(id)` |
| `valid_from` | `timestamp with time zone` | yes |  |  |
| `valid_to` | `timestamp with time zone` |  |  |  |

- Unique (area_name) where `valid_to IS NULL`

#### Core: `scene_diffs`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_diffs_id_seq')` | PK |
| `source` | `text` | yes |  |  |
| `gate_hash` | `text` | yes |  |  |
| `observed_at` | `timestamp with time zone` | yes |  |  |
| `previous_sync` | `timestamp with time zone` |  |  |  |
| `objects_added` | `integer` | yes | `0` |  |
| `objects_changed` | `integer` | yes | `0` |  |
| `objects_removed` | `integer` | yes | `0` |  |
| `median_delta_m` | `double precision` |  |  |  |
| `max_delta_m` | `double precision` |  |  |  |
| `renames` | `jsonb` |  |  |  |

#### Core: `scene_edges`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_edges_id_seq')` | PK |
| `area_name` | `text` | yes |  |  |
| `instance_name` | `text` | yes |  |  |
| `class_name` | `text` | yes | `''` |  |
| `from_name` | `text` | yes | `''` |  |
| `to_name` | `text` | yes | `''` |  |
| `from_x` | `double precision` | yes | `0` |  |
| `from_y` | `double precision` | yes | `0` |  |
| `to_x` | `double precision` | yes | `0` |  |
| `to_y` | `double precision` | yes | `0` |  |
| `synced_at` | `timestamp with time zone` | yes | `now()` |  |
| `ctrl1_x` | `double precision` |  |  |  |
| `ctrl1_y` | `double precision` |  |  |  |
| `ctrl2_x` | `double precision` |  |  |  |
| `ctrl2_y` | `double precision` |  |  |  |

- Unique (area_name, instance_name)

#### Core: `scene_lane_versions`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_lane_versions_id_seq')` | PK |
| `area_name` | `text` | yes |  |  |
| `lane` | `text` | yes |  |  |
| `shape_hash` | `text` | yes |  |  |
| `def_hash` | `text` | yes |  |  |
| `shape` | `jsonb` | yes |  |  |
| `directed_rows` | `smallint` | yes |  |  |
| `twins_agree` | `boolean` | yes | `true` |  |
| `disagreement` | `text` | yes | `''` |  |
| `max_vertex_delta_m` | `double precision` |  |  |  |
| `supersedes_id` | `bigint` |  |  | → `scene_lane_versions(id)` |
| `diff_id` | `bigint` | yes |  | → `scene_diffs(id)` |
| `valid_from` | `timestamp with time zone` | yes |  |  |
| `valid_to` | `timestamp with time zone` |  |  |  |

- Unique (area_name, lane) where `valid_to IS NULL`

#### Core: `scene_map_versions`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_map_versions_id_seq')` | PK |
| `map_name` | `text` | yes |  |  |
| `content_sha` | `text` | yes |  |  |
| `map_md5` | `text` | yes | `''` |  |
| `source_robot` | `text` | yes | `''` |  |
| `body_gz` | `bytea` |  |  |  |
| `scan_cloud_gz` | `bytea` |  |  |  |
| `raw_bytes` | `bigint` | yes | `0` |  |
| `synced_at` | `timestamp with time zone` | yes |  |  |
| `superseded_at` | `timestamp with time zone` |  |  |  |
| `diff_id` | `bigint` |  |  | → `scene_diffs(id)` |
| `confirmed_at` | `timestamp with time zone` |  |  |  |

- Unique (map_name, content_sha)

#### Core: `scene_points`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_points_id_seq')` | PK |
| `area_name` | `text` | yes |  |  |
| `instance_name` | `text` | yes |  |  |
| `class_name` | `text` | yes |  |  |
| `point_name` | `text` | yes | `''` |  |
| `group_name` | `text` | yes | `''` |  |
| `label` | `text` | yes | `''` |  |
| `pos_x` | `double precision` | yes | `0` |  |
| `pos_y` | `double precision` | yes | `0` |  |
| `pos_z` | `double precision` | yes | `0` |  |
| `dir` | `double precision` | yes | `0` |  |
| `properties_json` | `jsonb` | yes | `'{}'::jsonb` |  |
| `synced_at` | `timestamp with time zone` | yes | `now()` |  |

- Unique (area_name, instance_name)

#### Core: `scene_reflectors`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('scene_reflectors_id_seq')` | PK |
| `kind` | `text` | yes |  |  |
| `x` | `double precision` | yes |  |  |
| `y` | `double precision` | yes |  |  |
| `width` | `double precision` |  |  |  |
| `shape_hash` | `text` | yes |  |  |
| `supersedes_id` | `bigint` |  |  | → `scene_reflectors(id)` |
| `diff_id` | `bigint` | yes |  | → `scene_diffs(id)` |
| `map_version_id` | `bigint` |  |  | → `scene_map_versions(id)` |
| `valid_from` | `timestamp with time zone` | yes |  |  |
| `valid_to` | `timestamp with time zone` |  |  |  |

#### Core: `schema_migrations`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `version` | `integer` | yes |  | PK |
| `applied_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `sourceability_events`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('sourceability_events_id_seq')` | PK |
| `process_key` | `text` | yes |  |  |
| `style_id` | `text` | yes | `''` |  |
| `payload_code` | `text` | yes | `''` |  |
| `sourceable` | `boolean` | yes |  |  |
| `status` | `text` | yes | `''` |  |
| `reason` | `text` | yes | `''` |  |
| `missing_payload` | `text` | yes | `''` |  |
| `op` | `text` | yes | `'sourceability_change'` |  |
| `source` | `text` | yes | `''` |  |
| `actor` | `text` | yes | `'system'` |  |
| `metadata` | `jsonb` |  |  |  |
| `observed_at` | `timestamp with time zone` | yes | `now()` |  |

#### Core: `style_claims`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `process_id` | `text` | yes |  |  |
| `style_id` | `text` | yes |  |  |
| `core_node_name` | `text` | yes |  |  |
| `role` | `text` | yes |  |  |
| `swap_mode` | `text` | yes |  |  |
| `payload_code` | `text` | yes | `''` |  |
| `allowed_payload_codes` | `text` | yes | `'[]'` |  |
| `uop_capacity` | `integer` | yes | `0` |  |
| `reorder_point` | `integer` | yes | `0` |  |
| `seq` | `integer` | yes | `0` |  |
| `inbound_source` | `text` | yes | `''` |  |
| `outbound_destination` | `text` | yes | `''` |  |
| `paired_core_node` | `text` | yes | `''` |  |
| `second_paired_core_node` | `text` | yes | `''` |  |
| `containment_destination` | `text` | yes | `''` |  |
| `partial_destination` | `text` | yes | `''` |  |

#### Core: `supply_refusals`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('supply_refusals_id_seq')` | PK |
| `loader_node` | `text` | yes |  |  |
| `payload_code` | `text` | yes |  |  |
| `station_id` | `text` | yes | `''` |  |
| `refused_at` | `timestamp with time zone` | yes |  |  |
| `refused_by` | `text` | yes | `''` |  |
| `ack_at` | `timestamp with time zone` |  |  |  |
| `ack_choice` | `text` | yes | `''` |  |
| `ack_process_id` | `text` | yes | `''` |  |
| `closed_at` | `timestamp with time zone` |  |  |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |

- Unique (loader_node, payload_code) where `closed_at IS NULL`

#### Core: `test_commands`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('test_commands_id_seq')` | PK |
| `command_type` | `text` | yes |  |  |
| `robot_id` | `text` | yes |  |  |
| `vendor_order_id` | `text` | yes | `''` |  |
| `vendor_state` | `text` | yes | `''` |  |
| `location` | `text` | yes | `''` |  |
| `config_id` | `text` | yes | `''` |  |
| `detail` | `text` | yes | `''` |  |
| `created_at` | `timestamp with time zone` | yes | `now()` |  |
| `updated_at` | `timestamp with time zone` | yes | `now()` |  |
| `completed_at` | `timestamp with time zone` |  |  |  |

#### Core: `tte_samples`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('tte_samples_id_seq')` | PK |
| `computed_at` | `timestamp with time zone` | yes |  |  |
| `process_id` | `text` | yes |  |  |
| `style_id` | `text` | yes |  |  |
| `core_node_name` | `text` | yes |  |  |
| `payload_code` | `text` | yes |  |  |
| `uop_remaining` | `integer` | yes |  |  |
| `rate_per_sec` | `double precision` | yes |  |  |
| `tte_seconds` | `double precision` |  |  |  |
| `style_status` | `text` | yes |  |  |
| `reorder_point` | `integer` | yes | `0` |  |
| `rate_grain` | `text` | yes | `''` |  |
| `kind` | `text` | yes | `'cell'` |  |

<!-- /generated -->

### Edge tables (SQLite)

<!-- generated: shingo-edge/store/schema/schema.snapshot.sql -->

44 tables.

| Table | Columns | Primary key | References |
|---|---|---|---|
| [`admin_users`](#edge-admin_users) | 4 | `id` |  |
| [`bin_returns`](#edge-bin_returns) | 7 | `order_uuid` |  |
| [`catid_holds`](#edge-catid_holds) | 16 | `id` |  |
| [`changeover_node_tasks`](#edge-changeover_node_tasks) | 11 | `id` | [`orders`](#edge-orders), [`process_changeovers`](#edge-process_changeovers), [`process_nodes`](#edge-process_nodes), [`style_node_claims`](#edge-style_node_claims) |
| [`changeover_participants`](#edge-changeover_participants) | 7 | `id` | [`changeover_node_tasks`](#edge-changeover_node_tasks), [`process_changeovers`](#edge-process_changeovers), [`process_nodes`](#edge-process_nodes) |
| [`changeover_station_tasks`](#edge-changeover_station_tasks) | 5 | `id` | [`operator_stations`](#edge-operator_stations), [`process_changeovers`](#edge-process_changeovers) |
| [`core_loader_payloads`](#edge-core_loader_payloads) | 3 | `loader_key`, `payload_code` |  |
| [`core_loader_positions`](#edge-core_loader_positions) | 6 | `loader_key`, `position_node` |  |
| [`core_loaders`](#edge-core_loaders) | 17 | `loader_key` |  |
| [`counter_snapshots`](#edge-counter_snapshots) | 11 | `id` | [`reporting_points`](#edge-reporting_points) |
| [`demand_origins_open`](#edge-demand_origins_open) | 17 | `episode_key` |  |
| [`feed_copy`](#edge-feed_copy) | 5 | `feed` |  |
| [`flow_presets`](#edge-flow_presets) | 8 | `id` | [`processes`](#edge-processes) |
| [`hourly_counts`](#edge-hourly_counts) | 6 | `id` | [`processes`](#edge-processes), [`styles`](#edge-styles) |
| [`hourly_counts_local_legacy`](#edge-hourly_counts_local_legacy) | 7 | `id` | [`processes`](#edge-processes), [`styles`](#edge-styles) |
| [`inventory_delta_seq`](#edge-inventory_delta_seq) | 6 | `scope_kind`, `scope_key`, `epoch` |  |
| [`line_counters`](#edge-line_counters) | 12 | `id` |  |
| [`node_lineside_bucket`](#edge-node_lineside_bucket) | 7 | `id` | [`process_nodes`](#edge-process_nodes) |
| [`operator_stations`](#edge-operator_stations) | 13 | `id` | [`processes`](#edge-processes) |
| [`order_history`](#edge-order_history) | 6 | `id` | [`orders`](#edge-orders) |
| [`orders`](#edge-orders) | 43 | `id` | [`process_nodes`](#edge-process_nodes), `orders` |
| [`outbox`](#edge-outbox) | 7 | `id` |  |
| [`payload_catalog`](#edge-payload_catalog) | 8 | `id` |  |
| [`plc_catid_observations`](#edge-plc_catid_observations) | 6 | `process_id`, `catid` | [`processes`](#edge-processes) |
| [`process_changeovers`](#edge-process_changeovers) | 15 | `id` | [`processes`](#edge-processes), [`styles`](#edge-styles) |
| [`process_groups`](#edge-process_groups) | 5 | `id` |  |
| [`process_node_runtime_states`](#edge-process_node_runtime_states) | 28 | `id` | [`orders`](#edge-orders), [`process_nodes`](#edge-process_nodes), [`style_node_claims`](#edge-style_node_claims) |
| [`process_nodes`](#edge-process_nodes) | 16 | `id` | [`operator_stations`](#edge-operator_stations), [`processes`](#edge-processes) |
| [`process_payloads`](#edge-process_payloads) | 4 | `id` | [`processes`](#edge-processes) |
| [`process_routing_nodes`](#edge-process_routing_nodes) | 10 | `id` | [`processes`](#edge-processes) |
| [`processes`](#edge-processes) | 15 | `id` | [`process_groups`](#edge-process_groups), [`styles`](#edge-styles) |
| [`production_tick_cursor`](#edge-production_tick_cursor) | 3 | `id` |  |
| [`release_paperwork`](#edge-release_paperwork) | 4 | `order_id`, `kind` |  |
| [`reporting_points`](#edge-reporting_points) | 8 | `id` | [`styles`](#edge-styles) |
| [`scene_geometry_edges`](#edge-scene_geometry_edges) | 10 | `from_name`, `to_name` |  |
| [`scene_geometry_meta`](#edge-scene_geometry_meta) | 3 | `id` |  |
| [`scene_geometry_points`](#edge-scene_geometry_points) | 5 | `instance_name` |  |
| [`schema_migrations`](#edge-schema_migrations) | 2 | `version` |  |
| [`shifts`](#edge-shifts) | 5 | `id` |  |
| [`sourcing_state`](#edge-sourcing_state) | 9 | `process_id`, `style_id` |  |
| [`style_node_claims`](#edge-style_node_claims) | 49 | `id` | [`styles`](#edge-styles) |
| [`style_node_claims_quarantine`](#edge-style_node_claims_quarantine) | 53 |  |  |
| [`styles`](#edge-styles) | 7 | `id` | [`processes`](#edge-processes) |
| [`supply_refusals_open`](#edge-supply_refusals_open) | 7 | `loader_node`, `payload_code` |  |

#### Edge: `admin_users`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `username` | `TEXT` | yes |  | unique |
| `password_hash` | `TEXT` | yes |  |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |

#### Edge: `bin_returns`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `order_uuid` | `TEXT` | yes |  | PK |
| `bin_label` | `TEXT` | yes | `''` |  |
| `payload_code` | `TEXT` | yes | `''` |  |
| `state` | `TEXT` | yes |  |  |
| `destination` | `TEXT` | yes | `''` |  |
| `reason` | `TEXT` | yes | `''` |  |
| `updated_at` | `TEXT` | yes |  |  |

#### Edge: `catid_holds`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  |  |
| `catid` | `TEXT` | yes |  |  |
| `process_node_id` | `INTEGER` | yes | `0` |  |
| `payload_code` | `TEXT` | yes | `''` |  |
| `reason` | `TEXT` | yes |  |  |
| `qty` | `INTEGER` | yes |  |  |
| `strokes` | `INTEGER` | yes |  |  |
| `first_tick_at` | `TEXT` | yes |  |  |
| `last_tick_at` | `TEXT` | yes |  |  |
| `resolved_at` | `TEXT` |  |  |  |
| `resolution` | `TEXT` | yes | `''` |  |
| `resolved_by` | `TEXT` | yes | `''` |  |
| `last_bin_id` | `INTEGER` | yes | `0` |  |
| `last_declared` | `INTEGER` | yes | `0` |  |
| `last_left_at` | `TEXT` | yes | `''` |  |

- Unique (process_id, catid, process_node_id, payload_code, last_bin_id, last_left_at) where `resolved_at IS NULL`

#### Edge: `changeover_node_tasks`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_changeover_id` | `INTEGER` | yes |  | → `process_changeovers(id) ON DELETE CASCADE` |
| `process_node_id` | `INTEGER` | yes |  | → `process_nodes(id) ON DELETE CASCADE` |
| `from_claim_id` | `INTEGER` |  |  | → `style_node_claims(id) ON DELETE SET NULL` |
| `to_claim_id` | `INTEGER` |  |  | → `style_node_claims(id) ON DELETE SET NULL` |
| `situation` | `TEXT` | yes | `'unchanged'` |  |
| `state` | `TEXT` | yes | `'pending'` |  |
| `next_material_order_id` | `INTEGER` |  |  | → `orders(id) ON DELETE SET NULL` |
| `old_material_release_order_id` | `INTEGER` |  |  | → `orders(id) ON DELETE SET NULL` |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |
| `skip_note` | `TEXT` | yes | `''` |  |

- Unique (process_changeover_id, process_node_id)

#### Edge: `changeover_participants`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `process_changeover_id` | `INTEGER` | yes |  | → `process_changeovers(id) ON DELETE CASCADE` |  |
| `core_node_name` | `TEXT` | yes |  |  |  |
| `process_node_id` | `INTEGER` |  |  | → `process_nodes(id) ON DELETE SET NULL` |  |
| `role` | `TEXT` | yes |  |  | 'task' \| 'indexed_over' |
| `owning_task_id` | `INTEGER` |  |  | → `changeover_node_tasks(id) ON DELETE SET NULL` |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |  |

- Unique (process_changeover_id, core_node_name)

#### Edge: `changeover_station_tasks`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_changeover_id` | `INTEGER` | yes |  | → `process_changeovers(id) ON DELETE CASCADE` |
| `operator_station_id` | `INTEGER` | yes |  | → `operator_stations(id) ON DELETE CASCADE` |
| `state` | `TEXT` | yes | `'waiting'` |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Unique (process_changeover_id, operator_station_id)

#### Edge: `core_loader_payloads`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `loader_key` | `TEXT` | yes |  |  | the owning loader's identity token |
| `payload_code` | `TEXT` | yes |  |  |  |
| `min_stock` | `INTEGER` | yes | `0` |  |  |

- Primary key (loader_key, payload_code)

#### Edge: `core_loader_positions`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `loader_key` | `TEXT` | yes |  |  | the owning loader's identity token |
| `position_node` | `TEXT` | yes |  |  | the position node NAME (a real node) |
| `payload_code` | `TEXT` | yes |  |  |  |
| `home_kind` | `TEXT` | yes | `''` |  | 'home' \| 'buffer' (synced from Core); '' = pre-field Core, fall back to classifying by empty payload |
| `min_stock` | `INTEGER` | yes | `0` |  |  |
| `ordinal` | `INTEGER` | yes | `0` |  | where the operator dragged this window; 0 everywhere = nothing arranged, fall back to a number-aware name sort |

- Primary key (loader_key, position_node)

#### Edge: `core_loaders`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `loader_key` | `TEXT` | yes |  | PK | the loader IDENTITY token ("loader:&lt;id>") |
| `role` | `TEXT` | yes |  |  |  |
| `name` | `TEXT` | yes | `''` |  |  |
| `layout` | `TEXT` | yes | `''` |  |  |
| `replenishment` | `TEXT` | yes | `''` |  |  |
| `outbound_dest` | `TEXT` | yes | `''` |  |  |
| `inbound_source` | `TEXT` | yes | `''` |  |  |
| `config_gen` | `INTEGER` | yes | `0` |  | unread, left at 0: a rolled-back build's loader statements name it (Edge v27) |
| `funnel_windows` | `INTEGER` | yes | `0` |  | 1 = one window at a time; 0 = spread across windows (the default everywhere) |
| `leaves_bare` | `INTEGER` | yes | `0` |  | 1 = the stage-1 half of a two-stage unloader: its CLEAR leaves the cart bare and Core stamps the cart's own marker. Core owns it. |
| `takes_bare` | `INTEGER` | yes | `0` |  | 1 = the stage-2 half of a two-stage unloader, whatever its role: the board draws its windows as an unloader's and nothing asks a carrier into them (Edge v33). Core owns it. |
| `auto_push` | `INTEGER` | yes | `0` |  | 1 = a CLEAR, a PUSH EMPTY or this unloader's own empty-out landing re-pulls its next full. Core owns it (bin_loaders.auto_push). |
| `pulled_directly` | `INTEGER` | yes | `0` |  | 1 = a loader whose finished carts the process pulls straight off its windows: the CLEAR sends the cart nowhere (Edge v10). Core owns it. |
| `board_hides_idle` | `INTEGER` | yes | `0` |  | How the station's board draws (Edge v23). Core owns both (bin_loaders.board_hides_idle, board_shows_all_parts). |
| `board_shows_all_parts` | `INTEGER` | yes | `0` |  |  |
| `no_clear_partials` | `INTEGER` | yes | `0` |  | 1 = a dedicated loader whose Clear partials is off: the board offers no home clear (Edge v46). Core owns it (bin_loaders.no_clear_partials). |
| `synced_at` | `TEXT` | yes | `(datetime('now'))` |  |  |

#### Edge: `counter_snapshots`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `reporting_point_id` | `INTEGER` | yes |  | → `reporting_points(id) ON DELETE CASCADE` |  |
| `count_value` | `INTEGER` | yes |  |  |  |
| `delta` | `INTEGER` | yes | `0` |  |  |
| `anomaly` | `TEXT` |  |  |  |  |
| `operator_confirmed` | `INTEGER` | yes | `0` |  |  |
| `recorded_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `recorded_ms` | `INTEGER` |  |  |  | The production tick feed ships from this table (messaging.TickShipper), so the row carries what the wire event needs, bound by the one INSERT the poll already runs: recorded_ms is the Go clock read just before that INSERT (unix ms; recorded_at above has second granularity), process_id is the reporting point's, and style_id is the ATTRIBUTED style: the one that runs the part the PLC reported for the stroke (catid), or the point's style when the part says nothing (engine attributeTick). NULL on rows written before the shipper existed; those were sent through the outbox. catid is the PLC's confirmed part at the stroke, '' when it was not fresh or named no part. |
| `process_id` | `INTEGER` |  |  |  |  |
| `style_id` | `INTEGER` |  |  |  |  |
| `catid` | `TEXT` | yes | `''` |  |  |

#### Edge: `demand_origins_open`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `episode_key` | `TEXT` | yes |  | PK |  |
| `origin_id` | `TEXT` | yes |  |  |  |
| `revision` | `INTEGER` | yes | `1` |  | revision is monotonic per episode and is what Core's upsert compares. NOT a timestamp: two services cannot agree on one, and the whole point of the guard is that it settles ordering without agreement. |
| `kind` | `TEXT` | yes |  |  | The identity fields. Redundant with episode_key, which encodes most of them, but stored rather than re-parsed: the message is assembled from this row and a parse that can fail has no business on the emit path. |
| `direction` | `TEXT` | yes | `''` |  |  |
| `trigger_kind` | `TEXT` | yes | `''` |  |  |
| `trigger_ref` | `TEXT` | yes | `''` |  |  |
| `process_id` | `TEXT` | yes | `''` |  | THE EDGE PROCESS NAME ("SNF2"), NOT THIS DATABASE'S processes.id. It was INTEGER holding the row id, and Core's demand_origins.process_id was BIGINT to match — while process_styles.process_id and PlantClaimsReport.ProcessID, both already deployed, carry the name. Core therefore held two unjoinable descriptions of one set of processes. Fixed on both sides (Core migration v63) before any plant ran Core's v59.  NOT A FOREIGN KEY, and it never was one usefully: this row must survive long enough to assemble its own close message, and Edge runs with foreign_keys OFF anyway (see store.Open), so the old REFERENCES-shaped INTEGER bought nothing that the name does not. |
| `core_node_name` | `TEXT` | yes | `''` |  |  |
| `payload_code` | `TEXT` | yes | `''` |  |  |
| `opened_total` | `INTEGER` | yes | `0` |  | Stamped once at the falling edge and never recomputed. |
| `reorder_point` | `INTEGER` | yes | `0` |  | The claim's reorder level the decision was made at (NodeClaim.DemandLevel): the Edge's auto-reorder, never Core's threshold. It was named threshold and sent in Core's threshold field until Edge v32. |
| `expected_orders` | `INTEGER` |  |  |  | NULLABLE: a denominator that is UNKNOWABLE is a different state from one that is 1, and both 0 and 1 render as a real ratio somebody would draw a conclusion from. expected_unknown_reason says why, because a NULL with no reason is indistinguishable from a bug. |
| `expected_unknown_reason` | `TEXT` | yes | `''` |  |  |
| `rerequest_count` | `INTEGER` | yes | `0` |  | rerequest_count is operator pushes that JOINED this episode. Six re-requests against one demand is a better signal than six demands of one order each. |
| `discretionary` | `INTEGER` | yes | `0` |  |  |
| `opened_at` | `TEXT` | yes | `(datetime('now'))` |  |  |

#### Edge: `feed_copy`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `feed` | `TEXT` | yes |  | PK |
| `digest` | `TEXT` | yes |  |  |
| `body` | `TEXT` |  |  |  |
| `received_at` | `TEXT` |  |  |  |
| `confirmed_at` | `TEXT` |  |  |  |

#### Edge: `flow_presets`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `name` | `TEXT` | yes |  |  |
| `version` | `INTEGER` | yes | `1` |  |
| `flow_json` | `TEXT` | yes |  |  |
| `created_by` | `TEXT` | yes | `''` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |
| `archived_at` | `TEXT` |  |  |  |

- Unique (process_id, name, version)

#### Edge: `hourly_counts`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `style_id` | `INTEGER` | yes |  | → `styles(id) ON DELETE CASCADE` |
| `bucket_start` | `INTEGER` | yes |  |  |
| `delta` | `INTEGER` | yes | `0` |  |
| `updated_at` | `TEXT` |  | `(datetime('now'))` |  |

- Unique (process_id, style_id, bucket_start)

#### Edge: `hourly_counts_local_legacy`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `style_id` | `INTEGER` | yes |  | → `styles(id) ON DELETE CASCADE` |
| `count_date` | `TEXT` | yes |  |  |
| `hour` | `INTEGER` | yes |  |  |
| `delta` | `INTEGER` | yes | `0` |  |
| `updated_at` | `TEXT` |  | `(datetime('now'))` |  |

- Unique (process_id, style_id, count_date, hour)

#### Edge: `inventory_delta_seq`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `scope_kind` | `TEXT` | yes |  |  |
| `scope_key` | `TEXT` | yes |  |  |
| `epoch` | `INTEGER` | yes | `0` |  |
| `next_seq` | `INTEGER` | yes | `1` |  |
| `net` | `INTEGER` | yes | `0` |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Primary key (scope_kind, scope_key, epoch)

#### Edge: `line_counters`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  |  |
| `label` | `TEXT` | yes | `''` |  |
| `plc_name` | `TEXT` | yes |  |  |
| `tag_name` | `TEXT` | yes |  |  |
| `last_count` | `INTEGER` | yes | `0` |  |
| `last_poll_at` | `TEXT` |  |  |  |
| `warlink_managed` | `INTEGER` | yes | `0` |  |
| `tag_name_2` | `TEXT` | yes | `''` |  |
| `last_count_2` | `INTEGER` | yes | `0` |  |
| `last_poll_at_2` | `TEXT` |  |  |  |
| `warlink_managed_2` | `INTEGER` | yes | `0` |  |

- Unique (plc_name, tag_name)

#### Edge: `node_lineside_bucket`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `node_id` | `INTEGER` | yes |  | → `process_nodes(id) ON DELETE CASCADE` |
| `payload_code` | `TEXT` | yes |  |  |
| `qty` | `INTEGER` | yes | `0` |  |
| `state` | `TEXT` | yes | `'active'` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Check `state IN ('active', 'stranded')`
- Unique (node_id, payload_code, state)

#### Edge: `operator_stations`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `code` | `TEXT` | yes |  |  |
| `name` | `TEXT` | yes |  |  |
| `note` | `TEXT` | yes | `''` |  |
| `area_label` | `TEXT` | yes | `''` |  |
| `sequence` | `INTEGER` | yes | `0` |  |
| `controller_node_id` | `TEXT` | yes | `''` |  |
| `enabled` | `INTEGER` | yes | `1` |  |
| `health_status` | `TEXT` | yes | `'offline'` |  |
| `last_seen_at` | `TEXT` |  |  |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Unique (process_id, code)

#### Edge: `order_history`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `order_id` | `INTEGER` | yes |  | → `orders(id)` |
| `old_status` | `TEXT` | yes |  |  |
| `new_status` | `TEXT` | yes |  |  |
| `detail` | `TEXT` | yes | `''` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |

#### Edge: `orders`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `uuid` | `TEXT` | yes |  | unique |  |
| `order_type` | `TEXT` | yes |  |  |  |
| `status` | `TEXT` | yes | `'pending'` |  |  |
| `process_node_id` | `INTEGER` |  |  | → `process_nodes(id) ON DELETE SET NULL` |  |
| `retrieve_empty` | `INTEGER` | yes | `1` |  |  |
| `quantity` | `INTEGER` | yes | `0` |  |  |
| `delivery_node` | `TEXT` | yes | `''` |  | delivery_node: authoritative for SIMPLE orders (one bin, one destination).  For COMPLEX orders it is effectively a DISPLAY value and nothing correctness-critical reads it any more. A complex leg has many dropoffs, so one destination field cannot say where its bin came to rest; auto-confirm legs store '' outright; and the swap legs that do store something name the node the ROBOT ends at, not the node the BIN ends at. Every decision that used to consult it — the delivered gate, the supply/evac classifier, the sim operator's confirm scope — now reads steps_json (see swap_leg_role.go).  It is NOT the same value as Core's orders.delivery_node. Edge does not send one: ComplexOrderRequest has no such field, and Core derives its own from the steps (extractEndpoints). Two columns, one name, independent values. |
| `staging_node` | `TEXT` | yes | `''` |  |  |
| `source_node` | `TEXT` | yes | `''` |  |  |
| `load_type` | `TEXT` | yes | `''` |  | load_type: dormant. Nothing on either module read it, and its writers went (2026-10-25); kept at its default so a rolled-back Edge's INSERT runs. |
| `waybill_id` | `TEXT` |  |  |  |  |
| `external_ref` | `TEXT` |  |  |  |  |
| `final_count` | `INTEGER` |  |  |  |  |
| `count_confirmed` | `INTEGER` | yes | `0` |  |  |
| `eta` | `TEXT` |  |  |  |  |
| `auto_confirm` | `INTEGER` | yes | `0` |  |  |
| `steps_json` | `TEXT` | yes | `''` |  |  |
| `staged_expire_at` | `TEXT` |  |  |  |  |
| `bin_id` | `INTEGER` |  |  |  |  |
| `payload_code` | `TEXT` | yes | `''` |  |  |
| `sibling_order_id` | `INTEGER` |  |  | → `orders(id) ON DELETE SET NULL` |  |
| `queue_reason` | `TEXT` | yes | `''` |  |  |
| `queue_code` | `TEXT` | yes | `''` |  |  |
| `authored_by` | `TEXT` | yes | `'edge'` |  | authored_by: who decided this order should exist. 'edge' (the default, and what every existing row is) means this Edge created it and sent it up; 'core' means Core created it and pushed the row down. Nothing branches on it: it labels the board and it is what a projected-row test asserts against. Deliberately cheap to stop rendering. |
| `fault_since` | `TEXT` | yes | `''` |  | The fault clock (v36). Set only while the order is faulted; the handler clears them on any other status, derived from the status rather than from a pushed empty value — see messaging/edge_handler.go and the queue_reason incident it documents. fault_notice_after_s is Core's replan/fault threshold in seconds as it stood when the fault was pushed; 0 means an older Core that did not send one. |
| `fault_deadline` | `TEXT` | yes | `''` |  |  |
| `fault_notice_after_s` | `INTEGER` | yes | `0` |  |  |
| `fault_ref` | `TEXT` | yes | `''` |  | The fleet's reason as protocol.TermRef JSON. Stored as a reference, not a rendered sentence: the sentence changes when the clock crosses the threshold, and the board re-renders it without another push. |
| `departed_at` | `TEXT` |  |  |  | departed_at (v39): when this leg stopped being its cell's business — the instant the fleet confirmed the last step of steps_json whose node is in the claim's cell set. NULL is "still working the cell", which is what every row starts as and what a leg whose last cell step is its FINAL step stays as forever (terminal covers that shape; nothing stamps it).  Stamped once and never cleared: it records a physical event, and Core re-fires already-FINISHED blocks after a restart, so a second write would have to be a no-op anyway. NULL, not '', because the two admission guards read it as a three-state answer alongside a status — see MarkDeparted. |
| `cell_left_at` | `TEXT` |  |  |  | cell_left_at (v40): when the fleet confirmed the last step of steps_json whose node is in the claim's cell set — the robot has left the cell's NODES. This is what departed_at alone used to mean.  departed_at is now the CONJUNCTION: the robot has left AND the leg's own placement at the line position has been recorded. The two separate for single_robot, which places at step 7 and lifts the spent carrier off OutboundStaging at step 8 — so it leaves the cell's nodes one step after it has placed and before the placement is on the books.  Its one reader is settleCellPlacement (engine/leg_departure.go), which completes the departure when the placement lands second. NULL means the leg has not left the cell's nodes. |
| `pending_intent` | `TEXT` | yes | `''` |  | pending_intent (Edge v4): the future action this order armed when it was dispatched, as a JSON envelope (engine/order_intent.go) — today only a pull-from-market's auto-clear on delivery (a row an older build armed for a clear-loader-home is read as an unknown kind and dropped at its take). '' = nothing armed. Taken exactly once by the firing site. |
| `station_wait` | `INTEGER` |  |  |  | station_wait / wait_kind (Edge v7): where Core last staged this order — the station wait's ordinal among the plan's station waits (NULL at a lane wait, and until a Core that reports it), and the wait's kind. |
| `wait_kind` | `TEXT` | yes | `''` |  |  |
| `release_facts` | `TEXT` |  |  |  | release_facts (Edge v8): the leg's static release facts, computed from its steps when this Edge created it (release.Facts, JSON): the nodes it leaves a bin at, the bin its release lifts, the nodes a release lets a bin cross, its role at its process node. NULL for an order with no steps this Edge authored, and for every row older than v8. |
| `release_intent` | `TEXT` | yes | `''` |  | release_intent / release_held (Edge v9): the act's promise to this leg for one station wait (release.Intent, JSON; '' = none), and the last hold or rejection sentence the board's chip shows. |
| `release_held` | `TEXT` | yes | `''` |  |  |
| `want` | `TEXT` | yes | `''` |  | want (Edge v30): the height an empty is wanted at, as this Edge stated it on the order's request (protocol.Height's spelling; '' is none). SEND BACK knows its pull by it; a projection never writes it. |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `payload_desc` | `TEXT` | yes | `''` |  |  |
| `origin_id` | `TEXT` | yes | `''` |  |  |
| `origin_class` | `TEXT` | yes | `''` |  |  |

#### Edge: `outbox`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `topic` | `TEXT` | yes |  |  |
| `payload` | `BLOB` | yes |  |  |
| `msg_type` | `TEXT` | yes | `''` |  |
| `retries` | `INTEGER` | yes | `0` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |
| `sent_at` | `TEXT` |  |  |  |

#### Edge: `payload_catalog`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK |  |
| `name` | `TEXT` | yes |  |  |  |
| `code` | `TEXT` | yes | `''` |  |  |
| `description` | `TEXT` | yes | `''` |  |  |
| `uop_capacity` | `INTEGER` | yes | `0` |  |  |
| `catid` | `TEXT` | yes | `''` |  | (cycle_seconds, an Edge-local cycle time with no reader, was dropped by Edge v3, 2026-09-28.) |
| `manifest` | `TEXT` | yes | `'[]'` |  |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |  |

#### Edge: `plc_catid_observations`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `plc_name` | `TEXT` | yes | `''` |  |
| `catid` | `TEXT` | yes |  |  |
| `first_seen` | `TEXT` | yes | `(datetime('now'))` |  |
| `last_seen` | `TEXT` | yes | `(datetime('now'))` |  |
| `observations` | `INTEGER` | yes | `1` |  |

- Primary key (process_id, catid)

#### Edge: `process_changeovers`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |  |
| `from_style_id` | `INTEGER` |  |  | → `styles(id) ON DELETE SET NULL` |  |
| `to_style_id` | `INTEGER` | yes |  | → `styles(id) ON DELETE CASCADE` |  |
| `state` | `TEXT` | yes | `'planned'` |  |  |
| `called_by` | `TEXT` | yes | `''` |  |  |
| `notes` | `TEXT` | yes | `''` |  |  |
| `started_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `completed_at` | `TEXT` |  |  |  |  |
| `triggered_by` | `TEXT` | yes | `''` |  |  |
| `verify_live_catid` | `TEXT` | yes | `''` |  |  |
| `start_catid` | `TEXT` | yes | `''` |  | The PLC's confirmed CATID when the changeover started, and when its post-cutover watch closed: the evidence a CATID pin is suggested from. '' when the monitor had no identity at that moment. |
| `end_catid` | `TEXT` | yes | `''` |  |  |
| `origin_id` | `TEXT` | yes | `''` |  | origin_id is this changeover's demand episode. Empty until minted.  It goes on THIS row rather than in demand_origins_open because this row already has exactly the episode's lifetime: one changeover is one episode (to_style_id is written only at INSERT, nothing re-targets a row, and cancel-and-redirect cancels this one and inserts a fresh one — a new row and a new episode). Restart-durable for free. |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |  |

#### Edge: `process_groups`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `name` | `TEXT` | yes |  | unique |
| `description` | `TEXT` | yes | `''` |  |
| `sort_order` | `INTEGER` | yes | `0` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |

#### Edge: `process_node_runtime_states`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `process_node_id` | `INTEGER` | yes |  | unique, → `process_nodes(id) ON DELETE CASCADE` |  |
| `active_claim_id` | `INTEGER` |  |  | → `style_node_claims(id) ON DELETE SET NULL` | active_claim_id NAMES A STYLE'S CLAIM, NOT A CARRIER, and the name is the trap. It answers "which style-claim is this node running" — configuration and intent — and eighteen paths write it, most of them from the process's active style. It is not "the claim the bin standing here was stocked for": a changeover moves it while a carrier stays put, and reading it for identity is the defect class this table's lineside_* columns exist to end. What is standing here is lineside_payload_code below.  The surviving readers all want the configuration answer: the changeover readiness check, the switch-node skip arm, the release-time capacity reset, the cancel re-bind, and the clear-shaped writes that thread it back unchanged. The claim advance at applyChangeoverRelease / applyStagedDelivery is what keeps that answer current during a changeover.  ON DELETE SET NULL, so a deleted claim leaves the pointer NULL rather than dangling, and every reader here treats NULL as "no opinion" and fails open. That is the designed degradation and it is also why a config edit can silently widen behaviour — HK SMN_01 is the live instance. A reader that must not fail open should ask the running style through ResolveNodeClaim instead of dereferencing this. |
| `active_bin_id` | `INTEGER` |  |  |  |  |
| `active_bin_epoch` | `INTEGER` | yes | `0` |  | active_bin_epoch mirrors Core's bins.delta_epoch for the bin currently active at this slot. Edge stamps every outgoing BinUOPDelta with the value so Core's epoch-aware dedup accepts the delta. Survives Edge restart so post-restart ticks don't emit at epoch=0 against a bin already at epoch>=1.  Four writers, and "FetchNodeBins refresh" was not one of them: the OrderDelivered envelope, Core's LoadBin/clear/count replies, the BinAtLineside re-bind after a changeover cancel, and Core's BinEpochRefresh push. Every FetchNodeBins call site discards the epoch it is handed. |
| `last_bin_id` | `INTEGER` |  |  |  | last_bin_id / last_bin_epoch are the bin that last left this slot and the stamp it left with, written by every statement that moves active_bin_id. An empty slot's active_bin_epoch belongs to no bound bin; these say whose it was, so a late correction for a carrier that has left cannot rebind it under an ended generation. |
| `last_bin_epoch` | `INTEGER` | yes | `0` |  |  |
| `released_bin_id` | `INTEGER` |  |  |  | released_bin_id / released_count / released_at are the carrier this seat's last release declared, the count it declared and when (the engine's clock), written by the release's count statement. A stroke logged with no cart of its part records them as the last cart off (catid_holds.last_*). |
| `released_count` | `INTEGER` | yes | `0` |  |  |
| `released_at` | `TEXT` |  |  |  |  |
| `adj_bin_id` | `INTEGER` |  |  |  | adj_bin_id / adj_bin_epoch / adj_as_of_seq are the carrier, generation and AsOfSeq of the last fenced count this slot took (the record-count fence). A count for the same carrier and generation taken earlier in the station's stream, delivered later, is refused. |
| `adj_bin_epoch` | `INTEGER` | yes | `0` |  |  |
| `adj_as_of_seq` | `INTEGER` | yes | `0` |  |  |
| `lineside_payload_code` | `TEXT` | yes | `''` |  | lineside_payload_code is what the carrier standing here actually is, as opposed to what this node's style says should be here. Core sends it on OrderDelivered, off the same bin row it already reads for the count and the epoch; this side has no bins table and cannot look it up. |
| `lineside_payload_known` | `INTEGER` | yes | `0` |  | lineside_payload_known is the difference between "this carrier is empty" and "nobody could tell me what this carrier is". Both spell themselves '' in the column above, and every reader that had to guess guessed the same way: fall back to the claim, i.e. to the requested identity, which is the read this column pair exists to end. Ask this flag, not the emptiness of the string. |
| `lineside_source` | `TEXT` | yes | `''` |  | lineside_source names who asserted the identity: a delivery envelope, a person at the window, or the departure that took it away. Not a ranking -- an automatic source is not more trustworthy than a person here -- it is so a later reader can tell which question was answered. |
| `lineside_at` | `TEXT` | yes | `''` |  | lineside_at is when the identity was established. updated_at cannot answer it: every count tick moves updated_at, so a payload recorded once and ticked for an hour reads as an hour old by that clock and is not. The identity's staleness is a different question from the count's. |
| `remaining_uop_cached` | `INTEGER` | yes | `0` |  |  |
| `pending_uop_delta` | `INTEGER` | yes | `0` |  | pending_uop_delta holds tick counts that arrived while no bin was bound (the pickup->delivery gap); the next tick with a bound bin of the held part (pending_uop_payload) applies current+pending and resets it. Durable across restart. |
| `active_order_id` | `INTEGER` |  |  | → `orders(id) ON DELETE SET NULL` |  |
| `staged_order_id` | `INTEGER` |  |  | → `orders(id) ON DELETE SET NULL` |  |
| `active_pull` | `INTEGER` | yes | `1` |  |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `keep_staged_resumed_at` | `TEXT` | yes | `''` |  | keep_staged_resumed_at is when an operator last resumed this line's keep-staged keeper from the board after a cancel paused it ('' = never). Its own column because the claim's updated_at is the setup card's "flow saved" date, and a resume is not a change to the flow. Last, where the v17 ALTER puts it on an existing database. |
| `pending_uop_payload` | `TEXT` | yes | `''` |  | pending_uop_payload is the part of the ticks held in pending_uop_delta, written by the hold's own statement and cleared with it. The hold replays only onto a bound carrier of that part; a different part's carrier takes none. '' is a hold from before the column, which replays as it always did. |
| `released_empty` | `INTEGER` | yes | `0` |  | released_empty: the bound carrier was released as empty (RELEASE EMPTY, an underpack, a release declaring 0). It is not charged afterwards: a consume tick drains the pile, then is held for the next carrier of the part. Set by the release's count write, cleared when the carrier leaves or another carrier binds. After the v17 column, where the v19 ALTERs put both. |
| `placed_seq` | `INTEGER` |  |  |  | placed_seq: the placement stamp of the landing this seat last bound (Core's bin_placement_seq). The landing's bind refuses one not newer, so a redelivered or overtaken landing binds nothing. Kept by every leave; a person's bind leaves it as it is. NULL until the first landing. After released_empty, where the v20 ALTER puts it. |
| `hold_stopped` | `INTEGER` | yes | `0` |  | hold_stopped: a produce seat's pile would have passed one pack and was thrown away whole; the seat sets nothing aside until another cart binds here or a person enters a count (a Core lifecycle message never clears it). After placed_seq, where the v49 ALTER puts it. |

#### Edge: `process_nodes`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |  |
| `operator_station_id` | `INTEGER` |  |  | → `operator_stations(id) ON DELETE SET NULL` |  |
| `core_node_name` | `TEXT` | yes | `''` |  |  |
| `code` | `TEXT` | yes |  |  |  |
| `name` | `TEXT` | yes |  |  |  |
| `sequence` | `INTEGER` | yes | `0` |  |  |
| `enabled` | `INTEGER` | yes | `1` |  |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `deleted_at` | `TEXT` |  |  |  | Soft delete, for the same reason styles has one. Deleting a process_node CASCADEs into process_node_runtime_states and changeover_node_tasks — and changeover_node_tasks.process_node_id is NOT NULL, so per-node changeover detail is destroyed outright with no SET NULL option. 118 such rows already exist on the Springfield edge whose node is gone while all 118 parent changeovers survive: readable history with an unreadable middle. The uniqueness constraint moved to a partial index below so a re-created node can reuse a retired code. |
| `curtain_enabled` | `INTEGER` | yes | `0` |  | The FG light-curtain release interlock, per node: an operator button mutes the curtain at this spot through a BOOL PLC tag, and with the interlock on, a release that sends a robot in here waits for the tag to read curtain_safe_value (a direct WarLink read at the click, fail-closed). curtain_safe_value is NULL until someone chooses it: which value means "released" is a site fact, and a default inverts the gate wherever it guesses wrong. An enabled row with NULL refuses every release, and the setter refuses to write one. |
| `curtain_plc_name` | `TEXT` | yes | `''` |  |  |
| `curtain_tag_name` | `TEXT` | yes | `''` |  |  |
| `curtain_safe_value` | `INTEGER` |  |  |  |  |
| `line_counter_id` | `INTEGER` |  |  |  | The station counter this position is counted at (line_counters.id); NULL = counted by the process's production counter. No FK (the edge runs with foreign keys off): every read LEFT JOINs line_counters, so a binding to a deleted counter reads as unbound. |

- Unique (process_id, code) where `deleted_at IS NULL`
- Unique (process_id, core_node_name) where `core_node_name <> '' AND deleted_at IS NULL`

#### Edge: `process_payloads`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `payload_code` | `TEXT` | yes |  |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Unique (process_id, payload_code)

#### Edge: `process_routing_nodes`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` | yes |  | → `processes(id) ON DELETE CASCADE` |
| `core_node_name` | `TEXT` | yes |  |  |
| `role` | `TEXT` | yes |  |  |
| `label` | `TEXT` | yes | `''` |  |
| `sequence` | `INTEGER` | yes | `0` |  |
| `enabled` | `INTEGER` | yes | `1` |  |
| `origin` | `TEXT` | yes | `'engineer'` |  |
| `called_by` | `TEXT` | yes | `''` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Check `role IN ('source','staging','destination')`
- Check `origin IN ('engineer','backfill')`
- Unique (process_id, core_node_name, role)

#### Edge: `processes`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `name` | `TEXT` | yes |  | unique |  |
| `description` | `TEXT` | yes | `''` |  |  |
| `active_style_id` | `INTEGER` |  |  | → `styles(id) ON DELETE SET NULL` |  |
| `target_style_id` | `INTEGER` |  |  | → `styles(id) ON DELETE SET NULL` |  |
| `counter_plc_name` | `TEXT` | yes | `''` |  |  |
| `counter_tag_name` | `TEXT` | yes | `''` |  |  |
| `counter_enabled` | `INTEGER` | yes | `0` |  |  |
| `identity_tag_name` | `TEXT` | yes | `''` |  | The part-identity (CATID) tag: '' derives CATID_0N from the counter tag; identity_none = 1 is a process with none. NOT NULL, so a settings save that sends every field as typed cannot switch the monitor off. identity_warlink_managed: shingo enabled the tag's WarLink publishing. |
| `identity_none` | `INTEGER` | yes | `0` |  |  |
| `identity_warlink_managed` | `INTEGER` | yes | `0` |  |  |
| `changeover_auto_arm` | `TEXT` | yes | `'auto'` |  |  |
| `group_id` | `INTEGER` |  |  | → `process_groups(id) ON DELETE SET NULL` |  |
| `flow_composer_enabled` | `INTEGER` | yes | `0` |  | flow_composer_enabled gates the HMI flow composer for this process. OFF until the engineer has reviewed the routing set derived into process_routing_nodes below; the backfill re-derives only while it is off, so a reviewed set is never silently re-seeded. |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |  |

#### Edge: `production_tick_cursor`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK |
| `last_id` | `INTEGER` | yes |  |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Check `id = 1`

#### Edge: `release_paperwork`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `order_id` | `INTEGER` | yes |  |  |
| `kind` | `TEXT` | yes |  |  |
| `applied_json` | `TEXT` | yes | `'{}'` |  |
| `updated_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Primary key (order_id, kind)

#### Edge: `reporting_points`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `style_id` | `INTEGER` | yes |  | → `styles(id) ON DELETE CASCADE` |
| `plc_name` | `TEXT` | yes |  |  |
| `tag_name` | `TEXT` | yes |  |  |
| `last_count` | `INTEGER` | yes | `0` |  |
| `last_poll_at` | `TEXT` |  |  |  |
| `enabled` | `INTEGER` | yes | `1` |  |
| `warlink_managed` | `INTEGER` | yes | `0` |  |

- Unique (plc_name, tag_name)

#### Edge: `scene_geometry_edges`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `from_name` | `TEXT` | yes |  |  |
| `to_name` | `TEXT` | yes |  |  |
| `from_x` | `REAL` | yes |  |  |
| `from_y` | `REAL` | yes |  |  |
| `to_x` | `REAL` | yes |  |  |
| `to_y` | `REAL` | yes |  |  |
| `ctrl1_x` | `REAL` |  |  |  |
| `ctrl1_y` | `REAL` |  |  |  |
| `ctrl2_x` | `REAL` |  |  |  |
| `ctrl2_y` | `REAL` |  |  |  |

- Primary key (from_name, to_name)

#### Edge: `scene_geometry_meta`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK |
| `revision` | `TEXT` | yes | `''` |  |
| `synced_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Check `id = 1`

#### Edge: `scene_geometry_points`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `instance_name` | `TEXT` | yes |  | PK |
| `class_name` | `TEXT` | yes | `''` |  |
| `pos_x` | `REAL` | yes |  |  |
| `pos_y` | `REAL` | yes |  |  |
| `dir` | `REAL` | yes | `0` |  |

#### Edge: `schema_migrations`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `version` | `INTEGER` | yes |  | PK |
| `applied_at` | `TEXT` | yes | `CURRENT_TIMESTAMP` |  |

#### Edge: `shifts`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `name` | `TEXT` | yes | `''` |  |
| `shift_number` | `INTEGER` | yes |  | unique |
| `start_time` | `TEXT` | yes |  |  |
| `end_time` | `TEXT` | yes |  |  |

#### Edge: `sourcing_state`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `process_id` | `TEXT` | yes |  |  |
| `style_id` | `TEXT` | yes |  |  |
| `status` | `TEXT` | yes | `'green'` |  |
| `missing` | `TEXT` | yes | `'[]'` |  |
| `absent` | `TEXT` | yes | `'[]'` |  |
| `at_risk` | `TEXT` | yes | `'[]'` |  |
| `reason` | `TEXT` | yes | `''` |  |
| `computed_at` | `TEXT` | yes | `''` |  |
| `synced_at` | `TEXT` | yes | `(datetime('now'))` |  |

- Primary key (process_id, style_id)

#### Edge: `style_node_claims`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |  |
| `style_id` | `INTEGER` | yes |  | → `styles(id) ON DELETE CASCADE` |  |
| `core_node_name` | `TEXT` | yes |  |  |  |
| `role` | `TEXT` | yes | `'consume'` |  |  |
| `swap_mode` | `TEXT` | yes |  |  |  |
| `payload_code` | `TEXT` | yes | `''` |  |  |
| `uop_capacity` | `INTEGER` | yes | `0` |  | DEAD. Capacity is Core's fact, resolved from payload_catalog on every claim read (capacity.SQL) and keyed on payload_code above. Nothing reads or writes this column; it holds whatever it held when the copies stopped, and it goes on the next rebuildStyleNodeClaims(). It is not dropped here because a column rebuild is its own change with its own risk, and because the old numbers are the only record of how far the copies had drifted. |
| `reorder_point` | `INTEGER` | yes | `0` |  |  |
| `auto_reorder` | `INTEGER` | yes | `0` |  | Cell auto-reorder is opt-IN: a claim must not arm itself. Was DEFAULT 1, which made "new claim" mean "count-driven order creation is live" before anyone chose it. Every INSERT in the codebase names this column explicitly (processes/claims.go UpsertClaim, processes/styles.go cloneClaimColumns, seeddev/seed_edge.go), so the default is reachable only by a future hand-written INSERT that omits it — which is exactly the case that should land off. Fresh databases only: this is CREATE TABLE IF NOT EXISTS and there is no ALTER for this column, so an existing plant DB keeps its current table definition and its current per-claim values either way. |
| `inbound_staging` | `TEXT` | yes | `''` |  |  |
| `outbound_staging` | `TEXT` | yes | `''` |  |  |
| `inbound_source` | `TEXT` | yes | `''` |  |  |
| `outbound_destination` | `TEXT` | yes | `''` |  |  |
| `containment_destination` | `TEXT` | yes | `''` |  | Quality containment: where a bin of this claim's payload goes when containment is active for it (a quality alert on the payload, or an operator's per-bin quality hold), INSTEAD of the ordinary FG outbound_destination. Same vocabulary as outbound_destination (a node name); blank = no containment route configured = the divert is inert for this claim, which is the default and the common case. Resolved Core-side at dispatch (the single-authority dropoff rule) against Core's authoritative containment state; the Edge never enforces it. |
| `partial_destination` | `TEXT` | yes | `''` |  | partial_destination: where this claim's carrier goes when it leaves the line unfinished (a node or a group), to be called back and finished. Blank = it goes where a full goes. Core judges "unfinished" from its own count and chooses the drop; the Edge only authors the route. |
| `curtain_required` | `INTEGER` | yes | `0` |  | The light-curtain release interlock's style tie (v19): whether THIS style's releases at THIS node wait for the node's curtain. The claim is the (style, node) pair, and the rule the plant chose is THE BIN DICTATES: a full bin leaving carries the claim that filled it, and that claim's flag - not the node's blanket enable, not the style the line is becoming during a changeover - decides whether the release gates on the tag. An empty carrier carries no claim, so empty placements never gate (they never pass the curtain; an empty that does is the operator's fault, not the robot's). |
| `strict_pack` | `INTEGER` | yes | `0` |  | strict_pack (v26): a produce claim's bound bin is charged only up to its pack (the catalog's uop_capacity for the claim's part); the ticks past it are held on the seat for the next bin of the part. 0 = uncapped, today's count. |
| `pack_tolerance_pct` | `INTEGER` | yes | `0` |  | pack_tolerance_pct (v34): a strict claim's miscount tolerance, a percent of the pack (0-20). A station's RELEASE of a bin within it of the pack declares the bin full; further short ships the real count. 0 = no tolerance: the real count always ships. |
| `allowed_payload_codes` | `TEXT` | yes | `''` |  |  |
| `auto_request_payload` | `TEXT` | yes | `''` |  |  |
| `keep_staged_node` | `TEXT` | yes | `''` |  | The keep-staged node: one more node this line's full or empty comes from instead of inbound_source, with a spare kept standing on it. Blank is off. Replaced the keep_staged flag, which armed a spot on inbound_staging and so gave that column a second meaning. |
| `paired_core_node` | `TEXT` | yes | `''` |  |  |
| `auto_confirm` | `INTEGER` | yes | `0` |  |  |
| `sequence` | `INTEGER` | yes | `0` |  |  |
| `lineside_soft_threshold` | `INTEGER` | yes | `0` |  |  |
| `reuse_compatible_bins` | `INTEGER` | yes | `0` |  | DEAD AS A SWITCH. It armed the no-swap shortcut for a press-index cell whose next style makes the same part; that shortcut is baked in (ApplyReuseCompatibleBinsShortcut, 2026-09-09) and reads this column no more. Kept, and flowspec marks it Unused rather than Forbidden, so the seven Hopkinsville rows that carry it keep their value instead of being cleared by the next save. |
| `changeover_evac_nodes` | `TEXT` | yes | `''` |  | Which core NODES hold bins that block the tooling change, as a JSON array of node names ("PLN_001"/"PLN_002"). Same shape and same reasoning as allowed_payload_codes: a small set on one row rather than a child table, because an index-paired node has no claim row of its own to hang it on. Empty = nothing marked = today's behaviour. |
| `changeover_carryover_disposition` | `TEXT` | yes | `'replace'` |  | What happens to a marked position's bin when its part CARRIES OVER — the same payload on that position in both styles. "replace" (the default) clears it like any other marked position and brings a fresh carrier through staging; "keep_lineside" leaves the bin where it is, because that part does not have to move for the setup; "outbound_staging" walks the SAME bin to the cell's outbound staging spot to clear the floor and brings it back on the tooling-done release. Never consulted when the payloads differ — the bin has to change anyway. |
| `refill_by` | `TEXT` | yes | `'clearing_robot'` |  | Which robot of a press-index pair brings the back position its next carrier: clearing_robot (Robot 1, R1 evacuates and refills) or index_robot (Robot 2, R2 indexes and refills). Describes the cell's hardware, so UpsertClaim warns when two styles on one press disagree. |
| `key_route` | `TEXT` | yes | `''` |  | SEER robot-SELECTION hints, carried through to the fleet request. key_route is a JSON array of map points to prefer passing through, IN ORDER; key_task is 'load'/'unload'. Both empty on every claim until one is configured, and empty means the fleet picks freely. A point that does not resolve terminates the robot's waybill on issue, which is why ValidateNodeClaim checks each one against Core's synced node list. |
| `key_task` | `TEXT` | yes | `''` |  |  |
| `auto_push` | `INTEGER` | yes | `0` |  |  |
| `below_reorder_since` | `TEXT` |  |  |  | below_reorder_since is the FALLING EDGE of this claim's level: the instant remaining UOP first went at-or-below reorder_point, cleared when it recovers above reorder_point + margin. NULL means "not below".  It is the durable half of the demand episode. The level is evaluated once a period by the demand reconciler's level sweep (sweepCellLevels), and the timestamp is written only ON TRANSITION; this column exists because Edge restarts (systemctl restart shingoedge) more often than anything else in the system, and an in-memory-only edge means a restart mid-episode loses it, the next pass mints a duplicate, and the first never closes.  On the CLAIM rather than the episode because it is a per-claim level observation and the claim is the row the predicate already reads. The episode itself is keyed per PROCESS and lives in demand_origins_open. |
| `source` | `TEXT` | yes | `'admin'` |  | ── ATTRIBUTION ────────────────────────────────────────────────────── Every claim row says who wrote it and from where, because the HMI flow composer makes the claim table fully open: an operator on the floor writes the same rows an engineer writes on the desktop. All of these are SERVER-STAMPED — the client never sends them.  source is the writer: 'admin' (the desktop claim editor), 'hmi' (the station's flow composer), 'generated' (the retired Generate variants), 'cloned' (CloneStyle). DEFAULT 'admin' is right for every row that already exists: the desktop was the only writer there was. |
| `called_by` | `TEXT` | yes | `''` |  | called_by is the session user (admin) or the station (hmi); '' on rows that predate attribution and on generated/cloned rows made by a caller with no session. |
| `updated_at` | `TEXT` |  |  |  | updated_at is set on every upsert. NULL means never touched since attribution landed; it cannot carry a datetime('now') default because an ALTER cannot add a non-constant default to a populated table. |
| `retired_at` | `TEXT` |  |  |  | retired_at replaces DELETE for a claim that changeover history still references (changeover_node_tasks.from_claim_id / to_claim_id): the row stays so the history label never renders blank, and list reads skip it. Re-adding the same (style, node) claim revives the row. |
| `source_preset_id` | `INTEGER` |  |  |  | source_preset_id / _version are PROVENANCE: which flow_presets row and version this claim was expanded from, when it was. Never a drift oracle — drift is computed from the rows themselves. |
| `source_preset_version` | `INTEGER` |  |  |  |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |  |
| `staging_node` | `TEXT` | yes | `''` |  |  |
| `release_node` | `TEXT` | yes | `''` |  |  |
| `inbound_source_node` | `TEXT` | yes | `''` |  |  |
| `inbound_source_node_group` | `TEXT` | yes | `''` |  |  |
| `outbound_source_node` | `TEXT` | yes | `''` |  |  |
| `outbound_source_node_group` | `TEXT` | yes | `''` |  |  |
| `outbound_source` | `TEXT` | yes | `''` |  |  |
| `mode` | `TEXT` | yes | `'loader'` |  |  |
| `second_paired_core_node` | `TEXT` | yes | `''` |  |  |

- Unique (style_id, core_node_name)

#### Edge: `style_node_claims_quarantine`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INT` |  |  |  |
| `style_id` | `INT` |  |  |  |
| `core_node_name` | `TEXT` |  |  |  |
| `role` | `TEXT` |  |  |  |
| `swap_mode` | `TEXT` |  |  |  |
| `payload_code` | `TEXT` |  |  |  |
| `uop_capacity` | `INT` |  |  |  |
| `reorder_point` | `INT` |  |  |  |
| `auto_reorder` | `INT` |  |  |  |
| `inbound_staging` | `TEXT` |  |  |  |
| `outbound_staging` | `TEXT` |  |  |  |
| `inbound_source` | `TEXT` |  |  |  |
| `outbound_destination` | `TEXT` |  |  |  |
| `containment_destination` | `TEXT` |  |  |  |
| `partial_destination` | `TEXT` |  |  |  |
| `curtain_required` | `INT` |  |  |  |
| `strict_pack` | `INT` |  |  |  |
| `pack_tolerance_pct` | `INT` |  |  |  |
| `allowed_payload_codes` | `TEXT` |  |  |  |
| `auto_request_payload` | `TEXT` |  |  |  |
| `keep_staged_node` | `TEXT` |  |  |  |
| `paired_core_node` | `TEXT` |  |  |  |
| `auto_confirm` | `INT` |  |  |  |
| `sequence` | `INT` |  |  |  |
| `lineside_soft_threshold` | `INT` |  |  |  |
| `reuse_compatible_bins` | `INT` |  |  |  |
| `changeover_evac_nodes` | `TEXT` |  |  |  |
| `changeover_carryover_disposition` | `TEXT` |  |  |  |
| `refill_by` | `TEXT` |  |  |  |
| `key_route` | `TEXT` |  |  |  |
| `key_task` | `TEXT` |  |  |  |
| `auto_push` | `INT` |  |  |  |
| `below_reorder_since` | `TEXT` |  |  |  |
| `source` | `TEXT` |  |  |  |
| `called_by` | `TEXT` |  |  |  |
| `updated_at` | `TEXT` |  |  |  |
| `retired_at` | `TEXT` |  |  |  |
| `source_preset_id` | `INT` |  |  |  |
| `source_preset_version` | `INT` |  |  |  |
| `created_at` | `TEXT` |  |  |  |
| `staging_node` | `TEXT` |  |  |  |
| `release_node` | `TEXT` |  |  |  |
| `inbound_source_node` | `TEXT` |  |  |  |
| `inbound_source_node_group` | `TEXT` |  |  |  |
| `outbound_source_node` | `TEXT` |  |  |  |
| `outbound_source_node_group` | `TEXT` |  |  |  |
| `outbound_source` | `TEXT` |  |  |  |
| `mode` | `TEXT` |  |  |  |
| `second_paired_core_node` | `TEXT` |  |  |  |
| `quarantined_at` | `TEXT` | yes | `''` |  |
| `quarantined_reason` | `TEXT` | yes | `''` |  |
| `quarantined_sync_loader_count` | `INTEGER` | yes | `0` |  |
| `quarantined_sync_loader_keys` | `TEXT` | yes | `''` |  |

#### Edge: `styles`

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `INTEGER` | yes |  | PK autoincrement |
| `process_id` | `INTEGER` |  |  | → `processes(id) ON DELETE CASCADE` |
| `name` | `TEXT` | yes |  |  |
| `description` | `TEXT` | yes | `''` |  |
| `expected_catid` | `TEXT` | yes | `''` |  |
| `created_at` | `TEXT` | yes | `(datetime('now'))` |  |
| `deleted_at` | `TEXT` |  |  |  |

- Unique (process_id, name) where `deleted_at IS NULL`

#### Edge: `supply_refusals_open`

| Column | Type | Not null | Default | Key | Comment |
|---|---|---|---|---|---|
| `loader_node` | `TEXT` | yes |  |  |  |
| `payload_code` | `TEXT` | yes |  |  |  |
| `refused_at` | `TEXT` | yes | `(datetime('now'))` |  | refused_* is the supplier's half. |
| `refused_by` | `TEXT` | yes | `''` |  | refused_by is STATION-level, not person-level: the loader board carries no operator identity and calledBy falls back to the station name. Recorded as a known limitation rather than dressed up as attribution it cannot make. |
| `ack_at` | `TEXT` |  |  |  | ack_* is the customer's half, and ack_at IS NULL is a REAL, QUERYABLE STATE: told, not answered. Making WAIT the absence of an action would collapse "the operator chose to keep waiting" and "nobody has looked at the screen" into one row, and the second of those is the original complaint this whole project started from. |
| `ack_choice` | `TEXT` | yes | `''` |  | '' \| 'wait' \| 'changeover' |
| `ack_process_id` | `TEXT` | yes | `''` |  | ack_process_id is the process NAME ("SNF2") of the cell that ANSWERED — matching the demand grain, which keys on the name and not a row id.  Note carefully: this is who answered, NOT who was told. Resolving the addressee (which cells a loader is currently supplying) is the station view's to work out per build (its served rows), not this table's. There is deliberately no column for the addressee until something can write one. |

- Primary key (loader_node, payload_code)

<!-- /generated -->
