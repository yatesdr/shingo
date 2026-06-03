# AMR Task Board

Live real-time view of all active AMR orders and robot assignments. Accessible from the Core web UI at `/board`.

---

## Overview

The Task Board is a shop-floor-facing page that shows every in-flight AMR order in a single table. It updates in real time via SSE (Server-Sent Events) — no page refresh needed.

Each row represents one active order with an assigned robot:

| Column     | Source                                  | Description                          |
|------------|----------------------------------------|--------------------------------------|
| Robot      | `robot_id`                             | AMR vehicle assigned to the order    |
| Source     | `source_node`                          | Pickup node name                     |
| Payload    | `payload_code`                         | Material identifier                   |
| Current    | `current_station` (from robot cache)    | Where the robot is right now         |
| Dest       | `delivery_node`                        | Drop-off node name                   |
| Status     | `status`                               | Current order state                  |
| ETA        | Computed from ETA cache on in_transit  | Estimated arrival time (relative)    |

---

## Architecture

### Data Flow

```
Browser ──SSE──▶ Core /events endpoint
                 │
                 ├─ status_changed ──▶ update/flash row
                 ├─ eta_update ──────▶ patch ETA cell
                 ├─ dispatched ───────▶ fetch full order, add row
                 ├─ queued ───────────▶ fetch full order, add row
                 ├─ completed ────────▶ remove row (red flash)
                 ├─ failed ───────────▶ remove row (red flash)
                 ├─ cancelled ────────▶ remove row (red flash)
                 └─ skipped ──────────▶ remove row (red flash)
```

### Backend

- **`engine_board.go`** — `BoardOrder` struct and `GetActiveOrdersWithRobotLocation()` / `GetActiveOrderWithRobotLocation()` queries. Enriches DB orders with robot current-station from the in-memory robot cache and ETA from the ETA cache.
- **`store/orders/orders.go`** — `ListActiveBoard()` SQL: excludes terminal statuses (`confirmed`, `failed`, `cancelled`, `skipped`) and orders without an assigned robot. Ordered oldest-first.
- **`handlers_board.go`** — HTTP handlers: `GET /board` (page), `GET /api/board/orders` (JSON list), `GET /api/board/orders?id=N` (single order lookup). Returns `200 + null` for terminal orders so the frontend can clean up ghost rows.
- **`sse.go`** — SSE listener for `EventOrderStatusChanged` broadcasts `status_changed` immediately (synchronous). When status is `in_transit`, a goroutine fetches the order's source/dest from DB and broadcasts a separate `eta_update` event — this avoids blocking the event emitter on DB I/O. The goroutine checks the order is still `in_transit` before broadcasting to avoid stale ETA for orders that transitioned again mid-flight.
- **`sse.go`** — `EventOrderSkipped` listener broadcasts `skipped` so the board can remove skipped orders immediately.

### Frontend

- **`board.js`** — Vanilla JS (no framework). Connects to `/events` SSE stream, fetches initial order list on the `connected` event, and incrementally updates rows on each `order-update` event. Flash animations highlight additions (green) and removals (red). Station and status filters are dynamically populated from the order data and persist across reconnects via a `knownStations`/`knownStatuses` set.
- **`board.html`** — Minimal template: header with connection indicator and filter dropdowns, table with `<tbody id="board-body">`, empty-state message.
- **`style.css`** — Board styles scoped under `.board-page`. Dark theme by default with full `[data-theme="light"]` overrides for all text, backgrounds, borders, and status colors.

### Key Design Decisions

**ETA computed on the server, not in JS.** The ETA cache lives in Core (dispatch package). `eta.Stamp()` returns an ISO timestamp of the estimated arrival. JS only formats it as a relative duration ("5m", "2h15m", "arriving").

**Terminal status removal is SSE-driven, not poll-driven.** Initial load fetches all active orders. After that, only SSE events update the board. No polling interval.

**Goroutine for ETA to avoid blocking the event bus.** The EventBus is synchronous — subscribers run on the emitting goroutine. The DB call for ETA would block engine wiring. The `status_changed` broadcast fires immediately; ETA follows asynchronously.

**Filters use in-memory sets, not DOM length checks.** `knownStations` and `knownStatuses` track which options have been added to the dropdowns. New stations or statuses that appear after initial load (via SSE reconnect) are added correctly.

**Exponential backoff on SSE reconnect.** Starts at 3s, doubles each failure, caps at 30s. Resets to 3s on successful connection.

---

## Files

| File                                        | Role                              |
|---------------------------------------------|-----------------------------------|
| `engine/engine_board.go`                    | BoardOrder struct + queries        |
| `store/orders/orders.go`                    | ListActiveBoard SQL                |
| `store/orders.go`                           | Delegate                          |
| `engine/engine_accessors.go`                | EtaCache() accessor                |
| `www/engine_iface.go`                       | ServiceAccess interface methods    |
| `www/handlers_board.go`                     | HTTP handlers                      |
| `www/router.go`                             | Route registration                 |
| `www/sse.go`                                | SSE listeners (status + ETA)      |
| `www/static/pages/board.js`                 | Frontend logic                     |
| `www/templates/board.html`                  | Page template                      |
| `www/templates/layout.html`                 | Nav link added                     |
| `www/static/style.css`                      | Board + light-theme CSS            |
