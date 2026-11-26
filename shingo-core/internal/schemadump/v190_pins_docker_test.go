//go:build docker

package schemadump_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"shingocore/internal/schemadump"
	"shingocore/store"
)

// v190Rig is one database at v189, born on the baseline and stepped down, with
// setup applied: what migration 190 meets.
type v190Rig struct {
	t    *testing.T
	ctx  context.Context
	inst *schemadump.Instance
	name string
}

func newV190Rig(t *testing.T, setup string) *v190Rig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	r := &v190Rig{t: t, ctx: ctx, inst: start(ctx, t)}
	name, err := r.inst.BuildFresh(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.name = name
	if err := store.MigrateDown(r.inst.DBConfigForTest(name), 190); err != nil {
		t.Fatalf("down 190: %v", err)
	}
	if setup != "" {
		if err := r.inst.ApplyRaw(ctx, name, setup); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	return r
}

func (r *v190Rig) shape(when string) string {
	r.t.Helper()
	d, err := r.inst.Dump(r.ctx, r.name)
	if err != nil {
		r.t.Fatalf("%s: dump: %v", when, err)
	}
	return schemadump.Canonical(schemadump.ExcludePartitions(d))
}

// refusedNaming boots this build and wants migration 190 refused with an error
// that contains each of want, having changed nothing: the shape, no audit row,
// the version not recorded.
func (r *v190Rig) refusedNaming(want ...string) {
	r.t.Helper()
	was := r.shape("before the boot")
	err := r.inst.Reboot(r.ctx, r.name)
	if err == nil {
		r.t.Errorf("the boot succeeded, want migration 190 refused naming %q", want)
	} else {
		for _, w := range want {
			if !strings.Contains(err.Error(), w) {
				r.t.Errorf("the refusal does not name %q:\n  %v", w, err)
			}
		}
	}
	if now := r.shape("after the boot"); now != was {
		r.t.Errorf("the boot changed the shape:\n%s", shapeDiff(was, now))
	}
	db := openTest(r.t, r.inst, r.name)
	var recorded bool
	var audited int
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 190),
		(SELECT count(*) FROM audit_log WHERE entity_type = 'schema' AND entity_id = 190)`).Scan(&recorded, &audited); err != nil {
		r.t.Fatal(err)
	}
	if recorded || audited != 0 {
		r.t.Errorf("v190 recorded %v with %d audit rows, want neither", recorded, audited)
	}
}

// TestV190_AHeldRenameTargetNamesItsHolder: a rename whose target name another
// object holds fails, saying who holds it. A sequence and an index share their
// names with every relation of schema public, so the holder may be on any
// table; a constraint with no index shares its table's alone. None is a step
// that is skipped: "gone" for a rename means the from name is absent.
func TestV190_AHeldRenameTargetNamesItsHolder(t *testing.T) {
	t.Parallel()
	const seq1 = `ALTER SEQUENCE payloads_id_seq RENAME TO payloads_id_seq1;`
	for _, c := range []struct {
		name, setup string
		want        []string
	}{
		{"a sequence another table owns", seq1 + `
			CREATE TABLE zz_holder (id bigint);
			CREATE SEQUENCE payloads_id_seq OWNED BY zz_holder.id;`,
			[]string{"payloads_id_seq1", "sequence payloads_id_seq", "zz_holder"}},
		{"a view", seq1 + `
			CREATE VIEW payloads_id_seq AS SELECT 1 AS one;`,
			[]string{"payloads_id_seq1", "view payloads_id_seq"}},
		{"another table's key", `
			ALTER TABLE payloads RENAME CONSTRAINT payloads_pkey TO payloads_pkey1;
			CREATE TABLE zz_holder (id bigint CONSTRAINT payloads_pkey PRIMARY KEY);`,
			[]string{"payloads_pkey1", "index payloads_pkey", "zz_holder"}},
		{"the table's own check constraint", `
			ALTER TABLE payloads RENAME CONSTRAINT payloads_code_key TO payloads_code_key1;
			ALTER TABLE payloads ADD CONSTRAINT payloads_code_key CHECK (code <> '!');`,
			[]string{"payloads_code_key1", "constraint payloads_code_key on table payloads"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			newV190Rig(t, c.setup).refusedNaming(c.want...)
		})
	}
}

// TestV190_ADropNamesWhatDependsOnIt: a drop that would fail on a dependent,
// or take one with it unrecorded (so down could not put it back), fails naming
// the dependent.
func TestV190_ADropNamesWhatDependsOnIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, setup string
		want        []string
	}{
		{"a view on a table", `
			CREATE TABLE payload_instances (id bigint PRIMARY KEY);
			CREATE VIEW zz_view AS SELECT id FROM payload_instances;`,
			[]string{"payload_instances", "zz_view"}},
		{"another table's foreign key", `
			CREATE TABLE payload_instances (id bigint PRIMARY KEY);
			CREATE TABLE zz_child (parent bigint REFERENCES payload_instances(id));`,
			[]string{"payload_instances", "zz_child"}},
		{"a default on the sequence a table owns", `
			CREATE TABLE db_test (id serial);
			CREATE TABLE zz_borrower (id integer DEFAULT nextval('db_test_id_seq'));`,
			[]string{"db_test", "zz_borrower"}},
		{"a trigger, which goes with its table", `
			CREATE TABLE db_test (id integer);
			CREATE FUNCTION zz_fn() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END';
			CREATE TRIGGER zz_trigger BEFORE INSERT ON db_test FOR EACH ROW EXECUTE FUNCTION zz_fn();`,
			[]string{"db_test", "zz_trigger"}},
		{"a view on a column", `
			ALTER TABLE payloads ADD COLUMN bin_id bigint;
			CREATE VIEW zz_bin_view AS SELECT bin_id FROM payloads;`,
			[]string{"bin_id", "zz_bin_view"}},
		{"an index whose predicate reads a column, which goes with it", `
			ALTER TABLE payloads ADD COLUMN manifest_confirmed boolean;
			CREATE INDEX zz_partial ON payloads (id) WHERE manifest_confirmed;`,
			[]string{"manifest_confirmed", "zz_partial"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			newV190Rig(t, c.setup).refusedNaming(c.want...)
		})
	}
}

// TestV190_DownPutsAnOwnedSequenceBackAsItWas: a dropped table's sequence
// comes back of the type it had.
func TestV190_DownPutsAnOwnedSequenceBackAsItWas(t *testing.T) {
	t.Parallel()
	r := newV190Rig(t, `CREATE TABLE db_test (id serial PRIMARY KEY, note text);`)
	was := r.shape("with the table")
	if err := r.inst.Reboot(r.ctx, r.name); err != nil {
		t.Fatalf("up 190: %v", err)
	}
	if err := store.MigrateDown(r.inst.DBConfigForTest(r.name), 190); err != nil {
		t.Fatalf("down 190: %v", err)
	}
	if now := r.shape("after down"); now != was {
		t.Errorf("down did not restore the table's shape:\n%s", shapeDiff(was, now))
	}
}

// TestV190_ADatabaseAtV190IsUntouched: v190 is edited in place, so a database
// already at it must satisfy its Verify: the next boot re-runs nothing and
// changes nothing, schema and rows (a re-run re-records schema_migrations).
// Each plant as it stands (the first had payload_instances dropped by hand
// before 190 ran), and a database born on the baseline.
func TestV190_ADatabaseAtV190IsUntouched(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, setup string }{
		{"born on the baseline", ""},
		{"drift-a, payload_instances dropped by hand first", schemadump.DriftA + "DROP TABLE payload_instances;"},
		{"drift-b", schemadump.DriftB},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newV190Rig(t, c.setup)
			if err := r.inst.Reboot(r.ctx, r.name); err != nil {
				t.Fatalf("up 190: %v", err)
			}
			was, err := r.inst.DumpWithRowsForTest(r.ctx, r.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.CheckDatabase(r.inst.DBConfigForTest(r.name)); err != nil {
				t.Errorf("migrate check refuses a database at v190: %v", err)
			}
			if err := r.inst.Reboot(r.ctx, r.name); err != nil {
				t.Fatalf("the next boot: %v", err)
			}
			now, err := r.inst.DumpWithRowsForTest(r.ctx, r.name)
			if err != nil {
				t.Fatal(err)
			}
			if now != was {
				t.Errorf("the next boot changed the database:\n%s", shapeDiff(was, now))
			}
		})
	}
}
