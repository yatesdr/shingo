# UI Guide

Verified against: b9210bce

ShinGo Core provides a web-based management interface at `http://<host>:8083`. All pages support light and dark mode via the theme toggle in the navigation bar.

This guide covers the pages an operator or engineer reaches from the nav. The
roster below is every route that renders a page, generated from
`shingo-core/www/router.go` and the templates each handler renders; the
sections after it are the tour. The full route list, `/api` included, is in
[API Reference](api-reference.md).

<!-- generated: shingo-core/www/router.go pages -->

| Path | Template (in `shingo-core/www/templates/`) | Title | Auth |
|---|---|---|---|
| `/` | `dashboard.html` | Dashboard | public |
| `/bins` | `bins.html` | Bins | login |
| `/config` | `config.html` | Configuration | login |
| `/cycle-time` | `cycle-time.html` | Cycle time | public |
| `/demand-episodes` | `demand-episodes.html` | Demand episodes | public |
| `/demand-episodes/{originID}` | `demand-episode.html` |  | public |
| `/diagnostics` | `diagnostics.html` | Logs | login |
| `/edges` | `edges.html` | Stations | login |
| `/fleet-explorer` | `rds_explorer.html` | Fleet Explorer | login |
| `/heartbeat` | `heartbeat.html` | Production Heartbeat — Shingo | public |
| `/inventory` | `inventory.html` | Inventory | public |
| `/inventory/count-history` | `count-history.html` |  | public |
| `/login` | `login.html` | Login | public |
| `/material-flags` | `material-flags.html` | Material flags | public |
| `/missions` | `missions.html` | Mission Telemetry | public |
| `/missions/{orderID}` | `mission-detail.html` |  | public |
| `/nodes` | `nodes.html` | Nodes | public |
| `/orders` | `orders.html` | Orders | public |
| `/orphans` | `orphans.html` | Orphan reconciliation | public |
| `/overview` | `overview.html` | Operations Overview | public |
| `/payloads` | `payloads.html` | Payloads | login |
| `/robots` | `robots.html` | Robots | public |
| `/sourcing` | `sourcing.html` | Sourcing | login |
| `/test-orders` | `test-orders.html` | Test Orders | login |
| `/wall-display/{id}` | `dashboard-display.html`, `dashboard-frame.html`, `dashboard-map.html`, `dashboard-node-report.html`, `heartbeat.html` |  | public |

<!-- /generated -->

## The nav

The top bar (`www/templates/layout.html`) reads left to right:

- **Dashboard**, **Overview**, **Orders**, **Missions**, **Robots**.
- **Assets**: Inventory and Nodes; Bins and Payloads once logged in.
- **Preview** (logged in): Sourcing, Cycle time, Episodes, Orphans, Flags —
  pages that are built but not finished. The group hides the link, not the
  page: every route in it but `/sourcing` is public, as the roster's Auth
  column shows.
- **Admin** (logged in): Stations, Test Orders, Fleet Explorer, Logs, Config.

A `login` page opened without a session redirects to `/login` and returns to
the page after sign-in. A fresh install seeds `admin` / `admin`
(`ensureDefaultAdmin`) — **change it before the station goes live** (Config ›
Account).

## Operations

### Dashboard

**Route:** `/`

The landing page. Top to bottom: the **Dashboards** hub, where wall displays
are listed, created and opened (see [Wall Displays](dashboard-platform.md));
the **Core Health** strip; and the **Active Orders** card, which redraws on
every `order-update`. Logged in, **Re-sync edges** asks every Edge to re-send
its registration and cell catalog.

The Core Health strip names only what is down, beside the verdict and the
build. Its **Faulted** figure counts orders past `rds.fault_notice_after`; the
total faulted right now is in the tile's title. Only those past the threshold
colour the verdict — a fault under it is a robot replanning, and a strip that
goes amber for those is amber most of the day. The same figures are on
`/api/core/health` as `faulted_notice` / `faulted_now`.

### Overview

**Route:** `/overview`

The operations snapshot. One filter bar — window (today, 7 or 30 days),
station, robot — drives every section: the headline KPIs with an alerts banner
when something needs a look, **Mission Trends**, **Robot Fleet** (utilization),
and **Plant Footprint** with a drill-down. Active orders and alerts update
live; the windowed figures carry their as-of time.

### Orders

**Route:** `/orders`

Lists all transport orders with status, type, source/destination, and timestamps.
Click a row to open the order in a modal: its metadata, the robot, the vendor
order ID, the timeline from the audit log, notes for the decisions the timeline
does not show, and a link to its mission telemetry. `/orders?open=<ID>` opens
that modal on load, so an order is linkable; `/orders/detail?id=<ID>` redirects
there.

Rows refresh in place rather than reloading the page, with a 30-second sweep as a
backstop for a dropped event. The text filter and the scroll position survive a
refresh. Logged in, **Manual Order** submits a transport, staged or move-robot
order.

The modal's **Controls** (logged in) are offered only where the server says the
action applies: **Terminate Order** (cancels with the fleet and releases its
bins), **Hard Release** (a wait Core owns), **Re-send Terminate** (the fleet
still runs an order Core ended), **Force Confirm** (a delivered order), and
**Set Priority**.

**Faulted orders** carry a sentence under the badge, in the same slot a queued
order uses for its wait reason:

- Under `rds.fault_notice_after` it reads **Replanning · 14 s**. A robot
  re-planning usually clears on its own; it is deliberately not called a fault,
  and the fleet's own reason is withheld with the word.
- At or past the threshold it reads **Fault · cannot replan (60011) · 3m 12s ·
  gives up in 41m** — the fleet's reason as RDS gave it, the elapsed time, and
  the countdown to `rds.fault_grace`.

Both durations tick in the browser, and the sentence changes from Replanning to
Fault on its own as the clock crosses the threshold.

The summary chip row counts only faults past the threshold; a replan raises no
chip, because a chip that fires on every fault stops being read. A **Faulted**
status filter sits with the other status pills.

In the modal a faulted order's sentence appears in the reason slot — not the
red error slot, which belongs to an order that has actually ended. The timeline
shows, on each faulted row, the fleet's reason and how long the order stayed
faulted; the open fault's dwell is live. A `grace_timeout` terminal row carries
the reason the order faulted for, so a failed order says why rather than only
that it timed out.

### Missions

**Route:** `/missions`, and `/missions/{orderID}` for one mission

Analytical drill page for completed work: the mission list, dwell per state,
breakdowns by robot and route, cell activity, and the failure Pareto. All
respect the filter bar. `/missions/{orderID}` is one mission: its summary,
every stage it held and for how long, and the fleet's messages during it.

The **Faults** card answers what a faulted order does next. Faults per day are
split replanning vs fault by `rds.fault_notice_after`, because a raw fault
total hides the few that mattered among the replans. Beside the split: the
outcome breakdown (recovered / cancelled / gave up / still faulted) with p50 and
p95 dwell for each, and top-10 tables by robot, by node, and by the fleet's own
reason code. Counts sit beside every percentile, and no data shows as an
em-dash rather than a zero.

### Robots

**Route:** `/robots`

Live status of all robots in the fleet, in three tabs: **Robots** (the tiles),
**Fleet** (now and history), and **Cleaning** (the air-knife schedule, per
robot). A tile shows connection, availability, current station, battery and
whether the robot is busy; click it for detail and controls.

A tile also names the order the robot is on: `on #4412` normally, `on #4412 ·
replanning` under the fault threshold, and a warning chip `on #4412 · fault
<live clock>` past it. Text, never the tile's colour — the tile's colour is the
robot's state, and a stuck order is not a stuck robot.

Robot alarms are deliberately absent from this line. Reflector and localization
alarms fire constantly, so an alarm printed beside a fault reads as its cause.

Admin actions (logged in), in the robot's detail:
- **Take out of dispatch** / **Make dispatchable**
- **Take control** / **Release control** of the fleet manager's control lock
  (a robot someone else holds stays undispatchable)
- **Retry failed step** — re-runs only the failed step, and on a Shingo order
  asks first, because a robot that dropped its load would finish on an empty deck
- **Force complete** — for the fleet's own parking and charging tasks; hidden,
  and refused by the server, on a Shingo order
- On the Cleaning tab: a robot's schedule switch, and **Clean** to send it
  through the air knife now

## Assets

### Inventory

**Route:** `/inventory`, and `/inventory/count-history` for the count ledger

Two tables from one read (`/api/inventory/loop`): **Supermarket positions**, a
row per loader position and part the threshold monitor watches, and **Lines**,
a row per claim of a running style at an Edge process, consume and produce.
Each table opens on the rows that need a look, worst first, with the healthy
rest folded behind one control at its foot. A line above the tables names each
bin whose count makes its part's sum untrustworthy, with a link to recount it.

Logged in, a position's UoP threshold is edited in its row and saved on Enter;
the threshold calculator, which is uncalibrated, opens from the column's ⓘ and
saves only on Confirm (see
[UoP threshold replenishment](../../docs/uop-threshold-replenishment.md)).
**Export** downloads the inventory. The **Count history** tab is the count
ledger and the carriers flagged for a recount.

### Nodes

**Route:** `/nodes`

The node tiles (physical locations and their groups), each coloured by what it
holds — empty, has payload, empty bin, staged, maintenance, claimed, disabled —
with a filter by name and by zone. Click a node for its detail and current
inventory.

Admin actions (logged in):
- **Add Node Group**, **+ Station**, and lanes within a group
- Create, edit, and delete nodes
- **Sync from Fleet** — posts to `/nodes/sync-fleet` and mirrors the fleet's
  scene into nodes

The zone-from-areas sync (`POST /nodes/sync-scene`) exists in the backend but has
no UI trigger — no template or page script references it.

### Bins

**Route:** `/bins` (login)

Physical containers: every bin with its type, status, location, payload and
count, filtered by text, type and status. The **Bin Types** accordion creates,
edits and deletes bin types; **+ Bin** creates 1 to 100 carriers from a label
prefix; **Cycle Count** walks the filtered bins for a count. The **Lineside
Buckets** section is Core's mirror of the Edges' piles.

A row opens the bin's detail, in four tabs:

- **Overview** — where it is, what it holds, and an alert banner when its
  status or its order needs attention.
- **Contents** — the manifest (each part's quantity derived from the count and
  the template), **Load** a payload with an optional count (withheld, with the
  reason, while an order holds the bin before delivery), **Clear Bin**, and
  **Confirm Manifest** / **Unconfirm**.
- **Actions** — status (**Activate**, **Flag**, **Maintenance**, the
  **Staged** toggle, **Retire**); **Lock** with a name / **Unlock**; **Move**
  by hand, or **Request Move** to send a robot; **Record Count**; the
  properties (label, description, bin type, totes on cart).
- **Journal** — **Add Note**, the audit trail, and recent orders.

Selecting rows raises a bulk bar: Activate, Flag, Maintenance, Lock, Unlock,
Retire. Each verb is one `POST /api/bins/action` (or `/api/bins/bulk-action`);
`executeBinAction` in `www/bin_actions.go` is the list of verbs. **There is no
bin delete** — retire is the terminal action.

### Payloads

**Route:** `/payloads` (login)

Payload definitions (container content templates): create, edit and delete
payloads with their UOP capacity, template manifest (the parts and how many of
each one production cycle uses) and compatible bin types; **Import** reads
payloads from a file. The **Containment** column activates and releases quality
containment for a payload: while active, every finished-goods delivery of it
diverts to its claims' containment destination.

## Preview

### Sourcing

**Route:** `/sourcing` (login)

What every process is running and can change over to right now. A summary
strip counts processes by state, worst first. **Fill these first** ranks the
missing payloads by how many changeovers each would unblock; **Ahead** is each
place's forecast — what it will need next, when, and the bin it would be sent
today (a forecast holds nothing). Below, a rail of processes and, for the one
selected, its running style, each style's verdict (sourcing, at risk when
`sourceability.enable_at_risk` is on, blocked, or not set up) with where the
free bins are, and its verdict history. The page refreshes on
`sourcing-update`, which fires when a verdict changes.

### Demand episodes

**Route:** `/demand-episodes`, and `/demand-episodes/{originID}` for one episode

The demand grain is the **episode**: `/demand-episodes` lists open and recent
episodes, and `/demand-episodes/{originID}` is the origin-indexed forensic
view — one demand and every order it spawned, each linking to its
`/missions/{orderID}` detail.

Beside it in the Preview group: **Cycle time** (`/cycle-time`), the interval
between production ticks per station, payload and direction, from the applied
UoP deltas; **Orphans** (`/orphans`), orders that should have carried a demand
origin and did not, and their trend; **Flags** (`/material-flags`), open demand
past the worry line, bindings held a long time, and carriers the part may not
travel in.

## Admin

### Stations

**Route:** `/edges` (login)

One row per enrolled edge station: its name, uid, host, schema version (beside
Core's own), status, the plant zone it reports, its clock offset from Core and
its register conflicts. The **name** is yours to change (**Rename**) — it is
read by nothing but a person and never travels on the wire. The **uid** is the
identity everything is keyed on, fixed at enrollment.

An edge that has never been configured mints its own uid and comes up working,
listed as **Unclaimed**: **Name it** to make it an ordinary station. If the box
is *replacing* an existing station, do not name it — put the existing
station's uid on the box instead (or restore its backup), so its history stays
attached.

### Test Orders

**Route:** `/test-orders` (login)

Testing interface for submitting orders through different pathways:

- **Kafka Orders** — simple and complex orders through the full Kafka messaging pipeline, as an edge station would send them
- **Direct Orders** — simple and complex move orders straight to the RDS fleet backend, bypassing ShinGo's dispatch layer
- **Robot Commands** — raw commands to the fleet backend for testing

### Fleet Explorer

**Route:** `/fleet-explorer` (login)

Raw API explorer for the fleet backend (Seer RDS). Send arbitrary GET/POST requests to the fleet API and inspect the responses. Useful for debugging fleet integration and verifying RDS state.

Pre-populated with common RDS endpoints (robots, orders, bins, scene, etc.).

### Logs

**Route:** `/diagnostics` (login)

Tabs:
- **Debug Log** — the in-memory ring buffer, every subsystem, filterable by
  subsystem and paged 100 rows at a time; new entries stream in live
- **CMS Transactions** — the CMS feed's health card and its transactions (see
  [CMS Integration](cms-integration.md))
- **Reconciliation** — the anomalies the reconciliation checks find, with the
  repair each allows (Reapply Completion, Release Claim, Release Staged Bin)
- **Recovery** — the recovery actions taken
- **Fire Alarm** — only when `fire_alarm.enabled`; activate and clear, with the
  auto-resume choice

### Configuration

**Route:** `/config` (login)

One page, one column, one **Save**, in seven sections:

- **Plant** — the plant timezone (read-only when `PLANT_TIMEZONE` sets it)
- **Fleet** — the fleet manager address, request timeout, status poll interval,
  and how long a faulted order waits before it fails
- **Messaging** — the Kafka brokers, and behind **Change…** the group ID and
  the orders and dispatch topics
- **Notifications** — e-mail alerts: mail server, TLS, sign-in, sender,
  recipients, the wait between alerts; **Send a test…**
- **Fire alarm** — whether the fire-alarm controls show, and how the
  auto-resume box starts out
- **Database** — the Postgres server, database name, sign-in, SSL mode and
  connection pool; **Test connection**
- **Account** — **Change password…** for the signed-in login

Save sends the changed sections to `PUT /api/config`. A secret is never shown:
its box says "saved" or "none saved", a blank box keeps it, and **Remove**
clears it. Some settings apply only after a restart, and the page names each
one whose saved value is waiting for one. Which settings apply on save and
which wait is in [Configuration](configuration.md).

## Real-Time Updates

Most pages receive live updates via Server-Sent Events from `/events`, and the
browser reconnects on its own if the connection drops. The event names and
their payloads are listed in [API Reference](api-reference.md).
