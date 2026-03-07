# Data Model

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

A **Payload** defines what a container should hold. A **Bin** is the physical container; it carries its payload assignment, manifest, and UOP remaining directly. An **Order** moves a bin between **Nodes**.

---

## Entities

### Payload

A template defining a container's expected contents and capacity. Analogous to a product SKU for containers.

| Field | Description |
|-------|-------------|
| `code` | Unique identifier (e.g., `BRK-ROTOR-KIT`) |
| `description` | Human-readable label |
| `uop_capacity` | Production cycles supported by a full load |
| `default_manifest_json` | Template manifest (parts and quantities for a full load) |

**Relationships:**
- Has many compatible **bin_types** (via `payload_bin_types` junction)
- Can be assigned to **nodes** (via `node_payloads` junction) — controls which payloads are accepted at delivery nodes

### Bin

A physical container that can be tracked, moved, and stored. The bin is the primary physical entity. It carries its payload assignment, manifest, and consumption state directly.

| Field | Description |
|-------|-------------|
| `label` | Unique identifier / QR code (e.g., `SHG:0042`) |
| `bin_type_id` | Physical container class |
| `node_id` | Current floor location (nullable — bin may be in transit) |
| `status` | Lifecycle state (see [Bin Statuses](#bin-statuses)) |
| `claimed_by` | Order ID that has claimed this bin for transport (nullable) |
| `payload_code` | Assigned payload template code (empty if unloaded) |
| `manifest` | JSON list of parts and quantities (actual contents) |
| `uop_remaining` | Production cycles of material left |
| `manifest_confirmed` | Whether the operator has verified the contents |
| `loaded_at` | When the manifest was confirmed (used for FIFO ordering) |
| `staged_at` | When the bin entered staged status |
| `staged_expires_at` | When staged status auto-expires back to available |

**Relationships:**
- Belongs to one **BinType**
- Sits at one **Node** (nullable)
- Can be claimed by one **Order**

### BinType

Classification for physical containers. Controls compatibility with nodes and payloads.

| Field | Description |
|-------|-------------|
| `code` | Unique identifier (e.g., `TOTE-A`, `PALLET`) |
| `description` | Human-readable label |
| `width_in`, `height_in` | Physical dimensions (inches) |

**Relationships:**
- Has many **Bins**
- Compatible with many **Payloads** (via `payload_bin_types`)
- Accepted at many **Nodes** (via `node_bin_types`) — used for lane bin type restrictions

### Node

A physical floor location where bins can sit.

| Field | Description |
|-------|-------------|
| `name` | Unique location identifier (e.g., `STG-A1`, `LINE1-IN`) |
| `is_synthetic` | Whether this is a virtual grouping node (not a physical slot) |
| `zone` | Logical area grouping |
| `enabled` | Whether the node accepts orders |
| `node_type_id` | Classification (optional) |
| `parent_id` | Parent node for hierarchical structures (optional) |

**Relationships:**
- Belongs to one **NodeType** (optional)
- Has one parent **Node** (optional, for hierarchy)
- Has many child **Nodes**
- Has many **Bins** at this location
- Has many **node_properties** (key-value pairs, e.g., `depth` for lane slots)
- Accepts specific **BinTypes** (via `node_bin_types`)
- Accepts specific **Payloads** (via `node_payloads`)
- Associated with **Stations** (via `node_stations`)

### NodeType

Classification for nodes. Controls dispatch behavior.

| Code | Name | Synthetic | Purpose |
|------|------|-----------|---------|
| `NGRP` | Node Group | Yes | Groups lanes and/or direct children for dispatch resolution |
| `LANE` | Lane | Yes | Linear sequence of slots in a supermarket |
| `SHF` | Shuffle Row | Yes | Temporary holding for reshuffle operations |

Physical nodes (storage slots, line-side locations, staging areas) typically have no node type.

### Order

A transport request to move a bin between nodes.

| Field | Description |
|-------|-------------|
| `edge_uuid` | UUID assigned by the requesting edge station |
| `station_id` | Which edge station requested this |
| `order_type` | `retrieve`, `store`, or `move` |
| `status` | Lifecycle state (see [Order Statuses](#order-statuses)) |
| `pickup_node` | Source node name |
| `delivery_node` | Destination node name |
| `vendor_order_id` | Fleet backend's order ID |
| `vendor_state` | Fleet backend's current state string |
| `robot_id` | Assigned robot |
| `priority` | Dispatch priority (higher = more urgent) |
| `payload_code` | What payload was requested (for retrieve orders) |
| `bin_id` | Which bin is being moved |
| `parent_order_id` | Parent compound order (for reshuffle child orders) |
| `sequence` | Step sequence within a compound order |

---

## Statuses

### Bin Statuses

| Status | Description |
|--------|-------------|
| `available` | Normal operating state — eligible for dispatch |
| `staged` | Temporarily placed at destination, awaiting operator confirmation |
| `flagged` | Marked for attention — excluded from dispatch but remains in place |
| `maintenance` | Pulled for repair — excluded from dispatch |
| `retired` | Permanently removed from operations (record retained) |

### Order Statuses

| Status | Description |
|--------|-------------|
| `pending` | Created, awaiting dispatch |
| `sourcing` | Resolver is finding source or destination |
| `dispatched` | Sent to fleet backend |
| `in_transit` | Robot is moving the bin |
| `delivered` | Robot has placed the bin at destination |
| `confirmed` | Edge station has confirmed physical receipt |
| `completed` | Fully done |
| `failed` | Fleet or system error |
| `cancelled` | Cancelled by station or operator |

### Manifest Confirmation

Bins track whether their contents have been verified via `manifest_confirmed`:

| State | Meaning |
|-------|---------|
| `false` | Payload assigned but contents not verified — bin is treated as empty by dispatch |
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

A bin is eligible for retrieval only when all three conditions are met:
1. `manifest_confirmed = true` — contents have been verified
2. `status = 'available'` — bin is in normal operating state
3. `claimed_by IS NULL` — bin is not already claimed by another order

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

When an order targets a synthetic node (NGRP), the dispatch resolver walks the hierarchy to find the appropriate physical slot. For retrieves, it finds the oldest eligible bin across all children. For stores, it finds the deepest available slot, preferring consolidation (same payload in the same lane).

### Lane Storage

Lanes are linear sequences of slots where only the front slot (depth 1) is physically accessible. Storage is packed from the back (deepest first). See [payloads.md](payloads.md) for detailed storage and reshuffle logic.

### Bin Type Restrictions

Lanes can restrict which bin types they accept (via `node_bin_types`). During store resolution, lanes that do not accept the incoming bin's type are skipped.

---

## Relationship Diagram

```
node_types --< nodes --< node_properties
                 |
                 |--< node_stations
                 |--< node_payloads ---> payloads
                 |--< node_bin_types --> bin_types
                 |
                 +--< bins
                        |
                        +--> bin_types --< payload_bin_types --> payloads
                                                                    |
                                                                    +--< payload_manifest

orders --< order_history
  |
  |---> nodes (source_node_id, dest_node_id)
  +---> bins

corrections ---> nodes, bins
cms_transactions ---> nodes, bins, orders

outbox                  (message queue)
audit_log               (system-wide audit)
admin_users             (authentication)
edge_registry           (connected edge stations)
scene_points            (fleet map cache)
demands, production_log (demand planning)
test_commands           (fleet testing)
```
