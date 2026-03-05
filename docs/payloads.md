# Payloads — Concepts & Setup

This document covers payload management and automated supermarket storage in Shingo: core concepts, terminology, and setup procedures.

---

## Design Principles

Shingo treats payload management as a closed-loop system. Every cart is registered, tracked, and verified at each handoff.

- **System-directed retrieval.** Operators request material by type; the system locates and retrieves the correct cart automatically, including reshuffling blocked carts.
- **Engineered depletion.** Carts are designed so all parts deplete together after a known number of production cycles. A single value — units of production (UOP) remaining — describes cart state.
- **FIFO enforcement.** The oldest material is always retrieved first, enforced automatically by storage and retrieval logic.
- **Physical verification.** Each cart carries a QR code scanned by the robot at pickup to confirm identity and maintain chain of custody.

---

## Terminology

### Payload Style

A style defines a cart type — its physical form and expected contents.

Fields:
- **Code**: short identifier (e.g., `BRK-ROTOR-KIT`)
- **Name**: descriptive label (e.g., "Brake Rotor Kit")
- **UOP Capacity**: production cycles supported by a full load (e.g., 24)
- **Template Manifest**: expected part numbers and quantities for a full load
- **Physical Geometry** (optional): width, depth, height, weight limit — used for slot compatibility

If the same physical cart frame carries different parts for different processes, define separate styles. The style represents the combination of container and contents.

### Payload Instance

An instance is a specific physical cart.

Fields:
- **Tag ID**: unique QR code identifier (e.g., `SHG:0042`)
- **Style**: determines contents and UOP capacity
- **Location**: current node
- **Loaded At**: timestamp for FIFO ordering
- **UOP Remaining**: production cycles of material left
- **Manifest**: actual contents (may differ from template if partially consumed or corrected)
- **Status**: `available`, `claimed`, `in_transit`, `empty`, `flagged`, `maintenance`, `retired`

### Supermarket

An automated storage area consisting of lanes and a shuffle row, represented as a synthetic (group) node of type `SUP`.

### Lane

A linear sequence of storage slots within a supermarket. The front slot (depth 1) is robot-accessible; deeper slots are blocked by those in front. Lane depth varies — lanes in the same supermarket may differ in length. Each lane is a synthetic node of type `LAN`.

### Slot

A single storage position within a lane, holding exactly one cart. Each slot has a **depth** value: 1 = front (accessible), higher = deeper (blocked). Slots are physical nodes with a `depth` property.

### Shuffle Row

Temporary holding slots used during retrieval reshuffles. When a target cart is blocked, the system moves blocking carts to the shuffle row, retrieves the target, then restocks the blocking carts.

The shuffle row must have at least as many slots as the deepest lane minus one. Shuffle slots are fully accessible (no blocking between them). The shuffle row is a synthetic node of type `SHF`.

### UOP (Unit of Production)

One cycle of the manufacturing process supported by the cart's parts. A Brake Rotor Kit with UOP capacity 24 contains enough parts for 24 brake assemblies. UOP remaining drives reorder decisions: when it drops below the configured threshold, the system orders a replacement cart.

---

## Storage Logic

### Contiguous Packing

Carts in a lane are packed from the back. Occupied slots form a continuous block from the deepest position forward. No empty gaps exist between carts. New carts are placed in the deepest available empty slot.

```
Empty lane:    [ ][ ][ ][ ][ ]    Store first cart →  [ ][ ][ ][ ][A]
                                  Store second cart → [ ][ ][ ][B][A]
                                  Store third cart →  [ ][ ][C][B][A]
```

Cart A is the oldest (first in, deepest position). FIFO retrieval takes A first.

### Store Order Resolution

When a store order targets a supermarket:

1. Find all lanes with available space
2. Prefer lanes already holding the same style (consolidation)
3. Among those, prefer the lane with the deepest target slot (most packed)
4. If no lane holds that style, select any lane with space
5. Place the cart in the deepest accessible empty slot

### Retrieve — Direct Access

When a station requests a cart of a specific style:

1. Locate the oldest cart of that style across all lanes (by load date)
2. If the cart is at the front of its lane (no blocking carts), retrieve directly
3. Deliver to the requesting station

### Retrieve — Reshuffle

If the oldest cart is blocked:

1. **Unbury**: move blocking carts from the lane to the shuffle row, front to back
2. **Retrieve**: pick up the target cart and deliver it
3. **Restock**: return blocking carts from the shuffle row to the lane, deepest-first

After restocking, remaining carts maintain FIFO order and the lane is packed with no gaps.

```
Before:   [C][B][A]    Target: A (oldest)

Unbury:   Move C → Shuffle, Move B → Shuffle
Pick:     Retrieve A → deliver to station
Restock:  Move B → depth 3, Move C → depth 2

After:    [ ][C][B]    B is now oldest, at the back
```

### Lane Locking

During a reshuffle, the lane is locked — no other store or retrieve operations may use it. The lock is released upon completion.

### Reshuffle Failure

If a robot fails mid-reshuffle:

1. The sequence halts; no further moves are attempted
2. The reshuffle is marked as failed
3. The lane remains locked (carts are split between lane and shuffle row)
4. An operator alert is raised

The operator may then **retry** the failed move, **abort** and manually return carts, or **skip** the move if the blockage was manually cleared. The lane remains locked until the operator restores a consistent state.

### Flagged Carts in Lanes

Flagged carts are treated as occupied slots. Stores can occur in front of them. During a reshuffle that passes through a flagged cart, the system moves it to the shuffle row normally but places it at depth 1 on restock for easy maintenance access.

---

## Consumption Tracking

### Station-Side (Edge)

When a cart is delivered to a production station:

1. UOP remaining is initialized to the style's full capacity
2. PLC counters track production output
3. Each unit produced decrements UOP remaining by 1
4. At the configured threshold, a replacement cart is ordered automatically
5. At zero, the cart is marked empty

### Reorder Rules

Each station configures reorder rules linking a payload style to a UOP threshold:

- "BRK-ROTOR-KIT: reorder at 6 UOP remaining"
- "BRK-PAD-KIT: reorder at 20 UOP remaining"

Set the threshold to allow sufficient time for retrieval and delivery before depletion.

---

## Cart Identification

### Tag Format

Tags follow the format `SHG:NNNN` (e.g., `SHG:0042`). The `SHG:` prefix distinguishes Shingo tags from other QR codes. The number is unique across all carts regardless of style.

### QR Code Functions

1. **Verification**: robot scans at pickup to confirm identity; mismatches halt the operation
2. **Damage tracking**: repeated errors against a specific tag enable proactive flagging for repair
3. **Audit trail**: every move, scan, error, and inspection is logged against the tag ID

### Physical Labels

- Use durable metal or heavy-duty polymer asset tags
- Print both the QR code and human-readable tag ID on the same label
- Mount in a consistent position for robot camera alignment
- Consider a backup label in a second location

---

## Flagging & Maintenance

### Flagging

A flagged cart:
- Remains at its current location
- Is excluded from retrieve orders
- Is highlighted in the supermarket view
- Can be retrieved for inspection when convenient

### Maintenance Flow

1. Operator flags the cart with a reason → status: `flagged`
2. Operator triggers "Retrieve for Maintenance" → system delivers to accessible location → status: `maintenance`
3. After repair, operator clears the flag → status: `available`, returned to storage

### Retiring

Set status to `retired` to permanently remove a cart from operations. The record is retained for historical reference.

---

## Setup

### Step 1: Define Payload Styles

On the **Config** page:
1. Open the Payload Styles section
2. For each cart type, create a style with code, name, and UOP capacity
3. Optionally add the template manifest (expected parts and quantities)

### Step 2: Register Cart Instances

On the **Fleet Inventory** page:
1. Click "Register Cart"
2. Enter or scan the tag ID
3. Select the style
4. Set the initial location
5. For bulk registration: specify count, style, tag range, and location

### Step 3: Create a Supermarket

On the **Nodes** page:
1. Click "Create Supermarket", assign a name and zone
2. Add lanes: specify slot count and vendor location IDs per slot (first = depth 1)
3. Define the shuffle row: slot count and vendor locations (minimum: deepest lane minus one)
4. Review and create — the system generates all nodes, sets depth properties, and links the hierarchy

### Step 4: Assign Style Eligibility

For each delivery node:
1. Open the node detail
2. Under "Payload Styles", select accepted styles
3. This controls which styles appear in the edge station's order dropdown

### Step 5: Configure Edge Reorder Rules

On each edge station's **Setup** page:
1. Under "Reorder Rules", add a rule per style
2. Set the UOP threshold for auto-reorder
3. Enable the rule
4. The style catalog is synced automatically from core

### Step 6: Load Initial Inventory

1. Load carts at the loading dock (apply parts, set manifest)
2. Issue store orders to place them in the supermarket
3. The system assigns each cart to the optimal lane and slot
4. Verify positions in the supermarket visualization

---

## Operations

### Supermarket View

Click a supermarket on the Nodes page to view the lane visualization: slot occupancy, flagged carts, and active reshuffles.

### Fleet Inventory

Lists all carts with filtering by style, status, or location. Click a cart to view its full event history.

### Orders

Compound orders (reshuffles) display a progress indicator for child moves. Expand to view individual moves.

### Discrepancy Handling

On QR scan mismatch:
1. The order is halted
2. The mismatch is logged
3. The operator is notified to investigate and resolve (correct system data or relocate the cart)

---

## Glossary

| Term | Definition |
|------|-----------|
| **Style** | A cart type definition: physical form, expected contents, UOP capacity |
| **Instance** | A specific physical cart, identified by QR tag |
| **Tag ID** | Unique QR code identifier on a cart (e.g., `SHG:0042`) |
| **UOP** | Unit of Production — one manufacturing cycle supported by the cart's parts |
| **Supermarket** | Automated storage zone containing lanes and a shuffle row |
| **Lane** | Linear sequence of storage slots; front is accessible, back is blocked |
| **Slot** | Single storage position in a lane; holds one cart |
| **Depth** | Slot position in its lane (1 = front/accessible, higher = deeper/blocked) |
| **Shuffle Row** | Temporary holding slots used during retrieval reshuffles |
| **Reshuffle** | Moving blocking carts to the shuffle row to access a target cart |
| **FIFO** | First In, First Out — oldest material is retrieved first |
| **Contiguous Packing** | Occupied slots form an unbroken block from the back of a lane |
| **Flagged** | Cart marked as problematic, excluded from dispatch until resolved |
| **Manifest** | List of parts and quantities on a specific cart |
| **Template Manifest** | Expected parts and quantities for a fully-loaded cart of a given style |
| **Reorder Rule** | Edge configuration triggering an automatic order when UOP remaining drops below a threshold |
| **Style Catalog** | List of styles available to an edge station, synced from core |
