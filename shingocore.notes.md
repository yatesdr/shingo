# ShinGo - Project Notes

## What Is ShinGo?

ShinGo is the central dispatch server for the ShinGo suite of manufacturing logistics tools. It tracks **payloads** (physical transport units like totes, shelf units, and knock-down containers) across **nodes** (physical plant locations), dispatches AMR robots via SEER RDS, and manages inventory manifests.

## The ShinGo Suite

| Project | Role | Runs On |
|---------|------|---------|
| **plcio** | Pure Go PLC driver library | embedded |
| **ShinGo Link** | PLC-to-IT gateway | edge (one per PLC) |
| **ShinGo Edge** | E-Kanban material dispatch | edge (one per production line) |
| **ShinGo Alert** | PLC alert notifications | edge |
| **ShinGo** | Central dispatch server | server |

## Core Domain Model

### Nodes
Physical locations in the plant (storage racks, line-side locations, staging areas, quality hold). RDS is the master for location naming - bins are defined in RDS and synced into ShinGo. Naming convention uses leading characters for zone type: `A-100` = assembly, `P-200` = press shop, `Q-300` = quality hold, etc.

### PayloadTypes
Reusable templates that define a CLASS of payload. Example: "Job Style A Kit" describes the ideal mix of parts needed to produce N finished goods on a line. Maps directly to ShinGo Edge's `payload_desc` field. Evolves over time as material flows are optimized.

### Payloads
Physical transport units (carts, totes, shelving units, KD containers). **Anonymous and interchangeable** - any cart can become any PayloadType when packed at the origination point. Tracked through the system until relieved of that type. Status lifecycle: `available` → `in_transit` → `at_line` → `empty` → repacked → `available`.

### Manifests
The actual contents of a specific payload. Line items with ERP-compatible fields (part number/cat-id, quantity, production date, lot code). Kept flexible to accommodate evolving ERP integration (Epicor CMS). Updated by ShinGo Edge (consumption estimates), manual count stations (partial returns), and warehouse (replenishment).

## How It Works

### Retrieve Flow (warehouse → line)
1. ShinGo Edge PLC counter ticks → parts consumed → remaining drops below reorder point
2. ShinGo Edge sends `order_request` to ShinGo via MQTT/Kafka with `payload_desc`, `delivery_node`, `staging_node`
3. ShinGo finds an available payload matching the requested type at a storage node
4. ShinGo creates an RDS join order (fromLoc → toLoc) to dispatch an AMR
5. Robot picks up payload, delivers to line-side
6. ShinGo Edge confirms receipt, manifest updated

### Store Flow (line → warehouse)
1. ShinGo Edge sends `storage_waybill` for empty/partial payload
2. If partial: human counts remaining parts at count station
3. ShinGo selects storage node with capacity, dispatches AMR
4. Payload returns to storage, becomes available for repacking

## Architecture Decisions

- **RDS = fleet manager, ShinGo = logistics brain.** RDS handles robot navigation and fleet health. ShinGo owns payload tracking, inventory, and dispatch decisions. This avoids vendor lock-in.
- **Dual database**: SQLite for development, PostgreSQL for production. Dialect abstraction via `store/dialect.go`.
- **Write-through cache**: SQL is source of truth, Redis for fast reads. Updated after every SQL commit.
- **RDS bins are master for location naming.** ShinGo syncs FROM RDS, never the reverse.
- **Payloads are anonymous.** No permanent cart IDs. Tracked by type + location, not individual identity.
- **Manifest flexibility.** Schema kept loose to evolve with ERP integration needs.

## RDS Integration

SEER RDS Core API: HTTP on port 8088, WebSocket on 8089. No authentication.

Key endpoints used:
- `/robotsStatus` - robot positions, battery, state, containers
- `/binDetails` - bin occupancy and holder status
- `/setOrder` - create join orders (pickup → delivery)
- `/orderDetails/{id}` - order status polling
- `/terminate` - cancel orders
- `/dispatchable` - pause/resume robots
- `/scene` - plant layout (areas, robot groups, doors, lifts)
- `/ping` - health check

## ShinGo Edge Message Types

### Outbound (ShinGo Edge → ShinGo)
- `order_request` - retrieve/move order (payload_desc, delivery_node, staging_node, quantity)
- `storage_waybill` - store order (pickup_node, final_count)
- `delivery_receipt` - delivery confirmed (final_count)
- `order_cancel` - abort order
- `redirect_request` - change delivery node mid-flight

### Inbound (ShinGo → ShinGo Edge)
- DispatchReply: ack / waybill / update / delivered (with waybill_id, eta, status_detail)

## Tech Stack

- **Language**: Go 1.24
- **Web**: chi router, HTMX, SSE, gorilla/sessions
- **Database**: SQLite (modernc.org/sqlite) / PostgreSQL (pgx)
- **Cache**: Redis (go-redis)
- **Messaging**: MQTT (paho) / Kafka (segmentio/kafka-go)
- **Templates**: Go html/template with Clone()-per-page pattern
- **Build**: Makefile + GitHub Actions (linux/darwin amd64+arm64, windows amd64)

## Planned Features

### Phase 1: Foundation
1. Auto-populate nodes from RDS bins
2. PayloadType CRUD (reusable templates with default manifests)
3. Payload CRUD & assignment (create, assign type, assign to node, track status)
4. Manifest management (flexible line items, ERP-compatible fields)

### Phase 2: Integration
5. Bin occupancy sync (reconcile ShinGo state with RDS bin status)
6. Robot controls in UI (pause/resume, retry failed, manual finish)
7. Order lifecycle controls (terminate, reprioritize)
8. Scene/area sync (auto-populate zones from RDS areas)

### Phase 3: Advanced
9. WebSocket real-time (replace polling with RDS WebSocket push)
10. ERP integration (Epicor CMS - part master, lot tracking, inventory reconciliation)
