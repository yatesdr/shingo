# Configuration Reference

Verified against: 74c3eb77

ShinGo Core reads its configuration from a YAML file (default: `shingocore.yaml`). **No config file is written on first run.** If the file is absent, `config.Load` returns `config.Defaults()` and creates nothing; the file is written only by a save on the `/config` page (`PUT /api/config`, `apiConfigSave` in `www/handlers_config.go`), which is the one place Core writes it.

A save marshals the whole live config (`(*config.Config).Save`), so after the first save the file holds every key below and any comments in it are gone. Keys the page does not show (`cms`, `staging`, `display`, `sim` and the rest) are set by editing the file while Core is stopped; a save from the page keeps their values.

## Initial Setup

The database connection is the only setting required before first launch. Copy [`shingocore.example.yaml`](../shingocore.example.yaml) to `shingocore.yaml` and set its `database.postgres` block. [PostgreSQL Setup](postgresql-setup.md) covers the server side.

## Command-Line Flags

| Flag | Description |
|------|-------------|
| `--config PATH` | Path to config file (default: `shingocore.yaml`) |
| `--version` | Print version and exit |
| `--help` | Print usage and exit |
| `--log-debug[=FILTER]` | Enable debug logging. Optional comma-separated filter of subsystems. |
| `--reset-db` | Wipe the database before starting. Requires interactive confirmation |
| `--pprof-port PORT` | Serve Go's pprof endpoints on `127.0.0.1:PORT` (default 0: off) |

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
| `inbox_dedup` | Inbound envelope dedup decisions |
| `engine` | Engine lifecycle events |

Without a filter (`--log-debug`), all subsystems are logged.

`--log-debug` controls the optional **file** (`shingo-debug.log`) only. What
reaches **stderr** — and therefore journald under systemd — is controlled by
`logging.stderr_subsystems` in the YAML; see [logging](#logging) below. The
in-memory ring buffer behind the Logs page (`/diagnostics`, Debug Log tab) is
never filtered by either: every subsystem always lands there.

## Runtime Configuration

The `/config` page has seven sections: Plant, Fleet, Messaging, Notifications,
Fire alarm, Database and Account. Save sends only the changed sections to
`PUT /api/config`; Account is the password change and saves nothing to the
file. A save validates the posted sections against a copy of the live config,
writes the file, swaps the copy in, then reconfigures what changed:

- **Applied on save:** the fleet manager address, request timeout and the
  fault grace Core itself enforces (`ReconfigureFleet`); the Kafka brokers and
  group ID (`ReconfigureMessaging`); notifications (`ReconfigureNotifications`);
  the fire-alarm switches, which are read where they are used.
- **Applied after a restart:** the plant timezone, the robot status poll
  interval, the orders and dispatch topics, the fault grace offered to Edges,
  and every database setting. A changed database section is pinged before the
  save writes it, and refused if it does not answer. `coreRestartFields` in
  `www/config_sections.go` is the list; the page shows a restart notice naming
  each one whose saved value differs from what the process started with.

Core reads the file once, at boot (`config.Load`): a hand edit to any key takes
effect at the next start.

## Config File Reference

The table below is every key the YAML accepts: key and default from
`config.Defaults()` (rendered to `shingo-core/config/defaults.snapshot.txt` by
`make defaults-snapshot`), type and description from the field comments in
`shingo-core/config`. The repository root's `make docs-gen` regenerates it.

<!-- generated: shingo-core/config Config -->

| Key | Type | Default | Description |
|---|---|---|---|
| `cms.access_key` | string | _(unset)_ | AccessKey and SecretKey go in the site-local yaml, never the repo. They are never logged, never in an error, and String() renders them as &lt;set>/&lt;empty>. |
| `cms.base_url` | string | _(empty)_ | BaseURL is the middleware's inventory_transactions endpoint. Empty disables the whole subsystem. |
| `cms.bin` | string | _(empty)_ | Bin is the CMS bin code this site's rows carry — master data CMS owns, not anything shingo can derive. See wire.Config.Bin. Empty ships an empty Bin, which the middleware refuses; no site in this repository sets one. |
| `cms.decrease_type` | string | `D` | The vocabulary CMS expects on every posted row. Each is a config edit rather than a code change. |
| `cms.department` | string | _(empty)_ | Blank in the vendor's sample. Declared so a value is a yaml edit. |
| `cms.health_window` | duration | `24h0m0s` | HealthWindow is how far back a REJECTED or FAILED posting still counts toward the diagnostics verdict. |
| `cms.increase_type` | string | `I` | The vocabulary CMS expects on every posted row. Each is a config edit rather than a code change. |
| `cms.insecure_skip_verify` | bool | `false` | InsecureSkipVerify disables TLS certificate verification on the calls to the middleware. It defaults to FALSE and the repository ships no site that sets it — it is opt-in, per site, in the site-local yaml. |
| `cms.max_attempts` | int | `12` | retryable send failures before a posting is failed |
| `cms.max_requeues` | int | `3` | MaxRequeues bounds how many times the reconciler may return one posting to the send queue before giving up on it. |
| `cms.operation` | string | _(empty)_ | Blank in the vendor's sample. Declared so a value is a yaml edit. |
| `cms.poll_interval` | duration | `30s` | the poster's backstop tick, and the base of its retry backoff |
| `cms.reason_code` | string | `TEST-AMR` | The vocabulary CMS expects on every posted row. Each is a config edit rather than a code change. |
| `cms.secret_key` | string | _(unset)_ | AccessKey and SecretKey go in the site-local yaml, never the repo. They are never logged, never in an error, and String() renders them as &lt;set>/&lt;empty>. |
| `cms.settle_window` | duration | `5m0s` | SettleWindow is how long an inflight posting is left alone before the reconciler asks the middleware about it. A POST that succeeded may not be queryable for a moment afterwards, and asking too early gets "no such transaction" about something that is about to exist — an answer that would requeue a posting that already landed. |
| `cms.timeout` | duration | `30s` | HTTP timeout on each call to the middleware |
| `cms.unit_of_measure` | string | `EA` | The vocabulary CMS expects on every posted row. Each is a config edit rather than a code change. |
| `cms.user_id` | string | `SHINGO` | The vocabulary CMS expects on every posted row. Each is a config edit rather than a code change. |
| `database.postgres.conn_max_lifetime` | duration | `0s` | The connection-pool limits store.OpenWithoutMigrate applies. Zero (the shipped value) means its own fallback: 25 open, 10 idle, a 5m lifetime. |
| `database.postgres.database` | string | `shingocore` | database name |
| `database.postgres.host` | string | `localhost` | PostgreSQL server hostname or IP |
| `database.postgres.max_idle_conns` | int | `0` | The connection-pool limits store.OpenWithoutMigrate applies. Zero (the shipped value) means its own fallback: 25 open, 10 idle, a 5m lifetime. |
| `database.postgres.max_open_conns` | int | `0` | The connection-pool limits store.OpenWithoutMigrate applies. Zero (the shipped value) means its own fallback: 25 open, 10 idle, a 5m lifetime. |
| `database.postgres.password` | string | _(unset)_ | database password |
| `database.postgres.port` | int | `5432` | PostgreSQL server port |
| `database.postgres.sslmode` | string | `disable` | disable, require, verify-ca or verify-full |
| `database.postgres.user` | string | `shingocore` | database user |
| `demand.childless_grace` | duration | `15m0s` | ChildlessGrace is how long an episode may stay open with ZERO orders against it before the sweep closes it `unattributed`. |
| `demand.orphan_grace` | duration | `24h0m0s` | OrphanGrace is how long an order stamped `orphan` stays in the finding set before the sweep ages it out. |
| `demand.reconcile_interval` | duration | `1m0s` | ReconcileInterval is the sweep cadence. Not a hot path — cost is bounded by open-episode count, which is one per place currently short of material — so a minute of latency on the rare miss costs nothing. <= 0 disables the sweep entirely, which leaves the notification paths as the only close mechanism; that is the pre-sweep behaviour and is deliberately reachable for a plant that wants to bisect a problem. |
| `dispatch.futility.alert_throttle` | duration | `15m0s` | AlertThrottle suppresses repeat records for the same tuple. Modelled on ThresholdMonitor's swapContradictionWindow, which is 15m for the same reason: the condition persists, so the record should not repeat per-order. |
| `dispatch.futility.threshold` | int | `20` | Threshold is how many futile terminals on one (station, process_node, payload) inside Window trip the record. Default 20: above the rate a legitimate tuple reaches and far below a cascade's. |
| `dispatch.futility.window` | duration | `1h0m0s` | Window is the rolling window the count is taken over. |
| `display.bar_steps` | int | `10` | BarSteps is how many steps the orphan-trend magnitude bars quantise to. |
| `display.concern_after` | duration | `1h0m0s` | ConcernAfter is the duration at which an open episode is a problem. Also its own channel, distinct from WorryAfter's. |
| `display.cycle_band_width` | float64 | `0.25` | CycleBandWidth is the histogram band width, in multiples of the key's own median. The band count follows from it — see domain.CycleBands. |
| `display.cycle_flush_interval` | duration | `5s` | CycleFlushInterval is the Edge accumulator's delta flush cadence. A key whose median cycle is at or below it is measuring the transport, not the cell, and the page says so instead of printing the number as a takt. |
| `display.cycle_min_samples` | int | `10` | CycleMinSamples is the fewest gaps a (station, payload, direction) key needs before its p90 and p99 are reported at all. |
| `display.cycle_spread_multiple` | float64 | `3` | CycleSpreadMultiple sets where a key's tail begins: median + k × (p90 − median). Expressed against the key's OWN spread rather than as a percentage of its takt, because dispersion inverts between a plant's fast and slow payload families and a percentage would over-trigger on one and under-trigger on the other. |
| `display.min_bucket_orders` | int | `20` | MinBucketOrders is the smallest order count for which an orphan RATE is shown at full strength. Below it the rate is greyed and the raw orphan count carries the bucket. |
| `display.min_expected_orders` | int | `2` | MinExpectedOrders is the smallest expected_orders for which a cost ratio is shown at full strength. Below it the ratio is greyed and the absolute order count carries the row. |
| `display.orphan_bucket` | duration | `1h0m0s` | OrphanBucket is the time-bucket width for the orphan-rate trend. |
| `display.overpack_binloads` | float64 | `1` | OverpackBinloads is where a negative ledger stops being explicable as ONE overpacked carrier, expressed in multiples of that carrier's own payload capacity rather than in units. |
| `display.ramp_steps` | int | `5` | RampSteps is how many steps the magnitude ramp has. |
| `display.route_index_min_robot_samples` | int | `10` | RouteIndexMinRobotSamples is the fewest indexed missions a robot needs before ITS index is shown at full strength. Below it the figure is greyed and the mission count carries the row — the same rule as MinExpectedOrders and MinBucketOrders, one surface along. |
| `display.route_index_min_route_samples` | int | `8` | RouteIndexMinRouteSamples is the fewest missions a route needs before its median may be used as the denominator of a robot's route index. |
| `display.stale_binding_after` | duration | `72h0m0s` | StaleBindingAfter is how long a carrier may hold one binding before that binding is a CANDIDATE for having gone stale — ShinGo still believes the carrier holds payload P at node N while the floor has moved on around it. |
| `display.worry_after` | duration | `45m0s` | WorryAfter is the duration at which an open demand episode stops being routine. Crossing it takes ITS OWN VISUAL CHANNEL — a ring treatment plus a printed band name — never colour alone. |
| `fire_alarm.auto_resume_default` | bool | `false` | default checkbox state for auto-resume on clear |
| `fire_alarm.enabled` | bool | `false` | feature gate; false = hidden from UI |
| `logging.stderr_subsystems` | list of string | `[dispatch engine core_handler kafka outbox protocol]` | StderrSubsystems is the allow-list of debuglog subsystems mirrored to stderr (under systemd, the journal). |
| `messaging.dispatch_topic` | string | `shingo.dispatch` | the topic Core publishes to edges on |
| `messaging.kafka.brokers` | list of string | `[localhost:9092]` | Kafka broker addresses, host:port |
| `messaging.kafka.dial_timeout` | duration | `0s` | DialTimeout bounds the per-broker reachability probe in Connect. Zero means the 5s production default. A config that names unreachable brokers pays this per broker, serially — which is why it is a knob at all: the config-save handler reconfigures messaging inline, and a test (or a plant) saving broker names that don't resolve would hold the handler for 5s × brokers. |
| `messaging.kafka.group_id` | string | `shingocore` | Kafka consumer group Core joins |
| `messaging.orders_topic` | string | `shingo.orders` | the topic edges publish to Core on |
| `messaging.outbox_drain_interval` | duration | `5s` | how often the outbox drainer publishes pending messages |
| `messaging.signing_key` | string | _(unset)_ | optional HMAC-SHA256 shared secret for envelope signing |
| `messaging.stale_edge_threshold` | duration | `15m0s` | StaleEdgeThreshold is how long an edge can go without a heartbeat before core marks it stale and announces it. It is also the horizon the demand reconciler reads to decide whether a station's silence is evidence about anything — one number for one question. Zero falls back to the 15 minute default. Tune down for faster reaction to edge failures at the cost of more false positives on flaky links; tune up if edges routinely pause longer than 15 min. |
| `messaging.station_id` | string | `core` | this Core's station identity on the wire |
| `notifications.enabled` | bool | `false` | master switch for e-mail alerts |
| `notifications.from_address` | string | _(empty)_ | the From: address alerts are sent as |
| `notifications.recipients` | list of string | _(empty)_ | addresses every alert goes to |
| `notifications.smtp_host` | string | `localhost` | SMTP server hostname or IP |
| `notifications.smtp_password` | string | _(unset)_ | SMTP login password |
| `notifications.smtp_port` | int | `587` | SMTP server port |
| `notifications.smtp_tls` | bool | `true` | send over TLS (notify.TLSSend) rather than plain SMTP |
| `notifications.smtp_user` | string | _(empty)_ | SMTP login user |
| `notifications.throttle_minutes` | int | `15` | ThrottleMinutes is saved and shown by the config page; no sender reads it. |
| `rds.base_url` | string | `http://192.168.1.100:8088` | the fleet backend's (Seer RDS) API base URL |
| `rds.fault_grace` | duration | `45m0s` | FaultGrace is how long an order sits in `faulted` after RDS reports FAILED before Core gives up and fails it. A robot that recovers inside the window (FAILED->RUNNING) clears the deadline and the order carries on, so this is really "how long we let the floor sort a stuck AMR out before the order is written off". |
| `rds.fault_notice_after` | duration | `1m0s` | FaultNoticeAfter is how long an order must have been faulted before the floor is told the word "fault". Below it the order is described as REPLANNING; at or above it, as a FAULT with the fleet's reason. |
| `rds.poll_interval` | duration | `5s` | how often the poller reads RDS for order status changes |
| `rds.stranded_sweep_window` | duration | `2h0m0s` | StrandedSweepWindow is how recently a `_TRANSIT` bin's order must have ended for the reconciliation sweep to infer where that bin was set down. |
| `rds.timeout` | duration | `10s` | HTTP timeout on each RDS API call |
| `robot_confidence.baseline_days` | int | `14` | BaselineDays is the trailing window the per-segment fleet median is computed over. It must not be same-day: against a same-day baseline a plant-wide degradation moves the median with it and the event vanishes. |
| `robot_confidence.dead_band_confidence` | float64 | `0.02` | DeadBandMetres and DeadBandConfidence are clauses 1 and 2 of the write rule — how far a robot must move, or how much the number must change, since the last STORED sample. |
| `robot_confidence.dead_band_metres` | float64 | `0.25` | DeadBandMetres and DeadBandConfidence are clauses 1 and 2 of the write rule — how far a robot must move, or how much the number must change, since the last STORED sample. |
| `robot_confidence.enabled` | bool | `true` | Enabled false skips the write path ENTIRELY rather than writing and discarding — the kill switch if a plant sees any load surprise. |
| `robot_confidence.failed_interval` | duration | `10s` | The three rate limits on the clauses that fire while a robot is stationary. Without them a robot sitting in a bad state would store a row every poll. (clause 5) |
| `robot_confidence.low_confidence_retention_days` | int | `90` | LowConfidenceRetentionDays keeps the low trail far longer than raw: the row count is tiny and it is what an incident review reads. |
| `robot_confidence.low_confidence_threshold` | float64 | `0.5` | LowConfidenceThreshold is both the double-write cut for the forensic trail and clause 3 of the write rule. |
| `robot_confidence.low_interval` | duration | `10s` | The three rate limits on the clauses that fire while a robot is stationary. Without them a robot sitting in a bad state would store a row every poll. (clause 3) |
| `robot_confidence.raw_retention_days` | int | `14` | RawRetentionDays is how long full-resolution samples are kept. Two weeks is the shortest window that can answer "is this new?" from raw data; seven days gives one week and nothing to compare it against. |
| `robot_confidence.snap_tolerance_metres` | float64 | `2` | SnapToleranceMetres is how far a sample may sit from a path segment and still be attributed to it by the nightly roll-up. Generous by necessity: scene_edges stores only segment endpoints, so a curved path is snapped against its chord, which on a curve can sit more than a metre off the driven lane. Read-time only — changing it re-bins future roll-ups and never touches a stored sample. |
| `robot_confidence.stuck_interval` | duration | `30s` | The three rate limits on the clauses that fire while a robot is stationary. Without them a robot sitting in a bad state would store a row every poll. (clause 4) |
| `sim.anchor_wall` | timestamp | _(zero)_ | SHARED wall anchor for fast-forward sync: sim-now = epoch + speed×(wallNow−anchor). Set IDENTICALLY in core+edge to the run-start wall time so the two clocks stay in lockstep (no cross-process drift). Zero = per-process boot anchor (drifts — only safe single-process). |
| `sim.enabled` | bool | `false` | the sim switch; needs a sim build and SHINGO_ALLOW_SIM=1 |
| `sim.epoch` | timestamp | _(zero)_ | sim clock start (fast-forward origin). Zero = wall-now |
| `sim.fail_rate` | float64 | `0` | 0.0–1.0 per-transition fault probability; default 0 |
| `sim.fleet_size` | int | `0` | Finite-fleet model. With none of these set the sim fleet is infinite: one synthetic robot per active order, flat transit. (0 = infinite fleet (default); >0 = finite robot pool, orders queue for a free robot) |
| `sim.jitter_pct` | float64 | `0.2` | ± fraction applied to transit; default 0.2 |
| `sim.max_speed` | float64 | `0` | effective-speed cap; <=0 → default (clock.DefaultSimMaxSpeed, 10×). The integration sim can only process the real choreography so fast; past this the clock would outrun it and wedge, so requests are clamped here (honest readout shows asked-vs-running). Set very high to effectively uncap. |
| `sim.seed` | int64 | `0` | PRNG seed; 0 = derive from time and log it |
| `sim.speed` | float64 | `0` | time multiplier: 2.0 = twice as fast. Default 1.0 |
| `sim.transit_max` | duration | `0s` | Finite-fleet model. With none of these set the sim fleet is infinite: one synthetic robot per active order, flat transit. (max per-move transit (uniform draw with transit_min); must exceed transit_min to take effect) |
| `sim.transit_min` | duration | `0s` | Finite-fleet model. With none of these set the sim fleet is infinite: one synthetic robot per active order, flat transit. (min per-move transit; 0 falls back to transit_time ± jitter) |
| `sim.transit_time` | duration | `5s` | base per-block transit; default 5s |
| `sourceability.enable_at_risk` | bool | `false` | EnableAtRisk lets a satisfiable-but-projected-empty style report YELLOW. Off, the plant sees green and red only. The at-risk tier is always COMPUTED; this only controls whether it surfaces as a status. |
| `sourceability.horizon` | duration | `30m0s` | Horizon: a line projecting empty within this window is at risk. Default 30m. |
| `sourceability.rate_window` | duration | `30m0s` | RateWindow is the look-back for the per-payload consumption rate that feeds time-to-empty. Default 30m. |
| `staging.abandon_stuck` | duration | `1h0m0s` | AbandonStuck cancels orders stuck past this age. It covers exactly TWO statuses — dispatched and staged — the ones where the fleet has the order and nothing is moving: a leg handed over that never started (the long-weekend drain case), or a robot parked at a staging node. protocol.IsStuckSweepCandidate is the authority; this comment is not. (default 1h; 0 = disabled) |
| `staging.abandon_stuck_operator_gated` | duration | `4h0m0s` | AbandonStuckOperatorGated is the SEPARATE, longer bound for a staged leg whose release is a HUMAN action rather than a system step — a coordinated two-robot swap parked at its wait point (dispatch.IsOperatorGatedStaging). (default 4h; 0 = never) |
| `staging.auto_confirm_delivered` | duration | `5m0s` | AutoConfirmDelivered is how long an order may sit at delivered with no receipt before the sweep confirms it; an order sent with SkipAutoConfirm is never confirmed this way. (0 = disabled) |
| `staging.sweep_interval` | duration | `5m0s` | SweepInterval is the reconciliation sweep's tick. (default 5m) |
| `staging.ttl` | duration | `0s` | TTL is RETIRED. Staged bins never expire; they are released only by the next claim or by a person (engine resolveNodeStaging). The key is still read so an existing config file loads; a non-zero value is ignored, and Core logs that once at startup. (retired: ignored) |
| `timezone` | string | _(empty)_ | Timezone is the plant's IANA zone for DISPLAY rendering (plant-local timestamps on every core page, via shared/planttime) and for the plant-local date-filter resolution. Empty resolves to UTC (planttime.DefaultZone, resolved by planttime.ResolveZone): visibly foreign rather than plausibly local, so an unset zone shows. Storage and the wire stay UTC regardless; this field never touches either. PLANT_TIMEZONE env still overrides it, so existing deployments don't move on upgrade; Edges are offered the same zone Core's screens use (www.OfferedPlantTimezone), as read at Core's boot. Read once at boot on both paths: a change applies after a restart. |
| `web.host` | string | `0.0.0.0` | web server listen address |
| `web.port` | int | `8083` | web server port |
| `web.session_secret` | string | _(set; redacted)_ | SessionSecret signs the login session cookie. Nothing generates it: the shipped value is a fixed placeholder literal, and an empty value falls back to a second literal in www (newSessionStore). Set it on every deployment. |

<!-- /generated -->

### logging

Gates which debuglog subsystems are mirrored to stderr. Under systemd that is
journald, so this is the journal-volume knob.

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
- **Muting is not disabling.** `rds` is off the default list because the
  fleet poller traces every poll, which would flood the journal. The poller
  still runs unchanged; only its tracing is out of the journal.
- **Nothing is lost to the UI.** The ring buffer behind the Logs page is
  unfiltered (`TestStderrGate_RingBufferKeepsMutedSubsystems`).
- The value semantics are pinned in `shingo-core/config/logging_test.go`
  (`TestStderrSubsystems_AbsentUsesDefaults`,
  `TestStderrSubsystems_AllMeansNoRestriction`,
  `TestStderrSubsystems_EmptyAndNullMuteTheMirror`). Retention itself is a separate,
  host-wide decision — see [Deploy artifacts](../deploy/README.md).

### dispatch.futility

The rate-per-tuple futility detector (`dispatch.FutilityConfig`, installed by
`InstallFutilityDetector` at boot): a net for the class of failure where the
planner turns a bounded physical condition into unbounded orchestration work —
orders planned and abandoned over and over for one tuple, none reaching a
robot, while every surface stays green.

```yaml
dispatch:
    futility:
        threshold: 20
        window: 60m
        alert_throttle: 15m
```

**There is no `enabled` key.** The detector records at every site, always. A
`threshold` or `window` of zero describes nothing countable and disables it by
being malformed (`TestFutility_MalformedConfigReturnsNil`); that is not a
supported way to turn it off.

A "futile" terminal is an order that reached a terminal status without ever
having reached `in_transit` — planned, then abandoned before a robot moved.
Any order for the same tuple reaching `in_transit` resets the count
(`TestFutility_InTransitResetsTheTuple`).

**Rate, not a run count.** Normal operation produces runs of consecutive futile
terminals with a long tail and no clean cut-off, so a run-length threshold
either fires on ordinary days or misses the cascade. Counting per tuple inside
a rolling `window` separates them (`TestFutility_LegitimateSlowRunDoesNotFire`,
`TestFutility_CascadeFires`).

**Absolute, not learned.** The threshold is a fixed number, not a trailing
baseline: a learned baseline is trained on whatever incidents its window holds.

**Observe-only.** One log line and one `audit_log` row per trigger, repeats for
the same tuple suppressed for `alert_throttle`
(`TestFutility_ThrottlesRepeats`). No chip, no alert, no brake — a brake on an
unproven threshold stops real work.

### Duration Format

Duration fields accept Go duration strings: `5s`, `10s`, `1m`, `500ms`, `2m30s`.
