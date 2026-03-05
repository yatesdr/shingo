# Shingo

Material tracking and automated transport system for manufacturing plants. Manages payload flow (bins, containers, raw materials) between warehouse storage and production line stations using autonomous mobile robots. Named from the Japanese 信号 ("signal" / "instruction"). Shingo bridges the AMR fleet manager and production processes, coordinating material requests and delivery.

## Modules

| Module | Description |
|--------|-------------|
| [**shingo-core**](shingo-core/) | Central server. Receives orders from edge nodes, resolves source/destination, dispatches to the robot fleet, and tracks fulfillment. PostgreSQL (recommended) or SQLite. |
| **shingo-edge** | Shop-floor client. Runs at each production line. Tracks PLC counters, manages payload inventory, handles operator order workflows (retrieve, store, move), and communicates with core via Kafka. |
| [**protocol**](protocol/) | Shared wire protocol. JSON envelope format, message types, payload schemas, two-phase decode, and TTL-based expiry. |

## Structure

```
shingo/
  protocol/       shared Go module (shingo/protocol)
  shingo-core/    central server (module: shingocore)
  shingo-edge/    shop-floor client (module: shingoedge)
  docs/           protocol and reference documentation
```

## Building

Each module builds independently. Go 1.24+.

```sh
cd shingo-core && make build
cd shingo-edge && go build ./...
cd protocol    && go test ./...
```

See [shingo-core/README.md](shingo-core/README.md) for full build targets and configuration.

## Documentation

| Document | Location |
|----------|----------|
| Core setup & configuration | [shingo-core/README.md](shingo-core/README.md) |
| PostgreSQL setup | [shingo-core/docs/postgresql-setup.md](shingo-core/docs/postgresql-setup.md) |
| Configuration reference | [shingo-core/docs/configuration.md](shingo-core/docs/configuration.md) |
| UI guide | [shingo-core/docs/ui-guide.md](shingo-core/docs/ui-guide.md) |
| Architecture & development | [shingo-core/docs/architecture.md](shingo-core/docs/architecture.md) |
| REST API reference | [shingo-core/docs/api-reference.md](shingo-core/docs/api-reference.md) |
| Wire protocol spec | [docs/wire-protocol.md](docs/wire-protocol.md) |
| Terminology | [docs/terminology.md](docs/terminology.md) |

## Messaging

Core and edge communicate over Kafka using a unified JSON envelope protocol with dual topics:

- `shingo.orders` — edge to core (order requests, heartbeats)
- `shingo.dispatch` — core to edge (acknowledgements, status updates)

See [docs/wire-protocol.md](docs/wire-protocol.md) for the full specification.

## License

Proprietary. See [LICENSE](LICENSE).
