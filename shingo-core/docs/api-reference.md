# API Reference

All endpoints return JSON. Protected endpoints require authentication via session cookie (log in through the web UI).

Base URL: `http://<host>:8083`

## Public Endpoints

No authentication required. Read-only.

### Nodes

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/nodes` | List all nodes |
| `GET` | `/api/nodes/inventory` | Node inventory (payloads at each node) |
| `GET` | `/api/nodes/occupancy` | Fleet occupancy cross-reference |
| `GET` | `/api/nodes/detail?id=<ID>` | Single node detail |
| `GET` | `/api/nodestate` | Node state cache |
| `GET` | `/api/map/points` | Fleet scene map points |
| `GET` | `/api/map/edges` | Fleet scene path segments (advanced curves) between map points |
| `GET` | `/api/stations` | Selectable stations for pickers and filters (from orders + edge registry). `[{"id","label"}]` — `id` is the opaque station identity a caller submits and stores; `label` is `edge_registry.display_name`, for display only |

#### GET /api/nodes

Returns all registered nodes.

```json
[
  {
    "id": 1,
    "name": "STG-001",
    "is_synthetic": false,
    "zone": "warehouse-a",
    "enabled": true,
    "depth": 0,
    "created_at": "2026-03-04T10:00:00Z",
    "updated_at": "2026-03-04T10:00:00Z",
    "node_type_id": 1,
    "parent_id": 7,
    "node_type_code": "STG",
    "parent_name": "SMKT-A"
  }
]
```

#### GET /api/nodes/occupancy

Compares fleet-reported bin occupancy with ShinGo's tracked payloads. Flags discrepancies.

```json
[
  {
    "location_id": "BIN-001",
    "node_name": "STG-001",
    "fleet_occupied": true,
    "in_shingo": true,
    "discrepancy": ""
  }
]
```

### Orders

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/orders` | List orders (supports `?status=<STATUS>` filter) |
| `GET` | `/api/orders/detail?id=<ID>` | Single order — the bare row, no history |
| `GET` | `/api/orders/enriched?id=<ID>` | The order plus its history |

#### GET /api/orders

```json
[
  {
    "id": 42,
    "edge_uuid": "a1b2c3d4-...",
    "station_id": "plant-a.line-1",
    "order_type": "retrieve",
    "payload_code": "BIN-A",
    "source_node": "STG-007",
    "delivery_node": "LSL-001",
    "status": "in_transit",
    "vendor_order_id": "sg-42-abc123",
    "robot_id": "AMR-003",
    "priority": 0,
    "created_at": "2026-03-04T10:00:00Z"
  }
]
```

### Robots

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/robots` | All robots with live status |

#### GET /api/robots

`fleet.RobotStatus` (`shingo-core/fleet/optional.go:131`) carries **no JSON tags**, so it serializes
with Go's default PascalCase field names — not snake_case like the rest of this API. Callers must
match the case exactly.

```json
[
  {
    "VehicleID": "AMR-003",
    "Connected": true,
    "Available": true,
    "Busy": true,
    "Emergency": false,
    "Blocked": false,
    "IsError": false,
    "BatteryLevel": 85.0,
    "Charging": false,
    "CurrentMap": "plant-map-1",
    "Model": "<vendor model>",
    "IP": "192.0.2.10",
    "X": 12.5,
    "Y": 3.2,
    "Angle": 1.57
  }
]
```

### Payloads

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/payloads` | List all payload templates |
| `GET` | `/api/payloads/detail?id=<ID>` | Single payload with details |
| `GET` | `/api/payloads/manifest?id=<ID>` | Template manifest items for a payload |
| `GET` | `/api/payloads/templates/bin-types?id=<ID>` | Compatible bin types for a payload |

#### GET /api/payloads

```json
[
  {
    "id": 1,
    "code": "BRK-ROTOR-KIT",
    "description": "Brake Rotor Kit",
    "uop_capacity": 24
  }
]
```

### Bins

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/bins/by-node?id=<ID>` | Bins at a specific node. The param is `id`, not `node_id` |
| `GET` | `/api/bins/available` | List available (unoccupied) bins |

### Demand episodes

The `/api/demands` CRUD surface is gone — the quota table went at migration v106
(`shingo-core/www/router.go:158-159`). The demand grain is now the **episode**:

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/demand-episodes` | Open and recent demand episodes |
| `GET` | `/demand-episodes/{originID}` | One episode and every order it spawned |

### Health

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/health` | System health check |

#### GET /api/health

```json
{
  "status": "ok",
  "database": true,
  "fleet": true,
  "messaging": true
}
```

## Protected Endpoints

Authentication required (session cookie).

### Node Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/nodes/create` | Create a node (form post) |
| `POST` | `/nodes/update` | Update a node (form post) |
| `POST` | `/nodes/delete` | Delete a node (form post) |
| `POST` | `/nodes/sync-fleet` | Sync nodes from fleet scene data |
| `POST` | `/nodes/sync-scene` | Sync zones from fleet areas |

### Node Properties

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/nodes/properties/set` | `{"node_id": 1, "key": "k", "value": "v"}` | Set a key-value property |
| `POST` | `/api/nodes/properties/delete` | `{"node_id": 1, "key": "k"}` | Delete a property |

### Payload Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/payloads/create` | Create payload (form post) |
| `POST` | `/payloads/update` | Update payload (form post) |
| `POST` | `/payloads/delete` | Delete payload (form post) |
| `POST` | `/api/payloads/create` | Create payload (JSON) |
| `POST` | `/api/payloads/update` | Update payload (JSON) |
| `POST` | `/api/payloads/manifest/create` | Add a template manifest item (JSON) |
| `POST` | `/api/payloads/manifest/update` | Update a template manifest item (JSON) |
| `POST` | `/api/payloads/manifest/delete` | Delete a template manifest item (JSON) |
| `POST` | `/api/payloads/confirm-manifest` | Confirm a bin's manifest (JSON) |
| `POST` | `/api/payloads/templates/bin-types` | Set compatible bin types (JSON) |

### Bin Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/bins/create` | Create bin (form post) |
| `POST` | `/bins/retire` | Retire a bin (form post). There is no bin delete — a bin is retired, not removed |
| `POST` | `/bin-types/create` | Create bin type (form post) |
| `POST` | `/bin-types/update` | Update bin type (form post) |
| `POST` | `/bin-types/delete` | Delete bin type (form post) |
| `POST` | `/api/bins/action` | Bin status action (JSON: flag, maintain, retire, activate) |
| `POST` | `/api/bins/bulk-register` | Bulk register bins (JSON) |

### Bin Payload Assignment

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/bins/assign-payload` | Assign payload to bin (sets payload code, populates manifest) |
| `POST` | `/api/bins/confirm-manifest` | Confirm manifest (sets manifest_confirmed, loaded_at) |
| `POST` | `/api/bins/clear-payload` | Clear payload assignment from bin |
| `POST` | `/api/bins/bulk-register` | Bulk register bins (JSON) |

### Node Group Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/nodegroup/create` | Create supermarket node group (JSON) |
| `GET` | `/api/nodegroup/layout?id=<ID>` | Get group layout (lanes, slots) |
| `POST` | `/api/nodegroup/delete` | Delete node group (JSON) |
| `POST` | `/api/nodegroup/add-lane` | Add lane to group (JSON) |
| `POST` | `/api/nodegroup/reorder-lane` | Reorder lane slots (JSON) |

### Order Management

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/orders/terminate` | `{"order_id": 123}` | Cancel order (fleet + local) |
| `POST` | `/api/orders/priority` | `{"order_id": 123, "priority": 5}` | Set order priority |

### Robot Management

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/robots/availability` | `{"vehicle_id": "AMR-003", "available": true}` | Set robot availability |
| `POST` | `/api/robots/retry` | `{"vehicle_id": "AMR-003"}` | Retry failed task |
| `POST` | `/api/robots/force-complete` | `{"vehicle_id": "AMR-003"}` | Force complete current task |

### Test Orders (Kafka)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/test-orders` | List test orders |
| `GET` | `/api/test-orders/detail?id=<ID>` | Test order detail |
| `POST` | `/api/test-orders/submit` | Submit test order via Kafka |
| `POST` | `/api/test-orders/cancel` | Cancel test order |
| `POST` | `/api/test-orders/receipt` | Send delivery receipt |
| `GET` | `/api/test-orders/robots` | Available robots for testing |
| `GET` | `/api/test-orders/scene-points` | Scene points for testing |

### Test Orders (Direct to Fleet)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/test-orders/direct` | List direct fleet orders |
| `POST` | `/api/test-orders/direct` | Submit order directly to fleet backend |

### RDS Commands

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/test-commands/submit` | Send raw RDS command |
| `GET` | `/api/test-commands` | List previous commands |
| `GET` | `/api/test-commands/status?id=<ID>` | Check command status |

### Fleet Proxy

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/fleet/proxy` | `{"method": "GET", "path": "/robots"}` | Proxy request to fleet backend |

## SSE Events

**Endpoint:** `GET /events`

Server-sent events for real-time browser updates. No authentication required.

`order-update` is one event name carrying several shapes, discriminated by `type`. The order key is
`order_id`, not `id` (`shingo-core/www/sse.go:250-257`); `node-update` uses `node_id`
(`sse.go:343`).

```
event: order-update
data: {"type": "status_changed", "order_id": 42, "new_status": "delivered"}

event: order-update
data: {"type": "dispatched", "order_id": 42, "vendor_order_id": "sg-42-abc123"}

event: node-update
data: {"node_id": 5, "action": "updated"}

event: debug-log
data: {"timestamp": "...", "subsystem": "dispatch", "message": "..."}
```

## Error Responses

All error responses use this format:

```json
{
  "error": "description of what went wrong"
}
```

HTTP status codes:
- `400` — Bad request (missing/invalid parameters)
- `401` — Unauthorized (not authenticated, for protected endpoints)
- `404` — Resource not found
- `500` — Internal server error
