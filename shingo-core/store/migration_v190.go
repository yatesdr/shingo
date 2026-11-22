package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"shingocore/store/schema"
)

// v190 — the plants' schema leftovers: objects the boot's drift report named
// at the plants on v0.18.0, none of which this build (or v0.18.0) names in
// code. A database born on the baseline has none of them and two plants carry
// different ones, so every step reads the catalog first and changes only what
// it finds (internal/schemadump TestV190_TheLeftoversGoAndDownPutsThemBack).
//
//   - demand_registry.payload_code gets the build's DEFAULT ''.
//   - payloads loses bin_id (with its foreign key payloads_bin_id_fkey1) and
//     manifest_confirmed; payloads_pkey1, payloads_code_key1 and
//     payloads_id_seq1, left by an old rebuild of the table, take the build's
//     names.
//   - nodes.node_type_id and nodes.parent_id become bigint, and
//     nodes_node_type_id_fkey goes: nothing in the build relies on it to
//     refuse a write (nodes.DeleteType has no production caller, the node form
//     posts only an existing type, every reader LEFT JOINs node_types), and a
//     database born on the baseline runs without it.
//   - orders.deferred_steps_json goes (owner, 2026-11-21: deleted, no dump).
//   - the tables db_test, payload_instances and lane_access go.
//
// DOWN RESTORES SHAPE, NOT ROWS, AND ONLY THIS DATABASE'S. Up writes one
// audit_log row per object it changes (entity_type 'schema', entity_id 190,
// actor 'migration v190'), holding the object's definition as the catalog read
// it before the change: a column's type, default and nullability (and the
// constraints and indexes on it), a constraint's definition, a name, a dropped
// table's columns, constraints, indexes and owned sequences. Down reads those
// rows newest first, recreates exactly that, empty (foreign keys last, once
// everything they may point at is back), and deletes the rows. A database up
// changed nothing on gets nothing back.
//
// One transaction (the runner's): the lock on every table it touches that is
// there, then the steps in order, the tables dropped first. Idempotent: a step
// whose leftover is gone does nothing and records nothing. Nothing CASCADEs: a
// drop first reads what depends on it and fails naming what it would not
// record (v190NoDependents), and a rename whose target is held fails naming
// the holder (v190NameFree).

const (
	v190Entity = "schema"
	v190Actor  = "migration v190"
)

// v190Column is one column as the catalog defines it.
type v190Column struct {
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	Default *string `json:"default,omitempty"`
	NotNull bool    `json:"not_null,omitempty"`
}

// v190Named is a named constraint or index with its definition.
type v190Named struct {
	Name string `json:"name"`
	Def  string `json:"def"`
}

// v190Sequence is a sequence a dropped table owned, its type and the owning
// column. A record written before the type was kept has none: down then makes
// Postgres's default, a bigint sequence.
type v190Sequence struct {
	Name   string `json:"name"`
	Column string `json:"column"`
	Type   string `json:"type,omitempty"`
}

// v190Record is what up read before one change: the audit row's old_value.
type v190Record struct {
	Table       string         `json:"table,omitempty"`
	Column      *v190Column    `json:"column,omitempty"`
	Name        string         `json:"name,omitempty"`
	To          string         `json:"to,omitempty"`
	Def         string         `json:"def,omitempty"`
	Columns     []v190Column   `json:"columns,omitempty"`
	Constraints []v190Named    `json:"constraints,omitempty"`
	Indexes     []v190Named    `json:"indexes,omitempty"`
	Sequences   []v190Sequence `json:"sequences,omitempty"`
}

// v190Step is one leftover. gone is a catalog query, on args, answering true
// when the leftover is not there (for a rename: the from name is absent,
// whoever holds the other); read, run only when it is there, returns the
// record of what up will change, or the reason up must not change it; up is
// the change and down its inverse, both from the record.
type v190Step struct {
	action string
	gone   string
	args   []any
	read   func(tx *sql.Tx) (*v190Record, error)
	up     func(r *v190Record) []string
	down   func(r *v190Record) []string
}

// isGone runs the step's gone query. A query that fails reads as not gone, so
// verify re-runs the (idempotent) migration rather than trusting it.
func (s v190Step) isGone(q schema.Querier) bool {
	var gone bool
	if err := q.QueryRow(s.gone, s.args...).Scan(&gone); err != nil {
		return false
	}
	return gone
}

// find is the record of what up will change, nil when the leftover is gone.
func (s v190Step) find(tx *sql.Tx) (*v190Record, error) {
	var gone bool
	if err := tx.QueryRow(s.gone, s.args...).Scan(&gone); err != nil {
		return nil, err
	}
	if gone {
		return nil, nil
	}
	return s.read(tx)
}

// The tables go first: at one plant payload_instances was the old payloads
// table, and it held the names the renames below give back to payloads.
var v190Steps = []v190Step{
	v190DropTable("db_test"),
	v190DropTable("payload_instances"),
	v190DropTable("lane_access"),
	v190SetDefault("demand_registry", "payload_code", "''::text"),
	v190DropConstraint("payloads", "payloads_bin_id_fkey1"),
	v190DropColumn("payloads", "bin_id"),
	v190DropColumn("payloads", "manifest_confirmed"),
	v190RenameConstraint("payloads", "payloads_pkey1", "payloads_pkey"),
	v190RenameConstraint("payloads", "payloads_code_key1", "payloads_code_key"),
	v190RenameSequence("payloads_id_seq1", "payloads_id_seq"),
	v190DropColumn("orders", "deferred_steps_json"),
	v190DropConstraint("nodes", "nodes_node_type_id_fkey"),
	v190Retype("nodes", "node_type_id", "bigint"),
	v190Retype("nodes", "parent_id", "bigint"),
}

// v190Tables are the tables the steps touch, locked first where there.
var v190Tables = []string{"demand_registry", "payloads", "orders", "nodes", "db_test", "payload_instances", "lane_access"}

func v190Lock(tx *sql.Tx) error {
	var present []string
	for _, t := range v190Tables {
		if schema.TableExists(tx, t) {
			present = append(present, t)
		}
	}
	return migrationExec(tx, "v190 lock", "LOCK TABLE "+strings.Join(present, ", ")+" IN ACCESS EXCLUSIVE MODE")
}

// v190Up is migration 190's apply function.
func v190Up(tx *sql.Tx) error {
	if err := v190Lock(tx); err != nil {
		return err
	}
	for _, s := range v190Steps {
		r, err := s.find(tx)
		if err != nil {
			return fmt.Errorf("v190 %s: %w", s.action, err)
		}
		if r == nil {
			continue
		}
		old, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO audit_log (entity_type, entity_id, action, old_value, new_value, actor)
			VALUES ($1, 190, $2, $3, $4, $5)`, v190Entity, s.action, string(old), r.To, v190Actor); err != nil {
			return fmt.Errorf("v190 %s: record it: %w", s.action, err)
		}
		for _, q := range s.up(r) {
			if err := migrationExec(tx, "v190 "+s.action, q); err != nil {
				return err
			}
		}
		log.Printf("v190: %s %s", s.action, v190Describe(r))
	}
	return nil
}

// v190Verify is v190's post-condition: every leftover is gone. Catalog reads
// only; a read that fails is a leftover there (re-run), never one gone.
func v190Verify(q schema.Querier) bool {
	for _, s := range v190Steps {
		if !s.isGone(q) {
			return false
		}
	}
	return true
}

// v190Down puts back, newest first, every object up changed on this database,
// from up's audit rows, and deletes those rows. Foreign keys go on last.
func v190Down(tx *sql.Tx) error {
	byAction := map[string]v190Step{}
	for _, s := range v190Steps {
		byAction[s.action] = s
	}
	rows, err := tx.Query(`SELECT id, action, old_value FROM audit_log
		 WHERE entity_type = $1 AND entity_id = 190 AND actor = $2 ORDER BY id DESC`, v190Entity, v190Actor)
	if err != nil {
		return fmt.Errorf("v190 down: read up's record: %w", err)
	}
	type undo struct {
		id     int64
		action string
		r      v190Record
	}
	var todo []undo
	for rows.Next() {
		var u undo
		var old string
		if err := rows.Scan(&u.id, &u.action, &old); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(old), &u.r); err != nil {
			rows.Close()
			return fmt.Errorf("v190 down: audit_log row %d: %w", u.id, err)
		}
		todo = append(todo, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}
	if err := v190Lock(tx); err != nil {
		return err
	}
	var foreignKeys []string
	for _, u := range todo {
		s, ok := byAction[u.action]
		if !ok {
			return fmt.Errorf("v190 down: audit_log row %d: no step %q", u.id, u.action)
		}
		for _, q := range s.down(&u.r) {
			if strings.Contains(q, " FOREIGN KEY ") {
				foreignKeys = append(foreignKeys, q)
				continue
			}
			if err := migrationExec(tx, "v190 down "+u.action, q); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM audit_log WHERE id = $1`, u.id); err != nil {
			return err
		}
		log.Printf("v190 down: undo %s %s", u.action, v190Describe(&u.r))
	}
	for _, q := range foreignKeys {
		if err := migrationExec(tx, "v190 down foreign key", q); err != nil {
			return err
		}
	}
	return nil
}

func v190Describe(r *v190Record) string {
	switch {
	case r.Column != nil:
		return r.Table + "." + r.Column.Name
	case r.Name != "" && r.Table != "":
		return r.Table + "." + r.Name
	case r.Name != "":
		return r.Name
	}
	return r.Table
}

// ── the catalog reads ─────────────────────────────────────────────────────

// The fragments the gone queries share: a column, a constraint, a sequence
// and a table of schema public, by name ($n placeholders as numbered).
const (
	v190ColumnSQL = `SELECT 1 FROM pg_attribute a JOIN pg_class t ON t.oid = a.attrelid
		 JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		WHERE t.relname = $1 AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped`
	v190ConstraintSQL = `SELECT 1 FROM pg_constraint k JOIN pg_class t ON t.oid = k.conrelid
		 JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		WHERE t.relname = $1 AND k.conname = `
	v190SequenceSQL = `SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'S' AND c.relname = `
)

// Who holds a name a rename wants. v190RelationHolderSQL ($1 the name): every
// table, index, sequence and view of schema public shares it, so the holder
// may be any of them; it is named with the table it belongs to.
// v190ConstraintHolderSQL ($1 the table, $2 from, $3 to): a constraint shares
// its table's constraint names and, where an index backs it, the index's
// name, a relation's.
const (
	v190RelationHolderSQL = `SELECT pg_describe_object('pg_class'::regclass, c.oid, 0) || COALESCE(
		 (SELECT ' of table ' || h.relname FROM pg_index x JOIN pg_class h ON h.oid = x.indrelid WHERE x.indexrelid = c.oid),
		 (SELECT ' owned by table ' || h.relname FROM pg_depend d JOIN pg_class h ON h.oid = d.refobjid
		   WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND c.relkind = 'S'
		     AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i') LIMIT 1), '')
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname = $1`
	v190ConstraintHolderSQL = `SELECT pg_describe_object('pg_constraint'::regclass, k.oid, 0)
		  FROM pg_constraint k JOIN pg_class t ON t.oid = k.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		 WHERE t.relname = $1 AND k.conname = $3
		UNION ALL
		SELECT pg_describe_object('pg_class'::regclass, c.oid, 0) || COALESCE(
		 (SELECT ' of table ' || h.relname FROM pg_index x JOIN pg_class h ON h.oid = x.indrelid WHERE x.indexrelid = c.oid), '')
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relname = $3
		   AND EXISTS (SELECT 1 FROM pg_constraint k JOIN pg_class t ON t.oid = k.conrelid
		                WHERE t.relname = $1 AND t.relnamespace = n.oid AND k.conname = $2 AND k.conindid <> 0)`
)

// v190NameFree refuses a rename of from to a name that is held, by q on args,
// with a sentence naming each holder.
func v190NameFree(tx *sql.Tx, from, to, q string, args ...any) error {
	held, err := v190Lines(tx, q, args...)
	if err != nil {
		return err
	}
	if len(held) > 0 {
		return fmt.Errorf("%s cannot take the name %s: %s holds it; rename or drop that, then start Core again (nothing was changed)",
			from, to, strings.Join(held, " and "))
	}
	return nil
}

// v190Lines runs a one-column catalog query.
func v190Lines(tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// What a drop takes. Each v190Going* is the objects a drop takes that v190
// records or Postgres owns: a table ($1) with its row type, constraints,
// indexes, defaults and owned sequences; a column ($1, $2) with the
// constraints and indexes over it and its default; a constraint ($1, $2) with
// its index. v190DependentsSQL is whatever else hangs on one of them: it
// would make the drop fail (a view, another table's key or default) or go
// with it unrecorded, so that down could not put it back (a trigger, a
// policy, an index whose predicate reads the column). Internal dependencies
// are Postgres's own, except an identity column's sequence.
const (
	v190TableRef = `t AS (SELECT c.oid, c.reltype FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname = $1)`
	v190GoingTable = `WITH ` + v190TableRef + `, going (classid, objid, objsubid) AS (
		SELECT 'pg_class'::regclass::oid, t.oid, 0 FROM t
		UNION ALL SELECT 'pg_type'::regclass::oid, t.reltype, 0 FROM t
		UNION ALL SELECT 'pg_type'::regclass::oid, y.typarray, 0 FROM t JOIN pg_type y ON y.oid = t.reltype
		UNION ALL SELECT 'pg_constraint'::regclass::oid, k.oid, 0 FROM t JOIN pg_constraint k ON k.conrelid = t.oid
		 WHERE k.contype IN ('p', 'u', 'c', 'f', 'x', 'n')
		UNION ALL SELECT 'pg_class'::regclass::oid, x.indexrelid, 0 FROM t JOIN pg_index x ON x.indrelid = t.oid
		UNION ALL SELECT 'pg_attrdef'::regclass::oid, a.oid, 0 FROM t JOIN pg_attrdef a ON a.adrelid = t.oid
		UNION ALL SELECT 'pg_class'::regclass::oid, s.oid, 0 FROM t
		  JOIN pg_depend o ON o.refobjid = t.oid AND o.refclassid = 'pg_class'::regclass
		   AND o.classid = 'pg_class'::regclass AND o.deptype = 'a'
		  JOIN pg_class s ON s.oid = o.objid AND s.relkind = 'S')`
	v190GoingColumn = `WITH ` + v190TableRef + `, col AS (SELECT t.oid, a.attnum FROM t
		  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attname = $2 AND NOT a.attisdropped),
		going (classid, objid, objsubid) AS (
		SELECT 'pg_class'::regclass::oid, col.oid, col.attnum::integer FROM col
		UNION ALL SELECT 'pg_constraint'::regclass::oid, k.oid, 0 FROM col JOIN pg_constraint k ON k.conrelid = col.oid
		 WHERE k.contype IN ('p', 'u', 'c', 'f', 'x', 'n') AND col.attnum = ANY (k.conkey)
		UNION ALL SELECT 'pg_class'::regclass::oid, x.indexrelid, 0 FROM col JOIN pg_index x ON x.indrelid = col.oid
		 WHERE col.attnum = ANY (x.indkey)
		UNION ALL SELECT 'pg_attrdef'::regclass::oid, a.oid, 0 FROM col
		  JOIN pg_attrdef a ON a.adrelid = col.oid AND a.adnum = col.attnum)`
	v190GoingConstraint = `WITH ` + v190TableRef + `, k AS (SELECT c.oid, c.conindid FROM t
		  JOIN pg_constraint c ON c.conrelid = t.oid AND c.conname = $2),
		going (classid, objid, objsubid) AS (
		SELECT 'pg_constraint'::regclass::oid, k.oid, 0 FROM k
		UNION ALL SELECT 'pg_class'::regclass::oid, k.conindid, 0 FROM k WHERE k.conindid <> 0 AND EXISTS (
		 SELECT 1 FROM pg_depend o WHERE o.classid = 'pg_class'::regclass AND o.objid = k.conindid
		    AND o.refclassid = 'pg_constraint'::regclass AND o.refobjid = k.oid AND o.deptype = 'i'))`
	v190DependentsSQL = `
		SELECT DISTINCT pg_describe_object(d.classid, d.objid, d.objsubid)
		  FROM pg_depend d
		  JOIN going g ON g.classid = d.refclassid AND g.objid = d.refobjid
		   AND (g.objsubid = 0 OR g.objsubid = d.refobjsubid)
		 WHERE (d.deptype IN ('n', 'a') OR (d.deptype = 'i' AND d.classid = 'pg_class'::regclass
		        AND EXISTS (SELECT 1 FROM pg_class s WHERE s.oid = d.objid AND s.relkind = 'S')))
		   AND NOT EXISTS (SELECT 1 FROM going w WHERE w.classid = d.classid AND w.objid = d.objid
		        AND (w.objsubid = 0 OR w.objsubid = d.objsubid))
		 ORDER BY 1`
)

// v190NoDependents refuses a drop of what with a sentence naming every
// dependent going does not cover.
func v190NoDependents(tx *sql.Tx, what, going string, args ...any) error {
	found, err := v190Lines(tx, going+v190DependentsSQL, args...)
	if err != nil {
		return err
	}
	if len(found) > 0 {
		return fmt.Errorf("%s cannot be dropped while these depend on it: %s; drop or move them, then start Core again (nothing was changed)",
			what, strings.Join(found, "; "))
	}
	return nil
}

// v190ReadColumn is a column of a table in public as the catalog defines it;
// nil when it is not there.
func v190ReadColumn(tx *sql.Tx, table, column string) (*v190Column, error) {
	var c v190Column
	var def sql.NullString
	err := tx.QueryRow(`SELECT a.attname, format_type(a.atttypid, a.atttypmod), pg_get_expr(d.adbin, d.adrelid), a.attnotnull
		  FROM pg_attribute a
		  JOIN pg_class t ON t.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE t.relname = $1 AND a.attname = $2 AND a.attnum > 0 AND NOT a.attisdropped`, table, column).
		Scan(&c.Name, &c.Type, &def, &c.NotNull)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if def.Valid {
		c.Default = &def.String
	}
	return &c, nil
}

// v190ReadNamed runs a two-column (name, definition) catalog query.
func v190ReadNamed(tx *sql.Tx, q string, args ...any) ([]v190Named, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []v190Named
	for rows.Next() {
		var n v190Named
		if err := rows.Scan(&n.Name, &n.Def); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// v190Constraints are a table's constraints, keys first (a foreign key may
// lean on one), or only those over one column.
func v190Constraints(tx *sql.Tx, table, column string) ([]v190Named, error) {
	return v190ReadNamed(tx, `SELECT k.conname, pg_get_constraintdef(k.oid)
		  FROM pg_constraint k
		  JOIN pg_class t ON t.oid = k.conrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		 WHERE t.relname = $1 AND k.contype IN ('p', 'u', 'c', 'f', 'x')
		   AND ($2 = '' OR (SELECT a.attnum FROM pg_attribute a WHERE a.attrelid = t.oid AND a.attname = $2) = ANY (k.conkey))
		 ORDER BY CASE k.contype WHEN 'p' THEN 0 WHEN 'u' THEN 1 ELSE 2 END, k.conname`, table, column)
}

// v190Indexes are a table's indexes that back no constraint, or only those
// over one column.
func v190Indexes(tx *sql.Tx, table, column string) ([]v190Named, error) {
	return v190ReadNamed(tx, `SELECT ic.relname, pg_get_indexdef(x.indexrelid)
		  FROM pg_index x
		  JOIN pg_class t ON t.oid = x.indrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		  JOIN pg_class ic ON ic.oid = x.indexrelid
		 WHERE t.relname = $1
		   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = x.indexrelid)
		   AND ($2 = '' OR (SELECT a.attnum FROM pg_attribute a WHERE a.attrelid = t.oid AND a.attname = $2) = ANY (x.indkey))
		 ORDER BY ic.relname`, table, column)
}

// ── the steps ──────────────────────────────────────────────────────────────

func v190SetDefault(table, column, def string) v190Step {
	return v190Step{
		action: "set default",
		gone:   `SELECT NOT EXISTS (` + v190ColumnSQL + ` AND NOT a.atthasdef)`,
		args:   []any{table, column},
		read: func(tx *sql.Tx) (*v190Record, error) {
			c, err := v190ReadColumn(tx, table, column)
			if err != nil || c == nil {
				return nil, err
			}
			return &v190Record{Table: table, Column: c, To: def}, nil
		},
		up: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s`, r.Table, r.Column.Name, r.To)}
		},
		down: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT`, r.Table, r.Column.Name)}
		},
	}
}

func v190DropConstraint(table, name string) v190Step {
	return v190Step{
		action: "drop constraint",
		gone:   `SELECT NOT EXISTS (` + v190ConstraintSQL + `$2)`,
		args:   []any{table, name},
		read: func(tx *sql.Tx) (*v190Record, error) {
			if err := v190NoDependents(tx, table+"."+name, v190GoingConstraint, table, name); err != nil {
				return nil, err
			}
			return &v190Record{Table: table, Name: name, Def: schema.ConstraintDef(tx, table, name)}, nil
		},
		up: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT %s`, r.Table, r.Name)}
		},
		down: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s %s`, r.Table, r.Name, r.Def)}
		},
	}
}

func v190DropColumn(table, column string) v190Step {
	return v190Step{
		action: "drop column",
		gone:   `SELECT NOT EXISTS (` + v190ColumnSQL + `)`,
		args:   []any{table, column},
		read: func(tx *sql.Tx) (*v190Record, error) {
			c, err := v190ReadColumn(tx, table, column)
			if err != nil || c == nil {
				return nil, err
			}
			if err := v190NoDependents(tx, table+"."+column, v190GoingColumn, table, column); err != nil {
				return nil, err
			}
			r := &v190Record{Table: table, Column: c}
			if r.Constraints, err = v190Constraints(tx, table, column); err != nil {
				return nil, err
			}
			if r.Indexes, err = v190Indexes(tx, table, column); err != nil {
				return nil, err
			}
			return r, nil
		},
		up: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, r.Table, r.Column.Name)}
		},
		down: func(r *v190Record) []string {
			out := []string{fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s`, r.Table, v190ColumnDef(*r.Column))}
			for _, k := range r.Constraints {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s %s`, r.Table, k.Name, k.Def))
			}
			for _, x := range r.Indexes {
				out = append(out, x.Def)
			}
			return out
		},
	}
}

func v190RenameConstraint(table, from, to string) v190Step {
	return v190Step{
		action: "rename constraint",
		gone:   `SELECT NOT EXISTS (` + v190ConstraintSQL + `$2)`,
		args:   []any{table, from},
		read: func(tx *sql.Tx) (*v190Record, error) {
			if err := v190NameFree(tx, from, to, v190ConstraintHolderSQL, table, from, to); err != nil {
				return nil, err
			}
			return &v190Record{Table: table, Name: from, To: to}, nil
		},
		up: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s RENAME CONSTRAINT %s TO %s`, r.Table, r.Name, r.To)}
		},
		down: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s RENAME CONSTRAINT %s TO %s`, r.Table, r.To, r.Name)}
		},
	}
}

func v190RenameSequence(from, to string) v190Step {
	return v190Step{
		action: "rename sequence",
		gone:   `SELECT NOT EXISTS (` + v190SequenceSQL + `$1)`,
		args:   []any{from},
		read: func(tx *sql.Tx) (*v190Record, error) {
			if err := v190NameFree(tx, from, to, v190RelationHolderSQL, to); err != nil {
				return nil, err
			}
			return &v190Record{Name: from, To: to}, nil
		},
		up: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER SEQUENCE %s RENAME TO %s`, r.Name, r.To)}
		},
		down: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER SEQUENCE %s RENAME TO %s`, r.To, r.Name)}
		},
	}
}

func v190Retype(table, column, typ string) v190Step {
	return v190Step{
		action: "retype column",
		gone:   `SELECT NOT EXISTS (` + v190ColumnSQL + ` AND format_type(a.atttypid, a.atttypmod) <> $3)`,
		args:   []any{table, column, typ},
		read: func(tx *sql.Tx) (*v190Record, error) {
			c, err := v190ReadColumn(tx, table, column)
			if err != nil || c == nil {
				return nil, err
			}
			return &v190Record{Table: table, Column: c, To: typ}, nil
		},
		up: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE %s`, r.Table, r.Column.Name, r.To)}
		},
		down: func(r *v190Record) []string {
			return []string{fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE %s`, r.Table, r.Column.Name, r.Column.Type)}
		},
	}
}

func v190DropTable(table string) v190Step {
	return v190Step{
		action: "drop table",
		gone: `SELECT NOT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname = $1)`,
		args: []any{table},
		read: func(tx *sql.Tx) (*v190Record, error) {
			if err := v190NoDependents(tx, "table "+table, v190GoingTable, table); err != nil {
				return nil, err
			}
			return v190ReadTable(tx, table)
		},
		up: func(r *v190Record) []string {
			return []string{`DROP TABLE ` + r.Table}
		},
		down: func(r *v190Record) []string {
			var out []string
			for _, s := range r.Sequences {
				create := `CREATE SEQUENCE ` + s.Name
				if s.Type != "" {
					create += ` AS ` + s.Type
				}
				out = append(out, create)
			}
			cols := make([]string, len(r.Columns))
			for i, c := range r.Columns {
				cols[i] = v190ColumnDef(c)
			}
			out = append(out, fmt.Sprintf("CREATE TABLE %s (%s)", r.Table, strings.Join(cols, ", ")))
			for _, k := range r.Constraints {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s %s`, r.Table, k.Name, k.Def))
			}
			for _, x := range r.Indexes {
				out = append(out, x.Def)
			}
			for _, s := range r.Sequences {
				out = append(out, fmt.Sprintf(`ALTER SEQUENCE %s OWNED BY %s.%s`, s.Name, r.Table, s.Column))
			}
			return out
		},
	}
}

// v190ReadTable is a table's definition: its columns in order, constraints,
// indexes and the sequences its columns own.
func v190ReadTable(tx *sql.Tx, table string) (*v190Record, error) {
	r := &v190Record{Table: table}
	names, err := v190ReadNamed(tx, `SELECT a.attname, '' FROM pg_attribute a
		  JOIN pg_class t ON t.oid = a.attrelid
		  JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		 WHERE t.relname = $1 AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, table)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		c, err := v190ReadColumn(tx, table, name.Name)
		if err != nil {
			return nil, err
		}
		if c == nil {
			return nil, fmt.Errorf("column %s.%s vanished while it was read", table, name.Name)
		}
		r.Columns = append(r.Columns, *c)
	}
	if r.Constraints, err = v190Constraints(tx, table, ""); err != nil {
		return nil, err
	}
	if r.Indexes, err = v190Indexes(tx, table, ""); err != nil {
		return nil, err
	}
	if r.Sequences, err = v190Sequences(tx, table); err != nil {
		return nil, err
	}
	return r, nil
}

// v190Sequences are the sequences a table's columns own.
func v190Sequences(tx *sql.Tx, table string) ([]v190Sequence, error) {
	rows, err := tx.Query(`SELECT s.relname, a.attname, format_type(q.seqtypid, NULL)
		  FROM pg_depend d
		  JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
		  JOIN pg_sequence q ON q.seqrelid = s.oid
		  JOIN pg_class t ON t.oid = d.refobjid
		  JOIN pg_namespace n ON n.oid = t.relnamespace AND n.nspname = 'public'
		  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		 WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
		   AND d.deptype = 'a' AND t.relname = $1
		 ORDER BY s.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []v190Sequence
	for rows.Next() {
		var s v190Sequence
		if err := rows.Scan(&s.Name, &s.Column, &s.Type); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// v190ColumnDef is a column definition for CREATE TABLE or ADD COLUMN.
func v190ColumnDef(c v190Column) string {
	def := c.Name + " " + c.Type
	if c.Default != nil {
		def += " DEFAULT " + *c.Default
	}
	if c.NotNull {
		def += " NOT NULL"
	}
	return def
}
