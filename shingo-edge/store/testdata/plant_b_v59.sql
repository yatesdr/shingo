-- An Edge schema at v59 as a plant holds it: sqlite_master text of shingo tables, tables before indexes.
-- No rows. Read by plant_shape_test.go; never edited by hand.

CREATE TABLE admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE auto_reorder_flips (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id  INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    turned_on   INTEGER NOT NULL,
    by          TEXT NOT NULL,
    source      TEXT NOT NULL,
    at          TEXT NOT NULL
);
CREATE TABLE bin_returns (
		order_uuid   TEXT PRIMARY KEY,
		bin_label    TEXT NOT NULL DEFAULT '',
		payload_code TEXT NOT NULL DEFAULT '',
		state        TEXT NOT NULL,
		destination  TEXT NOT NULL DEFAULT '',
		reason       TEXT NOT NULL DEFAULT '',
		updated_at   TEXT NOT NULL);
CREATE TABLE catid_holds (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id      INTEGER NOT NULL,
    catid           TEXT    NOT NULL,
    process_node_id INTEGER NOT NULL DEFAULT 0,
    payload_code    TEXT    NOT NULL DEFAULT '',
    reason          TEXT    NOT NULL,
    qty             INTEGER NOT NULL,
    strokes         INTEGER NOT NULL,
    first_tick_at   TEXT    NOT NULL,
    last_tick_at    TEXT    NOT NULL,
    resolved_at     TEXT,
    resolution      TEXT    NOT NULL DEFAULT '',
    resolved_by     TEXT    NOT NULL DEFAULT ''
, last_bin_id INTEGER NOT NULL DEFAULT 0, last_declared INTEGER NOT NULL DEFAULT 0, last_left_at TEXT NOT NULL DEFAULT '');
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
    PRIMARY KEY (loader_key, payload_code)
);
CREATE TABLE core_loader_positions (
    loader_key     TEXT    NOT NULL,   -- the owning loader's identity token
    position_node  TEXT    NOT NULL,   -- the position node NAME (a real node)
    payload_code   TEXT    NOT NULL,
    min_stock      INTEGER NOT NULL DEFAULT 0,
    ordinal INTEGER NOT NULL DEFAULT 0, home_kind TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (loader_key, position_node)
);
CREATE TABLE core_loaders (
    loader_key     TEXT    NOT NULL,   -- the loader IDENTITY token ("loader:<id>")
    role           TEXT    NOT NULL,
    name           TEXT    NOT NULL DEFAULT '',
    layout         TEXT    NOT NULL DEFAULT '',
    replenishment  TEXT    NOT NULL DEFAULT '',
    outbound_dest  TEXT    NOT NULL DEFAULT '',
    inbound_source TEXT    NOT NULL DEFAULT '',
    config_gen     INTEGER NOT NULL DEFAULT 0,
    synced_at      TEXT    NOT NULL DEFAULT (datetime('now')), funnel_windows INTEGER NOT NULL DEFAULT 0, auto_push INTEGER NOT NULL DEFAULT 0, leaves_bare INTEGER NOT NULL DEFAULT 0, pulled_directly INTEGER NOT NULL DEFAULT 0, board_hides_idle INTEGER NOT NULL DEFAULT 0, board_shows_all_parts INTEGER NOT NULL DEFAULT 0, takes_bare INTEGER NOT NULL DEFAULT 0, no_clear_partials INTEGER NOT NULL DEFAULT 0, edge_station_uid TEXT NOT NULL DEFAULT '', edge_display_name TEXT NOT NULL DEFAULT '',
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
, recorded_ms INTEGER, process_id INTEGER, style_id INTEGER, catid TEXT NOT NULL DEFAULT '');
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
    reorder_point       INTEGER NOT NULL DEFAULT 0,
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
CREATE TABLE feed_copy (
					feed         TEXT PRIMARY KEY,
					digest       TEXT NOT NULL,
					body         TEXT,
					received_at  TEXT,
					confirmed_at TEXT
				);
CREATE TABLE flow_presets (
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
CREATE TABLE hourly_counts (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    bucket_start INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, bucket_start)
);
CREATE TABLE "hourly_counts_local_legacy" (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    style_id     INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    count_date   TEXT NOT NULL,
    hour         INTEGER NOT NULL,
    delta        INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT DEFAULT (datetime('now')),
    UNIQUE(process_id, style_id, count_date, hour)
);
CREATE TABLE "inventory_delta_seq" (
			scope_kind TEXT NOT NULL,
			scope_key  TEXT NOT NULL,
			epoch      INTEGER NOT NULL DEFAULT 0,
			next_seq   INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL DEFAULT (datetime('now')), net INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (scope_kind, scope_key, epoch)
		);
CREATE TABLE line_counters (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id      INTEGER NOT NULL,
    label           TEXT    NOT NULL DEFAULT '',
    plc_name        TEXT    NOT NULL,
    tag_name        TEXT    NOT NULL,
    last_count      INTEGER NOT NULL DEFAULT 0,
    last_poll_at    TEXT,
    warlink_managed INTEGER NOT NULL DEFAULT 0, tag_name_2 TEXT NOT NULL DEFAULT '', last_count_2 INTEGER NOT NULL DEFAULT 0, last_poll_at_2 TEXT, warlink_managed_2 INTEGER NOT NULL DEFAULT 0,
    UNIQUE (plc_name, tag_name)
);
CREATE TABLE node_lineside_bucket (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    node_id      INTEGER NOT NULL REFERENCES process_nodes(id) ON DELETE CASCADE,
    payload_code TEXT NOT NULL,
    qty          INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'stranded')),
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (node_id, payload_code, state)
);
CREATE TABLE operator_stations (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id         INTEGER NOT NULL REFERENCES "processes_legacy"(id) ON DELETE CASCADE,
    code               TEXT NOT NULL,
    name               TEXT NOT NULL,
    note               TEXT NOT NULL DEFAULT '',
    area_label         TEXT NOT NULL DEFAULT '',
    sequence           INTEGER NOT NULL DEFAULT 0,
    controller_node_id TEXT NOT NULL DEFAULT '',
    enabled            INTEGER NOT NULL DEFAULT 1,
    health_status      TEXT NOT NULL DEFAULT 'offline',
    last_seen_at       TEXT,
    created_at         TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at         TEXT NOT NULL DEFAULT (datetime('now')), core_loader_key TEXT NOT NULL DEFAULT '',
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
    payload_id      INTEGER REFERENCES payloads(id),
    retrieve_empty  INTEGER NOT NULL DEFAULT 1,
    quantity        INTEGER NOT NULL DEFAULT 0,
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
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
, steps_json TEXT NOT NULL DEFAULT '', staged_expire_at TEXT, process_node_id INTEGER REFERENCES process_nodes(id) ON DELETE SET NULL, payload_code TEXT NOT NULL DEFAULT '', bin_uop_remaining INTEGER, bin_id INTEGER, sibling_order_id INTEGER REFERENCES orders(id) ON DELETE SET NULL, queue_reason TEXT NOT NULL DEFAULT '', queue_code TEXT NOT NULL DEFAULT '', authored_by TEXT NOT NULL DEFAULT 'edge', payload_desc TEXT NOT NULL DEFAULT '', origin_id TEXT NOT NULL DEFAULT '', origin_class TEXT NOT NULL DEFAULT '', fault_since TEXT NOT NULL DEFAULT '', fault_deadline TEXT NOT NULL DEFAULT '', fault_notice_after_s INTEGER NOT NULL DEFAULT 0, fault_ref TEXT NOT NULL DEFAULT '', departed_at TEXT, cell_left_at TEXT, pending_intent TEXT NOT NULL DEFAULT '', station_wait INTEGER, wait_kind TEXT NOT NULL DEFAULT '', release_facts TEXT, release_intent TEXT NOT NULL DEFAULT '', release_held TEXT NOT NULL DEFAULT '', want TEXT NOT NULL DEFAULT '', robot_id TEXT NOT NULL DEFAULT '', staged_at TEXT, core_version INTEGER NOT NULL DEFAULT 0);
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
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    code         TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    uop_capacity INTEGER NOT NULL DEFAULT 0,
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
, catid TEXT NOT NULL DEFAULT '', manifest TEXT NOT NULL DEFAULT '[]');
CREATE TABLE payloads (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    job_style_id     INTEGER NOT NULL REFERENCES "styles_legacy"(id) ON DELETE CASCADE,
    location         TEXT NOT NULL,
    staging_node     TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    manifest         TEXT NOT NULL DEFAULT '{}',
    multiplier       REAL NOT NULL DEFAULT 1,
    production_units INTEGER NOT NULL DEFAULT 0,
    remaining        INTEGER NOT NULL DEFAULT 0,
    reorder_point    INTEGER NOT NULL DEFAULT 0,
    reorder_qty      INTEGER NOT NULL DEFAULT 1,
    retrieve_empty   INTEGER NOT NULL DEFAULT 1,
    status           TEXT NOT NULL DEFAULT 'active',
    payload_code     TEXT NOT NULL DEFAULT '',
    auto_reorder     INTEGER NOT NULL DEFAULT 1,
    role             TEXT NOT NULL DEFAULT 'consume',
    auto_remove_empties INTEGER NOT NULL DEFAULT 0,
    auto_order_empties  INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at       TEXT NOT NULL DEFAULT (datetime('now')), blueprint_code TEXT NOT NULL DEFAULT '',
    UNIQUE(job_style_id, location)
);
CREATE TABLE plc_catid_observations (
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    plc_name     TEXT    NOT NULL DEFAULT '',
    catid        TEXT    NOT NULL,
    first_seen   TEXT    NOT NULL DEFAULT (datetime('now')),
    last_seen    TEXT    NOT NULL DEFAULT (datetime('now')),
    observations INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (process_id, catid)
);
CREATE TABLE process_changeovers (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id      INTEGER NOT NULL REFERENCES "processes_legacy"(id) ON DELETE CASCADE,
    from_style_id   INTEGER REFERENCES "styles_legacy"(id) ON DELETE SET NULL,
    to_style_id     INTEGER NOT NULL REFERENCES "styles_legacy"(id) ON DELETE CASCADE,
    state           TEXT NOT NULL DEFAULT 'planned',
    called_by       TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    started_at      TEXT NOT NULL DEFAULT (datetime('now')),
    completed_at    TEXT,
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
, triggered_by TEXT NOT NULL DEFAULT '', verify_live_catid TEXT NOT NULL DEFAULT '', origin_id TEXT NOT NULL DEFAULT '', start_catid TEXT NOT NULL DEFAULT '', end_catid TEXT NOT NULL DEFAULT '');
CREATE TABLE process_groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE process_node_runtime_states (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    process_node_id    INTEGER NOT NULL UNIQUE REFERENCES process_nodes(id) ON DELETE CASCADE,
    active_claim_id    INTEGER REFERENCES style_node_claims(id) ON DELETE SET NULL,
    active_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    staged_order_id    INTEGER REFERENCES orders(id) ON DELETE SET NULL,
    updated_at         TEXT NOT NULL DEFAULT (datetime('now'))
, active_pull INTEGER NOT NULL DEFAULT 1, remaining_uop_cached INTEGER NOT NULL DEFAULT 0, active_bin_id INTEGER, active_bin_epoch INTEGER NOT NULL DEFAULT 0, pending_uop_delta INTEGER NOT NULL DEFAULT 0, lineside_payload_code TEXT NOT NULL DEFAULT '', lineside_payload_known INTEGER NOT NULL DEFAULT 0, lineside_source TEXT NOT NULL DEFAULT '', lineside_at TEXT NOT NULL DEFAULT '', last_bin_id INTEGER, last_bin_epoch INTEGER NOT NULL DEFAULT 0, adj_bin_id INTEGER, adj_bin_epoch INTEGER NOT NULL DEFAULT 0, adj_as_of_seq INTEGER NOT NULL DEFAULT 0, keep_staged_resumed_at TEXT NOT NULL DEFAULT '', pending_uop_payload TEXT NOT NULL DEFAULT '', released_empty INTEGER NOT NULL DEFAULT 0, placed_seq INTEGER, released_bin_id INTEGER, released_count INTEGER NOT NULL DEFAULT 0, released_at TEXT, hold_stopped INTEGER NOT NULL DEFAULT 0, reorder_hold TEXT NOT NULL DEFAULT '', reorder_hold_at TEXT NOT NULL DEFAULT '', reorder_hold_note TEXT NOT NULL DEFAULT '', reorder_go_at TEXT NOT NULL DEFAULT '');
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
    deleted_at          TEXT
, curtain_enabled INTEGER NOT NULL DEFAULT 0, curtain_plc_name TEXT NOT NULL DEFAULT '', curtain_tag_name TEXT NOT NULL DEFAULT '', curtain_safe_value INTEGER, line_counter_id INTEGER, loader_window INTEGER NOT NULL DEFAULT 0);
CREATE TABLE process_payloads (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    process_id   INTEGER NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
    payload_code TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(process_id, payload_code)
);
CREATE TABLE process_routing_nodes (
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
CREATE TABLE processes (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_style_id     INTEGER REFERENCES "styles_legacy"(id) ON DELETE SET NULL,
    target_style_id     INTEGER REFERENCES "styles_legacy"(id) ON DELETE SET NULL,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
, counter_plc_name TEXT NOT NULL DEFAULT '', counter_tag_name TEXT NOT NULL DEFAULT '', counter_enabled INTEGER NOT NULL DEFAULT 0, changeover_auto_arm TEXT NOT NULL DEFAULT 'auto', group_id INTEGER REFERENCES process_groups(id) ON DELETE SET NULL, flow_composer_enabled INTEGER NOT NULL DEFAULT 0, identity_tag_name TEXT NOT NULL DEFAULT '', identity_none INTEGER NOT NULL DEFAULT 0, identity_warlink_managed INTEGER NOT NULL DEFAULT 0, auto_reorder_switch_hmi INTEGER NOT NULL DEFAULT 0);
CREATE TABLE "processes_legacy" (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    active_job_style_id INTEGER REFERENCES "styles_legacy"(id) ON DELETE SET NULL,
    created_at          TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE production_tick_cursor (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    last_id    INTEGER NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE release_paperwork (
		order_id     INTEGER NOT NULL,
		kind         TEXT NOT NULL,
		applied_json TEXT NOT NULL DEFAULT '{}',
		updated_at   TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (order_id, kind));
CREATE TABLE reporting_points (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    style_id        INTEGER NOT NULL REFERENCES styles(id) ON DELETE CASCADE,
    plc_name        TEXT NOT NULL,
    tag_name        TEXT NOT NULL,
    last_count      INTEGER NOT NULL DEFAULT 0,
    last_poll_at    TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    warlink_managed INTEGER NOT NULL DEFAULT 0, last_part_at TEXT,
    UNIQUE(plc_name, tag_name)
);
CREATE TABLE scene_geometry_edges (
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
CREATE TABLE scene_geometry_meta (
    id        INTEGER PRIMARY KEY CHECK (id = 1),
    revision  TEXT NOT NULL DEFAULT '',
    synced_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE scene_geometry_points (
    instance_name TEXT NOT NULL,
    class_name    TEXT NOT NULL DEFAULT '',
    pos_x         REAL NOT NULL,
    pos_y         REAL NOT NULL,
    dir           REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (instance_name)
);
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
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
    synced_at   TEXT NOT NULL DEFAULT (datetime('now')), absent TEXT NOT NULL DEFAULT '[]',
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
    auto_reorder            INTEGER NOT NULL DEFAULT 0,
    inbound_staging         TEXT NOT NULL DEFAULT '',
    outbound_staging        TEXT NOT NULL DEFAULT '',
    inbound_source          TEXT NOT NULL DEFAULT '',
    outbound_destination    TEXT NOT NULL DEFAULT '',
    containment_destination TEXT NOT NULL DEFAULT '',
    allowed_payload_codes   TEXT NOT NULL DEFAULT '',
    keep_staged_node        TEXT NOT NULL DEFAULT '',
    paired_core_node        TEXT NOT NULL DEFAULT '',
    auto_confirm            INTEGER NOT NULL DEFAULT 0,
    sequence                INTEGER NOT NULL DEFAULT 0,
    lineside_soft_threshold INTEGER NOT NULL DEFAULT 0,
    reuse_compatible_bins   INTEGER NOT NULL DEFAULT 0,
    auto_push               INTEGER NOT NULL DEFAULT 0,
    below_reorder_since     TEXT,
    created_at              TEXT NOT NULL DEFAULT (datetime('now')),
    staging_node            TEXT NOT NULL DEFAULT '',
    release_node            TEXT NOT NULL DEFAULT '',
    inbound_source_node     TEXT NOT NULL DEFAULT '',
    inbound_source_node_group TEXT NOT NULL DEFAULT '',
    outbound_source_node    TEXT NOT NULL DEFAULT '',
    outbound_source_node_group TEXT NOT NULL DEFAULT '',
    outbound_source         TEXT NOT NULL DEFAULT '',
    mode                    TEXT NOT NULL DEFAULT 'loader',
    second_paired_core_node TEXT NOT NULL DEFAULT '',
    changeover_evac_nodes   TEXT NOT NULL DEFAULT '',
    key_route               TEXT NOT NULL DEFAULT '',
    key_task                TEXT NOT NULL DEFAULT '',
    changeover_carryover_disposition TEXT NOT NULL DEFAULT 'replace',
    source                  TEXT NOT NULL DEFAULT 'admin',
    called_by               TEXT NOT NULL DEFAULT '',
    updated_at              TEXT,
    retired_at              TEXT,
    source_preset_id        INTEGER,
    source_preset_version   INTEGER, curtain_required INTEGER NOT NULL DEFAULT 0, partial_destination TEXT NOT NULL DEFAULT '', strict_pack INTEGER NOT NULL DEFAULT 0, pack_tolerance_pct INTEGER NOT NULL DEFAULT 0, refill_by TEXT NOT NULL DEFAULT 'clearing_robot', hold_limit INTEGER NOT NULL DEFAULT 0,
    UNIQUE(style_id, core_node_name)
);
CREATE TABLE style_node_claims_quarantine(
  id INT,
  style_id INT,
  core_node_name TEXT,
  role TEXT,
  swap_mode TEXT,
  payload_code TEXT,
  uop_capacity INT,
  reorder_point INT,
  auto_reorder INT,
  inbound_staging TEXT,
  outbound_staging TEXT,
  inbound_source TEXT,
  outbound_destination TEXT,
  allowed_payload_codes TEXT,
  keep_staged INT,
  paired_core_node TEXT,
  auto_confirm INT,
  sequence INT,
  lineside_soft_threshold INT,
  reuse_compatible_bins INT,
  auto_push INT,
  below_reorder_since TEXT,
  created_at TEXT,
  staging_node TEXT,
  release_node TEXT,
  inbound_source_node TEXT,
  inbound_source_node_group TEXT,
  outbound_source_node TEXT,
  outbound_source_node_group TEXT,
  outbound_source TEXT,
  mode TEXT,
  second_paired_core_node TEXT,
  changeover_evac_nodes TEXT,
  key_route TEXT,
  key_task TEXT,
  changeover_carryover_disposition TEXT,
  source TEXT,
  called_by TEXT,
  updated_at TEXT,
  retired_at TEXT,
  source_preset_id INT,
  source_preset_version INT
, quarantined_at TEXT NOT NULL DEFAULT '', quarantined_reason TEXT NOT NULL DEFAULT '', quarantined_sync_loader_count INTEGER NOT NULL DEFAULT 0, quarantined_sync_loader_keys TEXT NOT NULL DEFAULT '', containment_destination TEXT, keep_staged_node TEXT, curtain_required INTEGER, partial_destination TEXT, strict_pack INT, pack_tolerance_pct INT, refill_by TEXT, hold_limit INT);
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
CREATE INDEX idx_auto_reorder_flips_process ON auto_reorder_flips(process_id, id DESC);
CREATE UNIQUE INDEX idx_catid_holds_open
    ON catid_holds(process_id, catid, process_node_id, payload_code, last_bin_id, last_left_at) WHERE resolved_at IS NULL;
CREATE INDEX idx_changeovers_process_started ON process_changeovers(process_id, started_at DESC);
CREATE INDEX idx_cnt_changeover_id ON changeover_node_tasks(process_changeover_id);
CREATE INDEX idx_counter_snapshots_anomaly ON counter_snapshots(anomaly, operator_confirmed)
    WHERE anomaly IS NOT NULL AND operator_confirmed = 0;
CREATE INDEX idx_cp_changeover_id ON changeover_participants(process_changeover_id);
CREATE INDEX idx_cp_node_name ON changeover_participants(core_node_name);
CREATE INDEX idx_cst_changeover_id ON changeover_station_tasks(process_changeover_id);
CREATE INDEX idx_order_history_order_id ON order_history(order_id);
CREATE INDEX idx_orders_live_delivery_node ON orders(delivery_node) WHERE status NOT IN ('cancelled','confirmed','failed','skipped');
CREATE INDEX idx_orders_live_process_node ON orders(process_node_id) WHERE status NOT IN ('cancelled','confirmed','failed','skipped');
CREATE INDEX idx_orders_process_node_delivery_id ON orders(process_node_id, delivery_node, id);
CREATE INDEX idx_orders_process_node_id ON orders(process_node_id);
CREATE INDEX idx_orders_release_intent ON orders(process_node_id) WHERE release_intent <> '';
CREATE INDEX idx_orders_source_node ON orders(source_node);
CREATE INDEX idx_orders_status ON orders(status);
CREATE INDEX idx_orders_uuid ON orders(uuid);
CREATE INDEX idx_outbox_pending ON outbox(sent_at) WHERE sent_at IS NULL;
CREATE INDEX idx_payload_catalog_code ON payload_catalog(code);
CREATE UNIQUE INDEX idx_process_nodes_process_code_live
		ON process_nodes(process_id, code) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_process_nodes_process_core_name
				ON process_nodes(process_id, core_node_name)
				WHERE core_node_name <> '' AND deleted_at IS NULL;
CREATE INDEX idx_process_routing_nodes_process ON process_routing_nodes(process_id);
CREATE UNIQUE INDEX idx_styles_process_name_live
    ON styles(process_id, name) WHERE deleted_at IS NULL;
