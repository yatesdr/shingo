//go:build docker

package binresolver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/sourceability"
)

// eligibility_golden_docker_test.go — the SQL half of the eligibility
// photograph.
//
// THIS FILE IS THE HALF THE TREE CLAIMED TO HAVE AND DID NOT. The pure golden
// test next door said the SQL-backed predicates were "covered by the docker
// half (see eligibility_golden_docker_test.go)". No such file existed. The
// harness a collapse was meant to lean on was one third built, and a comment
// was the only thing saying otherwise.
//
// Same job as the pure half, other language: one fixture set, every predicate's
// verdict recorded side by side, regenerable with -update. It records what each
// one answers. It does NOT assert they agree, because they do not — this file
// exists to make the disagreement legible before anything tries to remove it.
//
// HOW A VERDICT IS TAKEN. Every column drives the REAL production function and
// asks whether it selected (or counted) the fixture's bin. Nothing here restates
// a WHERE clause: a harness that re-spelled the predicates would be one more
// spelling measuring the others. Where a predicate is an exported SQL fragment
// rather than a function (EmptyCarrierWhere), the fragment itself is
// interpolated, which is how sourceable_status_docker_test.go pins its pair.
//
// EVERY FIXTURE GETS ITS OWN PAYLOAD CODE AND ITS OWN NODES. Several of these
// predicates are plant-wide counts, so fixtures sharing a payload would count
// each other and no row would mean anything on its own. The bin attributes are
// the shared eligFixtures verbatim; only the payload STRING is per-fixture, and
// the wrong-part and empty arms keep their relationship to what is asked for.
//
// "" means the predicate took the bin. Any other string is a rejection.
//
// Run with -update to regenerate:
//
//	go test -tags docker -run TestGolden_EligibilitySQL -update ./dispatch/binresolver/

// eligNodeAxis carries the axes that live on the NODE (or on the payload's
// bin-type rules) rather than on the bin. They are separate from eligFixture
// because the pure predicates cannot see them: adding them there would grow the
// pure golden with rows whose every column duplicates the baseline.
type eligNodeAxis struct {
	Disabled     bool // nodes.enabled = false
	Synthetic    bool // nodes.is_synthetic = true
	BinTypeRuleX bool // payload_bin_types names a type this bin is not
}

// eligSQLCase is one fixture as the SQL half runs it: the shared bin attributes
// plus the node axes.
type eligSQLCase struct {
	Name string
	Fx   eligFixture
	Node eligNodeAxis
}

// eligSQLCases is eligFixtures verbatim, plus the three axes only a database can
// express. The extras reuse the baseline bin so their column differences are
// attributable to the node (or the bin-type rule) and nothing else.
func eligSQLCases() []eligSQLCase {
	cases := make([]eligSQLCase, 0, len(eligFixtures)+3)
	for _, f := range eligFixtures {
		cases = append(cases, eligSQLCase{Name: f.Name, Fx: f})
	}
	var base eligFixture
	for _, f := range eligFixtures {
		if f.Name == "full_of_X_confirmed" {
			base = f
		}
	}
	if base.Name == "" {
		panic("eligSQLCases: baseline fixture full_of_X_confirmed is gone from eligFixtures")
	}
	cases = append(cases,
		eligSQLCase{Name: "node_disabled", Fx: base, Node: eligNodeAxis{Disabled: true}},
		eligSQLCase{Name: "node_synthetic", Fx: base, Node: eligNodeAxis{Synthetic: true}},
		eligSQLCase{Name: "bin_type_rule_excludes", Fx: base, Node: eligNodeAxis{BinTypeRuleX: true}},
	)
	return cases
}

// eligSQLRow is one fixture's verdict from each SQL-backed predicate.
type eligSQLRow struct {
	Fixture string `json:"fixture"`

	// bins.FindSourceFIFO — the store-wide source picker, and the predicate the
	// other readers' comments all claim to be copying.
	FindSourceFIFO string `json:"find_source_fifo"`

	// store.FindSourceBinInLane — the lane-scoped accessible picker.
	LaneAccessible string `json:"lane_accessible"`

	// store.FindBuriedBin / store.FindOldestBuriedBin — the two buried readers.
	LaneBuried       string `json:"lane_buried"`
	LaneBuriedOldest string `json:"lane_buried_oldest"`

	// sourceability availablePoolByPayload, reached through BuildInputs.
	SourceabilityPool string `json:"sourceability_pool"`

	// sourceability.PoolBreakdownByPayload — the drill-in's free count.
	PoolBreakdownFree string `json:"pool_breakdown_free"`

	// service.InventoryService.PreflightAvailability — the changeover preflight.
	InventoryPreflight string `json:"inventory_preflight"`

	// bins.EmptyCarrierWhere — the empty-carrier predicate every FindEmpty*
	// composes from.
	EmptyCarrier string `json:"empty_carrier"`
}

const eligRejected = "rejected"

func eligVerdict(taken bool) string {
	if taken {
		return ""
	}
	return eligRejected
}

func TestGolden_EligibilitySQL(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	ctx := context.Background()

	// reservations.order_id carries an FK, so the reserved arm needs a real
	// order. Nothing reads it; the row only has to exist.
	var orderID int64
	if err := db.DB.QueryRow(
		`INSERT INTO orders (edge_uuid) VALUES ('elig-golden-sql') RETURNING id`,
	).Scan(&orderID); err != nil {
		t.Fatalf("seed order for reservations: %v", err)
	}

	binType, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		t.Fatalf("get DEFAULT bin type: %v", err)
	}
	// A type no fixture bin is, so the bin-type-rule arm can name it.
	otherType := &bins.BinType{Code: "ELIG-OTHER", Description: "bin-type-rule arm"}
	if err := db.CreateBinType(otherType); err != nil {
		t.Fatalf("create ELIG-OTHER bin type: %v", err)
	}

	inv := service.NewInventoryService(db)
	cases := eligSQLCases()
	rows := make([]eligSQLRow, 0, len(cases))

	for i, c := range cases {
		f := eligSQLBuild(t, db, binType.ID, otherType.ID, orderID, i, c)
		rows = append(rows, eligSQLVerdicts(ctx, t, db, inv, f))
	}

	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	got = append(got, '\n')

	const goldenPath = "testdata/golden/eligibility_sql.json"
	if *updateFlag {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d fixtures)", goldenPath, len(rows))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden file %s not found (run with -update to create): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("SQL eligibility matrix changed.\n--- want (golden) ---\n%s\n--- got ---\n%s\n"+
			"If this change is intended, re-run with -update and justify each differing row. "+
			"A collapse that changes no behaviour produces NO diff here.",
			want, got)
	}
}

// eligSQLFixtureRows is what one case put in the database: the bins, and the
// payload each predicate should be asked for.
type eligSQLFixtureRows struct {
	Name string

	FlatBinID int64
	FlatAsk   string

	LaneID    int64
	LaneBinID int64
	LaneAsk   string

	BuriedLane int64
	BuriedAsk  string
}

// eligSQLBuild lays down one case's topology.
//
// THREE BINS, NOT ONE, because the lane readers need lane positions the flat
// readers cannot see, and the two buried readers need a position the accessible
// one excludes by definition: a bin cannot be both at the mouth and behind
// something. The attributes are identical on all three, so a column difference
// is the predicate's and not the fixture's.
func eligSQLBuild(t *testing.T, db *store.DB, binTypeID, otherTypeID, orderID int64,
	i int, c eligSQLCase) eligSQLFixtureRows {
	t.Helper()

	flatAsk := fmt.Sprintf("EP-%02d", i)
	laneAsk := fmt.Sprintf("EL-%02d", i)
	buriedAsk := fmt.Sprintf("EB-%02d", i)

	out := eligSQLFixtureRows{Name: c.Name, FlatAsk: flatAsk, LaneAsk: laneAsk, BuriedAsk: buriedAsk}

	// The payload row carries the capacity the fixture declares; Cap 0 is the
	// "capacity unknown" arm and is seeded as 0, not skipped.
	for _, code := range []string{flatAsk, laneAsk, buriedAsk} {
		p := &payloads.Payload{Code: code, UOPCapacity: c.Fx.Cap}
		if err := payloads.Create(db.DB, p); err != nil {
			t.Fatalf("%s: create payload %s: %v", c.Name, code, err)
		}
		if c.Node.BinTypeRuleX {
			// A rule naming a type this bin is not. The advisory clause is
			// FindSourceFIFO's alone, so this arm is what splits it from every
			// reader that does not compose the clause.
			if err := db.SetPayloadBinTypes(p.ID, []int64{otherTypeID}); err != nil {
				t.Fatalf("%s: set payload bin types: %v", c.Name, err)
			}
		}
	}

	// binPayload keeps the fixture's relationship to what is asked: the same
	// code when the fixture carries the wanted part, a different one for the
	// wrong-part arm, empty for the carrier arm.
	binPayload := func(ask string) string {
		switch c.Fx.Payload {
		case wantPayload:
			return ask
		case "":
			return ""
		default:
			return ask + "-X"
		}
	}

	flatNode := &nodes.Node{
		Name:        fmt.Sprintf("ELIG-F-%02d", i),
		Enabled:     !c.Node.Disabled,
		IsSynthetic: c.Node.Synthetic,
		Zone:        "A",
	}
	if err := nodes.Create(db.DB, flatNode); err != nil {
		t.Fatalf("%s: create flat node: %v", c.Name, err)
	}
	out.FlatBinID = eligSQLBin(t, db, binTypeID, orderID, flatNode.ID,
		fmt.Sprintf("ELIG-F-%02d", i), binPayload(flatAsk), c)

	// Lane A: the fixture bin alone at the mouth, so the accessible reader can
	// reach it.
	laneA := &nodes.Node{Name: fmt.Sprintf("ELIG-LA-%02d", i), Enabled: true}
	if err := nodes.Create(db.DB, laneA); err != nil {
		t.Fatalf("%s: create lane A: %v", c.Name, err)
	}
	d1, d2 := 1, 2
	// The node axes apply to the SLOT the fixture bin stands on, not just to
	// the flat node: the lane readers join the bin's own node, so an axis left
	// off the slot would record the baseline in the lane columns and read as
	// agreement that was never measured.
	slotA := &nodes.Node{Name: fmt.Sprintf("ELIG-LA-%02d-S1", i), Enabled: !c.Node.Disabled,
		IsSynthetic: c.Node.Synthetic, ParentID: &laneA.ID, Depth: &d1}
	if err := nodes.Create(db.DB, slotA); err != nil {
		t.Fatalf("%s: create lane A slot: %v", c.Name, err)
	}
	out.LaneID = laneA.ID
	out.LaneBinID = eligSQLBin(t, db, binTypeID, orderID, slotA.ID,
		fmt.Sprintf("ELIG-LA-%02d", i), binPayload(laneAsk), c)

	// Lane B: a blocker at the mouth and the fixture bin behind it, which is
	// what makes the two buried readers answer about it at all. The blocker is
	// a bare carrier — the blocker predicate counts any bin in a shallower slot
	// and reads nothing else off it.
	laneB := &nodes.Node{Name: fmt.Sprintf("ELIG-LB-%02d", i), Enabled: true}
	if err := nodes.Create(db.DB, laneB); err != nil {
		t.Fatalf("%s: create lane B: %v", c.Name, err)
	}
	slotB1 := &nodes.Node{Name: fmt.Sprintf("ELIG-LB-%02d-S1", i), Enabled: true, ParentID: &laneB.ID, Depth: &d1}
	slotB2 := &nodes.Node{Name: fmt.Sprintf("ELIG-LB-%02d-S2", i), Enabled: !c.Node.Disabled,
		IsSynthetic: c.Node.Synthetic, ParentID: &laneB.ID, Depth: &d2}
	if err := nodes.Create(db.DB, slotB1); err != nil {
		t.Fatalf("%s: create lane B mouth: %v", c.Name, err)
	}
	if err := nodes.Create(db.DB, slotB2); err != nil {
		t.Fatalf("%s: create lane B deep slot: %v", c.Name, err)
	}
	if _, err := db.DB.Exec(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available')`,
		binTypeID, fmt.Sprintf("ELIG-LB-%02d-BLOCKER", i), slotB1.ID); err != nil {
		t.Fatalf("%s: create lane B blocker: %v", c.Name, err)
	}
	out.BuriedLane = laneB.ID
	eligSQLBin(t, db, binTypeID, orderID, slotB2.ID,
		fmt.Sprintf("ELIG-LB-%02d", i), binPayload(buriedAsk), c)

	return out
}

// eligSQLBin writes one fixture bin and returns its id. Direct INSERT rather
// than the aggregate so created_at and loaded_at are pinned: FIFO order is a
// timestamp comparison, and a fixture that drifted with the wall clock would
// make the golden non-reproducible.
func eligSQLBin(t *testing.T, db *store.DB, binTypeID, orderID, nodeID int64,
	label, payload string, c eligSQLCase) int64 {
	t.Helper()

	created := time.Unix(1_700_000_000, 0).UTC()
	var loaded any
	if !c.Fx.NoLoadedA {
		loaded = time.Unix(1_700_000_500, 0).UTC()
	}
	var claimed any
	if c.Fx.Claimed {
		claimed = orderID
	}

	var id int64
	if err := db.DB.QueryRow(`
		INSERT INTO bins (bin_type_id, label, node_id, status, payload_code,
		                  uop_remaining, manifest_confirmed, locked, claimed_by,
		                  loaded_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		binTypeID, label, nodeID, string(c.Fx.Status), payload,
		c.Fx.UOP, c.Fx.Confirmed, c.Fx.Locked, claimed,
		loaded, created).Scan(&id); err != nil {
		t.Fatalf("%s: create bin %s: %v", c.Name, label, err)
	}

	if c.Fx.Reserved {
		if _, err := db.DB.Exec(
			`INSERT INTO reservations (order_id, bin_id, state, resource_kind)
			 VALUES ($1,$2,'pending','bin')`, orderID, id); err != nil {
			t.Fatalf("%s: reserve bin %s: %v", c.Name, label, err)
		}
	}
	return id
}

// eligSQLVerdicts asks every SQL-backed predicate about one case's bins.
func eligSQLVerdicts(ctx context.Context, t *testing.T, db *store.DB,
	inv *service.InventoryService, f eligSQLFixtureRows) eligSQLRow {
	t.Helper()

	row := eligSQLRow{Fixture: f.Name}

	// FindSourceFIFO is plant-wide, so the ask payload alone scopes it to this
	// fixture's bin.
	got, err := bins.FindSourceFIFO(db.DB, f.FlatAsk, 0)
	row.FindSourceFIFO = eligVerdict(err == nil && got != nil && got.ID == f.FlatBinID)

	laneBin, err := db.FindSourceBinInLane(f.LaneID, f.LaneAsk)
	row.LaneAccessible = eligVerdict(err == nil && laneBin != nil && laneBin.ID == f.LaneBinID)

	buried, _, err := db.FindBuriedBin(f.BuriedLane, f.BuriedAsk)
	row.LaneBuried = eligVerdict(err == nil && buried != nil)

	oldest, _, err := db.FindOldestBuriedBin(f.BuriedLane, f.BuriedAsk)
	row.LaneBuriedOldest = eligVerdict(err == nil && oldest != nil)

	in, err := sourceability.BuildInputs(db.DB, time.Hour)
	if err != nil {
		t.Fatalf("%s: BuildInputs: %v", f.Name, err)
	}
	row.SourceabilityPool = eligVerdict(in.Pool[f.FlatAsk] > 0)

	breakdown, err := sourceability.PoolBreakdownByPayload(db.DB)
	if err != nil {
		t.Fatalf("%s: PoolBreakdownByPayload: %v", f.Name, err)
	}
	row.PoolBreakdownFree = eligVerdict(breakdown[f.FlatAsk].Free > 0)

	pre, err := inv.PreflightAvailability(ctx, "", []string{f.FlatAsk})
	if err != nil {
		t.Fatalf("%s: PreflightAvailability: %v", f.Name, err)
	}
	preTaken := false
	for _, a := range pre.Available {
		if a.PayloadCode == f.FlatAsk && a.BinCount > 0 {
			preTaken = true
		}
	}
	row.InventoryPreflight = eligVerdict(preTaken)

	// The empty-carrier predicate is an exported fragment, not a function, so
	// the fragment itself is interpolated — the same way the status pair is
	// pinned in store/bins. Anything else would be a restatement.
	var empty bool
	q := `SELECT EXISTS (SELECT 1 ` + bins.BinFromClause + bins.EmptyCarrierWhere + ` AND b.id = $1)`
	if err := db.DB.QueryRow(q, f.FlatBinID).Scan(&empty); err != nil {
		t.Fatalf("%s: evaluate EmptyCarrierWhere: %v", f.Name, err)
	}
	row.EmptyCarrier = eligVerdict(empty)

	return row
}
