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

**The station identity is `station_uid`, and Core mints it at enrollment** (`config/config.go:17-36`). It is opaque, it never changes, and it is the value that travels as `protocol.Address.Station`. An operator copies it into the YAML by hand, once — that step is what distinguishes a new station (enroll, take a fresh uid) from replacement hardware for an existing one (do not enroll; copy the existing uid onto the new box and the station's history stays attached). Empty means unenrolled, and startup **refuses** rather than deriving one.

It used to be composed as `{namespace}.{line_id}` (e.g. `plant-a.line-1`). Since v66 those two are **labels only** — no defaults, no role in identity (`config/config.go:38-43`).

All other settings — PLC connection, web server port, counter thresholds — are adjusted through the web UI. The YAML file is application-managed and should not be edited by hand during normal operation.

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

`--log-debug` controls the optional **file** (`shingo-debug.log`) only. What
reaches **stderr**, and so journald under systemd, is `logging.stderr_subsystems`
in the YAML, with Core's semantics: absent is the default list (every subsystem
except `outbox`, `inventory_delta`, `kafka` and `reporter`), `[all]` mirrors
everything, `[]` mirrors nothing. The browser log UI shows every subsystem
either way.

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
| Orders | `/orders` | Active transport orders with status tracking and delivery confirmation |
| Manual Order | `/manual-order` | Create retrieve, store, and move orders manually |
| Changeover | `/changeover` | Production line changeover workflow |
| Production | `/production` | Hourly production counts and shift reporting |

### Administration Pages

| Page | Route | Description |
|------|-------|-------------|
| Processes | `/processes` | Production lines |
| Styles | `/styles` | Job styles and their node claims |
| Payload catalog | `/payload-catalog` | Payload templates for this station |
| Reporting points | `/reporting-points` | PLC counter tags bound to job styles |
| PLCs | `/plcs` | Discovered PLCs, WarLink connection, tag reads |
| Shifts | `/shifts` | Shift windows for production bucketing |
| Operator stations | `/operator-stations` | Station definitions and node assignment |
| Config | `/config` | App config, Kafka, and **Backups** |
| Diagnostics | `/diagnostics` | System health, Kafka and PLC connectivity |

There is no `/setup` page — configuration is these separate pages.

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

The changeover feature tracks the workflow when switching a production line from one job style to another. The changeover row itself has three states and moves exactly once:

```
active -> completed   (Complete Cutover, or the auto-completion path)
active -> cancelled   (Cancel Changeover)
```

Both are terminal (`domain/changeover_state.go:19-21`). The row is inserted `active` (`service/changeover_service.go:61`); nothing advances it step by step.

The sequencing lives one level down, on the **node tasks** the changeover creates — one per node that has to change. Each walks its own ladder, driven by the operator and by order events:

```
swap_required -> staging_requested -> staged -> empty_requested
              -> line_cleared -> release_requested -> released
```

with `unchanged` for a node that needs no work, `switched` for an operator skip, and the off-ladder dispositions `error`, `capacity_blocked`, `awaiting_material`, `abandoned` and `cancelled` (`domain/changeover_node_state.go:19-71`). Cutover is gated on every node task reaching a terminal state *and* every order those tasks reference reaching a terminal status (`engine/operator_changeover_cutover.go:29-36`); there is no override.

While a changeover is active the owning process sits in `production_state = 'changeover_active'`, returning to `active_production` on either cutover or cancel.

See [UI Guide](docs/ui-guide.md) for what the operator sees at each node-task state.

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
