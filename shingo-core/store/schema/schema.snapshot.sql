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

CREATE TABLE public.bin_loader_homes (
    loader_id bigint NOT NULL,
    position_node_id bigint NOT NULL,
    payload_code text NOT NULL,
    min_stock integer DEFAULT 0 NOT NULL,
    uop_threshold integer DEFAULT 0 NOT NULL,
    sort_order integer DEFAULT 0 NOT NULL,
    home_kind text DEFAULT 'home'::text NOT NULL,
    CONSTRAINT bin_loader_homes_home_kind_check CHECK ((home_kind = ANY (ARRAY['home'::text, 'buffer'::text])))
);

CREATE TABLE public.bin_loader_payloads (
    loader_id bigint NOT NULL,
    payload_code text NOT NULL,
    min_stock integer DEFAULT 0 NOT NULL,
    uop_threshold integer DEFAULT 0 NOT NULL
);

CREATE TABLE public.bin_loader_quotas (
    loader_id bigint NOT NULL,
    bin_type_id bigint NOT NULL,
    want integer DEFAULT 0 NOT NULL,
    CONSTRAINT bin_loader_quotas_want_check CHECK ((want >= 0))
);

CREATE TABLE public.bin_loaders (
    id bigint NOT NULL,
    name text NOT NULL,
    role text NOT NULL,
    layout text NOT NULL,
    replenishment text NOT NULL,
    outbound_dest text DEFAULT ''::text NOT NULL,
    inbound_source text DEFAULT ''::text NOT NULL,
    buffer_dest text DEFAULT ''::text NOT NULL,
    config_gen bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    archived_at timestamp with time zone,
    funnel_windows boolean DEFAULT false NOT NULL,
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

CREATE TABLE public.bin_types (
    id bigint NOT NULL,
    code text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    width_in double precision DEFAULT 0 NOT NULL,
    height_in double precision DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.bin_types_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bin_types_id_seq OWNED BY public.bin_types.id;

CREATE TABLE public.bin_uop_audit (
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
    station text DEFAULT ''::text NOT NULL,
    detail jsonb,
    loader_id bigint
);

CREATE SEQUENCE public.bin_uop_audit_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bin_uop_audit_id_seq OWNED BY public.bin_uop_audit.id;

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
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.bins_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.bins_id_seq OWNED BY public.bins.id;

CREATE TABLE public.cell_config (
    cell_id text NOT NULL,
    station text NOT NULL,
    primary_process_id bigint NOT NULL,
    sub_process_ids jsonb DEFAULT '[]'::jsonb NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.cell_part_events (
    id bigint NOT NULL,
    cell_id text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    recorded_at timestamp with time zone NOT NULL,
    edge_snapshot_id bigint NOT NULL,
    count_value bigint DEFAULT 0 NOT NULL,
    delta bigint DEFAULT 0 NOT NULL,
    anomaly text DEFAULT ''::text NOT NULL,
    process_id bigint DEFAULT 0 NOT NULL,
    style_id bigint DEFAULT 0 NOT NULL
)
PARTITION BY RANGE (recorded_at);

CREATE SEQUENCE public.cell_part_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.cell_part_events_id_seq OWNED BY public.cell_part_events.id;

CREATE TABLE public.cell_targets (
    cell_id text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    target_cycle_ms bigint DEFAULT 0 NOT NULL,
    owner text DEFAULT ''::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.cms_transactions (
    id bigint NOT NULL,
    node_id bigint NOT NULL,
    node_name text DEFAULT ''::text NOT NULL,
    txn_type text DEFAULT ''::text NOT NULL,
    cat_id text NOT NULL,
    delta bigint DEFAULT 0 NOT NULL,
    qty_before bigint DEFAULT 0 NOT NULL,
    qty_after bigint DEFAULT 0 NOT NULL,
    bin_id bigint,
    bin_label text DEFAULT ''::text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    source_type text DEFAULT 'movement'::text NOT NULL,
    order_id bigint,
    notes text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.cms_transactions_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.cms_transactions_id_seq OWNED BY public.cms_transactions.id;

CREATE TABLE public.corrections (
    id bigint NOT NULL,
    correction_type text NOT NULL,
    node_id bigint NOT NULL,
    bin_id bigint,
    cat_id text DEFAULT ''::text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    quantity bigint DEFAULT 0 NOT NULL,
    reason text NOT NULL,
    actor text DEFAULT 'system'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.corrections_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.corrections_id_seq OWNED BY public.corrections.id;

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
    parent_origin_id uuid,
    station_id text DEFAULT ''::text NOT NULL,
    process_id text DEFAULT ''::text NOT NULL,
    core_node_name text DEFAULT ''::text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    opened_at timestamp with time zone NOT NULL,
    opened_total integer DEFAULT 0 NOT NULL,
    threshold integer DEFAULT 0 NOT NULL,
    used_edge_reports boolean DEFAULT false NOT NULL,
    revision bigint DEFAULT 1 NOT NULL,
    expected_orders integer,
    expected_reason text DEFAULT ''::text NOT NULL,
    uop_delivered integer DEFAULT 0 NOT NULL,
    rerequest_count integer DEFAULT 0 NOT NULL,
    signal_count integer DEFAULT 0 NOT NULL,
    discretionary boolean DEFAULT false NOT NULL,
    closed_at timestamp with time zone,
    close_reason text DEFAULT ''::text NOT NULL,
    closed_by text
);

CREATE TABLE public.demand_registry (
    id bigint NOT NULL,
    station_id text NOT NULL,
    core_node_name text NOT NULL,
    role text NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    outbound_dest text DEFAULT ''::text NOT NULL,
    replenish_uop_threshold integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    loader_id bigint
);

CREATE SEQUENCE public.demand_registry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.demand_registry_id_seq OWNED BY public.demand_registry.id;

CREATE TABLE public.demands (
    id bigint NOT NULL,
    cat_id text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    demand_qty bigint DEFAULT 0 NOT NULL,
    produced_qty bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.demands_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.demands_id_seq OWNED BY public.demands.id;

CREATE TABLE public.downtime_event_dedup (
    station text NOT NULL,
    edge_event_id bigint NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.downtime_events (
    id bigint NOT NULL,
    station text NOT NULL,
    plc_name text NOT NULL,
    reason text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    ended_at timestamp with time zone,
    duration_ms bigint DEFAULT 0 NOT NULL,
    edge_event_id bigint DEFAULT 0 NOT NULL
)
PARTITION BY RANGE (started_at);

CREATE SEQUENCE public.downtime_events_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.downtime_events_id_seq OWNED BY public.downtime_events.id;

CREATE TABLE public.edge_cells (
    station text NOT NULL,
    cell_label text NOT NULL,
    bindings jsonb DEFAULT '[]'::jsonb NOT NULL,
    first_seen timestamp with time zone DEFAULT now() NOT NULL,
    last_seen timestamp with time zone DEFAULT now() NOT NULL,
    stale boolean DEFAULT false NOT NULL
);

CREATE TABLE public.edge_lineside_reports (
    station text NOT NULL,
    core_node_name text NOT NULL,
    payload_code text NOT NULL,
    bin_count integer DEFAULT 0 NOT NULL,
    bin_uop integer DEFAULT 0 NOT NULL,
    bucket_qty integer DEFAULT 0 NOT NULL,
    reported_at timestamp with time zone NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.edge_registry (
    id bigint NOT NULL,
    station_uid text DEFAULT ''::text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    station_id text NOT NULL,
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
    conflict_at timestamp with time zone
);

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
    station_id text DEFAULT ''::text NOT NULL,
    processed_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.inventory_delta_dedup (
    station text NOT NULL,
    scope_kind text NOT NULL,
    scope_key text NOT NULL,
    epoch bigint DEFAULT 0 NOT NULL,
    last_seq bigint NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.lineside_buckets (
    id bigint NOT NULL,
    station text NOT NULL,
    core_node_name text NOT NULL,
    pair_key text NOT NULL,
    style_id bigint NOT NULL,
    part_number text NOT NULL,
    qty integer NOT NULL,
    payload_code text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT lineside_buckets_qty_check CHECK ((qty >= 0))
);

CREATE SEQUENCE public.lineside_buckets_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.lineside_buckets_id_seq OWNED BY public.lineside_buckets.id;

CREATE TABLE public.load_sequences (
    name text NOT NULL,
    task_names text DEFAULT '[]'::text NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

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
    robot_station text DEFAULT ''::text NOT NULL,
    blocks_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    errors_json jsonb DEFAULT '[]'::jsonb NOT NULL,
    detail text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

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
    station_id text DEFAULT ''::text NOT NULL,
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
    robot_alarms_json jsonb
);

CREATE SEQUENCE public.mission_telemetry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.mission_telemetry_id_seq OWNED BY public.mission_telemetry.id;

CREATE TABLE public.node_bin_types (
    node_id bigint NOT NULL,
    bin_type_id bigint NOT NULL
);

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
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.node_properties_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.node_properties_id_seq OWNED BY public.node_properties.id;

CREATE TABLE public.node_stations (
    node_id bigint NOT NULL,
    station_id text NOT NULL,
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
    step_index integer NOT NULL,
    action text NOT NULL,
    node_name text NOT NULL,
    dest_node text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
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

CREATE SEQUENCE public.order_history_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.order_history_id_seq OWNED BY public.order_history.id;

CREATE TABLE public.orders (
    id bigint NOT NULL,
    edge_uuid text NOT NULL,
    station_id text DEFAULT ''::text NOT NULL,
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
    bin_id bigint,
    payload_code text DEFAULT ''::text NOT NULL,
    wait_index integer DEFAULT 0 NOT NULL,
    queue_reason text DEFAULT ''::text NOT NULL,
    queue_code text,
    queue_cause text,
    skip_auto_confirm boolean DEFAULT false NOT NULL,
    sibling_order_uuid text DEFAULT ''::text NOT NULL,
    source_intent text DEFAULT ''::text NOT NULL,
    coordinated boolean DEFAULT false NOT NULL,
    remaining_uop integer,
    origin_id uuid,
    origin_class text DEFAULT ''::text NOT NULL,
    open_for_children boolean DEFAULT false NOT NULL,
    orphan_aged_at timestamp with time zone
);

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
    station_id text DEFAULT ''::text NOT NULL,
    retries integer DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    sent_at timestamp with time zone
);

CREATE SEQUENCE public.outbox_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.outbox_id_seq OWNED BY public.outbox.id;

CREATE TABLE public.payload_bin_types (
    payload_id bigint NOT NULL,
    bin_type_id bigint NOT NULL
);

CREATE TABLE public.payload_manifest (
    id bigint NOT NULL,
    payload_id bigint NOT NULL,
    part_number text DEFAULT ''::text NOT NULL,
    quantity bigint DEFAULT 0 NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

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
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.payloads_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.payloads_id_seq OWNED BY public.payloads.id;

CREATE TABLE public.process_styles (
    process_id text NOT NULL,
    style_id text NOT NULL,
    config_gen bigint DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    is_active boolean DEFAULT false NOT NULL
);

CREATE TABLE public.production_log (
    id bigint NOT NULL,
    cat_id text NOT NULL,
    station_id text NOT NULL,
    quantity bigint NOT NULL,
    reported_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE SEQUENCE public.production_log_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

ALTER SEQUENCE public.production_log_id_seq OWNED BY public.production_log.id;

CREATE TABLE public.production_tick_dedup (
    station text NOT NULL,
    edge_snapshot_id bigint NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
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
    expires_at timestamp with time zone,
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

CREATE TABLE public.schema_migrations (
    version integer NOT NULL,
    applied_at timestamp with time zone DEFAULT now() NOT NULL
);

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
    seq integer DEFAULT 0 NOT NULL
);

CREATE TABLE public.supply_refusals (
    id bigint NOT NULL,
    loader_node text NOT NULL,
    payload_code text NOT NULL,
    station_id text DEFAULT ''::text NOT NULL,
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

ALTER TABLE ONLY public.admin_users ALTER COLUMN id SET DEFAULT nextval('public.admin_users_id_seq'::regclass);

ALTER TABLE ONLY public.audit_log ALTER COLUMN id SET DEFAULT nextval('public.audit_log_id_seq'::regclass);

ALTER TABLE ONLY public.bin_loaders ALTER COLUMN id SET DEFAULT nextval('public.bin_loaders_id_seq'::regclass);

ALTER TABLE ONLY public.bin_types ALTER COLUMN id SET DEFAULT nextval('public.bin_types_id_seq'::regclass);

ALTER TABLE ONLY public.bin_uop_audit ALTER COLUMN id SET DEFAULT nextval('public.bin_uop_audit_id_seq'::regclass);

ALTER TABLE ONLY public.bins ALTER COLUMN id SET DEFAULT nextval('public.bins_id_seq'::regclass);

ALTER TABLE ONLY public.cell_part_events ALTER COLUMN id SET DEFAULT nextval('public.cell_part_events_id_seq'::regclass);

ALTER TABLE ONLY public.cms_transactions ALTER COLUMN id SET DEFAULT nextval('public.cms_transactions_id_seq'::regclass);

ALTER TABLE ONLY public.corrections ALTER COLUMN id SET DEFAULT nextval('public.corrections_id_seq'::regclass);

ALTER TABLE ONLY public.dashboards ALTER COLUMN id SET DEFAULT nextval('public.dashboards_id_seq'::regclass);

ALTER TABLE ONLY public.demand_registry ALTER COLUMN id SET DEFAULT nextval('public.demand_registry_id_seq'::regclass);

ALTER TABLE ONLY public.demands ALTER COLUMN id SET DEFAULT nextval('public.demands_id_seq'::regclass);

ALTER TABLE ONLY public.downtime_events ALTER COLUMN id SET DEFAULT nextval('public.downtime_events_id_seq'::regclass);

ALTER TABLE ONLY public.edge_registry ALTER COLUMN id SET DEFAULT nextval('public.edge_registry_id_seq'::regclass);

ALTER TABLE ONLY public.lineside_buckets ALTER COLUMN id SET DEFAULT nextval('public.lineside_buckets_id_seq'::regclass);

ALTER TABLE ONLY public.mission_events ALTER COLUMN id SET DEFAULT nextval('public.mission_events_id_seq'::regclass);

ALTER TABLE ONLY public.mission_telemetry ALTER COLUMN id SET DEFAULT nextval('public.mission_telemetry_id_seq'::regclass);

ALTER TABLE ONLY public.node_properties ALTER COLUMN id SET DEFAULT nextval('public.node_properties_id_seq'::regclass);

ALTER TABLE ONLY public.node_types ALTER COLUMN id SET DEFAULT nextval('public.node_types_id_seq'::regclass);

ALTER TABLE ONLY public.nodes ALTER COLUMN id SET DEFAULT nextval('public.nodes_id_seq'::regclass);

ALTER TABLE ONLY public.order_bins ALTER COLUMN id SET DEFAULT nextval('public.order_bins_id_seq'::regclass);

ALTER TABLE ONLY public.order_history ALTER COLUMN id SET DEFAULT nextval('public.order_history_id_seq'::regclass);

ALTER TABLE ONLY public.orders ALTER COLUMN id SET DEFAULT nextval('public.orders_id_seq'::regclass);

ALTER TABLE ONLY public.outbox ALTER COLUMN id SET DEFAULT nextval('public.outbox_id_seq'::regclass);

ALTER TABLE ONLY public.payload_manifest ALTER COLUMN id SET DEFAULT nextval('public.payload_manifest_id_seq'::regclass);

ALTER TABLE ONLY public.payloads ALTER COLUMN id SET DEFAULT nextval('public.payloads_id_seq'::regclass);

ALTER TABLE ONLY public.production_log ALTER COLUMN id SET DEFAULT nextval('public.production_log_id_seq'::regclass);

ALTER TABLE ONLY public.recovery_actions ALTER COLUMN id SET DEFAULT nextval('public.recovery_actions_id_seq'::regclass);

ALTER TABLE ONLY public.reservations ALTER COLUMN id SET DEFAULT nextval('public.reservations_id_seq'::regclass);

ALTER TABLE ONLY public.scene_edges ALTER COLUMN id SET DEFAULT nextval('public.scene_edges_id_seq'::regclass);

ALTER TABLE ONLY public.scene_points ALTER COLUMN id SET DEFAULT nextval('public.scene_points_id_seq'::regclass);

ALTER TABLE ONLY public.sourceability_events ALTER COLUMN id SET DEFAULT nextval('public.sourceability_events_id_seq'::regclass);

ALTER TABLE ONLY public.supply_refusals ALTER COLUMN id SET DEFAULT nextval('public.supply_refusals_id_seq'::regclass);

ALTER TABLE ONLY public.test_commands ALTER COLUMN id SET DEFAULT nextval('public.test_commands_id_seq'::regclass);

ALTER TABLE ONLY public.admin_users
    ADD CONSTRAINT admin_users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.admin_users
    ADD CONSTRAINT admin_users_username_key UNIQUE (username);

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
    ADD CONSTRAINT bin_types_code_key UNIQUE (code);

ALTER TABLE ONLY public.bin_types
    ADD CONSTRAINT bin_types_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bin_uop_audit
    ADD CONSTRAINT bin_uop_audit_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.cell_config
    ADD CONSTRAINT cell_config_pkey PRIMARY KEY (cell_id);

ALTER TABLE ONLY public.cell_targets
    ADD CONSTRAINT cell_targets_pkey PRIMARY KEY (cell_id, payload_code);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.corrections
    ADD CONSTRAINT corrections_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.dashboards
    ADD CONSTRAINT dashboards_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.demand_origins
    ADD CONSTRAINT demand_origins_pkey PRIMARY KEY (origin_id);

ALTER TABLE ONLY public.demand_registry
    ADD CONSTRAINT demand_registry_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.demand_registry
    ADD CONSTRAINT demand_registry_station_id_core_node_name_payload_code_key UNIQUE (station_id, core_node_name, payload_code);

ALTER TABLE ONLY public.demands
    ADD CONSTRAINT demands_cat_id_key UNIQUE (cat_id);

ALTER TABLE ONLY public.demands
    ADD CONSTRAINT demands_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.downtime_event_dedup
    ADD CONSTRAINT downtime_event_dedup_pkey PRIMARY KEY (station, edge_event_id);

ALTER TABLE ONLY public.edge_cells
    ADD CONSTRAINT edge_cells_pkey PRIMARY KEY (station, cell_label);

ALTER TABLE ONLY public.edge_lineside_reports
    ADD CONSTRAINT edge_lineside_reports_pkey PRIMARY KEY (station, core_node_name, payload_code);

ALTER TABLE ONLY public.edge_registry
    ADD CONSTRAINT edge_registry_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.edge_registry
    ADD CONSTRAINT edge_registry_station_id_key UNIQUE (station_id);

ALTER TABLE ONLY public.inbox
    ADD CONSTRAINT inbox_pkey PRIMARY KEY (msg_id);

ALTER TABLE ONLY public.inventory_delta_dedup
    ADD CONSTRAINT inventory_delta_dedup_pkey PRIMARY KEY (station, scope_kind, scope_key, epoch);

ALTER TABLE ONLY public.lineside_buckets
    ADD CONSTRAINT lineside_buckets_node_pair_style_part_key UNIQUE (core_node_name, pair_key, style_id, part_number);

ALTER TABLE ONLY public.lineside_buckets
    ADD CONSTRAINT lineside_buckets_pkey PRIMARY KEY (id);

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
    ADD CONSTRAINT node_stations_pkey PRIMARY KEY (node_id, station_id);

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

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.outbox
    ADD CONSTRAINT outbox_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.payload_bin_types
    ADD CONSTRAINT payload_bin_types_pkey PRIMARY KEY (payload_id, bin_type_id);

ALTER TABLE ONLY public.payload_manifest
    ADD CONSTRAINT payload_manifest_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.payloads
    ADD CONSTRAINT payloads_code_key UNIQUE (code);

ALTER TABLE ONLY public.payloads
    ADD CONSTRAINT payloads_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.process_styles
    ADD CONSTRAINT process_styles_pkey PRIMARY KEY (process_id, style_id);

ALTER TABLE ONLY public.production_log
    ADD CONSTRAINT production_log_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.production_tick_dedup
    ADD CONSTRAINT production_tick_dedup_pkey PRIMARY KEY (station, edge_snapshot_id);

ALTER TABLE ONLY public.recovery_actions
    ADD CONSTRAINT recovery_actions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_edges
    ADD CONSTRAINT scene_edges_area_name_instance_name_key UNIQUE (area_name, instance_name);

ALTER TABLE ONLY public.scene_edges
    ADD CONSTRAINT scene_edges_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.scene_points
    ADD CONSTRAINT scene_points_area_name_instance_name_key UNIQUE (area_name, instance_name);

ALTER TABLE ONLY public.scene_points
    ADD CONSTRAINT scene_points_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);

ALTER TABLE ONLY public.sourceability_events
    ADD CONSTRAINT sourceability_events_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.supply_refusals
    ADD CONSTRAINT supply_refusals_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.test_commands
    ADD CONSTRAINT test_commands_pkey PRIMARY KEY (id);

CREATE INDEX cell_config_station_idx ON public.cell_config USING btree (station);

CREATE UNIQUE INDEX edge_registry_station_uid_key ON public.edge_registry USING btree (station_uid) WHERE (station_uid <> ''::text);

CREATE INDEX idx_audit_entity ON public.audit_log USING btree (entity_type, entity_id);

CREATE INDEX idx_bin_loader_homes_loader ON public.bin_loader_homes USING btree (loader_id);

CREATE INDEX idx_bin_uop_audit_bin_time ON public.bin_uop_audit USING btree (bin_id, applied_at DESC);

CREATE INDEX idx_bin_uop_audit_loader ON public.bin_uop_audit USING btree (loader_id, applied_at) WHERE (loader_id IS NOT NULL);

CREATE INDEX idx_bin_uop_audit_op ON public.bin_uop_audit USING btree (op);

CREATE INDEX idx_bin_uop_audit_op_time ON public.bin_uop_audit USING btree (op, applied_at DESC);

CREATE UNIQUE INDEX idx_bins_label_unique ON public.bins USING btree (label) WHERE (label <> ''::text);

CREATE INDEX idx_bins_locked ON public.bins USING btree (locked) WHERE (locked = true);

CREATE INDEX idx_bins_node ON public.bins USING btree (node_id);

CREATE INDEX idx_bins_payload_code ON public.bins USING btree (payload_code);

CREATE INDEX idx_bins_status ON public.bins USING btree (status);

CREATE INDEX idx_bins_type ON public.bins USING btree (bin_type_id);

CREATE INDEX idx_cell_part_events_cell_time ON ONLY public.cell_part_events USING btree (cell_id, recorded_at);

CREATE INDEX idx_cms_txn_created ON public.cms_transactions USING btree (created_at);

CREATE INDEX idx_cms_txn_node ON public.cms_transactions USING btree (node_id);

CREATE UNIQUE INDEX idx_demand_origins_open_key ON public.demand_origins USING btree (episode_key) WHERE (closed_at IS NULL);

CREATE INDEX idx_demand_origins_opened_at ON public.demand_origins USING btree (opened_at);

CREATE INDEX idx_demand_registry_payload ON public.demand_registry USING btree (payload_code);

CREATE INDEX idx_downtime_events_station_time ON ONLY public.downtime_events USING btree (station, started_at);

CREATE INDEX idx_edge_cells_station ON public.edge_cells USING btree (station);

CREATE INDEX idx_inbox_processed_at ON public.inbox USING btree (processed_at);

CREATE INDEX idx_lineside_buckets_node_style ON public.lineside_buckets USING btree (core_node_name, style_id);

CREATE INDEX idx_lineside_buckets_payload ON public.lineside_buckets USING btree (payload_code);

CREATE INDEX idx_mission_events_order ON public.mission_events USING btree (order_id);

CREATE INDEX idx_mission_telemetry_completed ON public.mission_telemetry USING btree (core_completed);

CREATE INDEX idx_mission_telemetry_robot ON public.mission_telemetry USING btree (robot_id);

CREATE INDEX idx_mission_telemetry_station ON public.mission_telemetry USING btree (station_id);

CREATE INDEX idx_node_stations_station ON public.node_stations USING btree (station_id);

CREATE INDEX idx_nodes_claimed_by ON public.nodes USING btree (claimed_by) WHERE (claimed_by IS NOT NULL);

CREATE INDEX idx_order_bins_bin ON public.order_bins USING btree (bin_id);

CREATE INDEX idx_order_bins_order ON public.order_bins USING btree (order_id);

CREATE INDEX idx_order_history_code ON public.order_history USING btree (code, created_at) WHERE (code IS NOT NULL);

CREATE INDEX idx_order_history_order ON public.order_history USING btree (order_id);

CREATE INDEX idx_order_history_ref_payload ON public.order_history USING btree (((ref ->> 'payload'::text))) WHERE (ref IS NOT NULL);

CREATE INDEX idx_orders_delivery_node ON public.orders USING btree (delivery_node);

CREATE INDEX idx_orders_origin_id ON public.orders USING btree (origin_id) WHERE (origin_id IS NOT NULL);

CREATE INDEX idx_orders_status ON public.orders USING btree (status);

CREATE UNIQUE INDEX idx_orders_uuid ON public.orders USING btree (edge_uuid) WHERE (edge_uuid <> ''::text);

CREATE INDEX idx_orders_vendor ON public.orders USING btree (vendor_order_id);

CREATE INDEX idx_outbox_pending ON public.outbox USING btree (sent_at) WHERE (sent_at IS NULL);

CREATE INDEX idx_payload_manifest_payload ON public.payload_manifest USING btree (payload_id);

CREATE INDEX idx_production_log_cat ON public.production_log USING btree (cat_id);

CREATE INDEX idx_recovery_actions_created ON public.recovery_actions USING btree (created_at);

CREATE INDEX idx_reservations_bin ON public.reservations USING btree (bin_id);

CREATE INDEX idx_reservations_kind_node ON public.reservations USING btree (resource_kind, node_id);

CREATE INDEX idx_reservations_order ON public.reservations USING btree (order_id);

CREATE INDEX idx_scene_edges_area ON public.scene_edges USING btree (area_name);

CREATE INDEX idx_scene_points_area ON public.scene_points USING btree (area_name);

CREATE INDEX idx_scene_points_class ON public.scene_points USING btree (class_name);

CREATE INDEX idx_sourceability_events_key_time ON public.sourceability_events USING btree (process_key, style_id, observed_at DESC);

CREATE INDEX idx_sourceability_events_payload_time ON public.sourceability_events USING btree (missing_payload, observed_at DESC) WHERE (missing_payload <> ''::text);

CREATE INDEX idx_style_claims_payload ON public.style_claims USING btree (payload_code);

CREATE INDEX idx_style_claims_process_style ON public.style_claims USING btree (process_id, style_id);

CREATE UNIQUE INDEX idx_supply_refusals_open ON public.supply_refusals USING btree (loader_node, payload_code) WHERE (closed_at IS NULL);

CREATE INDEX idx_supply_refusals_payload ON public.supply_refusals USING btree (payload_code, refused_at DESC);

CREATE INDEX ix_process_styles_active ON public.process_styles USING btree (process_id) WHERE is_active;

CREATE UNIQUE INDEX uq_reservations_bin_active ON public.reservations USING btree (bin_id) WHERE ((resource_kind = 'bin'::text) AND (state = ANY (ARRAY['pending'::text, 'confirmed'::text])));

CREATE UNIQUE INDEX uq_reservations_slot_active ON public.reservations USING btree (node_id) WHERE ((resource_kind = 'slot'::text) AND (state = ANY (ARRAY['pending'::text, 'confirmed'::text])));

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

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id);

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.cms_transactions
    ADD CONSTRAINT cms_transactions_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.corrections
    ADD CONSTRAINT corrections_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.corrections
    ADD CONSTRAINT corrections_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

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
    ADD CONSTRAINT orders_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.orders
    ADD CONSTRAINT orders_parent_order_id_fkey FOREIGN KEY (parent_order_id) REFERENCES public.orders(id);

ALTER TABLE ONLY public.payload_bin_types
    ADD CONSTRAINT payload_bin_types_bin_type_id_fkey FOREIGN KEY (bin_type_id) REFERENCES public.bin_types(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.payload_bin_types
    ADD CONSTRAINT payload_bin_types_payload_id_fkey FOREIGN KEY (payload_id) REFERENCES public.payloads(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.payload_manifest
    ADD CONSTRAINT payload_manifest_payload_id_fkey FOREIGN KEY (payload_id) REFERENCES public.payloads(id) ON DELETE CASCADE;

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES public.bins(id);

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id);

ALTER TABLE ONLY public.reservations
    ADD CONSTRAINT reservations_order_id_fkey FOREIGN KEY (order_id) REFERENCES public.orders(id);
