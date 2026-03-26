# Node Naming Standards

Reference document for physical and synthetic node naming conventions used across the Shingo system.

## Overview

Nodes represent physical locations on the shop floor mapped by an AMR. They are defined in the fleet management software and carry corresponding (X, Y) coordinates used for robot navigation.

Node IDs follow the VDA5050 standard `nodeId` field — a unique string identifier assigned to each waypoint on the navigation map. Consistent naming is critical for the following reasons:

| Concern | Detail |
|---------|--------|
| **Shingo Integration** | Shingo references nodes by name at dispatch time (`GetNodeByDotName`). The node name appears in order records, audit logs, CMS transaction boundaries, and the diagnostics UI. Inconsistent or ambiguous names impede order routing, troubleshooting, and ERP reconciliation. |
| **Human Readability** | Operators, maintenance staff, and engineers must be able to read a node name in a log or on a screen and immediately identify the process it refers to. Shingo Node Groups can be used to represent specific lines, zones, areas, or processes within the plant. |
| **Shingo Data Model** | Shingo's data model introduces additional constructs that reference common plant terminology in supporting records. Because node names are defined in third-party fleet management software, the goal is to keep them simple and universal. |

### How Node Names Flow Through the System

When an Edge station creates a material order, the node names from the StyleNodeClaim routing fields (`InboundSource`, `InboundStaging`, `CoreNodeName`, `OutboundStaging`, `OutboundDestination`) are embedded into `ComplexOrderStep` structs and sent to Core over Kafka. Core's `resolveStepNode()` function (`dispatch/complex.go`) then processes each step through a three-tier resolution pipeline:

1. **Explicit Node**: The step specifies a concrete node name → Core calls `GetNodeByDotName()` to validate it exists, then checks if it's a synthetic NGRP. If not, uses it directly.
2. **Node Group (NGRP)**: The step specifies an NGRP name → Core's GroupResolver applies the configured algorithm (FIFO, FAVL, LKND, DPTH) to select a concrete physical slot within the group.
3. **Global Fallback**: The step has no node name (empty string) → Core resolves via `payloadCode` using `FindSourceBinFIFO()` (for retrieval) or `FindStorageDestination()` (for storage).

This pipeline is transparent to Edge — a StyleNodeClaim's `InboundSource` can be a physical node (`SMN_004`), a node group (`SYN_Supermarket_1`), or blank. Core handles all three cases identically through `resolveStepNode()`.

## Where Node Names Are Set

Node names originate in the fleet management software — for example, RDS (Robot Dispatch System) — not in Shingo. They are entered by the staff configuring the navigation map. Shingo consumes whatever name the fleet manager assigns; it does not generate or validate naming structure at registration time.

In the Shingo Core database, physical nodes are stored in the `nodes` table with columns including `name` (TEXT UNIQUE), `is_synthetic` (BOOLEAN), `node_type_id` (references `node_types`), `parent_id` (for hierarchy), and `depth` (slot position in a lane). Core syncs these from the fleet manager via scene sync (`fleet/seerrds/adapter.go`), and Edge receives a copy via the `node.list_request`/`node.list_response` protocol exchange.

## Physical Node Naming Convention

The recommended format for physical node names is:

```
{TYPE_CODE}_{SEQUENCE}
```

| Part | Description | Example |
|------|-------------|---------|
| `TYPE_CODE` | 3-letter uppercase code identifying the node category (see Node Type Codes below) | `PLN` |
| `SEQUENCE` | Zero-padded 3-digit position or index | `001`, `042` |

The separator is an underscore (`_`) rather than a hyphen. Hyphens are valid in VDA5050 `nodeId` strings, but underscores are safer when node names appear in SQL identifiers, column aliases, or composite keys — contexts where a hyphen can be misinterpreted as a minus operator. Underscores also pass through most export and import pipelines without requiring quoting.

| Example | Description |
|---------|-------------|
| `PLN_001` | Press Line Node, position 1 |
| `SMN_034` | Supermarket Node, position 34 |
| `DZN_008` | Drop Zone Node, position 8 |

### How Type Codes Relate to the Shingo Data Model

Shingo Core maintains a `node_types` table that stores type codes (e.g., `NGRP`, `LANE`). Physical nodes can optionally reference a `node_type_id`. The physical node type codes listed below (`PLN`, `ALN`, `SMN`, etc.) are a **naming convention** — they encode the node's purpose into its name for human readability. Shingo does not parse or validate the type code prefix; it uses the `node_types` table for programmatic type checking (e.g., checking `NodeTypeCode == "NGRP"` to determine if a node should be resolved via the GroupResolver).

This means the type code in a node's name is for humans; the `node_type_id` foreign key is for Shingo's dispatch logic.

## Node Type Codes

| Code | Name | Description |
|------|------|-------------|
| `PLN` | Press Line Node | A node at a press line directly interfaced by a human operator or press-side robot. Robots pick up or deposit material here as part of the press feed cycle. |
| `ALN` | Assembly Line Node | A node at an assembly station. Functions identically to a Press Line Node in terms of AMR interaction — the distinction exists for routing logic, reporting, and CMS zone boundaries. |
| `SLN` | Staging Lane Node | A node in a staging lane that supports hot-swapping of material without stopping line production. Used during changeover processes or when a loaded bin must be exchanged without a line stoppage. In Shingo, these map to the `InboundStaging` and `OutboundStaging` fields on a StyleNodeClaim. |
| `SMN` | Supermarket Node | A node within an AMR storage area (supermarket). Applies to both large central supermarkets and satellite or line-side markets. These nodes are typically children of an NGRP (Node Group) in Shingo and participate in FIFO retrieval and reshuffle logic via the GroupResolver. |
| `DZN` | Drop Zone Node | A buffer between processes; handoff point. |
| `QIN` | Quality Inspect Node | A node at a quality inspection station. Material arriving here is held pending QC sign-off before being released for further routing. Sites should use process-based naming rather than customer-specific naming for this node type (e.g., GP12). |
| `QHN` | Quality Hold Node | A node designated for quarantined material that has failed inspection or is pending disposition. Distinct from `QIN` — this is a hold location, not an active inspection station. |
| `RWN` | Rework Node | A node at a rework station where non-conforming parts are returned for correction before re-entering the production flow. |
| `FGN` | Finished Goods Node | A node in the finished goods area. Represents the final AMR delivery point before product exits the Shingo-managed floor zone. |
| `SCN` | Scrap Node | A node at a scrap or discard location. Material routed here is considered end-of-life within the production system. |
| `BMN` | Bin Maintenance Node | A node to send the AMR and bin to the AMR maintenance zone. |
| `UTN` | Utility Node | A node that acts as a catch-all for overflow, bank builds, or temporary zones. |
| `PLK` | Pick Location Node | A node for part picks: supermarket decant, repack, or similar functions. |

### Type Codes in Context: Order Routing

When a StyleNodeClaim on Edge specifies `InboundSource: "SMN_034"`, Edge's `buildStep()` function (`engine/material_orders.go`) creates a `ComplexOrderStep{Action: "pickup", Node: "SMN_034"}`. Core receives this and resolves it:

- `SMN_034` is a physical node → Core validates it exists via `GetNodeByDotName("SMN_034")` → uses it directly as the pickup location.

When the claim specifies `InboundSource: "SYN_Supermarket_1"` (an NGRP):

- Core detects `IsSynthetic == true` and `NodeTypeCode == "NGRP"` → delegates to GroupResolver → algorithm selects a specific slot (e.g., `SMN_034`) within the group.

When the claim leaves `InboundSource` blank:

- `buildStep()` returns `ComplexOrderStep{Action: "pickup"}` with no node → Core falls back to global resolution via `payloadCode`.

## Synthetic Nodes (Shingo-Managed)

Synthetic nodes exist only within Shingo's data model. They have no corresponding VDA5050 waypoint or fleet manager entry and carry no physical (X, Y) coordinates. They are created and managed directly in Shingo to define dispatch behaviors, group physical nodes, and configure resolver algorithms.

### Naming Convention

Synthetic nodes use descriptive full-text names prefixed with `SYN_` to clearly distinguish them from physical nodes in logs, order records, and the diagnostics UI:

```
SYN_{Descriptive Name}
```

| Example | Description |
|---------|-------------|
| `SYN_Supermarket_1` | Central supermarket node group |
| `SYN_Lineside_A` | Line-side satellite market, line A |
| `SYN_Bulk_Storage` | Bulk storage node group |

Full-text naming is intentional — synthetic nodes are configured by Shingo administrators, not fleet operators, so readability takes priority over brevity.

### Hierarchy

Synthetic nodes form a two-level hierarchy:

| Level | Type | Description |
|-------|------|-------------|
| 1 | **Node Group (NGRP)** | The top-level synthetic node. Accepts inbound orders and distributes them to child nodes based on the configured resolver algorithm (FIFO, FAVL, LKND, DPTH). Inheritable properties propagate to children unless overridden. |
| 2a | **Physical Child** | A physical floor node registered as a direct child of the NGRP. Addressable individually via dot notation. |
| 2b | **Lane (LANE)** | A depth-aware synthetic child node. Represents a slot sequence within dense-pack storage. Manages buried-payload reshuffle logic. |

In the Core database, this hierarchy is modeled via `parent_id` on the `nodes` table. A LANE's children are physical slot nodes with a `depth` value (1 = front/accessible, N = back/storage). The NGRP type is stored in the `node_types` table and checked by the GroupResolver when deciding how to resolve a step.

#### How the GroupResolver Uses This Hierarchy

When `resolveStepNode()` encounters an NGRP node, it delegates to the GroupResolver (`dispatch/group_resolver.go`), which queries the NGRP's children:

**Retrieval algorithms** (select which bin to pick up):
| Algorithm | Behavior |
|-----------|----------|
| **FIFO** | Selects the oldest bin by `loaded_at` timestamp. If the bin is buried (not at the front of a lane), triggers a reshuffle compound order to unbury it first. |
| **FAVL** | First available — picks the first unclaimed bin at an accessible depth. No reshuffle. |

**Storage algorithms** (select where to put a bin):
| Algorithm | Behavior |
|-----------|----------|
| **LKND** | Like-kind consolidation — prefers lanes that already contain the same payload code, then falls back to the emptiest lane. |
| **DPTH** | Depth-first packing — fills lanes from back to front, preferring lanes over direct children. |

The algorithm is configured per NGRP via node properties (`retrieve_algorithm`, `store_algorithm`). A `LaneLock` mechanism prevents concurrent reshuffles on the same lane.

### Addressing

Child nodes within a group may be addressed directly using Shingo dot notation:

| Address | Description |
|---------|-------------|
| `SYN_Supermarket_1.SMN_004` | Direct physical child node |
| `SYN_Supermarket_1.Lane_3` | Lane child node |

Core's `GetNodeByDotName()` function (`store/nodes.go`) handles dot notation by splitting on the first dot and querying for the child node whose `parent_id` matches the parent name. This enables explicit targeting of a specific slot or lane within a group, bypassing the GroupResolver's algorithm.

Individual slot nodes within a Lane are not directly addressable. Addressing a Lane slot directly would bypass the packing algorithm and is not permitted. Lane slot nodes do not appear in the node selection list.

## Notes

| # | Note |
|---|------|
| 1 | All node names must be registered in the fleet management software before being referenced in Shingo. Shingo will reject orders targeting an unrecognized node name. |
| 2 | Synthetic nodes (NGRP, LANE) are managed internally by Shingo and do not require a corresponding fleet management entry. See the Synthetic Nodes section for naming convention and hierarchy. |
| 3 | Node names are case-sensitive in Shingo's database and protocol layer. Uppercase type codes must be used consistently across all deployments. |
