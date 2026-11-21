CREATE FUNCTION public.bin_types_family_of_one() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
		 DECLARE
		   family_code TEXT := NEW.code;
		 BEGIN
		   IF NEW.family_id IS NOT NULL THEN
		     RETURN NEW;
		   END IF;
		   IF NEW.bare_of IS NOT NULL THEN
		     SELECT family_id INTO NEW.family_id FROM bin_types WHERE id = NEW.bare_of;
		     RETURN NEW;
		   END IF;
		   -- A code already held is an insert about to conflict: it makes no family.
		   SELECT family_id INTO NEW.family_id FROM bin_types WHERE code = NEW.code;
		   IF NEW.family_id IS NOT NULL THEN
		     RETURN NEW;
		   END IF;
		   IF EXISTS (SELECT 1 FROM carrier_families WHERE code = family_code) THEN
		     family_code := NEW.code || '-' || NEW.id;
		   END IF;
		   -- Two first inserts of one code race here, and only one of them will stand.
		   INSERT INTO carrier_families (code) VALUES (family_code)
		     ON CONFLICT (code) DO UPDATE SET updated_at = NOW() RETURNING id INTO NEW.family_id;
		   RETURN NEW;
		 END
		 $$;

CREATE TABLE public.admin_users (
    id bigint NOT NULL,
    username text NOT NULL,
    password_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.admin_users_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.admin_users_id_seq OWNED BY public.admin_users.id;

CREATE TABLE public.area_confidence_daily (
    day date NOT NULL,
    area_name text NOT NULL,
    class_name text DEFAULT ''::text NOT NULL,
    p05 double precision,
    p25 double precision,
    p50 double precision,
    p75 double precision,
    p95 double precision,
    samples integer NOT NULL,
    mean_good double precision,
    samples_good integer DEFAULT 0 NOT NULL,
    min_conf double precision,
    robots integer NOT NULL,
    robots_seen text[],
    sentinel_samples integer DEFAULT 0 NOT NULL,
    sentinel_robots integer DEFAULT 0 NOT NULL,
    conf_hist integer[]
);

COMMENT ON TABLE public.area_confidence_daily IS 'The per-zone roll-up (v82). One reading can be in several zones (SEER areas overlap), so its samples column does not sum to the plant total.';

CREATE TABLE public.audit_log (
    id bigint NOT NULL,
    entity_type text NOT NULL,
    entity_id bigint DEFAULT 0 NOT NULL,
    action text NOT NULL,
    old_value text DEFAULT ''::text NOT NULL,
    new_value text DEFAULT ''::text NOT NULL,
    actor text DEFAULT 'system'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.audit_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.audit_log_id_seq OWNED BY public.audit_log.id;

CREATE TABLE public.bin_loader_home_bin_types (
    position_node_id bigint NOT NULL,
    bin_type_id bigint NOT NULL
);

COMMENT ON TABLE public.bin_loader_home_bin_types IS 'What each loader window can physically take (v75).';

CREATE TABLE public.bin_loader_homes (
    loader_id bigint NOT NULL,
    position_node_id bigint NOT NULL,
    payload_code text NOT NULL,
    uop_threshold integer DEFAULT 0 NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    home_kind text DEFAULT 'home'::text NOT NULL,
    CONSTRAINT bin_loader_homes_home_kind_check CHECK ((home_kind = ANY (ARRAY['home'::text, 'buffer'::text])))
);

CREATE TABLE public.bin_loader_payloads (
    loader_id bigint NOT NULL,
    payload_code text NOT NULL,
    uop_threshold integer DEFAULT 0 NOT NULL
);

CREATE TABLE public.bin_loader_quotas (
    loader_id bigint NOT NULL,
    bin_type_id bigint NOT NULL,
    want integer DEFAULT 0 NOT NULL,
    CONSTRAINT bin_loader_quotas_want_check CHECK ((want >= 0))
);

COMMENT ON TABLE public.bin_loader_quotas IS 'A loader''s declared carrier mix (v75).';

CREATE TABLE public.bin_loaders (
    id bigint NOT NULL,
    name text NOT NULL,
    role text NOT NULL,
    layout text NOT NULL,
    replenishment text NOT NULL,
    outbound_dest text DEFAULT ''::text NOT NULL,
    inbound_source text DEFAULT ''::text NOT NULL,
    config_gen bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    archived_at timestamp with time zone,
    funnel_windows boolean DEFAULT false NOT NULL,
    accept_partials boolean DEFAULT false NOT NULL,
    auto_push boolean DEFAULT false NOT NULL,
    second_stage_loader_id bigint,
    fed_directly boolean DEFAULT false NOT NULL,
    pulled_directly boolean DEFAULT false NOT NULL,
    board_hides_idle boolean DEFAULT false NOT NULL,
    board_shows_all_parts boolean DEFAULT false NOT NULL,
    drains_empty boolean DEFAULT false NOT NULL,
    drains_tops_bottoms boolean DEFAULT false NOT NULL,
    drains_leftover boolean DEFAULT false NOT NULL,
    drains_unverified boolean DEFAULT false NOT NULL,
    no_clear_partials boolean DEFAULT false NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    CONSTRAINT bin_loaders_layout_check CHECK ((layout = ANY (ARRAY['shared_window'::text, 'dedicated_positions'::text]))),
    CONSTRAINT bin_loaders_replenishment_check CHECK ((replenishment = ANY (ARRAY['operator'::text, 'threshold'::text]))),
    CONSTRAINT bin_loaders_role_check CHECK ((role = ANY (ARRAY['produce'::text, 'consume'::text])))
);

CREATE SEQUENCE public.bin_loaders_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bin_loaders_id_seq OWNED BY public.bin_loaders.id;

CREATE SEQUENCE public.bin_placement_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

COMMENT ON SEQUENCE public.bin_placement_seq IS 'The placement stamp (v150): one value per placement Core records, taken in the arrival''s own UPDATE and carried on the landing. A seat refuses a landing whose stamp is not newer than the one it last bound, so a redelivered or overtaken landing binds nothing. A sequence, not a clock, because a clock can step.';

CREATE TABLE public.bin_types (
    id bigint NOT NULL,
    code text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    width_in double precision DEFAULT 0 NOT NULL,
    height_in double precision DEFAULT 0 NOT NULL,
    length_in double precision DEFAULT 0 NOT NULL,
    required_robot_group text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    totes_per_layer integer,
    bare_of bigint,
    bare boolean GENERATED ALWAYS AS ((bare_of IS NOT NULL)) STORED NOT NULL,
    family_id bigint NOT NULL,
    CONSTRAINT bin_types_bare_of_not_self CHECK ((bare_of <> id)),
    CONSTRAINT bin_types_totes_per_layer_check CHECK ((totes_per_layer > 0))
);

COMMENT ON COLUMN public.bin_types.length_in IS 'The third dimension (v90). The table carried width and height only, which left a code like 45x58x32 holding its own length in the string, where no query could reach it. Width x length is a type''s footprint: a label and a check, never a decision. What a cart may be stamped as is its carrier family (v172); a slot''s fit is type identity everywhere.';

COMMENT ON COLUMN public.bin_types.totes_per_layer IS 'How many totes one layer of this carrier holds (v161). NULL: not a tote carrier, or not said.';

CREATE SEQUENCE public.bin_types_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bin_types_id_seq OWNED BY public.bin_types.id;

CREATE TABLE public.bin_uop_delta_daily (
    day date NOT NULL,
    bin_id bigint NOT NULL,
    epoch_seq bigint NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    ticks integer NOT NULL,
    consumed integer NOT NULL,
    added integer NOT NULL,
    first_uop integer,
    last_uop integer,
    min_uop integer,
    crossings integer DEFAULT 0 NOT NULL
);

COMMENT ON TABLE public.bin_uop_delta_daily IS 'The permanent daily roll-up of the raw delta stream (v94): growth accepted.';

CREATE TABLE public.bin_uop_exception (
    id bigint NOT NULL,
    kind text NOT NULL,
    bin_id bigint,
    payload_code text DEFAULT ''::text NOT NULL,
    actor text DEFAULT ''::text NOT NULL,
    epoch_seq bigint,
    occurred_at timestamp with time zone NOT NULL,
    before_uop integer,
    after_uop integer,
    deepest_uop integer,
    recovered_at timestamp with time zone,
    op text NOT NULL,
    detail jsonb,
    station_uid text DEFAULT ''::text NOT NULL
);

COMMENT ON TABLE public.bin_uop_exception IS 'The permanent exceptions ledger (v93): no retention, ever.';

COMMENT ON COLUMN public.bin_uop_exception.station_uid IS 'The Edge whose report the exception is about, for the kinds a station writes (negative_crossing on bin_uop_delta, stale_epoch, payload_mismatch, edge_rollback, boundary on edge_rollback, report_divergence, hold_dropped). Until v185 that uid was in actor, or in hold_dropped''s detail; actor holds only a who.';

CREATE SEQUENCE public.bin_uop_exception_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bin_uop_exception_id_seq OWNED BY public.bin_uop_exception.id;

CREATE TABLE public.bin_uop_ledger (
    id bigint NOT NULL,
    bin_id bigint NOT NULL,
    before_uop integer,
    after_uop integer NOT NULL,
    op text NOT NULL,
    source text DEFAULT ''::text NOT NULL,
    order_id bigint,
    payload_code text DEFAULT ''::text NOT NULL,
    actor text DEFAULT ''::text NOT NULL,
    metadata jsonb,
    applied_at timestamp with time zone DEFAULT now() NOT NULL,
    node_id bigint,
    station_uid text DEFAULT ''::text NOT NULL,
    detail jsonb,
    loader_id bigint,
    reason text DEFAULT ''::text NOT NULL
);

COMMENT ON COLUMN public.bin_uop_ledger.station_uid IS 'The Edge a row is about. For a station-reported op (bin_uop_delta and the drops and rebinds beside it) the who is that Edge, so actor is blank; for a lifted row it is the order''s station, and actor holds a Core actor when the order was one''s.';

CREATE SEQUENCE public.bin_uop_ledger_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bin_uop_ledger_id_seq OWNED BY public.bin_uop_ledger.id;

CREATE TABLE public.bins (
    id bigint NOT NULL,
    bin_type_id bigint NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    node_id bigint,
    status text DEFAULT 'available'::text NOT NULL,
    claimed_by bigint,
    staged_at timestamp with time zone,
    staged_expires_at timestamp with time zone,
    payload_code text DEFAULT ''::text NOT NULL,
    manifest jsonb,
    uop_remaining integer DEFAULT 0 NOT NULL,
    delta_epoch bigint DEFAULT 1 NOT NULL,
    manifest_confirmed boolean DEFAULT false NOT NULL,
    locked boolean DEFAULT false NOT NULL,
    locked_by text DEFAULT ''::text NOT NULL,
    locked_at timestamp with time zone,
    last_counted_at timestamp with time zone,
    last_counted_by text DEFAULT ''::text NOT NULL,
    loaded_at timestamp with time zone,
    anomaly_at timestamp with time zone,
    quality_hold boolean DEFAULT false NOT NULL,
    hold_by text DEFAULT ''::text NOT NULL,
    hold_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    lifted_at timestamp with time zone,
    totes_on_cart integer,
    last_lift_at timestamp with time zone,
    lifted_from_node_id bigint,
    anomaly_note text DEFAULT ''::text NOT NULL,
    undeclared_carrier_at timestamp with time zone,
    held_since timestamp with time zone,
    CONSTRAINT bins_totes_on_cart_check CHECK ((totes_on_cart >= 0))
);

COMMENT ON COLUMN public.bins.quality_hold IS 'Quality containment: the per-bin hold marker an operator''s Send to Quality Hold sets. A held bin is not re-sourceable and not re-releasable to FG while it waits for (or sits in) containment. Default false is every other bin.';

COMMENT ON COLUMN public.bins.lifted_at IS 'When a streamed carrier''s generation ended at a lift (v151): set with the bump in the lift''s UPDATE and cleared at the arrival. A late window starting at or after it is not the lifted carrier''s (refused as after_lift).';

COMMENT ON COLUMN public.bins.totes_on_cart IS 'The totes standing on the cart, filled or empty (v161). NULL is unknown; 0 is tops and bottoms. Every statement that empties a carrier writes NULL.';

COMMENT ON COLUMN public.bins.last_lift_at IS 'When a robot last lifted the bin (v162). Every lift writes it, in the lift''s own UPDATE; nothing clears it. The vacate hold''s floor.';

COMMENT ON COLUMN public.bins.lifted_from_node_id IS 'The node a streamed carrier was lifted from (v175), written with lifted_at. A late draw of the generation the lift ended is booked there.';

CREATE SEQUENCE public.bins_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bins_id_seq OWNED BY public.bins.id;

CREATE TABLE public.carrier_families (
    id bigint NOT NULL,
    code text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    lift_bintask text DEFAULT ''::text NOT NULL,
    wait_bintask text DEFAULT ''::text NOT NULL
);

COMMENT ON TABLE public.carrier_families IS 'The physical cart a bin type is one setting of (v172).';

CREATE SEQUENCE public.carrier_families_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.carrier_families_id_seq OWNED BY public.carrier_families.id;

CREATE TABLE public.cell_config (
    cell_label text NOT NULL,
    station_uid text NOT NULL,
    primary_process_id bigint NOT NULL,
    sub_process_ids jsonb DEFAULT '[]'::jsonb NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON COLUMN public.cell_config.cell_label IS 'The operator''s label for the cell (cell_id until v185), the key.';

CREATE TABLE public.cell_part_events (
    id bigint NOT NULL,
    station_uid text NOT NULL,
    recorded_at timestamp with time zone NOT NULL,
    edge_snapshot_id bigint NOT NULL,
    count_value bigint DEFAULT 0 NOT NULL,
    delta bigint DEFAULT 0 NOT NULL,
    anomaly text DEFAULT ''::text NOT NULL,
    process_id bigint DEFAULT 0 NOT NULL,
    style_id bigint DEFAULT 0 NOT NULL
)
PARTITION BY RANGE (recorded_at);

COMMENT ON TABLE public.cell_part_events IS 'One row per PLC counter tick, projected from production.ticks (and production.tick from older Edges). Partitioned monthly by recorded_at: store/heartbeat creates the current and next month''s partitions at boot and drops partitions older than the retention window nightly. The dedup key is the unique index uq_cell_part_events_tick (v128). station_uid is the sending Edge''s uid, the envelope''s station (cell_id until v185).';

CREATE SEQUENCE public.cell_part_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.cell_part_events_id_seq OWNED BY public.cell_part_events.id;

CREATE TABLE public.cleaning_mission_setup (
    id smallint DEFAULT 1 NOT NULL,
    points text[] NOT NULL,
    frequency interval NOT NULL,
    active_from time without time zone NOT NULL,
    active_to time without time zone NOT NULL,
    min_battery smallint DEFAULT 25 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT cleaning_mission_setup_id_check CHECK ((id = 1))
);

COMMENT ON TABLE public.cleaning_mission_setup IS 'The one air-knife mission set up on the Robots page''s Cleaning tab (v158; floor data, never yaml); read by the cleaning loop every tick and by the tab.';

CREATE TABLE public.cleaning_missions (
    id bigint NOT NULL,
    vehicle_id text NOT NULL,
    vendor_order_id text NOT NULL,
    state text NOT NULL,
    reason text NOT NULL,
    dispatched_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone
);

COMMENT ON TABLE public.cleaning_missions IS 'One row per pinned cleaning order sent to the fleet, the only record of it: no Core orders row is ever written (v158). Read by the cleaning loop (open rows, last dispatch per robot) and the Cleaning tab''s history.';

CREATE SEQUENCE public.cleaning_missions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.cleaning_missions_id_seq OWNED BY public.cleaning_missions.id;

CREATE TABLE public.cleaning_opt_outs (
    vehicle_id text NOT NULL
);

COMMENT ON TABLE public.cleaning_opt_outs IS 'A row per robot never to clean (v158); read by the cleaning loop''s guard and the robot pop-up''s switch.';

CREATE TABLE public.cms_postings (
    id bigint NOT NULL,
    body_sha text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    transaction_id text DEFAULT ''::text NOT NULL,
    http_status integer,
    attempts integer DEFAULT 0 NOT NULL,
    requeue_count integer DEFAULT 0 NOT NULL,
    next_retry_at timestamp with time zone,
    last_error text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    inflight_at timestamp with time zone,
    posted_at timestamp with time zone,
    settled_at timestamp with time zone
);

COMMENT ON TABLE public.cms_postings IS 'One row per POST to the CMS middleware (v102). Separate from cms_transactions because a transaction is a local fact (a bin crossed a boundary) and a posting is a claim about a remote system (whether it knows). Their lifecycles do not line up: one posting carries many transactions, and a posting can be retried while the transactions it carries never change.';

CREATE SEQUENCE public.cms_postings_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.cms_postings_id_seq OWNED BY public.cms_postings.id;

CREATE TABLE public.cms_transactions (
    id bigint NOT NULL,
    node_id bigint NOT NULL,
    node_name text DEFAULT ''::text NOT NULL,
    cat_id text NOT NULL,
    delta bigint DEFAULT 0 NOT NULL,
    bin_id bigint,
    bin_label text DEFAULT ''::text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    source_type text NOT NULL,
    order_id bigint,
    notes text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    posting_id bigint,
    robot_id text DEFAULT ''::text NOT NULL,
    storeroom text DEFAULT ''::text NOT NULL,
    location_node_id bigint,
    location_node_name text DEFAULT ''::text NOT NULL
);

COMMENT ON COLUMN public.cms_transactions.delta IS 'Signed: negative leaves the boundary, positive arrives at it. There is deliberately no txn_type (it was sign(delta) as a word, free to disagree with the delta beside it) and no qty_before or qty_after (computed at insert from a recursive subtree scan, unsound under concurrent moves; v101 dropped them). A running total is a query, not a column.';

COMMENT ON COLUMN public.cms_transactions.source_type IS 'No DEFAULT: every writer sets it, and a default that silently supplies it is a place for a future writer to forget. Historical rows carry correction and stay browsable.';

CREATE SEQUENCE public.cms_transactions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.cms_transactions_id_seq OWNED BY public.cms_transactions.id;

CREATE TABLE public.dashboards (
    id bigint NOT NULL,
    name text NOT NULL,
    kind text DEFAULT 'task-board'::text NOT NULL,
    stations_json text DEFAULT '[]'::text NOT NULL,
    config_json text DEFAULT '{}'::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.dashboards IS 'Floor-facing definitions for the wall-display platform. A row is a saved, named view of Core''s live data, scoped to a set of stations (an area), rendered chromeless at /wall-display/{id}?kiosk=1 and managed on the hub at /. Pure presentation: it owns no operational data beyond its own definition. kind selects the renderer (task-board, robot-map, heartbeat, node-report); stations_json is the area filter; config_json carries per-kind options. The table name matches the /api/.../dashboards namespace and domain.Dashboard, not the page''s name.';

CREATE SEQUENCE public.dashboards_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.dashboards_id_seq OWNED BY public.dashboards.id;

CREATE TABLE public.demand_origins (
    origin_id uuid NOT NULL,
    episode_key text NOT NULL,
    kind text NOT NULL,
    direction text DEFAULT ''::text NOT NULL,
    trigger_kind text DEFAULT ''::text NOT NULL,
    trigger_ref text DEFAULT ''::text NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    process_id text DEFAULT ''::text NOT NULL,
    core_node_name text DEFAULT ''::text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    opened_at timestamp with time zone,
    opened_total integer DEFAULT 0 NOT NULL,
    threshold integer DEFAULT 0 NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    expected_orders integer,
    expected_reason text DEFAULT ''::text NOT NULL,
    rerequest_count integer DEFAULT 0 NOT NULL,
    signal_count integer DEFAULT 0 NOT NULL,
    discretionary boolean DEFAULT false NOT NULL,
    closed_at timestamp with time zone,
    close_reason text DEFAULT ''::text NOT NULL,
    closed_by text,
    plan text DEFAULT ''::text NOT NULL,
    plan_verdict text DEFAULT ''::text NOT NULL,
    need_at timestamp with time zone,
    predicted_need_at timestamp with time zone,
    provenance text DEFAULT ''::text NOT NULL
);

CREATE TABLE public.demand_registry (
    id bigint NOT NULL,
    station_uid text NOT NULL,
    core_node_name text NOT NULL,
    role text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    outbound_dest text DEFAULT ''::text NOT NULL,
    replenish_uop_threshold integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    loader_id bigint
);

COMMENT ON COLUMN public.demand_registry.replenish_uop_threshold IS 'UOP-threshold replenishment: when above 0, Core''s threshold monitor holds this (loader, payload) loop''s combined in-loop UOP (bins and buckets) against it. 0 is off.';

CREATE SEQUENCE public.demand_registry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.demand_registry_id_seq OWNED BY public.demand_registry.id;

CREATE TABLE public.edge_cells (
    station_uid text NOT NULL,
    cell_label text NOT NULL,
    bindings jsonb DEFAULT '[]'::jsonb NOT NULL,
    first_seen timestamp with time zone DEFAULT now() NOT NULL,
    last_seen timestamp with time zone DEFAULT now() NOT NULL,
    stale boolean DEFAULT false NOT NULL,
    catalog_digest text DEFAULT ''::text NOT NULL
);

COMMENT ON COLUMN public.edge_cells.catalog_digest IS 'The digest the cell catalog that made this row live carried (v187): an apply marks every cell of the station stale and makes each listed one live in one transaction, so every live row of a station holds the digest of the catalog it last applied, which Core quotes back to that Edge. No live row is no catalog held. Empty: made live before v187, or by a catalog that carried no digest; it matches no Edge''s, so the Edge re-sends. A stale row keeps the digest it was last live under; nothing reads it.';

CREATE TABLE public.edge_lineside_reports (
    station_uid text NOT NULL,
    core_node_name text NOT NULL,
    payload_code text NOT NULL,
    reported_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.edge_registry (
    id bigint NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    hostname text DEFAULT ''::text NOT NULL,
    version text DEFAULT ''::text NOT NULL,
    registered_at timestamp with time zone DEFAULT now() NOT NULL,
    last_heartbeat timestamp with time zone,
    status text DEFAULT 'active'::text NOT NULL,
    bound_hostname text DEFAULT ''::text NOT NULL,
    bound_instance text DEFAULT ''::text NOT NULL,
    prev_instance text DEFAULT ''::text NOT NULL,
    bound_at timestamp with time zone,
    claimed_at timestamp with time zone,
    conflict_hostname text DEFAULT ''::text NOT NULL,
    conflict_count bigint DEFAULT 0 NOT NULL,
    conflict_at timestamp with time zone,
    schema_version integer,
    clock_offset_ms bigint,
    timezone text DEFAULT ''::text NOT NULL,
    tick_pending bigint,
    tick_oldest_unsent_age_ms bigint,
    tick_reported_at timestamp with time zone,
    tick_rejected bigint DEFAULT 0 NOT NULL,
    CONSTRAINT edge_registry_station_uid_nonblank CHECK ((station_uid <> ''::text))
);

COMMENT ON TABLE public.edge_registry IS 'The identity table, one row per Edge, keyed by station_uid (total and never blank since v185): what Core correlates all history to, and what an envelope''s Address.Station carries. display_name is the operator''s label. Core mints the uid rather than the Pi because a Core-issued token is re-issuable: replace the hardware for an existing station and hand the new Pi the existing uid, and history does not move because identity did not move. Not the BIGSERIAL id either: it is real and it is read (registry.List scans it), but it is allocated by an INSERT ... ON CONFLICT driven by an Edge-supplied string, a consequence of the name, not independent of it. bound_hostname and conflict_* are the duplicate-edge detector (v64); bound_instance, prev_instance and bound_at are the binding lease (v66), for the case the hostname detector is blind to: two Pis flashed from one SD image share a hostname. See registry.Register for why recurrence, an instance coming back after being displaced, is the signature a restarting single Pi can never produce. The conflict_* columns are the alarm''s persisted state, not a derived view: the question has this ever happened here has to be answerable in SQL months later.';

COMMENT ON COLUMN public.edge_registry.station_uid IS 'What Core correlates all history to. Core mints it at enrollment and it never changes. Opaque on purpose: the moment an identifier is legible it acquires an editor.';

COMMENT ON COLUMN public.edge_registry.display_name IS 'The operator''s label (PLANT / EDGE-2). Operator-owned, freely mutable, and it never crosses the wire as an identifier.';

COMMENT ON COLUMN public.edge_registry.claimed_at IS 'NULL until a human has said what this station is. It is not status: Register writes status = active on every register, so anything recorded there is erased by the next heartbeat''s worth of traffic. The question has anybody acknowledged this box has to survive registration, so it gets its own column and only moves one way. NULL is a real and expected state: an unconfigured Pi mints its own uid and comes up working. What it cannot do is claim to be an existing station: a minted uid is random, so it always makes its own row and can never take over another one.';

COMMENT ON COLUMN public.edge_registry.schema_version IS 'The applied schema_migrations version the edge''s register payload last reported (protocol.EdgeRegister.SchemaVersion; v137), written only from the wire, never by a human. NULL means no register has carried one.';

COMMENT ON COLUMN public.edge_registry.clock_offset_ms IS 'The edge''s clock minus Core''s, as its last heartbeat implied it: the envelope''s ts minus Core''s clock on receipt, the broker''s delay included (v154). Written by the heartbeat''s UPDATE; NULL until a heartbeat carried a ts.';

COMMENT ON CONSTRAINT edge_registry_station_uid_nonblank ON public.edge_registry IS 'Every row has its uid. Until v185 a row could exist with none (a heartbeat from a legacy Edge not yet enrolled), and the uid key was partial; v185 folded each blank uid from the row''s station_id, whose value was the uid, and deleted the rows that could take none.';

CREATE SEQUENCE public.edge_registry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.edge_registry_id_seq OWNED BY public.edge_registry.id;

CREATE TABLE public.inbox (
    msg_id text NOT NULL,
    msg_type text DEFAULT ''::text NOT NULL,
    processed_at timestamp with time zone DEFAULT now() NOT NULL,
    reply_outbox_id bigint
);

CREATE TABLE public.inventory_delta_dedup (
    station_uid text NOT NULL,
    scope_kind text NOT NULL,
    scope_key text NOT NULL,
    epoch bigint DEFAULT 0 NOT NULL,
    last_seq bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    applied_net bigint,
    applied_window_end timestamp with time zone
);

COMMENT ON TABLE public.inventory_delta_dedup IS 'At-most-once dedup for inventory delta envelopes. Distinct from inbox dedup, which gates order-message processing: this gates the count change the envelope carries. last_seq is the highest SequenceID Core has applied for the (station_uid, scope_kind, scope_key) tuple; epoch is the bin''s delta_epoch when the delta was emitted (bucket-scope deltas carry 0). A bucket''s row is not cleared when the pile empties, and must not be: since v126 (applied_net) an absent row applies the station''s whole running net. The admin delete resets the row instead of deleting it (store/inventory/inventory.go).';

CREATE TABLE public.kanban_families (
    kanban_node_id bigint NOT NULL,
    family_id bigint NOT NULL,
    pool integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT kanban_families_pool_check CHECK ((pool >= 0))
);

COMMENT ON TABLE public.kanban_families IS 'The pool of each carrier family a kanban owns (v172).';

CREATE TABLE public.kanban_layer_levels (
    kanban_node_id bigint NOT NULL,
    bin_type_id bigint NOT NULL,
    layers integer NOT NULL,
    want integer NOT NULL,
    CONSTRAINT kanban_layer_levels_layers_check CHECK ((layers >= 0)),
    CONSTRAINT kanban_layer_levels_want_check CHECK ((want >= 0))
);

COMMENT ON TABLE public.kanban_layer_levels IS 'Per kanban, carrier type and height in layers, how many carts the kanban wants standing that high (v161 as node_maintain_layer_levels, renamed at v169). A height of 1 or more is communication, summed against nothing; the row at 0 (v164) is tops and bottoms, a cap of its own beside the type''s stack level in kanban_levels.';

CREATE TABLE public.kanban_levels (
    kanban_node_id bigint NOT NULL,
    bin_type_id bigint NOT NULL,
    want integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT kanban_levels_want_check CHECK ((want >= 0))
);

COMMENT ON TABLE public.kanban_levels IS 'How many empty carriers of each type a kanban is to hold, and the type''s own overflow (v90 as node_maintain_levels, renamed at v169). Separate from bin_loader_quotas on purpose: a quota is a preference bounded by never-2N, a level is the number Core keeps.';

CREATE TABLE public.kanban_sources (
    kanban_node_id bigint NOT NULL,
    rank integer NOT NULL,
    source_node_id bigint NOT NULL,
    CONSTRAINT kanban_sources_check CHECK ((source_node_id <> kanban_node_id)),
    CONSTRAINT kanban_sources_rank_check CHECK ((rank >= 1))
);

COMMENT ON TABLE public.kanban_sources IS 'The groups a kanban''s keeper may pull from, in rank order (v169).';

CREATE TABLE public.kanban_supports (
    kanban_node_id bigint NOT NULL,
    process_node_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.kanban_supports IS 'Which process nodes a kanban serves (v90 as node_maintain_supports, renamed at v169): the resolved node set, because a claim is Edge-local and Core cannot read one.';

CREATE TABLE public.kanbans (
    node_id bigint NOT NULL,
    station_uid text,
    strict boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.kanbans IS 'One row per kanban: its station and strict (v169).';

CREATE TABLE public.lane_confidence_daily (
    day date NOT NULL,
    area_name text NOT NULL,
    lane text NOT NULL,
    p05 double precision,
    p25 double precision,
    p50 double precision,
    p75 double precision,
    p95 double precision,
    samples integer NOT NULL,
    mean_good double precision,
    samples_good integer DEFAULT 0 NOT NULL,
    min_conf double precision,
    robots integer NOT NULL,
    robots_seen text[],
    sentinel_samples integer DEFAULT 0 NOT NULL,
    sentinel_robots integer DEFAULT 0 NOT NULL,
    reloc_failed_samples integer DEFAULT 0 NOT NULL,
    reloc_failed_robots integer DEFAULT 0 NOT NULL,
    map_mismatch_samples integer DEFAULT 0 NOT NULL,
    version_id bigint NOT NULL,
    conf_hist integer[]
);

COMMENT ON TABLE public.lane_confidence_daily IS 'The permanent per-lane roll-up (v80), in place of the per-segment one, whose rows were at the directed-segment grain, half a lane.';

CREATE TABLE public.lane_robot_confidence_daily (
    day date NOT NULL,
    area_name text NOT NULL,
    lane text NOT NULL,
    vehicle_id text NOT NULL,
    p05 double precision,
    p25 double precision,
    p50 double precision,
    p75 double precision,
    p95 double precision,
    samples integer NOT NULL,
    mean_good double precision,
    samples_good integer DEFAULT 0 NOT NULL,
    min_conf double precision,
    sentinel_samples integer DEFAULT 0 NOT NULL,
    version_id bigint NOT NULL,
    conf_hist integer[]
);

COMMENT ON TABLE public.lane_robot_confidence_daily IS 'The permanent per-lane, per-robot grain the map filters by AMR (v83). lane_confidence_daily merges its robots irreversibly, so this carries the intersection.';

CREATE TABLE public.lineside_buckets (
    id bigint NOT NULL,
    station_uid text NOT NULL,
    core_node_name text NOT NULL,
    payload_code text NOT NULL,
    qty integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    state text NOT NULL,
    CONSTRAINT lineside_buckets_qty_check CHECK ((qty >= 0)),
    CONSTRAINT lineside_buckets_state_check CHECK ((state = ANY (ARRAY['active'::text, 'stranded'::text])))
);

COMMENT ON TABLE public.lineside_buckets IS 'Core''s mirror of the Edge''s bucket model: parts at a node, by payload. core_node_name is the canonical node name, not the Edge''s process_nodes.id: the two int64 namespaces share a type and mean different things at each end, and reading the Edge''s id as Core''s produced cross-plant attribution bugs. It resolves to nodes.id through GetNodeByName. The station left the key at v65: which node holds the parts is the fact, which Pi reported it is not. The key is (core_node_name, payload_code, state), state active or stranded (v131).';

COMMENT ON COLUMN public.lineside_buckets.station_uid IS 'Attribute data, not identity: the station that last reported this pile. Never a predicate (v65).';

COMMENT ON COLUMN public.lineside_buckets.payload_code IS 'The payload this pile''s parts belong to, filled by the Edge at capture from the order context. Empty means unknown (an orphan whose claim was deleted before the capture resolved a payload) and is excluded from SystemUOPForPayload: a conservative undercount, never an overcount.';

CREATE SEQUENCE public.lineside_buckets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.lineside_buckets_id_seq OWNED BY public.lineside_buckets.id;

CREATE TABLE public.lineside_drain_ledger (
    id bigint NOT NULL,
    node_id bigint NOT NULL,
    payload_code text NOT NULL,
    before_qty integer NOT NULL,
    after_qty integer NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.lineside_drain_ledger IS 'One row per applied lineside drain (v120). A drain is consumption at a node from a pile, not a bin event, so bin_uop_ledger cannot take it (bin_id is NOT NULL), and the pile''s row deletes at qty 0; this is where the history survives.';

CREATE SEQUENCE public.lineside_drain_ledger_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.lineside_drain_ledger_id_seq OWNED BY public.lineside_drain_ledger.id;

CREATE TABLE public.load_sequences (
    name text NOT NULL,
    task_names text DEFAULT '[]'::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    lift_at integer NOT NULL,
    CONSTRAINT load_sequences_lift_at_in_task_names CHECK (((lift_at >= 0) AND (lift_at < GREATEST(json_array_length((task_names)::json), 1))))
);

COMMENT ON TABLE public.load_sequences IS 'The advanced-load-sequence registry. One row per sequence name; task_names is a JSON array of binTask names in execution order. Editable data (a plant names its RDS-side binTask keys differently); seeded by v50 with Child cart interlock. A payload selects a sequence by name via payloads.advanced_load_sequence.';

COMMENT ON COLUMN public.load_sequences.lift_at IS 'The index in task_names of the name that lifts (v179).';

CREATE TABLE public.mission_events (
    id bigint NOT NULL,
    order_id bigint NOT NULL,
    vendor_order_id text DEFAULT ''::text NOT NULL,
    old_state text NOT NULL,
    new_state text NOT NULL,
    robot_id text DEFAULT ''::text NOT NULL,
    robot_x double precision,
    robot_y double precision,
    robot_angle double precision,
    robot_battery double precision,
    rds_station text DEFAULT ''::text NOT NULL,
    blocks_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    errors_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.mission_events IS 'Raw vendor observations, one row per transition: what the fleet reported, in what order, with the robot where. Fed by the RDS status poller, so new_state holds vendor states (RUNNING, WAITING, FINISHED: SEER''s vocabulary), not Core''s order statuses. The one exception is BLOCK_FINISHED, Core''s own per-leg marker, deliberately not a vendor state so a reader filtering on real states does not pick it up. Not the source for canonical timing: two clocks meet here, and order_history is the one written by Core''s own state machine. Same subject as mission_telemetry at a different grain, answering a different question: do not merge them, and do not grow this into a leg table beyond the per-block rows it carries.';

COMMENT ON COLUMN public.mission_events.rds_station IS 'The RDS map point the robot reported standing at: a fleet name, not a station.';

CREATE SEQUENCE public.mission_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.mission_events_id_seq OWNED BY public.mission_events.id;

CREATE TABLE public.mission_telemetry (
    id bigint NOT NULL,
    order_id bigint NOT NULL,
    vendor_order_id text DEFAULT ''::text NOT NULL,
    robot_id text DEFAULT ''::text NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    order_type text DEFAULT ''::text NOT NULL,
    source_node text DEFAULT ''::text NOT NULL,
    delivery_node text DEFAULT ''::text NOT NULL,
    terminal_state text DEFAULT ''::text NOT NULL,
    vendor_created timestamp with time zone,
    vendor_completed timestamp with time zone,
    core_created timestamp with time zone,
    core_completed timestamp with time zone,
    duration_ms bigint DEFAULT 0 NOT NULL,
    vendor_duration_ms bigint DEFAULT 0 NOT NULL,
    blocks_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    errors_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    warnings_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    notices_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    robot_alarms_json jsonb,
    requested_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT mission_telemetry_one_requester CHECK (((station_uid = ''::text) OR (requested_by = ''::text))),
    CONSTRAINT mission_telemetry_requested_by_known CHECK ((requested_by = ANY (ARRAY[''::text, 'core-operator'::text, 'core-direct'::text, 'core-test'::text]))),
    CONSTRAINT mission_telemetry_station_uid_not_actor CHECK ((station_uid <> ALL (ARRAY['core-operator'::text, 'core-direct'::text, 'core-test'::text])))
);

COMMENT ON TABLE public.mission_telemetry IS 'What the robots did, one row per mission: what the fleet actually carried out, not what happened to the orders. Written only when an order reaches a vendor terminal (finalizeMissionTelemetry, gated on fleet.IsTerminalState), so most terminal orders have no row here. That survivorship is correct for the question this table answers, since every skip and most cancels never reached a robot, and wrong for any question about order outcomes, which belong to orders and order_history. Reading failure counts from here was a real bug (see GetFailures).';

COMMENT ON COLUMN public.mission_telemetry.duration_ms IS 'Lead time (created to terminal, queue included). Execution time, what the robot actually spent with faulted dwell excluded, is computed on read from order_history (store/telemetry/execution.go).';

COMMENT ON COLUMN public.mission_telemetry.requested_by IS 'The order''s requested_by, as orders carries it.';

CREATE SEQUENCE public.mission_telemetry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.mission_telemetry_id_seq OWNED BY public.mission_telemetry.id;

CREATE TABLE public.node_bin_types (
    node_id bigint NOT NULL,
    bin_type_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON COLUMN public.node_bin_types.created_at IS 'When the row was written (v90); the table carried no timestamp before. Written by the group settings modal, as node_properties is.';

CREATE TABLE public.node_payloads (
    node_id bigint NOT NULL,
    payload_id bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.node_properties (
    id bigint NOT NULL,
    node_id bigint NOT NULL,
    key text NOT NULL,
    value text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON COLUMN public.node_properties.updated_at IS 'When the row last moved (v90). The property UPSERT changes the value in place, so a created_at-only row read as untouched forever. The audit_log says who and what; this says when.';

CREATE SEQUENCE public.node_properties_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.node_properties_id_seq OWNED BY public.node_properties.id;

CREATE TABLE public.node_stations (
    node_id bigint NOT NULL,
    station_uid text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.node_types (
    id bigint NOT NULL,
    code text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    is_synthetic boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.node_types_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.node_types_id_seq OWNED BY public.node_types.id;

CREATE TABLE public.nodes (
    id bigint NOT NULL,
    name text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    is_synthetic boolean DEFAULT false NOT NULL,
    zone text DEFAULT ''::text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    depth integer,
    node_type_id bigint,
    parent_id bigint,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    claimed_by bigint
);

COMMENT ON COLUMN public.nodes.display_name IS 'The person-readable label (v144). The node''s own name is the fleet''s identity: scenesync matches the fleet''s location set by name and deletes a physical node the scene does not carry, so the cosmetic rename rides a column of its own; empty renders the name.';

CREATE SEQUENCE public.nodes_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.nodes_id_seq OWNED BY public.nodes.id;

CREATE TABLE public.order_bins (
    id bigint NOT NULL,
    order_id bigint NOT NULL,
    bin_id bigint NOT NULL,
    pick_step integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    drop_step integer,
    CONSTRAINT order_bins_steps CHECK ((((pick_step IS NOT NULL) OR (drop_step IS NULL)) AND ((drop_step IS NULL) OR (pick_step < drop_step))))
);

CREATE SEQUENCE public.order_bins_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.order_bins_id_seq OWNED BY public.order_bins.id;

CREATE TABLE public.order_history (
    id bigint NOT NULL,
    order_id bigint NOT NULL,
    status text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    code text,
    actor text,
    ref jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.order_history IS 'Every status transition an order made, in order. The canonical timing source: both endpoints of execution time come from here rather than mission_telemetry, because these rows are written by Core''s own state machine, on Core''s clock. code, actor and ref are the typed reason (v55); all three are NULL on rows written before v55 and on uncoded transitions.';

COMMENT ON COLUMN public.order_history.code IS 'protocol.TermCode on a terminal row, protocol.QueueCode on a to-queued row. Prefer it over parsing detail, which is prose.';

COMMENT ON COLUMN public.order_history.actor IS 'Who caused the transition.';

COMMENT ON COLUMN public.order_history.ref IS 'What the reason concerns, as JSONB: node, payload, peer. The reference is what makes a reason actionable: ref->>payload is the GROUP BY that turns no_source_bin into starvation by cause.';

CREATE SEQUENCE public.order_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.order_history_id_seq OWNED BY public.order_history.id;

CREATE TABLE public.order_intake_refusals (
    edge_uuid text NOT NULL,
    error_code text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    refused_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.order_intake_refusals IS 'The pair legs Core refused at intake (v115). A refused request leaves no order row, so the partner naming one could not tell refused from not-received-yet; the pair rule reads this when the partner''s row is absent.';

CREATE TABLE public.orders (
    id bigint NOT NULL,
    edge_uuid text NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    order_type text DEFAULT 'retrieve'::text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    quantity bigint DEFAULT 1 NOT NULL,
    source_node text DEFAULT ''::text NOT NULL,
    delivery_node text DEFAULT ''::text NOT NULL,
    process_node text DEFAULT ''::text NOT NULL,
    vendor_order_id text DEFAULT ''::text NOT NULL,
    vendor_state text DEFAULT ''::text NOT NULL,
    robot_id text DEFAULT ''::text NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    payload_desc text DEFAULT ''::text NOT NULL,
    error_detail text DEFAULT ''::text NOT NULL,
    steps_json text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    parent_order_id bigint,
    sequence integer DEFAULT 0 NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    wait_index integer DEFAULT 0 NOT NULL,
    queue_reason text DEFAULT ''::text NOT NULL,
    queue_code text,
    queue_cause text,
    skip_auto_confirm boolean DEFAULT false NOT NULL,
    sibling_order_uuid text DEFAULT ''::text NOT NULL,
    key_route text DEFAULT ''::text NOT NULL,
    key_task text DEFAULT ''::text NOT NULL,
    source_intent text DEFAULT ''::text NOT NULL,
    coordinated boolean DEFAULT false NOT NULL,
    remaining_uop integer,
    origin_id uuid,
    origin_class text DEFAULT ''::text NOT NULL,
    open_for_children boolean DEFAULT false NOT NULL,
    station_unaware boolean DEFAULT false NOT NULL,
    projects_to_station_uid text DEFAULT ''::text NOT NULL,
    plan text DEFAULT ''::text NOT NULL,
    orphan_aged_at timestamp with time zone,
    destination_resolved_at timestamp with time zone,
    recovers_order_id bigint,
    named_bin_id bigint,
    want text DEFAULT ''::text NOT NULL,
    steps_done integer DEFAULT 0 NOT NULL,
    block_actions jsonb DEFAULT '{}'::jsonb,
    version bigint DEFAULT 1 NOT NULL,
    asked_vendor_order_id text DEFAULT ''::text NOT NULL,
    requested_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT orders_one_requester CHECK (((station_uid = ''::text) OR (requested_by = ''::text))),
    CONSTRAINT orders_requested_by_known CHECK ((requested_by = ANY (ARRAY[''::text, 'core-operator'::text, 'core-direct'::text, 'core-test'::text]))),
    CONSTRAINT orders_station_uid_not_actor CHECK ((station_uid <> ALL (ARRAY['core-operator'::text, 'core-direct'::text, 'core-test'::text])))
);

COMMENT ON COLUMN public.orders.station_uid IS 'The station that sent the order or is its owner: an Edge''s uid, or blank for an order no station asked for (since v185, Core''s actors are in requested_by, and a CHECK keeps them out of this column).';

COMMENT ON COLUMN public.orders.origin_id IS 'The demand this order was created to serve. See origin_class.';

COMMENT ON COLUMN public.orders.origin_class IS 'Whether the order should have had a demand. It is what makes origin_id IS NULL answerable: without it that predicate selects every consume-side order, every opportunistic stage and every admin action, with the actual lost origins buried in there. Only orphan is a finding.';

COMMENT ON COLUMN public.orders.open_for_children IS 'Sealedness, on the compound parent: sealed = NOT open_for_children. Sealed is the concept''s name everywhere else (SealDigGroup), and this is the column that carries it. Named for the exception so the safe state is the zero value in both languages: false here and false for a bare orders.Order literal in Go, and both mean sealed. Only a compound parent means anything by it: a reshuffle that may still gain children is not finished when its current children are all terminal, and all children terminal is what AdvanceCompoundOrder''s success arm and AdvanceStuckReshuffleParents read as done. Born sealed by DEFAULT false; only SetCompoundOpen changes it.';

COMMENT ON COLUMN public.orders.station_unaware IS 'The station named on the order has no row for it: it did not send the order and the order does not project there. Its station is sent nothing about it. Named for the exception; false is every order but a Core-born one that does not project (v145).';

COMMENT ON COLUMN public.orders.projects_to_station_uid IS 'The station a station-unaware order''s delivered and picked_up notices go to; blank is nobody. Unread for a station-known order (v146).';

COMMENT ON COLUMN public.orders.plan IS 'The planner''s pick at birth, as JSON: the bin, its node and the bin row as the pick saw it. Acquiring reads it instead of picking again, and picks only when it no longer stands. Blank is no pick (v147).';

COMMENT ON COLUMN public.orders.orphan_aged_at IS 'When an order was judged an orphan (v61). A timestamp rather than a fourth origin_class, so aging records when a judgement was made without overwriting what the judgement was.';

COMMENT ON COLUMN public.orders.destination_resolved_at IS 'When the store-slot selector chose this order''s destination, at intake. Written only by admission (admit), only when resolveSyntheticDestination rewrote a group into a concrete slot; NULL means the destination was not chosen at intake (named concretely by the sender, or the group was full and planMove resolves it at dispatch). It exists for one reader, the burial shadow''s tripwire (service/burial_shadow.go), which has to tell a placement that skipped the guard from a guard consulted before the world moved, and so has to know when the guard looked: intake resolves before the order row exists, and the fleet commit can follow minutes later when the order queues behind capacity. Nullable, and the fallback is the old behaviour: an order without a stamp is judged against fleet commit.';

COMMENT ON COLUMN public.orders.recovers_order_id IS 'The cancelled order whose bin this order returns (v140). Set only on a Core-minted return for a bin left on a robot''s deck by a cancel; NULL on every other order.';

COMMENT ON COLUMN public.orders.named_bin_id IS 'The one bin a move is for, named by the station that sent it (a keep-staged spare''s return; v141). The bin hold and claim statements take that bin and no other, and only while it stands on the source node. NULL on every other order. No foreign key: a bin row deleted under a waiting move must read as the bin has left, not turn the move into one that names nothing.';

COMMENT ON COLUMN public.orders.want IS 'The height an empty is wanted at, as the station stated it on its request (protocol.OrderRequest.Want; v166): blank none, a decimal for that many layers, tops_bottoms for tops and bottoms only. Every find of the order reads it.';

COMMENT ON COLUMN public.orders.steps_done IS 'How many steps of the order''s plan the fleet has finished, by position (v167): the block subscriber advances it as each block finishes the plan''s next open step (an order with no stored plan counts its two implied steps). The route strip reads where the robot is from it.';

COMMENT ON COLUMN public.orders.block_actions IS 'What each block Core sent the fleet for this order is, by block id: a plan action (pickup, dropoff, wait) or blank for a block of no step''s kind (v179). Written by the create and by each append; the block subscriber reads a finished block''s meaning from it. NULL on an order sent before v179, read by the old name rule for its whole life.';

COMMENT ON COLUMN public.orders.version IS 'One counter for everything the order''s station is told (v181): bumped in the same statement as a status write, a robot write that names a new robot, and a change of wait the station is told. It rides the order.update, the reconcile snapshot and the projection, and the station applies only a newer one. Born 1, so a 0 on the wire is an older Core.';

COMMENT ON COLUMN public.orders.asked_vendor_order_id IS 'The vendor order id Core is asking the fleet to create, from the hand that takes the order''s holds until the fleet answers; blank when Core is not asking (v182). Core-only, never on the wire. An open ask is spoken for whatever the status says.';

COMMENT ON COLUMN public.orders.requested_by IS 'Which Core door made an order no station asked for: core-operator (the manual and spot doors), core-direct (the direct doors), core-test (the test doors), or blank (a station''s order, or one Core made itself, such as a recovery or a cancel-return). Until v185 the three were written into the station column.';

CREATE SEQUENCE public.orders_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.orders_id_seq OWNED BY public.orders.id;

CREATE TABLE public.outbox (
    id bigint NOT NULL,
    topic text NOT NULL,
    payload bytea NOT NULL,
    msg_type text DEFAULT ''::text NOT NULL,
    dst_station text DEFAULT ''::text NOT NULL,
    retries integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    sent_at timestamp with time zone
);

COMMENT ON COLUMN public.outbox.dst_station IS 'The addressee: a station''s uid, * for every station, or core-test. Not a station uid column (outbox.station_id until v185).';

CREATE SEQUENCE public.outbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.outbox_id_seq OWNED BY public.outbox.id;

CREATE TABLE public.parts (
    id bigint NOT NULL,
    part_number text NOT NULL,
    catid text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.parts IS 'A part under both of the names it is known by (v107): the part number CMS books against and the cat id a cell''s PLC declares. A table rather than two columns on payload_manifest because a cat id belongs to the part, not to the line that mentions it, and because a foreign key from the line is the one guard that cannot be forgotten at a door.';

CREATE SEQUENCE public.parts_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.parts_id_seq OWNED BY public.parts.id;

CREATE TABLE public.payload_bin_types (
    payload_id bigint NOT NULL,
    bin_type_id bigint NOT NULL
);

CREATE TABLE public.payload_containment (
    payload_code text NOT NULL,
    active boolean DEFAULT false NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    activated_by text DEFAULT ''::text NOT NULL,
    activated_at timestamp with time zone,
    deactivated_by text DEFAULT ''::text NOT NULL,
    deactivated_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.payload_containment IS 'The per-payload quality containment flag. A quality alert on a part sets active, and every FG-bound delivery of that payload diverts to its claim''s containment destination instead of the FG drop zone. The audit columns are written by the store on every set and clear: containment nobody can attribute is containment nobody can defend. One row per payload code; the row persists after deactivation so the history (who, when, why) survives the alert clearing.';

CREATE TABLE public.payload_manifest (
    id bigint NOT NULL,
    payload_id bigint NOT NULL,
    part_number text DEFAULT ''::text NOT NULL,
    parts_per_cycle bigint DEFAULT 1 NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    part_id bigint,
    CONSTRAINT payload_manifest_part_number_not_blank CHECK ((part_number <> ''::text))
);

COMMENT ON COLUMN public.payload_manifest.part_number IS 'Names a part, and has since it was created. Its values at the plants were cat ids, because the entry form asked for one: that was a data correction (v107, v108), not a reason to rename the column after the mistake.';

COMMENT ON COLUMN public.payload_manifest.parts_per_cycle IS 'A ratio, not a count: how many of this part one assembly cycle consumes, an assembly consume setting. A bin''s count of its parts is derived at read time from bins.uop_remaining and stored nowhere; the rule is protocol.ManifestItem''s. It was called quantity until v99 renamed it; the value did not change, because it was always the ratio. Default 1: a line with no stated ratio is one per cycle.';

CREATE SEQUENCE public.payload_manifest_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.payload_manifest_id_seq OWNED BY public.payload_manifest.id;

CREATE TABLE public.payloads (
    id bigint NOT NULL,
    code text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    uop_capacity integer DEFAULT 0 NOT NULL,
    robot_group text DEFAULT ''::text NOT NULL,
    advanced_load_sequence text DEFAULT ''::text NOT NULL,
    near_empty_enabled boolean DEFAULT false NOT NULL,
    near_empty_robot_group text DEFAULT ''::text NOT NULL,
    near_empty_threshold_pct integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    tote_layers integer,
    uop_per_tote integer,
    CONSTRAINT payloads_tote_layers_check CHECK ((tote_layers > 0)),
    CONSTRAINT payloads_uop_per_tote_check CHECK ((uop_per_tote > 0))
);

COMMENT ON COLUMN public.payloads.tote_layers IS 'The layers of totes a full cart stands (v161). NULL is unknown.';

COMMENT ON COLUMN public.payloads.uop_per_tote IS 'The UOP (in the capacity''s unit: pieces, for a payload of one part) that fill one tote (v161). NULL is unknown.';

CREATE SEQUENCE public.payloads_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.payloads_id_seq OWNED BY public.payloads.id;

CREATE TABLE public.plant_claims_reports (
    process_id text NOT NULL,
    station_uid text NOT NULL,
    digest text NOT NULL,
    received_at timestamp with time zone NOT NULL
);

COMMENT ON TABLE public.plant_claims_reports IS 'One row per station and process: the digest of the last plant-claims report that station sent for the process (v143; keyed by station and process since v186). Each station is quoted its own rows on its heartbeat ack, so two stations reporting one process each stop re-sending; the mirror (process_styles, style_claims) holds the last writer''s report. Two rows for one process are the reported-by-both flag on the Edges page. An empty report deletes every row of its process, with the mirror.';

CREATE TABLE public.plant_confidence_daily (
    day date NOT NULL,
    samples_read bigint DEFAULT 0 NOT NULL,
    orphan_samples bigint DEFAULT 0 NOT NULL,
    unkeyable_edges integer DEFAULT 0 NOT NULL,
    unkeyable_samples bigint DEFAULT 0 NOT NULL,
    unversioned_samples bigint DEFAULT 0 NOT NULL,
    map_mismatch_samples bigint DEFAULT 0 NOT NULL,
    unattributed_samples bigint DEFAULT 0 NOT NULL,
    robot_rows integer DEFAULT 0 NOT NULL,
    lane_rows integer DEFAULT 0 NOT NULL,
    area_rows integer DEFAULT 0 NOT NULL,
    residuals_null integer DEFAULT 0 NOT NULL,
    lanes_fail_only integer DEFAULT 0 NOT NULL,
    lane_robot_rows integer DEFAULT 0 NOT NULL
);

COMMENT ON TABLE public.plant_confidence_daily IS 'The plant-day record for counts that have no lane to hang on: orphans, unkeyable, unversioned, unattributed (v82).';

CREATE TABLE public.process_positions (
    process_id text NOT NULL,
    core_node_name text NOT NULL
);

COMMENT ON TABLE public.process_positions IS 'The seats a process binds at that no claim names (loader windows and homes, FG drains), mirrored from plant.claims for addressing only (v152).';

CREATE TABLE public.process_styles (
    process_id text NOT NULL,
    style_id text NOT NULL,
    config_gen bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    is_active boolean DEFAULT false NOT NULL,
    station_uid text
);

CREATE TABLE public.recovery_actions (
    id bigint NOT NULL,
    action text NOT NULL,
    target_type text NOT NULL,
    target_id bigint DEFAULT 0 NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    actor text DEFAULT 'system'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.recovery_actions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.recovery_actions_id_seq OWNED BY public.recovery_actions.id;

CREATE TABLE public.reservations (
    id bigint NOT NULL,
    order_id bigint NOT NULL,
    bin_id bigint,
    state text DEFAULT 'pending'::text NOT NULL,
    reserved_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    resource_kind text DEFAULT 'bin'::text NOT NULL,
    node_id bigint,
    mode text,
    CONSTRAINT reservations_kind_target_check CHECK ((((resource_kind = 'bin'::text) AND (bin_id IS NOT NULL) AND (node_id IS NULL)) OR ((resource_kind = ANY (ARRAY['slot'::text, 'mouth'::text, 'occupancy'::text])) AND (node_id IS NOT NULL) AND (bin_id IS NULL)))),
    CONSTRAINT reservations_mode_check CHECK (((mode IS NULL) OR (mode = ANY (ARRAY['inbound'::text, 'outbound'::text, 'dig'::text])))),
    CONSTRAINT reservations_resource_kind_check CHECK ((resource_kind = ANY (ARRAY['bin'::text, 'slot'::text, 'mouth'::text, 'occupancy'::text]))),
    CONSTRAINT reservations_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'confirmed'::text])))
);

CREATE SEQUENCE public.reservations_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.reservations_id_seq OWNED BY public.reservations.id;

CREATE TABLE public.robot_alarm_onsets (
    hour timestamp with time zone NOT NULL,
    vehicle_id text NOT NULL,
    code integer NOT NULL,
    onsets integer NOT NULL,
    first_at timestamp with time zone NOT NULL
);

COMMENT ON TABLE public.robot_alarm_onsets IS 'Alarm onsets per robot per hour per code, permanent (v158), past RDS''s five-week alarm retention; read by the Fleet tab and the cleaning trial.';

CREATE TABLE public.robot_confidence_daily (
    day date NOT NULL,
    vehicle_id text NOT NULL,
    residual double precision,
    cells integer NOT NULL,
    samples integer NOT NULL,
    mean double precision,
    p05 double precision,
    map_mismatch_samples integer DEFAULT 0 NOT NULL
);

COMMENT ON TABLE public.robot_confidence_daily IS 'The permanent per-robot residual roll-up (v77).';

CREATE TABLE public.robot_confidence_low (
    id bigint NOT NULL,
    vehicle_id text NOT NULL,
    sampled_at timestamp with time zone NOT NULL,
    confidence double precision NOT NULL,
    x double precision NOT NULL,
    y double precision NOT NULL,
    angle double precision NOT NULL,
    rds_station text DEFAULT ''::text NOT NULL,
    last_rds_station text DEFAULT ''::text NOT NULL,
    order_id bigint DEFAULT 0 NOT NULL,
    on_task boolean DEFAULT false NOT NULL,
    blocked boolean DEFAULT false NOT NULL,
    reloc_status smallint NOT NULL,
    area_ids text[],
    map_md5 text,
    alarm_codes integer[]
)
PARTITION BY RANGE (sampled_at);

COMMENT ON TABLE public.robot_confidence_low IS 'The long-retention low-confidence forensic trail (v77).';

CREATE SEQUENCE public.robot_confidence_low_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.robot_confidence_low_id_seq OWNED BY public.robot_confidence_low.id;

CREATE TABLE public.robot_confidence_samples (
    id bigint NOT NULL,
    vehicle_id text NOT NULL,
    sampled_at timestamp with time zone NOT NULL,
    confidence double precision NOT NULL,
    x double precision NOT NULL,
    y double precision NOT NULL,
    angle double precision NOT NULL,
    rds_station text DEFAULT ''::text NOT NULL,
    last_rds_station text DEFAULT ''::text NOT NULL,
    order_id bigint DEFAULT 0 NOT NULL,
    on_task boolean DEFAULT false NOT NULL,
    blocked boolean DEFAULT false NOT NULL,
    reloc_status smallint NOT NULL,
    area_ids text[],
    map_md5 text,
    alarm_codes integer[]
)
PARTITION BY RANGE (sampled_at);

COMMENT ON TABLE public.robot_confidence_samples IS 'Raw localization-confidence samples, daily partitions (v77).';

COMMENT ON COLUMN public.robot_confidence_samples.rds_station IS 'The RDS map point the robot reported at the sample: a fleet name, not a station.';

COMMENT ON COLUMN public.robot_confidence_samples.last_rds_station IS 'The RDS map point the robot last stood at: a fleet name, not a station.';

CREATE SEQUENCE public.robot_confidence_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.robot_confidence_samples_id_seq OWNED BY public.robot_confidence_samples.id;

CREATE TABLE public.robot_deviceconfig_pulls (
    id bigint NOT NULL,
    vehicle_id text NOT NULL,
    pulled_at timestamp with time zone DEFAULT now() NOT NULL,
    model_md5 text NOT NULL,
    params jsonb NOT NULL,
    config jsonb NOT NULL
);

COMMENT ON TABLE public.robot_deviceconfig_pulls IS 'The Device Config tab''s pull history (v184): one append-only row per robot per pull. params is the flattened leaf set the matrix reads (a tab open never re-parses a robot''s whole model); config is the SANITIZED whole model: credential values are SHA-256 fingerprints before either column is written, because a robot''s live model carries a wifi password in the clear. model_md5 hashes the raw bytes at pull time, kept as provenance only: the fleet''s status envelope model_md5 matches no robot''s file, so it is not a staleness signal and nothing reads this column for one.';

CREATE SEQUENCE public.robot_deviceconfig_pulls_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.robot_deviceconfig_pulls_id_seq OWNED BY public.robot_deviceconfig_pulls.id;

CREATE TABLE public.robot_hours (
    hour timestamp with time zone NOT NULL,
    vehicle_id text NOT NULL,
    connected_s integer DEFAULT 0 NOT NULL,
    charging_s integer DEFAULT 0 NOT NULL,
    blocked_s integer DEFAULT 0 NOT NULL,
    metres double precision DEFAULT 0 NOT NULL,
    charge_starts integer DEFAULT 0 NOT NULL,
    battery_min double precision,
    lift_count integer
);

COMMENT ON TABLE public.robot_hours IS 'The 2-second robot poll kept as totals per robot per UTC hour, permanent (v158); read by the Robots page''s Fleet tab and the cleaning trial (robot-days, km).';

CREATE TABLE public.robot_lidar_hours (
    hour timestamp with time zone NOT NULL,
    vehicle_id text NOT NULL,
    lidar text NOT NULL,
    rds_station text NOT NULL,
    use_for_loc boolean NOT NULL,
    beams integer NOT NULL,
    valid integer NOT NULL,
    rssi_med_valid double precision,
    rssi_p25_valid double precision,
    rssi_p75_valid double precision,
    near_valid integer NOT NULL
);

COMMENT ON TABLE public.robot_lidar_hours IS 'One laser frame per parked robot per hour reduced per lidar, 90-day retention (v158); read per robot against itself by the cleaning trial''s before/after time join.';

CREATE TABLE public.scene_areas (
    id bigint NOT NULL,
    area_name text NOT NULL,
    class_name text NOT NULL,
    polygon jsonb NOT NULL,
    reflector_count integer DEFAULT 0 NOT NULL,
    color_pen bigint,
    color_brush bigint,
    properties jsonb,
    shape_hash text NOT NULL,
    def_hash text NOT NULL,
    max_vertex_delta_m double precision,
    supersedes_id bigint,
    diff_id bigint NOT NULL,
    map_version_id bigint,
    valid_from timestamp with time zone NOT NULL,
    valid_to timestamp with time zone
);

COMMENT ON TABLE public.scene_areas IS 'Declared map areas, temporally versioned, parsed from the robot .smap (v81).';

CREATE SEQUENCE public.scene_areas_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_areas_id_seq OWNED BY public.scene_areas.id;

CREATE TABLE public.scene_diffs (
    id bigint NOT NULL,
    source text NOT NULL,
    gate_hash text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    previous_sync timestamp with time zone,
    objects_added integer DEFAULT 0 NOT NULL,
    objects_changed integer DEFAULT 0 NOT NULL,
    objects_removed integer DEFAULT 0 NOT NULL,
    median_delta_m double precision,
    max_delta_m double precision
);

COMMENT ON TABLE public.scene_diffs IS 'One row per observed map edit (v81): the join that relates an RDS /scene edit to a robot .smap edit made in the same sitting.';

CREATE SEQUENCE public.scene_diffs_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_diffs_id_seq OWNED BY public.scene_diffs.id;

CREATE TABLE public.scene_edges (
    id bigint NOT NULL,
    area_name text NOT NULL,
    instance_name text NOT NULL,
    class_name text DEFAULT ''::text NOT NULL,
    from_name text DEFAULT ''::text NOT NULL,
    to_name text DEFAULT ''::text NOT NULL,
    from_x double precision DEFAULT 0 NOT NULL,
    from_y double precision DEFAULT 0 NOT NULL,
    to_x double precision DEFAULT 0 NOT NULL,
    to_y double precision DEFAULT 0 NOT NULL,
    synced_at timestamp with time zone DEFAULT now() NOT NULL,
    ctrl1_x double precision,
    ctrl1_y double precision,
    ctrl2_x double precision,
    ctrl2_y double precision
);

CREATE SEQUENCE public.scene_edges_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_edges_id_seq OWNED BY public.scene_edges.id;

CREATE TABLE public.scene_lane_versions (
    id bigint NOT NULL,
    area_name text NOT NULL,
    lane text NOT NULL,
    shape_hash text NOT NULL,
    def_hash text NOT NULL,
    shape jsonb NOT NULL,
    directed_rows smallint NOT NULL,
    twins_agree boolean DEFAULT true NOT NULL,
    disagreement text DEFAULT ''::text NOT NULL,
    max_vertex_delta_m double precision,
    supersedes_id bigint,
    diff_id bigint NOT NULL,
    valid_from timestamp with time zone NOT NULL,
    valid_to timestamp with time zone
);

COMMENT ON TABLE public.scene_lane_versions IS 'Per-lane geometry versions (v81): what lane_confidence_daily.version_id points at.';

CREATE SEQUENCE public.scene_lane_versions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_lane_versions_id_seq OWNED BY public.scene_lane_versions.id;

CREATE TABLE public.scene_map_versions (
    id bigint NOT NULL,
    map_name text NOT NULL,
    content_sha text NOT NULL,
    map_md5 text DEFAULT ''::text NOT NULL,
    source_robot text DEFAULT ''::text NOT NULL,
    body_gz bytea,
    scan_cloud_gz bytea,
    raw_bytes bigint DEFAULT 0 NOT NULL,
    synced_at timestamp with time zone NOT NULL,
    superseded_at timestamp with time zone,
    diff_id bigint,
    confirmed_at timestamp with time zone
);

COMMENT ON TABLE public.scene_map_versions IS 'The archived .smap blob, gzipped, the scan cloud in its own column (v81).';

CREATE SEQUENCE public.scene_map_versions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_map_versions_id_seq OWNED BY public.scene_map_versions.id;

CREATE TABLE public.scene_points (
    id bigint NOT NULL,
    area_name text NOT NULL,
    instance_name text NOT NULL,
    class_name text NOT NULL,
    point_name text DEFAULT ''::text NOT NULL,
    group_name text DEFAULT ''::text NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    pos_x double precision DEFAULT 0 NOT NULL,
    pos_y double precision DEFAULT 0 NOT NULL,
    pos_z double precision DEFAULT 0 NOT NULL,
    dir double precision DEFAULT 0 NOT NULL,
    properties_json jsonb DEFAULT '{}'::jsonb NOT NULL,
    synced_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.scene_points_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_points_id_seq OWNED BY public.scene_points.id;

CREATE TABLE public.scene_reflectors (
    id bigint NOT NULL,
    kind text NOT NULL,
    x double precision NOT NULL,
    y double precision NOT NULL,
    width double precision,
    shape_hash text NOT NULL,
    diff_id bigint NOT NULL,
    map_version_id bigint,
    valid_from timestamp with time zone NOT NULL,
    valid_to timestamp with time zone
);

COMMENT ON TABLE public.scene_reflectors IS 'Reflector positions, temporally versioned; identity is the position (v81).';

CREATE SEQUENCE public.scene_reflectors_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.scene_reflectors_id_seq OWNED BY public.scene_reflectors.id;

CREATE TABLE public.schema_migrations (
    version integer NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.schema_migrations IS 'The migration runner''s own bookkeeping (protocol/migrate): one row per applied version. A database born on the baseline records the whole retired chain.';

CREATE TABLE public.sourceability_events (
    id bigint NOT NULL,
    process_key text NOT NULL,
    style_id text DEFAULT ''::text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    sourceable boolean NOT NULL,
    status text DEFAULT ''::text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    missing_payload text DEFAULT ''::text NOT NULL,
    op text DEFAULT 'sourceability_change'::text NOT NULL,
    source text DEFAULT ''::text NOT NULL,
    actor text DEFAULT 'system'::text NOT NULL,
    metadata jsonb,
    observed_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.sourceability_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.sourceability_events_id_seq OWNED BY public.sourceability_events.id;

CREATE TABLE public.style_claims (
    process_id text NOT NULL,
    style_id text NOT NULL,
    core_node_name text NOT NULL,
    role text NOT NULL,
    swap_mode text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    allowed_payload_codes text DEFAULT '[]'::text NOT NULL,
    uop_capacity integer DEFAULT 0 NOT NULL,
    reorder_point integer DEFAULT 0 NOT NULL,
    seq integer DEFAULT 0 NOT NULL,
    inbound_source text DEFAULT ''::text NOT NULL,
    outbound_destination text DEFAULT ''::text NOT NULL,
    paired_core_node text DEFAULT ''::text NOT NULL,
    second_paired_core_node text DEFAULT ''::text NOT NULL,
    containment_destination text DEFAULT ''::text NOT NULL,
    partial_destination text DEFAULT ''::text NOT NULL
);

CREATE TABLE public.supply_refusals (
    id bigint NOT NULL,
    loader_node text NOT NULL,
    payload_code text NOT NULL,
    refused_at timestamp with time zone NOT NULL,
    refused_by text DEFAULT ''::text NOT NULL,
    ack_at timestamp with time zone,
    ack_choice text DEFAULT ''::text NOT NULL,
    ack_process_id text DEFAULT ''::text NOT NULL,
    closed_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.supply_refusals_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.supply_refusals_id_seq OWNED BY public.supply_refusals.id;

CREATE TABLE public.test_commands (
    id bigint NOT NULL,
    command_type text NOT NULL,
    robot_id text NOT NULL,
    vendor_order_id text DEFAULT ''::text NOT NULL,
    vendor_state text DEFAULT ''::text NOT NULL,
    location text DEFAULT ''::text NOT NULL,
    config_id text DEFAULT ''::text NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone
);

CREATE SEQUENCE public.test_commands_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.test_commands_id_seq OWNED BY public.test_commands.id;

CREATE TABLE public.tte_samples (
    id bigint NOT NULL,
    computed_at timestamp with time zone NOT NULL,
    process_id text NOT NULL,
    style_id text NOT NULL,
    core_node_name text NOT NULL,
    payload_code text NOT NULL,
    uop_remaining integer NOT NULL,
    rate_per_sec double precision NOT NULL,
    tte_seconds double precision,
    style_status text NOT NULL,
    reorder_point integer DEFAULT 0 NOT NULL,
    rate_grain text DEFAULT ''::text NOT NULL,
    kind text DEFAULT 'cell'::text NOT NULL
);

COMMENT ON TABLE public.tte_samples IS 'The per-line time-to-empty the sourceability pass computes on every full pass (v118). Written only by the two-minute full recompute, self-pruning at 45 days.';

CREATE SEQUENCE public.tte_samples_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.tte_samples_id_seq OWNED BY public.tte_samples.id;

ALTER TABLE ONLY public.admin_users ALTER COLUMN id SET DEFAULT nextval('public.admin_users_id_seq'::regclass);

ALTER TABLE ONLY public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq'::regclass);

ALTER TABLE ONLY public.bin_loaders ALTER COLUMN id SET DEFAULT nextval('public.bin_loaders_id_seq'::regclass);

ALTER TABLE ONLY public.bin_types ALTER COLUMN id SET DEFAULT nextval('public.bin_types_id_seq'::regclass);

ALTER TABLE ONLY public.bin_uop_exception ALTER COLUMN id SET DEFAULT nextval('public.bin_uop_exception_id_seq'::regclass);

ALTER TABLE ONLY public.bin_uop_ledger ALTER COLUMN id SET DEFAULT nextval('public.bin_uop_ledger_id_seq'::regclass);

ALTER TABLE ONLY public.bins ALTER COLUMN id SET DEFAULT nextval('public.bins_id_seq'::regclass);

ALTER TABLE ONLY public.carrier_families ALTER COLUMN id SET DEFAULT nextval('public.carrier_families_id_seq'::regclass);

ALTER TABLE ONLY public.cell_part_events ALTER COLUMN id SET DEFAULT nextval('public.cell_part_events_id_seq'::regclass);

ALTER TABLE ONLY public.cleaning_missions ALTER COLUMN id SET DEFAULT nextval('public.cleaning_missions_id_seq'::regclass);

ALTER TABLE ONLY public.cms_postings ALTER COLUMN id SET DEFAULT nextval('public.cms_postings_id_seq'::regclass);

ALTER TABLE ONLY public.cms_transactions ALTER COLUMN id SET DEFAULT nextval('public.cms_transactions_id_seq'::regclass);

ALTER TABLE ONLY public.dashboards ALTER COLUMN id SET DEFAULT nextval('public.dashboards_id_seq'::regclass);

ALTER TABLE ONLY public.demand_registry ALTER COLUMN id SET DEFAULT nextval('public.demand_registry_id_seq'::regclass);

ALTER TABLE ONLY public.edge_registry ALTER COLUMN id SET DEFAULT nextval('public.edge_registry_id_seq'::regclass);

ALTER TABLE ONLY public.lineside_buckets ALTER COLUMN id SET DEFAULT nextval('public.lineside_buckets_id_seq'::regclass);

ALTER TABLE ONLY public.lineside_drain_ledger ALTER COLUMN id SET DEFAULT nextval('public.lineside_drain_ledger_id_seq'::regclass);

ALTER TABLE ONLY public.mission_events ALTER COLUMN id SET DEFAULT nextval('public.mission_events_id_seq'::regclass);

ALTER TABLE ONLY public.mission_telemetry ALTER COLUMN id SET DEFAULT nextval('public.mission_telemetry_id_seq'::regclass);

ALTER TABLE ONLY public.node_properties ALTER COLUMN id SET DEFAULT nextval('public.node_properties_id_seq'::regclass);

ALTER TABLE ONLY public.node_types ALTER COLUMN id SET DEFAULT nextval('public.node_types_id_seq'::regclass);

ALTER TABLE ONLY public.nodes ALTER COLUMN id SET DEFAULT nextval('public.nodes_id_seq'::regclass);

ALTER TABLE ONLY public.order_bins ALTER COLUMN id SET DEFAULT nextval('public.order_bins_id_seq'::regclass);

ALTER TABLE ONLY public.order_history ALTER COLUMN id SET DEFAULT nextval('public.order_history_id_seq'::regclass);

ALTER TABLE ONLY public.orders ALTER COLUMN id SET DEFAULT nextval('public.orders_id_seq'::regclass);

ALTER TABLE ONLY public.outbox ALTER COLUMN id SET DEFAULT nextval('public.outbox_id_seq'::regclass);

ALTER TABLE ONLY public.parts ALTER COLUMN id SET DEFAULT nextval('public.parts_id_seq'::regclass);

ALTER TABLE ONLY public.payload_manifest ALTER COLUMN id SET DEFAULT nextval('public.payload_manifest_id_seq'::regclass);

ALTER TABLE ONLY public.payloads ALTER COLUMN id SET DEFAULT nextval('public.payloads_id_seq'::regclass);

ALTER TABLE ONLY public.recovery_actions ALTER COLUMN id SET DEFAULT nextval('public.recovery_actions_id_seq'::regclass);

ALTER TABLE ONLY public.reservations ALTER COLUMN id SET DEFAULT nextval('public.reservations_id_seq'::regclass);

ALTER TABLE ONLY public.robot_confidence_low ALTER COLUMN id SET DEFAULT nextval('public.robot_confidence_low_id_seq'::regclass);

ALTER TABLE ONLY public.robot_confidence_samples ALTER COLUMN id SET DEFAULT nextval('public.robot_confidence_samples_id_seq'::regclass);

ALTER TABLE ONLY public.robot_deviceconfig_pulls ALTER COLUMN id SET DEFAULT nextval('public.robot_deviceconfig_pulls_id_seq'::regclass);

ALTER TABLE ONLY public.scene_areas ALTER COLUMN id SET DEFAULT nextval('public.scene_areas_id_seq'::regclass);

ALTER TABLE ONLY public.scene_diffs ALTER COLUMN id SET DEFAULT nextval('public.scene_diffs_id_seq'::regclass);

ALTER TABLE ONLY public.scene_edges ALTER COLUMN id SET DEFAULT nextval('public.scene_edges_id_seq'::regclass);

ALTER TABLE ONLY public.scene_lane_versions ALTER COLUMN id SET DEFAULT nextval('public.scene_lane_versions_id_seq'::regclass);

ALTER TABLE ONLY public.scene_map_versions ALTER COLUMN id SET DEFAULT nextval('public.scene_map_versions_id_seq'::regclass);

ALTER TABLE ONLY public.scene_points ALTER COLUMN id SET DEFAULT nextval('public.scene_points_id_seq'::regclass);

ALTER TABLE ONLY public.scene_reflectors ALTER COLUMN id SET DEFAULT nextval('public.scene_reflectors_id_seq'::regclass);

ALTER TABLE ONLY public.sourceability_events ALTER COLUMN id SET DEFAULT nextval('public.sourceability_events_id_seq'::regclass);

ALTER TABLE ONLY public.supply_refusals ALTER COLUMN id SET DEFAULT nextval('public.supply_refusals_id_seq'::regclass);

ALTER TABLE ONLY public.test_commands ALTER COLUMN id SET DEFAULT nextval('public.test_commands_id_seq'::regclass);

ALTER TABLE ONLY public.tte_samples ALTER COLUMN id SET DEFAULT nextval('public.tte_samples_id_seq'::regclass);

ALTER TABLE ONLY public.admin_users
    ADD CONSTRAINT admin_users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.admin_users
    ADD CONSTRAINT admin_users_username_key UNIQUE (username);

ALTER TABLE ONLY public.area_confidence_daily
    ADD CONSTRAINT area_confidence_daily_pkey PRIMARY KEY (day, area_name);

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bin_loader_home_bin_types
    ADD CONSTRAINT bin_loader_home_bin_types_pkey PRIMARY KEY (position_node_id, bin_type_id);

ALTER TABLE ONLY public.bin_loader_homes
    ADD CONSTRAINT bin_loader_homes_position_node_id_key UNIQUE (position_node_id);

ALTER TABLE ONLY public.bin_loader_payloads
    ADD CONSTRAINT bin_loader_payloads_pkey PRIMARY KEY (loader_id, payload_code);

ALTER TABLE ONLY public.bin_loader_quotas
    ADD CONSTRAINT bin_loader_quotas_pkey PRIMARY KEY (loader_id, bin_type_id);

ALTER TABLE ONLY public.bin_loaders
    ADD CONSTRAINT bin_loaders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bin_types
    ADD CONSTRAINT bin_types_bare_of_key UNIQUE (bare_of);

ALTER TABLE ONLY public.bin_types
    ADD CONSTRAINT bin_types_code_key UNIQUE (code);

ALTER TABLE ONLY public.bin_types
    ADD CONSTRAINT bin_types_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bin_uop_delta_daily
    ADD CONSTRAINT bin_uop_delta_daily_pkey PRIMARY KEY (day, bin_id, epoch_seq, payload_code, reason, station_uid);

ALTER TABLE ONLY public.bin_uop_exception
    ADD CONSTRAINT bin_uop_exception_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bin_uop_ledger
    ADD CONSTRAINT bin_uop_ledger_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.carrier_families
    ADD CONSTRAINT carrier_families_code_key UNIQUE (code);

ALTER TABLE ONLY public.carrier_families
    ADD CONSTRAINT carrier_families_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.cell_config
    ADD CONSTRAINT cell_config_pkey PRIMARY KEY (cell_label);

ALTER TABLE ONLY public.cleaning_mission_setup
    ADD CONSTRAINT cleaning_mission_setup_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.cleaning_missions
    ADD CONSTRAINT cleaning_missions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.cleaning_opt_outs
    ADD CONSTRAINT cleaning_opt_outs_pkey PRIMARY KEY (vehicle_id);

ALTER TABLE ONLY public.cms_postings
    ADD CONSTRAINT cms_postings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.dashboards
    ADD CONSTRAINT dashboards_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.demand_origins
    ADD CONSTRAINT demand_origins_pkey PRIMARY KEY (origin_id);

ALTER TABLE ONLY public.demand_registry
    ADD CONSTRAINT demand_registry_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.demand_registry
    ADD CONSTRAINT demand_registry_station_uid_core_node_name_payload_code_key UNIQUE (station_uid, core_node_name, payload_code);

ALTER TABLE ONLY public.edge_cells
    ADD CONSTRAINT edge_cells_pkey PRIMARY KEY (station_uid, cell_label);

ALTER TABLE ONLY public.edge_lineside_reports
    ADD CONSTRAINT edge_lineside_reports_pkey PRIMARY KEY (station_uid, core_node_name, payload_code);

ALTER TABLE ONLY public.edge_registry
    ADD CONSTRAINT edge_registry_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.edge_registry
    ADD CONSTRAINT edge_registry_station_uid_key UNIQUE (station_uid);

ALTER TABLE ONLY public.inbox
    ADD CONSTRAINT inbox_pkey PRIMARY KEY (msg_id);

ALTER TABLE ONLY public.inventory_delta_dedup
    ADD CONSTRAINT inventory_delta_dedup_pkey PRIMARY KEY (station_uid, scope_kind, scope_key, epoch);

ALTER TABLE ONLY public.kanban_families
    ADD CONSTRAINT kanban_families_pkey PRIMARY KEY (kanban_node_id, family_id);

ALTER TABLE ONLY public.kanban_layer_levels
    ADD CONSTRAINT kanban_layer_levels_pkey PRIMARY KEY (kanban_node_id, bin_type_id, layers);

ALTER TABLE ONLY public.kanban_levels
    ADD CONSTRAINT kanban_levels_pkey PRIMARY KEY (kanban_node_id, bin_type_id);

ALTER TABLE ONLY public.kanban_sources
    ADD CONSTRAINT kanban_sources_kanban_node_id_source_node_id_key UNIQUE (kanban_node_id, source_node_id);

ALTER TABLE ONLY public.kanban_sources
    ADD CONSTRAINT kanban_sources_pkey PRIMARY KEY (kanban_node_id, rank);

ALTER TABLE ONLY public.kanban_supports
    ADD CONSTRAINT kanban_supports_pkey PRIMARY KEY (kanban_node_id, process_node_id);

ALTER TABLE ONLY public.kanbans
    ADD CONSTRAINT kanbans_pkey PRIMARY KEY (node_id);

ALTER TABLE ONLY public.lane_confidence_daily
    ADD CONSTRAINT lane_confidence_daily_pkey PRIMARY KEY (day, area_name, lane, version_id);

ALTER TABLE ONLY public.lane_robot_confidence_daily
    ADD CONSTRAINT lane_robot_confidence_daily_pkey PRIMARY KEY (day, area_name, lane, version_id, vehicle_id);

ALTER TABLE ONLY public.lineside_buckets
    ADD CONSTRAINT lineside_buckets_node_payload_state_key UNIQUE (core_node_name, payload_code, state);

ALTER TABLE ONLY public.lineside_buckets
    ADD CONSTRAINT lineside_buckets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.lineside_drain_ledger
    ADD CONSTRAINT lineside_drain_ledger_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.load_sequences
    ADD CONSTRAINT load_sequences_pkey PRIMARY KEY (name);

ALTER TABLE ONLY public.mission_events
    ADD CONSTRAINT mission_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.mission_telemetry
    ADD CONSTRAINT mission_telemetry_order_id_key UNIQUE (order_id);

ALTER TABLE ONLY public.mission_telemetry
    ADD CONSTRAINT mission_telemetry_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.node_bin_types
    ADD CONSTRAINT node_bin_types_pkey PRIMARY KEY (node_id, bin_type_id);

ALTER TABLE ONLY public.node_payloads
    ADD CONSTRAINT node_payloads_pkey PRIMARY KEY (node_id, payload_id);

ALTER TABLE ONLY public.node_properties
    ADD CONSTRAINT node_properties_node_id_key_key UNIQUE (node_id, key);

ALTER TABLE ONLY public.node_properties
    ADD CONSTRAINT node_properties_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.node_stations
    ADD CONSTRAINT node_stations_pkey PRIMARY KEY (node_id, station_uid);

ALTER TABLE ONLY public.node_types
    ADD CONSTRAINT node_types_code_key UNIQUE (code);

ALTER TABLE ONLY public.node_types
    ADD CONSTRAINT node_types_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_name_key UNIQUE (name);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.order_bins
    ADD CONSTRAINT order_bins_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.order_history
    ADD CONSTRAINT order_history_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.order_intake_refusals
    ADD CONSTRAINT order_intake_refusals_pkey PRIMARY KEY (edge_uuid);

ALTER TABLE public.orders
    ADD CONSTRAINT orders_dispatched_names_its_job CHECK (((status <> 'dispatched'::text) OR (vendor_order_id <> ''::text))) NOT VALID;

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.outbox
    ADD CONSTRAINT outbox_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.parts
    ADD CONSTRAINT parts_part_number_key UNIQUE (part_number);

ALTER TABLE ONLY public.parts
    ADD CONSTRAINT parts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.payload_bin_types
    ADD CONSTRAINT payload_bin_types_pkey PRIMARY KEY (payload_id, bin_type_id);

ALTER TABLE ONLY public.payload_containment
    ADD CONSTRAINT payload_containment_pkey PRIMARY KEY (payload_code);

ALTER TABLE ONLY public.payload_manifest
    ADD CONSTRAINT payload_manifest_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.payloads
    ADD CONSTRAINT payloads_code_key UNIQUE (code);

ALTER TABLE ONLY public.payloads
    ADD CONSTRAINT payloads_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.plant_claims_reports
    ADD CONSTRAINT plant_claims_reports_pkey PRIMARY KEY (station_uid, process_id);

ALTER TABLE ONLY public.plant_confidence_daily
    ADD CONSTRAINT plant_confidence_daily_pkey PRIMARY KEY (day);

ALTER TABLE ONLY public.process_positions
    ADD CONSTRAINT process_positions_pkey PRIMARY KEY (process_id, core_node_name);

ALTER TABLE ONLY public.process_styles
    ADD CONSTRAINT process_styles_pkey PRIMARY KEY (process_id, style_id);

ALTER TABLE ONLY public.recovery_actions
    ADD CONSTRAINT recovery_actions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.robot_alarm_onsets
    ADD CONSTRAINT robot_alarm_onsets_pkey PRIMARY KEY (hour, vehicle_id, code);

ALTER TABLE ONLY public.robot_confidence_daily
    ADD CONSTRAINT robot_confidence_daily_pkey PRIMARY KEY (day, vehicle_id);

ALTER TABLE ONLY public.robot_deviceconfig_pulls
    ADD CONSTRAINT robot_deviceconfig_pulls_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.robot_hours
    ADD CONSTRAINT robot_hours_pkey PRIMARY KEY (hour, vehicle_id);

ALTER TABLE ONLY public.robot_lidar_hours
    ADD CONSTRAINT robot_lidar_hours_pkey PRIMARY KEY (hour, vehicle_id, lidar);

ALTER TABLE ONLY public.scene_areas
    ADD CONSTRAINT scene_areas_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_diffs
    ADD CONSTRAINT scene_diffs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_edges
    ADD CONSTRAINT scene_edges_area_name_instance_name_key UNIQUE (area_name, instance_name);

ALTER TABLE ONLY public.scene_edges
    ADD CONSTRAINT scene_edges_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_lane_versions
    ADD CONSTRAINT scene_lane_versions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_map_versions
    ADD CONSTRAINT scene_map_versions_map_name_content_sha_key UNIQUE (map_name, content_sha);

ALTER TABLE ONLY public.scene_map_versions
    ADD CONSTRAINT scene_map_versions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_points
    ADD CONSTRAINT scene_points_area_name_instance_name_key UNIQUE (area_name, instance_name);

ALTER TABLE ONLY public.scene_points
    ADD CONSTRAINT scene_points_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_reflectors
    ADD CONSTRAINT scene_reflectors_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);

ALTER TABLE ONLY public.sourceability_events
    ADD CONSTRAINT sourceability_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.supply_refusals
    ADD CONSTRAINT supply_refusals_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.test_commands
    ADD CONSTRAINT test_commands_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.tte_samples
    ADD CONSTRAINT tte_samples_pkey PRIMARY KEY (id);

CREATE INDEX cell_config_station_uid_idx ON public.cell_config USING btree (station_uid);

CREATE INDEX idx_area_confidence_daily_area ON public.area_confidence_daily USING btree (area_name, day DESC);

CREATE INDEX idx_audit_entity ON public.audit_log USING btree (entity_type, entity_id);

CREATE INDEX idx_bin_loader_homes_loader ON public.bin_loader_homes USING btree (loader_id);

CREATE INDEX idx_bin_types_family ON public.bin_types USING btree (family_id);

CREATE INDEX idx_bin_uop_exception_bin ON public.bin_uop_exception USING btree (bin_id, occurred_at DESC);

CREATE INDEX idx_bin_uop_exception_occurred ON public.bin_uop_exception USING btree (occurred_at DESC);

CREATE INDEX idx_bin_uop_exception_open ON public.bin_uop_exception USING btree (kind, occurred_at DESC) WHERE (recovered_at IS NULL);

CREATE INDEX idx_bin_uop_ledger_bin_time ON public.bin_uop_ledger USING btree (bin_id, applied_at DESC);

CREATE INDEX idx_bin_uop_ledger_loader ON public.bin_uop_ledger USING btree (loader_id, applied_at) WHERE (loader_id IS NOT NULL);

CREATE INDEX idx_bin_uop_ledger_op ON public.bin_uop_ledger USING btree (op);

CREATE INDEX idx_bin_uop_ledger_op_time ON public.bin_uop_ledger USING btree (op, applied_at DESC);

CREATE UNIQUE INDEX idx_bins_label_unique ON public.bins USING btree (label) WHERE (label <> ''::text);

CREATE INDEX idx_bins_locked ON public.bins USING btree (locked) WHERE (locked = true);

CREATE INDEX idx_bins_node ON public.bins USING btree (node_id);

CREATE INDEX idx_bins_payload_code ON public.bins USING btree (payload_code);

CREATE INDEX idx_bins_status ON public.bins USING btree (status);

CREATE INDEX idx_bins_type ON public.bins USING btree (bin_type_id);

CREATE INDEX idx_bins_undeclared_carrier ON public.bins USING btree (id) WHERE (undeclared_carrier_at IS NOT NULL);

CREATE INDEX idx_cell_part_events_station_uid_time ON ONLY public.cell_part_events USING btree (station_uid, recorded_at);

CREATE INDEX idx_cleaning_missions_time ON public.cleaning_missions USING btree (dispatched_at);

CREATE INDEX idx_cleaning_missions_vehicle_time ON public.cleaning_missions USING btree (vehicle_id, dispatched_at DESC);

CREATE INDEX idx_cms_postings_inflight ON public.cms_postings USING btree (id) WHERE (status = 'inflight'::text);

CREATE INDEX idx_cms_postings_pending ON public.cms_postings USING btree (next_retry_at) WHERE (status = 'pending'::text);

CREATE INDEX idx_cms_txn_created ON public.cms_transactions USING btree (created_at);

CREATE INDEX idx_cms_txn_node ON public.cms_transactions USING btree (node_id);

CREATE UNIQUE INDEX idx_cms_txn_one_movement ON public.cms_transactions USING btree (order_id, bin_id, node_id, cat_id, delta) NULLS NOT DISTINCT WHERE (order_id IS NOT NULL);

CREATE INDEX idx_cms_txn_unposted ON public.cms_transactions USING btree (id) WHERE (posting_id IS NULL);

CREATE UNIQUE INDEX idx_demand_origins_open_key ON public.demand_origins USING btree (episode_key) WHERE (closed_at IS NULL);

CREATE INDEX idx_demand_origins_opened_at ON public.demand_origins USING btree (opened_at);

CREATE INDEX idx_demand_registry_payload ON public.demand_registry USING btree (payload_code);

CREATE INDEX idx_edge_cells_station_uid ON public.edge_cells USING btree (station_uid);

CREATE INDEX idx_inbox_processed_at ON public.inbox USING btree (processed_at);

CREATE INDEX idx_lineside_buckets_payload ON public.lineside_buckets USING btree (payload_code);

CREATE INDEX idx_lineside_drain_ledger_applied_at ON public.lineside_drain_ledger USING btree (applied_at);

CREATE INDEX idx_lineside_drain_ledger_node_payload_time ON public.lineside_drain_ledger USING btree (node_id, payload_code, applied_at DESC);

CREATE INDEX idx_mission_events_order ON public.mission_events USING btree (order_id);

CREATE INDEX idx_mission_telemetry_completed ON public.mission_telemetry USING btree (core_completed);

CREATE INDEX idx_mission_telemetry_robot ON public.mission_telemetry USING btree (robot_id);

CREATE INDEX idx_mission_telemetry_station_uid ON public.mission_telemetry USING btree (station_uid);

CREATE INDEX idx_node_properties_key ON public.node_properties USING btree (key);

CREATE INDEX idx_node_stations_station_uid ON public.node_stations USING btree (station_uid);

CREATE INDEX idx_nodes_claimed_by ON public.nodes USING btree (claimed_by) WHERE (claimed_by IS NOT NULL);

CREATE INDEX idx_nodes_parent ON public.nodes USING btree (parent_id);

CREATE INDEX idx_order_bins_order ON public.order_bins USING btree (order_id);

CREATE INDEX idx_order_history_code ON public.order_history USING btree (code, created_at) WHERE (code IS NOT NULL);

CREATE INDEX idx_order_history_order ON public.order_history USING btree (order_id);

CREATE INDEX idx_order_history_ref_payload ON public.order_history USING btree (((ref ->> 'payload'::text))) WHERE (ref IS NOT NULL);

CREATE INDEX idx_orders_delivery_node ON public.orders USING btree (delivery_node);

CREATE INDEX idx_orders_origin_id ON public.orders USING btree (origin_id) WHERE (origin_id IS NOT NULL);

COMMENT ON INDEX public.idx_orders_origin_id IS 'The child lookup. cost_ratio is COUNT(*) WHERE origin_id = X, the central query of the demand surface. Partial because most orders are structurally originless and indexing their NULLs buys nothing.';

CREATE INDEX idx_orders_recovers_order_id ON public.orders USING btree (recovers_order_id) WHERE (recovers_order_id IS NOT NULL);

CREATE INDEX idx_orders_status ON public.orders USING btree (status);

CREATE UNIQUE INDEX idx_orders_uuid ON public.orders USING btree (edge_uuid) WHERE (edge_uuid <> ''::text);

COMMENT ON INDEX public.idx_orders_uuid IS 'Unique, and partial. Two orders sharing an edge_uuid has no story: GetByUUID breaks the tie with ORDER BY id DESC, so a duplicate would silently redirect every lookup, including the ownership check behind cancel and release, onto the newer row. Blank is excluded because a blank means no Edge asked for this, and several rows can honestly be blank at once. The restore- exemption it once carried (for the retired put-back subsystem''s synthetic parents) went at v89: UUID means UUID for every order, and compound children mint a real one.';

CREATE INDEX idx_orders_vendor ON public.orders USING btree (vendor_order_id);

CREATE INDEX idx_outbox_pending ON public.outbox USING btree (sent_at) WHERE (sent_at IS NULL);

CREATE INDEX idx_parts_catid ON public.parts USING btree (catid) WHERE (catid <> ''::text);

CREATE INDEX idx_payload_manifest_part ON public.payload_manifest USING btree (part_id);

CREATE INDEX idx_payload_manifest_payload ON public.payload_manifest USING btree (payload_id);

CREATE UNIQUE INDEX idx_payload_manifest_payload_part ON public.payload_manifest USING btree (payload_id, part_number);

CREATE INDEX idx_process_positions_node ON public.process_positions USING btree (core_node_name);

CREATE INDEX idx_recovery_actions_created ON public.recovery_actions USING btree (created_at);

CREATE INDEX idx_reservations_bin ON public.reservations USING btree (bin_id);

CREATE INDEX idx_reservations_kind_node ON public.reservations USING btree (resource_kind, node_id);

CREATE INDEX idx_reservations_order ON public.reservations USING btree (order_id);

CREATE INDEX idx_robot_alarm_onsets_code_hour ON public.robot_alarm_onsets USING btree (code, hour);

CREATE INDEX idx_robot_confidence_low_time ON ONLY public.robot_confidence_low USING btree (sampled_at);

CREATE INDEX idx_robot_confidence_low_vehicle_time ON ONLY public.robot_confidence_low USING btree (vehicle_id, sampled_at);

CREATE INDEX idx_robot_confidence_samples_time ON ONLY public.robot_confidence_samples USING btree (sampled_at);

CREATE INDEX idx_robot_confidence_samples_vehicle_time ON ONLY public.robot_confidence_samples USING btree (vehicle_id, sampled_at);

CREATE INDEX idx_robot_deviceconfig_pulls_vehicle_time ON public.robot_deviceconfig_pulls USING btree (vehicle_id, pulled_at DESC);

CREATE INDEX idx_robot_lidar_hours_hour ON public.robot_lidar_hours USING btree (hour);

CREATE INDEX idx_scene_areas_current ON public.scene_areas USING btree (area_name, valid_from DESC);

CREATE UNIQUE INDEX idx_scene_areas_one_open ON public.scene_areas USING btree (area_name) WHERE (valid_to IS NULL);

CREATE INDEX idx_scene_diffs_observed ON public.scene_diffs USING btree (observed_at DESC);

CREATE INDEX idx_scene_edges_area ON public.scene_edges USING btree (area_name);

CREATE INDEX idx_scene_lane_versions_current ON public.scene_lane_versions USING btree (area_name, lane, valid_from DESC);

CREATE UNIQUE INDEX idx_scene_lane_versions_one_open ON public.scene_lane_versions USING btree (area_name, lane) WHERE (valid_to IS NULL);

CREATE INDEX idx_scene_points_area ON public.scene_points USING btree (area_name);

CREATE INDEX idx_scene_points_class ON public.scene_points USING btree (class_name);

CREATE INDEX idx_scene_reflectors_current ON public.scene_reflectors USING btree (shape_hash, valid_from DESC);

CREATE INDEX idx_sourceability_events_key_time ON public.sourceability_events USING btree (process_key, style_id, observed_at DESC);

CREATE INDEX idx_sourceability_events_payload_time ON public.sourceability_events USING btree (missing_payload, observed_at DESC) WHERE (missing_payload <> ''::text);

CREATE INDEX idx_style_claims_payload ON public.style_claims USING btree (payload_code);

CREATE INDEX idx_style_claims_process_style ON public.style_claims USING btree (process_id, style_id);

CREATE UNIQUE INDEX idx_supply_refusals_open ON public.supply_refusals USING btree (loader_node, payload_code) WHERE (closed_at IS NULL);

CREATE INDEX idx_supply_refusals_payload ON public.supply_refusals USING btree (payload_code, refused_at DESC);

CREATE INDEX idx_tte_samples_place_time ON public.tte_samples USING btree (core_node_name, payload_code, computed_at DESC);

CREATE INDEX idx_tte_samples_process_time ON public.tte_samples USING btree (process_id, payload_code, computed_at DESC);

CREATE INDEX ix_process_styles_active ON public.process_styles USING btree (process_id) WHERE is_active;

CREATE INDEX order_bins_by_bin ON public.order_bins USING btree (bin_id, order_id DESC);

CREATE UNIQUE INDEX order_bins_drop ON public.order_bins USING btree (order_id, drop_step);

CREATE UNIQUE INDEX order_bins_pick ON public.order_bins USING btree (order_id, pick_step);

CREATE UNIQUE INDEX uq_cell_part_events_tick ON ONLY public.cell_part_events USING btree (station_uid, edge_snapshot_id, recorded_at);

CREATE UNIQUE INDEX uq_reservations_bin_active ON public.reservations USING btree (bin_id) WHERE ((resource_kind = 'bin'::text) AND (state = ANY (ARRAY['pending'::text, 'confirmed'::text])));

CREATE UNIQUE INDEX uq_reservations_slot_active ON public.reservations USING btree (node_id) WHERE ((resource_kind = 'slot'::text) AND (state = ANY (ARRAY['pending'::text, 'confirmed'::text])));

CREATE TRIGGER bin_types_family_of_one BEFORE INSERT ON public.bin_types FOR EACH ROW EXECUTE FUNCTION public.bin_types_family_of_one();

ALTER TABLE ONLY public.bin_loader_home_bin_types
    ADD CONSTRAINT bin_loader_home_bin_types_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.bin_loader_home_bin_types
    ADD CONSTRAINT bin_loader_home_bin_types_position_node_id_fkey FOREIGN KEY (position_node_id) REFERENCES public.bin_loader_homes(position_node_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.bin_loader_homes
    ADD CONSTRAINT bin_loader_homes_loader_id_fkey FOREIGN KEY (loader_id) REFERENCES public.bin_loaders(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.bin_loader_homes
    ADD CONSTRAINT bin_loader_homes_position_node_id_fkey FOREIGN KEY (position_node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.bin_loader_payloads
    ADD CONSTRAINT bin_loader_payloads_loader_id_fkey FOREIGN KEY (loader_id) REFERENCES public.bin_loaders(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.bin_loader_quotas
    ADD CONSTRAINT bin_loader_quotas_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.bin_loader_quotas
    ADD CONSTRAINT bin_loader_quotas_loader_id_fkey FOREIGN KEY (loader_id) REFERENCES public.bin_loaders(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.bin_loaders
    ADD CONSTRAINT bin_loaders_second_stage_loader_id_fkey FOREIGN KEY (second_stage_loader_id) REFERENCES public.bin_loaders(id);

ALTER TABLE ONLY public.bin_types
    ADD CONSTRAINT bin_types_bare_of_fkey FOREIGN KEY (bare_of) REFERENCES public.bin_types(id);

ALTER TABLE ONLY public.bin_types
    ADD CONSTRAINT bin_types_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.carrier_families(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id);

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_lifted_from_node_id_fkey FOREIGN KEY (lifted_from_node_id) REFERENCES public.nodes(id) ON DELETE SET NULL;

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.kanban_families
    ADD CONSTRAINT kanban_families_family_id_fkey FOREIGN KEY (family_id) REFERENCES public.carrier_families(id) ON DELETE RESTRICT;

ALTER TABLE ONLY public.kanban_families
    ADD CONSTRAINT kanban_families_kanban_node_id_fkey FOREIGN KEY (kanban_node_id) REFERENCES public.kanbans(node_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_layer_levels
    ADD CONSTRAINT kanban_layer_levels_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_layer_levels
    ADD CONSTRAINT kanban_layer_levels_kanban_node_id_fkey FOREIGN KEY (kanban_node_id) REFERENCES public.kanbans(node_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_levels
    ADD CONSTRAINT kanban_levels_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_levels
    ADD CONSTRAINT kanban_levels_kanban_node_id_fkey FOREIGN KEY (kanban_node_id) REFERENCES public.kanbans(node_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_sources
    ADD CONSTRAINT kanban_sources_kanban_node_id_fkey FOREIGN KEY (kanban_node_id) REFERENCES public.kanbans(node_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_sources
    ADD CONSTRAINT kanban_sources_source_node_id_fkey FOREIGN KEY (source_node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_supports
    ADD CONSTRAINT kanban_supports_kanban_node_id_fkey FOREIGN KEY (kanban_node_id) REFERENCES public.kanbans(node_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanban_supports
    ADD CONSTRAINT kanban_supports_process_node_id_fkey FOREIGN KEY (process_node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.kanbans
    ADD CONSTRAINT kanbans_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.lane_confidence_daily
    ADD CONSTRAINT lane_confidence_daily_version_id_fkey FOREIGN KEY (version_id) REFERENCES public.scene_lane_versions(id);

ALTER TABLE ONLY public.lane_robot_confidence_daily
    ADD CONSTRAINT lane_robot_confidence_daily_version_id_fkey FOREIGN KEY (version_id) REFERENCES public.scene_lane_versions(id);

ALTER TABLE ONLY public.node_bin_types
    ADD CONSTRAINT node_bin_types_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_bin_types
    ADD CONSTRAINT node_bin_types_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_payloads
    ADD CONSTRAINT node_payloads_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_payloads
    ADD CONSTRAINT node_payloads_payload_id_fkey FOREIGN KEY (payload_id) REFERENCES public.payloads(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_properties
    ADD CONSTRAINT node_properties_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.node_stations
    ADD CONSTRAINT node_stations_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_claimed_by_fkey FOREIGN KEY (claimed_by) REFERENCES public.orders(id);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.order_bins
    ADD CONSTRAINT order_bins_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.order_bins
    ADD CONSTRAINT order_bins_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.order_history
    ADD CONSTRAINT order_history_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_parent_order_id_fkey FOREIGN KEY (parent_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_recovers_order_id_fkey FOREIGN KEY (recovers_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.payload_bin_types
    ADD CONSTRAINT payload_bin_types_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.payload_bin_types
    ADD CONSTRAINT payload_bin_types_payload_id_fkey FOREIGN KEY (payload_id) REFERENCES public.payloads(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.payload_manifest
    ADD CONSTRAINT payload_manifest_part_id_fkey FOREIGN KEY (part_id) REFERENCES public.parts(id);

ALTER TABLE ONLY public.payload_manifest
    ADD CONSTRAINT payload_manifest_payload_id_fkey FOREIGN KEY (payload_id) REFERENCES public.payloads(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.scene_areas
    ADD CONSTRAINT scene_areas_diff_id_fkey FOREIGN KEY (diff_id) REFERENCES public.scene_diffs(id);

ALTER TABLE ONLY public.scene_areas
    ADD CONSTRAINT scene_areas_map_version_id_fkey FOREIGN KEY (map_version_id) REFERENCES public.scene_map_versions(id);

ALTER TABLE ONLY public.scene_areas
    ADD CONSTRAINT scene_areas_supersedes_id_fkey FOREIGN KEY (supersedes_id) REFERENCES public.scene_areas(id);

ALTER TABLE ONLY public.scene_lane_versions
    ADD CONSTRAINT scene_lane_versions_diff_id_fkey FOREIGN KEY (diff_id) REFERENCES public.scene_diffs(id);

ALTER TABLE ONLY public.scene_lane_versions
    ADD CONSTRAINT scene_lane_versions_supersedes_id_fkey FOREIGN KEY (supersedes_id) REFERENCES public.scene_lane_versions(id);

ALTER TABLE ONLY public.scene_map_versions
    ADD CONSTRAINT scene_map_versions_diff_id_fkey FOREIGN KEY (diff_id) REFERENCES public.scene_diffs(id);

ALTER TABLE ONLY public.scene_reflectors
    ADD CONSTRAINT scene_reflectors_diff_id_fkey FOREIGN KEY (diff_id) REFERENCES public.scene_diffs(id);

ALTER TABLE ONLY public.scene_reflectors
    ADD CONSTRAINT scene_reflectors_map_version_id_fkey FOREIGN KEY (map_version_id) REFERENCES public.scene_map_versions(id);
