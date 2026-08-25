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
-- the rollups live in hourly_counts (90 days) and daily_counts (permanent).
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

-- hourly_counts is the MIDDLE rung of the counting ladder, and as of 2026-07 it
-- is the only one with a bounded life:
--
--     counter_snapshots  raw, one row per poll   14 days  (counters.SnapshotRetention)
--     hourly_counts      per process/style/hour  90 days  (counters.HourlyRetention)
--     daily_counts       per process/style/day   permanent
--
-- The 90-day window is only safe because daily_counts already holds the total,
-- and counters.PurgeRolledUpHourly enforces that literally rather than by
-- convention — it refuses to delete an hour whose day has no daily_counts row.
CREATE TABLE IF NOT EXISTS hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);

-- daily_counts is the permanent end of the ladder: one row per process, style
-- and calendar date, ~1,900 rows a year at Springfield's measured rate. Written
-- only by counters.RollUpDaily, never deleted.
--
-- IT CARRIES NO FOREIGN KEYS, AND THAT IS THE DESIGN, NOT AN OVERSIGHT. Its
-- sibling hourly_counts declares ON DELETE CASCADE on both process_id and
-- style_id. Copying that here would mean:
--
--  1. A HARD DELETE OF A PROCESS DESTROYS THE PERMANENT RECORD. Styles are
--     soft-deleted now (store/processes/styles.go, DeleteStyle sets deleted_at)
--     precisely so that retiring a part number stops destroying what it
--     counted. processes is still a hard DELETE
--     (store/processes/processes.go, DeleteProcess), so a CASCADE from there
--     would take every year of daily totals with it on a routine config
--     action. That is the same defect one table over.
--
--  2. THE ALTERNATIVE — RESTRICT — REBUILDS THE TRAP DIRECTLY ABOVE. A NO
--     ACTION clause on a NOT NULL child column is exactly what made 6 of 8
--     style deletions impossible on the Springfield dump. A new child table
--     with a restricting edge is that bug with a different name.
--
--  3. THE ROLLUP COULD THEN FAIL ON DATA THAT ALREADY EXISTS. The Springfield
--     database of 2026-07-27 carries 457 hourly_counts rows whose style row is
--     gone (styles 8, 10, 11, 32 — measured on edge-golden.db). After
--     RUNBOOK-0.5's ordered data plan one survives: id 144030, style 32,
--     count_date 2026-06-25. With foreign_keys(1) enabled and an FK here, the
--     rollup's INSERT for that row is refused and the whole retention pass
--     errors — a stale row would become a broken background job.
--
-- The integrity that FKs would buy is bought instead by having exactly one
-- writer: RollUpDaily aggregates rows that are already in hourly_counts, so
-- there is no ingress that could invent a process_id or style_id. And a table
-- with no FK clauses contributes zero rows to PRAGMA foreign_key_check, so it
-- cannot regress the enforcement gate that RUNBOOK-0.5 exists to open.
CREATE TABLE IF NOT EXISTS daily_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL,
    style_id     INTEGER NOT NULL,
    count_date   TEXT NOT NULL,
    total        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date)
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

CREATE TABLE IF NOT EXISTS process_node_runtime_states (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id    INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    active_claim_id    INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    active_bin_id      INTEGER,
    -- active_bin_epoch mirrors Core's bins.delta_epoch for the bin
    -- currently active at this slot. Edge stamps every outgoing
    -- BinUOPDelta with the value so Core's epoch-aware dedup accepts
    -- the delta. Populated on LoadBin response, FetchNodeBins refresh,
    -- and bin-arrival events; survives Edge restart so post-restart
    -- ticks don't emit at epoch=0 against a bin already at epoch>=1.
    active_bin_epoch   INTEGER NOT NULL DEFAULT 0,
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
    reuse_compatible_bins   INTEGER NOT NULL DEFAULT 0,
    -- Which press-index positions hold bins that block the tooling change, as a
    -- JSON array of position keys ("front"/"paired"/"second"). Same shape and same
    -- reasoning as allowed_payload_codes: a small set on one row rather than a
    -- child table, because the back positions have no claim rows of their own.
    -- Empty = no position marked = today's behaviour.
    changeover_evac_positions   TEXT NOT NULL DEFAULT '',
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
    -- Turns a loader's card into a loading instruction during a changeover:
    -- which empty bin type the changing-over cells are waiting for. Off by
    -- default; see NodeClaim.ChangeoverLoadDirective for why it lives here
    -- rather than on the Core loader aggregate.
    changeover_load_directive INTEGER NOT NULL DEFAULT 0,
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


-- home_location_loaders — membership set marking a bin loader's layout as
-- "home location" (each payload its own dedicated node) vs the default single
-- window. Orthogonal to operator_driven_loaders (type vs layout). See
-- store/home_location_loaders.go.
CREATE TABLE IF NOT EXISTS home_location_loaders (
    core_node_name TEXT NOT NULL,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_by     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (core_node_name)
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

CREATE INDEX IF NOT EXISTS idx_changeovers_process_id ON process_changeovers(process_id);
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
    part_number  TEXT NOT NULL,
    qty          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_lineside_active_unique
    ON node_lineside_bucket(node_id, part_number)
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
--   bucket scope → "<NodeID>|<PairKey>|<StyleID>|<PartNumber>"
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
`
