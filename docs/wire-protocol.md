# Shingo Wire Protocol Specification

**Version:** 1
**Last updated:** 2026-03-19

## Overview

The Shingo wire protocol defines a JSON-based messaging format for communication between Shingo Core (central server / dispatch) and Shingo Edge (shop-floor client) nodes. Messages are transported over Kafka. The protocol supports the full order lifecycle for material transport, plus a generic data channel for edge lifecycle management (registration, heartbeat) and future data exchange (inventory queries, production stats, scheduling).

This document specifies everything needed to implement a compatible producer or consumer in any language or system.

## Transport

### Broker

| Broker | Edge -> Core Topic | Core -> Edge Topic |
|--------|-------------------|--------------------|
| Kafka  | `shingo.orders`   | `shingo.dispatch`  |

### Topic Architecture

The protocol uses **two topics** for strict separation of traffic direction:

- **`shingo.orders`** -- Edge -> Core. Carries order-related requests and data channel messages (registration, heartbeats) from edge nodes upstream to the core server. Core subscribes to this topic. Edges publish to it.

- **`shingo.dispatch`** -- Core -> Edge. Carries order replies and data channel responses (registration acks, heartbeat acks) from the core server downstream to edge nodes. Edges subscribe to this topic. Core publishes to it.

Edges never see each other's order traffic. Core never sees its own replies echoed back.

### Kafka Configuration

| Parameter | Value |
|-----------|-------|
| Consumer group (Core) | `shingo-core` on `shingo.orders` |
| Consumer group (Edge) | `shingo-edge-{station_id}` on `shingo.dispatch` |
| Start offset (new consumers) | Latest (no replay of stale backlog on first join) |
| Message key (orders topic) | `src.station` (ordering per edge's messages) |
| Message key (dispatch topic) | `dst.station` (ordering per edge's replies) |
| Topic retention | 24 hours (configurable server-side) |

---

## Envelope Format

Every message on both topics is a single JSON object with the following structure. All fields are present in every message (except `cor`, which is omitted when empty).

```json
{
  "v":    1,
  "type": "order.request",
  "id":   "550e8400-e29b-41d4-a716-446655440000",
  "src":  {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "dst":  {"role": "core", "station": "",               "factory": ""},
  "ts":   "2026-02-18T10:00:00Z",
  "exp":  "2026-02-18T10:10:00Z",
  "cor":  "previous-message-id",
  "p":    { ... }
}
```

### Envelope Fields

| Field  | JSON Key | Type     | Required | Description |
|--------|----------|----------|----------|-------------|
| Version | `v`     | integer  | Yes | Protocol version. Currently `1`. Consumers should reject messages with unknown versions. |
| Type   | `type`   | string   | Yes | Message type identifier. Determines the schema of `p`. See [Message Types](#message-types). |
| ID     | `id`     | string   | Yes | Unique message identifier. UUID v4, lowercase hex with hyphens (RFC 4122). Example: `"550e8400-e29b-41d4-a716-446655440000"`. |
| Source | `src`    | Address  | Yes | Sender identity. See [Address](#address-object). |
| Destination | `dst` | Address | Yes | Intended recipient. See [Address](#address-object). |
| Timestamp | `ts`  | string   | Yes | ISO 8601 / RFC 3339 timestamp in UTC. When the message was created. Example: `"2026-02-18T10:00:00Z"`. |
| Expires At | `exp` | string  | Yes | ISO 8601 / RFC 3339 timestamp in UTC. After this time, receivers should drop the message without processing. See [Message Expiry](#message-expiry). A zero-value (`"0001-01-01T00:00:00Z"`) means no expiry. |
| Correlation ID | `cor` | string | No | When present, links this message to a previous message by its `id`. Used in request/reply patterns (e.g., an `order.ack` reply sets `cor` to the original `order.request` message ID). Omitted from JSON when empty. |
| Payload | `p`    | object   | Yes | Message-type-specific payload. Schema determined by `type`. During two-phase decode, this field is initially treated as raw bytes/opaque JSON and only deserialized after routing decisions are made. |

### Address Object

```json
{"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"}
```

| Field   | JSON Key  | Type   | Description |
|---------|-----------|--------|-------------|
| Role    | `role`    | string | `"edge"` or `"core"`. Identifies the sender/receiver class. |
| Station | `station` | string | Station identifier. For edges, this is typically `"{namespace}.{line_id}"` (e.g., `"plant-a.line-1"`). For core, this is the configured station ID (e.g., `"core"`). For broadcast dispatch messages, this is `"*"`. Empty string is valid for core destinations (core is a singleton). |
| Factory | `factory` | string | Factory/plant identifier (e.g., `"plant-a"`). Used for multi-site deployments. Empty string is valid when not applicable. |

---

## Two-Phase Decode

The protocol is designed for efficient filtering. Receivers should decode in two phases:

### Phase 1: Header Decode

Parse only the routing fields needed for filtering and expiry checks. In the reference implementation, this is the `RawHeader` struct:

```json
{
  "v":    1,
  "type": "order.request",
  "id":   "550e8400-...",
  "dst":  {"role": "core", "station": "", "factory": ""},
  "exp":  "2026-02-18T10:10:00Z"
}
```

This requires parsing only 5 top-level fields. The `p` (payload) bytes are never touched.

**Actions in Phase 1:**

1. **Version check** -- reject if `v` is not a recognized version.
2. **Expiry check** -- drop if `exp` is in the past (current UTC time > `exp`).
3. **Destination filter** -- apply topic-specific filtering rules (see [Filtering](#filtering)).

If any check fails, discard the message. No payload bytes are deserialized.

### Phase 2: Full Decode

If Phase 1 passes, deserialize the complete envelope including the `p` field into the appropriate payload struct based on `type`.

For `data` type messages, this involves a **two-level decode**: first into the `Data` wrapper (to extract the `subject`), then the inner `data` field into the subject-specific struct. See [Data Channel](#data-channel).

### Filtering

**Core (subscribed to `shingo.orders`):**
Core accepts all messages on the orders topic. No filtering needed -- every message on `shingo.orders` is intended for core.

**Edge (subscribed to `shingo.dispatch`):**
Each edge filters by destination node:

```
accept if dst.station == self_station_id OR dst.station == "*"
reject otherwise
```

Since edges only subscribe to `shingo.dispatch`, all messages are already `dst.role == "edge"`. The filter only checks `dst.station`.

---

## Message Types

### Type String Format

Type strings use dotted notation: `{category}.{action}`. Two categories exist for typed messages:

- `order.*` -- Transport order lifecycle
- `data` -- Generic data channel (subject-based dispatch)

### Complete Type Registry

| Type String | Topic | Direction | Payload Type | Description |
|---|---|---|---|---|
| `data` | Both | Both | [Data](#data-channel) | Generic data exchange (registration, heartbeat, queries, etc.). Subject-based dispatch. |
| `order.request` | `shingo.orders` | Edge -> Core | [OrderRequest](#orderrequest) | New transport order submission |
| `order.complex_request` | `shingo.orders` | Edge -> Core | [ComplexOrderRequest](#complexorderrequest) | Multi-step transport order submission |
| `order.cancel` | `shingo.orders` | Edge -> Core | [OrderCancel](#ordercancel) | Request to cancel an existing order |
| `order.receipt` | `shingo.orders` | Edge -> Core | [OrderReceipt](#orderreceipt) | Delivery confirmation from operator |
| `order.redirect` | `shingo.orders` | Edge -> Core | [OrderRedirect](#orderredirect) | Change delivery destination mid-flight |
| `order.release` | `shingo.orders` | Edge -> Core | [OrderRelease](#orderrelease) | Resume a staged (dwelling) order |
| `order.storage_waybill` | `shingo.orders` | Edge -> Core | [OrderStorageWaybill](#orderstoragewaybill) | Store order submission (return to warehouse) |
| `order.ingest` | `shingo.orders` | Edge -> Core | [OrderIngestRequest](#orderingestrequest) | Submit a filled bin for storage |
| `order.ack` | `shingo.dispatch` | Core -> Edge | [OrderAck](#orderack) | Order accepted, source material located |
| `order.waybill` | `shingo.dispatch` | Core -> Edge | [OrderWaybill](#orderwaybill) | Robot assigned and dispatched |
| `order.update` | `shingo.dispatch` | Core -> Edge | [OrderUpdate](#orderupdate) | Status change or ETA update |
| `order.staged` | `shingo.dispatch` | Core -> Edge | [OrderStaged](#orderstaged) | Order dwelling at staging node, awaiting release |
| `order.delivered` | `shingo.dispatch` | Core -> Edge | [OrderDelivered](#orderdelivered) | Fleet reports delivery complete |
| `order.error` | `shingo.dispatch` | Core -> Edge | [OrderError](#ordererror) | Order processing failed |
| `order.cancelled` | `shingo.dispatch` | Core -> Edge | [OrderCancelled](#ordercancelled) | Order cancellation confirmed |

---

## Data Channel

The `data` message type provides a generic, extensible channel for non-order communication. Instead of adding new top-level message types for every feature, data messages use **subject-based dispatch**: the `subject` field inside the payload selects the schema, and the `data` field carries the subject-specific body.

### Data Payload Structure

```json
{
  "v": 1,
  "type": "data",
  "id": "...",
  "src": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "dst": {"role": "core", "station": "", "factory": ""},
  "ts": "2026-02-18T10:01:00Z",
  "exp": "2026-02-18T10:02:30Z",
  "p": {
    "subject": "edge.heartbeat",
    "data": {"station_id": "plant-a.line-1", "uptime_s": 3600, "active_orders": 2}
  }
}
```

| Field | JSON Key | Type | Description |
|---|---|---|---|
| Subject | `subject` | string | Identifies the data schema. Dotted notation (e.g., `"edge.heartbeat"`). See [Known Subjects](#known-subjects). |
| Data | `data` | object | Subject-specific payload. Schema determined by `subject`. Treated as raw JSON (`json.RawMessage`) until the handler switches on subject. |

### Two-Level Decode

Handlers process data messages in two steps:

1. **Level 1:** Decode the envelope payload into `Data` (extracts `subject` and raw `data` bytes).
2. **Level 2:** Switch on `subject` and decode the `data` field into the appropriate subject-specific struct.

This design means unhandled subjects never touch the inner body bytes -- only the `subject` string is inspected.

### Request/Reply

Data messages use the envelope's existing `cor` (correlation ID) field for request/reply patterns. No additional fields are needed. A reply sets `cor` to the original message's `id`.

### Known Subjects

| Subject | Direction | Data Schema | Description |
|---|---|---|---|
| `edge.register` | Edge -> Core | [EdgeRegister](#edgeregister) | Edge announces itself on startup or reconnect |
| `edge.registered` | Core -> Edge | [EdgeRegistered](#edgeregistered) | Core acknowledges registration |
| `edge.heartbeat` | Edge -> Core | [EdgeHeartbeat](#edgeheartbeat) | Periodic health ping (every 60 seconds) |
| `edge.heartbeat_ack` | Core -> Edge | [EdgeHeartbeatAck](#edgeheartbeatack) | Core acknowledges heartbeat |
| `edge.stale` | Core -> Edge | [EdgeStale](#edgestale) | Core notifies edge it has been marked stale |
| `edge.register_request` | Core -> Edge | [EdgeRegisterRequest](#edgeregisterrequest) | Core asks edge to re-register |
| `production.report` | Edge -> Core | [ProductionReport](#productionreport) | Edge sends production counts |
| `production.report_ack` | Core -> Edge | [ProductionReportAck](#productionreportack) | Core acknowledges production report |
| `node.list_request` | Edge -> Core | [NodeListRequest](#nodelistrequest) | Edge requests core's node list |
| `node.list_response` | Core -> Edge | [NodeListResponse](#nodelistresponse) | Core returns authoritative node list |
| `tag.verify_request` | Edge -> Core | [TagVerifyRequest](#tagverifyrequest) | Edge verifies scanned QR tag against order |
| `tag.verify_response` | Core -> Edge | [TagVerifyResponse](#tagverifyresponse) | Core returns tag verification result |
| `catalog.payloads_request` | Edge -> Core | [CatalogPayloadsRequest](#catalogpayloadsrequest) | Edge requests payload catalog |
| `catalog.payloads_response` | Core -> Edge | [CatalogPayloadsResponse](#catalogpayloadsresponse) | Core returns payload catalog |

New subjects can be added by defining a constant and a data schema -- no protocol interface changes required.

### Subject-Specific TTLs

Data messages have a default TTL of 5 minutes, but individual subjects can override this:

| Subject | TTL | Rationale |
|---|---|---|
| `edge.heartbeat` | 90 seconds | Stale after 1.5 heartbeat intervals |
| `edge.heartbeat_ack` | 90 seconds | Stale after 1.5 heartbeat intervals |
| `edge.register` | 5 minutes | Should complete quickly after connect |
| `edge.registered` | 5 minutes | Should complete quickly after connect |
| `production.report` | 5 minutes | Should be processed promptly |
| `production.report_ack` | 5 minutes | Should be processed promptly |
| `edge.stale` | 5 minutes | Notification, not time-critical |
| `edge.register_request` | 5 minutes | Should trigger re-register promptly |
| `node.list_request` | 5 minutes | Sync request |
| `node.list_response` | 5 minutes | Sync response |
| Unknown subjects | 5 minutes | Safe general default |

---

## Message Expiry (TTL)

Every message carries an absolute expiry timestamp in the `exp` field. Receivers must check this before processing:

```
if current_utc_time > exp:
    drop message (do not process)
```

### Default TTL Values

These are the TTLs applied by the sender when creating a message. The `exp` field is always an absolute timestamp (not a duration).

| Category | Message Types | TTL | Rationale |
|---|---|---|---|
| Data channel | `data` (default) | 5 minutes | Safe general default for data exchange |
| Data: heartbeat | `data` with subject `edge.heartbeat` / `edge.heartbeat_ack` | 90 seconds | Stale after 1.5 heartbeat intervals |
| Data: registration | `data` with subject `edge.register` / `edge.registered` | 5 minutes | Should complete quickly after connect |
| Order commands | `order.request`, `order.complex_request`, `order.cancel`, `order.redirect`, `order.release`, `order.storage_waybill`, `order.ingest` | 10 minutes | Operator can resubmit if expired |
| Order status | `order.ack`, `order.update` | 10 minutes | Status updates age fast |
| Important replies | `order.receipt`, `order.waybill`, `order.error`, `order.cancelled` | 30 minutes | Important, longer window needed |
| Delivery notification | `order.delivered` | 60 minutes | Critical notification, longest window |
| Unknown/fallback | Any unrecognized type | 10 minutes | Safe default |

### Why Absolute Timestamps

Relative TTLs (durations) are ambiguous when messages sit in a Kafka partition during edge reconnection. Absolute timestamps make expiry calculation trivial for any consumer: compare `exp` to your current UTC clock. No need to know when the message was enqueued or how long it sat in transit.

---

## Payload Schemas

All payloads are JSON objects nested inside the `p` field of the envelope. Fields marked "omitempty" may be absent from the JSON when their value is the zero value for that type (empty string, 0, false).

### Data Channel Schemas

These schemas are used as the `data` field inside `data`-type messages. See [Data Channel](#data-channel).

#### EdgeRegister

Sent by an edge node on startup and on every reconnect via `data` message with subject `edge.register`. Triggers an upsert in core's edge registry.

```json
{
  "station_id": "plant-a.line-1",
  "hostname":   "edge-01.local",
  "version":    "1.2.0",
  "line_ids":   ["line-1"]
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | Unique edge station identifier. Convention: `"{namespace}.{line_id}"`. |
| Hostname | `hostname` | string | No | OS hostname of the edge machine. Informational. |
| Version | `version` | string | No | Software version of the edge application. |
| Line IDs | `line_ids` | string[] | No | Production line identifiers this edge manages. JSON array of strings. |

The factory identifier is derived from the envelope `src.factory` field, not the registration payload.

#### EdgeHeartbeat

Sent every 60 seconds by each edge node via `data` message with subject `edge.heartbeat`.

```json
{
  "station_id":       "plant-a.line-1",
  "uptime_s":      3600,
  "active_orders": 2
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | Edge station identifier (must match registration). |
| Uptime | `uptime_s` | integer | No | Seconds since edge process started. |
| Active Orders | `active_orders` | integer | No | Number of currently active (non-terminal) orders. |

#### EdgeRegistered

Acknowledges a successful edge registration via `data` message with subject `edge.registered`. The `cor` (correlation ID) field in the envelope links back to the original registration message.

```json
{
  "station_id": "plant-a.line-1",
  "message": "registered"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | The registered edge station ID (echo back). |
| Message | `message` | string | No | Human-readable status message. |

#### EdgeHeartbeatAck

Acknowledges a heartbeat via `data` message with subject `edge.heartbeat_ack`. Provides server timestamp for optional clock drift detection.

```json
{
  "station_id": "plant-a.line-1",
  "server_ts":  "2026-02-18T10:01:01Z"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | The heartbeating edge station ID (echo back). |
| Server Timestamp | `server_ts` | timestamp | Yes | Core's current time as ISO 8601 / RFC 3339. Edges can compare with their own clock to detect drift. |

#### EdgeStale

Sent by core to notify an edge that it has been marked stale (missed heartbeats).

```json
{
  "station_id": "plant-a.line-1",
  "message":    "Edge marked stale: no heartbeat for 180s"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | The stale edge station ID. |
| Message | `message` | string | No | Human-readable detail. |

#### EdgeRegisterRequest

Sent by core to request that an edge re-register (e.g., after core restart or edge registry cleanup).

```json
{
  "station_id": "plant-a.line-1",
  "reason":     "Core restarted, edge registry cleared"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | The edge station that should re-register. |
| Reason | `reason` | string | No | Why re-registration is needed. |

#### ProductionReport

Sent by edge to report production counts from PLC reporting points.

```json
{
  "station_id": "plant-a.line-1",
  "reports": [
    {"cat_id": "BRK-ROTOR-KIT", "count": 12}
  ]
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | Reporting edge station. |
| Reports | `reports` | ProductionReportEntry[] | Yes | Array of production counts per payload catalog ID. |

**ProductionReportEntry:**

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Cat ID | `cat_id` | string | Yes | Payload catalog identifier. |
| Count | `count` | integer | Yes | Production count for this entry. |

#### ProductionReportAck

Acknowledges processing of a production report.

```json
{
  "station_id": "plant-a.line-1",
  "accepted":   1
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Station ID | `station_id` | string | Yes | The reporting edge station (echo back). |
| Accepted | `accepted` | integer | Yes | Number of report entries accepted. |

#### NodeListRequest

Sent by edge to request the core's authoritative node list. Empty body.

```json
{}
```

#### NodeListResponse

Core returns its node list. Used by edge to populate node dropdowns in the payload configuration UI.

```json
{
  "nodes": [
    {"name": "STG-001", "node_type": ""},
    {"name": "STORAGE-A", "node_type": "NGRP"}
  ]
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Nodes | `nodes` | NodeInfo[] | Yes | Array of node entries. |

**NodeInfo:**

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Name | `name` | string | Yes | Node name. |
| Node Type | `node_type` | string | No | Node type code (e.g., `"NGRP"`, `"LANE"`). Empty for physical nodes. |

#### TagVerifyRequest

Sent by edge to verify a scanned QR tag against the expected bin for an order.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "tag_id":     "SHG:0042",
  "location":   "LINE1-IN"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The order being verified. |
| Tag ID | `tag_id` | string | Yes | Scanned QR tag value. |
| Location | `location` | string | No | Where the scan occurred. |

#### TagVerifyResponse

Core's response to a tag verification request.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "match":      true,
  "expected":   "SHG:0042",
  "detail":     "Tag matches claimed bin"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The order that was verified. |
| Match | `match` | boolean | Yes | Whether the scanned tag matches the expected bin. |
| Expected | `expected` | string | No | The expected tag value (for mismatch diagnostics). |
| Detail | `detail` | string | No | Human-readable result detail. |

#### CatalogPayloadsRequest

Sent by edge to request the payload catalog from core. Empty body.

```json
{}
```

#### CatalogPayloadsResponse

Core returns the payload catalog. Used by edge to populate payload code dropdowns and look up UOP capacity.

```json
{
  "payloads": [
    {"id": 1, "name": "Brake Rotor Kit", "code": "BRK-ROTOR-KIT", "description": "Rotor assembly parts", "uop_capacity": 24}
  ]
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Payloads | `payloads` | CatalogPayloadInfo[] | Yes | Array of payload templates. |

**CatalogPayloadInfo:**

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| ID | `id` | integer | Yes | Core's internal payload ID. |
| Name | `name` | string | Yes | Payload template name. |
| Code | `code` | string | Yes | Unique payload type code (e.g., `"BRK-ROTOR-KIT"`). |
| Description | `description` | string | No | Human-readable description. |
| UOP Capacity | `uop_capacity` | integer | Yes | Production cycles supported by a full load. |

### Order Payloads: Edge -> Core

#### OrderRequest

Submits a new transport order. The `order_uuid` is generated by the edge and used to correlate all subsequent messages about this order.

```json
{
  "order_uuid":       "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "order_type":       "retrieve",
  "payload_code":     "BIN-A",
  "payload_desc":     "Small parts bin",
  "quantity":         1,
  "delivery_node":    "line-1-station-a",
  "source_node":      "",
  "staging_node":     "line-1-staging",
  "load_type":        "",
  "priority":         0,
  "retrieve_empty":   false
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | Client-generated UUID v4. Unique across all orders. |
| Order Type | `order_type` | string | Yes | One of: `"retrieve"`, `"move"`, `"store"`. |
| Payload Code | `payload_code` | string | No | Registered payload type code (e.g., `"BIN-A"`). Used by core to locate source material. |
| Payload Description | `payload_desc` | string | No | Human-readable description of the payload. |
| Quantity | `quantity` | integer | Yes | Number of items or units. |
| Delivery Node | `delivery_node` | string | Conditional | Where to deliver. Required for `retrieve` and `move` orders. |
| Source Node | `source_node` | string | Conditional | Where to pick up. Required for `move` and `store` orders. |
| Staging Node | `staging_node` | string | No | Intermediate staging location (e.g., for line-side buffer). |
| Load Type | `load_type` | string | No | Type of load (application-specific). |
| Priority | `priority` | integer | No | Higher = more urgent. Default `0`. |
| Retrieve Empty | `retrieve_empty` | boolean | No | If `true`, retrieve an empty container rather than a full one. |

**Order Types:**
- `retrieve` -- Fetch material from warehouse storage and deliver to a line-side station. Core selects the source node automatically (FIFO).
- `move` -- Move material between two specified locations. Both `source_node` and `delivery_node` are required.
- `store` -- Return material from a line-side station to warehouse storage. Core selects the storage destination automatically. `source_node` is required.

#### OrderCancel

Requests cancellation of an in-progress order.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "reason":     "Operator cancelled: wrong material"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | UUID of the order to cancel. |
| Reason | `reason` | string | Yes | Human-readable cancellation reason. |

#### OrderReceipt

Sent by the edge operator to confirm physical receipt of a delivery.

```json
{
  "order_uuid":   "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "receipt_type": "confirmed",
  "final_count":  48
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | UUID of the delivered order. |
| Receipt Type | `receipt_type` | string | Yes | Currently always `"confirmed"`. |
| Final Count | `final_count` | integer | Yes | Actual received quantity (may differ from ordered quantity). |

#### OrderRedirect

Changes the delivery destination of an in-flight order. Triggers re-dispatch in core.

```json
{
  "order_uuid":        "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "new_delivery_node": "line-2-station-b"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | UUID of the order to redirect. |
| New Delivery Node | `new_delivery_node` | string | Yes | New destination node name. |

#### OrderStorageWaybill

Submits a store order with source details and count.

```json
{
  "order_uuid":   "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "order_type":   "store",
  "payload_desc": "Empty bin return",
  "source_node":  "line-1-station-a",
  "final_count":  0
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | UUID of the store order. |
| Order Type | `order_type` | string | Yes | Always `"store"`. |
| Payload Description | `payload_desc` | string | No | Human-readable description. |
| Source Node | `source_node` | string | Yes | Where to pick up the payload for storage. |
| Final Count | `final_count` | integer | Yes | Item count at time of storage submission. |

### Order Payloads: Core -> Edge

#### OrderAck

Confirms that core has accepted the order, located source material, and is proceeding with dispatch.

```json
{
  "order_uuid":      "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "shingo_order_id": 1042,
  "source_node":     "storage-rack-7"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The edge-generated order UUID (echo back). |
| Shingo Order ID | `shingo_order_id` | integer | Yes | Core's internal order ID (int64). |
| Source Node | `source_node` | string | No | The node core selected as the source for retrieval. |

#### OrderWaybill

A robot has been assigned and dispatched. The order is now physically in motion.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "waybill_id": "sg-1042-a3f8b2c1",
  "robot_id":   "AMR-003",
  "eta":        "2026-02-18T10:15:00Z"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The edge-generated order UUID. |
| Waybill ID | `waybill_id` | string | Yes | Fleet vendor's transport order identifier. |
| Robot ID | `robot_id` | string | No | Identifier of the assigned robot/AMR. |
| ETA | `eta` | string | No | Estimated time of arrival. ISO 8601 / RFC 3339 or free-form string. |

#### OrderUpdate

Status change or ETA update for an in-progress order. May be sent multiple times.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "status":     "in_transit",
  "detail":     "Robot en route to destination",
  "eta":        "2026-02-18T10:14:30Z"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The edge-generated order UUID. |
| Status | `status` | string | Yes | Current fleet-side status string. |
| Detail | `detail` | string | No | Human-readable status detail. |
| ETA | `eta` | string | No | Updated ETA, if available. |

#### OrderDelivered

Fleet reports that the robot has completed delivery at the destination node. The edge should prompt the operator for receipt confirmation (or auto-confirm if configured).

```json
{
  "order_uuid":        "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "delivered_at":      "2026-02-18T10:13:45Z",
  "staged_expire_at":  "2026-02-18T10:23:45Z"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The edge-generated order UUID. |
| Delivered At | `delivered_at` | string | Yes | ISO 8601 / RFC 3339 timestamp of physical delivery. |
| Staged Expire At | `staged_expire_at` | string | No | If present, the order enters `staged` status (dwelling) until this time or until an `order.release` is received. Omitted for non-staged deliveries. |

#### OrderError

Order processing has failed. The order is in a terminal error state on the core side.

```json
{
  "order_uuid":  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "error_code":  "no_source",
  "detail":      "No source payload found for type BIN-A"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The edge-generated order UUID. |
| Error Code | `error_code` | string | Yes | Machine-readable error category. See [Error Codes](#error-codes). |
| Detail | `detail` | string | Yes | Human-readable error description. |

#### OrderCancelled

Confirms that an order has been successfully cancelled on the core side.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "reason":     "Operator cancelled: wrong material"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The cancelled order UUID. |
| Reason | `reason` | string | Yes | Cancellation reason (echoed from the cancel request). |

### Complex Order Payloads: Edge -> Core

#### ComplexOrderRequest

Submits a multi-step transport order. Used by material handling cycles (sequential, two-robot, single-robot) where a single order involves multiple pickup, dropoff, and wait steps chained together. Core dispatches the steps as a block-based order to the fleet backend.

```json
{
  "order_uuid":   "b2c3d4e5-f6a7-8901-bcde-f12345678901",
  "payload_code": "BIN-A",
  "payload_desc": "Small parts bin",
  "quantity":     1,
  "priority":     0,
  "steps": [
    {"action": "pickup",  "node": "STORAGE-A"},
    {"action": "dropoff", "node": "STG-001"},
    {"action": "wait"},
    {"action": "pickup",  "node": "STG-001"},
    {"action": "dropoff", "node": "LINE1-IN"}
  ]
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | Client-generated UUID v4. |
| Payload Code | `payload_code` | string | No | Payload type code for source material selection. |
| Payload Description | `payload_desc` | string | No | Human-readable description. |
| Quantity | `quantity` | integer | Yes | Number of items or units. |
| Priority | `priority` | integer | No | Higher = more urgent. Default `0`. |
| Steps | `steps` | ComplexOrderStep[] | Yes | Ordered list of steps. See below. |

**ComplexOrderStep:**

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Action | `action` | string | Yes | One of: `"pickup"`, `"dropoff"`, `"wait"`. |
| Node | `node` | string | No | Node or group name. Core auto-detects groups (NGRP) and resolves to a concrete slot. If omitted, Core uses global fallback via payload code. |

Steps are executed in sequence. A `wait` step causes the robot to dwell at its current position until an `order.release` message is received.

#### OrderRelease

Signals that a staged (dwelling) order should resume execution. Sent by the operator when the outgoing bin is ready for pickup.

```json
{
  "order_uuid": "b2c3d4e5-f6a7-8901-bcde-f12345678901"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | UUID of the staged order to release. |

#### OrderIngestRequest

Submits a newly filled bin for storage. Core sets the bin manifest from the payload template and dispatches a store order to move the bin to the warehouse.

```json
{
  "order_uuid":   "c3d4e5f6-a7b8-9012-cdef-234567890123",
  "payload_code": "BIN-A",
  "bin_label":    "SHG:0042",
  "source_node":  "LINE1-OUT",
  "quantity":     1,
  "manifest": [
    {"part_number": "RT-100", "quantity": 24, "description": "Rotor assembly"}
  ]
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | Client-generated UUID v4. |
| Payload Code | `payload_code` | string | Yes | Payload type for manifest lookup. |
| Bin Label | `bin_label` | string | Yes | QR label of the bin to ingest. |
| Source Node | `source_node` | string | Yes | Where the filled bin is located. |
| Quantity | `quantity` | integer | Yes | Number of items or units. |
| Manifest | `manifest` | IngestManifestItem[] | No | Explicit manifest items. If omitted, core uses the payload template. |

**IngestManifestItem:**

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Part Number | `part_number` | string | Yes | Part identifier. |
| Quantity | `quantity` | integer | Yes | Count of this part. |
| Description | `description` | string | No | Human-readable part description. |

### Complex Order Payloads: Core -> Edge

#### OrderStaged

Notifies the edge that an order has entered a dwelling (wait) state at a staging node. The operator should release the order when the physical swap is ready.

```json
{
  "order_uuid": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
  "detail":     "Waiting at STG-001 for operator release"
}
```

| Field | JSON Key | Type | Required | Description |
|---|---|---|---|---|
| Order UUID | `order_uuid` | string | Yes | The edge-generated order UUID. |
| Detail | `detail` | string | No | Human-readable status detail. |

---

## Error Codes

The `error_code` field in `order.error` messages uses the following values:

| Code | Meaning |
|---|---|
| `payload_type_error` | The requested payload type code is not registered in core. |
| `invalid_node` | A referenced node name (delivery, pickup, redirect destination) does not exist. |
| `no_source` | No available source payload found for the requested type (FIFO search failed). |
| `no_payload` | No unclaimed payload of the requested type at the specified source node. |
| `claim_failed` | Failed to claim/reserve a payload (concurrent contention). |
| `node_error` | Internal error resolving a node record. |
| `fleet_failed` | Fleet backend (robot dispatch) rejected the transport order. |
| `missing_source` | A move or store order was submitted without a required source node. |
| `no_storage` | No available storage destination node found. |
| `redirect_failed` | Redirect could not be completed (e.g., no source node for re-dispatch). |
| `unknown_type` | Unrecognized order type. |
| `internal_error` | Catch-all for unexpected internal failures. |

---

## Order Lifecycle

### Edge-Side State Machine

```
queued -> submitted -> acknowledged -> in_transit -> delivered -> confirmed
                                  \                /
                                   +-> staged ----+
                                       (wait for release)
                                             cancelled <-- (from any non-terminal)
```

| State | Meaning | Triggered By |
|---|---|---|
| `queued` | Created locally, not yet sent | Edge: operator submits order |
| `submitted` | Sent to core via messaging | Edge: outbox drainer publishes `order.request` |
| `acknowledged` | Core accepted, source located | Core sends `order.ack` |
| `in_transit` | Robot dispatched and moving | Core sends `order.waybill` |
| `staged` | Robot dwelling at node, awaiting release | Core sends `order.staged` |
| `delivered` | Fleet reports delivery complete | Core sends `order.delivered` |
| `confirmed` | Operator confirmed receipt | Edge sends `order.receipt` |
| `cancelled` | Order cancelled | Edge sends `order.cancel` or core sends `order.cancelled` |

### Core-Side State Machine

```
pending -> sourcing -> dispatched -> in_transit -> delivered -> confirmed -> completed
                                \                /
                                 +-> staged ----+
                                     (dwelling / wait step)
                                              failed / cancelled <-- (from active states)
```

| State | Meaning |
|---|---|
| `pending` | Order received from edge |
| `sourcing` | Locating source material / validating nodes |
| `dispatched` | Transport order created with fleet backend |
| `in_transit` | Robot is moving (fleet poller updates) |
| `staged` | Robot dwelling at a node, waiting for operator release (complex orders with `wait` steps) |
| `delivered` | Fleet reports delivery complete |
| `confirmed` | Edge sent delivery receipt |
| `completed` | Order fully completed |
| `failed` | Order processing failed (error sent to edge) |
| `cancelled` | Order cancelled |

### Message Flow: Successful Retrieve Order

```
Edge                          Broker                         Core
 |                              |                              |
 |-- order.request ----------->|-- shingo.orders ------------>|
 |                              |                              | (create order, find source,
 |                              |                              |  claim payload, dispatch to fleet)
 |<- order.ack ----------------|<- shingo.dispatch ---------- |
 |                              |                              | (fleet assigns robot)
 |<- order.waybill ------------|<- shingo.dispatch ---------- |
 |                              |                              | (fleet poller: robot arrives)
 |<- order.delivered ----------|<- shingo.dispatch ---------- |
 |                              |                              |
 | (operator confirms receipt)  |                              |
 |-- order.receipt ----------->|-- shingo.orders ------------>|
 |                              |                              | (mark confirmed -> completed)
```

### Message Flow: Complex Order with Wait Step

```
Edge                          Broker                         Core
 |                              |                              |
 |-- order.complex_request --->|-- shingo.orders ------------>|
 |                              |                              | (build block chain,
 |                              |                              |  dispatch to fleet)
 |<- order.ack ----------------|<- shingo.dispatch ---------- |
 |                              |                              | (fleet assigns robot,
 |                              |                              |  robot executes steps)
 |<- order.waybill ------------|<- shingo.dispatch ---------- |
 |                              |                              | (robot reaches wait step)
 |<- order.staged -------------|<- shingo.dispatch ---------- |
 |                              |                              |
 | (operator releases order)   |                              |
 |-- order.release ----------->|-- shingo.orders ------------>|
 |                              |                              | (fleet resumes,
 |                              |                              |  robot completes steps)
 |<- order.delivered ----------|<- shingo.dispatch ---------- |
 |                              |                              |
 | (operator confirms receipt) |                              |
 |-- order.receipt ----------->|-- shingo.orders ------------>|
 |                              |                              | (mark confirmed -> completed)
```

---

## Edge Registration and Heartbeat

Edge registration and heartbeat messages are sent via the [Data Channel](#data-channel).

### Registration Flow

1. Edge starts (or reconnects).
2. Edge publishes `data` message with subject `edge.register` on `shingo.orders`.
3. Core upserts the edge in `edge_registry` table, sets status to `"active"`.
4. Core publishes `data` message with subject `edge.registered` on `shingo.dispatch` (with `cor` linking to the original message).
5. Edge receives acknowledgement.

### Heartbeat Flow

1. Edge publishes `data` message with subject `edge.heartbeat` every **60 seconds** on `shingo.orders`.
2. Core updates `last_heartbeat` timestamp in `edge_registry`.
3. Core publishes `data` message with subject `edge.heartbeat_ack` on `shingo.dispatch` (with server timestamp for clock sync).

### Stale Edge Detection

Core runs a background check every **60 seconds**:
- If an edge's `last_heartbeat` is older than **180 seconds** (3 missed heartbeats), its status is set to `"stale"`.
- A new registration or heartbeat resets status to `"active"`.

### Edge Registry Schema

The core maintains an `edge_registry` table:

| Column | Type | Description |
|---|---|---|
| `id` | integer (auto) | Primary key |
| `station_id` | string (unique) | Edge station identifier |
| `factory_id` | string | Factory this edge belongs to |
| `hostname` | string | OS hostname |
| `version` | string | Software version |
| `line_ids` | string (JSON array) | Production lines managed by this edge |
| `registered_at` | timestamp | When the edge last registered |
| `last_heartbeat` | timestamp (nullable) | When the last heartbeat was received |
| `status` | string | `"active"` or `"stale"` |

---

## Data Types Reference

### Primitive Types

| Type | JSON Representation | Notes |
|---|---|---|
| string | `"text"` | UTF-8 encoded. |
| integer | `42` | Signed 64-bit. No fractional part. |
| float | `1.5` | IEEE 754 double-precision. Used for quantities. |
| boolean | `true` / `false` | |
| string[] | `["a", "b"]` | JSON array of strings. |
| timestamp | `"2026-02-18T10:00:00Z"` | ISO 8601 / RFC 3339. Always UTC (trailing `Z`). |
| uuid | `"550e8400-e29b-41d4-a716-446655440000"` | UUID v4, lowercase hex, hyphens. RFC 4122. |

### Null and Missing Fields

- Fields marked `omitempty` in the schemas above may be absent from the JSON when their value is the zero value for that type (`""` for strings, `0` for numbers, `false` for booleans).
- Consumers should treat absent fields the same as zero-valued fields.
- The `cor` envelope field is the only envelope-level field that may be absent.
- The `p` (payload) field is always present and always a JSON object (never `null`).

---

## Implementing a Consumer

### Minimal Consumer Algorithm (any language)

```
subscribe to topic ("shingo.orders" or "shingo.dispatch")

on_message(raw_bytes):
    # Phase 1: header parse
    header = json_parse(raw_bytes)       # or parse only needed fields
    if header.v != 1:
        log("unknown protocol version")
        return
    if header.exp != zero AND now_utc() > header.exp:
        return                           # expired, discard silently
    if not passes_filter(header.dst):
        return                           # not for us, discard silently

    # Phase 2: full parse
    envelope = json_parse(raw_bytes)     # full parse (or reuse from phase 1)
    payload = json_parse(envelope.p)     # deserialize based on envelope.type

    # Dispatch
    switch envelope.type:
        case "data":
            data = payload               # {subject, data}
            switch data.subject:
                case "edge.register":    handle_register(envelope, data.data)
                case "edge.heartbeat":   handle_heartbeat(envelope, data.data)
                ...
        case "order.request":    handle_order_request(envelope, payload)
        case "order.ack":        handle_order_ack(envelope, payload)
        ...
```

### Filter Implementation

**For core (listening on `shingo.orders`):**
```
passes_filter(dst) = true  # accept everything
```

**For edge (listening on `shingo.dispatch`):**
```
passes_filter(dst) = (dst.station == MY_STATION_ID) or (dst.station == "*")
```

### Implementing a Producer

```
# For order messages:
envelope = {
    "v":    1,
    "type": message_type,
    "id":   generate_uuid_v4(),
    "src":  {"role": my_role, "node": my_station_id, "factory": my_factory},
    "dst":  {"role": target_role, "node": target_station, "factory": target_factory},
    "ts":   now_utc_iso8601(),
    "exp":  now_utc_iso8601() + ttl_for(message_type),
    "p":    payload_object
}

# For data channel messages:
envelope = {
    "v":    1,
    "type": "data",
    "id":   generate_uuid_v4(),
    "src":  {"role": my_role, "node": my_station_id, "factory": my_factory},
    "dst":  {"role": target_role, "node": target_station, "factory": target_factory},
    "ts":   now_utc_iso8601(),
    "exp":  now_utc_iso8601() + data_ttl_for(subject),
    "p":    {"subject": subject, "data": body_object}
}

# Add correlation ID for reply messages
if is_reply:
    envelope.cor = original_message_id

publish(topic, json_serialize(envelope))
```

---

## Analytics and Stream Processing

### Subscribing for Analytics

An analytics system can subscribe to one or both topics to build a complete picture:

- **`shingo.orders`** -- See all edge activity: order submissions, cancellations, receipts, redirects, and edge health (data channel: registration/heartbeat). Good for measuring order throughput, edge uptime, and operator behavior.

- **`shingo.dispatch`** -- See all core decisions: acknowledgements, robot assignments, delivery notifications, errors. Good for measuring fulfillment latency, error rates, and fleet utilization.

- **Both topics** -- Complete end-to-end visibility. Correlate requests with responses using `order_uuid` (in payloads) and `cor` (in envelope). Calculate round-trip times by comparing `ts` fields.

### Key Fields for Analytics

| Metric | Source Fields |
|---|---|
| Order throughput | Count `order.request` messages per time window |
| Fulfillment latency | Time between `order.request` `ts` and `order.delivered` `ts` for same `order_uuid` |
| Acknowledgement time | Time between `order.request` `ts` and `order.ack` `ts` (use `cor` or match `order_uuid`) |
| Error rate | Count `order.error` / count `order.request` per time window |
| Error breakdown | Group `order.error` by `error_code` |
| Edge uptime | Track `data` messages with subject `edge.heartbeat` gaps per `station_id`; any gap > 180s = downtime period |
| Active edges | Count distinct `station_id` from `data` messages with subject `edge.heartbeat` in last 180s |
| Orders per edge | Group `order.request` by `src.station` |
| Orders per factory | Group `order.request` by `src.factory` |
| Material demand | Group `order.request` by `payload_type_code` and `delivery_node` |
| Robot utilization | Track `waybill_id` and `robot_id` from `order.waybill` messages |
| Redirect frequency | Count `order.redirect` / count `order.request` |
| Receipt confirmation time | Time between `order.delivered` `ts` and `order.receipt` `ts` for same `order_uuid` |
| Clock drift | Compare `server_ts` in `data` with subject `edge.heartbeat_ack` with edge's local clock |

### Correlation

All messages related to a single order share the same `order_uuid` in their payload `p` field. This is the primary join key for analytics.

The envelope `cor` (correlation ID) links a reply to the specific request that triggered it. This can be used for precise request-response pairing when the same `order_uuid` may have multiple updates.

### Stream Processing Topology Example

```
                                    +--> order_throughput (count per window)
                                    |
shingo.orders ---+--> filter(type="order.request") --+--> demand_by_material (group by payload_type_code)
                 |                                    |
                 +--> filter(type="data", subject="edge.heartbeat") --+--> edge_uptime (gap detection)
                 |
                 +--> filter(type="order.receipt") ---+--> join with delivered --> receipt_latency

                                    +--> fulfillment_latency (join request + delivered on order_uuid)
                                    |
shingo.dispatch -+--> filter(type="order.delivered") -+--> deliveries_per_hour
                 |
                 +--> filter(type="order.error") -----+--> error_rate, error_breakdown
                 |
                 +--> filter(type="order.waybill") ---+--> robot_utilization
```

---

## Wire Format Examples

### Edge Registration (edge -> core, via data channel)

```json
{
  "v": 1,
  "type": "data",
  "id": "f2b0ffe2-420b-42ee-849c-cb7434233cbb",
  "src": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "dst": {"role": "core", "station": "", "factory": ""},
  "ts": "2026-02-18T10:00:00Z",
  "exp": "2026-02-18T10:05:00Z",
  "p": {
    "subject": "edge.register",
    "data": {
      "station_id": "plant-a.line-1",
      "hostname": "edge-01.local",
      "version": "1.2.0",
      "line_ids": ["line-1"]
    }
  }
}
```

### Registration Acknowledgement (core -> edge, via data channel)

```json
{
  "v": 1,
  "type": "data",
  "id": "a9c3d8f1-1234-5678-abcd-ef0123456789",
  "src": {"role": "core", "station": "core", "factory": "plant-a"},
  "dst": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "ts": "2026-02-18T10:00:01Z",
  "exp": "2026-02-18T10:05:01Z",
  "cor": "f2b0ffe2-420b-42ee-849c-cb7434233cbb",
  "p": {
    "subject": "edge.registered",
    "data": {
      "station_id": "plant-a.line-1",
      "message": "registered"
    }
  }
}
```

### Heartbeat (edge -> core, via data channel)

```json
{
  "v": 1,
  "type": "data",
  "id": "b7d4e5f6-7890-1234-abcd-567890abcdef",
  "src": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "dst": {"role": "core", "station": "", "factory": ""},
  "ts": "2026-02-18T10:01:00Z",
  "exp": "2026-02-18T10:02:30Z",
  "p": {
    "subject": "edge.heartbeat",
    "data": {
      "station_id": "plant-a.line-1",
      "uptime_s": 60,
      "active_orders": 1
    }
  }
}
```

### Order Request (edge -> core)

```json
{
  "v": 1,
  "type": "order.request",
  "id": "c8e5f6a7-8901-2345-bcde-678901abcdef",
  "src": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "dst": {"role": "core", "station": "", "factory": ""},
  "ts": "2026-02-18T10:05:00Z",
  "exp": "2026-02-18T10:15:00Z",
  "p": {
    "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
    "order_type": "retrieve",
    "payload_code": "BIN-A",
    "quantity": 1,
    "delivery_node": "line-1-station-a",
    "staging_node": "line-1-staging"
  }
}
```

### Order Acknowledgement (core -> edge)

```json
{
  "v": 1,
  "type": "order.ack",
  "id": "d9f6a7b8-9012-3456-cdef-789012abcdef",
  "src": {"role": "core", "station": "core", "factory": "plant-a"},
  "dst": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "ts": "2026-02-18T10:05:02Z",
  "exp": "2026-02-18T10:15:02Z",
  "cor": "c8e5f6a7-8901-2345-bcde-678901abcdef",
  "p": {
    "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
    "shingo_order_id": 1042,
    "source_node": "storage-rack-7"
  }
}
```

### Order Error (core -> edge)

```json
{
  "v": 1,
  "type": "order.error",
  "id": "e0a7b8c9-0123-4567-def0-890123abcdef",
  "src": {"role": "core", "station": "core", "factory": "plant-a"},
  "dst": {"role": "edge", "station": "plant-a.line-1", "factory": "plant-a"},
  "ts": "2026-02-18T10:05:01Z",
  "exp": "2026-02-18T10:35:01Z",
  "cor": "c8e5f6a7-8901-2345-bcde-678901abcdef",
  "p": {
    "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
    "error_code": "no_source",
    "detail": "No source payload found for type BIN-A"
  }
}
```

---

## Versioning

The `v` field in the envelope is an integer protocol version. The current version is `1`.

**Forward compatibility rules:**

- Consumers must ignore unknown fields in both the envelope and payload objects. Do not fail on unexpected keys.
- Consumers must reject envelopes with `v` values they do not recognize.
- New optional payload fields may be added in a minor update without incrementing `v`.
- New message types (new `type` strings) may be added without incrementing `v`. Consumers should log and ignore unknown types.
- New data channel subjects may be added without incrementing `v`. Handlers should log and ignore unknown subjects.
- Changes to existing field semantics, field removals, or envelope structure changes require incrementing `v`.

---

## Appendix: Complete JSON Schemas

### Envelope Schema

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["v", "type", "id", "src", "dst", "ts", "exp", "p"],
  "properties": {
    "v":    {"type": "integer", "const": 1},
    "type": {"type": "string", "minLength": 1},
    "id":   {"type": "string", "format": "uuid"},
    "src":  {"$ref": "#/$defs/address"},
    "dst":  {"$ref": "#/$defs/address"},
    "ts":   {"type": "string", "format": "date-time"},
    "exp":  {"type": "string", "format": "date-time"},
    "cor":  {"type": "string"},
    "p":    {"type": "object"}
  },
  "$defs": {
    "address": {
      "type": "object",
      "required": ["role", "station", "factory"],
      "properties": {
        "role":    {"type": "string", "enum": ["edge", "core"]},
        "station": {"type": "string"},
        "factory": {"type": "string"}
      }
    }
  }
}
```

### Data Payload Schema

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["subject", "data"],
  "properties": {
    "subject": {"type": "string", "minLength": 1},
    "data":    {"type": "object"}
  }
}
```
