CREATE INDEX idx_changeovers_process_id ON process_changeovers(process_id);

CREATE INDEX idx_cnt_changeover_id ON changeover_node_tasks(process_changeover_id);

CREATE INDEX idx_counter_snapshots_anomaly ON counter_snapshots(anomaly, operator_confirmed)
    WHERE anomaly IS NOT NULL AND operator_confirmed = 0;

CREATE INDEX idx_cp_changeover_id ON changeover_participants(process_changeover_id);

CREATE INDEX idx_cp_node_name ON changeover_participants(core_node_name);

CREATE INDEX idx_cst_changeover_id ON changeover_station_tasks(process_changeover_id);

CREATE UNIQUE INDEX idx_lineside_active_unique
    ON node_lineside_bucket(node_id, part_number)
    WHERE state = 'active';

CREATE INDEX idx_lineside_node_state
    ON node_lineside_bucket(node_id, state);

CREATE INDEX idx_lineside_pair_state
    ON node_lineside_bucket(pair_key, state) WHERE pair_key != '';

CREATE INDEX idx_order_history_order_id ON order_history(order_id);

CREATE INDEX idx_orders_process_node_id ON orders(process_node_id);

CREATE INDEX idx_orders_source_node ON orders(source_node);

CREATE INDEX idx_orders_status ON orders(status);

CREATE INDEX idx_orders_uuid ON orders(uuid);

CREATE INDEX idx_outbox_pending ON outbox(sent_at) WHERE sent_at IS NULL;

CREATE UNIQUE INDEX idx_process_nodes_process_code_live
		ON process_nodes(process_id, code) WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX idx_process_nodes_process_core_name
				ON process_nodes(process_id, core_node_name)
				WHERE core_node_name <> '' AND deleted_at IS NULL;

CREATE UNIQUE INDEX idx_styles_process_name_live
			ON styles(process_id, name) WHERE deleted_at IS NULL;

CREATE TABLE admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE changeover_node_tasks (
    id                         INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id      INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    process_node_id            INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    from_claim_id              INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    to_claim_id                INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    situation                  TEXT NOT NULL DEFAULT 'unchanged',
    state                      TEXT NOT NULL DEFAULT 'pending',
    next_material_order_id     INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    old_material_release_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at                 TEXT NOT NULL DEFAULT (datetime('now')), skip_note TEXT NOT NULL DEFAULT '',
    UNIQUE(process_changeover_id, process_node_id)
);

CREATE TABLE changeover_participants (
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

CREATE TABLE changeover_station_tasks (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    process_changeover_id INTEGER NOT NULL REFERENCES process_changeovers(id) ON DELETE CASCADE,
    operator_station_id   INTEGER NOT NULL REFERENCES operator_stations(id) ON DELETE CASCADE,
    state                 TEXT NOT NULL DEFAULT 'waiting',
    updated_at            TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_changeover_id, operator_station_id)
);

CREATE TABLE core_loader_payloads (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    payload_code   TEXT    NOT NULL,
    min_stock      INTEGER NOT NULL DEFAULT 0,
    uop_threshold  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (loader_key, payload_code)
);

CREATE TABLE core_loader_positions (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    position_node  TEXT    NOT NULL,   -- the position node NAME (a real node)
    payload_code   TEXT    NOT NULL,
    kind           TEXT    NOT NULL DEFAULT '',  -- 'window' | 'dedicated' (synced from Core; Layout is authoritative if empty)
    min_stock      INTEGER NOT NULL DEFAULT 0,
    uop_threshold  INTEGER NOT NULL DEFAULT 0,
    ordinal        INTEGER NOT NULL DEFAULT 0,   -- where the operator dragged this window; 0 everywhere = nothing arranged, fall back to a number-aware name sort
    PRIMARY KEY (loader_key, position_node)
);

CREATE TABLE core_loader_quotas (
    loader_key    TEXT    NOT NULL,
    bin_type_code TEXT    NOT NULL,
    want          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (loader_key, bin_type_code)
);

CREATE TABLE core_loader_window_bin_types (
    loader_key     TEXT NOT NULL,
    position_node  TEXT NOT NULL,
    bin_type_code  TEXT NOT NULL,
    PRIMARY KEY (loader_key, position_node, bin_type_code)
);

CREATE TABLE core_loaders (
    loader_key     TEXT    NOT NULL,   -- the loader IDENTITY token ("loader:<id>")
    role           TEXT    NOT NULL,
    name           TEXT    NOT NULL DEFAULT '',
    layout         TEXT    NOT NULL DEFAULT '',
    replenishment  TEXT    NOT NULL DEFAULT '',
    outbound_dest  TEXT    NOT NULL DEFAULT '',
    inbound_source TEXT    NOT NULL DEFAULT '',
    buffer_dest    TEXT    NOT NULL DEFAULT '',
    config_gen     INTEGER NOT NULL DEFAULT 0,
    funnel_windows INTEGER NOT NULL DEFAULT 0,  -- 1 = one window at a time; 0 = spread across windows (the default everywhere)
    synced_at      TEXT    NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (loader_key)
);

CREATE TABLE counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id) ON DELETE CASCADE,
    count_value        INTEGER NOT NULL,
    delta              INTEGER NOT NULL DEFAULT 0,
    anomaly            TEXT,
    operator_confirmed INTEGER NOT NULL DEFAULT 0,
    recorded_at        TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE daily_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL,
    style_id     INTEGER NOT NULL,
    count_date   TEXT NOT NULL,
    total        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date)
);

CREATE TABLE demand_origins_open (
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

CREATE TABLE home_location_loaders (
    core_node_name TEXT NOT NULL,
    updated_at     TEXT NOT NULL DEFAULT (datetime('now')),
    updated_by     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (core_node_name)
);

CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);

CREATE TABLE inventory_delta_seq (
    scope_kind TEXT NOT NULL,
    scope_key  TEXT NOT NULL,
    epoch      INTEGER NOT NULL DEFAULT 0,
    next_seq   INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (scope_kind, scope_key, epoch)
);

CREATE TABLE node_lineside_bucket (
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

CREATE TABLE operator_stations (
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

CREATE TABLE order_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id   INTEGER NOT NULL REFERENCES orders(id),
    old_status TEXT NOT NULL,
    new_status TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE orders (
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
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE outbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    topic      TEXT NOT NULL,
    payload    BLOB NOT NULL,
    msg_type   TEXT NOT NULL DEFAULT '',
    retries    INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    sent_at    TEXT
);

CREATE TABLE payload_catalog (
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

CREATE TABLE process_changeovers (
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

CREATE TABLE process_node_runtime_states (
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

CREATE TABLE process_nodes (
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

CREATE TABLE processes (
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

CREATE TABLE reporting_points (
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

CREATE TABLE shifts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL DEFAULT '',
    shift_number INTEGER NOT NULL UNIQUE,
    start_time   TEXT NOT NULL,
    end_time     TEXT NOT NULL
);

CREATE TABLE sourcing_state (
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

CREATE TABLE style_node_claims (
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
    created_at              TEXT NOT NULL DEFAULT (datetime('now')), staging_node TEXT NOT NULL DEFAULT '', release_node TEXT NOT NULL DEFAULT '', inbound_source_node TEXT NOT NULL DEFAULT '', inbound_source_node_group TEXT NOT NULL DEFAULT '', outbound_source_node TEXT NOT NULL DEFAULT '', outbound_source_node_group TEXT NOT NULL DEFAULT '', outbound_source TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT 'loader', second_paired_core_node TEXT NOT NULL DEFAULT '',
    UNIQUE(style_id, core_node_name)
);

CREATE TABLE styles (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id     INTEGER REFERENCES processes(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    expected_catid TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL DEFAULT (datetime('now')),
    deleted_at     TEXT
);

CREATE TABLE supply_refusals_open (
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
