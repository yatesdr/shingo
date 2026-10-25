//go:build docker

package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
	"shingocore/store/sourceability"
)

// availability_agreement_docker_test.go — ground rule 6 as a test: every
// availability reader composes the one fragment (helpers.BinSourceableSQL, its
// owner-aware form BinSourceableSQLFor, or the stock half
// BinCarriesSourceableStockSQL) or its Go twin, and they agree. One fixture set,
// every reader, one table. A reader that omits an arm names it in availOmits.
//
// It is not the eligibility photograph (binresolver's golden pair), which
// records what each predicate answers and asserts nothing. This asserts: the
// expected answer of every cell is the one rule — a carrier is sourceable as
// stock of its part only when it is the plain good full — and a cell that
// disagrees is either PREDICTED (availPredicted, with the phase that closes it)
// or a failure. A predicted cell that has come to agree fails too, so the
// prediction list is the exact list of today's disagreements, never a ceiling.
// The predicted cells go to testdb.VerifyRed: green while they stand, red the
// moment the last is gone, which is when the VerifyRed call goes and the table
// is all assertions.
//
// EVERY READER IS ASKED THROUGH ITS PRODUCTION DOOR. The SQL readers are called
// as they are; the Go twins are reached through the caller that runs them
// (isBinAvailableForRetrieve through the group scan, tier 4's named full and
// tier 2's loader pool through Finder.Pick, the allocator's two arms through
// findAvailableForNeed), so a cell answers for the reader as the plant runs it,
// arms its caller asks included. Nothing here re-spells a predicate.
//
// EACH FIXTURE IS ONE ARM AWAY FROM THE GOOD FULL, with its own part and its
// own nodes, so a plant-wide reader's answer for one part is about one fixture.
// Every part declares one carrier type (payload_bin_types), so the carrier rule
// is live for all of them: the foreign carrier is another type, and the bare
// carrier is the bare marker of the declared type — never in a rule, which is
// how the SQL refuses it. Every fixture stands in five places, identical but
// for the node: a plain node (the plant-wide readers, tier 4, the allocator),
// the mouth of a lane, behind a blocker in a second lane (the digs), the one
// slot child of a node group (the group scan), and a dedicated loader's pinned
// home (tier 2). The disabled fixture's every position is switched off.
//
// The good full is the table's own guard: a reader that errors or reads
// nothing refuses the good full and fails, so no refusal below is vacuous.

// availFx is one carrier the table asks every reader about.
type availFx struct {
	name      string
	held      bool // bins.quality_hold, set through the store's hold door
	foreign   bool // a type its part's carrier rule does not name
	bare      bool // the bare marker of the declared type
	partLess  bool // payload_code '' — an empty carrier, asked for the part
	uop       int  // uop_remaining
	confirmed bool // manifest_confirmed
	disabled  bool // every node it stands on is switched off
}

const availGood = "good_full"

var availFixtures = []availFx{
	{name: availGood, uop: 10, confirmed: true},
	{name: "held", uop: 10, confirmed: true, held: true},
	{name: "foreign_carrier", uop: 10, confirmed: true, foreign: true},
	{name: "counted_out", uop: 0, confirmed: true},
	{name: "part_less", partLess: true},
	{name: "unconfirmed", uop: 10},
	{name: "disabled_node", uop: 10, confirmed: true, disabled: true},
	{name: "bare", uop: 10, confirmed: true, bare: true},
}

// availCopies is one fixture as the database holds it: the part each reader is
// asked for, and the bin it stands as in each of the five places.
type availCopies struct {
	fx      availFx
	payload string

	flat    *nodes.Node
	flatBin int64

	lane    int64
	laneBin int64

	buriedLane int64
	buriedBin  int64

	group    *nodes.Node
	groupBin int64

	home    *nodes.Node
	homeBin int64
}

// availTypes are the three carrier types the fixtures stand in.
type availTypes struct{ cart, foreign, marker int64 }

// availReader is one row of the table: a reader, where it lives, and whether it
// takes a fixture's carrier as stock of the fixture's part.
type availReader struct {
	name  string
	where string
	took  func(c availCopies) bool
}

// availOmits are the arms a reader does not ask, BY DESIGN: the cell's expected
// answer is "takes it", and the reason is the design. Keyed reader, then fixture.
var availOmits = map[string]map[string]string{
	// THE CONCRETE-NODE PICKUP FOR A REMOVAL: what stands where somebody named
	// (DECISIONS U.1, "left alone, by design"). A removal lifts whatever is
	// resident at the order's own line position, so the part-and-stock arms are
	// not its question (SPR ALN_006, isRemovalPickup).
	"allocator_removal": {
		"held":            "a removal clears the line whatever its hold; the hold keeps a carrier out of sourcing, not on a line it must leave",
		"foreign_carrier": "a removal names no part (claimPayload \"\"), so no carrier rule applies to what it lifts",
		"counted_out":     "U.1: the evac must lift a cart counted at nothing",
		"part_less":       "U.1: a removal lifts an empty as readily as a full",
		"unconfirmed":     "U.1: what stands at the line is lifted whether or not its manifest was confirmed",
		"bare":            "a removal lifts the marker cart a stage-1 CLEAR left at the line",
	},
}

// availPredicted are today's disagreements with the rule, each with the phase of
// the press-tranche-2 brief (rev 3) that closes it; "none" is a disagreement no
// phase names. A cell here that has come to agree fails the test: delete it.
var availPredicted = map[string]map[string]string{
	"lane_mouth": {
		"held": "F — the quality hold is not in BinSourceableSQLFor (helpers/bin_sourceable.go)",
	},
	"dig_oldest_buried": {
		"held":            "F — the hold, if it goes into the stock half the digs compose (BinCarriesSourceableStockSQL); else name it in availOmits",
		"foreign_carrier": "none — the digs compose no carrier rule (2026-09-14: they take the disabled-node rule only), yet U.1's reason holds: a dig must not expose a carrier nobody can take",
		"bare":            "none — the same carrier-rule gap as the foreign carrier",
	},
	"dig_shallowest_buried": {
		"held":            "F — as dig_oldest_buried",
		"foreign_carrier": "none — as dig_oldest_buried",
		"bare":            "none — as dig_oldest_buried",
	},
	"sourceability_pool": {
		"held": "F — the quality hold is not in BinSourceableSQL",
	},
	"pool_breakdown_free": {
		"held": "F — the quality hold is not in the stock half or the holds half it composes",
	},
	"changeover_preflight": {
		"held": "F — the quality hold is not in BinSourceableSQL",
	},
	"group_slot_child": {
		"held":            "F — isBinAvailableForRetrieve asks no quality hold",
		"foreign_carrier": "F — isBinAvailableForRetrieve composes no carrier rule",
		"bare":            "F — the same carrier-rule gap",
	},
	"tier4_named_full": {
		"unconfirmed":   "none — BinUnavailableReasonFor asks no manifest_confirmed, and namedFull adds none (finder.go namedFull loop)",
		"disabled_node": "F — the concrete-node arm asks no Enabled",
	},
	"tier2_loader_pool": {
		"disabled_node": "none — sourceFromDedicatedLoader reads the pool's bins with no Enabled on the positions",
	},
	"allocator_part_supply": {
		"held":          "F — BinUnavailableReason asks no quality hold",
		"disabled_node": "F — the concrete-node arm asks no Enabled",
	},
	"allocator_removal": {
		"disabled_node": "F — the concrete-node arm asks no Enabled; a removal is automation, and a disabled node is dead to it (helpers.NodeEnabledSQL)",
	},
}

// TestRule6_OneAvailabilityTable is ground rule 6: one fixture set through every
// reader that answers "may this carrier be sourced as stock of its part".
func TestRule6_OneAvailabilityTable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	types := availCarrierTypes(t, db)
	copies := make([]availCopies, 0, len(availFixtures))
	for i, fx := range availFixtures {
		copies = append(copies, availBuild(t, db, types, i, fx))
	}

	readers := availReaders(t, db, copies)
	checkAvailTableNames(t, readers)

	var violations []string
	for _, r := range readers {
		for _, c := range copies {
			took := r.took(c)
			want := c.fx.name == availGood
			if _, omitted := availOmits[r.name][c.fx.name]; omitted {
				want = true
			}
			why, predicted := availPredicted[r.name][c.fx.name]
			switch {
			case took == want && predicted:
				t.Errorf("rule 6: %s (%s) now %s the %s carrier, as the rule says. The prediction "+
					"%q is stale: delete it from availPredicted so the cell is an assertion.",
					r.name, r.where, takesWord(took), c.fx.name, why)
			case took != want && predicted:
				violations = append(violations, fmt.Sprintf("%s (%s) %s the %s carrier: %s",
					r.name, r.where, takesWord(took), c.fx.name, why))
			case took != want:
				t.Errorf("rule 6: %s (%s) %s the %s carrier; the one rule says it %s.\n\n"+
					"Every availability reader composes the one fragment (helpers.BinSourceableSQL) or "+
					"its Go twin. A reader that omits an arm by design names it in availOmits with the "+
					"reason; a reader that drifted is fixed.",
					r.name, r.where, takesWord(took), c.fx.name, takesWord(want))
			}
		}
	}
	testdb.VerifyRed(t, "rule 6: every availability reader composes the one fragment or its Go twin", violations)
}

func takesWord(took bool) string {
	if took {
		return "takes"
	}
	return "refuses"
}

// checkAvailTableNames fails on a name in availOmits or availPredicted that is
// no reader or no fixture, and on a cell in both: a stale name reads as
// coverage, and a cell cannot be both by design and a drift.
func checkAvailTableNames(t *testing.T, readers []availReader) {
	t.Helper()
	known := map[string]bool{}
	for _, r := range readers {
		known[r.name] = true
	}
	fixture := map[string]bool{}
	for _, fx := range availFixtures {
		fixture[fx.name] = true
	}
	for label, table := range map[string]map[string]map[string]string{"availOmits": availOmits, "availPredicted": availPredicted} {
		for reader, cells := range table {
			if !known[reader] {
				t.Errorf("%s names reader %q, which the table does not run", label, reader)
			}
			for fx := range cells {
				if !fixture[fx] || fx == availGood {
					t.Errorf("%s names %s × %q, which is no fixture the rule refuses", label, reader, fx)
				}
			}
		}
	}
	for reader, cells := range availOmits {
		for fx := range cells {
			if _, both := availPredicted[reader][fx]; both {
				t.Errorf("%s × %s is in availOmits and availPredicted: it is one or the other", reader, fx)
			}
		}
	}
}

// availCarrierTypes makes the declared carrier, a foreign one and the declared
// carrier's bare marker.
func availCarrierTypes(t *testing.T, db *store.DB) availTypes {
	t.Helper()
	cart := &bins.BinType{Code: "R6-CART"}
	if err := db.CreateBinType(cart); err != nil {
		t.Fatalf("create R6-CART: %v", err)
	}
	foreign := &bins.BinType{Code: "R6-FOREIGN"}
	if err := db.CreateBinType(foreign); err != nil {
		t.Fatalf("create R6-FOREIGN: %v", err)
	}
	marker, err := db.EnsureBareMarker(cart.ID)
	if err != nil {
		t.Fatalf("bare marker of R6-CART: %v", err)
	}
	return availTypes{cart: cart.ID, foreign: foreign.ID, marker: marker}
}

// availBuild lays one fixture down in its five places.
func availBuild(t *testing.T, db *store.DB, types availTypes, i int, fx availFx) availCopies {
	t.Helper()
	c := availCopies{fx: fx, payload: fmt.Sprintf("R6-P%02d", i)}
	p := &payloads.Payload{Code: c.payload, UOPCapacity: 10}
	if err := db.CreatePayload(p); err != nil {
		t.Fatalf("%s: create payload: %v", fx.name, err)
	}
	if err := db.SetPayloadBinTypes(p.ID, []int64{types.cart}); err != nil {
		t.Fatalf("%s: carrier rule: %v", fx.name, err)
	}
	typeID := types.cart
	switch {
	case fx.foreign:
		typeID = types.foreign
	case fx.bare:
		typeID = types.marker
	}
	node := func(suffix string, parent *int64, depth int, live bool) *nodes.Node {
		t.Helper()
		n := &nodes.Node{Name: fmt.Sprintf("R6-%02d-%s", i, suffix), Enabled: live, ParentID: parent}
		if depth > 0 {
			n.Depth = &depth
		}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("%s: create node %s: %v", fx.name, n.Name, err)
		}
		return n
	}
	put := func(at *nodes.Node) int64 {
		t.Helper()
		return availBin(t, db, fx, typeID, c.payload, at)
	}
	live := !fx.disabled

	c.flat = node("FLAT", nil, 0, live)
	c.flatBin = put(c.flat)

	lane := node("LANE", nil, 0, true)
	c.lane = lane.ID
	c.laneBin = put(node("LANE-S1", &lane.ID, 1, live))

	// Behind a bin at the mouth: what the two dig readers answer about. The
	// blocker is a part-less cart of the declared type; the blocker scan counts
	// any bin in a shallower slot and reads nothing else off it.
	buried := node("BURIED", nil, 0, true)
	c.buriedLane = buried.ID
	mouth := node("BURIED-S1", &buried.ID, 1, true)
	if _, err := db.DB.Exec(`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available')`,
		types.cart, fmt.Sprintf("R6-%02d-BLOCKER", i), mouth.ID); err != nil {
		t.Fatalf("%s: blocker: %v", fx.name, err)
	}
	c.buriedBin = put(node("BURIED-S2", &buried.ID, 2, live))

	groupID, err := db.CreateNodeGroup(fmt.Sprintf("R6-%02d-GRP", i))
	if err != nil {
		t.Fatalf("%s: create group: %v", fx.name, err)
	}
	if c.group, err = db.GetNode(groupID); err != nil {
		t.Fatalf("%s: reload group: %v", fx.name, err)
	}
	c.groupBin = put(node("GRP-C1", &groupID, 0, live))

	c.home = node("HOME", nil, 0, live)
	loaderID, err := db.CreateLoader(store.Loader{Name: fmt.Sprintf("R6-%02d-LDR", i),
		Role: loaders.RoleConsume, Layout: loaders.LayoutDedicatedPositions, Replenishment: "operator"})
	if err != nil {
		t.Fatalf("%s: create loader: %v", fx.name, err)
	}
	if err := db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: c.home.ID,
		PayloadCode: c.payload, Kind: loaders.HomeKindHome}); err != nil {
		t.Fatalf("%s: pin the home: %v", fx.name, err)
	}
	c.homeBin = put(c.home)
	return c
}

// availBin writes one copy of the fixture's carrier at a node. A direct INSERT,
// as the eligibility photograph does, so the attributes are exactly the
// fixture's; the hold goes through the store's own door.
func availBin(t *testing.T, db *store.DB, fx availFx, typeID int64, part string, at *nodes.Node) int64 {
	t.Helper()
	payload, loaded := part, any(time.Unix(1_700_000_500, 0).UTC())
	if fx.partLess {
		payload, loaded = "", nil
	}
	var id int64
	if err := db.DB.QueryRow(`
		INSERT INTO bins (bin_type_id, label, node_id, status, payload_code, uop_remaining,
		                  manifest_confirmed, loaded_at, created_at)
		VALUES ($1, $2, $3, 'available', $4, $5, $6, $7, $8) RETURNING id`,
		typeID, at.Name+"-BIN", at.ID, payload, fx.uop, fx.confirmed, loaded,
		time.Unix(1_700_000_000, 0).UTC()).Scan(&id); err != nil {
		t.Fatalf("%s: bin at %s: %v", fx.name, at.Name, err)
	}
	if fx.held {
		if err := db.SetBinQualityHold(id, true, "R6"); err != nil {
			t.Fatalf("%s: hold bin at %s: %v", fx.name, at.Name, err)
		}
	}
	return id
}

// availReaders is every reader the table runs. The plant-wide reads are taken
// once, after every fixture is down, and each cell looks its part up in them.
func availReaders(t *testing.T, db *store.DB, copies []availCopies) []availReader {
	t.Helper()
	inputs, err := sourceability.BuildInputs(db.DB, time.Hour)
	if err != nil {
		t.Fatalf("sourceability inputs: %v", err)
	}
	breakdown, err := sourceability.PoolBreakdownByPayload(db.DB)
	if err != nil {
		t.Fatalf("pool breakdown: %v", err)
	}
	codes := make([]string, 0, len(copies))
	for _, c := range copies {
		codes = append(codes, c.payload)
	}
	pre, err := service.NewInventoryService(db).PreflightAvailability(context.Background(), codes)
	if err != nil {
		t.Fatalf("changeover preflight: %v", err)
	}
	finder := NewSourceFinder(db, &DefaultResolver{DB: db}, nil)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	return []availReader{
		{"find_source_fifo", "store/bins/bin_manifest.go FindSourceFIFO (tier 5)", func(c availCopies) bool {
			b, err := db.FindSourceBinFIFO(c.payload, 0)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("%s: FindSourceBinFIFO: %v", c.fx.name, err)
			}
			return b != nil && b.ID != 0
		}},
		{"lane_mouth", "store/lane_queries.go FindSourceBinInLane", func(c availCopies) bool {
			b, err := db.FindSourceBinInLane(c.lane, c.payload, 0)
			return err == nil && b != nil && b.ID == c.laneBin
		}},
		{"dig_oldest_buried", "store/lane_queries.go FindOldestBuriedBin", func(c availCopies) bool {
			b, _, err := db.FindOldestBuriedBin(c.buriedLane, c.payload)
			return err == nil && b != nil && b.ID == c.buriedBin
		}},
		{"dig_shallowest_buried", "store/lane_queries.go FindBuriedBin", func(c availCopies) bool {
			b, _, err := db.FindBuriedBin(c.buriedLane, c.payload)
			return err == nil && b != nil && b.ID == c.buriedBin
		}},
		{"sourceability_pool", "store/sourceability/read.go availablePoolByPayload", func(c availCopies) bool {
			return inputs.Pool[c.payload] > 0
		}},
		{"pool_breakdown_free", "store/sourceability/read_page.go PoolBreakdownByPayload", func(c availCopies) bool {
			return breakdown[c.payload].Free > 0
		}},
		{"changeover_preflight", "service/inventory_preflight.go PreflightAvailability", func(c availCopies) bool {
			return !slices.Contains(pre.Missing, c.payload)
		}},
		{"group_slot_child", "dispatch/binresolver/helpers.go isBinAvailableForRetrieve, via the group scan", func(c availCopies) bool {
			res, err := (&GroupResolver{DB: db}).ResolveRetrieve(c.group, c.payload, reservations.Anyone, nil)
			return err == nil && res != nil && res.Bin != nil && res.Bin.ID == c.groupBin
		}},
		{"tier4_named_full", "dispatch/plan/finder.go Pick, namedFull", func(c availCopies) bool {
			return availPicked(t, finder, c, c.flat, c.flatBin)
		}},
		{"tier2_loader_pool", "dispatch/plan/finder.go sourceFromDedicatedLoader + binsource.RejectReason", func(c availCopies) bool {
			return availPicked(t, finder, c, c.home, c.homeBin)
		}},
		{"allocator_part_supply", "dispatch/allocator.go findAvailableForNeed, part supply", func(c availCopies) bool {
			return availAllocated(t, d, c, "")
		}},
		{"allocator_removal", "dispatch/allocator.go findAvailableForNeed, removal", func(c availCopies) bool {
			return availAllocated(t, d, c, c.flat.Name)
		}},
	}
}

// availPicked asks the tier cascade for a full of the fixture's part with the
// node as its named source, and reports whether it took the bin standing there.
func availPicked(t *testing.T, f *SourceFinder, c availCopies, src *nodes.Node, binID int64) bool {
	t.Helper()
	res := f.FindSourceForNeed(SourceNeed{Intent: IntentFull, PayloadCode: c.payload, SourceNode: src.Name})
	switch res.Outcome {
	case OutcomeFound:
		return res.Bin != nil && res.Bin.ID == binID
	case OutcomeWait:
		return false
	}
	t.Fatalf("%s: the cascade at %s answered outcome %v (%v), which no fixture here should reach",
		c.fx.name, src.Name, res.Outcome, res.Err)
	return false
}

// availAllocated asks the allocator's concrete-node arm about one pickup of the
// fixture's part at its plain node: a part supply, or — when processNode is the
// node itself — the removal of the order's own line position.
func availAllocated(t *testing.T, d *Dispatcher, c availCopies, processNode string) bool {
	t.Helper()
	step := resolvedStep{Action: protocol.ActionPickup, Node: c.flat.Name, PayloadCode: c.payload}
	pk := complexPickups([]resolvedStep{step}, processNode)[0]
	if (processNode != "") != (pk.kind == pickupRemoval) {
		t.Fatalf("%s: fixture drift — the pickup at %s is kind %v", c.fx.name, c.flat.Name, pk.kind)
	}
	got, hadBins, err := d.allocator.findAvailableForNeed(pk, resolvedStepPayload(step, c.payload))
	if err != nil {
		t.Fatalf("%s: findAvailableForNeed: %v", c.fx.name, err)
	}
	if !hadBins {
		t.Fatalf("%s: fixture drift — %s reads empty", c.fx.name, c.flat.Name)
	}
	return got != nil && got.ID == c.flatBin
}
