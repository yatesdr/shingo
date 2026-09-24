package schema

// sqliteDDL is the canonical "fresh DB" schema for shingo-edge. Every
// statement is idempotent (CREATE ... IF NOT EXISTS) so Apply() can
// be invoked at any point in a database's lifecycle.
//
// Moved here from store/schema.go in Phase 6.0b. The schema constant
// and the schemaMigrations cleanup constant used to be sibling
// constants in that file; the cleanup constant moved to
// store/migrations.go alongside the rest of the migration logic
// because conceptually it is a migration step (drop tables that have
// been fully removed from the canonical schema), not part of the
// "what should the database look like" definition.
const sqliteDDL = `
CREATE TABLE IF NOT EXISTS admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- process_groups is an organizational layer for the Processes admin page.
-- A process is in at most one group (or none — "Ungrouped"). Deleting a
-- group reverts its members to Ungrouped (ON DELETE SET NULL). Pure UI
-- taxonomy; nothing in the runtime reads group_id.
CREATE TABLE IF NOT EXISTS process_groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS processes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    target_style_id     INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    production_state    TEXT NOT NULL DEFAULT 'active_production',
    counter_plc_name    TEXT NOT NULL DEFAULT '',
    counter_tag_name    TEXT NOT NULL DEFAULT '',
    counter_enabled     INTEGER NOT NULL DEFAULT 0,
    auto_cutover_enabled INTEGER NOT NULL DEFAULT 0,
    changeover_auto_arm TEXT NOT NULL DEFAULT 'auto',
    group_id            INTEGER REFERENCES process_groups(id) ON DELETE SET NULL,
    -- flow_composer_enabled gates the HMI flow composer for this process. OFF
    -- until the engineer has reviewed the routing set derived into
    -- process_routing_nodes below; the backfill re-derives only while it is
    -- off, so a reviewed set is never silently re-seeded.
    flow_composer_enabled INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

-- deleted_at is a SOFT DELETE, and it is a foreign-key decision before it is
-- a UX one. Seven columns across six tables carry REFERENCES styles(id), four
-- of them ON DELETE CASCADE, and one of those (reporting_points) cascades
-- again into counter_snapshots. A hard DELETE of one style on the Springfield
-- edge takes up to 91,581 rows with it — measured, style 12, of which 91,256
-- are raw counter readings. Retiring a part number is a routine operator
-- action; destroying a quarter of the plant's counting history is not, and it
-- is not reversible. A soft-deleted row never leaves the table, so nothing
-- that points at it can dangle and the decision can be undone.
--
-- The uniqueness constraint moved OUT of the table and into a partial index
-- below, because UNIQUE(process_id, name) as a table constraint applies to
-- tombstoned rows too, which would make "delete style A, create style A again"
-- fail — an operator-visible regression that soft delete would otherwise
-- introduce silently.
CREATE TABLE IF NOT EXISTS styles (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id     INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    expected_catid TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at     TEXT
);
-- idx_styles_process_name_live is created in migrations.go, not here, for the
-- same reason as idx_orders_source_node: Apply runs against legacy-shaped
-- tables whose styles still carries line_id, where an index on process_id
-- fails and takes the whole DDL with it.

CREATE TABLE IF NOT EXISTS reporting_points (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id        INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    plc_name        TEXT NOT NULL,
    tag_name        TEXT NOT NULL,
    last_count      INTEGER NOT NULL DEFAULT 0,
    last_poll_at    TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    warlink_managed INTEGER NOT NULL DEFAULT 0,
    UNIQUE(plc_name, tag_name)
);

-- ON DELETE CASCADE, not the bare (NO ACTION) it carried until 2026-07.
--
-- A reporting point belongs to its style: styles -> reporting_points is
-- CASCADE, so deleting a style tries to take its reporting points with it —
-- and used to hit this clause as a restrict, because NO ACTION on a NOT NULL
-- column means refuse. The whole delete then aborted. Measured against the
-- Springfield dump of 2026-07-27 with foreign_keys ON: 6 of 8 style deletions
-- were REFUSED with FOREIGN KEY constraint failed (787), and the 6 were
-- exactly the styles the plant had actually been running. With this clause as
-- CASCADE the same probe deletes all 57 cleanly.
--
-- That is the edge that blocks enabling enforcement at all, so it is a schema
-- decision and not a preference. The cost is bounded by counters.SnapshotRetention
-- (14 days), so a style deletion destroys at most two weeks of raw readings —
-- the counting record lives in hourly_counts, which is kept permanently.
CREATE TABLE IF NOT EXISTS counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id) ON DELETE CASCADE,
    count_value        INTEGER NOT NULL,
    delta              INTEGER NOT NULL DEFAULT 0,
    anomaly            TEXT,
    operator_confirmed INTEGER NOT NULL DEFAULT 0,
    recorded_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS orders (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    uuid            TEXT NOT NULL UNIQUE,
    order_type      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL,
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
    -- delivery_node: authoritative for SIMPLE orders (one bin, one destination).
    --
    -- For COMPLEX orders it is effectively a DISPLAY value and nothing
    -- correctness-critical reads it any more. A complex leg has many dropoffs, so
    -- one destination field cannot say where its bin came to rest; auto-confirm
    -- legs store '' outright; and the swap legs that do store something name the
    -- node the ROBOT ends at, not the node the BIN ends at. Every decision that
    -- used to consult it — the delivered gate, the supply/evac classifier, the sim
    -- operator's confirm scope — now reads steps_json (see swap_leg_role.go).
    --
    -- It is NOT the same value as Core's orders.delivery_node. Edge does not send
    -- one: ComplexOrderRequest has no such field, and Core derives its own from
    -- the steps (extractEndpoints). Two columns, one name, independent values.
    delivery_node   TEXT NOT NULL DEFAULT '',
    staging_node    TEXT NOT NULL DEFAULT '',
    source_node     TEXT NOT NULL DEFAULT '',
    load_type       TEXT NOT NULL DEFAULT '',
    waybill_id      TEXT,
    external_ref    TEXT,
    final_count     INTEGER,
    count_confirmed INTEGER NOT NULL DEFAULT 0,
    eta             TEXT,
    auto_confirm    INTEGER NOT NULL DEFAULT 0,
    steps_json      TEXT NOT NULL DEFAULT '',
    staged_expire_at TEXT,
    bin_id          INTEGER,
    payload_code    TEXT NOT NULL DEFAULT '',
    sibling_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    queue_reason    TEXT NOT NULL DEFAULT '',
    queue_code      TEXT NOT NULL DEFAULT '',
    -- authored_by: who decided this order should exist. 'edge' (the default, and
    -- what every existing row is) means this Edge created it and sent it up;
    -- 'core' means Core created it and pushed the row down. Nothing branches on
    -- it: it labels the board and it is what a projected-row test asserts
    -- against. Deliberately cheap to stop rendering.
    authored_by     TEXT NOT NULL DEFAULT 'edge',
    -- The fault clock (v36). Set only while the order is faulted; the handler
    -- clears them on any other status, derived from the status rather than from
    -- a pushed empty value — see messaging/edge_handler.go and the queue_reason
    -- incident it documents. fault_notice_after_s is Core's replan/fault
    -- threshold in seconds as it stood when the fault was pushed; 0 means an
    -- older Core that did not send one.
    fault_since     TEXT NOT NULL DEFAULT '',
    fault_deadline  TEXT NOT NULL DEFAULT '',
    fault_notice_after_s INTEGER NOT NULL DEFAULT 0,
    -- The fleet's reason as protocol.TermRef JSON. Stored as a reference, not a
    -- rendered sentence: the sentence changes when the clock crosses the
    -- threshold, and the board re-renders it without another push.
    fault_ref       TEXT NOT NULL DEFAULT '',
    -- departed_at (v39): when this leg stopped being its cell's business — the
    -- instant the fleet confirmed the last step of steps_json whose node is in
    -- the claim's cell set. NULL is "still working the cell", which is what
    -- every row starts as and what a leg whose last cell step is its FINAL step
    -- stays as forever (terminal covers that shape; nothing stamps it).
    --
    -- Stamped once and never cleared: it records a physical event, and Core
    -- re-fires already-FINISHED blocks after a restart, so a second write would
    -- have to be a no-op anyway. NULL, not '', because the two admission guards
    -- read it as a three-state answer alongside a status — see MarkDeparted.
    departed_at     TEXT,
    -- cell_left_at (v40): when the fleet confirmed the last step of steps_json
    -- whose node is in the claim's cell set — the robot has left the cell's
    -- NODES. This is what departed_at alone used to mean.
    --
    -- departed_at is now the CONJUNCTION: the robot has left AND the leg's own
    -- placement at the line position has been recorded. The two separate for
    -- single_robot, which places at step 7 and lifts the spent carrier off
    -- OutboundStaging at step 8 — so it leaves the cell's nodes one step after
    -- it has placed and before the placement is on the books.
    --
    -- Its one reader is settleCellPlacement (engine/leg_departure.go), which
    -- completes the departure when the placement lands second. NULL means the
    -- leg has not left the cell's nodes.
    cell_left_at    TEXT,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(uuid);
-- idx_orders_source_node is created in migrations.go (after the orders table
-- is guaranteed current), not here: schema.Apply runs against legacy-shaped
-- order tables that predate the source_node column, where a canonical index
-- on it would fail.

CREATE TABLE IF NOT EXISTS order_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id   INTEGER NOT NULL REFERENCES orders(id),
    old_status TEXT NOT NULL,
    new_status TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS outbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    topic      TEXT NOT NULL,
    payload    BLOB NOT NULL,
    msg_type   TEXT NOT NULL DEFAULT '',
    retries    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    sent_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(sent_at) WHERE sent_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_order_history_order_id ON order_history(order_id);
CREATE INDEX IF NOT EXISTS idx_counter_snapshots_anomaly ON counter_snapshots(anomaly, operator_confirmed)
    WHERE anomaly IS NOT NULL AND operator_confirmed = 0;

CREATE TABLE IF NOT EXISTS shifts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL DEFAULT '',
    shift_number INTEGER NOT NULL UNIQUE,
    start_time   TEXT NOT NULL,
    end_time     TEXT NOT NULL
);

-- hourly_counts is the MIDDLE rung of the counting ladder:
--
--     counter_snapshots  raw, one row per poll   14 days  (counters.SnapshotRetention)
--     hourly_counts      per process/style/hour  permanent
--
-- There is no daily rung. A day total is SUM(delta) over the buckets in that
-- day's UTC range, which is a query, not a table — see the note on retention
-- below for why keeping the hours makes the second copy pointless.
--
-- BUCKETS ARE UTC. bucket_start is the unix second at the start of the UTC hour
-- a delta landed in, and it is the ONLY timezone-bearing decision in the
-- counting path — which is to say there is now no timezone decision stored at
-- all. The plant zone enters on the way OUT, when a local day or a local hour
-- is asked for.
--
-- WHY THIS CHANGED (2026-09-08). The buckets used to be keyed by plant-local
-- count_date + hour, which put a timezone into stored data and made an
-- unconfigured or wrong zone permanent rather than cosmetic. Hopkinsville ran
-- for three months with the Pi's OS zone (America/Indiana/Indianapolis) at a
-- Central plant, so every bucket was labelled an hour late and each day's last
-- hour landed on the following date. Core already stores UTC and resolves local
-- on read (shingo-core/www/plant_timezone.go, Q-004); this is the counter
-- rollup finally doing the same.
--
-- IT ALSO FIXES A BUG NOBODY HAD HIT YET. Local bucketing breaks at DST: on the
-- autumn fall-back 01:00-02:00 happens TWICE, both writes carry the same
-- (count_date, hour), and the UNIQUE + upsert SUMS two different real hours into
-- one row. In spring, hour 2 never exists and reads as downtime. UTC has no DST,
-- so neither can happen.
--
-- Retention is gone from this rung. It existed to bound growth, but the measured
-- rate is ~1,400 rows a year (Hopkinsville: 350 rows in three months — a row
-- appears only for an hour that actually produced). Keeping every hour forever
-- costs nothing and is what makes a DAY re-derivable at all, in whatever zone
-- is asked for. That is what retired the daily roll-up: its rows reproduced
-- exactly from these (verified across all 84 at Hopkinsville), so it was a
-- second copy of the same truth with a plant-local date baked into its key.
CREATE TABLE IF NOT EXISTS hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    bucket_start INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, bucket_start)
);

-- hourly_counts_local_legacy holds the plant-local rows written BEFORE the
-- 2026-09 move to UTC buckets. Nothing reads it and nothing writes it; it is
-- an archive, kept because the migration declined to reinterpret those rows
-- (see store/migrations.go migrateHourlyCountsToUTC for why guessing the zone
-- they were written in is not something the data supports).
--
-- IT IS DECLARED HERE, SO A FRESH INSTALL HAS IT TOO — EMPTY. That looks
-- redundant and is not: schema convergence between a fresh database and an
-- upgraded one is a tested property (internal/schemadump), and the alternative
-- was an entry in KnownDivergences, a list whose header says in capitals that
-- nothing new goes in it. An empty table on a new plant costs one line of
-- sqlite_master; a suppressed convergence failure costs the next person the
-- test was written for.
--
-- Safe to drop once the pre-2026-09 hour detail is no longer wanted. The day
-- totals for those dates are unaffected either way: SUM these rows by
-- count_date and they are exactly what the retired daily roll-up held.
-- The foreign keys are here because the upgraded copy has them: this table is
-- produced by RENAMEing hourly_counts, which carries its CREATE text across
-- unchanged. Declaring it without them makes a fresh database differ from an
-- upgraded one in exactly the way the convergence test exists to catch. The
-- CASCADE is therefore not a new decision — deleting a process discarded that
-- process's archived hours before this migration too.
CREATE TABLE IF NOT EXISTS hourly_counts_local_legacy (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);

CREATE TABLE IF NOT EXISTS payload_catalog (
    id            INTEGER PRIMARY KEY,
    name          TEXT NOT NULL,
    code          TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    uop_capacity  INTEGER NOT NULL DEFAULT 0,
    -- Edge-local per-part cycle time (seconds per UOP at the consuming
    -- cell). NOT synced from Core — different installations may run the
    -- same part at different rates, and the calculator on this Edge is
    -- the only consumer. Engineer-edited via the replenishment page;
    -- preserved across catalog syncs (UpsertCatalog excludes this column
    -- from its ON CONFLICT update list).
    cycle_seconds REAL NOT NULL DEFAULT 0,
    catid         TEXT NOT NULL DEFAULT '',
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS operator_stations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id         INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL,
    note               TEXT NOT NULL DEFAULT '',
    area_label         TEXT NOT NULL DEFAULT '',
    sequence           INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    device_mode        TEXT NOT NULL DEFAULT 'touch_hmi',
    enabled            INTEGER NOT NULL DEFAULT 1,
    health_status      TEXT NOT NULL DEFAULT 'offline',
    last_seen_at       TEXT,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, code)
);

CREATE TABLE IF NOT EXISTS process_nodes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id          INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    operator_station_id INTEGER REFERENCES operator_stations(id) ON DELETE SET NULL,
    core_node_name      TEXT NOT NULL DEFAULT '',
    code                TEXT NOT NULL,
    name                TEXT NOT NULL,
    sequence            INTEGER NOT NULL DEFAULT 0,
    enabled             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    -- Soft delete, for the same reason styles has one. Deleting a
    -- process_node CASCADEs into process_node_runtime_states and
    -- changeover_node_tasks — and changeover_node_tasks.process_node_id is
    -- NOT NULL, so per-node changeover detail is destroyed outright with no
    -- SET NULL option. 118 such rows already exist on the Springfield edge
    -- whose node is gone while all 118 parent changeovers survive: readable
    -- history with an unreadable middle. The uniqueness constraint moved to a
    -- partial index below so a re-created node can reuse a retired code.
    deleted_at          TEXT
);
-- idx_process_nodes_process_code_live is created in migrations.go — see the
-- note on idx_styles_process_name_live above.

-- process_routing_nodes is a process's ROUTING SET: the nodes it may route
-- material through that are not its own positions — where bins come from
-- (source), where they wait (staging) and where they go (destination). The
-- composer's pickers offer a press only these, never the plant.
--
-- A TABLE, NOT A ROLE COLUMN ON process_nodes, on three verified facts:
-- membership in process_nodes is runtime behaviour (the delivered fallback
-- treats "not a process node" as the correct silent answer for a supermarket
-- delivery, so a routing node there becomes alarm noise); changeover_service
-- auto-inserts process_nodes rows with a four-column INSERT that would take
-- any role default; and SetNodes re-points and deletes rows with no role
-- concept, so a role there dies on an unrelated board edit. One row per
-- (process, node, role): SMN_BUF_100 is legitimately a source AND a
-- destination, which a scalar role on a unique (process, node) row cannot say.
-- Positions stay in process_nodes; the effective routing picture is the
-- union. 'waypoint' is not a role — key_route is ordered and validated against
-- the map, not against this list.
--
-- origin records where a row came from: 'engineer' (typed on the desktop) or
-- 'backfill' (derived from live claims' source / staging / destination fields,
-- landing DISABLED until an engineer adopts it).
CREATE TABLE IF NOT EXISTS process_routing_nodes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id     INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    core_node_name TEXT NOT NULL,
    role           TEXT NOT NULL CHECK(role IN ('source','staging','destination')),
    label          TEXT NOT NULL DEFAULT '',
    sequence       INTEGER NOT NULL DEFAULT 0,
    enabled        INTEGER NOT NULL DEFAULT 1,
    origin         TEXT NOT NULL DEFAULT 'engineer' CHECK(origin IN ('engineer','backfill')),
    called_by      TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, core_node_name, role)
);
CREATE INDEX IF NOT EXISTS idx_process_routing_nodes_process ON process_routing_nodes(process_id);

-- process_payloads is a process's PART SET: the payloads its flows may put on
-- a position. The composer's part pickers offer a process only these, never
-- the 250-row plant catalog.
--
-- A TABLE, AND NOT DERIVED FROM THE CLAIMS. The obvious cheaper shape is
-- "whatever this process's claims already name", and it fails on exactly the
-- case that asked for this: a cell nobody has configured has no claims, so the
-- derived set is empty and there is nothing to put on its first position. The
-- HMI has no keyboard to search a catalog with, either.
--
-- THE OFFER LIST IS THE UNION of these rows and the payloads the process's
-- live claims name — the same shape the routing set has. Only one half is
-- writable, so it is not two sources of truth: a part claimed before this
-- table existed still shows up, and nothing has to be backfilled into rows
-- nobody switched on.
--
-- AN OFFER LIST, ENFORCED AT THE PICKER AND NOWHERE ELSE. There is no
-- save-time check that a claim's payload is in here, deliberately: the flow
-- save checks payload PRESENCE (domain/claim_validation.go) and a membership
-- check added here would refuse a claim an engineer wrote from the other
-- surface. Same rule as process_routing_nodes.
--
-- No separate process_id index: UNIQUE(process_id, payload_code) already
-- builds one with process_id leading, and every read of this table filters by
-- process — the precedent is written at flow_presets below.
CREATE TABLE IF NOT EXISTS process_payloads (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    payload_code TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, payload_code)
);

-- flow_presets is a NAMED FLOW an engineer saves for a process: a set of cells
-- (positions, choreography, sources, destinations) with the parts left blank,
-- so the floor can pick "the two-position press-index flow" for a new part
-- without building it. flow_json is the composer's cell set; a preset with a
-- payload in it is refused at write time, because a preset is a shape and
-- the part is chosen when it is applied.
--
-- Versioned, never edited in place: a claim expanded from a preset records
-- (source_preset_id, source_preset_version) as provenance, and that reference
-- has to keep meaning what it meant. archived_at hides a version from the
-- chooser without breaking the reference. UNIQUE(process_id, name, version).
CREATE TABLE IF NOT EXISTS flow_presets (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    flow_json   TEXT NOT NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    archived_at TEXT,
    UNIQUE(process_id, name, version)
);
-- No separate process_id index: UNIQUE(process_id, name, version) already
-- builds one with process_id leading, and every read of this table filters by
-- process. The second index was a duplicate b-tree maintained on every preset
-- write for a lookup the autoindex already served (migrate() drops it).

CREATE TABLE IF NOT EXISTS process_node_runtime_states (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id    INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    -- active_claim_id NAMES A STYLE'S CLAIM, NOT A CARRIER, and the name is
    -- the trap. It answers "which style-claim is this node running" —
    -- configuration and intent — and eighteen paths write it, most of them
    -- from the process's active style. It is not "the claim the bin standing
    -- here was stocked for": a changeover moves it while a carrier stays put,
    -- and reading it for identity is the defect class this table's
    -- lineside_* columns exist to end. What is standing here is
    -- lineside_payload_code below.
    --
    -- The surviving readers all want the configuration answer: the changeover
    -- readiness check, the switch-node skip arm, the release-time capacity
    -- reset, the cancel re-bind, and the clear-shaped writes that thread it
    -- back unchanged. The claim advance at applyChangeoverRelease /
    -- applyStagedDelivery is what keeps that answer current during a
    -- changeover.
    --
    -- ON DELETE SET NULL, so a deleted claim leaves the pointer NULL rather
    -- than dangling, and every reader here treats NULL as "no opinion" and
    -- fails open. That is the designed degradation and it is also why a
    -- config edit can silently widen behaviour — HK SMN_01 is the live
    -- instance. A reader that must not fail open should ask the running
    -- style through ResolveNodeClaim instead of dereferencing this.
    active_claim_id    INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    active_bin_id      INTEGER,
    -- active_bin_epoch mirrors Core's bins.delta_epoch for the bin
    -- currently active at this slot. Edge stamps every outgoing
    -- BinUOPDelta with the value so Core's epoch-aware dedup accepts
    -- the delta. Survives Edge restart so post-restart ticks don't emit
    -- at epoch=0 against a bin already at epoch>=1.
    --
    -- Four writers, and "FetchNodeBins refresh" was not one of them: the
    -- OrderDelivered envelope, Core's LoadBin/clear/count replies, the
    -- BinAtLineside re-bind after a changeover cancel, and Core's
    -- BinEpochRefresh push. Every FetchNodeBins call site discards the
    -- epoch it is handed.
    active_bin_epoch   INTEGER NOT NULL DEFAULT 0,
    -- last_bin_id / last_bin_epoch are the bin that last left this slot and
    -- the stamp it left with, written by every statement that moves
    -- active_bin_id. An empty slot's active_bin_epoch belongs to no bound bin;
    -- these say whose it was, so a late correction for a carrier that has
    -- left cannot rebind it under an ended generation.
    last_bin_id        INTEGER,
    last_bin_epoch     INTEGER NOT NULL DEFAULT 0,
    -- lineside_payload_code is what the carrier standing here actually is,
    -- as opposed to what this node's style says should be here. Core sends
    -- it on OrderDelivered, off the same bin row it already reads for the
    -- count and the epoch; this side has no bins table and cannot look it
    -- up.
    lineside_payload_code TEXT NOT NULL DEFAULT '',
    -- lineside_payload_known is the difference between "this carrier is
    -- empty" and "nobody could tell me what this carrier is". Both spell
    -- themselves '' in the column above, and every reader that had to guess
    -- guessed the same way: fall back to the claim, i.e. to the requested
    -- identity, which is the read this column pair exists to end. Ask this
    -- flag, not the emptiness of the string.
    lineside_payload_known INTEGER NOT NULL DEFAULT 0,
    -- lineside_source names who asserted the identity: a delivery envelope,
    -- a person at the window, or the departure that took it away. Not a
    -- ranking -- an automatic source is not more trustworthy than a person
    -- here -- it is so a later reader can tell which question was answered.
    lineside_source TEXT NOT NULL DEFAULT '',
    -- lineside_at is when the identity was established. updated_at cannot
    -- answer it: every count tick moves updated_at, so a payload recorded
    -- once and ticked for an hour reads as an hour old by that clock and is
    -- not. The identity's staleness is a different question from the
    -- count's.
    lineside_at TEXT NOT NULL DEFAULT '',
    remaining_uop_cached INTEGER NOT NULL DEFAULT 0,
    -- pending_uop_delta holds tick counts that arrived while no bin was
    -- bound (the pickup->delivery gap); the next tick with a bound bin
    -- applies current+pending and resets it. Durable across restart.
    pending_uop_delta  INTEGER NOT NULL DEFAULT 0,
    active_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    staged_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    active_pull        INTEGER NOT NULL DEFAULT 1,
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS style_node_claims (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id                INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    core_node_name          TEXT NOT NULL,
    role                    TEXT NOT NULL DEFAULT 'consume',
    swap_mode               TEXT NOT NULL,
    payload_code            TEXT NOT NULL DEFAULT '',
    -- DEAD. Capacity is Core's fact, resolved from payload_catalog on every
    -- claim read (capacity.SQL) and keyed on payload_code above.
    -- Nothing reads or writes this column; it holds whatever it held when the
    -- copies stopped, and it goes on the next rebuildStyleNodeClaims(). It is
    -- not dropped here because a column rebuild is its own change with its own
    -- risk, and because the old numbers are the only record of how far the
    -- copies had drifted.
    uop_capacity            INTEGER NOT NULL DEFAULT 0,
    reorder_point           INTEGER NOT NULL DEFAULT 0,
    -- Cell auto-reorder is opt-IN: a claim must not arm itself. Was DEFAULT 1,
    -- which made "new claim" mean "count-driven order creation is live" before
    -- anyone chose it. Every INSERT in the codebase names this column
    -- explicitly (processes/claims.go UpsertClaim, processes/styles.go
    -- cloneClaimColumns, seeddev/seed_edge.go), so the default is reachable
    -- only by a future hand-written INSERT that omits it — which is exactly
    -- the case that should land off. Fresh databases only: this is
    -- CREATE TABLE IF NOT EXISTS and there is no ALTER for this column, so an
    -- existing plant DB keeps its current table definition and its current
    -- per-claim values either way.
    auto_reorder            INTEGER NOT NULL DEFAULT 0,
    inbound_staging         TEXT NOT NULL DEFAULT '',
    outbound_staging        TEXT NOT NULL DEFAULT '',
    inbound_source          TEXT NOT NULL DEFAULT '',
    outbound_destination    TEXT NOT NULL DEFAULT '',
    allowed_payload_codes   TEXT NOT NULL DEFAULT '',
    auto_request_payload    TEXT NOT NULL DEFAULT '',
    keep_staged             INTEGER NOT NULL DEFAULT 0,
    evacuate_on_changeover  INTEGER NOT NULL DEFAULT 0,
    paired_core_node        TEXT NOT NULL DEFAULT '',
    auto_confirm            INTEGER NOT NULL DEFAULT 0,
    sequence                INTEGER NOT NULL DEFAULT 0,
    lineside_soft_threshold INTEGER NOT NULL DEFAULT 0,
    -- DEAD AS A SWITCH. It armed the no-swap shortcut for a press-index cell
    -- whose next style makes the same part; that shortcut is baked in
    -- (ApplyReuseCompatibleBinsShortcut, 2026-09-09) and reads this column no
    -- more. Kept, and flowspec marks it Unused rather than Forbidden, so the
    -- seven Hopkinsville rows that carry it keep their value instead of being
    -- cleared by the next save.
    reuse_compatible_bins   INTEGER NOT NULL DEFAULT 0,
    -- Which core NODES hold bins that block the tooling change, as a JSON array
    -- of node names ("PLN_001"/"PLN_002"). Same shape and same reasoning as
    -- allowed_payload_codes: a small set on one row rather than a child table,
    -- because an index-paired node has no claim row of its own to hang it on.
    -- Empty = nothing marked = today's behaviour.
    changeover_evac_nodes   TEXT NOT NULL DEFAULT '',
    -- Where a marked position's bin is CLEARED to, when this cell wants it
    -- somewhere other than its ordinary outbound destination. A node OR a group
    -- name; blank means normal routing, which is the default and the common
    -- case. There is no bay: see engine/changeover_tooling.go.
    changeover_evac_destination TEXT NOT NULL DEFAULT '',
    -- What happens to a marked position's bin when its part CARRIES OVER — the same
    -- payload on that position in both styles. "replace" (the default) clears it
    -- like any other marked position and brings a fresh carrier through staging;
    -- "keep_lineside" leaves the bin where it is, because that part does not
    -- have to move for the setup; "outbound_staging" walks the SAME bin to the
    -- cell's outbound staging spot to clear the floor and brings it back on the
    -- tooling-done release. Never consulted when the payloads differ — the bin
    -- has to change anyway.
    changeover_carryover_disposition TEXT NOT NULL DEFAULT 'replace',
    -- Which robot of a press-index pair fetches the replacement carrier.
    -- 0 = today's shape (R1 evacuates and refills); 1 = flipped (R1 evacuates
    -- only, R2 indexes and refills). Describes the cell's hardware, so
    -- UpsertClaim warns when two styles on one press disagree.
    index_robot_supplies    INTEGER NOT NULL DEFAULT 0,
    -- SEER robot-SELECTION hints, carried through to the fleet request.
    -- key_route is a JSON array of map points to prefer passing through, IN
    -- ORDER; key_task is 'load'/'unload'. Both empty on every claim until one
    -- is configured, and empty means the fleet picks freely. A point that does
    -- not resolve terminates the robot's waybill on issue, which is why
    -- ValidateNodeClaim checks each one against Core's synced node list.
    key_route               TEXT NOT NULL DEFAULT '',
    key_task                TEXT NOT NULL DEFAULT '',
    auto_push               INTEGER NOT NULL DEFAULT 0,
    -- UOP-threshold replenishment: tracks how reorder_point was set.
    -- 'legacy' = default, never edited (silent-inert when 0).
    -- 'manual' = engineer typed a value.
    -- 'calculated' = applied from the unified calculator.
    reorder_point_source    TEXT NOT NULL DEFAULT 'legacy',
    -- below_reorder_since is the FALLING EDGE of this claim's level: the
    -- instant remaining UOP first went at-or-below reorder_point, cleared when
    -- it recovers above reorder_point + margin. NULL means "not below".
    --
    -- It is the durable half of the demand episode's hot path. The level is
    -- evaluated on every PLC consume tick, so the timestamp is held in memory
    -- and written through only ON TRANSITION; this column exists because Edge
    -- restarts (systemctl restart shingoedge) more often than anything else in
    -- the system, and an in-memory-only edge means a restart mid-episode loses
    -- it, the next tick mints a duplicate, and the first never closes.
    --
    -- On the CLAIM rather than the episode because it is a per-claim level
    -- observation and the claim is the row the predicate already reads. The
    -- episode itself is keyed per PROCESS and lives in demand_origins_open — see
    -- O8 in demand-origin-design-2026-07-25.md.
    below_reorder_since     TEXT,
    -- ── ATTRIBUTION ──────────────────────────────────────────────────────
    -- Every claim row says who wrote it and from where, because the HMI flow
    -- composer makes the claim table fully open: an operator on the floor
    -- writes the same rows an engineer writes on the desktop. All of these
    -- are SERVER-STAMPED — the client never sends them.
    --
    -- source is the writer: 'admin' (the desktop claim editor), 'hmi' (the
    -- station's flow composer), 'generated' (GenerateStyles), 'cloned'
    -- (CloneStyle). DEFAULT 'admin' is right for every row that already
    -- exists: the desktop was the only writer there was.
    source                  TEXT NOT NULL DEFAULT 'admin',
    -- called_by is the session user (admin) or the station (hmi); '' on
    -- rows that predate attribution and on generated/cloned rows made by
    -- a caller with no session.
    called_by               TEXT NOT NULL DEFAULT '',
    -- updated_at is set on every upsert. NULL means never touched since
    -- attribution landed; it cannot carry a datetime('now') default because
    -- an ALTER cannot add a non-constant default to a populated table.
    updated_at              TEXT,
    -- retired_at replaces DELETE for a claim that changeover history still
    -- references (changeover_node_tasks.from_claim_id / to_claim_id): the row
    -- stays so the history label never renders blank, and list reads skip
    -- it. Re-adding the same (style, node) claim revives the row.
    retired_at              TEXT,
    -- source_preset_id / _version are PROVENANCE: which flow_presets row and
    -- version this claim was expanded from, when it was. Never a drift
    -- oracle — drift is computed from the rows themselves.
    source_preset_id        INTEGER,
    source_preset_version   INTEGER,
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(style_id, core_node_name)
);

-- REMOVED 2026-07-21 — loader_payload_thresholds.
--
-- The Edge-owned per-(loader, payload) UOP threshold table. Core owns that
-- value now (bin_loader_homes.uop_threshold -> BuildDemandRegistryFromAggregate
-- -> demand_registry -> the threshold monitor); the Edge write path terminated
-- in SendClaimSync(), a no-op stub retired when Core took ownership of the
-- loader aggregate. A threshold typed on the Edge page saved cleanly,
-- displayed, and reached nothing.
--
-- THE DDL IS DELETED; THE PHYSICAL TABLE IS DELIBERATELY LEFT ON DISK.
-- It is a POTENTIAL DROP TARGET, not yet dropped, pending sign-off.
--
-- Why it is not dropped yet:
--   1. ROLLBACK, not disk. A pre-sweep binary still lists this table in
--      schema_assert.go's required set, so verifySchema would refuse to boot
--      if the table were gone -- at a plant, with the line down. Leaving the
--      orphan table keeps binary rollback survivable. Code reverts cleanly; a
--      dropped table does not come back.
--   2. The rows are the only record of what engineers actually typed, and are
--      the input to the remediation at any plant whose Core-side thresholds
--      turn out empty. Springfield's are correctly on Core (verified
--      2026-07-21); other plants are unverified.
--
-- Its last code reader was shingo-core/cmd/migrateloaders, which is now deleted.
-- Nothing reads the rows any more; reason 2 above is a HUMAN remediation input,
-- not a program's. Both reasons for keeping it are still good, and neither one
-- is a code dependency.
--
-- Drop it with a normal migration once both plants have run a clean week on a
-- post-sweep binary. See EXEC-LOG-cobalt-kestrel-2284.md (queue item 5) and
-- FOLLOWUPS.md.

-- Core-owned loader config cache. Edge's persistent, last-known-good replica of
-- Core's bin_loaders aggregate, written full-state on each node-list sync from
-- NodeListResponse.Loaders. Persistent so an Edge reboot during a Core partition
-- keeps loaders configured (an in-memory cache would silent-starve). The loader
-- resolvers read it. Keyed by loader_key — the loader's surrogate IDENTITY token
-- ("loader:<id>"); the loader has no node of its own. Positions/payloads carry the
-- real member node NAMES (Edge's key space).
CREATE TABLE IF NOT EXISTS core_loaders (
    loader_key     TEXT    NOT NULL,   -- the loader IDENTITY token ("loader:<id>")
    role           TEXT    NOT NULL,
    name           TEXT    NOT NULL DEFAULT '',
    layout         TEXT    NOT NULL DEFAULT '',
    replenishment  TEXT    NOT NULL DEFAULT '',
    outbound_dest  TEXT    NOT NULL DEFAULT '',
    inbound_source TEXT    NOT NULL DEFAULT '',
    config_gen     INTEGER NOT NULL DEFAULT 0,
    funnel_windows INTEGER NOT NULL DEFAULT 0,  -- 1 = one window at a time; 0 = spread across windows (the default everywhere)
    -- 1 = a changeover commandeers this station's card and names the carrier the
    -- incoming style needs. Core owns it (bin_loaders); this is the mirror.
    changeover_load_directive INTEGER NOT NULL DEFAULT 0,
    -- The bin type a blank CLEAR at this unloader stamps on the carrier it
    -- leaves. Core owns it (bin_loaders.bare_bin_type_id); '' = stamps nothing.
    bare_bin_type_code TEXT NOT NULL DEFAULT '',
    -- 1 = a CLEAR, a PUSH EMPTY or this unloader's own empty-out landing
    -- re-pulls its next full. Core owns it (bin_loaders.auto_push).
    auto_push INTEGER NOT NULL DEFAULT 0,
    synced_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (loader_key)
);
CREATE TABLE IF NOT EXISTS core_loader_positions (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    position_node  TEXT    NOT NULL,   -- the position node NAME (a real node)
    payload_code   TEXT    NOT NULL,
    kind           TEXT    NOT NULL DEFAULT '',  -- 'window' | 'dedicated' (synced from Core; Layout is authoritative if empty)
    home_kind      TEXT    NOT NULL DEFAULT '',  -- 'home' | 'buffer' (synced from Core); '' = pre-field Core, fall back to classifying by empty payload
    min_stock      INTEGER NOT NULL DEFAULT 0,
    uop_threshold  INTEGER NOT NULL DEFAULT 0,
    ordinal        INTEGER NOT NULL DEFAULT 0,   -- where the operator dragged this window; 0 everywhere = nothing arranged, fall back to a number-aware name sort
    PRIMARY KEY (loader_key, position_node)
);
-- What each window can PHYSICALLY take, synced from Core. A window with no rows
-- here takes anything, which is what every window does until somebody says
-- otherwise.
CREATE TABLE IF NOT EXISTS core_loader_window_bin_types (
    loader_key     TEXT NOT NULL,
    position_node  TEXT NOT NULL,
    bin_type_code  TEXT NOT NULL,
    PRIMARY KEY (loader_key, position_node, bin_type_code)
);
-- The loader's declared carrier mix: how many of each type it wants on hand.
-- A PREFERENCE, not a cap — never-2N still bounds how many carriers exist.
CREATE TABLE IF NOT EXISTS core_loader_quotas (
    loader_key    TEXT    NOT NULL,
    bin_type_code TEXT    NOT NULL,
    want          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (loader_key, bin_type_code)
);
CREATE TABLE IF NOT EXISTS core_loader_payloads (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    payload_code   TEXT    NOT NULL,
    min_stock      INTEGER NOT NULL DEFAULT 0,
    uop_threshold  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (loader_key, payload_code)
);


-- REMOVED 2026-09-10 — home_location_loaders.
--
-- Membership set marking a bin loader's layout as "home location" (each payload
-- its own dedicated node) vs the default single window. The layout fact is
-- Core's (bin_loaders.layout → Loader.IsDedicated() → StationNodeView), and the
-- Go surface that mirrored it here — store/home_location_loaders.go, the
-- StyleService method, the API input field and the handler arm — was removed
-- once the claim editor stopped sending the flag. Its last reader anywhere was
-- shingo-core/cmd/migrateloaders, and that command is deleted: its derivation
-- ran at both plants in June 2026 and its input can no longer be produced,
-- because manual_swap has not been authorable in the claim editor since
-- 0ef5b959 (2026-06-22) and is not persistable at all since the retirement.
--
-- THE DDL IS DELETED; THE PHYSICAL TABLE IS DELIBERATELY LEFT ON DISK, the same
-- disposition loader_payload_thresholds got above and for the first of its two
-- reasons: a pre-sweep binary rolled back onto a plant must still boot, and a
-- dropped table does not come back. Dropping it is a data decision and the
-- owner's. Reason 2 there does not apply here — these rows record a layout Core
-- now owns outright, so they are not a remediation input.
--
-- Re-verified 2026-09-10 before deleting the CREATE: no reader and no writer in
-- any of the five modules. What is left is two comments that mention the name
-- (domain/station_view.go, engine/operator_supply_refusal_test.go), both
-- explaining that the copy WAS here and is not any more.
--
-- The previous note said the DDL stayed "so a fresh edge DB keeps the same shape
-- as the plants'". That is exactly the divergence the convergence test records,
-- not a reason — and it is the shape every entry in schemadump/known.go is about.
-- Its sibling operator_driven_loaders is dropped outright in migrate().

-- Every part identity a press's PLC has actually declared, post-debounce.
-- The plant's first persisted record of what the wire carries — see
-- store/plc_catid_observations.go for why that did not exist before. Nothing
-- reads it to make a decision.
CREATE TABLE IF NOT EXISTS plc_catid_observations (
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    plc_name     TEXT    NOT NULL DEFAULT '',
    catid        TEXT    NOT NULL,
    first_seen   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_seen    TEXT    NOT NULL DEFAULT (datetime('now')),
    observations INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (process_id, catid)
);

CREATE TABLE IF NOT EXISTS process_changeovers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id      INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    from_style_id   INTEGER REFERENCES styles(id) ON DELETE SET NULL,
    to_style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'planned',
    called_by       TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL DEFAULT (datetime('now')),
    completed_at    TEXT,
    triggered_by    TEXT NOT NULL DEFAULT '',
    verify_live_catid TEXT NOT NULL DEFAULT '',
    -- origin_id is this changeover's demand episode. Empty until minted.
    --
    -- It goes on THIS row rather than in demand_origins_open because this row
    -- already has exactly the episode's lifetime: one changeover is one
    -- episode (to_style_id is written only at INSERT, nothing re-targets a
    -- row, and cancel-and-redirect cancels this one and inserts a fresh one —
    -- a new row and a new episode). Restart-durable for free.
    origin_id       TEXT NOT NULL DEFAULT '',
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- demand_origins_open — the OPEN cell-kind demand episodes this Edge owns.
--
-- ONE NOUN, BOTH SERVICES. Core's table is demand_origins and holds HISTORY,
-- every episode open or closed; this one holds only what is open right now and
-- deletes a row on close. The "_open" suffix is the whole difference, and
-- saying it in the name is why these are not two words for one thing —
-- sibling_order_id / sibling_order_uuid is what that costs.
--
-- "origin" rather than "episode" because origin_id and origin_class are the
-- names carried on BOTH order tables and across the wire
-- (demand.origin_opened / demand.origin_closed), and those are the hardest
-- names to change later. "Episode" stays as the English word for the period an
-- origin covers — hence episode_key, which identifies one continuous period
-- rather than one row.
--
-- IT HOLDS THE WHOLE ROW, not just the id, because Core is sent STATE rather
-- than events: the close message has to carry kind, direction, expected_orders
-- and the rest, and at close time they are not derivable from anywhere else —
-- they were known at mint and sent, never kept. An earlier version stored four
-- columns and could not have assembled its own close.
--
-- The row is still DELETED on close, and that is what keeps "_open" honest.
-- Delivery after that point belongs to the durable outbox, which retries and
-- dead-letters on its own; holding the row until the message is acked would
-- make this "episodes Edge is still responsible for" and the name a lie.
-- Enqueue first, THEN delete: the reverse order can lose a close, this order
-- can at worst re-send one, and a re-send is a no-op under the revision guard.
--
-- episode_key is
-- "cell|<station>|<process name>|<payload>|<direction>", the same string Core
-- keys demand_origins on, built by one shared helper so the two sides cannot
-- spell it differently.
--
-- WHY A TABLE AND NOT A COLUMN ON style_node_claims (O8, resolved 2026-07-25).
-- The episode is per PROCESS; claims are per node. A/B sequential puts TWO
-- same-payload claims in one process — plants/demo.yaml PRESS-2, PLN_003 and
-- PLN_004, both auto_reorder — and FlipABNode fires RequestNodeMaterial on the
-- paired node. A current_origin_id column on the claim would hold the open
-- episode on claim A where claim B cannot see it, so B's fire would mint a
-- second episode for a place that already has one. The design's own grain rule
-- says the process needs the payload and which position is pulling is not a
-- second demand, so B must JOIN A's episode — which it can only do if the open
-- episode is addressable by the thing they share.
--
-- Rows are DELETED on close; this table holds only what is open. The history
-- lives on Core in demand_origins, which is the service that keeps history.
CREATE TABLE IF NOT EXISTS demand_origins_open (
    episode_key     TEXT PRIMARY KEY,
    origin_id       TEXT NOT NULL,
    -- revision is monotonic per episode and is what Core's upsert compares.
    -- NOT a timestamp: two services cannot agree on one, and the whole point of
    -- the guard is that it settles ordering without agreement.
    revision        INTEGER NOT NULL DEFAULT 1,
    -- The identity fields. Redundant with episode_key, which encodes most of
    -- them, but stored rather than re-parsed: the message is assembled from
    -- this row and a parse that can fail has no business on the emit path.
    kind            TEXT NOT NULL,
    direction       TEXT NOT NULL DEFAULT '',
    trigger_kind    TEXT NOT NULL DEFAULT '',
    trigger_ref     TEXT NOT NULL DEFAULT '',
    -- THE EDGE PROCESS NAME ("SNF2"), NOT THIS DATABASE'S processes.id.
    -- It was INTEGER holding the row id, and Core's demand_origins.process_id
    -- was BIGINT to match — while process_styles.process_id and
    -- PlantClaimsReport.ProcessID, both already deployed, carry the name. Core
    -- therefore held two unjoinable descriptions of one set of processes. Fixed
    -- on both sides (Core migration v63) before any plant ran Core's v59.
    --
    -- NOT A FOREIGN KEY, and it never was one usefully: this row must survive
    -- long enough to assemble its own close message, and Edge runs with
    -- foreign_keys OFF anyway (see store.Open), so the old REFERENCES-shaped
    -- INTEGER bought nothing that the name does not.
    process_id      TEXT NOT NULL DEFAULT '',
    core_node_name  TEXT NOT NULL DEFAULT '',
    payload_code    TEXT NOT NULL DEFAULT '',
    -- Stamped once at the falling edge and never recomputed.
    opened_total    INTEGER NOT NULL DEFAULT 0,
    threshold       INTEGER NOT NULL DEFAULT 0,
    -- NULLABLE: a denominator that is UNKNOWABLE is a different state from one
    -- that is 1, and both 0 and 1 render as a real ratio somebody would draw a
    -- conclusion from. expected_unknown_reason says why, because a NULL with no
    -- reason is indistinguishable from a bug.
    expected_orders INTEGER,
    expected_unknown_reason TEXT NOT NULL DEFAULT '',
    -- rerequest_count is operator pushes that JOINED this episode. Six
    -- re-requests against one demand is a better signal than six demands of
    -- one order each.
    rerequest_count INTEGER NOT NULL DEFAULT 0,
    discretionary   INTEGER NOT NULL DEFAULT 0,
    opened_at       TEXT NOT NULL DEFAULT (datetime('now'))
);

-- supply_refusals_open — a loader operator's standing statement that they
-- cannot fill a call, and the cell's answer to it.
--
-- ONE OPEN ROW PER CARD. The card is the trigger object: the reach-truck
-- operator is standing at (loader_node, payload_code) and that is the whole
-- surface they can see, so it is the key. Both board layouts reduce to it —
-- a shared window renders one card per payload, a dedicated home one card per
-- position, and buildLoaderCard(entry, code) is literally that pair in both.
--
-- OPEN-STATE ONLY, DELETED ON RESOLUTION, following demand_origins_open's
-- reasoning verbatim: "This table holds only what is open… and a row is DELETED
-- on close. The history lives on Core… mirroring it here would be a second copy
-- of the same facts, and the uopCache lesson is that a second copy starts
-- drifting from what it summarises." The normal end is a LOAD at that window for
-- that payload; UNDO is the mis-tap path. Both delete the row.
--
-- NO EXPIRY COLUMN, deliberately. The owner's rule is no re-alert, no expiry, no
-- snooze — it stands until the part is supplied. A row that could age out on a
-- timer would reintroduce exactly the snooze interval that decision removes.
CREATE TABLE IF NOT EXISTS supply_refusals_open (
    loader_node   TEXT NOT NULL,
    payload_code  TEXT NOT NULL,

    -- refused_* is the supplier's half.
    refused_at    TEXT NOT NULL DEFAULT (datetime('now')),
    -- refused_by is STATION-level, not person-level: the loader board carries no
    -- operator identity and calledBy falls back to the station name. Recorded as
    -- a known limitation rather than dressed up as attribution it cannot make.
    refused_by    TEXT NOT NULL DEFAULT '',

    -- ack_* is the customer's half, and ack_at IS NULL is a REAL, QUERYABLE
    -- STATE: told, not answered. Making WAIT the absence of an action would
    -- collapse "the operator chose to keep waiting" and "nobody has looked at
    -- the screen" into one row, and the second of those is the original
    -- complaint this whole project started from.
    ack_at        TEXT,
    ack_choice    TEXT NOT NULL DEFAULT '',   -- '' | 'wait' | 'changeover'
    -- ack_process_id is the process NAME ("SNF2") of the cell that ANSWERED —
    -- matching the demand grain, which keys on the name and not a row id.
    --
    -- Note carefully: this is who answered, NOT who was told. Resolving the
    -- addressee — which cells a loader is currently supplying — is unbuilt;
    -- PayloadsForLoader computes the loader→process mapping and then discards it
    -- into flat string sets. There is deliberately no column for the addressee
    -- until something can write one.
    ack_process_id TEXT NOT NULL DEFAULT '',

    PRIMARY KEY (loader_node, payload_code)
);

CREATE TABLE IF NOT EXISTS changeover_station_tasks (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    operator_station_id   INTEGER NOT NULL REFERENCES operator_stations(id) ON DELETE CASCADE,
    state                 TEXT NOT NULL DEFAULT 'waiting',
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, operator_station_id)
);

CREATE TABLE IF NOT EXISTS changeover_node_tasks (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id      INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    process_node_id            INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    from_claim_id              INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    to_claim_id                INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    situation                  TEXT NOT NULL DEFAULT 'unchanged',
    state                      TEXT NOT NULL DEFAULT 'pending',
    next_material_order_id     INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    old_material_release_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, process_node_id)
);

-- (process_id, started_at DESC) and not process_id alone. Every read of this
-- table filters by process and the two that matter — the composer's "when did
-- this style last run" and the picker's RECENT group — then want the newest
-- rows first. The leading column still serves every plain WHERE process_id = ?
-- the old single-column index served, so this REPLACES it rather than sitting
-- beside it (migrate() drops the old name).
--
-- It is the one composer cost that grows on its own: the history read was
-- 0.28 ms at 600 rows and 12.4 ms at 12,000, on a press that adds rows for
-- the life of the plant.
CREATE INDEX IF NOT EXISTS idx_changeovers_process_started ON process_changeovers(process_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_cst_changeover_id ON changeover_station_tasks(process_changeover_id);
CREATE INDEX IF NOT EXISTS idx_cnt_changeover_id ON changeover_node_tasks(process_changeover_id);

-- Changeover participants — the set of nodes a changeover PHYSICALLY TOUCHES,
-- frozen at plan time. Superset of the task set.
--
-- Why it exists: "which nodes is this changeover about" was being re-derived
-- independently by the release affordance, the cutover gate, and intake
-- gating, and those derivations disagreed. A press-index extension position is
-- traversed by the index motion but owns no task and no order, so a
-- task-keyed answer left it invisible -- and therefore open to unrelated robot
-- dispatch while a bin was about to be placed on it.
--
-- KEYED BY NAME, not process_node_id: an extension position may have no
-- process_nodes row, and it must stay representable and reportable rather than
-- dropped at write time -- reporting it is exactly what the plan-time
-- assertion does. process_node_id is therefore nullable.
--
-- NO ORDER COLUMNS. A participant is a membership fact, not work. The
-- no-phantom-orders rule is expressed as absent columns.
--
-- Written in the SAME TRANSACTION as changeover_node_tasks (see
-- service/changeover_service.go): a changeover with tasks but no participants,
-- or vice versa, is not a state any reader should have to handle.
CREATE TABLE IF NOT EXISTS changeover_participants (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    core_node_name        TEXT    NOT NULL,
    process_node_id       INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL,
    role                  TEXT    NOT NULL,
        -- 'task' | 'indexed_over'
    owning_task_id        INTEGER REFERENCES changeover_node_tasks(id) ON DELETE SET NULL,
    updated_at            TEXT    NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, core_node_name)
);
CREATE INDEX IF NOT EXISTS idx_cp_changeover_id ON changeover_participants(process_changeover_id);
CREATE INDEX IF NOT EXISTS idx_cp_node_name ON changeover_participants(core_node_name);

CREATE TABLE IF NOT EXISTS node_lineside_bucket (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id      INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    pair_key     TEXT NOT NULL DEFAULT '',
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    payload_code TEXT NOT NULL,
    qty          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_lineside_active_unique
    ON node_lineside_bucket(node_id, payload_code)
    WHERE state = 'active';

CREATE INDEX IF NOT EXISTS idx_lineside_node_state
    ON node_lineside_bucket(node_id, state);

CREATE INDEX IF NOT EXISTS idx_lineside_pair_state
    ON node_lineside_bucket(pair_key, state) WHERE pair_key != '';

-- Phase 1d of the UOP bin-as-truth refactor — sequence-id allocator
-- for inventory delta envelopes. One row per (scope_kind, scope_key);
-- next_seq advances atomically when InventoryDeltaReporter flushes a
-- non-zero delta for that scope. Edge guarantees monotonic SequenceID
-- per scope; Core uses inventory_delta_dedup to drop replays.
--
-- scope_kind ∈ {"bin", "bucket"}.
-- scope_key:
--   bin scope    → strconv(BinID)
--   bucket scope → "<NodeID>|<PairKey>|<StyleID>|<PayloadCode>"
-- epoch labels the bin's load-lifecycle for bins (0 for buckets).
-- Per-epoch counters mean a new bin load starts seq=1, immune to
-- prior-epoch counter drift surviving across Edge restarts / DB
-- restores. Old-epoch rows linger harmlessly.
CREATE TABLE IF NOT EXISTS inventory_delta_seq (
    scope_kind TEXT NOT NULL,
    scope_key  TEXT NOT NULL,
    epoch      INTEGER NOT NULL DEFAULT 0,
    next_seq   INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (scope_kind, scope_key, epoch)
);

-- Core's sourceability verdict per (process, style), pushed down on
-- SubjectSourcingState. Persistent so an HMI reload / Edge reboot during a Core
-- partition still shows the last-known changeover picture with no round-trip.
-- status is the gated result ("green" | "yellow" | "red"); missing / at_risk are
-- JSON arrays; reason is Core's generated sentence, displayed verbatim.
CREATE TABLE IF NOT EXISTS sourcing_state (
    process_id  TEXT NOT NULL,
    style_id    TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'green',
    missing     TEXT NOT NULL DEFAULT '[]',
    at_risk     TEXT NOT NULL DEFAULT '[]',
    reason      TEXT NOT NULL DEFAULT '',
    computed_at TEXT NOT NULL DEFAULT '',
    synced_at   TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (process_id, style_id)
);

-- The vendor map's geometry, cached from the node-list sync. Edge's persistent
-- last-known-good copy of Core's scene_points / scene_edges coordinates, the
-- station's cell picture draws from it. Persistent for the core_loaders reason:
-- an Edge that reboots during a Core partition keeps the map it last held.
-- Replaced wholesale, in one transaction, ONLY by a response that carried the
-- whole scene with its revision (domain.NewSceneGeometry); a name-only response
-- — the ordinary tick, when the revision matched — touches nothing here.
--
-- Keyed by instance_name: the node→map join is instance_name WHERE class_name
-- = 'GeneralLocation' (the bin location), never label, which is blank at
-- Springfield. pos_x/pos_y are NOT NULL because (0,0) is a real coordinate and a
-- NULL here would read as one; the write path refuses a point without both.
CREATE TABLE IF NOT EXISTS scene_geometry_points (
    instance_name TEXT NOT NULL,
    class_name    TEXT NOT NULL DEFAULT '',
    pos_x         REAL NOT NULL,
    pos_y         REAL NOT NULL,
    dir           REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (instance_name)
);
-- Keyed by endpoints, which is the identity the wire carries (SceneEdgeInfo has
-- no instance_name). The four control handles are NULL on a straight segment
-- and all present on a curved one — never three of four; the read path drops a
-- partial set back to NULL rather than draw a curve with an invented number.
CREATE TABLE IF NOT EXISTS scene_geometry_edges (
    from_name TEXT NOT NULL,
    to_name   TEXT NOT NULL,
    from_x    REAL NOT NULL,
    from_y    REAL NOT NULL,
    to_x      REAL NOT NULL,
    to_y      REAL NOT NULL,
    ctrl1_x   REAL,
    ctrl1_y   REAL,
    ctrl2_x   REAL,
    ctrl2_y   REAL,
    PRIMARY KEY (from_name, to_name)
);
-- One row (id = 1): the revision the two tables above were cut at, quoted on
-- every node-list request so Core can leave the geometry off. No row = never
-- synced, which loads as nil and asks Core for everything.
CREATE TABLE IF NOT EXISTS scene_geometry_meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    revision  TEXT NOT NULL DEFAULT '',
    synced_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`
