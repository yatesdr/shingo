# Configuration Reference

ShinGo Core stores its configuration in a YAML file (default: `shingocore.yaml`). A default config is generated automatically on first run.

> The YAML file is application-managed. Use the web UI config page (`/config`) to change runtime settings. The database connection must be set before first run — see [Initial Setup](#initial-setup). The YAML format is subject to change between versions.

## Initial Setup

The database connection is the only setting required before first launch. Create a minimal `shingocore.yaml`:

```yaml
database:
  driver: postgres
  postgres:
    host: 192.168.1.10
    port: 5432
    database: shingocore
    user: shingocore
    password: your-password
    sslmode: disable
```

All other settings have sensible defaults and can be adjusted through the web UI after startup. See [PostgreSQL Setup](postgresql-setup.md) for server-side database configuration.

For SQLite (development/testing only):

```yaml
database:
  driver: sqlite
  sqlite:
    path: shingocore.db
```

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

## Runtime Configuration

Fleet (RDS) and messaging (Kafka) settings can be changed at runtime through the web UI config page (`/config`). Changes are saved to the YAML file and the affected subsystem is hot-reloaded without restart.

## Config File Reference

The tables below document the current YAML structure for reference. This format may change between versions.

### database

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `driver` | string | `sqlite` | Database backend: `postgres` or `sqlite` |

#### database.postgres

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `host` | string | `localhost` | PostgreSQL server hostname or IP |
| `port` | int | `5432` | PostgreSQL server port |
| `database` | string | `shingocore` | Database name |
| `user` | string | `shingocore` | Database user |
| `password` | string | _(empty)_ | Database password |
| `sslmode` | string | `disable` | SSL mode: `disable`, `require`, `verify-ca`, `verify-full` |

#### database.sqlite

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `path` | string | `shingocore.db` | Path to SQLite database file |

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

### Duration Format

Duration fields accept Go duration strings: `5s`, `10s`, `1m`, `500ms`, `2m30s`.
