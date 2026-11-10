# Shingo Core

Verified against: 51489c78

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

The database connection is the only setting that must be configured before first launch. Copy [`shingocore.example.yaml`](shingocore.example.yaml) to `shingocore.yaml` and set its `database.postgres` block; [PostgreSQL Setup](docs/postgresql-setup.md) covers the server side.

Every other key has a default. The fleet, messaging, notifications, fire-alarm, plant-timezone and database settings are edited on the `/config` page, which writes the file; the rest are YAML-only. [Configuration](docs/configuration.md) lists every key and says which settings apply only after a restart.

On first startup, Shingo Core creates all database tables, indexes, and a default admin user automatically.

## Build Targets

```sh
make build      # Build for current platform
make test       # Run unit + fake-backed tests (no Docker required)
make test-all   # Run the Postgres-backed suite too (requires Docker)
make all        # Cross-compile (linux/windows/macos)
make fmt        # Format code
make vet        # Static analysis
```

### Docker-gated tests

Test files that need a live Postgres carry `//go:build docker` on
their first line (most of them in dispatch/, engine/, messaging/,
service/, store/, uop/ and www/). `make test` runs without the tag,
so it needs no Docker. `make test-all` runs the tagged suite through
`scripts/gate.sh docker`, which starts one shared Postgres container
for every package.

Behavioral contracts that don't need a DB — narrow-interface
satisfaction, bin-resolution algorithms, scanner flow — are covered
by tag-free fake-backed tests in `material/`, `fulfillment/` and
`dispatch/binresolver/`, so they run on every push without Docker.

## Debug Logging

Enable subsystem-filtered debug output to `shingo-debug.log`:

```sh
./shingocore --log-debug=dispatch,rds,kafka
```

Without a filter (`--log-debug`), all subsystems are logged. The
subsystem names, and how the file, stderr and the in-browser log
differ, are in [Configuration](docs/configuration.md).

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
- Core records processed inbound envelope IDs in a durable inbox, so a replayed order-channel message is dropped (`middleware.NewInboxDedup`).
- The Logs page (`/diagnostics`) carries the reconciliation drift checks and the recovery tooling for dead-lettered outbox messages.
- `GET /api/health` reports fleet, messaging and database connectivity beside the reconciliation summary; its `status` is that summary's verdict (`ok`, `degraded` or `critical`), not connectivity.
- Fire alarm activate/clear commands are audit-logged with actor and auto-resume setting.

## Documentation

| Document | Description |
|----------|-------------|
| [UI Guide](docs/ui-guide.md) | Web interface pages and features |
| [API Reference](docs/api-reference.md) | HTTP routes, SSE events, error format |
| [Configuration](docs/configuration.md) | Flags, debug subsystems, every YAML key |
| [PostgreSQL Setup](docs/postgresql-setup.md) | Database server setup and connection |
| [Architecture](docs/architecture.md) | Package layout, data flow, extension points |
| [CMS Integration](docs/cms-integration.md) | The inventory-transaction feed to CMS |
| [Wall Displays](docs/dashboard-platform.md) | Floor-facing kiosk displays |
| [Deploy artifacts](deploy/README.md) | What the installers put on a plant box |
| [Wire Protocol](../docs/wire-protocol.md) | Kafka messaging protocol specification |
| [Terminology](../docs/terminology.md) | Domain terms and vendor mapping |

## License

Proprietary. See [LICENSE](../LICENSE).
