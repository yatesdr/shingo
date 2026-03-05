package store

const schemaPostgres = `
CREATE TABLE IF NOT EXISTS nodes (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    vendor_location TEXT NOT NULL DEFAULT '',
    node_type    TEXT NOT NULL DEFAULT 'storage',
    zone         TEXT NOT NULL DEFAULT '',
    capacity     INTEGER NOT NULL DEFAULT 0,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS orders (
    id              BIGSERIAL PRIMARY KEY,
    edge_uuid       TEXT NOT NULL,
    station_id      TEXT NOT NULL DEFAULT '',
    factory_id      TEXT NOT NULL DEFAULT '',
    order_type      TEXT NOT NULL DEFAULT 'retrieve',
    status          TEXT NOT NULL DEFAULT 'pending',
    quantity        DOUBLE PRECISION NOT NULL DEFAULT 1,
    source_node_id  BIGINT REFERENCES nodes(id),
    dest_node_id    BIGINT REFERENCES nodes(id),
    pickup_node     TEXT NOT NULL DEFAULT '',
    delivery_node   TEXT NOT NULL DEFAULT '',
    vendor_order_id TEXT NOT NULL DEFAULT '',
    vendor_state    TEXT NOT NULL DEFAULT '',
    robot_id        TEXT NOT NULL DEFAULT '',
    priority        INTEGER NOT NULL DEFAULT 0,
    payload_desc    TEXT NOT NULL DEFAULT '',
    error_detail    TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ,
    style_id        BIGINT REFERENCES payload_styles(id),
    instance_id     BIGINT REFERENCES payload_instances(id),
    parent_order_id BIGINT REFERENCES orders(id),
    sequence        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(edge_uuid);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_vendor ON orders(vendor_order_id);

CREATE TABLE IF NOT EXISTS order_history (
    id          BIGSERIAL PRIMARY KEY,
    order_id    BIGINT NOT NULL REFERENCES orders(id),
    status      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_order_history_order ON order_history(order_id);

CREATE TABLE IF NOT EXISTS outbox (
    id          BIGSERIAL PRIMARY KEY,
    topic       TEXT NOT NULL,
    payload     BYTEA NOT NULL,
    msg_type    TEXT NOT NULL DEFAULT '',
    station_id  TEXT NOT NULL DEFAULT '',
    retries     INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_outbox_pending ON outbox(sent_at) WHERE sent_at IS NULL;

CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGSERIAL PRIMARY KEY,
    entity_type TEXT NOT NULL,
    entity_id   BIGINT NOT NULL DEFAULT 0,
    action      TEXT NOT NULL,
    old_value   TEXT NOT NULL DEFAULT '',
    new_value   TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT 'system',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_log(entity_type, entity_id);

CREATE TABLE IF NOT EXISTS corrections (
    id               BIGSERIAL PRIMARY KEY,
    correction_type  TEXT NOT NULL,
    node_id          BIGINT NOT NULL REFERENCES nodes(id),
    instance_id      BIGINT REFERENCES payload_instances(id),
    manifest_item_id BIGINT REFERENCES manifest_items(id),
    cat_id           TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    quantity         DOUBLE PRECISION NOT NULL DEFAULT 0,
    reason           TEXT NOT NULL,
    actor            TEXT NOT NULL DEFAULT 'system',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS admin_users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS payload_styles (
    id                    BIGSERIAL PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,
    code                  TEXT NOT NULL DEFAULT '',
    description           TEXT NOT NULL DEFAULT '',
    form_factor           TEXT NOT NULL DEFAULT 'other',
    uop_capacity          INTEGER NOT NULL DEFAULT 0,
    width_mm              DOUBLE PRECISION NOT NULL DEFAULT 0,
    height_mm             DOUBLE PRECISION NOT NULL DEFAULT 0,
    depth_mm              DOUBLE PRECISION NOT NULL DEFAULT 0,
    weight_kg             DOUBLE PRECISION NOT NULL DEFAULT 0,
    default_manifest_json JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS payload_instances (
    id              BIGSERIAL PRIMARY KEY,
    style_id        BIGINT NOT NULL REFERENCES payload_styles(id),
    node_id         BIGINT REFERENCES nodes(id),
    tag_id          TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'empty',
    uop_remaining   INTEGER NOT NULL DEFAULT 0,
    claimed_by      BIGINT REFERENCES orders(id),
    loaded_at       TIMESTAMPTZ,
    delivered_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_instances_style ON payload_instances(style_id);
CREATE INDEX IF NOT EXISTS idx_instances_node ON payload_instances(node_id);
CREATE INDEX IF NOT EXISTS idx_instances_status ON payload_instances(status);
CREATE INDEX IF NOT EXISTS idx_instances_tag ON payload_instances(tag_id);

CREATE TABLE IF NOT EXISTS payload_style_manifest (
    id          BIGSERIAL PRIMARY KEY,
    style_id    BIGINT NOT NULL REFERENCES payload_styles(id) ON DELETE CASCADE,
    part_number TEXT NOT NULL DEFAULT '',
    quantity    DOUBLE PRECISION NOT NULL DEFAULT 0,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_style_manifest_style ON payload_style_manifest(style_id);

CREATE TABLE IF NOT EXISTS instance_events (
    id          BIGSERIAL PRIMARY KEY,
    instance_id BIGINT NOT NULL REFERENCES payload_instances(id) ON DELETE CASCADE,
    event_type  TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    actor       TEXT NOT NULL DEFAULT 'system',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_instance_events_instance ON instance_events(instance_id);

CREATE TABLE IF NOT EXISTS manifest_items (
    id              BIGSERIAL PRIMARY KEY,
    instance_id     BIGINT NOT NULL REFERENCES payload_instances(id) ON DELETE CASCADE,
    part_number     TEXT NOT NULL DEFAULT '',
    quantity        DOUBLE PRECISION NOT NULL DEFAULT 0,
    production_date TEXT,
    lot_code        TEXT,
    notes           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_manifest_instance ON manifest_items(instance_id);

CREATE TABLE IF NOT EXISTS scene_points (
    id              BIGSERIAL PRIMARY KEY,
    area_name       TEXT NOT NULL,
    instance_name   TEXT NOT NULL,
    class_name      TEXT NOT NULL,
    point_name      TEXT NOT NULL DEFAULT '',
    group_name      TEXT NOT NULL DEFAULT '',
    label           TEXT NOT NULL DEFAULT '',
    pos_x           DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_y           DOUBLE PRECISION NOT NULL DEFAULT 0,
    pos_z           DOUBLE PRECISION NOT NULL DEFAULT 0,
    dir             DOUBLE PRECISION NOT NULL DEFAULT 0,
    properties_json JSONB NOT NULL DEFAULT '{}',
    synced_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(area_name, instance_name)
);
CREATE INDEX IF NOT EXISTS idx_scene_points_class ON scene_points(class_name);
CREATE INDEX IF NOT EXISTS idx_scene_points_area ON scene_points(area_name);

CREATE TABLE IF NOT EXISTS edge_registry (
    id              BIGSERIAL PRIMARY KEY,
    station_id      TEXT NOT NULL UNIQUE,
    factory_id      TEXT NOT NULL DEFAULT '',
    hostname        TEXT NOT NULL DEFAULT '',
    version         TEXT NOT NULL DEFAULT '',
    line_ids        TEXT NOT NULL DEFAULT '[]',
    registered_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_heartbeat  TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS demands (
    id           BIGSERIAL PRIMARY KEY,
    cat_id       TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    demand_qty   DOUBLE PRECISION NOT NULL DEFAULT 0,
    produced_qty DOUBLE PRECISION NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS production_log (
    id          BIGSERIAL PRIMARY KEY,
    cat_id      TEXT NOT NULL,
    station_id  TEXT NOT NULL,
    quantity    DOUBLE PRECISION NOT NULL,
    reported_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_production_log_cat ON production_log(cat_id);

CREATE TABLE IF NOT EXISTS test_commands (
    id              BIGSERIAL PRIMARY KEY,
    command_type    TEXT NOT NULL,
    robot_id        TEXT NOT NULL,
    vendor_order_id TEXT NOT NULL DEFAULT '',
    vendor_state    TEXT NOT NULL DEFAULT '',
    location        TEXT NOT NULL DEFAULT '',
    config_id       TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS node_types (
    id           BIGSERIAL PRIMARY KEY,
    code         TEXT NOT NULL UNIQUE,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    is_synthetic BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS node_stations (
    node_id    BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    station_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (node_id, station_id)
);
CREATE INDEX IF NOT EXISTS idx_node_stations_station ON node_stations(station_id);

CREATE TABLE IF NOT EXISTS node_payload_styles (
    node_id  BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    style_id BIGINT NOT NULL REFERENCES payload_styles(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (node_id, style_id)
);

CREATE TABLE IF NOT EXISTS node_properties (
    id         BIGSERIAL PRIMARY KEY,
    node_id    BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (node_id, key)
);
`
