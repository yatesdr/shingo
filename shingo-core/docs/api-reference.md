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

#### GET /api/nodes

Returns all registered nodes.

```json
[
  {
    "id": 1,
    "name": "STG-001",
    "fleet_location": "BIN-001",
    "node_type_id": 1,
    "zone": "warehouse-a",
    "capacity": 1,
    "enabled": true
  }
]
```

#### GET /api/nodes/occupancy

Compares fleet-reported bin occupancy with ShinGo's tracked payloads. Flags discrepancies.

```json
[
  {
    "node_name": "STG-001",
    "fleet_occupied": true,
    "shingo_occupied": true,
    "discrepancy": ""
  }
]
```

### Orders

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/orders` | List orders (supports `?status=<STATUS>` filter) |
| `GET` | `/api/orders/detail?id=<ID>` | Single order with history |

#### GET /api/orders

```json
[
  {
    "id": 42,
    "edge_uuid": "a1b2c3d4-...",
    "station_id": "plant-a.line-1",
    "order_type": "retrieve",
    "payload_type_code": "BIN-A",
    "pickup_node": "STG-007",
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

```json
[
  {
    "vehicle_id": "AMR-003",
    "connected": true,
    "available": true,
    "busy": true,
    "battery": 85,
    "current_station": "STG-007",
    "last_station": "LSL-001"
  }
]
```

### Payloads

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/payload-types` | List payload type definitions |
| `GET` | `/api/payloads` | List all payloads |
| `GET` | `/api/payloads/detail?id=<ID>` | Single payload with details |
| `GET` | `/api/payloads/manifest?id=<ID>` | Manifest items for a payload |

#### GET /api/payloads

```json
[
  {
    "id": 1,
    "payload_type_name": "BIN-A",
    "form_factor": "tote",
    "node_id": 5,
    "node_name": "STG-005",
    "status": "available",
    "notes": ""
  }
]
```

#### GET /api/payloads/manifest?id=1

```json
[
  {
    "id": 1,
    "payload_id": 1,
    "part_number": "PART-001",
    "quantity": 48,
    "production_date": "2026-03-01",
    "lot_code": "LOT-42",
    "notes": ""
  }
]
```

### Corrections

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/corrections` | List inventory corrections |

### Demands

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/demands` | List demand entries |
| `GET` | `/api/demands/<ID>/log` | Demand fulfillment log |

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

### Node Type Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/node-types/create` | Create node type (form post) |
| `POST` | `/node-types/update` | Update node type (form post) |
| `POST` | `/node-types/delete` | Delete node type (form post) |

### Node Properties

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/nodes/properties/set` | `{"node_id": 1, "key": "k", "value": "v"}` | Set a key-value property |
| `POST` | `/api/nodes/properties/delete` | `{"node_id": 1, "key": "k"}` | Delete a property |

### Payload Type Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/payload-types/create` | Create payload type (form post) |
| `POST` | `/payload-types/update` | Update payload type (form post) |
| `POST` | `/payload-types/delete` | Delete payload type (form post) |

### Payload Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/payloads/create` | Create payload (form post) |
| `POST` | `/payloads/update` | Update payload (form post) |
| `POST` | `/payloads/delete` | Delete payload (form post) |

### Manifest Items

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/payloads/manifest/create` | `{"payload_id": 1, "part_number": "P1", "quantity": 10}` | Add manifest item |
| `POST` | `/api/payloads/manifest/update` | `{"id": 1, "part_number": "P1", "quantity": 20}` | Update manifest item |
| `POST` | `/api/payloads/manifest/delete` | `{"id": 1}` | Remove manifest item |

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

### Corrections

| Method | Endpoint | Body | Description |
|--------|----------|------|-------------|
| `POST` | `/api/corrections/create` | `{"node_id": 1, "type": "add", ...}` | Create inventory correction |

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
| `POST` | `/api/nodes/test-order` | Quick test order from node page |

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

### Demand Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/demands` | Create demand entry |
| `PUT` | `/api/demands/<ID>` | Update demand entry |
| `PUT` | `/api/demands/<ID>/apply` | Apply single demand (generate order) |
| `DELETE` | `/api/demands/<ID>` | Delete demand entry |
| `POST` | `/api/demands/apply-all` | Apply all pending demands |
| `PUT` | `/api/demands/<ID>/produced` | Set produced quantity |
| `POST` | `/api/demands/<ID>/clear` | Clear produced quantity |
| `POST` | `/api/demands/clear-all` | Clear all produced quantities |

## SSE Events

**Endpoint:** `GET /events`

Server-sent events for real-time browser updates. No authentication required.

```
event: order-update
data: {"id": 42, "status": "delivered"}

event: node-update
data: {"id": 5, "action": "updated"}

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
