# PostgreSQL Setup

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

1. Creates all tables (`CREATE TABLE IF NOT EXISTS`)
2. Creates indexes
3. Seeds default node types (STG, LSL, SUP, OFL, STN, CHG)
4. Creates a default admin user (`admin` / `admin`)

On subsequent startups, migrations are idempotent and safe to re-run.

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
host    shingocore   shingocore   192.168.1.50/32  md5

# Entire subnet
host    shingocore   shingocore   192.168.1.0/24   md5
```

**Using scram-sha-256** (PostgreSQL 14+ recommended):

```
host    shingocore   shingocore   192.168.1.0/24   scram-sha-256
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
listen_addresses = '192.168.1.10'  # Specific interface
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
    host: 192.168.1.10
    port: 5432
    database: shingocore
    user: shingocore
    password: your-secure-password
    sslmode: disable
```

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
psql -h 192.168.1.10 -U shingocore -d shingocore
```

If this connects successfully, ShinGo Core will be able to connect with the same parameters.

## Backup

### pg_dump (logical backup)

```sh
pg_dump -h localhost -U shingocore shingocore > shingocore_backup.sql
```

### Restore

```sh
psql -h localhost -U shingocore shingocore < shingocore_backup.sql
```

