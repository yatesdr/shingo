package store

const schemaPostgres = `
CREATE TABLE IF NOT EXISTS nodes (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    rds_location TEXT NOT NULL DEFAULT '',
    node_type    TEXT NOT NULL DEFAULT 'storage',
    zone         TEXT NOT NULL DEFAULT '',
    capacity     INTEGER NOT NULL DEFAULT 0,
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS materials (
    id          BIGSERIAL PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    unit        TEXT NOT NULL DEFAULT 'ea',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS orders (
    id              BIGSERIAL PRIMARY KEY,
    edge_uuid    TEXT NOT NULL,
    client_id       TEXT NOT NULL DEFAULT '',
    factory_id      TEXT NOT NULL DEFAULT '',
    order_type      TEXT NOT NULL DEFAULT 'retrieve',
    status          TEXT NOT NULL DEFAULT 'pending',
    material_id     BIGINT REFERENCES materials(id),
    material_code   TEXT NOT NULL DEFAULT '',
    quantity        DOUBLE PRECISION NOT NULL DEFAULT 1,
    source_node_id  BIGINT REFERENCES nodes(id),
    dest_node_id    BIGINT REFERENCES nodes(id),
    pickup_node     TEXT NOT NULL DEFAULT '',
    delivery_node   TEXT NOT NULL DEFAULT '',
    rds_order_id    TEXT NOT NULL DEFAULT '',
    rds_state       TEXT NOT NULL DEFAULT '',
    robot_id        TEXT NOT NULL DEFAULT '',
    priority        INTEGER NOT NULL DEFAULT 0,
    payload_desc    TEXT NOT NULL DEFAULT '',
    error_detail    TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ,
    payload_type_id BIGINT REFERENCES payload_types(id),
    payload_id      BIGINT REFERENCES payloads(id)
);
CREATE INDEX IF NOT EXISTS idx_orders_uuid ON orders(edge_uuid);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_rds ON orders(rds_order_id);

CREATE TABLE IF NOT EXISTS node_inventory (
    id              BIGSERIAL PRIMARY KEY,
    node_id         BIGINT NOT NULL REFERENCES nodes(id),
    material_id     BIGINT NOT NULL REFERENCES materials(id),
    quantity        DOUBLE PRECISION NOT NULL DEFAULT 0,
    is_partial      BOOLEAN NOT NULL DEFAULT FALSE,
    delivered_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    source_order_id BIGINT REFERENCES orders(id),
    metadata        JSONB NOT NULL DEFAULT '{}',
    notes           TEXT NOT NULL DEFAULT '',
    claimed_by      BIGINT REFERENCES orders(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_inventory_node ON node_inventory(node_id);
CREATE INDEX IF NOT EXISTS idx_inventory_material ON node_inventory(material_id);
CREATE INDEX IF NOT EXISTS idx_inventory_fifo ON node_inventory(material_id, is_partial DESC, delivered_at ASC);

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
    client_id   TEXT NOT NULL DEFAULT '',
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
    material_id      BIGINT REFERENCES materials(id),
    inventory_id     BIGINT REFERENCES node_inventory(id),
    payload_id       BIGINT REFERENCES payloads(id),
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

CREATE TABLE IF NOT EXISTS payload_types (
    id                    BIGSERIAL PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,
    description           TEXT NOT NULL DEFAULT '',
    form_factor           TEXT NOT NULL DEFAULT 'other',
    default_manifest_json JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS payloads (
    id              BIGSERIAL PRIMARY KEY,
    payload_type_id BIGINT NOT NULL REFERENCES payload_types(id),
    node_id         BIGINT REFERENCES nodes(id),
    status          TEXT NOT NULL DEFAULT 'empty',
    claimed_by      BIGINT REFERENCES orders(id),
    delivered_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    notes           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_payloads_type ON payloads(payload_type_id);
CREATE INDEX IF NOT EXISTS idx_payloads_node ON payloads(node_id);
CREATE INDEX IF NOT EXISTS idx_payloads_status ON payloads(status);

CREATE TABLE IF NOT EXISTS manifest_items (
    id              BIGSERIAL PRIMARY KEY,
    payload_id      BIGINT NOT NULL REFERENCES payloads(id) ON DELETE CASCADE,
    part_number     TEXT NOT NULL DEFAULT '',
    quantity        DOUBLE PRECISION NOT NULL DEFAULT 0,
    production_date TEXT,
    lot_code        TEXT,
    notes           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_manifest_payload ON manifest_items(payload_id);
`
