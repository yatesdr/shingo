# Material Flow

This document covers bins, payloads, automated supermarket storage, and material handling cycles in Shingo.

---

## Terminology

### Payload

A payload defines a bin's expected contents and production capacity.

Fields:
- **Code**: short identifier (e.g., `BRK-ROTOR-KIT`)
- **Description**: descriptive label (e.g., "Brake Rotor Kit")
- **UOP Capacity**: production cycles supported by a full load (e.g., 24)
- **Template Manifest**: expected part numbers and quantities for a full load
- **Compatible Bin Types**: which physical bin types can carry this payload

If the same physical bin frame carries different parts for different processes, define separate payloads. The payload represents the combination of container configuration and contents.

### Bin

A bin is a specific physical container. It carries its payload assignment, manifest, and consumption state directly.

Fields:
- **Label**: unique QR code identifier (e.g., `SHG:0042`)
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

### Supermarket

An automated storage area consisting of lanes and a shuffle row, represented as a synthetic (group) node of type `NGRP`.

### Lane

A linear sequence of storage slots within a supermarket. The front slot (depth 1) is robot-accessible; deeper slots are blocked by those in front. Lane depth varies. Each lane is a synthetic node of type `LANE`.

### Slot

A single storage position within a lane, holding exactly one bin. Each slot has a **depth** value: 1 = front (accessible), higher = deeper (blocked). Slots are physical nodes with a `depth` property.

### Shuffle Row

Temporary holding slots used during retrieval reshuffles. When a target bin is blocked, the system moves blocking bins to the shuffle row, retrieves the target, then restocks the blocking bins.

The shuffle row must have at least as many slots as the deepest lane minus one. Shuffle slots are fully accessible (no blocking between them). The shuffle row is a synthetic node of type `SHF`.

### UOP (Unit of Production)

One cycle of the manufacturing process supported by the bin's parts. A Brake Rotor Kit with UOP capacity 24 contains enough parts for 24 brake assemblies. UOP remaining drives reorder decisions: when it drops below the configured threshold, the system orders a replacement bin.

---

## Storage Logic

### Contiguous Packing

Bins in a lane are packed from the back. Occupied slots form a continuous block from the deepest position forward. No empty gaps exist between bins. New bins are placed in the deepest available empty slot.

```
Empty lane:    [ ][ ][ ][ ][ ]    Store first bin ->  [ ][ ][ ][ ][A]
                                  Store second bin -> [ ][ ][ ][B][A]
                                  Store third bin ->  [ ][ ][C][B][A]
```

Bin A is the oldest (first in, deepest position). FIFO retrieval takes A first.

### Store Order Resolution

When a store order targets a supermarket:

1. Find all lanes with available space
2. Prefer lanes already holding the same payload (consolidation)
3. Among those, prefer the lane with the deepest target slot (most packed)
4. If no lane holds that payload, select any lane with space
5. Place the bin in the deepest accessible empty slot
6. Skip lanes that restrict bin types incompatible with the incoming bin

### Retrieve — Direct Access

When a station requests a bin of a specific payload:

1. Locate the oldest bin of that payload across all lanes (by `loaded_at` timestamp)
2. If the bin is at the front of its lane (no blocking bins), retrieve directly
3. Deliver to the requesting station

### Retrieve — Reshuffle

If the oldest bin is blocked:

1. **Unbury**: move blocking bins from the lane to the shuffle row, front to back
2. **Retrieve**: pick up the target bin and deliver it
3. **Restock**: return blocking bins from the shuffle row to the lane, deepest-first

After restocking, remaining bins maintain FIFO order and the lane is packed with no gaps.

```
Before:   [C][B][A]    Target: A (oldest)

Unbury:   Move C -> Shuffle, Move B -> Shuffle
Pick:     Retrieve A -> deliver to station
Restock:  Move B -> depth 3, Move C -> depth 2

After:    [ ][C][B]    B is now oldest, at the back
```

### Lane Locking

During a reshuffle, the lane is locked — no other store or retrieve operations may use it. The lock is released upon completion.

### Reshuffle Failure

If a robot fails mid-reshuffle:

1. The sequence halts; no further moves are attempted
2. The reshuffle is marked as failed
3. The lane remains locked (bins are split between lane and shuffle row)
4. An operator alert is raised

The operator may then **retry** the failed move, **abort** and manually return bins, or **skip** the move if the blockage was manually cleared. The lane remains locked until the operator restores a consistent state.

### Flagged Bins in Lanes

Flagged bins are treated as occupied slots. Stores can occur in front of them. During a reshuffle that passes through a flagged bin, the system moves it to the shuffle row normally but places it at depth 1 on restock for easy maintenance access.

---

## Material Handling Cycles

Material flow follows a circular pattern. A bin sits at a station. It is consumed (parts removed) or filled (parts added). When the bin is spent, two things happen: the outgoing bin leaves and a replacement arrives. This is one cycle, regardless of whether the station consumes or produces material.

Both consume and produce payloads follow the same cycle path. The **role** determines which direction bins flow — full in / empty out (consume), or empty in / full out (produce). The **cycle mode** determines the robot choreography.

### Triggering

A cycle triggers when UOP remaining crosses the configured reorder point. Two trigger sources are available, controlled by the **AutoReorder** setting:

- **AutoReorder ON** — The system monitors PLC counter deltas and triggers the cycle automatically when remaining drops below the reorder point.
- **AutoReorder OFF** — The operator presses the REQUEST button on the operator canvas to trigger the cycle manually.

Both consume and produce payloads use the same trigger mechanism. For consume payloads, remaining drops as parts are used. For produce payloads, remaining drops as bin capacity fills up.

### Cycle Modes

Each payload is configured with one of three cycle modes. The mode applies equally to both roles.

**Sequential** (default) — One robot handles the swap in two phases. Order A drives to lineside empty, waits for operator release, picks up the outgoing bin, and delivers it to the outgoing destination. When Order A is released, Order B is created to deliver the replacement from the pickup source.

**Two Robot** — Two robots work concurrently. Robot 1 (resupply) stages the replacement at the staging area and waits. Robot 2 (removal) navigates to lineside and waits. The operator releases both when ready — removal picks up the outgoing bin, resupply delivers the replacement.

**Single Robot** — One robot executes a 10-step sequence using two staging areas: pickup replacement → stage at area 1 → navigate to lineside → wait → pickup outgoing → stage at area 2 → pickup replacement from area 1 → deliver to lineside → pickup outgoing from area 2 → deliver to outgoing destination.

### Roles

| | Consume | Produce |
|---|---|---|
| **Incoming bins** | Full (parts to use) | Empty (capacity to fill) |
| **Outgoing bins** | Empty (spent) | Full (finished goods) |
| **RetrieveEmpty flag** | `false` — retrieve a full bin | `true` — retrieve an empty bin |

The role is derived at order creation time from the payload's `role` field. Operators do not configure retrieval direction directly.

### Operator Interactions

The operator canvas presents three actions for each payload:

- **REQUEST** — Triggers a material handling cycle (available as an early override even when AutoReorder is ON)
- **RELEASE** — Signals the robot to pick up the outgoing bin (when the order is staged at lineside)
- **CONFIRM** — Acknowledges delivery of the replacement bin; resets UOP remaining to capacity

### Reset Behavior

UOP remaining resets when the replacement bin is delivered and confirmed, not when the outgoing bin is ingested by Core. The reset value comes from the payload catalog's UOP capacity, synced from Core via Kafka. PayloadCode is mandatory on every payload configuration — it is the lookup key for the catalog.

### Node Configuration

Each cycle mode requires a set of node assignments. Nodes can be configured as a specific node, a node group (Core resolves to a physical node), or left blank for Core to decide.

| Node Field | Sequential | Two Robot | Single Robot |
|---|---|---|---|
| **Full Pickup Source** | Yes | Yes | Yes |
| **Staging Area 1** | — | Yes | Yes |
| **Staging Area 2** | — | — | Yes |
| **Outgoing Destination** | Yes | Yes | Yes |

- **Full Pickup Source**: where replacement bins come from (e.g., a supermarket node group)
- **Staging Area 1**: intermediate staging for the replacement bin before the swap
- **Staging Area 2**: second staging area for the outgoing bin during single-robot shuffle
- **Outgoing Destination**: where outgoing bins are sent (e.g., empty bin storage, wash area, or shipping)

---

## Bin Identification

### Label Format

Labels follow the format `SHG:NNNN` (e.g., `SHG:0042`). The `SHG:` prefix distinguishes Shingo labels from other QR codes. The number is unique across all bins regardless of type.

### QR Code Functions

1. **Verification**: robot scans at pickup to confirm identity; mismatches halt the operation
2. **Damage tracking**: repeated errors against a specific label enable proactive flagging for repair
3. **Audit trail**: every move, scan, error, and inspection is logged against the label

### Physical Labels

- Use durable metal or heavy-duty polymer asset tags
- Print both the QR code and human-readable label on the same tag
- Mount in a consistent position for robot camera alignment
- Consider a backup label in a second location

---

## Flagging and Maintenance

### Flagging

A flagged bin:
- Remains at its current location
- Is excluded from retrieve orders
- Is highlighted in the supermarket view
- Can be retrieved for inspection when convenient

### Maintenance Flow

1. Operator flags the bin with a reason — status: `flagged`
2. Operator triggers "Retrieve for Maintenance" — system delivers to accessible location — status: `maintenance`
3. After repair, operator activates the bin — status: `available`, returned to storage

### Retiring

Set status to `retired` to permanently remove a bin from operations. The record is retained for historical reference.

---

## Setup

### Step 1: Define Bin Types

On the **Bins** page:
1. Open the Bin Types section
2. For each container class, create a bin type with code, description, and dimensions

### Step 2: Define Payloads

On the **Payloads** page:
1. Create a payload with code, description, and UOP capacity
2. Set the template manifest (expected parts and quantities)
3. Assign compatible bin types

### Step 3: Register Bins

On the **Bins** page:
1. Click "Create Bin" or use bulk registration
2. Enter the label (QR code identifier)
3. Select the bin type
4. Optionally assign an initial node location

### Step 4: Assign Payloads to Bins

On the **Bins** page:
1. Select a bin
2. Assign a payload — this sets the payload code and populates the manifest from the template
3. Confirm the manifest — marks the bin as loaded and sets the FIFO timestamp

### Step 5: Create a Supermarket

On the **Nodes** page:
1. Click "Create Supermarket", assign a name and zone
2. Add lanes: specify slot count and vendor location IDs per slot (first = depth 1)
3. Define the shuffle row: slot count and vendor locations (minimum: deepest lane minus one)
4. Review and create — the system generates all nodes, sets depth properties, and links the hierarchy

### Step 6: Assign Payload Eligibility

For each delivery node:
1. Open the node detail
2. Under "Payloads", select accepted payloads
3. This controls which payloads appear in the edge station's order dropdown

### Step 7: Configure Edge Payloads

On each edge station's **Setup** page:
1. Under "Payloads", add a payload configuration
2. Select a Payload Code from the catalog (auto-fills description and UOP capacity)
3. Set the Location (lineside node), Role (consume or produce), and Reorder Point
4. Choose a Cycle Mode: sequential, two_robot, or single_robot
5. Configure node fields based on cycle mode: Full Pickup Source, Staging Area(s), Outgoing Destination
6. Enable AutoReorder for system-triggered cycles, or leave it off for operator-triggered REQUEST
7. The payload catalog is synced automatically from Core

### Step 8: Load Initial Inventory

1. Load bins at the loading dock (assign payload, confirm manifest)
2. Issue store orders to place them in the supermarket
3. The system assigns each bin to the optimal lane and slot
4. Verify positions in the supermarket visualization

---

## Operations

### Supermarket View

Click a supermarket on the Nodes page to view the lane visualization: slot occupancy, flagged bins, and active reshuffles.

### Bins Page

Lists all bins with filtering by bin type, status, or location. Shows payload assignment and manifest confirmation state. Click a bin for details and actions (flag, maintain, retire, clear, assign payload).

### Orders

Compound orders (reshuffles) display a progress indicator for child moves. Expand to view individual moves.

### Discrepancy Handling

On QR scan mismatch:
1. The order is halted
2. The mismatch is logged
3. The operator is notified to investigate and resolve (correct system data or relocate the bin)

---

## Glossary

| Term | Definition |
|------|-----------|
| **Payload** | A template defining expected bin contents and UOP capacity |
| **Bin** | A specific physical container, identified by QR label |
| **Bin Type** | Physical container classification (size, form factor) |
| **Label** | Unique QR code identifier on a bin (e.g., `SHG:0042`) |
| **UOP** | Unit of Production — one manufacturing cycle supported by the bin's parts |
| **Manifest Confirmed** | Whether an operator has verified the bin's contents match the payload template |
| **Supermarket** | Automated storage zone containing lanes and a shuffle row |
| **Lane** | Linear sequence of storage slots; front is accessible, back is blocked |
| **Slot** | Single storage position in a lane; holds one bin |
| **Depth** | Slot position in its lane (1 = front/accessible, higher = deeper/blocked) |
| **Shuffle Row** | Temporary holding slots used during retrieval reshuffles |
| **Reshuffle** | Moving blocking bins to the shuffle row to access a target bin |
| **FIFO** | First In, First Out — oldest material is retrieved first |
| **Contiguous Packing** | Occupied slots form an unbroken block from the back of a lane |
| **Flagged** | Bin marked as problematic, excluded from dispatch until resolved |
| **Manifest** | List of parts and quantities on a specific bin |
| **Template Manifest** | Expected parts and quantities for a fully-loaded bin of a given payload |
| **Reorder Point** | UOP threshold below which a material handling cycle is triggered (when AutoReorder is ON) |
| **AutoReorder** | Setting that controls whether cycles are triggered automatically by PLC counters or manually by the operator |
| **Cycle Mode** | Robot choreography strategy for a material handling cycle (sequential, two-robot, single-robot) |
| **Role** | Direction of material flow — consume (full in, empty out) or produce (empty in, full out) |
| **Outgoing Destination** | Node where spent or finished bins are sent after removal from the station |
| **Full Pickup Source** | Node or node group where replacement bins are retrieved from |
| **Staging Area** | Intermediate holding node used in hot-swap cycle modes |
| **Operator Canvas** | Real-time visual display on Edge showing payload states, order progress, and operator action buttons |
| **Payload Catalog** | List of payloads available to an edge station, synced from Core |
