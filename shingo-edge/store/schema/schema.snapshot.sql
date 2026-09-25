CREATE INDEX idx_changeovers_process_started ON process_changeovers(process_id, started_at DESC);

CREATE INDEX idx_cnt_changeover_id ON changeover_node_tasks(process_changeover_id);

CREATE INDEX idx_counter_snapshots_anomaly ON counter_snapshots(anomaly, operator_confirmed)
    WHERE anomaly IS NOT NULL AND operator_confirmed = 0;

CREATE INDEX idx_cp_changeover_id ON changeover_participants(process_changeover_id);

CREATE INDEX idx_cp_node_name ON changeover_participants(core_node_name);

CREATE INDEX idx_cst_changeover_id ON changeover_station_tasks(process_changeover_id);

CREATE INDEX idx_order_history_order_id ON order_history(order_id);

CREATE INDEX idx_orders_process_node_id ON orders(process_node_id);

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
    home_kind      TEXT    NOT NULL DEFAULT '',  -- 'home' | 'buffer' (synced from Core); '' = pre-field Core, fall back to classifying by empty payload
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

CREATE TABLE counter_snapshots (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    reporting_point_id INTEGER NOT NULL REFERENCES reporting_points(id) ON DELETE CASCADE,
    count_value        INTEGER NOT NULL,
    delta              INTEGER NOT NULL DEFAULT 0,
    anomaly            TEXT,
    operator_confirmed INTEGER NOT NULL DEFAULT 0,
    recorded_at        TEXT NOT NULL DEFAULT (datetime('now')),
    -- The production tick feed ships from this table (messaging.TickShipper),
    -- so the row carries what the wire event needs, bound by the one INSERT the
    -- poll already runs: recorded_ms is the Go clock read just before that
    -- INSERT (unix ms; recorded_at above has second granularity), and
    -- process_id / style_id are the reporting point's at stroke time. NULL on
    -- rows written before the shipper existed; those were sent through the
    -- outbox.
    recorded_ms        INTEGER,
    process_id         INTEGER,
    style_id           INTEGER
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

CREATE TABLE hourly_counts_local_legacy (
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
    net        INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (scope_kind, scope_key, epoch)
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
, payload_desc TEXT NOT NULL DEFAULT '', origin_id TEXT NOT NULL DEFAULT '', origin_class TEXT NOT NULL DEFAULT '');

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
    -- adj_bin_id / adj_bin_epoch / adj_as_of_seq are the carrier, generation
    -- and AsOfSeq of the last fenced count this slot took (the record-count
    -- fence). A count for the same carrier and generation taken earlier in
    -- the station's stream, delivered later, is refused.
    adj_bin_id         INTEGER,
    adj_bin_epoch      INTEGER NOT NULL DEFAULT 0,
    adj_as_of_seq      INTEGER NOT NULL DEFAULT 0,
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

CREATE TABLE production_tick_cursor (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    last_id    INTEGER NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
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
    created_at              TEXT NOT NULL DEFAULT (datetime('now')), staging_node TEXT NOT NULL DEFAULT '', release_node TEXT NOT NULL DEFAULT '', inbound_source_node TEXT NOT NULL DEFAULT '', inbound_source_node_group TEXT NOT NULL DEFAULT '', outbound_source_node TEXT NOT NULL DEFAULT '', outbound_source_node_group TEXT NOT NULL DEFAULT '', outbound_source TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT 'loader', second_paired_core_node TEXT NOT NULL DEFAULT '',
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
  auto_request_payload TEXT,
  keep_staged INT,
  evacuate_on_changeover INT,
  paired_core_node TEXT,
  auto_confirm INT,
  sequence INT,
  lineside_soft_threshold INT,
  reuse_compatible_bins INT,
  changeover_evac_nodes TEXT,
  changeover_evac_destination TEXT,
  changeover_carryover_disposition TEXT,
  index_robot_supplies INT,
  key_route TEXT,
  key_task TEXT,
  auto_push INT,
  reorder_point_source TEXT,
  below_reorder_since TEXT,
  source TEXT,
  called_by TEXT,
  updated_at TEXT,
  retired_at TEXT,
  source_preset_id INT,
  source_preset_version INT,
  created_at TEXT,
  staging_node TEXT,
  release_node TEXT,
  inbound_source_node TEXT,
  inbound_source_node_group TEXT,
  outbound_source_node TEXT,
  outbound_source_node_group TEXT,
  outbound_source TEXT,
  mode TEXT,
  second_paired_core_node TEXT
, quarantined_at TEXT NOT NULL DEFAULT '', quarantined_reason TEXT NOT NULL DEFAULT '', quarantined_sync_loader_count INTEGER NOT NULL DEFAULT 0, quarantined_sync_loader_keys TEXT NOT NULL DEFAULT '');

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
