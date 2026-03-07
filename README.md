# Shingo

Material tracking and automated transport system for manufacturing plants. Shingo manages the flow of bins and materials between warehouse storage and production line stations using autonomous mobile robots. Named from the Japanese 信号 ("signal" / "instruction"), Shingo bridges the robot fleet manager and production processes, coordinating material requests and delivery.

## Modules

| Module | Description |
|--------|-------------|
| [**shingo-core**](shingo-core/) | Central server. Receives orders from edge stations, resolves source and destination nodes, dispatches to the robot fleet, and tracks fulfillment. PostgreSQL or SQLite. |
| [**shingo-edge**](shingo-edge/) | Shop-floor client. Runs at each production line. Tracks PLC counters, manages material inventory, handles operator workflows, and communicates with core over Kafka. |
| [**protocol**](protocol/) | Shared wire protocol. JSON envelope format, message types, two-phase decode, and TTL-based expiry. |

## Concepts

### Bin-Centric Tracking

Shingo tracks material at the bin level. A **bin** is a physical container (tote, pallet, shelf unit) identified by a QR code label. Bins move between **nodes** — fixed floor locations such as storage slots, staging areas, and line-side positions. The bin record carries its manifest, remaining production capacity, and confirmation state. All dispatch and inventory logic operates on bins.

### Payload Templates

A **payload** defines what a bin should contain: a parts list and a unit-of-production (UOP) capacity. Payloads are templates. When a bin is loaded with material, it is assigned a payload code; the operator confirms the manifest; and the bin becomes eligible for automated retrieval.

### Automated Dispatch

Operators request material by payload type. Shingo Core locates the oldest eligible bin (FIFO), dispatches a robot to retrieve it, and tracks the order through delivery and operator confirmation. Storage orders work in reverse — the system selects the optimal storage slot automatically.

### Decoupled Architecture

Core and edge communicate asynchronously over Kafka. Each edge station operates independently; if connectivity to core is lost, the edge continues tracking local state and queues outbound messages for later delivery.

## Philosophy

- **System-directed retrieval.** Operators request material by type; the system handles sourcing, routing, and delivery.
- **Engineered depletion.** Bins are loaded so all parts deplete together after a known number of production cycles. A single counter — UOP remaining — describes consumption state.
- **FIFO enforcement.** The oldest material is always retrieved first, enforced automatically by the storage and retrieval logic.
- **Physical verification.** Each bin carries a QR code scanned at pickup to confirm identity and maintain chain of custody.
- **Vendor-agnostic fleet integration.** The fleet backend is abstracted behind an interface. The current implementation targets Seer RDS; other vendors can be added without changes to the dispatch layer.

## Structure

```
shingo/
  protocol/       shared Go module (shingo/protocol)
  shingo-core/    central server (module: shingocore)
  shingo-edge/    shop-floor client (module: shingoedge)
  docs/           shared reference documentation
```

## Getting Started

Each module builds independently. Requires Go 1.24 or later.

```sh
cd shingo-core && make build
cd shingo-edge && go build ./...
cd protocol    && go test ./...
```

See [shingo-core/README.md](shingo-core/README.md) and [shingo-edge/README.md](shingo-edge/README.md) for setup and usage instructions.

## Documentation

### Shared

| Document | Description |
|----------|-------------|
| [Data Model](docs/data-model.md) | Core entities, relationships, and status definitions |
| [Bins and Payloads](docs/payloads.md) | Bin management, payload assignment, supermarket storage |
| [Terminology](docs/terminology.md) | Domain terms and vendor terminology mapping |
| [Wire Protocol](docs/wire-protocol.md) | Kafka messaging protocol specification |

### Shingo Core

| Document | Description |
|----------|-------------|
| [Core README](shingo-core/README.md) | Quick start, build targets, debug logging |
| [UI Guide](shingo-core/docs/ui-guide.md) | Web interface pages and features |
| [API Reference](shingo-core/docs/api-reference.md) | REST API endpoints |
| [PostgreSQL Setup](shingo-core/docs/postgresql-setup.md) | Database server setup and connection |
| [Architecture](shingo-core/docs/architecture.md) | Package layout, data flow, extension points |

### Shingo Edge

| Document | Description |
|----------|-------------|
| [Edge README](shingo-edge/README.md) | Quick start, build targets, debug logging |
| [UI Guide](shingo-edge/docs/ui-guide.md) | Operator interface pages and workflows |

## Messaging

Core and edge communicate over Kafka using a JSON envelope protocol with dual topics:

- `shingo.orders` — edge to core (order requests, registration, heartbeats)
- `shingo.dispatch` — core to edge (acknowledgements, status updates, delivery notifications)

See [docs/wire-protocol.md](docs/wire-protocol.md) for the full specification.

## License

Proprietary. See [LICENSE](LICENSE).
