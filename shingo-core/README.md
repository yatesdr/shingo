# ShinGo Core

Central server for the ShinGo material tracking system. Receives transport orders from edge stations, resolves source/destination nodes, dispatches to the robot fleet, and tracks fulfillment through delivery.

## Quick Start

### Prerequisites

- Go 1.24+
- PostgreSQL 14+ (recommended) or SQLite
- Kafka broker
- Seer RDS fleet backend (or compatible)

### Build & Run

```sh
make build
./shingocore --config shingocore.yaml
```

The web UI is available at `http://localhost:8083` by default. Default login: `admin` / `admin`.

### Configuration

A default config is generated on first run. The only setting you typically need before starting is the database connection — see [PostgreSQL Setup](docs/postgresql-setup.md). All other settings can be adjusted through the web UI at `/config`.

See [Configuration Reference](docs/configuration.md) for details.

## Documentation

| Document | Description |
|----------|-------------|
| [Configuration Reference](docs/configuration.md) | Full YAML config reference |
| [PostgreSQL Setup](docs/postgresql-setup.md) | Database server setup, user creation, connection config |
| [UI Guide](docs/ui-guide.md) | All web pages with screenshots |
| [Architecture](docs/architecture.md) | Package layout, data flow, key patterns |
| [API Reference](docs/api-reference.md) | REST API endpoints |
| [Wire Protocol](../docs/wire-protocol.md) | Kafka messaging protocol spec |
| [Terminology](../docs/terminology.md) | Domain terms and vendor mapping |

## Build Targets

```sh
make build      # Build for current platform
make test       # Run tests
make all        # Cross-compile (linux/windows/macos)
make fmt        # Format code
make vet        # Static analysis
```

## Debug Logging

Enable subsystem-filtered debug output:

```sh
./shingocore --log-debug=dispatch,rds,kafka
```

Available subsystems: `rds`, `kafka`, `dispatch`, `protocol`, `outbox`, `core_handler`, `nodestate`, `engine`

## License

Proprietary. See [LICENSE](../LICENSE).
