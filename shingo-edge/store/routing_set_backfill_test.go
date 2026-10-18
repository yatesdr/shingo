package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingo/shared/scenefixtures"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// routing_set_backfill_test.go — the routing-set backfill against a whole
// plant's rows.
//
// THE ROWS GO IN VERBATIM, not through the store's writers: every row of
// processes, styles, process_nodes, payload_catalog and style_node_claims that
// shared/scenefixtures carries, dirty ones included — claims whose style row
// is gone, styles whose process row is gone, and soft-deleted styles. A naive
// claim→style→process join drops those; one that assumes the lookup succeeds
// throws. The backfill must do neither, and this is where that is checked.
//
// The fixture is the synthetic plant, not a pull: owner ruling 2026-09-13, and
// scenefixtures' own header says how it is derived. It preserves the dirty
// rows and every column, which is all this file reads it for. It used to be a
// SECOND copy of the same 2026-09-03 rows under store/testdata — the same
// plants, the same date, the same values, with the columns scenefixtures had
// dropped — and two files claiming to be one pull drift on the next re-pull.
// Embedded rather than read by path, so a pin cannot skip on a missing input:
// a pin that goes green on nothing is not a pin.

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

// expectedRoutingSets computes, from the JSON rows alone, what the backfill
// must produce per process: every non-empty source / staging / destination
// field of a claim on a LIVE style whose process exists, minus the process's
// own live positions. An independent derivation, so the test is not asking
// the SQL whether the SQL is right.
func expectedRoutingSets(f plantFixture) map[int64]map[string]bool {
	procs := map[int64]bool{}
	for _, p := range f.Processes {
		procs[fixtureInt(p["id"])] = true
	}
	styles := map[int64]map[string]any{}
	for _, s := range f.Styles {
		styles[fixtureInt(s["id"])] = s
	}
	positions := map[int64]map[string]bool{}
	for _, n := range f.ProcessNodes {
		if n["deleted_at"] != nil {
			continue
		}
		pid := fixtureInt(n["process_id"])
		if positions[pid] == nil {
			positions[pid] = map[string]bool{}
		}
		positions[pid][fixtureStr(n["core_node_name"])] = true
	}
	fields := map[string]string{
		"inbound_source":              domain.RoutingRoleSource,
		"outbound_destination":        domain.RoutingRoleDestination,
		"changeover_evac_destination": domain.RoutingRoleDestination,
		"inbound_staging":             domain.RoutingRoleStaging,
		"outbound_staging":            domain.RoutingRoleStaging,
	}
	out := map[int64]map[string]bool{}
	for pid := range procs {
		out[pid] = map[string]bool{}
	}
	for _, c := range f.StyleNodeClaims {
		s, ok := styles[fixtureInt(c["style_id"])]
		if !ok || s["deleted_at"] != nil {
			continue
		}
		pid := fixtureInt(s["process_id"])
		if !procs[pid] {
			continue
		}
		for field, role := range fields {
			name := fixtureStr(c[field])
			if name == "" || positions[pid][name] {
				continue
			}
			out[pid][name+":"+role] = true
		}
	}
	return out
}

func setOf(rows []domain.RoutingNode) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		out[r.CoreNodeName+":"+r.Role] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runBackfillOnce runs migration v25's body (backfillRoutingSetsOnce) in its
// own transaction over whatever the test loaded — the same call the migration
// runner makes, after the fixture rows are in.
func runBackfillOnce(t *testing.T, db *DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := backfillRoutingSetsOnce(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration v25 threw on the plant's rows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestRoutingBackfill_PlantA is the brief's exact pin, now over migration v25:
// on plant A, process 1 derives {Supermarket Empty Totes: source, Supermarket
// Area: destination, PLN_02: staging, PLN_05: staging} from its 20 live claims
// — and then drops PLN_02 and PLN_05 because both are positions of the process
// — so the final set is the two groups. Four of A's 29 claims point at deleted
// style rows; they contribute nothing and break nothing.
func TestRoutingBackfill_PlantA(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := readPlantFixture(t, "a")
	dirty := loadPlantFixture(t, db, f)
	if dirty.orphanClaims != 4 {
		t.Fatalf("plant A: %d orphan claims loaded, want 4 — the test does not have its dirty input", dirty.orphanClaims)
	}

	runBackfillOnce(t, db)

	rows, err := db.ListRoutingNodes(1)
	if err != nil {
		t.Fatalf("ListRoutingNodes(1): %v", err)
	}
	want := []string{"Supermarket Empty Totes:source", "Supermarket Area:destination"}
	if got := routingKeys(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("P400 routing set = %v, want %v\n(PLN_02 and PLN_05 are staging on the claims AND positions of the process; they must be derived and then dropped)", got, want)
	}
	for _, r := range rows {
		// Enabled: the derive reads live claims, so every name it finds is one
		// the press already routes through (owner ruling 2026-09-16).
		if r.Origin != domain.RoutingOriginBackfill || !r.Enabled {
			t.Errorf("%s: origin=%q enabled=%v, want backfill and in the set", r.CoreNodeName, r.Origin, r.Enabled)
		}
	}

	// The rest of the plant, by the independent derivation.
	expected := expectedRoutingSets(f)
	for pid, exp := range expected {
		got, err := db.ListRoutingNodes(pid)
		testutil.MustNoErr(t, err, "db.ListRoutingNodes")
		if strings.Join(sortedKeys(setOf(got)), ",") != strings.Join(sortedKeys(exp), ",") {
			t.Errorf("process %d: derived %v, want %v", pid, sortedKeys(setOf(got)), sortedKeys(exp))
		}
	}

	// ONCE: a process with any routing row is never touched again — even after
	// a claim edit that would otherwise add a name. This is what replaced the
	// gate as the backfill's stop (the gate has one meaning again).
	if _, err := db.Exec(`UPDATE style_node_claims SET inbound_source = 'NEW_SRC' WHERE id = (
		SELECT c.id FROM style_node_claims c JOIN styles s ON s.id = c.style_id WHERE s.process_id = 1 AND s.deleted_at IS NULL LIMIT 1)`); err != nil {
		t.Fatalf("edit a live P400 claim: %v", err)
	}
	runBackfillOnce(t, db)
	after, err := db.ListRoutingNodes(1)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if got := routingKeys(after); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a second run changed P400's set, which already had rows: %v", got)
	}
}

// TestRoutingBackfill_PlantB: on plant B migration v25 yields, for every
// process, exactly the sources / staging / destinations its live claims use
// (minus positions), and throws on none of the dirty rows — 8 claims whose
// style is gone, 2 on soft-deleted styles, 1 on a style whose process row is
// gone. It also pins that the bare-name handling does not split SMN_BUF_100:
// the buffer is a live source at two of B's presses and lands intact.
func TestRoutingBackfill_PlantB(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := readPlantFixture(t, "b")
	dirty := loadPlantFixture(t, db, f)
	if dirty.orphanClaims != 8 || dirty.deletedStyleClaims != 2 || dirty.missingProcessClaim != 1 {
		t.Fatalf("plant B dirty rows = %+v, want 8 orphan / 2 deleted-style / 1 missing-process — the test does not have its dirty input", dirty)
	}

	runBackfillOnce(t, db)

	byName := map[string]int64{}
	for _, p := range f.Processes {
		byName[fixtureStr(p["name"])] = fixtureInt(p["id"])
	}
	expected := expectedRoutingSets(f)
	for pid, exp := range expected {
		got, err := db.ListRoutingNodes(pid)
		if err != nil {
			t.Fatalf("ListRoutingNodes(%d): %v", pid, err)
		}
		if strings.Join(sortedKeys(setOf(got)), ",") != strings.Join(sortedKeys(exp), ",") {
			t.Errorf("process %d: derived %v, want %v", pid, sortedKeys(setOf(got)), sortedKeys(exp))
		}
		for _, r := range got {
			if r.Origin != domain.RoutingOriginBackfill || !r.Enabled {
				t.Errorf("process %d %s: origin=%q enabled=%v, want backfill and in the set", pid, r.CoreNodeName, r.Origin, r.Enabled)
			}
		}
	}

	// SMN_BUF_100, intact, as a source at both presses.
	for _, press := range []string{"Press B13", "Press B14"} {
		pid, ok := byName[press]
		if !ok {
			t.Fatalf("no process %s in plant B", press)
		}
		rows, err := db.ListRoutingNodes(pid)
		testutil.MustNoErr(t, err, "db.ListRoutingNodes")
		if !setOf(rows)["SMN_BUF_100:source"] {
			t.Errorf("%s: SMN_BUF_100 is the live claims' inbound_source and is not in the set: %v", press, routingKeys(rows))
		}
		for _, r := range rows {
			if strings.HasPrefix(r.CoreNodeName, "SMN_BUF") && r.CoreNodeName != "SMN_BUF_100" {
				t.Errorf("%s: SMN_BUF_100 was split or altered: %q", press, r.CoreNodeName)
			}
		}
	}

	// Delete refusal names the style, on real rows: SMN_BUF_100 is Press 4's
	// source on style "testpayload".
	p4 := byName["Press 4"]
	rows, err := db.ListRoutingNodes(p4)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	for _, r := range rows {
		if r.CoreNodeName == "SMN_BUF_100" && r.Role == domain.RoutingRoleSource {
			err := db.DeleteRoutingNode(p4, r.ID)
			if !errors.Is(err, processes.ErrRoutingNodeInUse) || !strings.Contains(err.Error(), "testpayload") {
				t.Errorf("deleting Press 4's live source: err = %v, want ErrRoutingNodeInUse naming style testpayload", err)
			}
		}
	}
}
