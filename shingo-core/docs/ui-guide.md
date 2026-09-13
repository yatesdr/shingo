# UI Guide

ShinGo Core provides a web-based management interface at `http://<host>:8083`. All pages support light and dark mode via the theme toggle in the navigation bar.

This guide covers the pages an operator or engineer reaches from the nav. It is
not the full route list — `shingo-core/www/router.go` registers around forty-six
`GET` pages, including narrower diagnostic surfaces (`/orphans`,
`/reconciliation`, `/cms-health`, `/buckets`, `/footprint`, `/cycle-time`,
`/material-flags`, …). The router is the roster; this is the tour.

## Public Pages

These pages are accessible without authentication.

### Dashboard

**Route:** `/`

The main landing page. Shows a real-time overview of system status including active orders, node utilization, fleet status, and recent activity. Data updates automatically via server-sent events (SSE).

The Core Health strip carries a **Faulted** figure counting orders past
`rds.fault_notice_after`; the total faulted right now is in the tile's title.
Only those past the threshold colour the verdict — most faults are a 20-second
replan, and a strip that goes amber for those is amber most of the day. The same
figure is on `/api/core/health` as `faulted_notice` / `faulted_now`.

<!-- screenshot:dashboard -->
![Dashboard](screenshots/dashboard.png)
<!-- /screenshot -->

### Nodes

**Route:** `/nodes`

Lists all registered nodes (physical locations) with their type, zone, capacity, and current payload count. Supports filtering by node type and zone. Click a node to view its details and current inventory.

Admin actions (authenticated):
- Create, edit, and delete nodes
- **Sync from Fleet** — the one button on the page (`www/templates/nodes.html:9`), posting to `/nodes/sync-fleet`

The zone-from-areas sync (`POST /nodes/sync-scene`, `www/router.go:568`) exists in the
backend but has no UI trigger — no template or page script references it.

<!-- screenshot:nodes -->
![Nodes](screenshots/nodes.png)
<!-- /screenshot -->

### Orders

**Route:** `/orders`

Lists all transport orders with status, type, source/destination, and timestamps. Click an order row to view full details including the order timeline.

Rows refresh in place rather than reloading the page, with a 30-second sweep as a
backstop for a dropped event. The text filter and the scroll position survive a
refresh.

**Faulted orders** carry a sentence under the badge, in the same slot a queued
order uses for its wait reason:

- Under `rds.fault_notice_after` (default 60s) it reads **Replanning · 14 s**.
  Most faults are a robot re-planning and clear on their own in about 20
  seconds; they are deliberately not called faults, and the fleet's own reason
  is withheld with the word.
- At or past the threshold it reads **Fault · cannot replan (60011) · 3m 12s ·
  gives up in 41m** — the fleet's reason as RDS gave it, the elapsed time, and
  the countdown to `rds.fault_grace`.

Both durations tick in the browser, and the sentence changes from Replanning to
Fault on its own as the clock crosses the threshold.

The summary chip row counts only faults past the threshold; a replan raises no
chip, because a chip that fires on every fault stops being read. A **Faulted**
status filter sits with the other status pills.

<!-- screenshot:orders -->
![Orders](screenshots/orders.png)
<!-- /screenshot -->

### Order Detail

**Route:** `/orders/detail?id=<ID>`

Full detail view for a single order. Shows order metadata, the assigned robot, vendor order ID, and a timeline of all status changes from the audit log.

A faulted order's sentence appears under the hero in the reason slot — not the
red error slot, which belongs to an order that has actually ended.

The timeline shows, on each faulted row, the fleet's reason and how long the
order stayed faulted; the open fault's dwell is live. A `grace_timeout` terminal
row carries the reason the order faulted for, so a failed order says why rather
than only that it timed out.

Admin actions (authenticated):
- Terminate order (cancels with fleet)
- Set priority

<!-- screenshot:order-detail -->
![Order Detail](screenshots/order-detail.png)
<!-- /screenshot -->

### Robots

**Route:** `/robots`

Live status of all robots in the fleet. Shows each robot's connection status, availability, current station, battery level, and whether it's busy. Click a robot tile to see detailed status and access controls.

A tile also names the order the robot is on: `on #4412` normally, `on #4412 ·
replanning` under the fault threshold, and a warning chip `on #4412 · fault
<live clock>` past it. Text, never the tile's colour — the tile's colour is the
robot's state, and a stuck order is not a stuck robot.

Robot alarms are deliberately absent from this line. Reflector and localization
alarms fire constantly, so an alarm printed beside a fault reads as its cause.

Admin actions (authenticated):
- Set robot available/unavailable
- Retry failed task
- Force complete current task

<!-- screenshot:robots -->
![Robots](screenshots/robots.png)
<!-- /screenshot -->

### Demand episodes

**Route:** `/demand-episodes`, and `/demand-episodes/{originID}` for one episode

There is no `/demand` page. The quota interface it described went with its table
at migration v106 (`shingo-core/www/router.go:158-159`). The demand grain is now
the **episode**: `/demand-episodes` lists open and recent episodes, and
`/demand-episodes/{originID}` is the origin-indexed forensic view — one demand
and every order it spawned, each linking to its `/missions/{orderID}` detail.

### Missions

**Route:** `/missions`

Analytical drill page for completed work: the mission list, dwell per state,
breakdowns by robot and route, and the failure Pareto. All respect the filter
bar.

The **Faults** card answers what a faulted order does next. Faults per day are
split replanning vs fault by `rds.fault_notice_after`, because the totals alone
say "730 faults a month" and hide the two dozen that mattered. Beside the split:
the outcome breakdown (recovered / cancelled / gave up / still faulted) with
p50 and p95 dwell for each, and top-10 tables by robot, by node, and by the
fleet's own reason code. Counts sit beside every percentile, and no data shows
as an em-dash rather than a zero.

The card is generic — whichever plant runs it, its own skew shows.

## Protected Pages

These pages require authentication. Log in at `/login`. A fresh install seeds `admin` / `admin` (`www/auth.go:77-89`) — **change it before the station goes live.**

### Payloads

**Route:** `/payloads`

Manage payload definitions (container content templates). Create, edit, and delete payloads with their UOP capacity, template manifests, and compatible bin types.

Actions:
- Create, edit, and delete payloads
- Define template manifest (expected parts and quantities)
- Assign compatible bin types

<!-- screenshot:payloads -->
![Payloads](screenshots/payloads.png)
<!-- /screenshot -->

### Bins

**Route:** `/bins`

Manage physical containers. View all bins with their type, status, location, and payload assignment. Assign payloads to bins, confirm manifests, and manage bin lifecycle.

Actions:
- Create and retire bins. **There is no bin delete** — retire is the terminal action
- Manage bin types (create/edit/delete)
- Assign payload to bin (sets payload code, populates manifest from template)
- Confirm manifest (marks bin as loaded, sets FIFO timestamp)
- Clear bin (removes payload assignment)
- Flag, maintain, retire, or activate bins
- Bulk register bins

<!-- screenshot:bins -->
![Bins](screenshots/bins.png)
<!-- /screenshot -->

### Test Orders

**Route:** `/test-orders`

Testing interface for submitting orders through different pathways:

- **Kafka Orders** — Submit orders through the full Kafka messaging pipeline, simulating what an edge station would send
- **Direct Fleet Orders** — Submit orders directly to the RDS fleet backend, bypassing ShinGo's dispatch layer
- **RDS Commands** — Send raw commands to the fleet backend for testing

<!-- screenshot:test-orders -->
![Test Orders](screenshots/test-orders.png)
<!-- /screenshot -->

### Diagnostics

**Route:** `/diagnostics`

System diagnostics page showing:
- Kafka connection status and topic details
- RDS fleet backend connectivity
- Edge station registry (registered edges, last heartbeat, status)
- Real-time debug log stream

<!-- screenshot:diagnostics -->
![Diagnostics](screenshots/diagnostics.png)
<!-- /screenshot -->

### Configuration

**Route:** `/config`

Edit runtime configuration directly from the browser. Changes are saved to `shingocore.yaml` and hot-reloaded without restarting.

Five sections (`www/templates/config.html:11,70,99,141,173`; the handler switches on them at `www/handlers_config.go:40-46`):

- **Database** — Postgres host, port, database, user, password, sslmode
- **General** — web host and port, session secret, debug options
- **Services** — fleet (RDS base URL, poll interval, timeout) and messaging (Kafka brokers, consumer group, topics)
- **Fire Alarm** — fire-alarm integration
- **Notifications** — notification targets

<!-- screenshot:config -->
![Configuration](screenshots/config.png)
<!-- /screenshot -->

### Fleet Explorer

**Route:** `/fleet-explorer`

Raw API explorer for the fleet backend (Seer RDS). Send arbitrary GET/POST requests to the fleet API and inspect the responses. Useful for debugging fleet integration and verifying RDS state.

Pre-populated with common RDS endpoints (robots, orders, bins, scene, etc.).

<!-- screenshot:fleet-explorer -->
![Fleet Explorer](screenshots/fleet-explorer.png)
<!-- /screenshot -->

## Adding Screenshots

**None of the twelve images referenced above exist yet.** `docs/screenshots/`
holds only a `.gitkeep`, so every `![...](screenshots/*.png)` renders broken.
To fill them in:

1. Take a screenshot of each page and save it into `docs/screenshots/` under the
   filename the reference above already uses
2. The `<!-- screenshot:name -->` markers show where each one belongs

Recommended screenshot dimensions: 1200px wide, captured in both light and dark mode if desired.

## Real-Time Updates

Most pages receive live updates via Server-Sent Events (SSE). The SSE endpoint is at `/events`. Event types include:

| Event | Description |
|-------|-------------|
| `order-update` | Order status changed. One name, several shapes, discriminated by a `type` field |
| `node-update` | Node state changed |
| `bin-update` | A bin moved or was modified. **There is no `payload-update`** |
| `robot-update` | Robot status changed |
| `debug-log` | New debug log entry |
| `cell-heartbeat` | Cell production heartbeat |
| `cms-transaction` | A CMS transaction landed |
| `fire-alarm` | Fire-alarm state changed |
| `mission-event` | Mission telemetry event |
| `sourcing-update` | A sourceability verdict moved (not a plain recompute) |
| `system-status` | Connectivity / health change |

The browser automatically reconnects if the SSE connection drops.
