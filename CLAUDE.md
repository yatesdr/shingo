# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Shingo (信号) — material tracking and automated transport system for manufacturing plants. Manages payload flow between warehouse storage and production line stations using autonomous mobile robots. Vendor-agnostic (currently integrates with Seer RDS fleet).

Monorepo with 3 independent Go 1.24 modules:
- **shingo-core** (`shingocore`) — central server: dispatch, fleet management, state tracking
- **shingo-edge** (`shingoedge`) — shop-floor client: PLC integration, material tracking, operator workflows
- **protocol** (`shingo/protocol`) — shared wire protocol: JSON envelope, message types, two-phase decode

## Build & Test Commands

Each module builds independently from its own directory:

```sh
# Core
cd shingo-core
make build          # build for current platform
make test           # go test -v ./...
make fmt            # go fmt ./...
make vet            # go vet ./...
make all            # cross-compile linux/windows/macos

# Edge (no Makefile)
cd shingo-edge
go build ./...
go test -v ./...

# Protocol
cd protocol
go test -v ./...
```

Run a single test:
```sh
cd shingo-core && go test -v ./dispatch -run TestHandleOrderRequest -count=1
```

Debug logging (subsystem-filtered):
```sh
go run ./cmd/shingocore --log-debug=rds,dispatch
go run ./cmd/shingoedge --log-debug=orders,plc
```

CI: GitHub Actions runs build + test per module on push (`.github/workflows/{core,edge,protocol}.yml`).

## Architecture

### Module Layout

Core packages: `cmd/`, `config/`, `engine/`, `dispatch/`, `messaging/`, `rds/`, `fleet/`, `store/`, `nodestate/`, `www/`
Edge packages: `cmd/`, `config/`, `engine/`, `orders/`, `plc/`, `changeover/`, `messaging/`, `store/`, `www/`

### Key Patterns

- **EventBus** — sync pub/sub within each component for inter-subsystem decoupling. Wired in `engine/engine.go` during `Start()`.
- **Adapter/Emitter interfaces** — each package defines its own emitter interface locally (e.g., `dispatch.Emitter`, `plc.Emitter`) to avoid import cycles. Engine wiring bridges these.
- **Outbox pattern** — messages written to an `outbox` DB table first, then drained to Kafka periodically. Ensures at-least-once delivery even when Kafka is unavailable.
- **Two-phase protocol decode** — `protocol.Ingestor` parses routing fields first (for filtering/expiry), then deserializes the full payload only if the message passes.
- **Dialect abstraction (core only)** — `store.DB` uses `Q()` with `?` placeholders + `Rebind()` to support both SQLite and Postgres.
- **Fleet backend interface** — `fleet.Backend` abstracts the robot fleet system. Current implementation: Seer RDS (`rds/` package).

### Messaging

Kafka with dual topics: `shingo.orders` (edge→core), `shingo.dispatch` (core→edge). See `docs/wire-protocol.md` for the protocol spec.

### Testing Patterns

- Temp SQLite DBs via `t.TempDir()` for store tests
- Mock emitter/backend interfaces (see `dispatch/dispatcher_test.go`, `plc/` tests)
- `fleet.Backend` interface enables fleet mocking without RDS dependency

## Domain Terms

- **Node** — physical floor location (storage, line-side, staging, lane slot)
- **Bin** — physical container tracked at a node (`store.Bin`)
- **BinType** — container class (size, form factor)
- **Blueprint** — template defining bin contents and UOP capacity (`store.Blueprint`)
- **Payload** — record linking a blueprint to a bin; tracks manifest confirmation and UOP (`store.Payload`)
- **Manifest** — list of parts in a payload; **blueprint_manifest** = template, **manifest_items** = actual
- **Order** — transport request (retrieve/store/move): edge→core→fleet
- **Station** — an edge instance identity (`{namespace}.{line_id}`)
- **Process** — production area (DB: `production_lines`)
- **Style** — end-item type (DB: `job_styles`)

## Conventions

- Config is YAML (`shingocore.yaml` / `shingoedge.yaml`), never hardcode defaults in code
- Database migrations run on startup via `store.migrate()`
- Static assets and templates use `go:embed`
- Auth: gorilla/sessions + bcrypt
- Protocol TTL (`exp` field) is absolute UTC timestamp, not relative duration
- Never include AI co-author references in commits
