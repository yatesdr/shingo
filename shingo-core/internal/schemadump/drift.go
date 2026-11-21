package schemadump

// The plants' leftovers, as SQL a test applies to a database this build's
// migrations will meet (Vintage.Drift): each object the boot's drift report
// named at a plant on v0.18.0, rebuilt from the definition the plant's
// catalog showed (design record, outside the repo). Migration 190
// (store/migration_v190.go) removes each where it finds it.
//
// Where the record gives a table only by name (db_test, payload_instances)
// or by its columns' names (lane_access), the shape here is a stand-in: the
// migration reads whatever the catalog holds, so any shape proves it, and
// the drift report names an extra table by its name alone.

// DriftA is the first plant's: payloads rebuilt by an old copy-migration (the
// key, the unique code and the id sequence under a "1" name, plus two columns
// the build no longer has, one with a foreign key), nodes' two id columns
// still integer with a foreign key the build does not have, a missing column
// default, and two stray tables.
const DriftA = `
ALTER TABLE demand_registry ALTER COLUMN payload_code DROP DEFAULT;
ALTER TABLE payloads ADD COLUMN bin_id bigint;
ALTER TABLE payloads ADD CONSTRAINT payloads_bin_id_fkey1 FOREIGN KEY (bin_id) REFERENCES bins(id);
ALTER TABLE payloads ADD COLUMN manifest_confirmed boolean DEFAULT false NOT NULL;
ALTER TABLE payloads RENAME CONSTRAINT payloads_pkey TO payloads_pkey1;
ALTER TABLE payloads RENAME CONSTRAINT payloads_code_key TO payloads_code_key1;
ALTER SEQUENCE payloads_id_seq RENAME TO payloads_id_seq1;
ALTER TABLE nodes ALTER COLUMN node_type_id TYPE integer, ALTER COLUMN parent_id TYPE integer;
ALTER TABLE nodes ADD CONSTRAINT nodes_node_type_id_fkey FOREIGN KEY (node_type_id) REFERENCES node_types(id);
CREATE TABLE db_test (id integer, note text);
INSERT INTO db_test VALUES (1, 'x');
CREATE TABLE payload_instances (
	id bigserial PRIMARY KEY,
	style_id bigint NOT NULL,
	node_id bigint REFERENCES nodes(id),
	tag_id text NOT NULL DEFAULT '',
	status text NOT NULL DEFAULT 'empty',
	uop_remaining integer NOT NULL DEFAULT 0,
	claimed_by bigint REFERENCES orders(id),
	loaded_at timestamptz,
	delivered_at timestamptz NOT NULL DEFAULT now(),
	notes text NOT NULL DEFAULT '',
	created_at timestamptz NOT NULL DEFAULT now(),
	updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_payload_instances_node ON payload_instances (node_id);
`

// DriftB is the second plant's: the same missing column default, a column on
// orders and a table, each added by hand.
const DriftB = `
ALTER TABLE demand_registry ALTER COLUMN payload_code DROP DEFAULT;
ALTER TABLE orders ADD COLUMN deferred_steps_json text;
CREATE TABLE lane_access (
	id bigserial PRIMARY KEY,
	lane_id bigint NOT NULL,
	owner_order_id bigint REFERENCES orders(id),
	state text NOT NULL DEFAULT 'idle',
	op text NOT NULL DEFAULT '',
	target_slot_id bigint,
	target_bin_id bigint,
	expected_from text NOT NULL DEFAULT '',
	dispatched boolean NOT NULL DEFAULT false,
	enqueued_at timestamptz NOT NULL DEFAULT now(),
	acquired_at timestamptz
);
CREATE INDEX idx_lane_access_lane ON lane_access (lane_id);
`
