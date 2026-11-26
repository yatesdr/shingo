package schemadump

// The plants' leftovers, as SQL a test applies to a database this build's
// migrations will meet (Vintage.Drift): each object the boot's drift report
// named at a plant on v0.18.0, rebuilt from the definition the plant's
// catalog showed (design record, outside the repo). Migration 190
// (store/migration_v190.go) removes each where it finds it.
//
// payload_instances is the plant's own definition, as its catalog holds it
// (schema only): shingo's first payloads table under a later name, which kept
// that table's key, its unique code, its foreign key and its id sequence by
// their payloads_* names. Those names are schema-wide and the rebuilt payloads
// table wants them back, so the shape is what decides whether migration 190
// can run: a stand-in proved it on a database that could not fail.
//
// db_test and lane_access were made by hand at the plants (no release created
// either), so each is a stand-in with the kinds of object the plant's holds:
// db_test an integer id with the sequence it owns, lane_access a key and
// indexes.

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
CREATE TABLE db_test (id serial PRIMARY KEY, note text);
INSERT INTO db_test VALUES (1, 'x');
CREATE SEQUENCE payloads_id_seq;
CREATE TABLE payload_instances (
	id bigint DEFAULT nextval('payloads_id_seq'::regclass) NOT NULL,
	code text NOT NULL,
	description text DEFAULT ''::text NOT NULL,
	uop_capacity integer DEFAULT 0 NOT NULL,
	default_manifest_json text DEFAULT '{}'::text NOT NULL,
	created_at timestamp with time zone DEFAULT now() NOT NULL,
	updated_at timestamp with time zone DEFAULT now() NOT NULL,
	bin_id bigint,
	manifest_confirmed boolean DEFAULT false NOT NULL,
	tag_id text DEFAULT ''::text NOT NULL,
	uop_remaining integer DEFAULT 0 NOT NULL,
	loaded_at text
);
ALTER SEQUENCE payloads_id_seq OWNED BY payload_instances.id;
ALTER TABLE payload_instances ADD CONSTRAINT payloads_pkey PRIMARY KEY (id);
ALTER TABLE payload_instances ADD CONSTRAINT payloads_code_key UNIQUE (code);
ALTER TABLE payload_instances ADD CONSTRAINT payloads_bin_id_fkey FOREIGN KEY (bin_id) REFERENCES bins(id);
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
