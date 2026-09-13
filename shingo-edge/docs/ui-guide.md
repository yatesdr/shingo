# UI Guide

Shingo Edge provides a browser-based operator interface at `http://<host>:8081`. All pages support light and dark mode via the theme toggle in the navigation bar.

## Operator Pages

These pages are the primary interface for production line operators.

### Material

**Route:** `/material`

Displays the current material inventory for the station's production lines. Each payload shows:

- Payload description and manifest contents
- Current UOP remaining and total capacity
- Reorder status (threshold, auto-reorder enabled/disabled)
- Active anomaly flags from PLC counter jumps (requiring operator confirmation)

When PLC integration is active, UOP remaining decrements automatically as production counters advance. If UOP remaining drops below the configured reorder threshold and auto-reorder is enabled, a replacement bin is ordered without operator intervention.

### Orders

**Route:** `/orders`

Lists all active transport orders for this station with real-time status updates. For each order, the page shows:

- Order type (retrieve, store, move) and requested payload
- Current status in the order lifecycle
- Assigned robot and ETA (when available)

Operator actions:
- **Confirm delivery** — Acknowledge physical receipt of a delivered bin and enter the final count
- **Redirect** — Change the delivery destination of an in-flight order
- **Cancel** — Abort an order from any non-terminal state

### Manual Order

**Route:** `/manual-order`

Create transport orders manually. Available order types:

- **Retrieve** — Pull material from warehouse storage to a line-side node
- **Store** — Return material from the line to warehouse storage
- **Move** — Relocate material between two specified nodes

The form populates available nodes and payload types from the station's configuration and the payload catalog synced from core.

### Changeover

**Route:** `/changeover`

Manages the production line changeover workflow when switching from one job style to another.

**Starting a changeover:**
1. Select the target job style (the new product to produce). A style whose material cannot be sourced is rendered disabled, with the sourcing status and note beside its name.
2. Click "Preview" to see the orders that would be created, or "Start Changeover" to begin.

**The changeover itself has three states, not a step sequence.** The `process_changeovers` row is `active` from the moment it is created, and moves once, to either `completed` or `cancelled` — `shingo-edge/domain/changeover_state.go:19-21`. The row is inserted `'active'` at `shingo-edge/service/changeover_service.go:61`; `completed` is written by the cutover at `shingo-edge/engine/operator_changeover_cutover.go:318` and `cancelled` by "Cancel Changeover" at `shingo-edge/engine/operator_changeover_cancel.go:75`. Both are terminal (`ChangeoverState.IsTerminal`). The state appears on the page as a plain badge next to `From Style → To Style`; there is no "Advance" button anywhere on this page.

The work the operator does is **per node**, not per changeover. While the changeover is `active` the page shows one table per operator station (plus a table of central nodes), each row a node with its From payload, its To payload, and a Progress column carrying a three-step ladder:

**1. Stage → 2. Evacuate → 3. Deliver.**

Which button is live is decided by that node's `changeover_node_tasks` state (`shingo-edge/domain/changeover_node_state.go:19-71`); the rendering is `shingo-edge/www/templates/partials/node-actions.html`:

| Node task state | What the operator sees |
|-----------------|------------------------|
| `swap_required` | **1. Stage** live; **Skip** |
| `staging_requested` | "1. Stage…" — the staging order is out |
| `staged` | Stage ✓, **2. Evacuate** live; **Skip** |
| `empty_requested` | "2. Release…" — the evacuation order is out |
| `line_cleared` | Stage ✓, Release ✓, **3. Deliver** live; **Skip** |
| `release_requested` | "3. Deliver…" — the delivery order is out |
| `released` / `switched` / `verified` | all three ✓, "Complete" |
| `unchanged` | all three disabled, "No change needed" |
| `error` | **Retry** (for a drop situation, **Retry Evac** — a drop has no staging leg); **Skip** |
| `capacity_blocked` | "Waiting for downstream capacity…", **Retry Evac**, **Skip**. Core's fulfillment scanner replays this on its own when capacity opens |
| `awaiting_material` | "Waiting for material at the line…", **Abandon**, **Accept Half-Swap**. Core parked the supply order because the pool at the node is dry; it un-parks itself when material arrives |
| `abandoned` | "Supply abandoned" |
| `cancelled` | "Cancelled" |

**Setup Done — Release Material** releases every leg of the changeover that is holding at inbound staging, in one click, instead of one release per order from the Orders page.

**Complete Cutover** is the operator's control for ending the changeover. It stays disabled until every node task is terminal; while it is disabled, a panel above the tables reads "Cutover is waiting on *N* items" and names each blocker, with the node and order id. Both conditions are checked by `canCompleteChangeover` (`shingo-edge/engine/operator_changeover_cutover.go:29-36`): every node task terminal, and every order those tasks reference terminal. There is deliberately no override — an out-of-order style flip is unrecoverable, so the panel's job is visibility, not permission.

The same gate also runs unattended: `tryCompleteProcessChangeover` fires on terminal-state events, so a changeover whose last node finishes while nobody is watching completes on its own. The button is the operator's path to the same check, not a separate one.

Each station card also carries a **Complete Station** button and its own state badge (`waiting` / `in_progress` / `switched` — `shingo-edge/domain/changeover_station_state.go:19-22`).

Cutover also flips the line's active style, clears the process's target style, and returns its `production_state` from `changeover_active` to `active_production`. **Cancel Changeover** — offered only while the row is `active` — clears the target style and returns `production_state` the same way, but leaves the active style where it was (`shingo-edge/engine/operator_changeover_cancel.go:75-88`). Cancelling can hand off directly into a new changeover to a different style; that opens a fresh row, not a continuation of this one.

### Production

**Route:** `/production`

Displays production count data from PLC reporting points. Features:

- Hourly production counts displayed as a bar chart
- Filtering by date and job style
- Shift boundary indicators (when shifts are configured)

## Administration Pages

These pages require authentication.

### Setup

**There is no `/setup` page.** Edge station configuration is a set of separate
pages, each with its own route (`shingo-edge/www/router.go`):

| Page | Route |
|---|---|
| Production lines | `/processes` |
| Job styles and their node claims | `/styles` |
| Process nodes | `/process-nodes` |
| Process groups | `/process-groups` |
| Payload catalog | `/payload-catalog` |
| Reporting points | `/reporting-points` |
| WarLink / PLCs | `/plcs` |
| Shifts | `/shifts` |
| Operator stations | `/operator-stations` |
| Core node list | `/core-nodes` |
| Replenishment | `/replenishment` |
| Kanbans | `/kanbans` |
| App config, including **Backups** | `/config` |

What each covers:

**Production Lines** (`/processes`) — Define the production lines (processes) managed by this station. Each line has a name and identifier.

**Job Styles** (`/styles`) — Define the product types (styles) produced on each line. One style is active per line at a time. Switching the active style is handled through the changeover workflow. This is also where a style's node claims are edited — swap mode, staging, source and destination.

**Payloads** (`/payload-catalog`) — Configure the payload templates available at this station. For each payload, set:
- Description and manifest (parts list)
- UOP capacity
- Reorder threshold and auto-reorder toggle
- Associated production line

The payload catalog is synced automatically from Shingo Core. Local configuration adds station-specific settings such as reorder thresholds.

**Reporting Points** (`/reporting-points`) — Bind PLC counter tags to job styles for automated production counting. Each reporting point specifies:
- PLC name (discovered from WarLink)
- Tag name (counter tag on the PLC)
- Associated job style

**WarLink / PLC** (`/plcs`) — View discovered PLCs and connection status. Test individual tag reads. Configure the WarLink connection address.

**Shifts** (`/shifts`) — Define up to three shift periods per day with names and time windows. Shifts are used for bucketing production counts on the Production page.

### Diagnostics

**Route:** `/diagnostics`

System health and connectivity information:

- Kafka connection status
- WarLink / PLC connectivity
- Edge registration status with core (active, stale, or unregistered)
- Last heartbeat timestamp

### Manual Message

**Route:** `/manual-message`

Diagnostic tool for sending arbitrary protocol messages to core. Intended for testing and troubleshooting only.

## Authentication

**Route:** `/login`

On first visit, the credentials entered become the admin account. Subsequent visits authenticate against that account. Sessions are maintained via HTTP-only cookies with a 7-day expiration.

## Real-Time Updates

Most pages receive live updates via Server-Sent Events (SSE). Order status changes, material consumption updates, and changeover state transitions appear automatically without page refresh.
