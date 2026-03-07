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

Node types include `NGRP` (node group), `LANE` (lane), `SHF` (shuffle row), plus physical nodes for storage, line-side, and staging.

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

Order statuses: `pending`, `sourcing`, `dispatched`, `in_transit`, `delivered`, `confirmed`, `completed`, `failed`, `cancelled`.

### Zone

A logical grouping of nodes, typically corresponding to a physical area of the facility (a floor, a warehouse section, a production line area).

| System | Term |
|---|---|
| Shingo | **Zone** |
| Seer RDS | Area (scene area) |
| MiR | Zone / Map group |
| VDA 5050 | Zone (same) |

### Station

A Shingo Edge instance identity. Each edge station has a unique station ID derived from its namespace and line identifier (e.g., `plant-a.line-1`). Stations are registered in core's edge registry and monitored via heartbeats.

### Process

A production area monitored by a station. Stored in the database as `production_lines`. Each process can produce one active job style at a time.

### Style

An end-item type that a process produces. Stored in the database as `job_styles`. Switching the active style on a process is handled through the changeover workflow.

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

A linear sequence of storage slots within a supermarket. The front slot (depth 1) is robot-accessible; deeper slots are blocked by those in front.

### Shuffle Row

Temporary holding slots used during retrieval reshuffles. When a target bin is blocked by other bins, the blocking bins are moved to the shuffle row, the target is retrieved, and the blocking bins are restocked.

### Changeover

The workflow for switching a production line from one job style to another. Progresses through a fixed sequence: stopping, counting out, storing, delivering, counting in, and ready.

## Naming Conventions

- **Go structs** use Shingo terms: `store.Bin`, `store.Payload`, `store.Node`
- **JSON API fields** use `snake_case`: `bin_type_id`, `payload_code`, `manifest_confirmed`
- **HTML/CSS classes** use `kebab-case`: `tile-loc`, `occupancy-modal`
- **Vendor-specific code** (inside `rds/`, Fleet Explorer) uses the vendor's own terminology
