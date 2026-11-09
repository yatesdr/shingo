# UI Guide

Verified against: ae059495

Shingo Edge serves its browser interface at `http://<host>:8081` (`web.port`). The navigation bar carries
**Production**, an **Operations** menu (Orders, Changeover, Processes, Replenishment, Quality Containment) and,
once logged in, an **Admin** menu (Kafka Direct, Config, Logs). Every page but the operator station display has
a light/dark theme toggle in the bar; the station display is a fixed dark kiosk screen.

## Operator Pages

These pages need no login.

<!-- generated: shingo-edge/www/router.go pages public -->

| Path | Template (in `shingo-edge/www/templates/`) | Title | Auth |
|---|---|---|---|
| `/` | `production.html` | Production | public |
| `/changeover` | `changeover.html` | Changeover | public |
| `/containment` | `containment.html` | Quality Containment | public |
| `/login` | `login.html` | Login | public |
| `/operator/station/{id}` | `operator-display.html` |  | public |
| `/orders` | `orders.html` | Orders | public |
| `/production` | `production.html` | Production | public |

<!-- /generated -->

### Production

**Route:** `/` and `/production`

The station's working view, filtered by process (or all processes). The header names the current style and,
during a changeover, the target style.

- **One card per operator station**, each a table of its nodes: payload, remaining UOP, the orders working the
  node, and the actions its role allows: **Request** and **Release** (the release asks how many parts remain:
  0 or blank releases the bin empty, a number releases it partial with that count), **Request Swap** at a
  produce node, **Request Empty**, **Load Bin** / **Clear Bin** / **View** at a loader, and **Clear** for stuck
  order references.
- **Manual Order**: the same form as the Manual Order page.
- **Lineside Buckets**: parts pulled to lineside at each node. A row marked "count anomaly at cutover" is a
  stranded pile: it neither drains nor counts. **Clear** deletes a row and tells Core the pile is gone.
- **Shift Production**: today's hourly counts as bars across the configured shifts, for one process or all.
  With no shifts configured (on `/config`) the chart says so.

### Orders

**Route:** `/orders`

This station's transport orders, filtered by status (All, Active, Faulted, Failed, Queued, In Transit,
Confirmed) and process, with a text filter. Each row shows the order type, payload, process, target node (and
staging node), status with its reason line (what a queued order waits for and for how long, a fault, why an
ended order ended), creation time and ETA. An order Core created rather than this station is tagged `core`
with its demand class.

Operator actions, by status:

- **Confirm** (`delivered`): confirm receipt with the order's quantity as the final count.
- **Release** (`staged`): release a staged order. It asks how many parts remain in the bin: 0 or blank releases
  it empty (manifest cleared), a number releases it partial with that count. Parts pulled to lineside are
  recorded at the operator station, not here. A countdown shows when the staging hold expires.
- **Submit** (`pending`): send an order that has not gone to Core.
- **Abort**: cancel any order not yet `confirmed`, `cancelled`, `failed` or `skipped`, after a confirm.

Statuses and transitions are in [order-lifecycle.md](../../docs/order-lifecycle.md).

### Changeover

**Route:** `/changeover`

Manages the production line changeover workflow when switching from one job style to another.

**Starting a changeover:**
1. Select the target job style (the new product to produce). A style whose material cannot be sourced is rendered disabled, with the sourcing status and note beside its name.
2. Click "Preview" to see the orders that would be created, or "Start Changeover" to begin.

**The changeover itself has three states, not a step sequence.** The `process_changeovers` row is `active` from the moment it is created (`(*ChangeoverService).Create`), and moves once, to either `completed` or `cancelled` (`domain.ChangeoverState`). `completed` is written by the cutover door, `(*Engine).completeCutover`, and `cancelled` by "Cancel Changeover" (`(*Engine).CancelProcessChangeover`). Both are terminal (`ChangeoverState.IsTerminal`). The state appears on the page as a plain badge next to `From Style → To Style`; there is no "Advance" button anywhere on this page.

The work the operator does is **per node**, not per changeover. While the changeover is `active` the page shows one table per operator station (plus a table of central nodes), each row a node with its From payload, its To payload, and a Progress column carrying a three-step ladder:

**1. Stage → 2. Evacuate → 3. Deliver.**

Step 2 reads **2. Evacuate** while it is the live button and **2. Release** otherwise. Which button is live is decided by that node's `changeover_node_tasks` state (`domain.NodeTaskState`); the rendering is `shingo-edge/www/templates/partials/node-actions.html`:

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

**Complete Cutover** is the operator's control for ending the changeover. It stays disabled until every node task is terminal; while it is disabled, a panel above the tables reads "Cutover is waiting on *N* items" and names each blocker, with the node and order id. Both conditions are checked by `(*Engine).canCompleteChangeover`: every node task terminal, and every order those tasks reference terminal. The button has no override, so the panel's job is visibility, not permission. Only the admin's **Mark as running** on the Processes page's Styles tab (`(*Engine).SetProcessActiveStyle`) completes past the gate, writing what it overrode on the row.

Every completion goes through one door, `completeCutover`. A terminal-state event does not complete a changeover by itself: it pokes the CATID monitor, which cuts over through the same gate once the PLC reports the incoming part. The button is the operator's path to the same check, not a separate one.

Each station card also carries a **Complete Station** button and its own state badge (`waiting` / `in_progress` / `switched` — `domain.StationTaskState`).

Cutover also flips the line's active style and clears the process's target style. **Cancel Changeover** — offered only while the row is `active` — clears the target style the same way, but leaves the active style where it was (`(*Engine).cancelProcessChangeoverInternal`). Cancelling can hand off directly into a new changeover to a different style (`(*Engine).CancelProcessChangeoverRedirect`); that opens a fresh row, not a continuation of this one.

While a changeover is active the process's `target_style_id` differs from its `active_style_id`. The screens derive their state words from those two pointers ("Changing over", "Running `<style>`", "No part running"); no state column is stored.

Below the active changeover, **Changeover History** lists past changeovers with their state, times, who started them and what completed them.

### Quality Containment

**Route:** `/containment`

Bins of a contained payload divert to a containment location instead of the finished-goods drop. The page has
three parts:

- **Contained payloads**: each payload under containment, since when, by whom and why. Containment is set on
  Core's Payloads screen. **Recall landed bins** moves that payload's bins still standing at a claim's FG
  outbound into the claim's containment destination.
- **Containment locations**: every node a claim names as its containment destination (set in a process's
  Settings, Quality hold), with the bins standing there and where a release goes. **Verify Good — release to
  FG** sends a checked bin to the claim's outbound.
- **Held bins**: bins marked held and still waiting for their containment move. **Unhold** clears a stale or
  mistaken hold and returns the bin to the ordinary flow.

### Operator station display

**Route:** `/operator/station/{id}`

The touch screen at an operator station (one per screen configured on a process's Operator screens tab). It
needs no login, has no theme toggle and lays itself out at a fixed 1024 px width so a small kiosk screen scales
it whole.

- **Header**: the station name, the process and its state ("Running `<style>`", "Changing over `<from>` →
  `<to>`", "No part running"), a staleness sign for the station's data, and the **STYLE** chip, which opens a
  read-only picture of the running style's flow. The header's buttons are **CHANGEOVER** (pick a style to change
  to), or **CUTOVER** and **CANCEL** while a changeover is active, or **PULL FULL FROM HOME** at a station beside
  a dedicated home loader; **REFRESH** is always there.
- **Tiles**: one per node the station claims, showing its payload, how full it is, and the orders coming to it.
  A tap opens the node's panel with the verbs its role and state allow: requesting material, a swap or an
  empty; **RELEASE** (with a keypad for the parts that remain, releasing empty or partial); **SEND BACK**;
  **SEND TO QUALITY HOLD** at a produce node whose claim has a containment route; loading or clearing a bin at a
  loader or unloader; and, during a changeover, the node's changeover steps.
- **Loader board**: a station whose every node is a loader's window draws a board of loader cards in place of
  the tiles and the header buttons.

The display does not poll: it rebuilds its view when the event stream says something it draws changed, on
every reconnect, when the screen becomes visible and after each tap. It reloads itself when the Edge restarts.

## Administration Pages

These pages require login.

<!-- generated: shingo-edge/www/router.go pages login -->

| Path | Template (in `shingo-edge/www/templates/`) | Title | Auth |
|---|---|---|---|
| `/config` | `config.html` | Configuration | login |
| `/diagnostics` | `diagnostics.html` | Logs | login |
| `/manual-message` | `manual-message.html` | Kafka Direct | login |
| `/manual-order` | `manual-order.html` | Manual Order | login |
| `/processes` | `processes.html` | Processes | login |
| `/replenishment` | `replenishment.html` | Replenishment | login |

<!-- /generated -->

### Configuration

**Route:** `/config`

The station's settings, in sections:

- **Station**: Station UID, plant timezone (blank takes the plant's zone from Core), and whether deliveries
  confirm themselves.
- **Core connection**: the link's state, and **Refresh nodes** / **Refresh catalog** to pull Core's node list
  and payload catalog now (both refresh on their own about every two minutes).
- **PLC link**: WarLink on or off, its address, whether tag values are pushed or polled, and the PLCs WarLink
  sees.
- **Messaging**: the Kafka brokers, with **Test brokers**.
- **Shifts**: up to three shifts, name, start and end in plant time. Shifts bucket the Production page's chart.
- **Backups** and **Restore from a backup**: see [backup-restore.md](backup-restore.md).
- **Account**: **Change password…** for the logged-in admin.

The page has one save bar (**Save** / **Discard**). Settings kept in `shingoedge.yaml` save in one
`PUT /api/config`; shifts live in the database and save with the same click. A setting marked "applies after a
restart" (Station UID, timezone) takes effect at the next start. The keys themselves are in the
[README](../README.md).

### Processes

**Route:** `/processes`

Where an engineer sets up the station's processes. The list groups processes by process group and shows, per
process, what it is running, its styles, whether operators may set up styles at the HMI, its production
counter, its curtain nodes, its operator screens and its state. A part-identity health strip above the list
flags each process whose PLC reports a part the Edge cannot place, with the verb that resolves it.

Opening a process gives four tabs:

- **Styles**: a rail of the recipes and styles, and each style's flow, as the same picture and model the
  operator station uses, with its positions table. Changes are saved with **Save flow**; **+ New style** puts
  parts on a recipe. A style row's menu holds rename, expected CATID, clone, delete and **Mark as running**,
  the admin's style switch for when the plant changed parts without shingo.
- **Operator screens**: the HMIs that work this process, and its loader windows.
- **Settings**: General, Production counter (the counter's PLC and tag), Changeover, HMI, Parts this process
  runs, Quality hold (the containment destination), Styles, the Routing set, and Danger (delete the process).
- **Recipes**: saved flow presets, and the flows found in this process's styles, to apply across styles.

A process's production counter is its reporting point: the Edge keeps the reporting point that binds that tag
to the active style in step whenever the process is saved or changes over, and PLC polling reads it from there.

### Replenishment

**Route:** `/replenishment`

Cell auto-reorder, one row per claim across every style of every process (the active style flagged): process,
style, node, payload, capacity, **Reorder At**, where that number came from (calculated, manual, legacy, not
set) and **AutoReorder**. **Apply** saves a row; **Revert to legacy** sets its reorder point to 0, turns
auto-reorder off and marks it legacy. A reorder point of 0 never orders. How the level sweep decides is in
[uop-threshold-replenishment.md](../../docs/uop-threshold-replenishment.md).

### Manual Order

**Route:** `/manual-order`

Create a transport order by hand. Order types:

- **Move**: from a source node to a delivery node.
- **Retrieve**: material to a delivery node, optionally through a staging node, optionally for a process node.
- **Complex (Dwell)**: to a staging node, wait there for release, then on to the production node.

Node lists come from Core; **Sync Nodes** re-fetches them. The Production page carries the same form.

### Logs

**Route:** `/diagnostics`

- **The debug log**: the newest lines of the Edge's in-memory log, filterable by subsystem, with auto-scroll,
  **Clear** and Older / Newer paging. Every subsystem shows here whatever reaches the journal.
- **Frozen**: shown only when a loop has stopped; the log then holds one entry with every goroutine's stack.
- **Core link**: the link's summary and each feed's state.
- **Reconciliation**: status, anomaly count, pending outbox and dead letters, with **Sync Order Status**; the
  anomalies by order; and the dead-lettered outbox messages, each with **Replay** (an expired message says so
  instead of replaying).

### Kafka Direct

**Route:** `/manual-message`

Send one protocol message to Core by hand: `edge.register`, `edge.heartbeat`, `node.list_request`,
`order.request`, `order.cancel`, `order.receipt` or `order.redirect`, built from a form with a payload preview.
For testing and troubleshooting only.

## Authentication

**Route:** `/login`

On first visit, the credentials entered become the admin account. Subsequent visits authenticate against that account. Sessions are maintained via HTTP-only cookies with a 7-day expiration.

## Real-Time Updates

Pages take live updates from the Edge's event stream (`/events`, Server-Sent Events): order status changes,
counter updates and changeover transitions appear without a page refresh.
