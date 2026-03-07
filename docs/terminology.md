# ShinGo Terminology Reference

This document defines the vendor-neutral terminology used throughout ShinGo and maps each term to its equivalent in common fleet management systems.

## Core Concepts

### Node

A physical location in the facility where payloads can be stored, picked up, or delivered. Every node has a name, a vendor location identifier (the name known to the fleet backend), a type, a zone, and a capacity.

| System | Term |
|---|---|
| ShinGo | **Node** |
| Seer RDS | Bin Location (`GeneralLocation` class in scene data) |
| MiR | Position / Station |
| Locus Robotics | Location |
| 6 River Systems | Destination |
| VDA 5050 | Node (same) |

Node types include `NGRP` (node group), `LANE` (lane), `SHF` (shuffle row), plus physical nodes for storage, line-side, and staging.

### Bin

A physical container that holds materials and is tracked as it moves between nodes. Each bin has a type, a status, and optionally a payload (blueprint application with manifest). The bin is the primary physical entity in Shingo.

| System | Term |
|---|---|
| ShinGo | **Bin** |
| Seer RDS | Goods / Container (`goodsId`, `containerName`) |
| MiR | Payload |
| Locus Robotics | Tote / Cart |
| VDA 5050 | Load |

Bin statuses: `available`, `staged`, `flagged`, `maintenance`, `retired`.

### Blueprint

A template defining what a bin should contain — its expected parts, quantities, and UOP capacity. When a blueprint is applied to a bin, a **payload** record is created.

| System | Term |
|---|---|
| ShinGo | **Blueprint** |
| Warehouse systems | SKU / Item master |

### Payload

An internal record linking a blueprint to a bin. Tracks manifest confirmation and UOP consumption. Users interact with bins and blueprints; the payload is an implementation detail.

A payload is only eligible for dispatch when its manifest is confirmed (`manifest_confirmed = true`), the bin status is `available`, and the bin is not claimed by another order.

### Manifest

The list of items (parts, materials) inside a payload. Each manifest item has a part number, quantity, and optional notes. A **blueprint manifest** defines the template; the payload's **manifest items** are the actual contents.

| System | Term |
|---|---|
| ShinGo | **Manifest** |
| Seer RDS | No direct equivalent (goods are tracked by ID only) |
| Warehouse systems | Pick list / Packing list |

### Order

A transport request to move a payload between nodes. Orders flow from ShinGo Edge to ShinGo Core, which dispatches them to the fleet backend.

| System | Term |
|---|---|
| ShinGo | **Order** |
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
| ShinGo | **Zone** |
| Seer RDS | Area (scene area) |
| MiR | Zone / Map group |
| VDA 5050 | Zone (same) |

## Robot & Fleet Concepts

### Available

Whether a robot is accepting new orders from the dispatch system. A robot that is not available will finish its current task but will not be assigned new work.

| System | Term | Values |
|---|---|---|
| ShinGo | **Available** (bool) | `true` / `false` |
| Seer RDS | Dispatchable | `dispatchable`, `undispatchable_unignore`, `undispatchable_ignore` |
| MiR | State (Ready) | Ready / Paused |
| VDA 5050 | Operating mode | Automatic / Semi-automatic / Manual |

### Connected

Whether the fleet backend can communicate with the robot.

| System | Term |
|---|---|
| ShinGo | **Connected** (bool) |
| Seer RDS | `connection_status` (int, 1 = connected) |
| MiR | Status (online/offline) |

### Station

The named map point where the robot is currently located or was last seen.

| System | Term |
|---|---|
| ShinGo | **CurrentStation** / **LastStation** |
| Seer RDS | `rbk_report.current_station` / `rbk_report.last_station` |
| MiR | Position name |
| VDA 5050 | `lastNodeId` |

### Busy

Whether the robot is currently executing an order or task.

| System | Term |
|---|---|
| ShinGo | **Busy** (bool) |
| Seer RDS | `procBusiness` (bool) |
| MiR | Mission status (executing) |

## Operations

### Retry Failed

Re-attempt the current failed operation on a robot. Used after the physical issue causing the failure has been resolved.

| System | Term |
|---|---|
| ShinGo | **RetryFailed** |
| Seer RDS | `POST /redoFailedOrder` |
| MiR | Retry mission |

### Force Complete

Manually mark the robot's current task as finished, skipping whatever operation was in progress. Used when material has been moved by hand or the task is stuck.

| System | Term |
|---|---|
| ShinGo | **ForceComplete** |
| Seer RDS | `POST /manualFinished` |
| MiR | Abort mission (with manual completion) |

### Set Availability

Control whether a robot accepts new dispatch orders.

| System | Term |
|---|---|
| ShinGo | **SetAvailability(bool)** |
| Seer RDS | `POST /dispatchable` with type string |
| MiR | `PUT /robots/{id}/status` |
| VDA 5050 | `instantActions` with `startPause`/`stopPause` |

## Infrastructure Concepts

### Map

The spatial layout data used by robots for navigation. Contains waypoints, paths, and location definitions.

| System | Term |
|---|---|
| ShinGo | **Map** |
| Seer RDS | Scene (contains areas, each with a logical map) |
| MiR | Map |
| VDA 5050 | Map (same) |

### Occupancy

Whether a node location is physically occupied by a payload, as reported by the fleet backend. Used to cross-reference fleet state against ShinGo's own tracking.

| System | Term |
|---|---|
| ShinGo | **Occupancy** / **OccupancyDetail** |
| Seer RDS | Bin status (`GET /binDetails`, `filled` field) |
| MiR | Shelf status |

### Fleet Backend

The vendor-specific robot fleet management system that ShinGo communicates with. ShinGo's `fleet.Backend` interface abstracts over vendor differences.

| System | Term |
|---|---|
| ShinGo | **Fleet Backend** |
| Seer RDS | RDS (Robot Dispatch System) |
| MiR | MiR Fleet |
| Locus Robotics | LocusServer |
| VDA 5050 | Master control |

### Fleet Explorer

The admin tool for sending raw API requests to the fleet backend. Vendor-specific by nature — the endpoint list and request formats are determined by the active backend.

## API Routes

| Route | Purpose |
|---|---|
| `GET /robots` | Robots page |
| `POST /api/robots/availability` | Set robot available/unavailable |
| `POST /api/robots/retry` | Retry failed task on robot |
| `POST /api/robots/force-complete` | Force complete current task |
| `GET /api/nodes/occupancy` | Fleet occupancy cross-reference |
| `GET /api/map/points` | Map infrastructure points |
| `POST /nodes/sync-fleet` | Sync nodes from fleet scene data |
| `GET /fleet-explorer` | Fleet Explorer page |
| `POST /api/fleet/proxy` | Proxy request to fleet backend |

## Naming Conventions

- **Go interfaces and structs** use the ShinGo terms: `store.Bin`, `store.Blueprint`, `store.Payload`, `store.Node`, `RobotStatus.Available`.
- **JSON API fields** use `snake_case`: `bin_type_id`, `blueprint_id`, `manifest_confirmed`, `node_id`, `available`.
- **HTML/CSS classes** use `kebab-case`: `tile-loc`, `occupancy-modal`, `data-available`.
- **Vendor-specific code** (inside `fleet/seerrds/`, RDS client, Fleet Explorer template) uses the vendor's own terminology since it maps directly to their API.
