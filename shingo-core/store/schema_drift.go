package store

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"shingocore/store/schema"
)

// The drift report: at boot, after migrate, the live schema read in one
// catalog query and compared with the snapshot this build ships
// (store/schema/schema.snapshot.sql, embedded): its tables, columns (type,
// default, NOT NULL), indexes and constraints, runtime partitions skipped.
// Each difference is a log line and a Core Health reason naming the object.
// It refuses nothing: since the squash at v184 a boot no longer repairs a
// hand-dropped index or column as the replay did, and this is how one is
// seen.

var (
	driftTableRe     = regexp.MustCompile(`(?s)^CREATE TABLE public\.(\w+) \(\n(.*?)\n\)(\nPARTITION BY .*)?;$`)
	driftPartitionRe = regexp.MustCompile(`^CREATE TABLE public\.\w+ PARTITION OF `)
	driftIndexRe     = regexp.MustCompile(`^(CREATE (?:UNIQUE )?INDEX (\w+) ON .*);$`)
	driftConRe       = regexp.MustCompile(`(?s)^ALTER TABLE (?:ONLY )?public\.(\w+)\s+ADD CONSTRAINT (\w+) (.*);$`)
	driftDefaultRe   = regexp.MustCompile(`^ALTER TABLE ONLY public\.(\w+) ALTER COLUMN (\w+) SET DEFAULT (.*);$`)
	driftAttachRe    = regexp.MustCompile(`ATTACH PARTITION public\.(\w+)`)
)

// snapshotObjects parses a pg_dump --schema-only text into its shape: each
// object keyed by its kind and name ("column orders.status"), with its
// definition as pg_dump writes it.
func snapshotObjects(dump string) (map[string]string, error) {
	out := map[string]string{}
	children := map[string]bool{}
	for _, m := range driftAttachRe.FindAllStringSubmatch(dump, -1) {
		children[m[1]] = true
	}
	defaults := map[string]string{}
	var columnOrder []string
	columns := map[string]string{}
	for _, stmt := range splitSchemaStatements(dump) {
		switch {
		case driftPartitionRe.MatchString(stmt):
			continue
		case strings.HasPrefix(stmt, "CREATE TABLE "):
			m := driftTableRe.FindStringSubmatch(stmt)
			if m == nil {
				return nil, fmt.Errorf("the snapshot has a CREATE TABLE the drift report cannot read: %.80s", stmt)
			}
			if children[m[1]] {
				continue
			}
			out["table "+m[1]] = ""
			for _, line := range strings.Split(m[2], "\n") {
				line = strings.TrimSuffix(strings.TrimSpace(line), ",")
				if name, def, ok := strings.Cut(line, " "); ok && name == "CONSTRAINT" {
					cname, cdef, _ := strings.Cut(def, " ")
					out["constraint "+m[1]+"."+cname] = cdef
					continue
				} else if ok {
					k := m[1] + "." + strings.Trim(name, `"`)
					columns[k] = def
					columnOrder = append(columnOrder, k)
				}
			}
		case driftIndexRe.MatchString(stmt):
			m := driftIndexRe.FindStringSubmatch(stmt)
			out["index "+m[2]] = m[1]
		case driftDefaultRe.MatchString(stmt):
			m := driftDefaultRe.FindStringSubmatch(stmt)
			defaults[m[1]+"."+m[2]] = m[3]
		case driftConRe.MatchString(stmt):
			m := driftConRe.FindStringSubmatch(stmt)
			if children[m[1]] {
				continue
			}
			out["constraint "+m[1]+"."+m[2]] = strings.Join(strings.Fields(m[3]), " ")
		}
	}
	for _, k := range columnOrder {
		def := columns[k]
		if d, ok := defaults[k]; ok {
			if base, ok := strings.CutSuffix(def, " NOT NULL"); ok {
				def = base + " DEFAULT " + d + " NOT NULL"
			} else {
				def += " DEFAULT " + d
			}
		}
		out["column "+k] = def
	}
	if len(out) < 100 {
		return nil, fmt.Errorf("read %d objects out of the snapshot: the parser is broken, not the schema", len(out))
	}
	return out, nil
}

// splitSchemaStatements splits a dump on lines ending a statement; a function
// body ($$ … $$) is one statement, and pg_dump's comment lines are dropped.
func splitSchemaStatements(dump string) []string {
	var out, cur []string
	inBody := false
	for _, line := range strings.Split(strings.ReplaceAll(dump, "\r\n", "\n"), "\n") {
		if !inBody && (strings.HasPrefix(line, "--") || strings.TrimSpace(line) == "" && len(cur) == 0) {
			continue
		}
		if strings.Count(line, "$$")%2 == 1 {
			inBody = !inBody
		}
		cur = append(cur, line)
		if !inBody && strings.HasSuffix(strings.TrimSpace(line), ";") {
			out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	return out
}

// liveObjectsSQL reads the live shape in pg_dump's own spelling: the same
// catalog functions pg_dump calls (format_type, pg_get_expr, pg_get_indexdef,
// pg_get_constraintdef), under the empty search_path pg_dump sets, so every
// name comes back schema-qualified as the snapshot has it. Partitions and
// their indexes and constraints are skipped; an index that backs a
// constraint is the constraint's.
const liveObjectsSQL = `
SELECT 'table ' || c.relname, ''
  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
UNION ALL
SELECT 'column ' || c.relname || '.' || a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod)
       || CASE WHEN a.attgenerated = 's' THEN ' GENERATED ALWAYS AS (' || pg_catalog.pg_get_expr(d.adbin, d.adrelid) || ') STORED'
               ELSE COALESCE(' DEFAULT ' || pg_catalog.pg_get_expr(d.adbin, d.adrelid), '') END
       || CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
  FROM pg_catalog.pg_attribute a
  JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition
   AND a.attnum > 0 AND NOT a.attisdropped
UNION ALL
SELECT 'index ' || ic.relname, pg_catalog.pg_get_indexdef(i.indexrelid)
  FROM pg_catalog.pg_index i
  JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
  JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
 WHERE n.nspname = 'public' AND NOT t.relispartition
   AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.conindid = i.indexrelid AND k.contype IN ('p', 'u', 'x'))
UNION ALL
SELECT 'constraint ' || t.relname || '.' || k.conname, pg_catalog.pg_get_constraintdef(k.oid)
  FROM pg_catalog.pg_constraint k
  JOIN pg_catalog.pg_class t ON t.oid = k.conrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
 WHERE n.nspname = 'public' AND NOT t.relispartition AND k.contype IN ('c', 'f', 'p', 'u', 'x')`

// liveObjects reads the live shape in one query, on one connection, under the
// empty search_path for that query only.
func (db *DB) liveObjects() (map[string]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SET LOCAL search_path = ''`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(liveObjectsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, def string
		if err := rows.Scan(&k, &def); err != nil {
			return nil, err
		}
		out[k] = strings.Join(strings.Fields(def), " ")
	}
	return out, rows.Err()
}

// SchemaDrift compares the live schema with the shipped snapshot and returns
// one sentence per difference, sorted, naming the object: what the snapshot
// has and the database lacks, what the database has that the snapshot does
// not, and what both have defined differently.
func (db *DB) SchemaDrift() ([]string, error) {
	want, err := snapshotObjects(schema.Snapshot)
	if err != nil {
		return nil, err
	}
	got, err := db.liveObjects()
	if err != nil {
		return nil, fmt.Errorf("read the live schema: %w", err)
	}
	// A table missing from one side is one difference, not one per column and
	// index it would have carried.
	goneTables, extraTables := map[string]bool{}, map[string]bool{}
	for k := range want {
		if t, ok := strings.CutPrefix(k, "table "); ok && !hasKey(got, k) {
			goneTables[t] = true
		}
	}
	for k := range got {
		if t, ok := strings.CutPrefix(k, "table "); ok && !hasKey(want, k) {
			extraTables[t] = true
		}
	}
	var out []string
	for k, def := range want {
		if t := objectTable(k, def); t != "" && goneTables[t] && !strings.HasPrefix(k, "table ") {
			continue
		}
		switch g, ok := got[k]; {
		case !ok:
			out = append(out, "schema drift: "+k+" is missing (this build ships it)")
		case g != strings.Join(strings.Fields(def), " "):
			out = append(out, "schema drift: "+k+" differs: the database has "+g+", this build ships "+def)
		}
	}
	for k, def := range got {
		if t := objectTable(k, def); t != "" && extraTables[t] && !strings.HasPrefix(k, "table ") {
			continue
		}
		if _, ok := want[k]; !ok {
			out = append(out, "schema drift: "+k+" is in the database and not in this build's schema")
		}
	}
	slices.Sort(out)
	return out, nil
}

// DriftAtBoot is the drift report Open took after migrating: nil for a
// database that matches the shipped schema, and for a DB opened without
// migrating.
func (db *DB) DriftAtBoot() []string { return db.driftAtBoot }

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

var indexTableRe = regexp.MustCompile(` ON (?:ONLY )?public\.(\w+) `)

// objectTable is the table an object belongs to: a column's or constraint's
// by its key, an index's by its definition.
func objectTable(key, def string) string {
	kind, name, _ := strings.Cut(key, " ")
	switch kind {
	case "table":
		return name
	case "column", "constraint":
		t, _, _ := strings.Cut(name, ".")
		return t
	case "index":
		if m := indexTableRe.FindStringSubmatch(def); m != nil {
			return m[1]
		}
	}
	return ""
}
