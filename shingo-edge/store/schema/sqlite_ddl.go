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
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS styles (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, name)
);

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

CREATE TABLE IF NOT EXISTS counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id),
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
    UNIQUE(process_id, code)
);

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
    auto_reorder            INTEGER NOT NULL DEFAULT 1,
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
    auto_push               INTEGER NOT NULL DEFAULT 0,
    -- UOP-threshold replenishment: tracks how reorder_point was set.
    -- 'legacy' = default, never edited (silent-inert when 0).
    -- 'manual' = engineer typed a value.
    -- 'calculated' = applied from the unified calculator.
    reorder_point_source    TEXT NOT NULL DEFAULT 'legacy',
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(style_id, core_node_name)
);

-- UOP-threshold replenishment (v6 C-push, opt-in):
--   Per-(loader, payload) trigger value Core's threshold monitor
--   compares against combined in-loop UOP (bins + buckets).
--   PK is (core_node_name, payload_code) — the canonical cross-system
--   identifier already used by style_node_claims, process_nodes, the
--   protocol, and Core's demand_registry. Multi-cell plants sharing a
--   loader end up with one row per binding, not per-Edge variants.
--   A row with replenish_uop_threshold = 0 is treated identically to
--   no row at all — Edge falls back to legacy bin-count, Core never
--   monitors.
CREATE TABLE IF NOT EXISTS loader_payload_thresholds (
    core_node_name          TEXT    NOT NULL,
    payload_code            TEXT    NOT NULL,
    replenish_uop_threshold INTEGER NOT NULL DEFAULT 0,
    source                  TEXT    NOT NULL DEFAULT 'legacy',
        -- 'legacy' | 'manual' | 'calculated'
    safety_factor           REAL    NOT NULL DEFAULT 1.5,
    lookback_days           INTEGER NOT NULL DEFAULT 14,
    threshold_calculated    INTEGER NOT NULL DEFAULT 0,
    threshold_calculated_at TEXT,
    threshold_confidence    TEXT    NOT NULL DEFAULT '',
        -- 'HIGH' | 'MEDIUM' | 'LOW' | ''
    overridden_inputs       TEXT    NOT NULL DEFAULT '',
        -- Comma-separated list of calculator-input field names the
        -- engineer overrode during the last Calculate that produced
        -- the current threshold. Empty when no overrides OR when
        -- source != 'calculated'. Example: 'l2_load_seconds,safety_factor'
    updated_at              TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_by              TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (core_node_name, payload_code)
);

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
    buffer_dest    TEXT    NOT NULL DEFAULT '',
    config_gen     INTEGER NOT NULL DEFAULT 0,
    synced_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (loader_key)
);
CREATE TABLE IF NOT EXISTS core_loader_positions (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    position_node  TEXT    NOT NULL,   -- the position node NAME (a real node)
    payload_code   TEXT    NOT NULL,
    kind           TEXT    NOT NULL DEFAULT '',  -- 'window' | 'dedicated' (synced from Core; Layout is authoritative if empty)
    min_stock      INTEGER NOT NULL DEFAULT 0,
    uop_threshold  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (loader_key, position_node)
);
CREATE TABLE IF NOT EXISTS core_loader_payloads (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    payload_code   TEXT    NOT NULL,
    min_stock      INTEGER NOT NULL DEFAULT 0,
    uop_threshold  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (loader_key, payload_code)
);

-- Transitional bin loaders (operator-driven, manual payload selection):
--   Membership set keyed by core_node_name alone — the only granularity
--   that is 1:1 with the physical loader. A loader shared across
--   processes/styles has multiple style_node_claims and process_nodes rows
--   but one core node, so a per-claim or per-process flag has no defined
--   reduction; this set sidesteps that. A loader in this set is wholly
--   operator-driven: the market-accounting L1 paths (UOP-threshold C-push
--   and legacy bin-count) are suppressed for it, while empties are staged
--   opportunistically (MaybePushLoader) and the operator selects the
--   payload at the board. Edge-only — never plumbed through ClaimSync;
--   Core's threshold monitor already idles for these loaders (their
--   thresholds are 0). Delete the row once supermarket space exists and
--   thresholds are calibrated.
CREATE TABLE IF NOT EXISTS operator_driven_loaders (
    core_node_name TEXT NOT NULL,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_by     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (core_node_name)
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
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
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
`
