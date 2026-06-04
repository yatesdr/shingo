# Dashboard Platform

Floor-facing **dashboards** for shingo-core: saved, named, station-scoped views
of Core's live data, each rendered as a chromeless full-screen page you point a
wall monitor at. The AMR Task Board is the first dashboard *kind*; the platform
is built to host more (throughput, andon, a robot map, …) without growing Core's
nav or its binary surface.

A dashboard is **pure presentation** — it owns no operational data beyond its
own definition. It reads Core's existing public order API and SSE stream; the
data owner (Core) serves its own displays, the same way shingo-edge serves its
operator HMIs from the edge binary.

---

## Model

A dashboard is a row in the `dashboards` table:

| Field           | Meaning                                                             |
|-----------------|--------------------------------------------------------------------|
| `name`          | Display title (shown on the board header, in the admin list)       |
| `kind`          | Renderer selector. v1: `task-board`                                |
| `stations_json` | The **area filter** — a JSON list of station IDs; empty = whole plant |
| `config_json`   | Per-kind options, opaque to the platform (reserved; unused in v1)  |
| `enabled`       | Organizational flag (does not gate the display in v1)              |
| `sort_order`    | Stable ordering in the admin list                                  |

Two surfaces:

- **Display** (public, chromeless): `GET /dashboard/{id}` — full-screen kiosk
  page for a monitor. No nav, no login. Config is baked into the page
  server-side; the page's JS pulls live data scoped to this dashboard.
- **Admin** (auth-gated): `GET /dashboards` — create / edit / delete dashboards
  and copy each one's display link. Lives in the **Admin** nav dropdown.

The old `/board` tab is gone; `/board` 301-redirects to `/dashboards`.

---

## Architecture

### Data flow

```
            ┌─────────────────────── Core ───────────────────────┐
Wall        │                                                     │
monitor ──▶ GET /dashboard/{id}      (chromeless page, config baked in)
  │         │                                                     │
  │ JS pulls scoped data:                                         │
  ├──────▶  GET /api/board/orders?dashboard={id}                  │
  │            └─ engine.GetActiveOrdersWithRobotLocationFiltered │
  │               └─ store: WHERE station_id IN (dashboard.stations)
  │                                                               │
  └── SSE  GET /events  ──(any order-update)──▶ debounced refetch │
            │              ("change-ping", see below)             │
            └─────────────────────────────────────────────────────┘
```

### Server-side scoping + SSE as a change-ping

Core's SSE hub is a **plant-wide broadcast** with no per-connection filtering,
and order events carry only `order_id` (not `station_id`). So area-scoping lives
in the **REST query**, not the event stream:

- `GET /api/board/orders?dashboard={id}` resolves the dashboard's station set and
  filters server-side (`store.ListActiveBoardFiltered` → `station_id IN (...)`).
- The display treats SSE purely as a **change-ping**: on any `order-update` it
  schedules a debounced (250 ms) refetch of its scoped list. No per-event row
  diffing; each board pulls only its own area's slice. Cost is a small full-list
  refetch per change — cheap at board scale, and far simpler than threading
  station context through every event type.

This is the one design point the shingo-edge analogy doesn't cover: edge is
single-station, so scoping is implicit; Core is multi-station, so scoping is real
work and belongs in the query.

### Backend

- **`store/schema/postgres_ddl.go`** — `dashboards` baseline table (fresh DBs).
- **`store/migrations.go`** — `v27` creates `dashboards` on existing DBs (idempotent, verified by `TableExists`).
- **`store/dashboards/dashboards.go`** — CRUD on `*sql.DB`. JSON-backed columns keep the schema flat; `marshalInput` validates config JSON at write time.
- **`store/orders/orders.go`** — `ListActiveBoardFiltered(stations)`: the board query with a positional-placeholder `IN (...)` station filter. Empty = unscoped.
- **`service/dashboard_service.go`** — `DashboardService`: CRUD wrapper + input normalization (trim name, default `kind`, de-dup stations).
- **`engine/engine_board.go`** — `GetActiveOrdersWithRobotLocationFiltered(stations)`; the unscoped method delegates to it with `nil`.
- **`www/handlers_dashboards.go`** — display handler (kind→template registry), admin page handler, and the CRUD/read API. `handlers_board.go` extends `/api/board/orders` with `?dashboard=`.
- **`www/router.go`** — routes + `renderBare` (executes a standalone template, not the nav `layout`, for chromeless pages).

### Frontend

- **`templates/dashboard-display.html`** — chromeless `<!DOCTYPE>` kiosk page; bakes in `data-dashboard-id` / `data-dashboard-kind`.
- **`static/pages/dashboard.js`** — display renderer (task-board kind): scoped fetch + change-ping refetch, reconnect backoff, clock, connection dot, and build-id auto-reload (a kiosk adopts a new Core build by reloading).
- **`static/dashboard.css`** — self-contained dark kiosk styling; large fonts and status color-coding for across-the-aisle legibility.
- **`templates/dashboards.html` + `static/pages/dashboards.js`** — admin CRUD (built with `el()` + real handlers; no inline events / no `data-action` strings).

---

## Implemented kinds

### `task-board`

The live order table (see Architecture above). Data: `/api/board/orders?dashboard=<id>`.

### `robot-map`

A spatial plant view: scene nodes laid out by their world coordinates, live robot
positions, and this dashboard's active orders color-coded by status. All data is
already public — **no backend was added** for this kind:

- **Layout** — `GET /api/map/points` (`scene_points`: `pos_x` / `pos_y` / `dir` / `label`).
- **Live robots** — the `robot-update` SSE feed, seeded once by `GET /api/robots`.
  The renderer normalizes both shapes (SSE lowercase tags vs the REST struct's Go
  field names) and derives robot `state` to match the SSE coloring.
- **Active orders** — the scoped board API. A robot working one of this dashboard's
  orders takes the order's **status color**; otherwise it shows its own **state color**.

Rendering is SVG with world coords mapped into the `viewBox`; the plant's long axis
is auto-oriented to fill a landscape monitor. Nodes are colored and sized by their
scene `class_name` — confirmed live as `ActionPoint`, `ChargePoint`, `ParkPoint`,
`LocationMark` (travel nodes), `GeneralLocation` — with action points enlarged and
named and the numerous travel nodes kept small (a background path network).
Verified against live Hopkinsville data:
robots share the scene-point coordinate frame (so node/robot alignment is correct),
and the fleet `Angle` is **radians** (converted to degrees for the heading marker).
The one remaining best-effort path is destination highlighting / route lines, which
depend on an order's node name resolving to a scene point
(`point_name` / `label` / `instance_name`); robot color-by-status is robust
regardless (it joins on `robot_id`). Area-scoping the *geometry* (vs. only the order
highlights) is a future option via the `?area=` filter `/api/map/points` already supports.

---

## Extending: adding a dashboard kind

The platform is kind-agnostic. The **robot-map** kind above was added exactly this
way — no schema, nav, or service change:

1. **Pick the data.** It already exists on Core's public surface — node geometry
   from `GET /api/map/points` (`scene_points`), live robot X/Y/heading from the
   `robot-update` SSE event, and active orders from `GET /api/board/orders`
   (with `source_node` / `delivery_node` to draw/highlight routes).
2. **Add a renderer template** (e.g. `templates/dashboard-map.html`) and
   register it in `handlers_dashboards.go`:
   ```go
   var dashboardTemplates = map[string]string{
       "task-board": "dashboard-display.html",
       "robot-map":  "dashboard-map.html",   // new
   }
   ```
3. **Add the renderer JS** — branch on `data-dashboard-kind` in `dashboard.js`
   (or a separate module) and draw onto a canvas/SVG. Reuse the same scoping +
   SSE-as-ping pattern; the `robot-update` event is your live position feed.
4. **List the kind** in `dashboards.js` `KINDS` so the admin form offers it.

No schema change, no nav change, no new service — `kind` + `config_json` carry
the variation. That's the platform's whole reason for existing.

---

## Future / not yet

- **Standalone display host.** Today the platform lives in the Core binary. The
  display's client talks only to public endpoints (`/api/board/orders`,
  `/api/dashboards`, `/events`), so it can be lifted into its own service later
  *if* operational pain shows up (dashboard tweaks shouldn't require redeploying
  the dispatcher; many monitors shouldn't each hold a full-firehose SSE link).
  Until then, in-core is correct — a dashboard owns no data of its own, so the
  data owner serves it.
- **Station picker.** The admin area filter is a comma-separated text input; a
  picker sourced from known stations would be a nicety.
- **`enabled` gating + `config_json` UI** are reserved but inert in v1.

---

## Files

| File                                          | Role                                   |
|-----------------------------------------------|----------------------------------------|
| `store/schema/postgres_ddl.go`                | `dashboards` baseline DDL              |
| `store/migrations.go`                         | v27 migration                          |
| `store/dashboards/dashboards.go`              | dashboard CRUD persistence             |
| `store/dashboards_test.go`                    | CRUD + normalization test (`docker`)   |
| `store/orders/orders.go`                      | `ListActiveBoardFiltered` station scope |
| `store/orders.go`                             | delegate                               |
| `service/dashboard_service.go`                | service + input normalization          |
| `engine/engine_board.go`                      | filtered board query                   |
| `engine/engine_accessors.go`                  | `DashboardService()` accessor          |
| `www/engine_iface.go`                         | `ServiceAccess` additions              |
| `www/handlers_dashboards.go`                  | display + admin + CRUD/read API        |
| `www/handlers_board.go`                       | `?dashboard=` scoping                  |
| `www/router.go`                               | routes + `renderBare`                  |
| `www/templates/dashboard-display.html`        | chromeless kiosk page (task-board)     |
| `www/templates/dashboard-map.html`            | chromeless kiosk page (robot-map)      |
| `www/templates/dashboards.html`               | admin page                             |
| `www/static/pages/dashboard.js`              | task-board renderer                    |
| `www/static/pages/dashboard-map.js`          | robot-map renderer (SVG)               |
| `www/static/pages/dashboards.js`             | admin CRUD                             |
| `www/static/dashboard.css`                    | kiosk styling (both kinds)             |
| `www/templates/layout.html`                   | nav: Task Board tab → Dashboards (Admin) |
