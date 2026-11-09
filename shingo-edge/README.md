# Shingo Edge

Verified against: b9210bce

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
except `outbox`, `inventory_delta` and `kafka`), `[all]` mirrors
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
| `protocol` | Wire protocol encode and decode |

## Command-Line Flags

| Flag | Description |
|------|-------------|
| `--config PATH` | Path to config file (default: `shingoedge.yaml`) |
| `-port PORT` | HTTP port override |
| `--restore` | Interactive restore from S3-compatible backup storage before startup |
| `--log-debug[=FILTER]` | Enable debug logging with optional subsystem filter |

## Configuration Keys

Every key `shingoedge.yaml` accepts: key and default from `config.Defaults()`
(rendered to `shingo-edge/config/defaults.snapshot.txt` by `make defaults-snapshot`),
type and description from the field comments in `shingo-edge/config`. `make docs-gen`
regenerates it. Durations take Go duration strings (`5s`, `1h0m0s`).

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
| `counter.jump_threshold` | int64 | `1000` | JumpThreshold is read by plc.CalculateDelta twice: a delta above it is labelled "jump", and a backward read is taken as a 16/32-bit rollover only when the wrap delta is at or under it (otherwise it is a "reset"). Neither label gates the count any more. |
| `database_path` | string | `shingoedge.db` | the SQLite database file |
| `demand.hysteresis_percent` | float64 | _(unset)_ | HysteresisPercent is the recovery margin, as a percentage of the claim's reorder_point. A cell episode opens when remaining UOP falls to or below reorder_point and closes only once it climbs back ABOVE reorder_point + margin. |
| `line_id` | string | _(empty)_ | Namespace / LineID are LABELS NOW, not identity. They no longer compose a station id and they no longer have defaults; they survive because existing plant configs carry them and silently dropping fields a plant has written is its own kind of surprise. |
| `logging.stderr_subsystems` | list of string | `[edge_handler engine heartbeat orders plant_claims plc production_ticks protocol release]` | StderrSubsystems is the allow-list of debuglog subsystems mirrored to stderr (under systemd, the journal). |
| `messaging.dispatch_topic` | string | `shingo.dispatch` | the topic Core publishes to edges on |
| `messaging.kafka.brokers` | list of string | _(empty)_ | Kafka broker addresses, host:port |
| `messaging.orders_topic` | string | `shingo.orders` | the topic this edge publishes to Core on |
| `messaging.outbox_drain_interval` | duration | `5s` | how often the outbox drainer publishes pending messages |
| `messaging.signing_key` | string | _(unset)_ | optional HMAC-SHA256 shared secret for envelope signing |
| `messaging.station_id` | string | _(empty)_ | an identity override for a bench edge with no station_uid (Config.StationID) |
| `namespace` | string | _(empty)_ | Namespace / LineID are LABELS NOW, not identity. They no longer compose a station id and they no longer have defaults; they survive because existing plant configs carry them and silently dropping fields a plant has written is its own kind of surprise. |
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
| `sim.max_speed` | float64 | `0` | effective-speed cap; <=0 → default (10×, the MEASURED ceiling — see clock.DefaultSimMaxSpeed). Past this the clock outruns the real choreography and the loop wedges, so requests are clamped. Must match core. Set very high to effectively uncap. |
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
| `station_uid` | string | _(empty)_ | StationUID is this edge's identity. It is opaque, it never changes, and it is the value that travels as protocol.Address.Station. |
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

While a changeover is active the owning process's `target_style_id` differs from its `active_style_id`; cutover and cancel both clear it. The screens derive their state words from those two pointers ("Changing over", "Running `<style>`", "No part running"); no state column is stored.

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
