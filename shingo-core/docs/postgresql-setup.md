# PostgreSQL Setup

Verified against: 4f4870c3

This guide covers setting up PostgreSQL for ShinGo Core, from server installation through connection configuration.

## Installing PostgreSQL

### Ubuntu / Debian

```sh
sudo apt update
sudo apt install postgresql postgresql-contrib
sudo systemctl enable postgresql
sudo systemctl start postgresql
```

### RHEL / Rocky / AlmaLinux

```sh
sudo dnf install postgresql-server postgresql-contrib
sudo postgresql-setup --initdb
sudo systemctl enable postgresql
sudo systemctl start postgresql
```

### macOS (Homebrew)

```sh
brew install postgresql@16
brew services start postgresql@16
```

### Windows

Download and run the installer from [postgresql.org/download/windows](https://www.postgresql.org/download/windows/). The installer includes pgAdmin and sets up the service automatically.

## Database and User Setup

One database and one user are required. No tables or grants beyond ownership are needed — ShinGo Core creates all tables, indexes, and seed data automatically on first startup.

Connect as the PostgreSQL superuser:

```sh
sudo -u postgres psql
```

```sql
-- 1. Create the application user
CREATE USER shingocore WITH PASSWORD 'your-secure-password';

-- 2. Create the database, owned by that user
CREATE DATABASE shingocore OWNER shingocore;
```

Because `shingocore` is the database **owner**, it has full privileges to create tables, indexes, and sequences. No additional `GRANT` statements are needed.

### What happens on first startup

When ShinGo Core connects to an empty database, it:

1. Applies the baseline, `store/schema/baseline.sql`, in one transaction: every table, index and constraint, the
   seed rows (the synthetic node types `LANE` and `NGRP`, the `_TRANSIT` node, the default load sequence) and
   `schema_migrations` v1 to v184. It lands whole or not at all, never in part: if Core stops part-way, the
   database server still finishes it, and the next start boots the database it made; if the database server stops
   part-way, nothing is left and the next start applies it again.
2. Runs the migrations above v184 in `store/migrations.go`.
3. Creates a default admin user (`admin` / `admin`) on a fresh install — `ensureDefaultAdmin` in `www/auth.go`. Idempotent, and it fires from the web layer rather than the migration. **Change this password before the station goes live.**

On subsequent startups only the migrations above v184 that the database lacks run. Core refuses to start on a
database missing any of v1 to v184 (an install older than v0.17.1: start v0.17.1 on it once first), and on tables,
sequences or functions in schema `public` restored without their `schema_migrations` rows (restore the whole dump).
An extension's objects in `public` do not count.

### Why ownership matters

The application user must be the database owner (not just granted `CONNECT` or `ALL PRIVILEGES`). The migration system creates and alters tables, drops legacy tables, and queries `information_schema` — operations that require ownership or superuser access. Using `OWNER shingocore` in the `CREATE DATABASE` statement is the simplest and most secure approach.

## Configuring pg_hba.conf

PostgreSQL controls client authentication through `pg_hba.conf`. You need to allow connections from the host running ShinGo Core.

### Finding pg_hba.conf

```sh
sudo -u postgres psql -c "SHOW hba_file;"
```

Common locations:
- Ubuntu/Debian: `/etc/postgresql/16/main/pg_hba.conf`
- RHEL/Rocky: `/var/lib/pgsql/data/pg_hba.conf`
- macOS (Homebrew): `/opt/homebrew/var/postgresql@16/pg_hba.conf`

### Authentication Rules

Add a line for the ShinGo Core host. The format is:

```
# TYPE  DATABASE    USER         ADDRESS          METHOD
```

**Local connections** (ShinGo Core on the same machine):

```
host    shingocore   shingocore   127.0.0.1/32     md5
host    shingocore   shingocore   ::1/128          md5
```

**Remote connections** (ShinGo Core on a different machine):

```
# Single host
host    shingocore   shingocore   192.0.2.50/32    md5

# Entire subnet
host    shingocore   shingocore   192.0.2.0/24     md5
```

**Using scram-sha-256** (PostgreSQL 14+ recommended):

```
host    shingocore   shingocore   192.0.2.0/24     scram-sha-256
```

After editing, reload PostgreSQL:

```sh
sudo systemctl reload postgresql
```

### Allowing Remote Connections

By default, PostgreSQL only listens on `localhost`. To accept remote connections, edit `postgresql.conf`:

```sh
sudo -u postgres psql -c "SHOW config_file;"
```

Set `listen_addresses`:

```
listen_addresses = '*'          # All interfaces
# or
listen_addresses = '192.0.2.10'     # Specific interface
```

Then restart PostgreSQL:

```sh
sudo systemctl restart postgresql
```

## ShinGo Core Configuration

In your `shingocore.yaml`:

```yaml
database:
  postgres:
    host: 192.0.2.10
    port: 5432
    database: shingocore
    user: shingocore
    password: your-secure-password
    sslmode: disable
```

The block also takes the connection-pool limits `max_open_conns`,
`max_idle_conns` and `conn_max_lifetime`; zero (the default) means the store's
own fallback of 25 open, 10 idle, a 5-minute lifetime. They, like every
database setting, apply after a restart. Their defaults and the rest of the
keys are in [Configuration](configuration.md).

### SSL Modes

| Mode | Description |
|------|-------------|
| `disable` | No SSL. Use on trusted networks (factory LAN). |
| `require` | Encrypt the connection but don't verify the server certificate. |
| `verify-ca` | Encrypt and verify the server certificate against a CA. |
| `verify-full` | Encrypt, verify CA, and verify the server hostname matches the certificate. |

For factory LAN deployments, `disable` is typical. For connections over untrusted networks, use `require` or higher.

## Testing the Connection

Verify connectivity before starting ShinGo Core:

```sh
psql -h 192.0.2.10 -U shingocore -d shingocore
```

If this connects successfully, ShinGo Core will be able to connect with the same parameters.

## Backup

### pg_dump (logical backup)

```sh
pg_dump -h localhost -U shingocore shingocore > shingocore_backup.sql
```

### Restore

```sh
psql -h localhost -U shingocore -d shingocore -v ON_ERROR_STOP=1 --single-transaction -f shingocore_backup.sql || {
  echo "restore FAILED on PostgreSQL $(psql -h localhost -U shingocore -d shingocore -Atc 'SHOW server_version'): nothing was restored, the reason is above" >&2
  false
}
```

A custom-format backup (`pg_dump -Fc`) restores the same way, all or nothing:

```sh
pg_restore -h localhost -U shingocore -d shingocore --exit-on-error --single-transaction shingocore_backup.dump || {
  echo "restore FAILED on PostgreSQL $(psql -h localhost -U shingocore -d shingocore -Atc 'SHOW server_version'): nothing was restored, the reason is above" >&2
  false
}
```

Don't drop the stop options. Without them `psql` exits 0 whatever failed, and `pg_restore` skips each table it
cannot create and keeps the rest, `schema_migrations` with it.

**`unrecognized parameter "pg_attver"`.** PostgreSQL's 2026-11-12 minors (14.25, 15.20, 16.16, 17.12, 18.7) record
that column option, and a `pg_dump` older than those minors writes it into the dump. A server older than them
rejects it. Restore onto one of those minors or a later one of the same branch: check with `SHOW server_version`.

Restore into an empty database, and restore the whole dump: Core refuses to start on tables, sequences or functions
that arrived without their `schema_migrations` rows, because nothing can then tell what shape they are.

**A dump from an older release.** This release carries no migration below v184; a database is born on the baseline
or already holds v1 to v184. A dump taken from a Core older than v0.17.1 (a `schema_migrations` that stops below
v184, or has a hole) is refused at start, naming the missing versions, and nothing in it is changed. To bring one
up:

1. Restore it whole into an empty database.
2. Start v0.17.1 on it once (`install-core.sh --tag v0.17.1`, or that release's binary by hand). It applies the
   versions this release no longer carries.
3. Then install this release, which applies the migrations above v184.

`shingocore migrate -config /etc/shingo/shingocore.yaml check` says, without changing anything, which of these a
database needs: `database v<cur> -> v<target>` when this build may be installed on it, or the refusal: the release to
start first, or the tables a restore skipped.

