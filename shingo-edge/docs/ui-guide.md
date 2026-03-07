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

### Kanbans

**Route:** `/kanbans`

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
1. Select the target job style (the new product to produce)
2. Click "Start Changeover" to begin the process

**Advancing through states:**

The changeover progresses through a fixed sequence of steps. At each step, the operator clicks "Advance" after completing the required physical work:

| State | Operator Action |
|-------|-----------------|
| Stopping | Halt current production |
| Counting Out | Count and verify current inventory |
| Storing | Return current inventory to warehouse (store orders) |
| Delivering | Receive new job materials (retrieve orders) |
| Counting In | Count and inspect incoming inventory |
| Ready | Verify readiness; advancing returns to Running |

Each transition is logged with the operator name and timestamp. The page also displays the changeover history for the line.

An in-progress changeover can be cancelled at any intermediate state, returning directly to the Running state.

### Production

**Route:** `/production`

Displays production count data from PLC reporting points. Features:

- Hourly production counts displayed as a bar chart
- Filtering by date and job style
- Shift boundary indicators (when shifts are configured)

## Administration Pages

These pages require authentication.

### Setup

**Route:** `/setup`

Central configuration page for the edge station. Organized into sections:

**Production Lines** — Define the production lines (processes) managed by this station. Each line has a name and identifier.

**Job Styles** — Define the product types (styles) produced on each line. One style is active per line at a time. Switching the active style is handled through the changeover workflow.

**Payloads** — Configure the payload templates available at this station. For each payload, set:
- Description and manifest (parts list)
- UOP capacity
- Reorder threshold and auto-reorder toggle
- Associated production line

The payload catalog is synced automatically from Shingo Core. Local configuration adds station-specific settings such as reorder thresholds.

**Reporting Points** — Bind PLC counter tags to job styles for automated production counting. Each reporting point specifies:
- PLC name (discovered from WarLink)
- Tag name (counter tag on the PLC)
- Associated job style

**WarLink / PLC** — View discovered PLCs and connection status. Test individual tag reads. Configure the WarLink connection address.

**Shifts** — Define up to three shift periods per day with names and time windows. Shifts are used for bucketing production counts on the Production page.

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
