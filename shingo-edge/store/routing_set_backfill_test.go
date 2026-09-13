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

// coreNameChecker builds the same test coreNodeNameIsUnknown applies: a name
// is known if Core lists it bare, or as a group child ("Group.CHILD") whose
// bare name matches. Absent on Core ⇒ needs a decision.
func coreNameChecker(f plantFixture) func(string) bool {
	known := map[string]bool{}
	for _, n := range f.Nodes {
		known[n.Name] = true
		if n.ParentName != nil && *n.ParentName != "" {
			known[*n.ParentName+"."+n.Name] = true
		}
	}
	return func(name string) bool {
		if known[name] {
			return false
		}
		for full := range known {
			if i := strings.LastIndex(full, "."); i >= 0 && full[i+1:] == name {
				return false
			}
		}
		return true
	}
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

// TestRoutingBackfill_PlantA is the brief's exact pin: on plant A, process 1
// derives {Supermarket Empty Totes: source, Supermarket Area: destination,
// PLN_02: staging, PLN_05: staging} from its 20 live claims — and then drops
// PLN_02 and PLN_05 because both are positions of the process — so the final
// set is the two groups. Four of A's 29 claims point at deleted style rows;
// they contribute nothing and break nothing.
func TestRoutingBackfill_PlantA(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := readPlantFixture(t, "a")
	dirty := loadPlantFixture(t, db, f)
	if dirty.orphanClaims != 4 {
		t.Fatalf("plant A: %d orphan claims loaded, want 4 — the test does not have its dirty input", dirty.orphanClaims)
	}

	reports, err := db.DeriveRoutingNodes(coreNameChecker(f))
	if err != nil {
		t.Fatalf("DeriveRoutingNodes threw on plant A's rows: %v", err)
	}
	byName := map[string]domain.RoutingDeriveReport{}
	for _, r := range reports {
		t.Log(r.Line())
		byName[r.ProcessName] = r
	}

	p400 := byName["Press A1"]
	if p400.ProcessID != 1 || p400.Claims != 20 {
		t.Fatalf("Press A1 report = %+v, want process 1 with 20 live claims", p400)
	}
	rows, err := db.ListRoutingNodes(1)
	if err != nil {
		t.Fatalf("ListRoutingNodes(1): %v", err)
	}
	want := []string{"Supermarket Empty Totes:source", "Supermarket Area:destination"}
	if got := routingKeys(rows); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("P400 routing set = %v, want %v\n(PLN_02 and PLN_05 are staging on the claims AND positions of the process; they must be derived and then dropped)", got, want)
	}
	for _, r := range rows {
		if r.Origin != domain.RoutingOriginBackfill || r.Enabled {
			t.Errorf("%s: origin=%q enabled=%v, want backfill and disabled", r.CoreNodeName, r.Origin, r.Enabled)
		}
	}
	if p400.Nodes != 2 || p400.NeedDecision != 0 {
		t.Fatalf("Press A1 report = %+v, want 2 nodes and 0 needing a decision (every name resolves in A's node table)", p400)
	}
	// THE NUMBERS, NOT THE SENTENCE. This asserted the line verbatim, so the
	// next wording change to a log message failed a test about a backfill. All
	// three numbers are already asserted above; what is left worth holding is
	// that the line the operator reads carries them.
	for _, want := range []string{"Press A1", "2 nodes", "20 claims", "0 need a decision"} {
		if !strings.Contains(p400.Line(), want) {
			t.Errorf("Press A1 line %q does not carry %q", p400.Line(), want)
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

	// Once the gate is on, a re-derive is a no-op — even after a claim edit
	// that would otherwise add a name.
	if err := db.SetFlowComposerEnabled(1, true); err != nil {
		t.Fatalf("enable gate: %v", err)
	}
	if _, err := db.Exec(`UPDATE style_node_claims SET inbound_source = 'NEW_SRC' WHERE id = (
		SELECT c.id FROM style_node_claims c JOIN styles s ON s.id = c.style_id WHERE s.process_id = 1 AND s.deleted_at IS NULL LIMIT 1)`); err != nil {
		t.Fatalf("edit a live P400 claim: %v", err)
	}
	rep, err := db.DeriveRoutingNodesForProcess(1, nil)
	if err != nil {
		t.Fatalf("re-derive with gate on: %v", err)
	}
	if !rep.FlowComposerEnabled {
		t.Fatalf("re-derive ran with the gate on: %+v", rep)
	}
	after, err := db.ListRoutingNodes(1)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	if got := routingKeys(after); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("gate on, yet P400's set changed: %v", got)
	}
}

// TestRoutingBackfill_PlantB: on plant B the backfill yields, for every
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

	reports, err := db.DeriveRoutingNodes(coreNameChecker(f))
	if err != nil {
		t.Fatalf("DeriveRoutingNodes threw on plant B's rows: %v", err)
	}
	if len(reports) != len(f.Processes) {
		t.Fatalf("%d reports for %d processes", len(reports), len(f.Processes))
	}
	byName := map[string]domain.RoutingDeriveReport{}
	for _, r := range reports {
		t.Log(r.Line())
		byName[r.ProcessName] = r
		if r.NeedDecision != 0 {
			t.Errorf("%s: %d names need a decision (%v) — every claim name resolves in B's node table", r.ProcessName, r.NeedDecision, r.Unknown)
		}
	}

	expected := expectedRoutingSets(f)
	totalClaims := 0
	for pid, exp := range expected {
		got, err := db.ListRoutingNodes(pid)
		if err != nil {
			t.Fatalf("ListRoutingNodes(%d): %v", pid, err)
		}
		if strings.Join(sortedKeys(setOf(got)), ",") != strings.Join(sortedKeys(exp), ",") {
			t.Errorf("process %d: derived %v, want %v", pid, sortedKeys(setOf(got)), sortedKeys(exp))
		}
		for _, r := range got {
			if r.Origin != domain.RoutingOriginBackfill || r.Enabled {
				t.Errorf("process %d %s: origin=%q enabled=%v, want backfill and disabled", pid, r.CoreNodeName, r.Origin, r.Enabled)
			}
		}
	}
	for _, r := range reports {
		totalClaims += r.Claims
	}
	if totalClaims != len(f.StyleNodeClaims)-dirty.orphanClaims-dirty.deletedStyleClaims-dirty.missingProcessClaim {
		t.Errorf("reports read %d claims in total, want the %d live ones (41 minus the dirty rows)", totalClaims,
			len(f.StyleNodeClaims)-dirty.orphanClaims-dirty.deletedStyleClaims-dirty.missingProcessClaim)
	}

	// SMN_BUF_100, intact, as a source at both presses.
	for _, press := range []string{"Press B13", "Press B14"} {
		rep, ok := byName[press]
		if !ok {
			t.Fatalf("no report for %s", press)
		}
		rows, err := db.ListRoutingNodes(rep.ProcessID)
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
	rows, err := db.ListRoutingNodes(p4.ProcessID)
	testutil.MustNoErr(t, err, "db.ListRoutingNodes")
	for _, r := range rows {
		if r.CoreNodeName == "SMN_BUF_100" && r.Role == domain.RoutingRoleSource {
			err := db.DeleteRoutingNode(p4.ProcessID, r.ID)
			if !errors.Is(err, processes.ErrRoutingNodeInUse) || !strings.Contains(err.Error(), "testpayload") {
				t.Errorf("deleting Press 4's live source: err = %v, want ErrRoutingNodeInUse naming style testpayload", err)
			}
		}
	}
}
