# Shingo Edge

Verified against: aa62233b

Shop-floor client for the Shingo material tracking system. Runs at each production line to track PLC counters, manage material inventory, handle operator order workflows, and communicate with Shingo Core over Kafka.

## Prerequisites

- Go 1.25 (the `go` line in `shingo-edge/go.mod`)
- Kafka broker (shared with Shingo Core)
- WarLink PLC driver (optional, for automated production counting)

## Quick Start

From `shingo-edge/`:

```sh
go build -o shingoedge ./cmd/shingoedge
./shingoedge --config shingoedge.yaml
```

The operator interface is available at `http://localhost:8081`. On a station, `install-edge.sh` at the repo
root builds the binary and installs it as the `shingo-edge` systemd service, with its config at
`/etc/shingo/shingoedge.yaml`.

### Initial Setup

[`shingoedge.example.yaml`](shingoedge.example.yaml) lists every key at its shipped default: copy it to
`shingoedge.yaml` and edit. A missing config file is not an error; the Edge starts on `config.Defaults()`. The
one setting an Edge cannot work without is the Kafka broker list:

```yaml
messaging:
  kafka:
    brokers:
      - 192.0.2.10:9092
```

**The station identity is `station_uid`.** It is opaque, it never changes, and it is the value that travels as
`protocol.Address.Station`. An Edge that starts without one mints a random uid (`main.ensureIdentity`,
`config.NewStationUID`) and writes it into the config file at once, so it survives a restart; Core lists the
station as unclaimed on its Stations page until someone names it. If the write fails, the Edge runs and warns
that it will mint a different uid on the next restart.

**Replacement hardware for an existing station takes that station's uid** — read off Core and put in
`station_uid` (or the Station UID field on `/config`) before the box first starts, or brought back by a restore
([Backup and Restore](docs/backup-restore.md)) — so the station's history stays attached. A minted uid is
always a new station.

`namespace` and `line_id` are labels only; nothing derives an identity from them. `messaging.station_id` is an
identity override for a bench Edge that has no `station_uid`.

Every other setting is on the web UI's `/config` page. Saving there rewrites the whole YAML file from the live
settings, so comments and hand formatting do not survive a save.

### First Login

On first visit, the login page prompts for a username and password. The credentials entered on first login become the admin account. Subsequent logins authenticate against that account.

## Build and Test

```sh
go build ./...
go test ./...
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
except `outbox`, `inventory_delta` and `kafka`, pinned by
`TestLogGate_DefaultMutesThePerTickSubsystems`), `[all]` mirrors
everything, `[]` mirrors nothing. The browser log UI (`/diagnostics`) shows every subsystem
either way.

| Subsystem | Description |
|-----------|-------------|
| `edge_handler` | Inbound messages from Core (order acks, waybills, status updates, deliveries) |
| `engine` | Engine lifecycle and engine-side events |
| `heartbeat` | Registration and heartbeat messaging |
| `inventory_delta` | UOP deltas and lineside pile levels flushed to Core |
| `kafka` | Kafka connect, publish and receive |
| `orders` | Order lifecycle state transitions |
| `outbox` | Outbox drain cycles |
| `plant_claims` | The plant-claims publisher |
| `plc` | PLC discovery, tag polling, counter deltas |
| `production_ticks` | Production ticks shipped to Core |
| `protocol` | Wire protocol decode of inbound envelopes |
| `release` | Release decisions |

## Command-Line Flags

| Flag | Description |
|------|-------------|
| `--config PATH` | Path to config file (default: `shingoedge.yaml`) |
| `--port PORT` | HTTP port override |
| `--restore` | Interactive restore from S3-compatible backup storage before startup |
| `--log-debug[=FILTER]` | Enable the debug log file, with an optional subsystem filter |
| `--pprof-port PORT` | Serve the Go profiler on `127.0.0.1:PORT` only (0, the default, is off) |

## Configuration Keys

Every key `shingoedge.yaml` accepts: key and default from `config.Defaults()`
(rendered to `shingo-edge/config/defaults.snapshot.txt` by `make defaults-snapshot` in `shingo-edge/`),
type and description from the field comments in `shingo-edge/config`. `make docs-gen` at the repo root
regenerates it. Durations take Go duration strings (`5s`, `1h0m0s`).
[`shingoedge.example.yaml`](shingoedge.example.yaml) holds the same keys at the same defaults.

<!-- generated: shingo-edge/config Config -->

| Key | Type | Default | Description |
|---|---|---|---|
| `backup.enabled` | bool | `false` | automatic backups on the schedule below |
| `backup.keep_daily` | int | `14` | Retention: how many backups are kept per time bucket (see shingo-edge/docs/backup-restore.md, Retention). |
| `backup.keep_hourly` | int | `48` | Retention: how many backups are kept per time bucket (see shingo-edge/docs/backup-restore.md, Retention). |
| `backup.keep_monthly` | int | `12` | Retention: how many backups are kept per time bucket (see shingo-edge/docs/backup-restore.md, Retention). |
| `backup.keep_weekly` | int | `8` | Retention: how many backups are kept per time bucket (see shingo-edge/docs/backup-restore.md, Retention). |
| `backup.s3.access_key` | string | _(unset)_ | S3 access key |
| `backup.s3.bucket` | string | _(empty)_ | the bucket archives go to |
| `backup.s3.endpoint` | string | _(empty)_ | the S3-compatible endpoint URL |
| `backup.s3.insecure_skip_tls_verify` | bool | `false` | skip the endpoint's certificate check |
| `backup.s3.region` | string | `us-east-1` | the bucket's region |
| `backup.s3.secret_key` | string | _(unset)_ | S3 secret key |
| `backup.s3.use_path_style` | bool | `true` | path-style bucket addressing rather than virtual-hosted |
| `backup.schedule_interval` | duration | `1h0m0s` | the automatic backup period |
| `core_api` | string | _(empty)_ | SimCoreAPI is Core's address for the sim operator's oracle reads (GET /api/nodes, /api/bins/by-node: engine/sim_core_oracle.go), read by sim builds only. The Edge reaches Core by the ask pair and Core's feeds; it has no HTTP client to Core. |
| `counter.jump_threshold` | int64 | `1000` | JumpThreshold is read by plc.CalculateDelta twice: a delta above it is labelled "jump", and a backward read is taken as a 16/32-bit rollover only when the wrap delta is at or under it (otherwise it is a "reset"). The labels describe how the counter moved; every delta is counted. |
| `database_path` | string | `shingoedge.db` | the SQLite database file |
| `demand.hysteresis_percent` | float64 | _(unset)_ | HysteresisPercent is the recovery margin, as a percentage of the claim's reorder_point. A cell episode opens when remaining UOP falls to or below reorder_point and closes only once it climbs back ABOVE reorder_point + margin. |
| `line_id` | string | _(empty)_ | Namespace / LineID are labels, not identity: nothing composes a station id from them, they have no defaults, and the engine only logs them at start. A plant config that carries them still loads. |
| `logging.stderr_subsystems` | list of string | `[edge_handler engine heartbeat orders plant_claims plc production_ticks protocol release]` | StderrSubsystems is the allow-list of debuglog subsystems mirrored to stderr (under systemd, the journal). |
| `messaging.dispatch_topic` | string | `shingo.dispatch` | the topic Core publishes to edges on |
| `messaging.kafka.brokers` | list of string | _(empty)_ | Kafka broker addresses, host:port |
| `messaging.orders_topic` | string | `shingo.orders` | the topic this edge publishes to Core on |
| `messaging.outbox_drain_interval` | duration | `5s` | how often the outbox drainer publishes pending messages |
| `messaging.signing_key` | string | _(unset)_ | optional HMAC-SHA256 shared secret for envelope signing |
| `messaging.station_id` | string | _(empty)_ | an identity override for a bench edge with no station_uid (Config.StationID) |
| `namespace` | string | _(empty)_ | Namespace / LineID are labels, not identity: nothing composes a station id from them, they have no defaults, and the engine only logs them at start. A plant config that carries them still loads. |
| `plant_claims.snapshot_interval` | duration | `1h0m0s` | SnapshotInterval is the safety snapshot's period: the full claims set is republished this often even when no edit triggered a publish, so a Core that missed one converges. It runs only while Core does not quote the digests back on the heartbeat ack; a Core that does is healed per process on the ack instead. <= 0 uses DefaultPlantClaimsSnapshotInterval. |
| `poll_rate` | duration | `1s` | the PLC counter loop's poll period; read at boot |
| `sim.anchor_wall` | timestamp | _(zero)_ | SHARED wall anchor for fast-forward sync: sim-now = epoch + speed×(wallNow−anchor). Set IDENTICALLY in core+edge to the run-start wall time so the two clocks stay in lockstep (no cross-process drift). Zero = per-process boot anchor (drifts). |
| `sim.calendar.enabled` | bool | `false` | default false (backward-compatible) |
| `sim.calendar.shifts` | list | _(empty)_ | ordered by start time; default 3×8h |
| `sim.calendar.shifts[].breaks` | list | — | breaks within this shift |
| `sim.calendar.shifts[].breaks[].end` | string | — | "HH:MM" 24h |
| `sim.calendar.shifts[].breaks[].start` | string | — | "HH:MM" 24h |
| `sim.calendar.shifts[].end` | string | — | "HH:MM" 24h, e.g. "14:00" |
| `sim.calendar.shifts[].start` | string | — | "HH:MM" 24h, e.g. "06:00" |
| `sim.calendar.weekend` | list of weekday | _(empty)_ | days with no production; default [Saturday, Sunday] |
| `sim.enabled` | bool | `false` | the sim switch; needs a sim build and SHINGO_ALLOW_SIM=1 |
| `sim.epoch` | timestamp | _(zero)_ | sim clock start (fast-forward origin). Zero = wall-now |
| `sim.max_speed` | float64 | `0` | effective-speed cap; <=0 → default (clock.DefaultSimMaxSpeed, 10×). Past this the clock outruns the real choreography and the loop wedges, so requests are clamped. Must match core. Set very high to effectively uncap. |
| `sim.operators.changeovers` | list | _(empty)_ | Changeovers are the changeovers the sim operator runs as a person at the station does: when the sim clock reaches At after the operator started, it presses START for Process to ToStyle, then each RELEASE the screen offers, and CUTOVER once the changeover's own gate says can_complete with no blockers. Read by simOperator.driveChangeovers. Empty: none. |
| `sim.operators.changeovers[].at` | duration | — | sim time after the sim operator starts |
| `sim.operators.changeovers[].process` | string | — | the process, by name |
| `sim.operators.changeovers[].to_style` | string | — | the style to change it to, by name |
| `sim.operators.enabled` | bool | `false` | the sim operator's switch |
| `sim.operators.lineside_pull_percent` | int | `0` | LinesidePullPercent is how much of a pack the consume operator pulls into the lineside bucket to release a bin empty: at a release with the tracked count at or under this percent of the pack (rounded up), the remainder is captured to the pile and the bin goes empty; above it, a count release. 0 = never (a count release always). Read by simOperator.consumeDisposition. |
| `sim.operators.loader_auto_load` | duration | `5s` | default 5s |
| `sim.operators.pair_release` | bool | `false` | PairRelease routes a staged swap through ReleaseStagedOrders — the operator's RELEASE button — instead of releasing each leg on its own. |
| `sim.operators.swap_release` | duration | `3s` | SwapRelease is the simulated reaction time between a swap reaching its wait (status "staged") and the operator pushing Release. Default 3s. |
| `sim.operators.unloader_auto_clear` | duration | `8s` | default 8s |
| `sim.operators.unloader_taps` | list | _(empty)_ | UnloaderTaps says what the operator taps at an unloader's windows, per operator station. A station not listed keeps the plain CLEAR, and leaves an empty cart standing. |
| `sim.operators.unloader_taps[].station` | string | — | an operator station's code |
| `sim.operators.unloader_taps[].tap` | string | — | SimTapLit or SimTapEmpty |
| `sim.processes` | list | _(empty)_ | the fake PLC counters the fake WarLink advances |
| `sim.processes[].identity_tag` | string | — | "" = no program change |
| `sim.processes[].line_depth` | int | — | loads between station and counter; 0 = none |
| `sim.processes[].plc_name` | string | — | the PLC the counter is on |
| `sim.processes[].station_tag` | string | — | "" = no station counter |
| `sim.processes[].tag_name` | string | — | the counter tag the Edge's process counts |
| `sim.processes[].tick_interval` | duration | — | default 3s (applied at consumption) |
| `sim.processes[].uop_per_tick` | int64 | — | default 1 (applied at consumption) |
| `sim.seed` | int64 | `0` | PRNG seed; 0 = derive from time and log it |
| `sim.speed` | float64 | `0` | time multiplier: 2.0 = twice as fast. Default 1.0 |
| `station_uid` | string | _(empty)_ | StationUID is this edge's identity. It is opaque, it never changes, and it is the value that travels as protocol.Address.Station. Empty on a first start: main.ensureIdentity mints a random uid and persists it, and Core holds the station unclaimed until a human names it. |
| `timezone` | string | _(empty)_ | IANA timezone for shift/hourly bucketing (e.g. "America/Chicago") |
| `warlink.enabled` | bool | `true` | false: the WarLink poller does not run |
| `warlink.host` | string | `localhost` | the WarLink service's host |
| `warlink.mode` | string | `sse` | "sse" (default) or "poll" |
| `warlink.poll_rate` | duration | `2s` | the WarLink poll period (<= 0: 2s) |
| `warlink.port` | int | `8080` | the WarLink service's HTTP port |
| `web.auto_confirm` | bool | `false` | AutoConfirm makes the orders this station mints confirm their own delivery: OR'ed with each claim's own auto_confirm wherever the Edge mints an order. |
| `web.host` | string | `0.0.0.0` | web server listen address |
| `web.port` | int | `8081` | web server port |
| `web.session_secret` | string | _(set; redacted)_ | SessionSecret signs the login session cookie. An unset one is generated at random (Defaults, Load), and the next Config.Save writes it down. |

<!-- /generated -->

## Operator Interface

Shingo Edge provides a browser-based interface for production line operators and supervisors.

### Operator Pages

<!-- generated: shingo-edge/www/router.go pages public -->

| Path | Template (in `shingo-edge/www/templates/`) | Title | Auth |
|---|---|---|---|
| `/` | `production.html` | Production | public |
| `/changeover` | `changeover.html` | Changeover | public |
| `/containment` | `containment.html` | Quality Containment | public |
| `/login` | `login.html` | Login | public |
| `/operator/station/{id}` | `operator-display.html` |  | public |
| `/orders` | `orders.html` | Orders | public |
| `/production` | `production.html` | Production | public |

<!-- /generated -->

### Administration Pages

<!-- generated: shingo-edge/www/router.go pages login -->

| Path | Template (in `shingo-edge/www/templates/`) | Title | Auth |
|---|---|---|---|
| `/config` | `config.html` | Configuration | login |
| `/diagnostics` | `diagnostics.html` | Logs | login |
| `/manual-message` | `manual-message.html` | Kafka Direct | login |
| `/manual-order` | `manual-order.html` | Manual Order | login |
| `/processes` | `processes.html` | Processes | login |
| `/replenishment` | `replenishment.html` | Replenishment | login |

<!-- /generated -->

The operator station display, `/operator/station/{id}`, is the touch screen at each operator station.
See [UI Guide](docs/ui-guide.md) for what each page shows and the operator workflows.

## Key Features

### PLC Integration

Shingo Edge reads PLC tags through the WarLink service (`warlink.*`), which either pushes each tag change or is
polled on a timer (`warlink.mode`). Each process names one production counter, a PLC and a counter tag, in its
Settings on `/processes`; the Edge keeps the reporting point that binds that tag to the process's active style.
A counter tick draws down the remaining UOP of a consume node's bin of the running part and fills a produce
node's bin (`(*Engine).handleCounterDelta`). A tick decides nothing about replenishment.

### Orders

The Edge mints orders for its nodes (operator requests, releases, changeover legs, auto-reorder) and sends them
to Core through its outbox; Core's status pushes move them on. Statuses, transitions and what each side does at
each are in [docs/order-lifecycle.md](../docs/order-lifecycle.md); the Orders page's actions are in the
[UI Guide](docs/ui-guide.md).

### Changeover

A changeover switches a process from one style to another. Its row is `active` until it moves once, to
`completed` or `cancelled`; the work is per node, on the node tasks it creates, each walking its own ladder
(`domain.NodeTaskState`). Cutover is gated on every node task and every order those tasks reference being
terminal (`(*Engine).canCompleteChangeover`). The [UI Guide](docs/ui-guide.md) has the states and what the
operator sees at each.

### Auto-Reorder

Each claim (a node's place in a style) carries a reorder point and an auto-reorder switch, set on
`/replenishment`. Once a minute the level sweep (`(*Engine).sweepCellLevels`) reads each claim's remaining UOP
against its level (`protocol.ClaimDemandLevel`): a consume claim breaches at or below its reorder point, a
produce claim at or above its level. For a breached claim with auto-reorder on and nothing already coming to
the node, the sweep asks for the bin. A consume claim with a reorder point of 0 never orders. The mechanism is
[uop-threshold-replenishment.md](../docs/uop-threshold-replenishment.md).

## Documentation

| Document | Description |
|----------|-------------|
| [Backup and Restore](docs/backup-restore.md) | Backup configuration, automatic backup behavior, and dead-machine restore procedure |
| [UI Guide](docs/ui-guide.md) | Operator interface pages and workflows |
| [shingoedge.example.yaml](shingoedge.example.yaml) | Every config key at its shipped default |
| [Order Lifecycle](../docs/order-lifecycle.md) | Order statuses and transitions |
| [Wire Protocol](../docs/wire-protocol.md) | Kafka messaging protocol specification |
| [Terminology](../docs/terminology.md) | Domain terms and vendor mapping |

## License

Proprietary. See [LICENSE](../LICENSE).
