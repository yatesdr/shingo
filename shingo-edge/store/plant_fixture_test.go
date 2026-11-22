package store

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"shingo/shared/scenefixtures"
)

// The synthetic plants of shared/scenefixtures, loaded with their rows as they
// stand and not through the store's writers: dirty rows included (claims whose
// style row is gone, styles whose process row is gone, soft-deleted styles).
// The fixture is the synthetic plant, not a pull (owner ruling 2026-09-13).
// Embedded rather than read by path, so a pin cannot skip on a missing input.

type plantFixture struct {
	Plant           string           `json:"plant"`
	Processes       []map[string]any `json:"processes"`
	Styles          []map[string]any `json:"styles"`
	ProcessNodes    []map[string]any `json:"process_nodes"`
	PayloadCatalog  []map[string]any `json:"payload_catalog"`
	StyleNodeClaims []map[string]any `json:"style_node_claims"`
	Nodes           []struct {
		Name       string  `json:"name"`
		ParentName *string `json:"parent_name"`
	} `json:"nodes"`
}

// dirtyRows is what the fixture loader saw, so a test can assert it HAD the
// dirty input rather than passing on a clean one.
type dirtyRows struct {
	orphanClaims        int // claim whose style row does not exist
	deletedStyleClaims  int // claim whose style is soft-deleted
	missingProcessClaim int // claim whose style's process row does not exist
}

// readPlantFixture decodes a synthetic plant into untyped rows.
//
// UNTYPED ON PURPOSE: insertVerbatim writes whatever columns the row and the
// table share, so a column added to the fixture reaches the database without a
// struct field, and a column the picture never reads is still in the row the
// backfill runs over. json.Number, so an id stays an id.
func readPlantFixture(t *testing.T, plant string) plantFixture {
	t.Helper()
	var raw []byte
	switch plant {
	case "a":
		raw = scenefixtures.RawA()
	case "b":
		raw = scenefixtures.RawB()
	default:
		t.Fatalf("readPlantFixture: no plant %q", plant)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var f plantFixture
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode plant %s: %v", plant, err)
	}
	if len(f.StyleNodeClaims) == 0 || len(f.Processes) == 0 {
		t.Fatalf("plant %s carries no claims/processes — nothing to pin", plant)
	}
	return f
}

// insertVerbatim writes each row into table using exactly the columns the row
// and the table share. Values go in as the JSON holds them (json.Number →
// int64 where integral), because the point is the plant's rows, not a
// re-validated copy of them: UpsertClaim would refuse an orphan or normalise
// a field, and then the test would be about something other than the plant.
func insertVerbatim(t *testing.T, db *DB, table string, rows []map[string]any) {
	t.Helper()
	colRows, err := db.Query(`SELECT name FROM pragma_table_info('` + table + `')`)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	tableCols := map[string]bool{}
	for colRows.Next() {
		var c string
		if err := colRows.Scan(&c); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		tableCols[c] = true
	}
	colRows.Close()

	for _, row := range rows {
		cols := make([]string, 0, len(row))
		for k := range row {
			if tableCols[k] {
				cols = append(cols, k)
			}
		}
		sort.Strings(cols)
		args := make([]any, 0, len(cols))
		for _, c := range cols {
			args = append(args, fixtureValue(row[c]))
		}
		q := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (" +
			strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("insert %s row %v: %v", table, row["id"], err)
		}
	}
}

func fixtureValue(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		// Neither an int nor a float: the fixture wrote something json.Number
		// cannot parse. Carry the raw text rather than a silent 0 — a fixture
		// defect must be visible in the row it lands in, not rounded away.
		return x.String()
	default:
		return v
	}
}

// loadPlantFixture puts the plant's rows into db and reports the dirty ones.
func loadPlantFixture(t *testing.T, db *DB, f plantFixture) dirtyRows {
	t.Helper()
	insertVerbatim(t, db, "processes", f.Processes)
	insertVerbatim(t, db, "styles", f.Styles)
	insertVerbatim(t, db, "process_nodes", f.ProcessNodes)
	// The catalog goes in BEFORE the claims: a claim's capacity is resolved
	// from it on every read, so a fixture loaded without it would read every
	// capacity as 0 and the pins below would be about nothing.
	insertVerbatim(t, db, "payload_catalog", f.PayloadCatalog)
	insertVerbatim(t, db, "style_node_claims", f.StyleNodeClaims)

	procs := map[int64]bool{}
	for _, p := range f.Processes {
		procs[fixtureInt(p["id"])] = true
	}
	styles := map[int64]map[string]any{}
	for _, s := range f.Styles {
		styles[fixtureInt(s["id"])] = s
	}
	var d dirtyRows
	for _, c := range f.StyleNodeClaims {
		s, ok := styles[fixtureInt(c["style_id"])]
		switch {
		case !ok:
			d.orphanClaims++
		case s["deleted_at"] != nil:
			d.deletedStyleClaims++
		case !procs[fixtureInt(s["process_id"])]:
			d.missingProcessClaim++
		}
	}
	return d
}

func fixtureInt(v any) int64 {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

func fixtureStr(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
