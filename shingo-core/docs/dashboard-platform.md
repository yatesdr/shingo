# Wall Display Platform

> **Naming, and where the seam is.** Owner decision 11 renamed the human-facing
> concept to **wall displays**, so the PAGE routes are `/wall-display/{id}` and
> `/wall-displays`. Everything else deliberately kept the old name: the
> `dashboards` table, the `/api/.../dashboards` namespace, `DashboardService`,
> `domain.Dashboard`. A page rename is undoable with a redirect; an API rename
> is not, and a table name is not something a passer-by reads off a screen.
> Expect both words in this file — that is the state of the code, not drift.

Floor-facing **wall displays** for shingo-core: saved, named, station-scoped
views of Core's live data, each rendered as a chromeless full-screen page you
point a wall monitor at. The AMR Task Board was the first *kind*; there are four
now (`task-board`, `robot-map`, `heartbeat`, `node-report`), hosted without
growing Core's nav or its binary surface.

A dashboard is **pure presentation** — it owns no operational data beyond its
own definition. It reads Core's existing public order API and SSE stream; the
data owner (Core) serves its own displays, the same way shingo-edge serves its
operator HMIs from the edge binary.

---

## Model

A dashboard is a row in the `dashboards` table:

| Field           | Meaning                                                             |
|-----------------|--------------------------------------------------------------------|
| `name`          | Display title. **Every kiosk template renders it in its own header** — a chromeless screen has to say what it is |
| `kind`          | Renderer selector: `task-board`, `robot-map`, `heartbeat`, `node-report` |
| `stations_json` | The **area filter** — a JSON list of station IDs; empty = whole plant |
| `config_json`   | Per-kind options, opaque to the platform (`node-report` stores `loader_id` here) |
| `enabled`       | Gates seeding and the kind-resolution redirects; does not gate a direct link |
| `sort_order`    | Stable ordering, and the tie-break for "first display of this kind"  |

### `?kiosk=1` is the load-bearing part of the URL

`GET /wall-display/{id}` serves **two different pages**:

- **bare** — framed inside Core's chrome (nav stays, plus a Fullscreen link),
  which is what a person clicking from the hub should get;
- **`?kiosk=1`** — the chromeless page itself, which is what a wall monitor is
  pointed at, and what the frame loads in its iframe.

So any redirect that lands on this route **must carry the query string**.
Dropping it does not 404 and does not error; it quietly returns a framed page
with a nav bar to a screen bolted to a wall. `GET /dashboard/{id}` 301s here
preserving the query for exactly this reason
(`TestWallDisplayMoved_PreservesQueryString`), and `/heartbeat`'s redirect had
the same bug in the other direction — it never attached `?kiosk=1` at all.

### Where they are managed

There is **no admin page**. Refactor #3 retired the standalone Manage table;
wall displays are created and edited on the **hub at `/`** (the Dashboard page,
`static/pages/dashboard-landing.js`). `GET /wall-displays` and `GET /dashboards`
both redirect there. The retired `templates/dashboards.html` +
`static/pages/dashboards.js` pair was deleted once nothing referenced it but the
template-parse test.

The old `/board` tab is gone; `/board` 301-redirects to `/dashboards`, which
then redirects to `/`.

### Defaults are seeded, not created

`engine.Start()` calls `DashboardService.SeedDefaultDashboards()`, which creates
an enabled whole-plant display for any of `heartbeat` / `task-board` /
`robot-map` that has none — "Plant Heartbeat", "Plant Flight Board", "Plant
Robot Map". Idempotent and per-kind, so it never clobbers curation.

Two consequences worth knowing before reasoning about this table: **every Core
has a heartbeat display from first boot** (so code branching on "does one
exist?" always takes the yes arm), and **most rows at a plant are seed rather
than curation** — at the 2026-07-27 measurement Springfield had 4 rows and
Hopkinsville 3, both carrying "Plant Heartbeat" under the seed's exact name.

---

## Architecture

### Data flow

```
            ┌─────────────────────── Core ───────────────────────┐
Wall        │                                                     │
monitor ──▶ GET /wall-display/{id}?kiosk=1   (chromeless page, config baked in)
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
- **`www/handlers_dashboards.go`** — display handler (kind→template registry), the `/dashboard/{id}` → `/wall-display/{id}` 301 (query-preserving), the hub redirects, and the CRUD/read API. `handlers_board.go` extends `/api/board/orders` with `?dashboard=`.
- **`www/router.go`** — routes + `renderBare` (executes a standalone template, not the nav `layout`, for chromeless pages).

### Frontend

- **`templates/dashboard-display.html`** — chromeless `<!DOCTYPE>` kiosk page; bakes in `data-dashboard-id` / `data-dashboard-kind`.
- **`static/pages/dashboard.js`** — display renderer (task-board kind): scoped fetch + change-ping refetch, reconnect backoff, clock, connection dot, and build-id auto-reload (a kiosk adopts a new Core build by reloading).
- **`static/dashboard.css`** — self-contained dark kiosk styling; large fonts and status color-coding for across-the-aisle legibility.
- **`static/pages/dashboard-landing.js`** — the hub on `/`: cards, create/edit modal, and the Open / Fullscreen links.

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
4. **List the kind** in `dashboard-landing.js` `KINDS` so the hub's create/edit
   form offers it. (This step named `dashboards.js` until that file was deleted
   — it was the retired admin CRUD, and editing it changed nothing on screen.)

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
| `www/static/pages/dashboard.js`              | task-board renderer                    |
| `www/static/pages/dashboard-map.js`          | robot-map renderer (SVG)               |
| `www/static/pages/dashboard-landing.js`      | the hub on `/` (where displays are made) |
| `www/static/dashboard.css`                    | kiosk styling (all kinds)              |
| `www/templates/layout.html`                   | nav: "Dashboard" → `/` (the hub)         |
