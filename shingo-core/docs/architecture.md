# Architecture

Verified against: fe06474a

Developer reference for ShinGo Core internals. Covers package layout, data flow, key patterns, and extension points.

## Package Layout

The top-level packages of `shingo-core/`; `go list ./...` from the module is the full list.

| Package | Holds |
|---|---|
| `cmd/shingocore/` | Entry point and composition root: flags, wiring, the two inbound dispatch tables (`routers.go`) |
| `cmd/` (others) | Dev tools: `seeddev`, `soakstat`, `simcalc`, the schema and defaults snapshot writers |
| `config/` | YAML config: `Defaults()`, `Load`, `Save`, per-section validation |
| `domain/` | Pure data types: bins, nodes, payloads, orders and their children |
| `engine/` | The orchestrator: lifecycle, the event bus and its subscribers (`wiring*.go`), service accessors |
| `dispatch/` | Order routing and the lifecycle state machine; `dispatch/binresolver/` (slot picking), `dispatch/binsource/`, `dispatch/plan/`, `dispatch/eta/` |
| `fulfillment/` | The queued-order fulfillment scanner, behind narrow Dispatcher/Resolver interfaces |
| `service/` | Validation + mutation behind the handlers (`BinService`, `NodeService`, `InventoryService`, …) |
| `messaging/` | Kafka client, outbox drainer, `CoreHandler`, `CoreDataService` (the Data-subject handlers); `messaging/middleware/` holds the inbox dedup |
| `asks/` | Core's half of the ask pair: routes an Edge's ask to the door that answers it |
| `store/` | Postgres: migrations, queries, outbox/inbox; one subpackage per table family (`store/bins`, `store/orders`, …) and `store/schema` for the baseline DDL |
| `www/` | Web server: router, handlers, templates, SSE, and the ask door bodies (`Doors`) |
| `fleet/` | The vendor-neutral fleet interface; `fleet/seerrds/` (the Seer RDS adapter), `fleet/simulator/` (in-memory, for tests and the sim) |
| `rds/` | The Seer RDS HTTP client the adapter uses |
| `scenesync/`, `scenemap/` | Mirroring the fleet's scene into nodes and scene points; parsing a robot's own map file |
| `material/`, `cms/` | CMS-boundary transaction building (pure); the CMS wire, client and poster — see [CMS Integration](cms-integration.md) |
| `kanban/`, `uop/` | Kanban loop arithmetic; the Core-side unit-of-production chokepoint |
| `notify/` | E-mail alerts |
| `plantspec/` | The declarative plant description `cmd/seeddev` loads |
| `internal/testdb/`, `testharness/` | Shared Postgres test harness; its re-export for the integration module |

Shared infrastructure lives in two sibling modules imported by both Core and Edge:
`protocol/` (envelope types, auth, debuglog, backoff, eventbus, outbox, router, clock) and
`shared/` (UI assets and the pure helpers promoted out of the binaries — see
[Shared layer](../../docs/shared-layer-promotion.md)).

## Startup Sequence

`main()` in `cmd/shingocore/main.go` initializes components in this order:

```
1. Parse flags; log the build stamp; start pprof if asked; open the debug log
2. Load YAML config; apply the stderr log gate
3. Open the database (migrate the schema)
4. Build the fleet backend (Seer RDS, or the simulator in a sim build) and ping it
5. Connect to Kafka (a failed connect retries in the background)
6. Create and start the engine (event bus, fleet poller, periodic tasks)
7. Build CoreHandler, CoreDataService, the ask server and the two dispatch tables
8. Start the protocol ingestor (Kafka consumer on messaging.orders_topic)
9. Start the outbox drainer (publishes on messaging.dispatch_topic) and the inbox purge
10. Start the web server
```

Shutdown is on `SIGINT`/`SIGTERM` (`awaitShutdown`): the web layer stops first,
then the deferred stops run in reverse order of start.

## Data Flow

### Order Lifecycle (Retrieve)

```
Edge Station                    Kafka                         ShinGo Core
     |                            |                               |
     |-- order.request --------->| orders topic                   |
     |                            |------>  Ingestor              |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      InboxDedup -> CoreHandler|
     |                            |          |                    |
     |                            |          v                    |
     |                            |      Dispatcher               |
     |                            |       (resolve source bin,    |
     |                            |        claim it, plan legs)   |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      Fleet Backend            |
     |                            |       (create RDS order)      |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      DB: order status         |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      EventBus: emit events    |
     |                            |          |                    |
     |                            |          v                    |
     |                     outbox | <---- Outbox: enqueue reply   |
     |                            |                               |
     |<-- order.ack -------------|  dispatch topic                |
     |                            |                               |
     |                            |      Fleet Poller             |
     |                            |       (every rds.poll_interval)|
     |                            |          |                    |
     |                            |          v                    |
     |                            |      Status change detected   |
     |                            |          |                    |
     |<-- order.waybill ---------|          v                    |
     |<-- order.delivered -------|      Outbox: enqueue updates  |
```

The statuses an order moves through, and what each leg does, are in
[Order Lifecycle](../../docs/order-lifecycle.md).

### Message Ingest Pipeline

```
Kafka consumer (messaging.orders_topic)
    |
    v
protocol.Ingestor (two-phase decode)
    |-- Phase 1: parse header (version, expiry, destination, src)
    |-- Expired: dropped; an expired ask is answered as expired
    |   ((*asks.Server).AnswerExpired, wired as Ingestor.OnExpired)
    |-- Phase 2: full payload decode
    |
    v
Protocol router (buildProtocolRouter), by envelope Type
    |-- order channel: inbox dedup middleware, then a CoreHandler method
    |       order.request / cancel / receipt / redirect / release / ingest,
    |       complex order request  --> messaging.Dispatcher
    |-- data: subject router (buildSubjectRouter), by Data subject
    |       edge.register, edge.heartbeat, ... --> CoreDataService methods
    |       edge.ask                            --> asks.Server --> www.Doors
    |-- reply-channel Types: registered no-ops (Core sends them, never receives)
```

`CoreHandler` holds the dispatcher behind the narrow `messaging.Dispatcher`
interface — one method per order-channel message type — rather than the
concrete `*dispatch.Dispatcher`. The composition root wires the concrete
dispatcher in; a compile-time assertion in `messaging/dispatcher.go` fails the
build if the two drift. Tests stub the dispatcher surface without spinning up
the real lifecycle machinery.

Both dispatch tables assert they are complete at build time:
`TestProtocolRouter_CoversEveryEnvelopeType` and
`TestSubjectRouter_CoversEveryInboundSubject` fail a table missing an envelope
Type or an inbound subject, and `TestCensus_AskKinds` fails an ask kind no door
answers. Boot repeats the same checks and refuses to start.

### Outbox Pattern

Messages to edge stations are never sent directly to Kafka. Handler logic
enqueues an outbox row, inside the caller's database transaction where it has
one; the outbox drainer (`messaging.NewOutboxDrainer`) publishes it. Enqueue
rings the drainer, so a message does not wait for the tick; the tick
(`messaging.outbox_drain_interval`) is the retry and purge cadence. Delivery
semantics — the retry ceiling, dead letters, ordering — are in
[Wire Protocol](../../docs/wire-protocol.md).

### Inbox Pattern

Order-channel messages from edge stations are deduplicated on the envelope
`id`, once, by a router middleware (`middleware.NewInboxDedup`, applied with
`UseFor` to the order-channel Types in `buildProtocolRouter`):

```
Inbound envelope (order channel)
    |
    v
NewInboxDedup middleware
    |-- INSERT INTO inbox (msg_id, msg_type, station_id) ON CONFLICT DO NOTHING
    |-- already recorded -> drop the replay (logged under inbox_dedup)
    |-- the insert failed  -> drop and log (forwarding could double-execute)
    |
    v
CoreHandler -> messaging.Dispatcher (interface)
```

This suppresses replayed requests, cancels, receipts, redirects, releases and
ingests without relying on in-memory state. Data and reply-channel Types pass
through ungated. Adding an order-channel Type means adding it to the `UseFor`
list. The inbox is purged daily (`(*store.DB).PurgeOldInbox`).

## Key Patterns

### EventBus

Synchronous pub/sub within the engine process (`engine.EventBus`, over
`protocol/eventbus`): subscribers run in registration order on the emitting
goroutine, and a panicking subscriber is recovered and logged. Used to decouple
order lifecycle handling from side effects (audit logging, bin tracking, edge
notifications, SSE).

```go
// Subscribe with the payload type bound at the call site
eventbus.SubscribeTyped(e.Events, func(evt eventbus.TypedEvent[EventType, OrderCompletedEvent]) {
    ev := evt.Payload
    // handle completion
}, EventOrderCompleted)

// Emit an event
e.Events.Emit(Event{
    Type:    EventOrderCompleted,
    Payload: OrderCompletedEvent{OrderID: 42},
})
```

`SubscribeTyped` skips (and logs) an event whose payload is not the bound type.
Event types are the `Event*` constants in `engine/events.go` — read the file
rather than a list here; it is the one that compiles. Event handlers are wired
in `engine/wiring.go` and its `wiring_*.go` siblings.

### Fleet Backend Interface

The `fleet.Backend` interface abstracts over vendor-specific fleet management systems. The current implementation uses Seer RDS.

```go
type Backend interface {
    CreateOrder(req CreateOrderRequest) (TransportOrderResult, error)
    CancelOrder(vendorOrderID string) error
    SetOrderPriority(vendorOrderID string, priority int) error
    Ping() error
    Name() string
    MapState(vendorState string) string            // vendor state -> shingo status
    IsTerminalState(vendorState string) bool
    ReleaseOrder(vendorOrderID string, blocks []OrderBlock, complete bool) error
    Reconfigure(cfg ReconfigureParams)
}
```

The composition root holds a `fleet.TrackingBackend`: a `Backend` plus the
order tracker the poller drives (`InitTracker`, `Tracker`).

### Adapter/Emitter Interfaces

Each package defines its own emitter interface to avoid import cycles. The engine bridges them during initialization.

```
dispatch.Emitter      -- emits order events via engine.Events
fleet.TrackerEmitter  -- the tracker's status changes, into the engine
store.DB              -- shared by engine, dispatch, handlers
fleet.Backend         -- used by dispatch, engine
```

### Database

The store layer uses PostgreSQL with native `$1, $2` placeholder syntax. The
baseline schema is `store/schema/postgres_ddl.go`; versioned migrations in
`store/migrations.go` run automatically on startup. The schema as a migrated
database actually has it is committed as `store/schema/schema.snapshot.sql`
(`TestSchemaSnapshotIsCurrent` fails when it goes stale); every table and
column is in [Data Model](../../docs/data-model.md).

### sendToEdge Helper

A common pattern for sending protocol messages to edge stations:

```go
func (e *Engine) sendToEdge(msgType string, stationID string, payload any) error
```

This builds a protocol envelope, encodes it, and enqueues it in the outbox. Used for waybills, status updates, delivery notifications, and cancellations.

## SSE (Server-Sent Events)

The `EventHub` in `www/sse.go` bridges engine events to browser clients:

```
Engine EventBus
    |
    v
EventHub.SetupEngineListeners()
    |-- subscribes to engine events
    |-- converts to SSE format
    |
    v
EventHub.Broadcast(eventType, data)
    |
    v
Connected browsers (GET /events, optionally ?topics=...)
```

The event names and their payloads are listed in
[API Reference](api-reference.md).

## Reconciliation And Recovery

Core runs reconciliation checks over orders, bins and the outbox — each check
is an `issue` value that `reconciliation.ListAnomalies` (`store/reconciliation`)
produces — and surfaces them through:

- the Logs page (`/diagnostics`), Reconciliation and Recovery tabs
- `GET /api/reconciliation` for programmatic inspection
- `GET /api/outbox/deadletters` and `POST /api/outbox/replay` for controlled recovery of dead-lettered outbound messages

## Testing

How to run the suites, and which test files need Docker, is in the
[module README](../README.md).

## Dependencies

Direct dependencies are in `shingo-core/go.mod`: chi (HTTP router),
gorilla/sessions (cookie sessions), pgx (Postgres driver), kafka-go, yaml.v3,
x/crypto (bcrypt), and testcontainers for the Docker-backed tests.

## Extension Points

### Adding a New Fleet Vendor

1. Implement `fleet.TrackingBackend` in a new package under `fleet/`
2. Add config fields in `config/config.go`
3. Choose the backend in `cmd/shingocore/main.go` based on that config

### Adding a New Web Page

1. Create a template in `www/templates/` with `{{define "content"}}...{{end}}`.
   `NewRouter` discovers every `templates/*.html` by glob and parses each
   against the layout and partials; there is no list to add it to.
2. Write its handler in a `www/handlers_*.go` file, rendering the template by
   file name (`h.render(w, r, "<name>.html", data)`).
3. Register the route in `NewRouter` — in the public group, or after
   `h.requireAuth` for a page that needs a login.
4. Add a nav link in `www/templates/layout.html` if the page belongs in the nav.

### Adding a New Event Type

1. Define the event type constant and payload struct in `engine/events.go`
2. Add the subscription handler in `engine/wiring.go`
3. If it needs SSE broadcast, add a listener in `www/sse.go`

### Adding a New Data Channel Subject

1. Define the subject constant and data struct in the `protocol` module, and
   add the subject to `protocol.CoreInboundSubjects`
2. Register its handler in `buildSubjectRouter` (`cmd/shingocore/routers.go`);
   `TestSubjectRouter_CoversEveryInboundSubject` fails until you do
3. No protocol version bump needed — see [Wire Protocol](../../docs/wire-protocol.md)
