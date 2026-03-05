# Architecture

Developer reference for ShinGo Core internals. Covers package layout, data flow, key patterns, and extension points.

## Package Layout

```
shingo-core/
  cmd/shingocore/       Entry point, flag parsing, component wiring
  config/               YAML config loading, hot-reload support
  engine/               Core orchestrator: lifecycle, event bus, order handlers
  dispatch/             Order routing: resolve source/dest, dispatch to fleet
  fleet/                Fleet backend interface (vendor-agnostic)
  rds/                  Seer RDS HTTP client (fleet.Backend implementation)
  store/                Database layer: schema, migrations, queries
  nodestate/            Node state cache (payload tracking at nodes)
  messaging/            Kafka producer/consumer, outbox drainer
  www/                  Web server: router, handlers, templates, SSE
  debuglog/             Subsystem-filtered debug logger
```

## Startup Sequence

The `main()` function in `cmd/shingocore/main.go` initializes components in this order:

```
1. Load YAML config
2. Open database (auto-migrate schema)
3. Initialize node state manager
4. Connect to Seer RDS fleet backend
5. Connect to Kafka
6. Create engine (wires all components)
7. Start engine (starts event bus, fleet poller, periodic tasks)
8. Start protocol ingestor (Kafka consumer on shingo.orders)
9. Start outbox drainer (publishes to shingo.dispatch)
10. Start web server
```

Shutdown is graceful on `SIGINT`/`SIGTERM`: components stop in reverse order.

## Data Flow

### Order Lifecycle (Retrieve)

```
Edge Station                    Kafka                         ShinGo Core
     |                            |                               |
     |-- order.request --------->| shingo.orders                  |
     |                            |------>  Ingestor              |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      CoreHandler              |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      Dispatcher               |
     |                            |       |    |                  |
     |                            |       |    v                  |
     |                            |       | Resolver              |
     |                            |       |  (find source node,   |
     |                            |       |   claim payload)      |
     |                            |       |    |                  |
     |                            |       v    v                  |
     |                            |      Fleet Backend            |
     |                            |       (create RDS order)      |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      DB: update order status  |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      EventBus: emit events   |
     |                            |          |                    |
     |                            |          v                    |
     |                     outbox | <---- Outbox: enqueue reply   |
     |                            |                               |
     |<-- order.ack -------------|  shingo.dispatch               |
     |                            |                               |
     |                            |      Fleet Poller             |
     |                            |       (polls RDS every 5s)    |
     |                            |          |                    |
     |                            |          v                    |
     |                            |      Status change detected   |
     |                            |          |                    |
     |<-- order.waybill ---------|          v                    |
     |<-- order.delivered -------|      Outbox: enqueue updates  |
```

### Message Ingest Pipeline

```
Kafka Consumer (shingo.orders)
    |
    v
Protocol Ingestor (two-phase decode)
    |-- Phase 1: parse header (version, expiry, destination)
    |-- Drop if expired or wrong destination
    |-- Phase 2: full payload decode
    |
    v
CoreHandler
    |-- order.request  --> Dispatcher.HandleOrder()
    |-- order.cancel   --> Dispatcher.HandleCancel()
    |-- order.receipt  --> Engine.HandleReceipt()
    |-- order.redirect --> Dispatcher.HandleRedirect()
    |-- data (edge.register)   --> Engine.RegisterEdge()
    |-- data (edge.heartbeat)  --> Engine.HandleHeartbeat()
```

### Outbox Pattern

Messages to edge stations are never sent directly to Kafka. Instead:

```
Handler logic
    |
    v
DB: INSERT INTO outbox (topic, data, msg_type, station_id)
    |
    v
OutboxDrainer (runs every 5s)
    |-- SELECT from outbox WHERE sent_at IS NULL
    |-- Publish each to Kafka
    |-- UPDATE outbox SET sent_at = NOW()
```

This ensures at-least-once delivery even when Kafka is temporarily unavailable.

## Key Patterns

### EventBus

Synchronous pub/sub within the engine process. Used to decouple order lifecycle handling from side effects (audit logging, payload tracking, edge notifications).

```go
// Subscribe to specific event types
engine.Events.SubscribeTypes(func(evt Event) {
    ev := evt.Payload.(OrderCompletedEvent)
    // handle completion
}, EventOrderCompleted)

// Emit an event
engine.Events.Emit(Event{
    Type:    EventOrderCompleted,
    Payload: OrderCompletedEvent{OrderID: 42},
})
```

Event types: `OrderDispatched`, `OrderStatusChanged`, `OrderFailed`, `OrderCompleted`, `OrderCancelled`, `OrderReceived`, `PayloadChanged`, `NodeUpdated`, `CorrectionApplied`

Event handlers are wired in `engine/wiring.go`.

### Fleet Backend Interface

The `fleet.Backend` interface abstracts over vendor-specific fleet management systems. The current implementation uses Seer RDS.

```go
type Backend interface {
    CreateOrder(req OrderRequest) (string, error)  // returns vendor order ID
    CancelOrder(vendorID string) error
    GetOrderStatus(vendorID string) (string, error)
    ListRobots() ([]RobotStatus, error)
    MapState(vendorState string) string            // vendor state -> shingo status
    IsTerminalState(vendorState string) bool
    // ... additional methods
}
```

To add a new fleet vendor, implement `fleet.Backend` and wire it in `main.go`.

### Adapter/Emitter Interfaces

Each package defines its own emitter interface to avoid import cycles. The engine bridges them during initialization.

```
dispatch.Emitter  -- emits order events via engine.Events
store.DB          -- shared by engine, dispatch, handlers
fleet.Backend     -- used by dispatch, engine
```

### Dialect Abstraction (SQL)

The store layer supports both SQLite and PostgreSQL using placeholder rewriting:

```go
// Write queries with ? placeholders
db.Q("SELECT * FROM nodes WHERE id = ? AND zone = ?", id, zone)

// Rebind() converts to $1, $2 for PostgreSQL at runtime
```

Schema files are separate: `schema_sqlite.go` and `schema_postgres.go`. Migrations run automatically on startup.

### sendToEdge Helper

A common pattern for sending protocol messages to edge stations:

```go
func (e *Engine) sendToEdge(msgType string, stationID string, payload any) error
```

This builds a protocol envelope, encodes it, and enqueues it in the outbox. Used for waybills, status updates, delivery notifications, and cancellations.

## Database Schema

### Core Tables

| Table | Purpose |
|-------|---------|
| `nodes` | Physical locations (storage, line-side, staging) |
| `node_types` | Node type definitions (STG, LSL, SUP, etc.) |
| `node_stations` | Node-to-station assignments |
| `node_payload_types` | Which payload types a node accepts |
| `node_properties` | Key-value properties per node |
| `orders` | Transport orders with full lifecycle state |
| `order_history` | Status change log per order |
| `payload_types` | Container type definitions |
| `payloads` | Tracked containers/bins |
| `manifest_items` | Items inside each payload |
| `corrections` | Manual inventory corrections |
| `demands` | Material demand planning entries |
| `production_log` | Production event log |
| `outbox` | Message queue for Kafka delivery |
| `audit_log` | System-wide audit trail |
| `admin_users` | Web UI authentication |
| `edge_registry` | Connected edge station tracking |
| `scene_points` | Fleet scene/map point cache |
| `test_commands` | RDS command test history |

### Key Relationships

```
node_types ---< nodes ---< payloads ---< manifest_items
                  |
                  +---< node_properties
                  +---< node_stations
                  +---< node_payload_types

payload_types ---< payloads

orders ---< order_history
```

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
Connected browsers (GET /events)
```

Event types sent to browsers: `order-update`, `node-update`, `payload-update`, `robot-update`, `debug-log`

## Testing

```sh
make test                                           # all tests
go test -v ./dispatch -run TestHandleOrderRequest   # single test
go test -v ./store                                  # store layer
```

- Store tests use temporary SQLite databases via `t.TempDir()`
- Dispatch tests use mock emitter and fleet backend interfaces
- No external dependencies (Kafka, PostgreSQL, RDS) needed for tests

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/go-chi/chi/v5` | HTTP router |
| `github.com/gorilla/sessions` | Cookie-based sessions |
| `github.com/jackc/pgx/v5/stdlib` | PostgreSQL driver |
| `modernc.org/sqlite` | Pure-Go SQLite driver |
| `github.com/segmentio/kafka-go` | Kafka client |
| `github.com/google/uuid` | UUID generation |
| `golang.org/x/crypto/bcrypt` | Password hashing |
| `gopkg.in/yaml.v3` | YAML config parsing |

## Extension Points

### Adding a New Fleet Vendor

1. Implement `fleet.Backend` in a new package (e.g., `mir/`)
2. Add config fields in `config/config.go`
3. Wire the backend in `cmd/shingocore/main.go` based on a config flag

### Adding a New Web Page

1. Create `www/templates/your-page.html` with `{{define "content"}}...{{end}}`
2. Add it to the `pages` slice in `www/router.go`
3. Create handler in `www/handlers_your_page.go`
4. Register route in the router

### Adding a New Event Type

1. Define the event type constant and payload struct in `engine/events.go`
2. Add the subscription handler in `engine/wiring.go`
3. If it needs SSE broadcast, add a listener in `www/sse.go`

### Adding a New Data Channel Subject

1. Define the subject constant and data struct in `protocol/types.go`
2. Add a handler case in `messaging/core_handler.go`
3. No protocol version bump needed — see [Wire Protocol](../docs/wire-protocol.md)
