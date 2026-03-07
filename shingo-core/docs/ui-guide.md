# UI Guide

ShinGo Core provides a web-based management interface at `http://<host>:8083`. All pages support light and dark mode via the theme toggle in the navigation bar.

## Public Pages

These pages are accessible without authentication.

### Dashboard

**Route:** `/`

The main landing page. Shows a real-time overview of system status including active orders, node utilization, fleet status, and recent activity. Data updates automatically via server-sent events (SSE).

<!-- screenshot:dashboard -->
![Dashboard](screenshots/dashboard.png)
<!-- /screenshot -->

### Nodes

**Route:** `/nodes`

Lists all registered nodes (physical locations) with their type, zone, capacity, and current payload count. Supports filtering by node type and zone. Click a node to view its details and current inventory.

Admin actions (authenticated):
- Create, edit, and delete nodes
- Sync nodes from fleet scene data
- Sync zones from fleet areas

<!-- screenshot:nodes -->
![Nodes](screenshots/nodes.png)
<!-- /screenshot -->

### Orders

**Route:** `/orders`

Lists all transport orders with status, type, source/destination, and timestamps. Click an order row to view full details including the order timeline.

<!-- screenshot:orders -->
![Orders](screenshots/orders.png)
<!-- /screenshot -->

### Order Detail

**Route:** `/orders/detail?id=<ID>`

Full detail view for a single order. Shows order metadata, the assigned robot, vendor order ID, and a timeline of all status changes from the audit log.

Admin actions (authenticated):
- Terminate order (cancels with fleet)
- Set priority

<!-- screenshot:order-detail -->
![Order Detail](screenshots/order-detail.png)
<!-- /screenshot -->

### Robots

**Route:** `/robots`

Live status of all robots in the fleet. Shows each robot's connection status, availability, current station, battery level, and whether it's busy. Click a robot tile to see detailed status and access controls.

Admin actions (authenticated):
- Set robot available/unavailable
- Retry failed task
- Force complete current task

<!-- screenshot:robots -->
![Robots](screenshots/robots.png)
<!-- /screenshot -->

### Demand

**Route:** `/demand`

Material demand planning interface. Create demand entries specifying what payload types are needed at which nodes and in what quantities. Demands can be applied individually or in bulk to generate transport orders.

<!-- screenshot:demand -->
![Demand](screenshots/demand.png)
<!-- /screenshot -->

## Protected Pages

These pages require authentication. Log in at `/login` (default credentials: `admin` / `admin`).

### Blueprints

**Route:** `/blueprints`

Manage blueprint definitions (container content templates). Create, edit, and delete blueprints with their UOP capacity, template manifests, and compatible bin types.

Actions:
- Create, edit, and delete blueprints
- Define template manifest (expected parts and quantities)
- Assign compatible bin types

<!-- screenshot:blueprints -->
![Blueprints](screenshots/blueprints.png)
<!-- /screenshot -->

### Bins

**Route:** `/bins`

Manage physical containers. View all bins with their type, status, location, and blueprint assignment. Apply blueprints to bins, confirm manifests, and manage bin lifecycle.

Actions:
- Create, edit, and delete bins
- Manage bin types (create/edit/delete)
- Apply blueprint to bin (creates payload)
- Confirm manifest (marks bin as loaded, sets FIFO timestamp)
- Clear bin (removes payload)
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

Configurable sections:
- **Fleet** — RDS base URL, poll interval, timeout
- **Messaging** — Kafka brokers, consumer group, topics

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

To add screenshots to this guide:

1. Create a `docs/screenshots/` directory
2. Take screenshots of each page and save them with the filenames shown above
3. The `<!-- screenshot:name -->` markers indicate where each screenshot belongs

Recommended screenshot dimensions: 1200px wide, captured in both light and dark mode if desired.

## Real-Time Updates

Most pages receive live updates via Server-Sent Events (SSE). The SSE endpoint is at `/events`. Event types include:

| Event | Description |
|-------|-------------|
| `order-update` | Order status changed |
| `node-update` | Node state changed |
| `payload-update` | Payload moved or modified |
| `robot-update` | Robot status changed |
| `debug-log` | New debug log entry |

The browser automatically reconnects if the SSE connection drops.
