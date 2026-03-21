# Shingo Edge

Shop-floor client for the Shingo material tracking system. Runs at each production line to track PLC counters, manage material inventory, handle operator order workflows, and communicate with Shingo Core over Kafka.

## Prerequisites

- Go 1.24+
- Kafka broker (shared with Shingo Core)
- WarLink PLC driver (optional, for automated production counting)

## Quick Start

```sh
go build ./...
./shingoedge --config shingoedge.yaml
```

The operator interface is available at `http://localhost:8081`.

### Initial Setup

On first launch with no configuration file, Shingo Edge generates a default `shingoedge.yaml` and starts with sensible defaults. The minimum configuration for a functional deployment is the Kafka broker address and the station identity:

```yaml
namespace: plant-a
line_id: line-1

messaging:
  kafka:
    brokers:
      - 192.168.1.10:9092
```

The station identity is derived as `{namespace}.{line_id}` (e.g., `plant-a.line-1`). This identity is used for all communication with Shingo Core and must be unique per edge instance.

All other settings — PLC connection, web server port, counter thresholds — can be adjusted through the web UI setup page at `/setup`. The YAML file is application-managed and should not be edited by hand during normal operation.

### First Login

On first visit, the login page prompts for a username and password. The credentials entered on first login become the admin account. Subsequent logins authenticate against that account.

## Build and Test

```sh
go build ./...
go test -v ./...
```

## Debug Logging

Enable subsystem-filtered debug output:

```sh
./shingoedge --log-debug=orders,plc
```

Without a filter (`--log-debug`), all subsystems are logged.

| Subsystem | Description |
|-----------|-------------|
| `engine` | Engine lifecycle events |
| `plc` | PLC discovery, tag polling, counter deltas |
| `orders` | Order lifecycle state transitions |
| `changeover` | Changeover state machine transitions |
| `kafka` | Kafka producer and consumer events |
| `edge_handler` | Inbound dispatch message handling |
| `heartbeat` | Registration and heartbeat messaging |
| `outbox` | Outbox drain cycles |
| `reporter` | Production reporting |
| `protocol` | Wire protocol encode and decode |

## Command-Line Flags

| Flag | Description |
|------|-------------|
| `--config PATH` | Path to config file (default: `shingoedge.yaml`) |
| `-port PORT` | HTTP port override |
| `--restore` | Interactive restore from S3-compatible backup storage before startup |
| `--log-debug[=FILTER]` | Enable debug logging with optional subsystem filter |

## Operator Interface

Shingo Edge provides a browser-based interface for production line operators and supervisors.

### Operator Pages

| Page | Route | Description |
|------|-------|-------------|
| Material | `/material` | Current material inventory, stock levels, and consumption state |
| Kanbans | `/kanbans` | Active transport orders with status tracking and delivery confirmation |
| Manual Order | `/manual-order` | Create retrieve, store, and move orders manually |
| Changeover | `/changeover` | Production line changeover workflow |
| Production | `/production` | Hourly production counts and shift reporting |

### Administration Pages

| Page | Route | Description |
|------|-------|-------------|
| Setup | `/setup` | Production lines, job styles, payloads, PLC reporting points |
| Diagnostics | `/diagnostics` | System health, Kafka and PLC connectivity |

See [UI Guide](docs/ui-guide.md) for detailed page descriptions and operator workflows.

## Key Features

### PLC Integration

Shingo Edge integrates with PLCs through the WarLink driver service. WarLink provides an HTTP API that abstracts over PLC protocols (EtherNet/IP, Modbus, etc.). Edge discovers connected PLCs and their available tags automatically.

**Reporting points** bind PLC counter tags to job styles. When a reporting point is active, Edge polls the counter at a configurable interval, calculates production deltas, and decrements UOP remaining on active bins. When UOP remaining drops below the configured threshold, a replacement bin is ordered automatically.

### Order Lifecycle

Orders follow a linear lifecycle on the edge side:

```
queued -> submitted -> acknowledged -> in_transit -> delivered -> confirmed
```

- **queued** — Created locally by operator action or auto-reorder
- **submitted** — Published to Kafka (via outbox)
- **acknowledged** — Core accepted the order and located source material
- **in_transit** — Robot assigned and moving
- **delivered** — Fleet reports delivery complete
- **confirmed** — Operator confirmed physical receipt

Orders can be cancelled from any non-terminal state.

### Changeover

The changeover feature tracks the workflow when switching a production line from one job style to another. The operator advances through a linear sequence of states:

```
running -> stopping -> counting_out -> storing -> delivering -> counting_in -> ready -> running
```

Each state transition is logged with the operator name and timestamp. An in-progress changeover can be cancelled at any intermediate state, returning directly to `running`.

### Auto-Reorder

Each payload can be configured with a reorder threshold. When PLC counters decrement UOP remaining below the threshold, the system automatically creates a retrieve order for a replacement bin. The threshold should allow sufficient time for retrieval and delivery before depletion.

## Documentation

| Document | Description |
|----------|-------------|
| [Backup and Restore](docs/backup-restore.md) | Backup configuration, automatic backup behavior, and dead-machine restore procedure |
| [UI Guide](docs/ui-guide.md) | Operator interface pages and workflows |
| [Wire Protocol](../docs/wire-protocol.md) | Kafka messaging protocol specification |
| [Terminology](../docs/terminology.md) | Domain terms and vendor mapping |

## License

Proprietary. See [LICENSE](../LICENSE).
