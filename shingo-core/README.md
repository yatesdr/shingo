# Shingo Core

Verified against: b9210bce

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
make test       # Run unit + fake-backed tests (no Docker required)
make test-all   # Run full suite including Postgres-backed tests (requires Docker)
make all        # Cross-compile (linux/windows/macos)
make fmt        # Format code
make vet        # Static analysis
```

### Docker-gated tests

Test files that need a live Postgres carry `//go:build docker` on
their first line (dispatch/, engine/, messaging/, service/, store/,
www/, and `shingo-edge/store/outbox_test.go`). `make test` skips
them by omitting the tag, so the default target runs fast and
green on a machine without Docker. `make test-all` (or
`go test -tags=docker ./...`) pulls them back in.

Behavioral contracts that don't need a DB — narrow-interface
satisfaction, algorithm correctness for bin resolution, scanner
flow — are covered by fake-backed tests in `material/`,
`fulfillment/`, and `dispatch/binresolver/`. Those stay tag-free
so they run on every push without Docker.

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

## Web Interface

Shingo Core provides a management interface for fleet operations, inventory, and system administration. Its pages, generated from `shingo-core/www/router.go`:

<!-- generated: shingo-core/www/router.go pages -->

| Path | Template (in `shingo-core/www/templates/`) | Title | Auth |
|---|---|---|---|
| `/` | `dashboard.html` | Dashboard | public |
| `/bins` | `bins.html` | Bins | login |
| `/config` | `config.html` | Configuration | login |
| `/cycle-time` | `cycle-time.html` | Cycle time | public |
| `/demand-episodes` | `demand-episodes.html` | Demand episodes | public |
| `/demand-episodes/{originID}` | `demand-episode.html` |  | public |
| `/diagnostics` | `diagnostics.html` | Logs | login |
| `/edges` | `edges.html` | Stations | login |
| `/fleet-explorer` | `rds_explorer.html` | Fleet Explorer | login |
| `/heartbeat` | `heartbeat.html` | Production Heartbeat — Shingo | public |
| `/inventory` | `inventory.html` | Inventory | public |
| `/inventory/count-history` | `count-history.html` |  | public |
| `/login` | `login.html` | Login | public |
| `/material-flags` | `material-flags.html` | Material flags | public |
| `/missions` | `missions.html` | Mission Telemetry | public |
| `/missions/{orderID}` | `mission-detail.html` |  | public |
| `/nodes` | `nodes.html` | Nodes | public |
| `/orders` | `orders.html` | Orders | public |
| `/orphans` | `orphans.html` | Orphan reconciliation | public |
| `/overview` | `overview.html` | Operations Overview | public |
| `/payloads` | `payloads.html` | Payloads | login |
| `/robots` | `robots.html` | Robots | public |
| `/sourcing` | `sourcing.html` | Sourcing | login |
| `/test-orders` | `test-orders.html` | Test Orders | login |
| `/wall-display/{id}` | `dashboard-display.html`, `dashboard-frame.html`, `dashboard-map.html`, `dashboard-node-report.html`, `heartbeat.html` |  | public |

<!-- /generated -->

See [UI Guide](docs/ui-guide.md) for detailed page descriptions.

## Reliability Notes

- All Core-to-Edge business and control traffic is queued through the durable outbox.
- Core tracks processed inbound envelope IDs in a durable inbox to suppress replayed mutating commands.
- The diagnostics page includes reconciliation drift checks and recovery tooling for dead-lettered outbox messages.
- The `/health` endpoint now reflects reconciliation severity in addition to dependency connectivity.
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
