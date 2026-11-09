package main

import (
	"strings"
	"testing"
)

// A pg_dump fragment in the shapes the Core snapshot carries: a column list
// with defaults, a generated column and an inline check; the sequence default,
// primary key, unique and foreign key added by ALTER TABLE; a partial unique
// index; a partitioned table; and a function body whose semicolons must not
// end anything.
const pgFixture = `CREATE FUNCTION public.f() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
		 BEGIN
		   RETURN NEW;
		 END
		 $$;

CREATE TABLE public.bins (
    id bigint NOT NULL,
    label text DEFAULT ''::text NOT NULL,
    node_id bigint,
    status text DEFAULT 'available'::text NOT NULL,
    bare boolean GENERATED ALWAYS AS ((bare_of IS NOT NULL)) STORED NOT NULL,
    CONSTRAINT bins_status_check CHECK ((status = ANY (ARRAY['available'::text, 'retired'::text])))
);

CREATE TABLE public.events (
    id bigint NOT NULL,
    at timestamp with time zone NOT NULL
)
PARTITION BY RANGE (at);

CREATE TABLE public.nodes (
    id bigint NOT NULL,
    name text NOT NULL
);

ALTER TABLE ONLY public.bins ALTER COLUMN id SET DEFAULT nextval('public.bins_id_seq'::regclass);

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.nodes
    ADD CONSTRAINT nodes_name_key UNIQUE (name);

ALTER TABLE ONLY public.bins
    ADD CONSTRAINT bins_node_id_fkey FOREIGN KEY (node_id) REFERENCES public.nodes(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX bins_label_key ON public.bins USING btree (label) WHERE (label <> ''::text);
`

func TestParsePgDump_ReadsEachShape(t *testing.T) {
	tables, err := parsePgDump(pgFixture)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*sqlTable{}
	for _, tb := range tables {
		by[tb.name] = tb
	}
	if len(by) != 3 {
		t.Fatalf("tables = %v, want bins, events, nodes", tables)
	}
	bins := by["bins"]
	cols := map[string]sqlColumn{}
	for _, c := range bins.cols {
		cols[c.name] = c
	}
	cases := []struct {
		col           string
		typ, def, key string
		notNull       bool
	}{
		{"id", "bigint", "nextval('bins_id_seq')", "PK", true},
		{"label", "text", "''", "", true},
		{"node_id", "bigint", "", "→ `nodes(id) ON DELETE SET NULL`", false},
		{"status", "text", "'available'", "", true},
		{"bare", "boolean", "generated: ((bare_of IS NOT NULL))", "", true},
	}
	for _, c := range cases {
		got, ok := cols[c.col]
		if !ok {
			t.Errorf("bins.%s not read", c.col)
			continue
		}
		if got.typ != c.typ || got.def != c.def || got.key != c.key || got.notNull != c.notNull {
			t.Errorf("bins.%s = %+v, want type %q default %q key %q notNull %v", c.col, got, c.typ, c.def, c.key, c.notNull)
		}
	}
	notes := strings.Join(bins.notes, "\n")
	for _, want := range []string{"Check `(status = ANY (ARRAY['available', 'retired']))`", "Unique (label) where `label <> ''`"} {
		if !strings.Contains(notes, want) {
			t.Errorf("bins notes miss %q:\n%s", want, notes)
		}
	}
	if !bins.refs["nodes"] {
		t.Error("bins does not reference nodes")
	}
	if c := by["nodes"].col("name"); c == nil || c.key != "unique" {
		t.Errorf("nodes.name = %+v, want unique", c)
	}
	if n := strings.Join(by["events"].notes, ""); !strings.Contains(n, "Partitioned: `RANGE (at)`") {
		t.Errorf("events notes = %q, want the partitioning", n)
	}
	if _, err := parsePgDump("CREATE TABLE public.t (\n    CONSTRAINT x EXCLUDE (a WITH =)\n);\n"); err == nil {
		t.Error("an unread table constraint parsed without error")
	}
}

// A SQLite DDL fragment in the shapes the Edge snapshot carries: trailing and
// leading `--` comments, a comment line indented under the column it belongs
// to, columns an ALTER appended after the closing line, a CHECK written
// against its parenthesis, inline references and table-level keys.
const sqliteFixture = `CREATE UNIQUE INDEX idx_t_live
    ON t(a, b) WHERE gone IS NULL;

CREATE TABLE t (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    a      TEXT NOT NULL,   -- the a, with a | pipe
    role   TEXT NOT NULL,
        -- 'x' | 'y'
    -- b leads here
    -- over two lines.
    b      INTEGER NOT NULL DEFAULT 0 REFERENCES other(id) ON DELETE CASCADE,
    kind   TEXT NOT NULL CHECK(kind IN ('p','q')),
    gone   TEXT,
    UNIQUE(a, role)
, added INTEGER NOT NULL DEFAULT 0, added2 TEXT NOT NULL DEFAULT '');

CREATE TABLE other(
    id INTEGER, k TEXT,
    PRIMARY KEY (id, k)
);
`

func TestParseSQLiteDDL_ReadsEachShape(t *testing.T) {
	tables, err := parseSQLiteDDL(sqliteFixture)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]*sqlTable{}
	for _, tb := range tables {
		by[tb.name] = tb
	}
	tb := by["t"]
	if tb == nil || by["other"] == nil {
		t.Fatalf("tables = %v", tables)
	}
	var names []string
	cols := map[string]sqlColumn{}
	for _, c := range tb.cols {
		names = append(names, c.name)
		cols[c.name] = c
	}
	if got := strings.Join(names, ","); got != "id,a,role,b,kind,gone,added,added2" {
		t.Errorf("columns = %s", got)
	}
	cases := map[string]sqlColumn{
		"id":     {typ: "INTEGER", key: "PK autoincrement", notNull: true},
		"a":      {typ: "TEXT", notNull: true, comment: "the a, with a | pipe"},
		"role":   {typ: "TEXT", notNull: true, comment: "'x' | 'y'"},
		"b":      {typ: "INTEGER", def: "0", key: "→ `other(id) ON DELETE CASCADE`", notNull: true, comment: "b leads here over two lines."},
		"kind":   {typ: "TEXT", notNull: true},
		"added2": {typ: "TEXT", def: "''", notNull: true},
	}
	for name, want := range cases {
		got := cols[name]
		want.name = name
		if got != want {
			t.Errorf("t.%s = %+v, want %+v", name, got, want)
		}
	}
	notes := strings.Join(tb.notes, "\n")
	for _, want := range []string{"Check `kind IN ('p','q')`", "Unique (a, role)", "Unique (a, b) where `gone IS NULL`"} {
		if !strings.Contains(notes, want) {
			t.Errorf("t notes miss %q:\n%s", want, notes)
		}
	}
	if got := strings.Join(by["other"].pk, ","); got != "id,k" {
		t.Errorf("other pk = %q, want id,k", got)
	}
	if _, err := parseSQLiteDDL("CREATE TABLE x (\n    a TEXT NOT NULL WEIRD\n);\n"); err == nil {
		t.Error("an unread column constraint parsed without error")
	}
}

// TestEmitSchema_RealSnapshots: both snapshots parse, every table has columns,
// the index links every table to its own section, and one named table renders
// alone — so a snapshot shape the parser does not know fails here.
func TestEmitSchema_RealSnapshots(t *testing.T) {
	for _, c := range []struct{ path, side, table, column string }{
		{coreSnapshot, "core", "edge_registry", "station_uid"},
		{edgeSnapshot, "edge", "style_node_claims", "swap_mode"},
	} {
		tables, err := loadSchema(repoRoot, c.path, c.side)
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range tables {
			if len(tb.cols) == 0 {
				t.Errorf("%s: %s has no columns", c.path, tb.name)
			}
		}
		full, err := emitSchema(repoRoot, c.path, c.side, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range tables {
			if !strings.Contains(full, "(#"+tableAnchor(c.side, tb.name)+")") {
				t.Errorf("%s: the index does not link %s", c.path, tb.name)
			}
		}
		one, err := emitSchema(repoRoot, c.path, c.side, []string{c.table})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(one, "| `"+c.column+"` |") || strings.Contains(one, "####") {
			t.Errorf("%s %s alone:\n%s", c.path, c.table, one)
		}
		if _, err := emitSchema(repoRoot, c.path, c.side, []string{"no_such_table"}); err == nil {
			t.Errorf("%s: an unknown table emitted without error", c.path)
		}
	}
}

// TestEmitVocabularies_RealTree pins the two enum tables on the real
// protocol package: every OrderType constant is a row, and swap modes split
// into configurable rows and the rest.
func TestEmitVocabularies_RealTree(t *testing.T) {
	types, err := emitOrderTypes(repoRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| `retrieve` | `OrderTypeRetrieve` |", "| `complex` | `OrderTypeComplex` |"} {
		if !strings.Contains(types, want) {
			t.Errorf("order types miss %q:\n%s", want, types)
		}
	}
	modes, err := emitSwapModes(repoRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(modes, "| `two_robot` | `SwapModeTwoRobot` | yes |") || !strings.Contains(modes, "`manual_swap` (`SwapModeManualSwap`)") {
		t.Errorf("swap modes:\n%s", modes)
	}
}
