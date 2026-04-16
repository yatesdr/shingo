# Shingo Core

Central server for the Shingo material tracking system. Receives transport orders from edge stations, resolves source and destination nodes, dispatches to the robot fleet, and tracks fulfillment through delivery.

## Prerequisites

- Go 1.25+
- PostgreSQL 14+
- Kafka broker
- Seer RDS fleet backend (or compatible)

## Quick Start

```sh
make build
./shingocore --config shingocore.yaml
```

The web UI is available at `http://localhost:8083`. Default login: `admin` / `admin`.

### Initial Setup

The database connection is the only setting that must be configured before first launch. Create a minimal `shingocore.yaml` with the connection details:

```yaml
database:
  postgres:
    host: 192.168.1.10
    port: 5432
    database: shingocore
    user: shingocore
    password: your-password
    sslmode: disable
```

See [PostgreSQL Setup](docs/postgresql-setup.md) for server-side database configuration.

All other settings — fleet connection, Kafka brokers, web server port — have sensible defaults and can be adjusted through the web UI at `/config`. The YAML file is application-managed and should not be edited by hand during normal operation.

On first startup, Shingo Core creates all database tables, indexes, and a default admin user automatically.

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

Without a filter (`--log-debug`), all subsystems are logged.

| Subsystem | Description |
|-----------|-------------|
| `rds` | Fleet API requests and responses |
| `kafka` | Kafka producer and consumer events |
| `dispatch` | Order routing and dispatch decisions |
| `protocol` | Wire protocol encode and decode |
| `outbox` | Outbox drain cycles |
| `core_handler` | Inbound message handling |
| `reconciliation` | Drift detection and recovery signals |
| `nodestate` | Node state cache operations |
| `engine` | Engine lifecycle events |
| `countgroup` | Advanced-zone polling, hysteresis transitions, fail-safe |

## Web Interface

Shingo Core provides a management interface for fleet operations, inventory, and system administration. Key pages:

| Page | Route | Description |
|------|-------|-------------|
| Dashboard | `/` | Real-time system overview with active orders and fleet status |
| Nodes | `/nodes` | Registered locations, supermarket visualization, fleet sync |
| Orders | `/orders` | Transport order list with status tracking and timeline |
| Robots | `/robots` | Live fleet status with availability and task controls |
| Bins | `/bins` | Physical container management, payload assignment, lifecycle |
| Payloads | `/payloads` | Payload template definitions and manifest configuration |
| Demand | `/demand` | Material demand planning and order generation |
| Test Orders | `/test-orders` | Order submission for testing (Kafka and direct fleet) |
| Diagnostics | `/diagnostics` | Debug logs, CMS transactions, reconciliation, recovery actions, and fire alarm control |
| Configuration | `/config` | Runtime settings (fleet, messaging) |
| Fleet Explorer | `/fleet-explorer` | Raw API explorer for the fleet backend |

See [UI Guide](docs/ui-guide.md) for detailed page descriptions.

## Reliability Notes

- All Core-to-Edge business and control traffic is queued through the durable outbox.
- Core tracks processed inbound envelope IDs in a durable inbox to suppress replayed mutating commands.
- The diagnostics page includes reconciliation drift checks and recovery tooling for dead-lettered outbox messages.
- The `/health` endpoint now reflects reconciliation severity in addition to dependency connectivity.
- Count-group polling includes a fail-safe timeout that forces safety lights on during sustained RDS communication failure.
- Fire alarm activate/clear commands are audit-logged with actor, timestamp, and auto-resume setting.

## Documentation

| Document | Description |
|----------|-------------|
| [UI Guide](docs/ui-guide.md) | Web interface pages and features |
| [API Reference](docs/api-reference.md) | REST API endpoints |
| [PostgreSQL Setup](docs/postgresql-setup.md) | Database server setup and connection |
| [Architecture](docs/architecture.md) | Package layout, data flow, extension points |
| [Wire Protocol](../docs/wire-protocol.md) | Kafka messaging protocol specification |
| [Terminology](../docs/terminology.md) | Domain terms and vendor mapping |

## License

Proprietary. See [LICENSE](../LICENSE).
