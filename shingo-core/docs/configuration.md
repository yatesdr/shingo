# Configuration Reference

ShinGo Core stores its configuration in a YAML file (default: `shingocore.yaml`). A default config is generated automatically on first run.

> **The YAML file is application-managed and should not be edited by hand during normal operation.** Use the web UI config page (`/config`) to change runtime settings. The only exception is the initial database connection, which must be set before first launch — see [Initial Setup](#initial-setup). The YAML format is subject to change between versions.

## Initial Setup

The database connection is the only setting required before first launch. Create a minimal `shingocore.yaml`:

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

All other settings have sensible defaults and can be adjusted through the web UI after startup. See [PostgreSQL Setup](postgresql-setup.md) for server-side database configuration.

## Command-Line Flags

| Flag | Description |
|------|-------------|
| `--config PATH` | Path to config file (default: `shingocore.yaml`) |
| `--version` | Print version and exit |
| `--help` | Print usage and exit |
| `--log-debug[=FILTER]` | Enable debug logging. Optional comma-separated filter of subsystems. |

### Debug Subsystems

Use `--log-debug=subsystem1,subsystem2` to filter debug output:

| Subsystem | What it logs |
|-----------|-------------|
| `rds` | RDS API requests and responses |
| `kafka` | Kafka producer/consumer events |
| `dispatch` | Order dispatch decisions and routing |
| `protocol` | Wire protocol encode/decode |
| `outbox` | Outbox drain cycles |
| `core_handler` | Inbound message handling |
| `nodestate` | Node state cache operations |
| `engine` | Engine lifecycle events |

Without a filter (`--log-debug`), all subsystems are logged.

`--log-debug` controls the optional **file** (`shingo-debug.log`) only. What
reaches **stderr** — and therefore journald under systemd — is controlled by
`logging.stderr_subsystems` in the YAML; see [logging](#logging) below. The
in-memory ring buffer behind the browser log UI (`/logs`) is never filtered by
either: every subsystem always lands there.

## Runtime Configuration

Fleet (RDS) and messaging (Kafka) settings can be changed at runtime through the web UI config page (`/config`). Changes are saved to the YAML file and the affected subsystem is hot-reloaded without restart.

## Config File Reference

The tables below document the current YAML structure for reference. This format may change between versions.

### database

| Field | Type | Default | Description |
|-------|------|---------|-------------|
#### database.postgres

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `host` | string | `localhost` | PostgreSQL server hostname or IP |
| `port` | int | `5432` | PostgreSQL server port |
| `database` | string | `shingocore` | Database name |
| `user` | string | `shingocore` | Database user |
| `password` | string | _(empty)_ | Database password |
| `sslmode` | string | `disable` | SSL mode: `disable`, `require`, `verify-ca`, `verify-full` |

### rds

Fleet backend (Seer RDS) connection settings. Configurable via web UI.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `base_url` | string | `http://192.168.1.100:8088` | RDS API base URL |
| `poll_interval` | duration | `5s` | How often to poll RDS for order status changes |
| `timeout` | duration | `10s` | HTTP request timeout for RDS API calls |

### web

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `host` | string | `0.0.0.0` | Web server listen address |
| `port` | int | `8083` | Web server port |
| `session_secret` | string | _(auto-generated)_ | Cookie signing key |

### messaging

Configurable via web UI.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `kafka.brokers` | string[] | `["localhost:9092"]` | Kafka broker addresses |
| `kafka.group_id` | string | `shingocore` | Kafka consumer group ID |
| `orders_topic` | string | `shingo.orders` | Kafka topic for edge-to-core messages |
| `dispatch_topic` | string | `shingo.dispatch` | Kafka topic for core-to-edge messages |
| `outbox_drain_interval` | duration | `5s` | How often to drain the outbox to Kafka |
| `station_id` | string | `core` | This core instance's station identifier |

### logging

Gates which debuglog subsystems are mirrored to stderr. Under systemd that is
journald, so this is the journal-volume knob.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `stderr_subsystems` | string[] | `[dispatch, engine, core_handler, kafka, outbox, protocol]` | Allow-list of subsystems mirrored to stderr |

```yaml
logging:
    stderr_subsystems: [dispatch, engine, core_handler, kafka, outbox, protocol]
```

Semantics:

| Value | Effect |
|---|---|
| _(key absent)_ | The default list above — everything except `rds` |
| `[all]` | Mirror every subsystem (the incident escape hatch) |
| `[]` or `null` | Mirror nothing |
| `[a, b]` | Mirror exactly those |

Notes:

- **It is an allow-list, not a mute-list.** A subsystem added to the code later
  stays out of the journal until someone opts it in. Core logs the effective
  list at boot (`shingocore: debug log mirroring to stderr: …`) so the omission
  is visible rather than silent.
- **Muting is not disabling.** `rds` is off the default list because it was
  125,817 lines/day at Springfield, against a journal whose retention had
  collapsed to ~15 days. The poller still runs unchanged; only its tracing is
  out of the journal.
- **Nothing is lost to the UI.** The ring buffer behind `/logs` is unfiltered.
- Retention itself is a separate, host-wide decision — see
  `shingo-core/deploy/README.md`.

### dispatch.futility

The rate-per-tuple futility detector: a net for the class of failure where the
planner turns a bounded physical condition into unbounded orchestration work.
Springfield 2026-07-21 produced 484 doomed swaps in under two hours, none of
which reached a robot, while every surface stayed green.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `threshold` | int | `20` | Futile terminals on one (station, process_node, payload) inside the window |
| `window` | duration | `60m` | Rolling window the count is taken over |
| `alert_throttle` | duration | `15m` | Repeat suppression per tuple |

```yaml
dispatch:
    futility:
        threshold: 20
        window: 60m
        alert_throttle: 15m
```

**There is no `enabled` key.** The detector records at every site, always. It
carried an `enabled` flag defaulting to `false` until 2026-09-01, which meant
it recorded at no site at all — opting in requires knowing the detector is
there. A `threshold` or `window` of zero describes nothing countable and
disables it by being malformed; that is not a supported way to turn it off.

A "futile" terminal is an order that reached a terminal status without ever
having reached `in_transit` — planned, then abandoned before a robot moved.
Any order for the same tuple reaching `in_transit` resets the count.

**Rate, not a run count.** A consecutive-run threshold is refuted by the
plant's own history: over 120 days on real tuples, off the incident window,
normal operation produced runs of 5 (×6), 6 (×2), 8, 9 (×3) and one of 26 —
a power-law tail with no knee. Time separates them cleanly: the worst
legitimate case ran ~4/h, the cascade ~242/h.

**Absolute, not learned.** A 30-day trailing baseline would have been trained
on the incident, and the database has a 2.5-week hole (2026-06-27 → 07-15)
that mis-baselines anything computed across it.

**Observe-only.** One log line and one `audit_log` row per trigger. No chip,
no alert, no brake — a brake on an unmeasured threshold stops real work. The
threshold above is a starting guess, and it stays observe-only until the
records say where a real one belongs.

### Duration Format

Duration fields accept Go duration strings: `5s`, `10s`, `1m`, `500ms`, `2m30s`.
