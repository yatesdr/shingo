# Shingo Wire Protocol Specification

Verified against: b9210bce

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
| Consumer group (Edge) | `shingo-edge-{station_uid}` on `shingo.dispatch`, always derived — there is no configurable override |
| Start offset (new consumers) | **Earliest.** `kafka-go` `ReaderConfig.StartOffset` defaults to `FirstOffset` and neither service sets it, so a group id that changes replays the topic from the start of retention |
| Message key | `station_uid` on Edge publishes; `dst.station` on Core publishes where the path knows one |
| Partitions | **1** (`ensureTopics`), which makes the message key and the balancer inert today — see below |
| Topic retention | broker default (no `ConfigEntries` are sent at topic creation) |

Three corrections to what this table used to say, all of them the kind that
matter only when somebody plans a change against them:

- **Start offset was documented as "Latest (no replay of stale backlog on first
  join)". It is Earliest.** Nothing in either service assigns `StartOffset`, and
  the library's documented default is `FirstOffset`. The consequence is bounded
  rather than dangerous — `Ingestor.HandleRaw` checks expiry BEFORE the station
  filter, so a replayed dispatch envelope is dropped, not executed — but a new
  consumer group does read the retained backlog and log every message of it.
- **The message keys were documented as already set. They were not** until the
  identity change; both `Publish` implementations sent `kafka.Message{Topic,
  Value}` with no `Key`.
- **And a key alone would not have done anything.** The writer balancer was
  `kafka.LeastBytes`, which never reads `msg.Key` — it routes by accumulated
  bytes per partition (`kafka-go` v0.4.50 `balancer.go:87-109`). It is
  `kafka.Hash` now. With `NumPartitions: 1` both are still no-ops; raising the
  partition count is what activates per-station ordering and per-edge partition
  assignment, and it is a broker-side operation.

**The consumer group is why a duplicate edge goes deaf.** One partition is
assigned to exactly one group member, so two edges sharing a station id share a
group and one of them receives no dispatch traffic at all while still
heartbeating, registering and publishing. Distinct `station_uid`s are what
prevent that today — not partitioning.

---

## Delivery guarantees

The Core → Edge outbox: what order things arrive in.

Answered 2026-08-02, because the projection work needs it and nobody had
checked. The short version is at the bottom if you only need the rule.

### The guarantee

**Steady state it is FIFO, and stronger than per-station: it is global FIFO.**

- One outbox table, dequeued by `ORDER BY id` on a `BIGSERIAL`
  (`shingo-core/store/messaging/messaging.go`, `store/schema/postgres_ddl.go`).
  `station_id` and `msg_type` are stored but appear in no `ORDER BY`, no
  partition, and no priority.
- One drainer goroutine, publishing the batch in a single loop, called inline so
  cycles never overlap (`protocol/outbox/drainer.go`).
- One topic, created with one partition (`shingo-core/messaging/client.go`). With
  a single partition Kafka preserves total order regardless of key.
- Edge consumes on one read loop per topic and dispatches synchronously through
  the subject router — a map lookup and a middleware chain, not a queue per type.
  **Edge does not reorder.**

### The hole

**The drainer does not head-of-line block.** On a publish error it increments the
retry count and returns, and the caller moves to the next message. So message A
failing while message B succeeds in the same pass delivers B first and A on a
later cycle.

`IsConnected()` gates the whole cycle, so a clean disconnect is safe — nothing
publishes at all. ⚠️ On EDGE that gate does not fire: `IsConnected()` there is
`kafkaW != nil` and `Connect()` performs no I/O (`kafkago.TCP` resolves lazily),
so it is true from the first connect until shutdown regardless of the network.
It reports that a writer object exists, not that a broker is reachable — see
`kafka_last_publish_ok` on edge `/status` for the latter. It must stay that way:
`drain()` returns early on a false, so a "reachable now" answer would stop
retrying at exactly the moment retrying is the point. What reorders is a *per-message* failure: a context timeout, an
oversized payload, a broker error that recovers mid-pass.

Worse, and the part to design against: once a message exhausts `MaxRetries` the
`retries < N` filter drops it from the stream **permanently**, while everything
behind it keeps flowing. That is not a delay, it is a hole.

Three things changed around this on 2026-08-22, none of which close the hole:

- **The wake arm mutes on failure.** The drainer's doorbell fires a drain per
  enqueue, and a drain retries *every* pending row — so under a transport
  failure each enqueue spent one of the message's `MaxRetries`. A publish
  failure now stands the wake arm down until a ticker drain succeeds, putting
  the tolerance back at `MaxRetries × interval`.
- **Dead letters are kept seven days**, delivered rows one. They shared a 24-hour
  cutoff, which destroyed the evidence of a loss before anyone found it. The
  purge is now two statements in one transaction rather than one with an `OR`.
- **`MaxRetries` is still a COUNT**, so its real-world tolerance silently
  rescales whenever the drain cadence changes — which is exactly what the
  doorbell did to it. Expressing it as a duration is the open item.

### Two smaller caveats

**The partition key is empty on Core.** `PartitionKey` is only ever assigned on
Edge. Inert today because the topic has one partition — but raising the partition
count turns this into live reordering, silently. The client's own comment already
flags it.

**`ORDER BY id` is not commit order.** Two concurrent transactions can commit in
the opposite order to their assigned ids, and a drain landing in that window
publishes the higher id first. Largely defused here because `EnqueueOutbox` takes
`*sql.DB` and does a single autocommit INSERT — it never joins a caller's
transaction — so two enqueues issued in sequence from one goroutine are ordered.
It only bites when two things enqueue from different goroutines.

### The rule

If two messages about the same order are enqueued in sequence from one code path,
they arrive in that order. **Do not build on that alone.** A consumer must
tolerate arriving second-first, because one failed publish is all it takes.

For the order projection specifically: the reconcile is **day-one load-bearing,
not a backstop**. "A status arrived for an order I have never seen" is a case
that will happen, not an unreachable one, and it must be handled rather than
logged as impossible.

---

## Envelope Format

Every message on both topics is a single JSON object with the following structure. All fields are present in every message (except `cor`, which is omitted when empty).

```json
{
  "v":    1,
  "type": "order.request",
  "id":   "550e8400-e29b-41d4-a716-446655440000",
  "src":  {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "dst":  {"role": "core", "station": ""},
  "ts":   "2026-02-18T10:00:00Z",
  "exp":  "2026-02-18T10:10:00Z",
  "cor":  "previous-message-id",
  "p":    { ... }
}
```

### Envelope

<!-- generated: protocol struct Envelope -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `v` | integer |  | Version is the protocol version (Version). A consumer rejects an envelope whose version it does not know. |
| `type` | string |  | Type is the message type (a Type constant); it selects the schema of Payload. |
| `id` | string |  | ID is the message's unique id: a UUID v4, lowercase hex with hyphens. |
| `src` | [`Address`](#address) |  | Src is the sender. |
| `dst` | [`Address`](#address) |  | Dst is the intended recipient. |
| `ts` | timestamp |  | Timestamp is when the message was created, RFC 3339 in UTC. |
| `exp` | timestamp |  | ExpiresAt is the absolute time after which a receiver drops the message unprocessed, RFC 3339 in UTC. The zero time ("0001-01-01T00:00:00Z") is no expiry (NoExpiry), never "expired". |
| `cor` | string | omitempty | CorID links a reply to the message it answers, by that message's ID (an order.ack's cor is its order.request's id). Omitted when empty. |
| `p` | JSON |  | Payload is the type's body, always a JSON object. It stays raw bytes through the header decode and is decoded only once routing has chosen a handler. |

<!-- /generated -->

### Address

```json
{"role": "edge", "station": "stn-4f8a1c02b7e39d15"}
```

<!-- generated: protocol struct Address -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `role` | string |  | Role is the sender or receiver class: RoleEdge or RoleCore. |
| `station` | string |  | Station is the station identity. For an edge, its station UID, minted by Core at enrollment. For Core, its configured station id. On a dispatch to every edge, StationBroadcast ("*"). Empty is valid on a message to Core, which is a singleton. |

<!-- /generated -->

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
  "src":  {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "dst":  {"role": "core", "station": ""},
  "exp":  "2026-02-18T10:10:00Z"
}
```

This requires parsing only the header's six top-level fields. The `p` (payload) bytes are never touched.

#### RawHeader

<!-- generated: protocol struct RawHeader -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `v` | integer |  | as Envelope.Version |
| `type` | string |  | as Envelope.Type |
| `id` | string |  | as Envelope.ID |
| `src` | [`Address`](#address) |  | as Envelope.Src |
| `dst` | [`Address`](#address) |  | as Envelope.Dst |
| `exp` | timestamp |  | as Envelope.ExpiresAt; checked before any payload byte is read |

<!-- /generated -->

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

Edge → Core types ride `shingo.orders`, Core → Edge types `shingo.dispatch`; `data` rides both. The direction is
the side whose composition root registers a handler that acts on the type (the other side registers an empty one),
and the payload is the struct that handler decodes.

<!-- generated: protocol/types.go AllTypes -->

| Type | Constant | Direction | Payload |
|---|---|---|---|
| `data` | `TypeData` | both | [`Data`](#data) |
| `order.request` | `TypeOrderRequest` | Edge → Core | [`OrderRequest`](#orderrequest) |
| `order.cancel` | `TypeOrderCancel` | Edge → Core | [`OrderCancel`](#ordercancel) |
| `order.receipt` | `TypeOrderReceipt` | Edge → Core | [`OrderReceipt`](#orderreceipt) |
| `order.redirect` | `TypeOrderRedirect` | Edge → Core | [`OrderRedirect`](#orderredirect) |
| `order.complex_request` | `TypeComplexOrderRequest` | Edge → Core | [`ComplexOrderRequest`](#complexorderrequest) |
| `order.release` | `TypeOrderRelease` | Edge → Core | [`OrderRelease`](#orderrelease) |
| `order.ingest` | `TypeOrderIngest` | Edge → Core | [`OrderIngestRequest`](#orderingestrequest) |
| `order.ack` | `TypeOrderAck` | Core → Edge | [`OrderAck`](#orderack) |
| `order.waybill` | `TypeOrderWaybill` | Core → Edge | [`OrderWaybill`](#orderwaybill) |
| `order.update` | `TypeOrderUpdate` | Core → Edge | [`OrderUpdate`](#orderupdate) |
| `order.delivered` | `TypeOrderDelivered` | Core → Edge | [`OrderDelivered`](#orderdelivered) |
| `order.error` | `TypeOrderError` | Core → Edge | [`OrderError`](#ordererror) |
| `order.cancelled` | `TypeOrderCancelled` | Core → Edge | [`OrderCancelled`](#ordercancelled) |
| `order.staged` | `TypeOrderStaged` | Core → Edge | [`OrderStaged`](#orderstaged) |
| `order.skipped` | `TypeOrderSkipped` | Core → Edge | [`OrderSkipped`](#orderskipped) |

<!-- /generated -->

---

## Data Channel

The `data` message type provides a generic, extensible channel for non-order communication. Instead of adding new top-level message types for every feature, data messages use **subject-based dispatch**: the `subject` field inside the payload selects the schema, and the `data` field carries the subject-specific body.

### Data Payload Structure

```json
{
  "v": 1,
  "type": "data",
  "id": "...",
  "src": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "dst": {"role": "core", "station": ""},
  "ts": "2026-02-18T10:01:00Z",
  "exp": "2026-02-18T10:06:00Z",
  "p": {
    "subject": "edge.heartbeat",
    "data": {"station_id": "stn-4f8a1c02b7e39d15", "feeds": {"nodes": "3f9c0a1b2d4e5f60", "scene": "", "catalog": "a3c26451e6bac0e0"}}
  }
}
```

#### Data

The subjects are the [Known Subjects](#known-subjects).

<!-- generated: protocol struct Data -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `subject` | string |  | Subject selects the body's schema: a Subject constant, dotted ("edge.heartbeat"). |
| `data` | JSON |  | Body is the subject's payload. It stays raw JSON until the handler registered for Subject decodes it, so an unhandled subject never has its body read. |

<!-- /generated -->

### Two-Level Decode

Handlers process data messages in two steps:

1. **Level 1:** Decode the envelope payload into `Data` (extracts `subject` and raw `data` bytes).
2. **Level 2:** Switch on `subject` and decode the `data` field into the appropriate subject-specific struct.

This design means unhandled subjects never touch the inner body bytes -- only the `subject` string is inspected.

### Request/Reply

Data messages use the envelope's existing `cor` (correlation ID) field for request/reply patterns. No additional fields are needed. A reply sets `cor` to the original message's `id`.

### Known Subjects

The direction is which side's inbound list names the subject (`protocol.CoreInboundSubjects` /
`EdgeInboundSubjects`); the payload is the struct that side's registered handler decodes.

<!-- generated: protocol/types.go AllSubjects -->

| Subject | Constant | Direction | Payload |
|---|---|---|---|
| `bins.snapshot` | `SubjectBinsSnapshot` | Core → Edge | [`NodeBinsSnapshot`](#nodebinssnapshot) |
| `bins.watch` | `SubjectBinsWatch` | Edge → Core | [`BinWatchList`](#binwatchlist) |
| `catalog.payloads_request` | `SubjectCatalogPayloadsRequest` | Edge → Core | none (empty body) |
| `catalog.payloads_response` | `SubjectCatalogPayloadsResponse` | Core → Edge | [`CatalogPayloadsResponse`](#catalogpayloadsresponse) |
| `containment.snapshot` | `SubjectContainmentSnapshot` | Core → Edge | [`ContainmentSnapshot`](#containmentsnapshot) |
| `demand.origin` | `SubjectDemandOrigin` | Edge → Core | [`DemandOriginState`](#demandoriginstate) |
| `edge.answer` | `SubjectEdgeAnswer` | Core → Edge | [`EdgeAnswer`](#edgeanswer) |
| `edge.ask` | `SubjectEdgeAsk` | Edge → Core | [`EdgeAsk`](#edgeask) |
| `edge.heartbeat` | `SubjectEdgeHeartbeat` | Edge → Core | [`EdgeHeartbeat`](#edgeheartbeat) |
| `edge.heartbeat_ack` | `SubjectEdgeHeartbeatAck` | Core → Edge | [`EdgeHeartbeatAck`](#edgeheartbeatack) |
| `edge.register` | `SubjectEdgeRegister` | Edge → Core | [`EdgeRegister`](#edgeregister) |
| `edge.register_request` | `SubjectEdgeRegisterRequest` | Core → Edge | [`EdgeRegisterRequest`](#edgeregisterrequest) |
| `edge.registered` | `SubjectEdgeRegistered` | Core → Edge | [`EdgeRegistered`](#edgeregistered) |
| `edge.stale` | `SubjectEdgeStale` | Core → Edge | [`EdgeStale`](#edgestale) |
| `inventory.bin_epoch_refresh` | `SubjectBinEpochRefresh` | Core → Edge | [`BinEpochRefresh`](#binepochrefresh) |
| `inventory.bin_uop_delta` | `SubjectBinUOPDelta` | Edge → Core | [`BinUOPDelta`](#binuopdelta) |
| `inventory.hold_dropped` | `SubjectHoldDropped` | Edge → Core | [`HoldDropped`](#holddropped) |
| `inventory.lineside_bucket_level` | `SubjectLinesideBucketLevel` | Edge → Core | [`LinesideBucketLevel`](#linesidebucketlevel) |
| `inventory.lineside_level_report` | `SubjectLinesideLevelReport` | Edge → Core | [`LinesideLevelReport`](#linesidelevelreport) |
| `inventory.uop_adjustment` | `SubjectUOPAdjustment` | Core → Edge | [`UOPAdjustment`](#uopadjustment) |
| `node.list_request` | `SubjectNodeListRequest` | Edge → Core | [`NodeListRequest`](#nodelistrequest) |
| `node.list_response` | `SubjectNodeListResponse` | Core → Edge | [`NodeListResponse`](#nodelistresponse) |
| `node.structure_changed` | `SubjectNodeStructureChanged` | Core → Edge | [`NodeStructureChanged`](#nodestructurechanged) |
| `order.projected` | `SubjectOrderProjected` | Core → Edge | [`OrderProjection`](#orderprojection) |
| `order.status_request` | `SubjectOrderStatusRequest` | Edge → Core | [`OrderStatusRequest`](#orderstatusrequest) |
| `order.status_response` | `SubjectOrderStatusResponse` | Core → Edge | [`OrderStatusResponse`](#orderstatusresponse) |
| `plant.claims` | `SubjectPlantClaims` | Edge → Core | [`PlantClaimsReport`](#plantclaimsreport) |
| `production.ticks` | `SubjectProductionTicks` | Edge → Core | [`ProductionTicks`](#productionticks) |
| `sourcing.state` | `SubjectSourcingState` | Core → Edge | [`SourcingStateReport`](#sourcingstatereport) |
| `supply.refusal` | `SubjectSupplyRefusal` | Edge → Core | [`SupplyRefusalState`](#supplyrefusalstate) |
| `supply.refusal_snapshot` | `SubjectSupplyRefusalSnapshot` | Core → Edge | [`SupplyRefusalSnapshot`](#supplyrefusalsnapshot) |
| `supply.refusal_state` | `SubjectSupplyRefusalState` | Core → Edge | [`SupplyRefusalState`](#supplyrefusalstate) |
| `transit.bin_landed` | `SubjectBinLanded` | Core → Edge | [`BinLanded`](#binlanded) |
| `transit.bin_picked_up` | `SubjectBinPickedUp` | Core → Edge | [`BinPickedUp`](#binpickedup) |
| `transit.bin_return` | `SubjectBinReturn` | Core → Edge | [`BinReturn`](#binreturn) |

<!-- /generated -->

Every row links its payload's field table below. The table is generated from `protocol.AllSubjects()` and the
composition roots' handler registrations, so a subject added to the code without regenerating this doc fails the
docgen drift test (`TestGeneratedBlocksAreCurrent`).

**`production.downtime` is removed** (2026-10-26), with the sim's downtime model,
its only sender, and Core's projection of it into `downtime_events`. The table and
its dedup table stay in the schema, empty and unwritten.

New subjects can be added by defining a constant and a data schema -- no protocol interface changes required.

### Subject-Specific TTLs

Data messages have a default TTL of 5 minutes, but individual subjects can override this:

<!-- generated: protocol/expiry.go DataTTLFor -->

| Subject | TTL |
|---|---|
| `bins.snapshot` | 5 min |
| `bins.watch` | 5 min |
| `catalog.payloads_request` | 5 min |
| `catalog.payloads_response` | 5 min |
| `containment.snapshot` | 5 min |
| `demand.origin` | 5 min |
| `edge.answer` | **none** |
| `edge.ask` | 25 s |
| `edge.heartbeat` | 5 min |
| `edge.heartbeat_ack` | 5 min |
| `edge.register` | 5 min |
| `edge.register_request` | 5 min |
| `edge.registered` | 5 min |
| `edge.stale` | 5 min |
| `inventory.bin_epoch_refresh` | **none** |
| `inventory.bin_uop_delta` | **none** |
| `inventory.hold_dropped` | **none** |
| `inventory.lineside_bucket_level` | **none** |
| `inventory.lineside_level_report` | 5 min |
| `inventory.uop_adjustment` | **none** |
| `node.list_request` | 5 min |
| `node.list_response` | 5 min |
| `node.structure_changed` | 5 min |
| `order.projected` | 5 min |
| `order.status_request` | 5 min |
| `order.status_response` | 5 min |
| `plant.claims` | 5 min |
| `production.ticks` | **none** |
| `sourcing.state` | 5 min |
| `supply.refusal` | 5 min |
| `supply.refusal_snapshot` | 5 min |
| `supply.refusal_state` | 5 min |
| `transit.bin_landed` | **none** |
| `transit.bin_picked_up` | **none** |
| `transit.bin_return` | 5 min |

A subject not listed gets the `data` type's TTL (5 min).

<!-- /generated -->

**Nine subjects carry NO expiry at all** (`protocol.NoExpiry`; the table above is generated
from `protocol/expiry.go`, and `protocol/docs_drift_test.go` keeps this count in step with it). Every other data
subject is a snapshot whose successor carries the same truth seconds later, so discarding a late
copy costs nothing. The bin delta is an *increment*: a dropped one is a permanently wrong count
that never self-corrects. The lineside bucket level is a snapshot of one pile row, but its
successor comes only when that pile next changes (for a stranded row, never), so a dropped one
leaves Core's mirror wrong until then. Core applies a level only above the row's high-water
`SequenceID`, so a late copy is a no-op. The production tick feed is sequenced like the bin
delta — Core keys each tick on (cell, snapshot id, recorded_at), so a late copy is a no-op and
a dropped one is a fake stop in MTBF and Lost. The two Core→Edge announcements (UoP adjustment,
bin epoch refresh) are counts a station still holds after an outage longer than TypeData's
5-minute TTL — dropped, every count that station reports under the superseded generation is
discarded at Core until the next announcement. A bin's lift (`transit.bin_picked_up`, 2026-09-30)
is the only trigger for the slot's clear, a changeover supply deferred to that lift, and a
consolidation's Order B; each branch of the Edge's handler is safe on a late copy (see
`protocol/expiry.go` for the audit and the one residual shape). A carrier's drop (`transit.bin_landed`,
2026-10-10) is the only door that binds a carrier Core placed; a late copy is refused by the
seat's placement stamp (`placed_seq`), so a redelivery binds nothing twice. A dropped produce
hold (`inventory.hold_dropped`, 2026-10-20) is parts that left the count; its row is the only
record of that, and Core writes it once per envelope id, so a late copy writes it late and a
redelivery writes nothing. An ask's answer
(`edge.answer`, 2026-10-19) carries what already happened at Core; the Edge applies it by its kind,
so a late copy redraws rows that are at worst one snapshot old, and a repeat of an answer is
applied twice to the same effect.

They are safe to arrive arbitrarily late because Core guards both ends — `ApplyBinUOPDelta`
dedups on `SequenceID` via `inventory_delta_dedup`, and the stale-epoch guard routes a delta from
a superseded generation to the discrepancy audit instead of applying it. So a late copy is applied
exactly once or recorded as a discrepancy; only *dropping* it leaves no record.

Measured at Springfield before the edge was hardwired: ~17 `bin_uop_delta` a day arrived past the
five-minute default and were discarded at the ingestor, averaging 142 minutes late and peaking at
23 hours — while the edge recorded every one as sent.

⚠️ A zero TTL means **no `exp` on the wire**, not "expires now". `now.Add(0)` stamps `exp = now`,
which reads as expired on the next tick — the exact inverse. Both stamping sites go through
`zeroOrDeadline`.

---

## Message Expiry (TTL)

Every message carries an absolute expiry timestamp in the `exp` field. Receivers must check this before processing:

```
if current_utc_time > exp:
    drop message (do not process)
```

### Default TTL Values

These are the TTLs applied by the sender when creating a message. The `exp` field is always an absolute timestamp (not a duration).

<!-- generated: protocol/expiry.go DefaultTTLFor -->

| Type | TTL |
|---|---|
| `data` | 5 min |
| `order.request` | 10 min |
| `order.cancel` | 10 min |
| `order.receipt` | 30 min |
| `order.redirect` | 10 min |
| `order.complex_request` | 10 min |
| `order.release` | **none** |
| `order.ingest` | **none** |
| `order.ack` | 10 min |
| `order.waybill` | 30 min |
| `order.update` | 10 min |
| `order.delivered` | 60 min |
| `order.error` | 30 min |
| `order.cancelled` | 30 min |
| `order.staged` | 10 min |
| `order.skipped` | 30 min |

A type not listed gets `FallbackTTL` (10 min). A `data` envelope's TTL is its subject's (below).

<!-- /generated -->

`order.release` and `order.ingest` carry no expiry: a dropped release strands a staged robot; a dropped ingest
leaves a full bin unconfirmed. Core fences a late ingest by its `bin_epoch`, and the release's `station_wait` echo
makes a late release for a passed wait a no-op. Neither is dead-lettered.

### Why Absolute Timestamps

Relative TTLs (durations) are ambiguous when messages sit in a Kafka partition during edge reconnection. Absolute timestamps make expiry calculation trivial for any consumer: compare `exp` to your current UTC clock. No need to know when the message was enqueued or how long it sat in transit.

---

## Payload Schemas

All payloads are JSON objects nested inside the `p` field of the envelope. Fields marked "omitempty" may be absent from the JSON when their value is the zero value for that type (empty string, 0, false).

### Data Channel Schemas

These schemas are used as the `data` field inside `data`-type messages. See [Data Channel](#data-channel).

#### EdgeRegister

Sent by an edge node on startup and on every reconnect via `data` message with subject `edge.register`.

**It is no longer an upsert.** Core resolves the station by `station_uid` and
UPDATEs it; a uid Core has never enrolled is REFUSED and writes nothing. An edge
cannot bring a station into existence by asserting a name — enrollment is a
separate, deliberate act on Core (`POST /api/edges/enroll`,
`shingo-core/www/router.go:415`). For the three-way split of station identity
that this rests on, see [terminology.md § Station](terminology.md#station).

```json
{
  "station_id": "stn-4f8a1c02b7e39d15",
  "hostname":   "edge-01.local",
  "instance":   "9a3f7c21d0b45e88",
  "version":    "1.2.0"
}
```

<!-- generated: protocol struct EdgeRegister -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station_id` | string |  | StationID is the station UID, minted by Core at enrollment. Core resolves the station by it and refuses a uid it never enrolled. A plant from before station UIDs carries its old station string as its uid. |
| `hostname` | string |  | Hostname is the OS hostname: attribute data and a weak duplicate signal, since two Pis flashed from one SD image share it. |
| `instance` | string | omitempty | Instance is a random id drawn once per process at boot; absent means "cannot judge". It is the only field that separates two clones of one SD card, and Core alarms when a displaced instance returns — one machine cannot produce that, because a reboot draws a value it has never used and a live process keeps the one it holds. |
| `version` | string |  | Version is the edge application's software version. |
| `timezone` | string | omitempty | Timezone is the edge's resolved plant display zone (IANA name, e.g. "America/Chicago"), sent so Core can show each station's clock zone on the /edges page. Empty on an unconfigured edge — the table shows the emptiness, which is the "go set it" signal. Additive: an old Core drops the unknown field (no DisallowUnknownFields anywhere); an old edge sends nothing and a new Core reads absence as "unknown". |
| `schema_version` | integer | omitempty | SchemaVersion is the Edge's applied schema_migrations version at process start — the version its boot migration left the database at (0 for an edge that predates the versioned runner). Sent on register, never the heartbeat: the version cannot change while the process lives, so repeating it every 60s would be noise. Core's /edges page shows it beside Core's own, so an edge left behind on a migration is visible without an SSH round trip to the box. -1 means "the edge knows but could not read it" (a failed Latest at boot); 0 with a blank is the honest pre-runner answer, not a fault. Additive, like Timezone: an old Core drops the unknown field and an old edge sends nothing, which a new Core reads as pre-runner. |
| `catalog` | [`CellCatalogEntry`](#cellcatalogentry)[] | omitempty |  |

<!-- /generated -->

##### CellCatalogEntry

<!-- generated: protocol struct CellCatalogEntry -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `cell_label` | string |  |  |
| `processes` | [`CellProcessBinding`](#cellprocessbinding)[] |  |  |

<!-- /generated -->

##### CellProcessBinding

<!-- generated: protocol struct CellProcessBinding -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `process_id` | integer |  |  |
| `style_id` | integer |  |  |
| `plc_name` | string |  |  |
| `tag_name` | string |  |  |

<!-- /generated -->

`line_ids` is **RETIRED** (v66). It sent `[cfg.LineID]` regardless of any station
override, so it was always `["line-1"]`, and its only consumer composed
`station + "." + line` into a dashboard scope no order row could match. The
column is dropped from `edge_registry` too.

**Payload copies of the station are gone** from the since-removed
`production.downtime` (`DowntimeEvent`) and the retired `lineside_bucket_delta` (which carried it twice in one
envelope). `BinUOPDelta` still carries `station`, but no handler reads it: every handler reads
`Envelope.Src.Station`. The old `if station == "" { station = env.Src.Station }`
reconciliation was a rule with two possible answers that only ever produced one
because a plant had exactly one station; distinct per-edge identity is precisely
what makes the two copies able to disagree.

#### EdgeHeartbeat

Sent every 60 seconds by each edge node via `data` message with subject `edge.heartbeat`.

```json
{
  "station_id": "stn-4f8a1c02b7e39d15",
  "feeds":      {"nodes": "3f9c0a1b2d4e5f60", "scene": "", "catalog": "a3c26451e6bac0e0"}
}
```

<!-- generated: protocol struct EdgeHeartbeat -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station_id` | string |  | StationID is the heartbeating station's UID, as registered. |
| `timezone` | string | omitempty | Timezone mirrors EdgeRegister.Timezone on every heartbeat. Edge reads it once at boot (not live), so the wire value flips only when a process that actually renders the new zone comes up — Core's table shows what each edge is ON, never "saved but not yet restarted". |
| `tick_pending` | integer | omitempty | TickPending and TickOldestUnsentAgeMS are the production-tick shipper's lag at send time: shippable counter_snapshots rows past its cursor, and the age of the oldest of them in ms (0 when none). Core keeps the latest per station and flags one whose feed has stalled. Absent from an Edge without the shipper, or when the read failed — never a zero nobody measured. |
| `tick_oldest_unsent_age_ms` | integer | omitempty | See `tick_pending`. |
| `feeds` | object (string → string) |  | Feeds names, per Core feed this Edge holds (FeedContainment, ...), the digest it holds — "" when it holds nothing yet. Core answers each key on the ack and sends the feed where they differ (see feeds.go). NOT omitempty: nil is how Core recognises an Edge that predates feeds and does nothing new for it, so a new Edge always sends at least {}. |

<!-- /generated -->

#### EdgeRegistered

Acknowledges a successful edge registration via `data` message with subject `edge.registered`. The `cor` (correlation ID) field in the envelope links back to the original registration message.

```json
{
  "station_id": "stn-4f8a1c02b7e39d15",
  "message": "registered"
}
```

<!-- generated: protocol struct EdgeRegistered -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station_id` | string |  | StationID echoes the registered station's UID. |
| `message` | string | omitempty | Message is a human-readable status message. |

<!-- /generated -->

#### EdgeHeartbeatAck

Acknowledges a heartbeat via `data` message with subject `edge.heartbeat_ack`. Provides server timestamp for optional clock drift detection.

```json
{
  "station_id": "stn-4f8a1c02b7e39d15",
  "server_ts":  "2026-02-18T10:01:01Z",
  "feeds":      {"nodes": "3f9c0a1b2d4e5f60", "scene": "9d1e", "catalog": "a3c26451e6bac0e0"},
  "claims":     {"PR1": "f407896ad88a27dd"}
}
```

<!-- generated: protocol struct EdgeHeartbeatAck -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station_id` | string |  | StationID echoes the heartbeating station's UID. |
| `server_ts` | timestamp |  | ServerTS is Core's clock when it answered. The edge compares it with its own and shows the offset on /status; the offset includes Core's outbox delay. |
| `timezone` | string | omitempty | Timezone is the plant's IANA zone as EXPLICITLY CONFIGURED on Core, and empty when Core has not been told either. It is deliberately not Core's RESOLVED zone: resolving folds in Core's own default, so an unconfigured site would broadcast that default to every edge with the confidence of an answer, the blank-zone warnings would clear, and a value nobody chose would quietly become the plant's clock. Empty propagates nothing and leaves every box visibly unset, which is the honest state. An edge adopts it only to FILL A BLANK — see the edge handler. Config arriving over a network must never overwrite a zone somebody typed locally. Additive, like EdgeRegister.Timezone: an old edge ignores the unknown field, and an old Core sends nothing, which a new edge reads as "Core has no opinion" rather than as an instruction. |
| `feeds` | object (string → string) |  | Feeds is Core's current digest for each key the heartbeat named that Core could read. A key Core failed to read is absent — not "", which would be a digest of nothing — so the Edge confirms nothing it was not told. NOT omitempty: a new Core always sends at least {}, and nil is how the Edge recognises an older Core and keeps its own timers running. |
| `claims` | object (string → string) |  | Claims is, for every process Core holds a plant-claims report for from this station, the digest that arrived with that process's last report. The Edge re-publishes a process whose digest differs or is missing, and publishes an empty report for a name it no longer has. nil means Core could not read them (or is older); {} means it holds none. |
| `bin_watch` | string | omitempty | BinWatch is the digest of the bins watch list Core holds for this Edge, as it arrived on SubjectBinsWatch; "" when Core holds none. The Edge publishes its list when this differs from its own. A POINTER, AND ABSENT IS AN ANSWER: nil (the key left out) is a Core without the bins feed, and the Edge then reads bins as it did before and publishes nothing. An Edge that sends no Feeds is not answered either. |
| `asks` | string[] | omitempty | Asks is every ask kind this Core answers (protocol.AskKinds, as registered), sorted. The Edge sends an ask of a kind only while the last ack named it, and refuses the act otherwise: nil (absent) is a Core without the ask pair. Not sent to an Edge that sends no Feeds. |

<!-- /generated -->

#### Node bins feed

The bins at the nodes an edge's operator stations draw, held by the edge as a copy. The edge owns the watch
list and Core quotes its digest on the ack (`bin_watch`); Core owns the snapshot, feed key `bins`, one per edge.
Both digests are in `protocol/feeds.go` (`BinWatchDigest`, `NodeBinsDigest`).

#### BinWatchList

Sent on `bins.watch`: the Core node names an edge's operator stations draw, sorted, each once, with their digest.
Core keeps the latest per edge in memory, quotes its digest on every heartbeat ack (`bin_watch`) and answers each
list with that edge's `bins` snapshot.

<!-- generated: protocol struct BinWatchList -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `digest` | string |  | Digest is BinWatchDigest(Nodes). Core quotes it back on EdgeHeartbeatAck.BinWatch as it arrived and never recomputes it. |
| `nodes` | string[] |  | Nodes are the Core node names this edge's operator stations draw, sorted, each once. |

<!-- /generated -->

#### NodeBinsSnapshot

Sent on `bins.snapshot`: the bins at the nodes one edge watches, under feed key `bins`. Sent to that edge alone on
receipt of its watch list, after a change at a watched node, and when its heartbeat quotes a different `bins`
digest. The edge holds it in memory only.

`NodeBinsSnapshot` (core -> one edge): `digest`, `rows` and `groups`. One row per watched name: `node`,
`occupied`, `bin_id`, `bin_label`, `bin_type_code`, `bare`, `payload_code`, `manifest`, `manifest_confirmed`,
`stocked`, `totes_on_cart`, `totes_per_layer`, `uop_capacity` (the part's), and for a held partial only (an
unfinished carrier waiting in a partial home of its part) `held_since`, `held_for` and `held_uop`. A node Core does
not know, or one with no bin, is `occupied: false` and nothing else. **No count is in a row** but a held partial's,
which nothing draws from while it waits: `stocked` is the bin's count above zero, so a count that moves without
crossing zero sends nothing. No time that moves and nothing about the asker is in it. Each group line is an outbound group,
with levels, of a consume loader with a watched window: `group`, `wants` (`bin_type`, `layers`, `want`) and
`assignments` (`node`, `bin_type`, `layers`, `group`, `none_short`). `layers` absent is the type at any height; a number is a wanted
height in layers, and `0` is tops & bottoms (a tote cart with no totes on it), the type's own row, sent only where
the group keeps one. `group`, only on a row that has it, names a source of the group the choice is for, the group
itself being whole; `none_short`, likewise, says no group is short of this cart, with `bin_type` blank. Both are words
for the card: the push still names the unloader's one outbound and Core chooses at its door, so an Edge that does not
know them ignores them, and Core deploys first. A plant with no levels sends `groups: []`.

<!-- generated: protocol struct NodeBinsSnapshot -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `digest` | string | omitempty | Digest is NodeBinsDigest of ListDigest, Rows and Groups, computed over the value sent: the bins feed's digest. |
| `list_digest` | string |  | ListDigest is the Digest of the BinWatchList this answers, as it arrived. |
| `rows` | [`NodeBinsRow`](#nodebinsrow)[] |  | Rows are one per watched name, in the list's order, each watched group's member rows directly after it. |
| `groups` | [`NodeBinsGroup`](#nodebinsgroup)[] |  | Groups are the outbound groups, with levels, of the consume loaders that have a watched window; [] for a plant with no levels. |

<!-- /generated -->

##### NodeBinsRow

<!-- generated: protocol struct NodeBinsRow -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node` | string |  |  |
| `state` | string |  |  |
| `members` | string[] | omitempty | Members is a watched group's members, each named "group.member", in the order Core lists the group's non-synthetic children (by name). |
| `occupied` | boolean |  |  |
| `bins` | integer | omitempty |  |
| `bin_id` | integer | omitempty |  |
| `bin_label` | string | omitempty |  |
| `bin_type_code` | string | omitempty |  |
| `bare` | boolean | omitempty |  |
| `payload_code` | string | omitempty |  |
| `manifest` | string | omitempty |  |
| `manifest_confirmed` | boolean |  |  |
| `epoch` | integer | omitempty | Epoch is the resident carrier's generation (bins.delta_epoch). |
| `stocked` | boolean |  | Stocked is the bin's count above zero. A count at or below zero is not stocked; a count that moves and stays above zero changes nothing here. |
| `partial` | boolean |  | Partial is the count above zero and below UOPCapacity; false with no capacity. |
| `totes_on_cart` | integer | omitempty |  |
| `totes_per_layer` | integer | omitempty |  |
| `uop_capacity` | integer | omitempty | UOPCapacity is the capacity of the carrier's part, 0 with no part. HeldSince, HeldFor and HeldUOP are set for a HELD partial only, an unfinished carrier waiting in a partial home of its part to be called back: when it entered the home (RFC 3339), the line whose claim keeps it there, and its declared count. None of them ticks: nothing draws from a held partial. |
| `held_since` | string | omitempty | See `uop_capacity`. |
| `held_for` | string | omitempty | See `uop_capacity`. |
| `held_uop` | integer | omitempty | See `uop_capacity`. |
| `inbound` | [`NodeBinsInbound`](#nodebinsinbound)[] | omitempty | Inbound is the live orders whose delivery resolves at this node, in order id order. |

<!-- /generated -->

##### NodeBinsInbound

<!-- generated: protocol struct NodeBinsInbound -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  |  |
| `bin_type` | string | omitempty |  |
| `payload` | string |  |  |
| `from` | string |  |  |

<!-- /generated -->

##### NodeBinsGroup

<!-- generated: protocol struct NodeBinsGroup -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `group` | string |  |  |
| `wants` | [`NodeBinsWant`](#nodebinswant)[] |  |  |
| `assignments` | [`NodeBinsAssignment`](#nodebinsassignment)[] |  |  |

<!-- /generated -->

##### NodeBinsWant

<!-- generated: protocol struct NodeBinsWant -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `bin_type` | string |  |  |
| `layers` | integer | omitempty |  |
| `want` | integer |  |  |

<!-- /generated -->

##### NodeBinsAssignment

<!-- generated: protocol struct NodeBinsAssignment -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node` | string |  |  |
| `bin_type` | string |  |  |
| `layers` | integer | omitempty |  |
| `group` | string | omitempty | Group is the group the choice is for, when that is not the group this line is the wants of: a source of it, short of the type while the kanban is whole. |
| `none_short` | boolean | omitempty | NoneShort says no group of the line is short of this cart: BinType is blank, and nothing is lit. |

<!-- /generated -->

#### EdgeStale

Sent by core to notify an edge that it has been marked stale (missed heartbeats).

```json
{
  "station_id": "stn-4f8a1c02b7e39d15",
  "message":    "Edge marked stale: no heartbeat for 180s"
}
```

<!-- generated: protocol struct EdgeStale -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station_id` | string |  | the stale station's UID |
| `message` | string |  | a human-readable detail |

<!-- /generated -->

#### EdgeRegisterRequest

Sent by core to request that an edge re-register (e.g., after core restart or edge registry cleanup).

```json
{
  "station_id": "stn-4f8a1c02b7e39d15",
  "reason":     "Core restarted, edge registry cleared"
}
```

<!-- generated: protocol struct EdgeRegisterRequest -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station_id` | string |  | the station that should re-register |
| `reason` | string |  | why it should |

<!-- /generated -->

#### NodeListRequest

Sent by edge to request the core's authoritative node list.

```json
{}
```

<!-- generated: protocol struct NodeListRequest -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `scene_revision` | string | omitempty |  |

<!-- /generated -->

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

<!-- generated: protocol struct NodeListResponse -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `nodes` | [`NodeInfo`](#nodeinfo)[] |  | Nodes is Core's node list. |
| `loaders` | [`LoaderInfo`](#loaderinfo)[] | omitempty | Loaders is the Core-owned bin-loader aggregate (each loader with its positions and payloads), which the Edge caches in core_loaders and its loader resolvers read. |
| `payload_bin_types` | [`PayloadBinTypeInfo`](#payloadbintypeinfo)[] | omitempty |  |
| `family` | object (string → string) | omitempty | Family is each bin type's carrier family, by bin-type code: two types are one physical cart exactly when their strings are equal, and a CLEAR offers only those. Per type, not per PayloadBinTypes row, because a type's family does not vary by payload. Bare markers are not in it: nobody chooses one. Absent from an older Core, and an older Edge ignores it; an Edge holding none does not filter its picker. |
| `scene_points` | [`ScenePointInfo`](#scenepointinfo)[] | omitempty | ScenePoints and SceneEdges are the vendor map's own universe of locations and the drivable segments between them. Sibling slices for the same reason as the two above, and additive in the same way. SHINGO WORKS IN APs and always has, so the node list is only the subset of map points that Shingo gave a job to. A key route is expressed in the VENDOR's universe — a plain waypoint (class LM) is its primary use — so validating one against the node list refuses a correct route confidently. Core has mirrored the whole scene graph since the SEER adapter was written; it simply never sent it down. NAMES EVERY TIME, GEOMETRY ON CHANGE. The key-route validator and the "absence is never a finding" guards read the full name set on every sync, so names are never left off. The coordinates and handles are what the station's cell picture draws from, and they are the bulk of the message — so Core stamps SceneRevision on every response and sends the geometry only when the request's revision does not match it. An Edge that received geometry keeps it (durably) and quotes the revision back. |
| `scene_edges` | [`SceneEdgeInfo`](#sceneedgeinfo)[] | omitempty | See `scene_points`. |
| `scene_revision` | string | omitempty | SceneRevision identifies the scene the slices above were cut from. Empty when the scene could not be read in full, in which case the Edge must keep whatever geometry it already has. It is a hash of the rows, not a counter: two Cores, or one Core restarted, answer the same scene with the same revision. |
| `digest` | string | omitempty | Digest is the FeedNodes digest of Nodes, Loaders, PayloadBinTypes and Family as this reply carries them (NodesDigest), on every path that builds the reply. An Edge holding that digest already applies none of the four. Absent from an older Core, whose replies apply as they always did. |

<!-- /generated -->

##### NodeInfo

<!-- generated: protocol struct NodeInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `name` | string |  | Name is the node's name. |
| `node_type` | string |  | NodeType is the node type code (NGRP, LANE); empty for a physical node. |
| `kanban` | boolean | omitempty | Kanban is true for a group Core keeps a level of empties in (a group with a level), so the Edge can refuse that group as a full-bin source when a claim is saved, as Core refuses it for a loader. Omitted when false, and an older Core never sends it: absent reads as false, so an Edge talking to such a Core refuses nothing on this account. |
| `accepts_partials` | boolean | omitempty | AcceptsPartials marks a window of a loader whose windows take partly filled carriers (accept_partials), and every group above one. A claim's partial home may not be one: the two say opposite things about the same carriers. False for every other node, and from a Core predating the field. |

<!-- /generated -->

##### LoaderInfo

<!-- generated: protocol struct LoaderInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `name` | string |  | Name is the loader's display name. |
| `loader_key` | string |  | LoaderKey is the loader's IDENTITY — the opaque token Core mints from bin_loaders.id as "loader:&lt;id>". It is the Edge cache key that groups a loader's windows for the never-2N budget and what the pooled threshold signal names. The loader has no node id of its own (a multi-window loader spans many nodes); its delivery targets are the explicit member nodes in Positions. domain.LoaderID stays a string newtype so a future UUID swap is invisible. |
| `role` | string |  | Role is produce (a loader) or consume (an unloader): ClaimRole's values. |
| `layout` | string |  | Layout is LoaderLayoutSharedWindow or LoaderLayoutDedicatedPositions: the one discriminator between a window and a dedicated position. |
| `replenishment` | string |  | Replenishment is LoaderReplenishmentOperator or LoaderReplenishmentThreshold. |
| `outbound_dest` | string | omitempty |  |
| `inbound_source` | string | omitempty |  |
| `funnel_windows` | boolean | omitempty | FunnelWindows restricts a shared_window loader to ONE window at a time: empties funnel to its first window on a budget of 1 instead of spreading one bin per window. Stated as the restriction so the zero value — which is also what a Core predating this field sends — means "spread", the behaviour every loader has today. Ignored for dedicated_positions loaders. |
| `leaves_bare` | boolean | omitempty | LeavesBare marks the stage-1 half of a two-stage unloader: its CLEAR leaves the cart with no bin on it, and Core stamps the cart's own bare marker, derived from the cart's type. The Edge posts a blank code there and shows a one-tap panel with no type on it — the marker is Core's bookkeeping and never reaches an operator. False — also what a Core predating this field sends — is an ordinary unloader. |
| `takes_bare` | boolean | omitempty | TakesBare marks the stage-2 half of a two-stage unloader, LeavesBare's mirror: its windows take the bare carts stage 1 leaves, and a person there puts a bin back on. The Edge draws its windows as an unloader's (the cart cards, the held partial's card, the CLEAR) whatever its role, and never asks for anything into them: stage 1 or Core's stage-2 pull brings every cart. Core sets it for any loader some stage 1 names as its second stage. False, also what a Core predating this field sends, is any other loader. |
| `auto_push` | boolean | omitempty | AutoPush re-pulls this unloader's next full when a CLEAR, a PUSH EMPTY or its own empty-out landing frees a window. It lives on the loader because the stored claim that used to carry it is retired and SynthClaim never set it. False — also what a Core predating this field sends — is what every Core-owned unloader does today. |
| `pulled_directly` | boolean | omitempty | PulledDirectly marks a loader whose finished carriers the process pulls straight off its windows (a two-stage unloader's stage 2, or any loader but a stage 1): the Edge's CLEAR there sends the carrier nowhere, quietly, and the board says it waits for the line. False, also what a Core predating this field sends, is today's: the empty-out to OutboundDest. |
| `board_hides_idle` | boolean | omitempty | BoardHidesIdle: the station's board leaves off cards and homes with nothing to do (an operator-ordered station still shows an empty spot). BoardShowsAllParts: the board lists every part the station takes, not only what a running style needs. Display only. A Core predating these fields sends both false, so deploy Core first. |
| `board_shows_all_parts` | boolean | omitempty | See `board_hides_idle`. |
| `positions` | [`LoaderPosition`](#loaderposition)[] | omitempty | Positions are the loader's member homes: its windows (shared_window) or its dedicated positions. |
| `payloads` | [`LoaderPayloadInfo`](#loaderpayloadinfo)[] | omitempty | Payloads is the shared allowed set, on a shared_window loader only. |

<!-- /generated -->

##### LoaderPosition

<!-- generated: protocol struct LoaderPosition -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `core_node_name` | string |  | CoreNodeName is the position's (or window's) node NAME, the Edge's key space. |
| `payload_code` | string |  | PayloadCode is the payload a dedicated position is bound to; empty for a window, a buffer, or a position not yet assigned one (HomeKind tells the last two apart). |
| `home_kind` | string | omitempty | HomeKind separates the two DIFFERENT things a blank PayloadCode can mean on a dedicated loader: a kept-partial BUFFER slot, and a HOME position the operator dragged in but has not assigned a payload to yet. Core has always distinguished them — bin_loader_homes.home_kind, which InSourcePool reads to keep an unassigned home OUT of the loader's source pool while a buffer stays in. It just never said so on the wire, so the Edge inferred "buffer" from an empty payload and reached the opposite answer for an unpinned home. Window-vs-dedicated is the parent loader's Layout; this resolves home-vs-buffer within a dedicated loader. Additive and unsentinelled, like Ordinal: a Core that predates the field sends nothing, every position decodes "", and the reader falls back to the empty-payload inference — exactly what it did before. See LoaderHomeKind* below. |
| `ordinal` | integer | omitempty | Ordinal is where the operator put this window. An admin screen lets them drag a loader's windows into the order they want filled; Core persists that and this is how it reaches the plant. It used to go no further than Core. There was no field here and no column in the Edge's cache, so the arrangement was accepted, stored, synced downward, and discarded on arrival — the Edge re-sorted by name. Since the funnel case delivers to "the first window" and spreading fills free windows in order, that decided which window a carrier physically went to. Additive and unsentinelled: a Core that predates the field sends nothing, every position decodes 0, every comparison ties, and the reader falls through to its name ordering — which is exactly what it did before. See shingo/shared/windoworder for the rule both sides share. |

<!-- /generated -->

##### LoaderPayloadInfo

<!-- generated: protocol struct LoaderPayloadInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `payload_code` | string |  | one payload of the allowed set |

<!-- /generated -->

The loader's UOP threshold is not on this wire (dropped 2026-10-28): it is Core's alone, and an older Core still sending `uop_threshold` is ignored on decode.

##### PayloadBinTypeInfo

<!-- generated: protocol struct PayloadBinTypeInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `payload_code` | string |  |  |
| `bin_type_code` | string |  |  |

<!-- /generated -->

##### ScenePointInfo

<!-- generated: protocol struct ScenePointInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `instance_name` | string |  |  |
| `class_name` | string |  |  |
| `pos_x` | float | omitempty | PosX / PosY / Dir are the point's world coordinates and heading, present only on a response that carries geometry (see NodeListResponse). |
| `pos_y` | float | omitempty | See `pos_x`. |
| `dir` | float | omitempty | See `pos_x`. |

<!-- /generated -->

##### SceneEdgeInfo

<!-- generated: protocol struct SceneEdgeInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `from` | string |  |  |
| `to` | string |  |  |
| `from_x` | float | omitempty | Endpoints, present only with geometry. Pointers for the same (0,0) reason as ScenePointInfo. |
| `from_y` | float | omitempty |  |
| `to_x` | float | omitempty |  |
| `to_y` | float | omitempty |  |
| `ctrl1_x` | float | omitempty | Cubic-Bezier control handles. nil on a straight segment. |
| `ctrl1_y` | float | omitempty |  |
| `ctrl2_x` | float | omitempty |  |
| `ctrl2_y` | float | omitempty |  |

<!-- /generated -->

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

<!-- generated: protocol struct CatalogPayloadsResponse -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `payloads` | [`CatalogPayloadInfo`](#catalogpayloadinfo)[] |  | Payloads is Core's payload catalog. |
| `digest` | string | omitempty | Digest is the FeedCatalog digest of Payloads (CatalogDigest). Absent from an older Core. |

<!-- /generated -->

##### CatalogPayloadInfo

<!-- generated: protocol struct CatalogPayloadInfo -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `id` | integer |  | Core's payload id |
| `name` | string |  | the payload template's name |
| `code` | string |  | the unique payload code |
| `description` | string |  | a human-readable description |
| `uop_capacity` | integer |  | production cycles a full load supports |
| `catid` | string | omitempty | CATID is the payload's part identity from its Core manifest: the DISTINCT part numbers, comma-joined (a single-part payload sends one value; a multi-part kit sends the full list). The edge splits it back into the style's part-identity SET — membership semantics, so a multi-part payload contributes every part it carries rather than nothing. Empty when the payload's manifest carries no part numbers. |
| `manifest` | [`ManifestItem`](#manifestitem)[] | omitempty | Manifest is the payload's manifest TEMPLATE: its lines in Core's order (payload_manifest by id), each a part and its per-cycle ratio. Absent when the payload has no lines, so a row without a template encodes, and digests, exactly as it did before the lines rode the catalog (TestCatalogDigest_ManifestLines). The Edge's load modal reads it from its catalog copy instead of asking Core per tap. CATID is derivable from the same rows and is still sent as its own field. |

<!-- /generated -->

##### ManifestItem

<!-- generated: protocol struct ManifestItem -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `part_number` | string |  |  |
| `parts_per_cycle` | integer |  |  |
| `description` | string |  |  |

<!-- /generated -->

#### PlantClaimsReport

Published by Edge on the `plant.claims` subject so Core can mirror the plant
spec's claim set and continuously compute sourceability (what every process can
source and change over to) without depending on Edge being up. Edge stays the
source of truth for the plant spec; this feed plumbs it onto Core. This is a
**subject on the existing `shingo.orders` topic**, not a new topic.

One message is the FULL claim set for ONE process. Core replaces its mirror for
that process on every message, so a series of messages (one per process) is a
full snapshot. Edge publishes:

- on every plant-spec change (style or claim edit),
- a periodic full snapshot (every few minutes), and
- a full snapshot on boot/reconnect.

The periodic + boot snapshots are how a late-joining or restarted Core rebuilds
its mirror — no Kafka compaction is used. The message carries a `config_gen`
stamp; Core keeps the newest per process and ignores an older snapshot landing
after a newer one (the out-of-order guard).

Loaders/unloaders are **excluded**: a `manual_swap` claim never appears in
`claims`. They enter the sourceability computation as pool supply/demand via the
Core-owned loader aggregate, not as style claims.

The value schema is **additive-only**. Future fields append with `omitempty`; an
older Core ignores unknown fields, and an older Core that has not registered the
`plant.claims` subject logs-and-drops the whole message (mixed-version no-op).

```json
{
  "process_id": "SNF2",
  "config_gen": 42,
  "styles": [
    {
      "style_id": "SNF2-A",
      "claims": [
        {
          "core_node_name": "STOR-01",
          "role": "consume",
          "swap_mode": "single_robot",
          "payload_code": "BIN-A",
          "allowed_payload_codes": ["BIN-A"],
          "uop_capacity": 100,
          "reorder_point": 20
        }
      ]
    }
  ]
}
```

<!-- generated: protocol struct PlantClaimsReport -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `process_id` | string |  | ProcessID is the Edge plant-spec process identifier (the process's Name on Edge — e.g. "SNF2"). Core mirrors it verbatim as the cache key; it is the logical partition for the sourceability computation. |
| `styles` | [`PlantClaimsStyle`](#plantclaimsstyle)[] |  | Styles is the full set of styles this process can run, each with its claim list. A style with no sourceability-relevant claims still appears (empty Claims) so Core knows the style exists for an all-styles recompute. |
| `config_gen` | integer | omitempty | ConfigGen is a stale-report guard Core honours but no Edge sets: the publisher leaves it zero, and zero always applies. Core would skip a report whose ConfigGen is older than the one its mirror holds for the process; with every report at zero that never happens, and a lost or out-of-order report is healed by Digest instead. Kept on the wire so a sender that does stamp it is still understood. |
| `digest` | string | omitempty | Digest is ClaimsDigest of this report, set by the Edge where it builds it. Core stores it per process and quotes it on the heartbeat ack, so a lost or out-of-order report is noticed and re-sent. Absent from an older Edge. |
| `positions` | string[] | omitempty | Positions are the core node names, sorted and de-duplicated, where this process binds carriers that no claim in Styles names: its loader windows and homes and its FG drains (a Core-owned loader has no claim row on the Edge). Core mirrors them so the landing's address read finds this station at those seats. omitempty: a process with none encodes, and digests, exactly as before the field existed, so no Edge republishes on upgrade for nothing; the digest carries the field, so a change re-sends it. |

<!-- /generated -->

##### PlantClaimsStyle

<!-- generated: protocol struct PlantClaimsStyle -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `style_id` | string |  | StyleID is the Edge plant-spec style identifier (the style's Name on Edge within ProcessID). Together (ProcessID, StyleID) is the sourceability computation key. |
| `claims` | [`PlantClaim`](#plantclaim)[] |  | Claims is the set of sourceability-relevant node claims for this style. Manual_swap (loader/unloader) claims are excluded at the publisher; only the material-flow claims the netting reads appear. |
| `active` | boolean | omitempty | Active marks the style the process is currently running — Edge's processes.active_style_id, which is the same field Edge already resolves node claims through (findActiveClaim keys on it). At most one style per process carries it. Additive: an older Core simply never sets its mirror column, and the sourcing page keeps saying no style is marked running. Absent on a report from an older Edge, which is indistinguishable from "this process has no active style set" — both mean Core must not claim to know what is running. |

<!-- /generated -->

##### PlantClaim

<!-- generated: protocol struct PlantClaim -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `core_node_name` | string |  | CoreNodeName is the node the claim binds (Core's node-name key space). The netting nets bin/order/reservation state against this. |
| `role` | string (`ClaimRole`) |  | Role is the material-flow direction: "consume" (node pulls material from upstream) or "produce" (node pushes material downstream). |
| `swap_mode` | string (`SwapMode`) |  | SwapMode is the changeover/dispatch mode for the claim. Carried so the computation can distinguish the flow modes; manual_swap never appears (excluded at the publisher). |
| `payload_code` | string |  | PayloadCode is the binding payload for non-manual_swap claims. For a single-payload claim this is the value the netting matches against. |
| `containment_destination` | string | omitempty | ContainmentDestination is where a contained payload's bins divert instead of the claim's ordinary FG outbound destination (quality containment). Blank = unconfigured = the divert is inert for this claim. Core resolves the divert at dispatch against its own authoritative containment state; this field only names WHERE. |
| `partial_destination` | string | omitempty | PartialDestination is where the claim's carrier goes when it leaves the line unfinished: its part set, its count above 0 and below capacity (a node or a group). Blank = it goes where a full goes. Core judges "unfinished" from its own count and chooses the drop; this field only names WHERE. |
| `allowed_payload_codes` | string[] |  | AllowedPayloadCodes is the effective payload set this claim accepts (ClaimAllowedPayloads on Edge). For most claims this is just [PayloadCode]; carried as the canonical set the netting reads. |
| `uop_capacity` | integer |  | UOPCapacity is the bin capacity (UOP) for the claim's payload — the denominator context for time-to-empty. Read by the at-risk tier. |
| `reorder_point` | integer |  | ReorderPoint is the role-dependent replenishment trigger, and BOTH roles read it as a UOP count. It said "produce: bin-count floor" and that semantics did not survive: the floor was the loader's, loader replenishment became Core-owned, and no reader of this field ever counted bins. On a produce cell the number is the UOP count at which the cell asks for its empty — honoured when it is strictly between zero and UOPCapacity, capacity otherwise. The single derivation is domain.NodeClaim.DemandLevel on Edge; nothing here reproduces it. MIRRORED, NOT YET READ. plantclaims.Replace inserts it into style_claims and no query on this side selects it back. It is carried for the fill-priority signal the computation is meant to grow, and until that reader exists the honest statement is that the column is a copy of Edge's value and nothing more. |
| `inbound_source` | string | omitempty | InboundSource is the pool this claim's inbound material is picked up FROM — a node name or a group name; Core resolves either. Blank means not reported. |
| `outbound_destination` | string | omitempty | OutboundDestination is where this claim's outbound material is dropped off TO — a node name or a group name, the same resolution as InboundSource. Blank means not reported. |
| `paired_core_node` | string | omitempty | PairedCoreNode is the claim's partner position, and it carries TWO meanings depending on SwapMode — the partner side at a sequential (A/B) cell, the back press at a press-index cell. Mirrored verbatim rather than disambiguated here: the overload lives on Edge's NodeClaim (see its field comment and docs/terminology.md), and a mirror that renamed it per mode would be a second vocabulary for the same column. Blank means not reported. |
| `second_paired_core_node` | string | omitempty | SecondPairedCoreNode is the optional third (back-most) position of a press-index cell, where the layout is C -> B -> A. Blank means not reported, which on a two-position cell is also the configured truth. |

<!-- /generated -->

#### SourcingStateReport

Published by Core on the `sourcing.state` subject — the outbound half of the
`plant.claims` feed. Core computes each `(process, style)`'s sourceability from
the mirror and pushes the verdict down so HMI screens know what can be changed
over to before an operator presses CHANGE. A **subject on the existing
`shingo.dispatch` topic**, not a new topic.

Core publishes:

- a **change delta** (`snapshot: false`) carrying only the styles whose verdict
  moved — on status / missing-list / at-risk-membership changes, and
- a **full snapshot** (`snapshot: true`) of that edge's slice (its own
  processes and every process with no recorded station) under feed key
  `sourcing`: sent when the edge's heartbeat quotes a different digest
  (`protocol.SourcingDigest`, which leaves out `computed_at` and the time to
  empty), and at once to an edge whose style disappeared. The edge digests its
  own cache with the same function, so a late-joining or restarted edge, or one
  that missed a delta, is sent the snapshot on its next heartbeat. There is no
  periodic snapshot and none on registration.

Edge persists the feed to SQLite, so the changeover picker reads the last-known
verdict with no Core round-trip at click time and it survives an HMI reload or an
Edge reboot during a Core partition. Steady state publishes nothing (time-to-empty
drift alone is not a change).

`status` is the **gated** result: when the owner has not enabled the at-risk
tier, a projected-empty style is published as `"green"` with `at_risk` omitted —
so no screen ever shows an unvalidated yellow. The value schema is
**additive-only**; an older Edge that has not registered the `sourcing.state`
subject logs-and-drops the whole message (mixed-version no-op).

```json
{
  "snapshot": true,
  "states": [
    {
      "process_id": "SNF2",
      "style_id": "SNF2-A",
      "status": "red",
      "missing": ["BIN-B", "BIN-C"],
      "reason": "Cannot change over — missing BIN-B, BIN-C.",
      "computed_at": "2026-07-18T20:00:00Z"
    }
  ]
}
```

<!-- generated: protocol struct SourcingStateReport -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `states` | [`SourcingState`](#sourcingstate)[] |  | States are the per-(process, style) verdicts. |
| `snapshot` | boolean | omitempty | Snapshot true is a full replace of the edge's slice (the edge drops rows not in it); false or absent is a change delta of only the styles whose verdict moved. |

<!-- /generated -->

##### SourcingState

<!-- generated: protocol struct SourcingState -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `process_id` | string |  | ProcessID is the process key, plant.claims' process_id. |
| `style_id` | string |  | StyleID is the style key within the process. |
| `status` | string |  | Status is "green", "yellow", "red", or "not_configured". "not_configured" means the style has no sourceability claims, so there is no verdict — NOT that it is fine. It is never selectable. An older Edge that does not know the value must treat it as not-selectable rather than falling back to a capable state; the picker's default arm does that. |
| `missing` | string[] | omitempty | Missing lists the payloads no available bin could satisfy (RED only). |
| `absent` | string[] | omitempty | Absent lists the Missing payloads Core has no bin of at all: none free, none spoken for, none riding a robot (RED only; a subset of Missing). It is the only "dry" an Edge may refuse a REQUEST on, because it is the only one with nothing to wait for; a payload in Missing and not here is congestion. An older Core never sends it, which reads as nothing absent, the safe side. |
| `at_risk` | [`SourcingAtRisk`](#sourcingatrisk)[] | omitempty | AtRisk lists lines projecting empty within the horizon (yellow tier only). |
| `reason` | string | omitempty | Reason is the operator-facing generated sentence (Core owns the wording; the HMI displays it verbatim and never invents text). |
| `computed_at` | timestamp |  | ComputedAt is when Core computed the verdict. |

<!-- /generated -->

##### SourcingAtRisk

<!-- generated: protocol struct SourcingAtRisk -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `payload_code` | string |  | the payload projecting empty |
| `node` | string | omitempty | the line (node) the projection is for |
| `time_to_empty_seconds` | float |  | projected seconds until the line empties (lower fills first) |

<!-- /generated -->

#### ContainmentSnapshot

Sent on `containment.snapshot`: Core's whole containment state (flags, held bins, what stands at each containment
destination) with its digest. Broadcast after a containment write, unicast when an Edge's heartbeat quotes a
different `containment` digest. An Edge older than the subject logs "no handler" once per broadcast.

<!-- generated: protocol struct ContainmentSnapshot -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `digest` | string | omitempty |  |
| `flags` | [`PayloadContainmentRow`](#payloadcontainmentrow)[] |  |  |
| `held_bins` | [`HeldBinRow`](#heldbinrow)[] |  |  |
| `destinations` | [`ContainmentDestination`](#containmentdestination)[] |  |  |

<!-- /generated -->

##### PayloadContainmentRow

<!-- generated: protocol struct PayloadContainmentRow -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `payload_code` | string |  |  |
| `active` | boolean |  |  |
| `reason` | string |  |  |
| `activated_by` | string |  |  |
| `activated_at` | timestamp |  |  |
| `deactivated_by` | string |  |  |
| `deactivated_at` | timestamp |  |  |

<!-- /generated -->

##### HeldBinRow

<!-- generated: protocol struct HeldBinRow -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `bin_id` | integer |  |  |
| `label` | string |  |  |
| `payload_code` | string |  |  |
| `node_name` | string |  |  |
| `hold_by` | string |  |  |
| `hold_at` | timestamp |  |  |

<!-- /generated -->

##### ContainmentDestination

<!-- generated: protocol struct ContainmentDestination -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node` | string |  |  |
| `children` | string[] |  |  |
| `bins` | [`ContainmentBin`](#containmentbin)[] |  |  |

<!-- /generated -->

##### ContainmentBin

<!-- generated: protocol struct ContainmentBin -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node` | string |  |  |
| `bin_id` | integer |  |  |
| `label` | string |  |  |
| `payload_code` | string |  |  |
| `uop` | integer |  |  |

<!-- /generated -->

#### SupplyRefusalState

Carried by two subjects. On `supply.refusal` (Edge -> Core) a loader operator says they cannot fill a call, or
takes that back. On `supply.refusal_state` (Core -> Edge) Core broadcasts the refusal's state so the cell can
answer; it is station-broadcast and filtered per process on arrival.

<!-- generated: protocol struct SupplyRefusalState -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `action` | string |  | Action is one of SupplyRefusalOpened / Acked / Closed and decides which of the fields below are authoritative in this message. |
| `loader_node` | string |  |  |
| `payload_code` | string |  |  |
| `refused_at` | timestamp | omitempty | Opened fields. |
| `refused_by` | string | omitempty | RefusedBy is STATION-level. The loader board carries no operator identity, so this is the station name and not a person — named honestly rather than dressed up as attribution it cannot make. |
| `ack_at` | timestamp | omitempty | Acked fields. |
| `ack_choice` | string | omitempty |  |
| `ack_process_id` | string | omitempty | AckProcessID is the process NAME ("SNF2") of the cell that ANSWERED, matching the demand grain. Note it is who answered, not who was told — with a broadcast there is no single addressee to record. |

<!-- /generated -->

#### SupplyRefusalSnapshot

Sent on `supply.refusal_snapshot`: Core's whole open refusal set, to one Edge whose `refusals` feed digest differs
from Core's. The Edge merges it per field and re-sends what it owns that Core is missing.

<!-- generated: protocol struct SupplyRefusalSnapshot -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `digest` | string |  |  |
| `open` | [`SupplyRefusalState`](#supplyrefusalstate)[] |  |  |

<!-- /generated -->

#### EdgeAsk

Sent on `edge.ask`: a question that wants an answer — a kind (`AskKinds`, `protocol/ask.go`) and that kind's
request body (`protocol/door_bodies.go`; `loader.retrieve`'s `LoaderRetrieveRequest` is in `protocol/ask.go`).
Core answers the kinds as asks only. It rides the edge outbox and expires after `AskTTL`; Core answers it with
`edge.answer` (a reply carrying the ask's envelope ID as `cor`), and answers one that arrives expired with status
408 and does nothing.

<!-- generated: protocol struct EdgeAsk -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `kind` | string |  |  |
| `body` | JSON |  |  |

<!-- /generated -->

#### EdgeAnswer

Sent on `edge.answer`: Core's answer to one ask, a reply whose `cor` is the ask's envelope ID — the kind, the
door's status and sentence, its response body, and Core's bins rows for the nodes the act touched. It never
expires. The edge applies it by its kind (rows into its bins table, then the kind's own fact) whether or not a tap
is still waiting.

<!-- generated: protocol struct EdgeAnswer -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `kind` | string |  |  |
| `status` | integer |  |  |
| `sentence` | string | omitempty |  |
| `body` | JSON | omitempty |  |
| `rows` | [`NodeBinsRow`](#nodebinsrow)[] | omitempty |  |

<!-- /generated -->

#### BinPickedUp

Sent on `transit.bin_picked_up`: the robot has the bin — the pickup half of a transit, distinct from delivery.

<!-- generated: protocol struct BinPickedUp -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  |  |
| `bin_id` | integer |  |  |
| `location` | string |  |  |
| `picked_up_at` | timestamp |  |  |
| `epoch` | integer | omitempty |  |

<!-- /generated -->

#### BinLanded

Sent on `transit.bin_landed`: a carrier came to rest at a node a station binds at — the drop half of a transit,
and the only door that binds a carrier Core placed.

<!-- generated: protocol struct BinLanded -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `bin_id` | integer |  |  |
| `core_node_name` | string |  |  |
| `count` | integer |  |  |
| `epoch` | integer |  |  |
| `payload_code` | string | omitempty |  |
| `payload_known` | boolean |  |  |
| `order_uuid` | string | omitempty |  |
| `placed_seq` | integer |  |  |

<!-- /generated -->

#### BinReturn

Sent on `transit.bin_return`, notice only: the bin a cancelled order left on a robot is returning, returned, or
held — keyed by the cancelled order's uuid. No status or count changes.

<!-- generated: protocol struct BinReturn -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  |  |
| `bin_label` | string |  |  |
| `payload_code` | string | omitempty |  |
| `state` | string |  |  |
| `destination` | string | omitempty |  |
| `reason` | string | omitempty |  |
| `at` | timestamp |  |  |

<!-- /generated -->

#### NodeStructureChanged

Sent on `node.structure_changed`: a node was reparented or deleted, so the Edge's topology view is stale.

<!-- generated: protocol struct NodeStructureChanged -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node_id` | integer |  |  |
| `node_name` | string |  |  |
| `old_parent_id` | integer | omitempty |  |
| `new_parent_id` | integer | omitempty |  |
| `action` | string |  | "reparented" or "group_deleted" |

<!-- /generated -->

#### UOPAdjustment

Sent on `inventory.uop_adjustment`: an operator's UoP correction on Core, pushed so the Edge's Bins view agrees.

<!-- generated: protocol struct UOPAdjustment -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `bin_id` | integer |  |  |
| `core_node_name` | string |  |  |
| `new_remaining` | integer |  |  |
| `actor` | string |  |  |
| `adjusted_at` | timestamp |  |  |
| `released` | boolean | omitempty | Released, when true, means the bin was MOVED off CoreNodeName in Core (admin Move). Edge clears that node's active_bin_id so its PLC ticks stop attributing consumption to a bin that has left, instead of applying NewRemaining (which is ignored when Released). Reuses this Core→Edge channel rather than a separate subject. Older Edges ignore the field. |
| `bound` | boolean | omitempty | Bound, when true, means the bin was MOVED onto CoreNodeName in Core (admin Move mirroring a physical relocation the robot-delivery path never recorded — manual fork-truck recovery, a failed delivery that left the bin unregistered). Edge binds that node's runtime to the bin: active_bin_id = BinID, active_bin_epoch = Epoch, remaining_uop_cached = NewRemaining — so its PLC ticks resume counting the arrived bin. The dual of Released; Core sets exactly one of the two per message. Core's Move guarantees the destination held no other bin, so Edge binds ahead of its active-bin guard and overwrites any stale pointer. Older Edges ignore the field. |
| `epoch` | integer | omitempty | Epoch carries the bin's delta_epoch so a bind seeds active_bin_epoch correctly and subsequent BinUOPDeltas carry the right generation for Core's epoch-aware dedup. Set on the Bound path (admin Move onto a node) and on a plain count correction (record_count): when the correction lands on a staged-but-unbound bin, Edge binds it with this epoch (P2-C5), so the resumed delta stream is accepted instead of dropped as epoch-0. Zero on the Released path and from older Cores. |
| `as_of_net` | integer | omitempty | AsOfNet and AsOfSeq are Core's applied_net and last_seq for (AsOfStation, BinID, Epoch), read in the transaction that wrote NewRemaining: how much of that station's running net Core had applied when it set the count. The station named, holding BinID at Epoch, rebases: remaining = NewRemaining + (flushed_net - AsOfNet) + unflushed and ignores an adjustment whose AsOfSeq is below the one it last took for the same (bin, epoch). Set only by a count, and only when exactly one station has an anchored cursor for the carrier in this generation; nil otherwise, and then the station writes NewRemaining as is, which is also what an Edge that predates the fields does. |
| `as_of_seq` | integer | omitempty | See `as_of_net`. |
| `as_of_station` | string | omitempty | AsOfStation is the station whose stream AsOfNet measures. Any other station modelling the node takes NewRemaining as is. |
| `payload_code` | string | omitempty | PayloadCode and PayloadKnown name what the carrier holds after a reset a person declared (an admin Load Payload or Clear, a non-lifecycle Actor): the part, or a known-empty carrier for a clear. The station records it as the carrier standing at the node, so the counts it sends carry the part the carrier now holds. PayloadKnown is false on every other adjustment: a count does not assert identity, and a lifecycle reset's carrier is named by its own landing or departure. Older Edges ignore the fields. |
| `payload_known` | boolean | omitempty | See `payload_code`. |

<!-- /generated -->

#### BinEpochRefresh

Sent on `inventory.bin_epoch_refresh`: re-anchor a bin's counting epoch without a full resync.

<!-- generated: protocol struct BinEpochRefresh -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `bin_id` | integer |  |  |
| `core_node_name` | string |  |  |
| `epoch` | integer |  |  |

<!-- /generated -->

#### OrderStatusRequest

Sent on `order.status_request` for startup reconciliation: the Edge names the orders it wants authoritative status
for.

<!-- generated: protocol struct OrderStatusRequest -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuids` | string[] |  |  |

<!-- /generated -->

#### OrderStatusResponse

Sent on `order.status_response`: the answer, plus `unlisted` — orders for that station the Edge did **not** name.

<!-- generated: protocol struct OrderStatusResponse -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `orders` | [`OrderStatusSnapshot`](#orderstatussnapshot)[] |  |  |
| `unlisted` | [`OrderProjection`](#orderprojection)[] | omitempty | Unlisted carries orders for the asking station that the Edge did NOT name — Core-authored ones it has no row for. This is the healing half of the reconcile, and it is why the reconcile is load-bearing rather than a backstop: the Core → Edge outbox drops a message permanently once it is past its retry limit, so a lost projection is a normal event and the only thing that repairs it is the Edge finding out here. Additive: an older Edge decodes the snapshots and ignores this field, which leaves it exactly where it is today. |

<!-- /generated -->

##### OrderStatusSnapshot

<!-- generated: protocol struct OrderStatusSnapshot -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  |  |
| `found` | boolean |  |  |
| `status` | string | omitempty |  |
| `station_id` | string | omitempty |  |
| `source_node` | string | omitempty |  |
| `delivery_node` | string | omitempty |  |
| `vendor_order_id` | string | omitempty |  |
| `error_detail` | string | omitempty |  |
| `queue_reason` | string | omitempty | QueueReason is Core's current blocking signal for a queued order (orders.queue_reason) — e.g. "no bin of requested payload in node group AMR Supermarket". Re-evaluated by the dispatcher/scanner and cleared on dispatch, so Edge refreshes it on each status resync. |
| `queue_code` | string | omitempty | QueueCode is the structured category behind QueueReason (protocol.QueueCode). Carried on the snapshot (additive) so an Edge resync doesn't lose the code; old Edge ignores it. |
| `fault_since` | timestamp | omitempty | Fault fields mirror OrderUpdate's, so a boot reconcile restores a faulted order's clock instead of leaving the board with a badge and no sentence until the next push — which, for an order that is already stuck, may never come. Same semantics: set only while faulted, cleared by the Edge from the status. See OrderUpdate for why they are not QueueReason. |
| `fault_deadline` | timestamp | omitempty |  |
| `fault_notice` | boolean | omitempty |  |
| `fault_notice_after_s` | integer | omitempty |  |
| `fault_ref` | [`TermRef`](#termref) | omitempty |  |

<!-- /generated -->

#### OrderProjection

Sent on `order.projected`: a whole order row, so an order the Edge did not create still appears on its board.

<!-- generated: protocol struct OrderProjection -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  |  |
| `order_type` | string (`OrderType`) |  |  |
| `status` | string |  |  |
| `station_id` | string |  |  |
| `quantity` | integer | omitempty |  |
| `source_node` | string | omitempty |  |
| `delivery_node` | string | omitempty |  |
| `payload_code` | string | omitempty |  |
| `payload_desc` | string | omitempty |  |
| `retrieve_empty` | boolean | omitempty | RetrieveEmpty distinguishes a pull of an empty carrier from a pull of a full one. Carried explicitly rather than derived from OrderType, because the Edge column is separate and the two have drifted before. |
| `origin_id` | string | omitempty | OriginID / OriginClass are the demand-episode attribution, passed through so a projected row answers "why does this exist" the same way a locally created one does. |
| `origin_class` | string | omitempty | See `origin_id`. |
| `queue_reason` | string | omitempty | QueueReason / QueueCode mirror OrderStatusSnapshot's, so a projection that arrives for a queued order explains the wait without a second round trip. |
| `queue_code` | string | omitempty | See `queue_reason`. |

<!-- /generated -->

#### DemandOriginState

Sent on `demand.origin`: the demand episodes the Edge owns, one whole episode row per message.

<!-- generated: protocol struct DemandOriginState -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `origin_id` | string |  |  |
| `revision` | integer |  | Revision is monotonic per origin, stamped by Edge on every change. It is the ONLY thing that decides whether an arriving message is newer than what Core holds — not a timestamp, which two services cannot agree on. |
| `episode_key` | string |  | EpisodeKey is the computed identity. Core keys its partial unique index on it, so both services build it with the constructors in episode_key.go rather than formatting their own. |
| `kind` | string |  |  |
| `direction` | string (`ClaimRole`) | omitempty | Direction carries the cell's ROLE — produce or consume. Typed, and the two values are the claim's own; "supply"/"evacuate" were a second vocabulary for the same fact and are retired (see protocol/episode_key.go). The JSON key stays `direction` so the wire shape is unchanged; only the value domain moved, which migration 87 carries for stored rows. |
| `trigger` | string | omitempty |  |
| `trigger_ref` | string | omitempty | TriggerRef is the claim key or ProcessChangeoverID behind the mint — forensic, not identity. |
| `process_id` | string | omitempty | ProcessID is THE GRAIN for a cell episode: choreography spans nodes, the process does not. THE EDGE PROCESS NAME ("SNF2"), NOT ITS SQLITE ROW ID. It was an int64 row id, which made Core hold two unjoinable identity systems for one set of processes — this field against process_styles.process_id and PlantClaimsReport.ProcessID, both of which are the name. See CellEpisodeKey in episode_key.go for the full argument and for the one exposure the change adds. |
| `core_node_name` | string | omitempty | CoreNodeName is the head node. Forensic, not the key. |
| `payload_code` | string | omitempty |  |
| `opened_at` | timestamp |  |  |
| `opened_total` | integer |  | OpenedTotal is the reading at the falling edge — the number the decision was made from, whatever its quality. |
| `threshold` | integer |  |  |
| `expected_orders` | integer | omitempty | ExpectedOrders is the system's own stated intent, STAMPED ONCE at the falling edge and never recomputed or accumulated. NULLABLE, and that is deliberate. The threshold kind's formula divides by the payload catalog's UOPCapacity, and the sizing entry point (dispatch.BinsToReachThreshold, on Core) explicitly refuses `perBinCapacity &lt;= 0` — which means somebody has hit it. Neither 0 nor 1 is honest there: both render as a real ratio and invite a conclusion from a denominator that does not exist. A demand whose denominator is UNKNOWABLE is a different state from one whose denominator is 1, and the surface shows it as "—". For a cell episode it is len(plan orders) — what BuildConsumePlan said it would create — NOT the literal 1 that RequestNodeMaterial takes as a BIN count. Accumulating it per re-fire is the failure mode most likely to look correct in review: it would render 2026-07-21 as ratio 1.0. |
| `expected_unknown_reason` | string | omitempty | ExpectedUnknownReason says WHY the denominator is unknowable, when it is. A NULL with no reason is indistinguishable from a bug. |
| `rerequest_count` | integer | omitempty | RerequestCount is operator pushes that JOINED this episode. |
| `discretionary` | boolean | omitempty | Discretionary marks an operator request with no open episode on a node the system reads as fine. Either the ledger is wrong, or the reorder point is, or the operator knows something the count does not. FLAG, DO NOT CONCLUDE. |
| `closed_at` | timestamp | omitempty | ClosedAt is nil while the episode is open. Its presence IS the close — there is no separate close message to lose. |
| `close_reason` | string | omitempty | CloseReason is one of the CloseReason* constants. Empty while open. |
| `closed_by` | string | omitempty | ClosedBy is "notification" or "sweep" — WHICH MECHANISM ended the episode. Empty means the sender did not say, which is a different fact from "a notification path closed it": an older Edge sends nothing here. It exists because the reconciling sweep uses the same close_reason codes as the notification paths deliberately, so without this a total silent failure of every notification path is indistinguishable from health. |

<!-- /generated -->

#### BinUOPDelta

Sent on `inventory.bin_uop_delta`: a bin's UoP moved.

<!-- generated: protocol struct BinUOPDelta -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station` | string |  |  |
| `bin_id` | integer |  |  |
| `payload_code` | string |  |  |
| `delta` | integer |  |  |
| `reason` | string (`BinUOPDeltaReason`) |  |  |
| `sequence_id` | integer |  |  |
| `epoch` | integer |  | Epoch is the bin's delta_epoch as known to Edge at emit time. Bumps on every Core-controlled lifecycle boundary (set_for_production, clear). The dedup PK on Core is (station, scope_kind, scope_key, epoch); extending the key with epoch means a stale Edge seq counter (deploy reset, backup restore, cache loss) can't poison the new bin life's delta stream. Pre-epoch envelopes deserialize with Epoch=0, which Core treats as the pre-migration cohort (matches the backfilled epoch=1 on existing rows after the migration runs). |
| `window_start` | timestamp |  |  |
| `window_end` | timestamp |  |  |
| `net` | integer | omitempty | Net is the running total of every Delta this Edge has flushed for the scope (station, bin, epoch), this one included. Core applies Net minus what it has already applied for the scope, so a lost, late, duplicated or reordered message is healed by the next one. Nil from an Edge built before the field existed; Core then applies Delta exactly as it used to. |

<!-- /generated -->

#### LinesideBucketLevel

Sent on `inventory.lineside_bucket_level`: a lineside pile row's level after a change. The Edge is its only
writer; Core mirrors it.

<!-- generated: protocol struct LinesideBucketLevel -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `core_node_name` | string |  |  |
| `payload_code` | string |  |  |
| `state` | string (`LinesideBucketState`) |  |  |
| `qty` | integer |  |  |
| `drained` | integer |  |  |
| `sequence_id` | integer |  |  |
| `window_end` | timestamp |  |  |

<!-- /generated -->

#### ProductionTicks

Sent on `production.ticks`: the batched per-station tick feed from the `counter_snapshots` shipper.

<!-- generated: protocol struct ProductionTicks -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `ticks` | [`ProductionTickEvent`](#productiontickevent)[] |  |  |

<!-- /generated -->

##### ProductionTickEvent

<!-- generated: protocol struct ProductionTickEvent -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `edge_snapshot_id` | integer |  |  |
| `process_id` | integer |  |  |
| `style_id` | integer |  |  |
| `count_value` | integer |  |  |
| `delta` | integer |  |  |
| `anomaly` | string | omitempty |  |
| `recorded_at` | timestamp |  |  |

<!-- /generated -->

#### LinesideLevelReport

Sent on `inventory.lineside_level_report`: the Edge's own view of its lineside levels, for Core's divergence
comparison.

<!-- generated: protocol struct LinesideLevelReport -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `station` | string |  |  |
| `reported_at` | timestamp |  |  |
| `entries` | [`LinesideLevelEntry`](#linesidelevelentry)[] |  |  |

<!-- /generated -->

##### LinesideLevelEntry

<!-- generated: protocol struct LinesideLevelEntry -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `core_node_name` | string |  |  |
| `payload_code` | string |  |  |
| `bin_count` | integer |  |  |
| `bin_uop` | integer |  |  |
| `bucket_qty` | integer |  |  |
| `bin_id` | integer | omitempty |  |
| `bin_epoch` | integer | omitempty |  |
| `flushed_seq` | integer | omitempty |  |

<!-- /generated -->

#### HoldDropped

Sent on `inventory.hold_dropped`: a produce seat's hold was dropped without replaying onto a bin. Core writes one
`hold_dropped` row (no bin) and lists it on /inventory.

<!-- generated: protocol struct HoldDropped -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node` | string |  |  |
| `payload_code` | string |  |  |
| `qty` | integer |  |  |
| `reason` | string |  |  |
| `called_by` | string | omitempty |  |
| `occurred_at` | timestamp |  |  |
| `order_id` | string | omitempty |  |

<!-- /generated -->

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
  "priority":         0,
  "retrieve_empty":   false
}
```

<!-- generated: protocol struct OrderRequest -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the order's id, a UUID v4 the edge generates; every later message about the order carries it. |
| `order_type` | string (`OrderType`) |  | OrderType is one of retrieve, retrieve_empty, move or complex. Intake refuses a type no planner serves (TermUnknownType). |
| `payload_code` | string | omitempty | PayloadCode is the registered payload Core sources material by. |
| `payload_desc` | string | omitempty | PayloadDesc is a human-readable description of the payload. |
| `quantity` | integer |  | Quantity is the number of items or units asked for. |
| `delivery_node` | string | omitempty | DeliveryNode is where to deliver; required for a retrieve and a move. |
| `source_node` | string | omitempty | SourceNode is where to pick up. Required for a move. Optional for a retrieve or retrieve_empty, where it names a supermarket group to pull from; blank is Core's global FIFO search. |
| `priority` | integer | omitempty | Priority: higher is more urgent; 0 by default. |
| `retrieve_empty` | boolean | omitempty | RetrieveEmpty asks for an empty carrier rather than a full one. An older Edge's retrieve with this set is promoted to retrieve_empty at intake. |
| `want` | string (`Height`) | omitzero | Want is the height the empty is wanted at, stated by the asker: none (the zero value, omitted) lets Core derive it where the carrier is set down, as it always has; a stated one is taken exactly — that height or wait, never a partial to finish and never the plant-wide scan. SEND BACK's pull states TopsAndBottomsOnly. Sent on an empty retrieve alone. Additive: an older Core ignores it and takes any empty (a stack to an outside that wanted tops & bottoms), so Core goes first; an older Edge never sends it. |
| `remaining_uop` | integer | omitempty | RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption. |
| `skip_auto_confirm` | boolean | omitempty | SkipAutoConfirm prevents Core's reconciliation sweep from auto-confirming this order when it is stuck at "delivered". Edge sets this for side-cycle orders (L1 loader empty-in, U1 unloader full-in) where a human operator must explicitly confirm after performing a physical action (loading or unloading the bin). Without this, Core auto-confirms the moment the bin arrives, bypassing the operator and immediately triggering the outbound leg (L2/U2) while the bin is still empty/full. |
| `origin_id` | string | omitempty | OriginID / OriginClass attribute this order to the demand episode that asked for it, Edge -> Core. Additive omitempty; SiblingOrderUUID on ComplexOrderRequest is the exact template. An order arriving with no OriginID from an Edge that predates this lands as an ORPHAN, not an error — see OriginClass. |
| `origin_class` | string | omitempty | See `origin_id`. |
| `bin_id` | integer | omitempty | BinID names the one bin a move is for: Core's bin id, as the Edge read it on the move's source node. Core lifts that bin and no other, and only while it still stands on the source node: a bin not on the source when the move arrives is refused at intake (order.error, "not at that source"), and once it has left a born move ends skipped instead of carrying whatever lands there next. 0 means no bin is named, and Core takes whatever stands on the source, as it always has. Sent only on a move whose decision read the bin: the return of a keep-staged spare, SEND BACK's move, the produce clear's, a loader's or an unloader's outbound and the containment moves (shingo-edge orders namedBin). Additive and omitempty both ways: an older Core ignores it and lifts whatever stands there, which is what it did before; an older Edge never sends it. |
| `quality_hold_by` | string | omitempty | QualityHoldBy asks Core to put the named bin (BinID) on quality hold, attributed to this: a station sending the bin to containment. The check that the bin stands on the source, the hold marker and the move's birth are one transaction, so the move is never born without its hold, and no hold is set on a bin the move does not find. Ignored without BinID, and on any type but a move. Additive and omitempty: an older Core ignores it and sets no marker (the move still takes the bin to containment); an older Edge never sends it and keeps its own hold call. |

<!-- /generated -->

**Order types** (`OrderType`):

<!-- generated: protocol/types.go OrderType -->

| Order type | Constant | Meaning |
|---|---|---|
| `retrieve` | `OrderTypeRetrieve` | pull a loaded bin matching a payload to a destination |
| `retrieve_empty` | `OrderTypeRetrieveEmpty` | pull an empty bin compatible with a payload to a destination |
| `store` | `OrderTypeStore` | never a kind of order (the bin resolver's put mode); nothing mints it, kept unread |
| `move` | `OrderTypeMove` | generic move; no manifest semantics |
| `complex` | `OrderTypeComplex` | multi-step order composed of sub-steps |
| `ingest` | `OrderTypeIngest` | edge-legacy: no longer minted (manifest-only ingest write); kept for historical order rows |
| `reshuffle_restore` | `OrderTypeReshuffleRestore` | OrderTypeReshuffleRestore is the RETIRED restore-blockers housekeeping type. The subsystem that minted it is deleted (blockers lie now) and no order is ever created with it again — the constant is kept ONLY so historical rows (and the one-shot retirement sweep) display and match a name rather than a raw string. |

<!-- /generated -->

`store`, `ingest` and `reshuffle_restore` name values nothing mints; intake refuses an order type no planner serves
(`unknown_type`).

#### OrderCancel

Requests cancellation of an in-progress order.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "reason":     "Operator cancelled: wrong material"
}
```

<!-- generated: protocol struct OrderCancel -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID names the order to cancel. |
| `reason` | string |  | Reason is the human-readable cancellation reason, or CancelReasonAcceptHalfSwap. |

<!-- /generated -->

#### OrderReceipt

Sent by the edge operator to confirm physical receipt of a delivery.

```json
{
  "order_uuid":   "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "receipt_type": "confirmed",
  "final_count":  48
}
```

<!-- generated: protocol struct OrderReceipt -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID names the delivered order. |
| `receipt_type` | string |  | ReceiptType is always "confirmed" today. |
| `final_count` | integer |  | FinalCount is the quantity actually received, which may differ from the quantity ordered. |

<!-- /generated -->

#### OrderRedirect

Changes the delivery destination of an in-flight order. Triggers re-dispatch in core.

```json
{
  "order_uuid":        "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "new_delivery_node": "line-2-station-b"
}
```

<!-- generated: protocol struct OrderRedirect -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID names the order to redirect. |
| `new_delivery_node` | string |  | NewDeliveryNode is the new destination node's name. |

<!-- /generated -->

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

<!-- generated: protocol struct OrderAck -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID echoes the edge's order UUID. |
| `shingo_order_id` | integer |  | ShingoOrderID is Core's own id for the order. |
| `source_node` | string | omitempty | SourceNode is the node Core chose as the source, when it has one. |

<!-- /generated -->

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

<!-- generated: protocol struct OrderWaybill -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the edge's order UUID. |
| `waybill_id` | string |  | WaybillID is the fleet vendor's transport order id. |
| `robot_id` | string | omitempty | RobotID is the assigned robot. |
| `eta` | string | omitempty | ETA is the estimated time of arrival: RFC 3339, or free text. |

<!-- /generated -->

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

<!-- generated: protocol struct OrderUpdate -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the edge's order UUID. |
| `status` | string |  | Status is the order's status at Core (a Status value). |
| `detail` | string | omitempty | Detail is a human-readable status detail. |
| `eta` | string | omitempty | ETA is the updated estimated time of arrival, when there is one. |
| `queue_reason` | string | omitempty | QueueReason carries the blocking signal from Core's orders.queue_reason when Status is "queued" — e.g. "no bin of requested payload in node group AMR Supermarket". Omitted (not cleared) for non-queued updates; omitempty means an absent field signals "leave unchanged", not "clear". Core sends it when an order's wait changes while it waits (dispatch.WriteQueueDetail). |
| `queue_code` | string | omitempty | QueueCode is the structured category behind QueueReason (protocol.QueueCode). Additive: old Edge ignores it; new Edge with old Core sees "" and falls back to the sentence. Ships alongside QueueReason so Edge can persist the code for future branching without a schema change. |
| `fault_since` | timestamp | omitempty | FaultSince is when the order faulted — the faulted history row's created_at, not orders.updated_at, which the vendor poll rewrites. The Edge ticks its clock from this. |
| `fault_deadline` | timestamp | omitempty | FaultDeadline is when Core gives up (FaultSince + config fault_grace). A live config change mid-grace drifts it, which is acceptable for a countdown and beats the poller's copy, which does not survive a restart. |
| `fault_notice` | boolean | omitempty | FaultNotice is Core's decision AT PUSH TIME about whether this is a replan or a fault worth the word. Always false on the push that creates the fault, because nothing has been faulted for a minute at the instant it faults — which is exactly why FaultNoticeAfterS rides along. |
| `fault_notice_after_s` | integer | omitempty | FaultNoticeAfterS is Core's threshold in whole seconds (config rds.fault_notice_after). It is echoed rather than hard-coded so the Edge can flip its own sentence from "Replanning" to "Fault" as the clock passes it, without owning the rule or waiting for another push. Same reasoning as DisplayConstants() on Core's own pages: the number that decides what an operator is told lives in one config file, and every surface is told what it is. Whole seconds because it crosses to a browser that does millisecond arithmetic on it, and a duration string would need a parser at the far end for a number nobody sets below a second. |
| `fault_ref` | [`TermRef`](#termref) | omitempty | FaultRef is the fleet's reason, the same reference stored on the faulted history row. Nil when the fleet gave none, which is the common case. THE REFERENCE CROSSES, NOT THE SENTENCE, and that is what makes one formatter possible. Detail carries the sentence as it stood at push time, and at push time an order has just faulted, so it always reads "Replanning" — the vendor reason is deliberately withheld under the threshold. An Edge given only Detail could therefore never show the reason, because no second push arrives to reveal it. Given the ref, the Edge calls FormatFaultSentence itself, with the same code Core calls, and crosses the threshold on its own clock. |

<!-- /generated -->

##### TermRef

<!-- generated: protocol struct TermRef -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `node` | string | omitempty | Node is the process / delivery node the reason concerns, dot-named. |
| `payload` | string | omitempty | Payload is the part number that could not be sourced, delivered or found. |
| `peer` | integer | omitempty | Peer is the sibling order whose fate caused this one — swap legs unwound together, reshuffle children abandoned with their parent. |
| `vendor_code` | integer | omitempty | VendorCode is the fleet's own numeric reason, as RDS reported it on the ORDER (OrderDetail.Errors[].Code) — not a robot's standing alarm, which is a different stream and is background rather than cause. Zero means the fleet gave no reason, which is the common case: ~94% of faulted orders carry no errors[] entry at all, so this field is absent far more often than it is present and its absence is information, not a gap. A reference, not a category. It is deliberately NOT a typed code vocabulary: one vendor code over 22 events is not a population to classify, and order_history.code stays empty on faulted rows. Classify at read time on ref->>'vendor_code' if a plant ever produces the variety. |
| `vendor_desc` | string | omitempty | VendorDesc is the fleet's text for VendorCode, stored as received. No translation table until there is something to translate — the live values are "cannot replan" and "Robot Suspended", which need none. |
| `detail` | string | omitempty | Detail is free text for the cases the fields above do not cover. Deliberately last and deliberately optional: anything that ends up here repeatedly is a missing field, not a place to put prose. |

<!-- /generated -->

#### OrderDelivered

Fleet reports that the robot has completed delivery at the destination node. The edge should prompt the operator for receipt confirmation (or auto-confirm if configured).

```json
{
  "order_uuid":        "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "delivered_at":      "2026-02-18T10:13:45Z",
  "staged_expire_at":  "2026-02-18T10:23:45Z"
}
```

<!-- generated: protocol struct OrderDelivered -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the edge's order UUID. |
| `delivered_at` | timestamp |  | DeliveredAt is when the fleet reported the physical delivery. |
| `staged_expire_at` | timestamp | omitempty | StagedExpireAt, when present, puts the order in staged (dwelling) until this time or until an order.release arrives. Absent on a delivery that does not stage. |
| `bin_id` | integer | omitempty | BinID carries the bin's Core-side ID so Edge can attribute PLC tick deltas to the right bin in the Phase 1 bin-as-truth flow. Nil for multi-bin orders; Phase 1 emits bucket deltas only in that case and bin attribution waits for the multi-bin handling refinement. Older Core/Edge builds tolerate either side being nil. |
| `uop_remaining` | integer | omitempty | UOPRemaining and DeltaEpoch are the bin's authoritative count and load-lifecycle epoch as of delivery, snapshotted by Core right after the bin lands at the destination (handleOrderDelivered, after applyBinArrivalForOrder). Edge seeds its runtime cache and stamps outgoing BinUOPDeltas from these — so the seed and the epoch ride the same Kafka message as the delivery itself, with no separate HTTP pull. UOPRemaining is a pointer so "no count was sent" (nil) is distinguishable from a genuinely-empty bin (0). NIL IS NOT ONLY AN OLD CORE, and reading it that way is how the Edge's own fallback got its wrong direction. Core sends the count when it named a bin AND could read that bin's row; a read that fails leaves this nil on a current build. A current Edge binds nothing from this message (the landing binds); for an older Core it binds through the landing verb with this count and never assumes a full carrier (engine.landFromOldCore). DeltaEpoch 0 is the pre-migration / unknown sentinel. Both are only meaningful for single-bin orders (BinID != nil). |
| `delta_epoch` | integer | omitempty | See `uop_remaining`. |
| `bin_payload_code` | string | omitempty | BinPayloadCode is the payload of the bin that just landed, read from the same row and at the same moment as UOPRemaining and DeltaEpoch above. It answers "what is this carrier", which the Edge cannot answer for itself: that side stores active_bin_id, an opaque Core id, and has no bins table. WITHOUT IT THE EDGE GUESSES, and the guess is the claim of whichever style the process is on — what the cell was ASKED FOR, not what is standing on it. Those agree until a changeover moves the cell on while a carrier is still there, which is Springfield SMN_029, 2026-09-02. This does not move the boundary. The Edge still decides where material goes; it stops inferring a fact Core was already holding. Pointer so "not established" (nil) is distinguishable from a carrier known to be empty (""), the same distinction UOPRemaining draws and for the same reason — and the Edge keeps that pair apart on its own side too, as RuntimeState.LinesidePayloadKnown. Only meaningful when BinID != nil. |
| `delivery_node` | string | omitempty | DeliveryNode is the Core dot-name of the destination. Populated for all orders so the Edge can bind the runtime cache even when the order was created on Core directly (no Edge order row). Broadcast to all edges when the order has no station (stationID==""); omitempty so older Edge builds ignore the field silently. |
| `bin_dest_node` | string | omitempty | BinDestNode is the Core dot-name of the node the carried bin came to rest at — set ONLY for multi-tote deliveries (F1b), where BinID names the one order_bin destined for the consuming process node. It is the per-bin landing node, which for a swap is authoritative and unambiguous, unlike DeliveryNode (the last dropoff — the supermarket for the evac leg) or a steps-derived finalDropoff. Edge binds when BinDestNode == its process node's CoreNodeName. Empty for single-bin orders, whose existing delivery-gate (steps finalDropoff / DeliveryNode) is unchanged. |
| `landing_sent` | boolean | omitempty | LandingSent says the Core that sent this also announces its carriers' drops on SubjectBinLanded. A new Edge binds nothing from a delivered that carries it. Without it the sender is an older Core that never sends a landing, and a new Edge routes the carrier fields above through its landing verb with no placement stamp: into an empty slot or onto the same bin only, never over a different bound carrier. Nothing else forbids a new Edge talking to an old Core (DESIGN-H S6), hence the flag. The flag, that adapter and the carrier fields above go on the cleanup sweep row the release after. |

<!-- /generated -->

#### OrderError

Order processing has failed. The order is in a terminal error state on the core side.

```json
{
  "order_uuid":  "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "error_code":  "no_source",
  "detail":      "No source payload found for type BIN-A"
}
```

<!-- generated: protocol struct OrderError -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the edge's order UUID. |
| `error_code` | string |  | ErrorCode is the machine-readable error category: a TermCode value when the order reached a terminal status, or the refusing door's own code (invalid_state, not_found, redirect_failed, ...) when Core refused the message. |
| `detail` | string |  | Detail is the human-readable error description. |
| `rows` | [`NodeBinsRow`](#nodebinsrow)[] | omitempty | Rows is Core's current bins rows for the nodes the refusal is about (a move's source, when its named bin is not there), for the Edge to write into its bins copy before it shows Detail. Absent on every other error; an older Edge ignores it. |

<!-- /generated -->

#### OrderCancelled

Confirms that an order has been successfully cancelled on the core side.

```json
{
  "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "reason":     "Operator cancelled: wrong material"
}
```

<!-- generated: protocol struct OrderCancelled -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the cancelled order's UUID. |
| `reason` | string |  | Reason is the cancellation reason, echoed from the cancel. |

<!-- /generated -->

#### OrderSkipped

Sent when the work turned out not to be needed: the order reached the terminal `skipped` status. Its wire shape mirrors `OrderError`; see [order-lifecycle.md](order-lifecycle.md).

<!-- generated: protocol struct OrderSkipped -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the edge's order UUID. |
| `error_code` | string |  | ErrorCode is the terminal code (a TermCode value) the skip ended with. |
| `detail` | string |  | Detail is the human-readable reason. |

<!-- /generated -->

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

<!-- generated: protocol struct ComplexOrderRequest -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the order's id, a UUID v4 the edge generates. |
| `payload_code` | string | omitempty | PayloadCode is the payload Core sources material by. |
| `payload_desc` | string | omitempty | PayloadDesc is a human-readable description of the payload. |
| `quantity` | integer |  | Quantity is the number of items or units asked for. |
| `priority` | integer | omitempty | Priority: higher is more urgent; 0 by default. |
| `process_node` | string | omitempty |  |
| `steps` | [`ComplexOrderStep`](#complexorderstep)[] |  | Steps are the order's steps, executed in sequence. |
| `sibling_order_uuid` | string | omitempty | SiblingOrderUUID is the edge UUID of the paired leg in a coordinated pair. BOTH legs carry it: every Edge door that makes a pair mints both uuids before it creates either, so each leg names its partner and neither goes out unpaired. Empty for non-swap orders — and for a RELAY, whose second leg collects what the first delivers (the Edge engine's stageSteps): its legs are linked on the Edge only, because a pair at Core would hold the feeder for a collector that cannot source until the feeder has gone. It did once ride only the second-created leg, because a leg could not name a sibling that did not exist yet. That made CREATION ORDER a correctness input: the first-created leg named nobody, so from its own side it was a solo order and could go to the fleet before its partner existed. Core now forms a pair from either leg's pointer alone (coordinatedPairLegs), and the intake back-link stays so a Core talking to an older Edge, which still sends the one-way shape, heals it on read. Which leg is created FIRST is NOT a role, so do not read one into it: it varies by mode and by the creating path. two_robot creates the supply leg first; a press-index CHANGEOVER does the same, but a steady-state press-index swap creates the evac leg (R1, which clears the press) first. The pointer says "these two legs are a pair", nothing more; a leg's role comes from its steps (see legTakesLineBin in Core's dispatch package). Core links both order rows on ingest — bidirectionally, via LinkSiblingsByEdgeUUID — so either leg can find the other: the pair rule admits both legs in one pass (coordinatedPairLegs), and the death rule resolves the survivor when one dies (HandleSwapPeerTerminal). |
| `key_route` | string[] | omitempty | KeyRoute / KeyTask are SEER robot-SELECTION hints carried from the claim's Routing configuration through to fleet.CreateOrderRequest. See that type for the vendor semantics. Both empty on every order until a claim configures them, which is the pre-existing behaviour exactly. Additive and omitempty: an older Core ignores them, and an older Edge simply never sends them. |
| `key_task` | string | omitempty | See `key_route`. |
| `remaining_uop` | integer | omitempty | RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption. |
| `origin_id` | string | omitempty | OriginID / OriginClass attribute this order to the demand episode that asked for it, Edge -> Core, same shape as OrderRequest's. BOTH legs of a swap pair carry the SAME OriginID: one fire of applyConsumePlan is one demand served by two order rows, and counting it as two demands would read every swap-mode episode 2x high. |
| `origin_class` | string | omitempty | See `origin_id`. |

<!-- /generated -->

##### ComplexOrderStep

<!-- generated: protocol struct ComplexOrderStep -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `action` | string |  | "pickup", "dropoff", "wait" |
| `node` | string | omitempty | node or group name (Core auto-resolves groups) |
| `empty` | boolean | omitempty | Empty marks a pickup leg that must fetch an EMPTY carrier rather than a payload-matching full — the produce-node "bring an empty to fill" leg (the store dual of a consume node's full retrieve). When true on a pickup, Core resolves an NGRP source to a slot holding an empty bin and claims an empty carrier, instead of the default full-retrieve semantics. Pickup-only; ignored on dropoff/wait. Backward-compatible: absent/false preserves the prior always-full behavior, so an older Core that ignores the field behaves exactly as today. |
| `payload_code` | string | omitempty | PayloadCode is the payload THIS STEP's bin selection resolves against, overriding the order's payload for this leg only. Empty means "use the order's", which is every leg of every order that does not need to say otherwise. ── WHY A LEG NEEDS ITS OWN ANSWER ──────────────────────────────────── A changeover swap is ONE order doing two jobs: it lifts the outgoing style's bin off the line and brings the incoming style's carrier back. The order carries the FROM-style payload, because its opening pickup has to find the bin that is actually there. Marking the refill leg Empty drops the full-bin content match — but NOT bin-type compatibility, which resolves against the order's payload through PayloadBinTypeAdvisoryClause. So a press changing from a payload on one carrier type to a payload on another was handed a carrier of the type it was LEAVING: wrong carrier one direction, and an unsatisfiable wait for a type the plant had none of in the other, which parked two supply legs until an operator abandoned them (sim 2026-08-24, N1-c). Setting it here is the smallest true statement: this leg is for that payload. The alternative was splitting the order in two, which doubles the robot trips for the common same-node case the single trip was chosen for. ── MIXED VERSIONS: CORE FIRST ──────────────────────────────────────── Additive and omitempty, like every other field added to this wire — but unlike the Core→Edge additions, an old receiver ignoring THIS one is not cosmetic: it silently reverts to the wrong-carrier behaviour above. Core and Edge deploy separately, so DEPLOY CORE FIRST. Pinned by TestComplexOrderStep_PayloadCode_MixedVersion, which asserts what an old Core does rather than leaving it to be discovered on a floor. |
| `bin_id` | integer | omitempty | BinID names the one bin a pickup takes: the operator's pick of a held partial to finish, from the list a finish-later claim's REQUEST shows. Core lifts that bin where it stands, or the leg waits. 0 for every other pickup, which Core sources as before. |
| `wait_kind` | string | omitempty | WaitKind declares WHO MAY ADVANCE a wait step, carried across the wire so the far side does not have to guess. ── WHY IT IS ON THE WIRE AT ALL ────────────────────────────────────── Core has always known: a wait it splices for a lane is stamped, and its release fence keys on that stamp. The Edge never received it. So a station holding a plan could not tell a wait it owns — "hold at staging until the line clears", which an operator ends — from one only Core's lane evaluator can advance, and the board either offered a button that could not work or offered nothing and explained nothing. The sim operator guessed with a three-strike retry cap and guessed wrong; a human at an HMI has strictly less to go on. Values are WaitKindStation ("station", station-owned) and, on a plan Core authors, WaitKindLane ("lane") and WaitKindVacate ("vacate"), both Core-owned. They are declared once, in waitkind.go. EMPTY IS NOT A FOURTH KIND. It means "authored before this field", and it is read as station-owned for exactly as long as pre-ruling orders are draining — the historical default, so nothing needs migrating. After the drain window an untagged wait is a defect, and the drift tests on both sides say so. |
| `purpose` | string | omitempty | Purpose names the station decision a station wait belongs to: which RELEASE lets the robot past it (the Edge's release.Purpose: swap, ready, tooling_done). The Edge authors it with the wait and reads it back from its own steps; Core carries steps as it receives them and decides nothing on it. Absent on a lane wait and on any step that is not a wait. |
| `exclusive_slot` | boolean | omitempty | ExclusiveSlot declares that this DROPOFF lands on a node that holds ONE bin at a time and must therefore be reserved before the robot is sent. ── WHY THE SENDER HAS TO SAY IT ────────────────────────────────────── Core gates its destination checks on node ROLE: a dropoff is reserved and capacity-checked when the node is a storage slot (a child of a LANE or NGRP), and skipped otherwise. The skip is deliberate and load-bearing — a two-robot SUPPLY leg delivers to a LINE node that a sibling EVAC clears, and gating that re-creates the deadlock 2b05dce fixed. A STAGING node is neither. It holds one bin like a slot, but it is seeded as a station with no parent, so Core's role test rejects it at the parent-nil guard and BOTH destination gates decline to act. Nothing reserves it and nothing checks it is free. Core cannot repair that by looking harder. Every station — line, press, weld, loader, unloader, staging, dest — carries the one STATION node type; the plantspec's Kind field is advisory and never persisted; and the staging designation lives in the EDGE cell config, which Core does not have. The sender is the only party that knows. ── THE INCIDENT THIS WAS ATTRIBUTED TO WAS NOT THIS BUG ───── This field and its fix are UNCHANGED and keep their standing. What is struck is the causal claim, which stood here and at fourteen other sites: "Springfield, 2026-08-12: AMR-04 held a bin for 48 minutes unable to place at SLN_003, with the fleet reporting the robot RUNNING and no error. Order 4580 was cancelled by an admin after 2h05m. Nothing was broken — nothing had ever asked whether SLN_003 was free." The plant queries say otherwise. Order 4580's DESTINATION was ALN_004; SLN_003 was a mid-route waypoint, not the node it could not place at. The sibling order ran the identical route on the same robot and completed twelve minutes earlier. The fleet wedged. Whatever held AMR-04 for 48 minutes, an unreserved staging node was not it. It is quoted once, here, rather than at each of the fifteen sites that carried it: a false sentence reproduced fifteen times to mark its own deletion is the disease this round is treating. THE GAP IS STILL REAL AND STILL REACHABLE, which is why nothing else moves. A declared staging dropoff is reserved by nothing and checked by nothing; two orders can take the same node and the second robot arrives to find it full. That argument stands on the code above without an incident under it, and it is the argument the fix should always have carried — TestDeclaredStagingDropoffIsReserved and the two invariant walks are what hold it, not a story. This is WaitKind's mirror, and the same rule: carried across the wire so the far side does not have to guess. There it was Core knowing something the Edge could not infer; here it is the Edge knowing something Core cannot. DROPOFF-ONLY; ignored on pickup/wait. Backward-compatible: absent/false is exactly today's behaviour, so an older sender — and an older Core that ignores the field — behave as they do now. Setting it on a LINE node would re-create the 2b05dce deadlock, so senders must not. |

<!-- /generated -->

Steps are executed in sequence. A `wait` step causes the robot to dwell at its current position until an `order.release` message is received.

#### OrderRelease

Signals that a staged (dwelling) order should resume execution. Sent by the operator when the outgoing bin is ready for pickup.

```json
{
  "order_uuid": "b2c3d4e5-f6a7-8901-bcde-f12345678901"
}
```

<!-- generated: protocol struct OrderRelease -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID names the staged order to release. |
| `remaining_uop` | integer | omitempty |  |
| `disposition` | [`UOPDisposition`](#uopdisposition) | omitempty |  |
| `called_by` | string | omitempty |  |
| `station_wait` | integer | omitempty | StationWait is the echo: the station wait this release is for, as OrderStaged.StationWait numbered it. Core applies a release only for the station wait its wait_index points at, parked there or driving to it; any other is a logged no-op. Absent = an Edge that predates it, which gets today's behaviour. |
| `as_of` | [`ReleaseFence`](#releasefence) | omitempty | AsOf fences the bin's count stream at the declaration: RemainingUOP is the count as of this point in the station's (bin, epoch) delta stream. Core writes the declared count with no generation bump and moves its dedup cursor for the scope to it, so a delta flushed before the release and delivered after it is at or below high water and skipped (its count is in the declaration). Nil = an Edge that predates it, or a release with no bound carrier: Core bumps the generation at the release as before. |

<!-- /generated -->

##### UOPDisposition

<!-- generated: protocol struct UOPDisposition -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `kind` | string (`UOPDispositionKind`) |  |  |
| `count` | integer | omitempty |  |
| `captures` | object (string → integer) | omitempty |  |
| `count_suggested` | integer | omitempty |  |
| `captures_suggested` | object (string → integer) | omitempty |  |

<!-- /generated -->

##### ReleaseFence

<!-- generated: protocol struct ReleaseFence -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `bin_id` | integer |  |  |
| `epoch` | integer |  |  |
| `as_of_seq` | integer |  |  |
| `as_of_net` | integer |  |  |
| `as_of_at` | timestamp |  |  |

<!-- /generated -->

#### OrderIngestRequest

Reports a produced (filled) bin so Core records its inventory manifest. This is a **manifest-only** write: Core sets *and confirms* the bin's payload and produced count in one transaction, and dispatches **nothing** — no store order is created (that leg was removed with the retired simple-produce mode). It is fire-and-forget: Core processes the request asynchronously and does not reply on success; a failure is logged Core-side and returned as an `order.error`.

Bin identity resolves two ways: a non-empty `bin_label` is looked up directly (manual ingest — an operator scanned a real tote); a blank `bin_label` falls back to `source_node`, and Core resolves the bin from what is parked at that node (headless produce-finalize, which tracks the bin by id, not label).

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

<!-- generated: protocol struct OrderIngestRequest -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is a UUID v4 per message, a correlation id only: an ingest creates no order. |
| `payload_code` | string |  | PayloadCode is the payload whose manifest the bin is booked under. |
| `bin_label` | string |  | BinLabel is the label of the bin to ingest (a manual ingest: a person scanned a real tote). Blank: Core resolves the bin by SourceNode. |
| `bin_id` | integer | omitempty | BinID pins the exact Core bin (0 = absent → BinLabel/SourceNode resolution). Exists for the release-time produce manifest: by the time Core processes a deferred ingest, a press-index R2 may already have indexed the fresh tote onto the position the manifest's tote occupied, so resolve-by-node would credit the wrong bin. Edge passes the runtime's active bin id, which Core itself seeded at delivery. |
| `bin_epoch` | integer | omitempty | BinEpoch fences the ingest to one life of the bin: Core applies it only while the bin's delta_epoch still equals it, and refuses it with an audit row otherwise. The ingest neither expires nor dead-letters, so a late one must not re-stamp a bin that has since been emptied and refilled. 0 = absent (the manual HTTP door), today's behaviour. |
| `source_node` | string |  | SourceNode is where the filled bin is parked; required when BinLabel is blank, since Core then resolves the bin from what stands there. |
| `quantity` | integer |  | Quantity is the operator-measured produced count (UOP); 0 is the payload's capacity. |
| `manifest` | [`IngestManifestItem`](#ingestmanifestitem)[] | omitempty | Manifest lists the bin's items explicitly; absent, Core uses the payload's manifest template. |
| `produced_at` | string | omitempty | RFC3339 timestamp from Edge at cell completion |
| `declared_qty` | integer | omitempty | DeclaredQty is the count a station's RELEASE FULL declared for a bin it counted below its pack (0 = absent: no declaration, an older Edge, or a release that is not a station's FULL). Quantity keeps its meaning, the counted number. A declaration makes Core write the count (set_for_production) and then stamp the bin to the pack of the part it books the bin under, from its own catalog (release_declared_full, counted to that pack), in one transaction, with a row naming who declared it; the row's detail keeps DeclaredQty and the declared part. A pack at or below the count stamps nothing; no pack for the part books the count with a no_pack row. An older Core ignores the field and books the counted number. |
| `called_by` | string | omitempty | CalledBy names who declared the bin full, for the stamp's row. Meaningful only with DeclaredQty. |
| `order_id` | string | omitempty | OrderID is the Edge UUID of the DEPARTING order (the value Core stores as that order's edge_uuid), not this message's id: OrderUUID above is a fresh uuid per ingest message. Core resolves it to its own order id only when it writes a stamp, and when it cannot, writes the row with no order id and says so in the row's detail. |

<!-- /generated -->

##### IngestManifestItem

<!-- generated: protocol struct IngestManifestItem -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `part_number` | string |  | the part's identifier |
| `quantity` | integer |  | the count of this part |
| `description` | string | omitempty | a human-readable part description |

<!-- /generated -->

### Complex Order Payloads: Core -> Edge

#### OrderStaged

Notifies the edge that an order has entered a dwelling (wait) state at a staging node. The operator should release the order when the physical swap is ready.

```json
{
  "order_uuid": "b2c3d4e5-f6a7-8901-bcde-f12345678901",
  "detail":     "Waiting at STG-001 for operator release"
}
```

<!-- generated: protocol struct OrderStaged -->

| Field | Type | Omitted when zero | Description |
|---|---|---|---|
| `order_uuid` | string |  | OrderUUID is the edge's order UUID. |
| `detail` | string | omitempty | Detail is a human-readable status detail. |
| `station_wait` | integer | omitempty | StationWait is the wait's ordinal among the order's STATION waits (0 is the first). Both sides agree on it, where the raw wait index does not: Core splices lane waits into a plan and the Edge's copy has none. Absent at a lane wait, and from a Core that predates it. |
| `wait_kind` | string | omitempty | WaitKind is the wait's owner: WaitKindStation, or a Core kind — WaitKindLane, or WaitKindVacate when the station has released and Core holds the leg until a bin leaves the node it drops on (IsCoreWaitKind). |

<!-- /generated -->

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

**There is one status vocabulary, not two.** Core and Edge share the full set, and the canonical state machine is one table: `validTransitions` in `protocol/types.go:484-533`. Edge mirrors Core's vocabulary because `orders.ApplyCoreStatus` (`shingo-edge/orders/lifecycle_service.go:122`) writes Core's status onto the Edge row, so the operator sees the truth of whichever machine owns the order (`protocol/types.go:471-474`). The statuses below are the fifteen constants in `protocol/status.go:56-85`; one of them, `sourcing`, is history only since C1.

**There is no `completed` status.** Terminal completion is `confirmed`. Terminal is *derived*, never hand-maintained — a status is terminal iff it has no outgoing edges in `validTransitions` (`protocol.IsTerminal`, `protocol/types.go:539-542`). That makes exactly four terminal statuses: `confirmed`, `failed`, `cancelled`, `skipped`.

See [order-lifecycle.md](order-lifecycle.md) for the per-status table of scope, writer and operator-facing meaning. What follows is the wire view: which message drives which transition.

### The spine

The full graph is `validTransitions`; this is the path an order normally walks.

```
  intake       (acquiring)          transport              receipt
  ------       -----------          ---------              -------
  pending  ───────────────────▶  dispatched ──▶ in_transit ──▶ delivered ──▶ confirmed
     │                            ▲    │              └─▶ staged ─┘
     ├──▶ queued (a cause) ───────┘    │        (dwelling at a wait step)
     │       ▲                         │
     │       └──── fleet refusal ──────┘
     └──▶ submitted ──▶ acknowledged    (Edge's own submission words)

  Acquiring is the act between pending and dispatched, never a stored status.
  queued is the one resting place short of the fleet, and always carries a cause.

  off the spine:
    faulted      from acknowledged / dispatched / in_transit / staged — RECOVERABLE
    reshuffling  from pending / queued — a compound digs the source out, then resumes to pending
    skipped      from pending / submitted / queued — terminal
    failed       from any active state — terminal
    cancelled    from any active state — terminal
```

<!-- generated: protocol/status.go AllStatuses -->

| Status | Constant | Terminal | Moves to |
|---|---|---|---|
| `pending` | `StatusPending` | no | `submitted`, `queued`, `dispatched`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `sourcing` | `StatusSourcing` | no | `acknowledged`, `dispatched`, `in_transit`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `queued` | `StatusQueued` | no | `acknowledged`, `dispatched`, `in_transit`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `submitted` | `StatusSubmitted` | no | `acknowledged`, `queued`, `cancelled`, `failed`, `skipped` |
| `dispatched` | `StatusDispatched` | no | `in_transit`, `staged`, `delivered`, `queued`, `cancelled`, `faulted`, `failed` |
| `acknowledged` | `StatusAcknowledged` | no | `dispatched`, `in_transit`, `staged`, `cancelled`, `faulted`, `failed` |
| `in_transit` | `StatusInTransit` | no | `delivered`, `staged`, `cancelled`, `faulted`, `failed` |
| `staged` | `StatusStaged` | no | `in_transit`, `delivered`, `cancelled`, `faulted`, `failed` |
| `delivered` | `StatusDelivered` | no | `confirmed`, `cancelled`, `failed` |
| `confirmed` | `StatusConfirmed` | **yes** |  |
| `faulted` | `StatusFaulted` | no | `in_transit`, `delivered`, `failed`, `cancelled` |
| `failed` | `StatusFailed` | **yes** |  |
| `cancelled` | `StatusCancelled` | **yes** |  |
| `reshuffling` | `StatusReshuffling` | no | `confirmed`, `pending`, `cancelled`, `failed` |
| `skipped` | `StatusSkipped` | **yes** |  |

<!-- /generated -->

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
 |                              |                              | (mark confirmed — terminal)
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
 |                              |                              | (mark confirmed — terminal)
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

<!-- generated: shingo-core/store/schema/schema.snapshot.sql edge_registry -->

| Column | Type | Not null | Default | Key |
|---|---|---|---|---|
| `id` | `bigint` | yes | `nextval('edge_registry_id_seq')` | PK |
| `station_uid` | `text` | yes | `''` |  |
| `display_name` | `text` | yes | `''` |  |
| `station_id` | `text` | yes |  | unique |
| `hostname` | `text` | yes | `''` |  |
| `version` | `text` | yes | `''` |  |
| `registered_at` | `timestamp with time zone` | yes | `now()` |  |
| `last_heartbeat` | `timestamp with time zone` |  |  |  |
| `status` | `text` | yes | `'active'` |  |
| `bound_hostname` | `text` | yes | `''` |  |
| `bound_instance` | `text` | yes | `''` |  |
| `prev_instance` | `text` | yes | `''` |  |
| `bound_at` | `timestamp with time zone` |  |  |  |
| `claimed_at` | `timestamp with time zone` |  |  |  |
| `conflict_hostname` | `text` | yes | `''` |  |
| `conflict_count` | `bigint` | yes | `0` |  |
| `conflict_at` | `timestamp with time zone` |  |  |  |
| `schema_version` | `integer` |  |  |  |
| `clock_offset_ms` | `bigint` |  |  |  |
| `timezone` | `text` | yes | `''` |  |
| `tick_pending` | `bigint` |  |  |  |
| `tick_oldest_unsent_age_ms` | `bigint` |  |  |  |
| `tick_reported_at` | `timestamp with time zone` |  |  |  |
| `tick_rejected` | `bigint` | yes | `0` |  |

- Unique (station_uid) where `station_uid <> ''`

<!-- /generated -->

---

## Data Types Reference

### Primitive Types

| Type | JSON Representation | Notes |
|---|---|---|
| string | `"text"` | UTF-8 encoded. |
| integer | `42` | Signed 64-bit. No fractional part. |
| float | `1.5` | IEEE 754 double-precision. |
| boolean | `true` / `false` | |
| string[] | `["a", "b"]` | JSON array of strings; `T[]` is an array of any type below. |
| object (K → V) | `{"k": "v"}` | JSON object used as a map from K to V. |
| JSON | any JSON value | Raw JSON (`json.RawMessage`), decoded by whatever reads it. |
| string (`Name`) | `"text"` | A string whose values are a named Go type's (a `protocol` enum, or a type that encodes itself as text). |
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
    "src":  {"role": my_role, "station": my_station_id},
    "dst":  {"role": target_role, "station": target_station},
    "ts":   now_utc_iso8601(),
    "exp":  now_utc_iso8601() + ttl_for(message_type),
    "p":    payload_object
}

# For data channel messages:
envelope = {
    "v":    1,
    "type": "data",
    "id":   generate_uuid_v4(),
    "src":  {"role": my_role, "station": my_station_id},
    "dst":  {"role": target_role, "station": target_station},
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
| Material demand | Group `order.request` by `payload_code` and `delivery_node` |
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
shingo.orders ---+--> filter(type="order.request") --+--> demand_by_material (group by payload_code)
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
  "src": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "dst": {"role": "core", "station": ""},
  "ts": "2026-02-18T10:00:00Z",
  "exp": "2026-02-18T10:05:00Z",
  "p": {
    "subject": "edge.register",
    "data": {
      "station_id": "stn-4f8a1c02b7e39d15",
      "hostname": "edge-01.local",
      "version": "1.2.0"
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
  "src": {"role": "core", "station": "core"},
  "dst": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "ts": "2026-02-18T10:00:01Z",
  "exp": "2026-02-18T10:05:01Z",
  "cor": "f2b0ffe2-420b-42ee-849c-cb7434233cbb",
  "p": {
    "subject": "edge.registered",
    "data": {
      "station_id": "stn-4f8a1c02b7e39d15",
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
  "src": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "dst": {"role": "core", "station": ""},
  "ts": "2026-02-18T10:01:00Z",
  "exp": "2026-02-18T10:06:00Z",
  "p": {
    "subject": "edge.heartbeat",
    "data": {
      "station_id": "stn-4f8a1c02b7e39d15",
      "feeds": {}
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
  "src": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
  "dst": {"role": "core", "station": ""},
  "ts": "2026-02-18T10:05:00Z",
  "exp": "2026-02-18T10:15:00Z",
  "p": {
    "order_uuid": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
    "order_type": "retrieve",
    "payload_code": "BIN-A",
    "quantity": 1,
    "delivery_node": "line-1-station-a"
  }
}
```

### Order Acknowledgement (core -> edge)

```json
{
  "v": 1,
  "type": "order.ack",
  "id": "d9f6a7b8-9012-3456-cdef-789012abcdef",
  "src": {"role": "core", "station": "core"},
  "dst": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
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
  "src": {"role": "core", "station": "core"},
  "dst": {"role": "edge", "station": "stn-4f8a1c02b7e39d15"},
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
      "required": ["role", "station"],
      "properties": {
        "role":    {"type": "string", "enum": ["edge", "core"]},
        "station": {"type": "string"}
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
