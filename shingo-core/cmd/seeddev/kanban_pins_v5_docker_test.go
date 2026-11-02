//go:build docker

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store/bins"
)

// kanban_pins_v5_docker_test.go — the family's acceptance on the rig's plant file: the
// walks cart by cart, the raid, the budgets at each state, and the CLEAR's family. Taken
// before the shape changes. Predictions: PINS.md, "BRIEF v5".
//
// THE WALK'S LEGS ARE PLAIN MOVES, as this harness's are: a position's old cart filled and
// moved to the unloader, then a retrieve of the new type off K. It is the counts and who
// fills which gap that are pinned here; the stack runs the complex legs.

// pinFamilyTypes is the rig's two families at the tree: the footprint says so.
var pinFamilyTypes = map[string][]string{
	"45x48":   {rigSmall, rigBig, rigTotes},
	"CART-32": {rig32},
}

var pinPositions = []string{"PLN_021", "PLN_022", "PLN_023", "PLN_024"}

// pinFound is the family's found. At the tree the only count there is, K's residents and
// comings; the loop read replaces it.
func (r *kanbanRig) pinFound(family string) int {
	r.t.Helper()
	k, err := r.db.GetNodeByName(rigK)
	testutil.MustNoErr(r.t, err, "K")
	counts, err := r.db.ReadKanbanCounts([]int64{k.ID})
	testutil.MustNoErr(r.t, err, "counts")
	levels, err := r.db.ListKanbanLevels(k.ID)
	testutil.MustNoErr(r.t, err, "levels")
	n := 0
	for _, lv := range levels {
		for _, code := range pinFamilyTypes[family] {
			if lv.BinTypeCode == code {
				c := counts.Level(lv, 0)
				n += c.Resident + c.Coming
			}
		}
	}
	return n
}

// foundIsPool is the acceptance at one sample, 10 and 6.
func (r *kanbanRig) foundIsPool(when string) {
	r.t.Helper()
	for family, pool := range map[string]int{"45x48": 10, "CART-32": 6} {
		if got := r.pinFound(family); got != pool {
			r.t.Errorf("%s: found(%s) = %d, want the pool %d", when, family, got, pool)
		}
	}
}

// pinFly is fly, sampling after each pass.
func (r *kanbanRig) pinFly(when string) {
	r.t.Helper()
	for pass := 0; pass < 12; pass++ {
		r.eng.RunFulfillmentScan()
		moved := false
		for _, o := range r.live() {
			switch {
			case o.Status == protocol.StatusDelivered:
				r.eng.Dispatcher().HandleOrderReceipt(rigEnvelope(o.StationID),
					&protocol.OrderReceipt{OrderUUID: o.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
				moved = true
			case o.VendorOrderID != "" && r.sim.HasOrder(o.VendorOrderID):
				r.sim.DriveSimpleLifecycle(o.VendorOrderID)
				moved = true
			}
		}
		r.foundIsPool(fmt.Sprintf("%s, pass %d", when, pass))
		if !moved {
			return
		}
	}
	r.t.Fatalf("%s: the fleet did not settle: %s", when, r.said(r.live()))
}

// tapWindows is the unloader's operator: every cart standing on a window with no order on
// it, cleared as it is lit. A cart nothing is short of leaves as it stands.
func (r *kanbanRig) tapWindows() int {
	r.t.Helper()
	lit := r.lit()
	taken := map[int64]bool{}
	for _, o := range r.live() {
		for _, c := range o.Carts {
			taken[c.BinID] = true
		}
		if o.NamedBinID != nil {
			taken[*o.NamedBinID] = true
		}
	}
	tapped := 0
	for _, w := range r.standing() {
		if taken[r.cartAt(w)] {
			continue
		}
		r.tap(w, rigK, lit[w].BinType)
		tapped++
	}
	return tapped
}

// typeAt is the type of the cart on a node, "" with none.
func (r *kanbanRig) typeAt(node string) string {
	r.t.Helper()
	var code string
	err := r.db.QueryRow(`SELECT t.code FROM bins b JOIN nodes n ON n.id = b.node_id JOIN bin_types t ON t.id = b.bin_type_id
		WHERE n.name = $1 ORDER BY b.id LIMIT 1`, node).Scan(&code)
	if err != nil {
		return ""
	}
	return code
}

// change is a START onto part, of type code, walked until every position holds the type:
// the episode opens, and each round the keeper ticks, the operator taps what stands, and
// each position whose new cart stands in K takes it, its old cart going to a window.
func (r *kanbanRig) change(when, part, code string) {
	r.t.Helper()
	r.openChangeover(part)
	for round := 0; round < 24; round++ {
		r.tick()
		r.foundIsPool(fmt.Sprintf("%s, round %d, ticked", when, round))
		r.pinFly(fmt.Sprintf("%s, round %d, the arm", when, round))
		tapped := r.tapWindows()
		r.pinFly(fmt.Sprintf("%s, round %d, tapped", when, round))
		swapped := 0
		for _, pos := range pinPositions {
			if r.typeAt(pos) == code || r.residents(rigK, code) == 0 || len(r.standing()) == len(r.windows) {
				continue
			}
			if old := r.cartAt(pos); old != 0 {
				r.fill(old, "KP-SM-L")
				r.move(pos, "PRESS-UNLOAD", old)
				r.pinFly(fmt.Sprintf("%s, round %d, %s out", when, round, pos))
			}
			r.order(protocol.OrderRequest{OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true,
				PayloadCode: part, SourceNode: rigK, DeliveryNode: pos, OriginClass: protocol.OriginClassNoDemand})
			r.pinFly(fmt.Sprintf("%s, round %d, %s in", when, round, pos))
			swapped++
		}
		done := true
		for _, pos := range pinPositions {
			done = done && r.typeAt(pos) == code
		}
		if done && tapped == 0 && swapped == 0 && len(r.standing()) == 0 {
			r.settleRow(code, part)
			return
		}
	}
	r.t.Errorf("%s: the positions never all took %s: %v; %s; live: %s", when, code,
		[]string{r.typeAt("PLN_021"), r.typeAt("PLN_022"), r.typeAt("PLN_023"), r.typeAt("PLN_024")}, r.cardsSaid(), r.said(r.live()))
}

// settleRow moves the claims' active style to the target and closes the episode, as the
// cutover does, so the next tick reads a steady plant.
func (r *kanbanRig) settleRow(code, part string) {
	r.t.Helper()
	r.closeChangeovers()
	plant := kanbanPressPlant(r.t)
	style := map[string]string{"KP-SM-L": "KP-SMALL", "KP-BIG": "KP-BIG", "KP-TOTE": "KP-TOTES", "KP-32": "KP-32"}[part]
	for i := range plant.Processes {
		if plant.Processes[i].Name == "KP-PRESS" {
			plant.Processes[i].ActiveStyle = style
		}
	}
	seedClaimsMirror(r.t, r.db, rigClaimedPlant(plant))
	r.tick()
}

// inK reads K's standing mix.
func (r *kanbanRig) inK(when string, small, big, totes, c32 int) {
	r.t.Helper()
	for code, want := range map[string]int{rigSmall: small, rigBig: big, rigTotes: totes, rig32: c32} {
		if got := r.residents(rigK, code); got != want {
			r.t.Errorf("%s: K holds %d %s, want %d: %s", when, got, code, want, r.cardsSaid())
		}
	}
}

// TestKanbanPin_TheWalkSmall32Big is the owner's walk: steady on small, START small to
// 32x30, steady, START 32x30 to big. Found equals the pool at every sample, the mix is held,
// and step 4 ends S2 B2 T2 C6 with no floaters.
func TestKanbanPin_TheWalkSmall32Big(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	r.foundIsPool("steady small")
	r.inK("steady small", 2, 2, 2, 6)
	for _, line := range []string{rigSmall, rigBig, rigTotes, rig32} {
		if got := r.card(rigK, line); got.Gap > 0 {
			t.Errorf("steady small: %s reads short %d, want none: %+v", line, got.Gap, got)
		}
	}
	r.change("small to 32x30", "KP-32", rig32)
	r.inK("steady 32x30", 6, 2, 2, 2)
	if toSomewhere := r.moves(); toSomewhere != 0 {
		t.Errorf("steady 32x30: %d surplus moves on a step with its mix whole", toSomewhere)
	}
	r.change("32x30 to big", "KP-BIG", rigBig)
	r.inK("step 4", 2, 2, 2, 6)
}

// TestKanbanPin_TheWalkAllBigsToSmall is the owner's literal case: all ten of the family
// dressed big. 8 re-types: the line's 4 at the windows on their way back, K's 4 through the
// arm.
func TestKanbanPin_TheWalkAllBigsToSmall(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	r.change("small to big", "KP-BIG", rigBig)
	r.reType(rigBig, "PKB_001", "PKB_002", "PKB_005", "PKB_006", "PKB_007", "PKB_008", "PKB_009", "PKB_010")
	r.tick()
	r.inK("all bigs", 0, 6, 0, 6)
	r.change("big to small", "KP-SM-L", rigSmall)
	r.inK("back on small", 2, 2, 2, 6)
}

// reType stamps whatever stands on the slots, by hand, as an admin re-type would.
func (r *kanbanRig) reType(code string, slots ...string) {
	r.t.Helper()
	bt, err := r.db.GetBinTypeByCode(code)
	testutil.MustNoErr(r.t, err, "type")
	for _, s := range slots {
		if c := r.cartAt(s); c != 0 {
			_, err := r.db.Exec(`UPDATE bins SET bin_type_id = $1 WHERE id = $2`, bt.ID, c)
			testutil.MustNoErr(r.t, err, "re-type")
		}
	}
}

// moves counts the arm's moves ever filed.
func (r *kanbanRig) moves() int {
	r.t.Helper()
	var n int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE edge_uuid LIKE 'core-sp-%' AND status NOT IN (`+
		protocol.TerminalStatusSQLList()+`)`).Scan(&n), "the arm's moves")
	return n
}

// stockSM stands 45x48 empties in the plain supermarket: the raid needs something to raid.
func (r *kanbanRig) stockSM() map[string]int64 {
	r.t.Helper()
	stock := map[string]int64{}
	for i, code := range []string{rigSmall, rigSmall, rigBig, rigBig} {
		bt, err := r.db.GetBinTypeByCode(code)
		testutil.MustNoErr(r.t, err, "type")
		slot := fmt.Sprintf("KSM_00%d", i*2+2)
		n, err := r.db.GetNodeByName(slot)
		testutil.MustNoErr(r.t, err, "slot")
		id := n.ID
		b := &bins.Bin{BinTypeID: bt.ID, Label: "KB-SM-" + slot, NodeID: &id, Status: "available"}
		testutil.MustNoErr(r.t, r.db.CreateBin(b), "stock")
		stock[slot] = b.ID
	}
	return stock
}

func (r *kanbanRig) smUnchanged(when string, stock map[string]int64) {
	r.t.Helper()
	for slot, id := range stock {
		if got := r.cartAt(slot); got != id {
			r.t.Errorf("%s: %s holds cart %d, want its stocked %d: the keeper raided the supermarket", when, slot, got, id)
		}
	}
	var asked int
	testutil.MustNoErr(r.t, r.db.QueryRow(`SELECT count(*) FROM orders WHERE edge_uuid LIKE 'core-mnt-%'`).Scan(&asked), "asks")
	if asked != 0 {
		r.t.Errorf("%s: %d asks outside with the family whole: %s", when, asked, r.said(r.live()))
	}
}

// TestKanbanPin_TheRaidSteady: R1 with KP-SM stocked. K's family is whole, so nothing comes
// out of the supermarket however the press cycles.
func TestKanbanPin_TheRaidSteady(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	stock := r.stockSM()
	s := rigSides[0]
	for cycle := 0; cycle < 3; cycle++ {
		r.tick()
		r.leave(s)
		r.tick()
		r.tapWindows()
		r.fly()
		r.refill(s)
		r.fly()
		r.tick()
		r.smUnchanged(fmt.Sprintf("cycle %d", cycle), stock)
		r.foundIsPool(fmt.Sprintf("cycle %d", cycle))
	}
}

// TestKanbanPin_TheRaidOnAChange: START small to big with KP-SM stocked. Big is short 4 inside
// a whole family, which is the arm's: an ask outside would end the loop at found 14.
func TestKanbanPin_TheRaidOnAChange(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	stock := r.stockSM()
	r.change("small to big", "KP-BIG", rigBig)
	r.smUnchanged("small to big", stock)
	r.foundIsPool("small to big")
	r.inK("small to big", 2, 2, 2, 6)
}

// TestKanbanPin_TheArmLeavesWindowsForTheEvacs: at START the line's four carts have no leg
// yet and K stands full of what it could re-type. The arm must not take the windows those
// four need: with every free window spoken for it sends one, and only so a change that
// needs a re-type before its first swap is not wedged.
func TestKanbanPin_TheArmLeavesWindowsForTheEvacs(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	r.handCarts(rigSmall, "PKB_007", "PKB_008", "PKB_009", "PKB_010")
	r.openChangeover("KP-BIG")
	r.tick()
	if got := r.moves(); got > 1 {
		t.Errorf("the arm filed %d moves with 4 windows free and 4 line carts about to need them, want at most 1", got)
	}
}

// TestKanbanPin_ClaimedOnEveryRigState: a type's cover is the positions the effective style
// puts on the line. On every rig state it is 4 for the running type and 0 for the rest, read
// beside DISTINCT positions in the claims, before the typed cover goes.
func TestKanbanPin_ClaimedOnEveryRigState(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	k, err := r.db.GetNodeByName(rigK)
	testutil.MustNoErr(t, err, "K")
	read := func(when, running string) {
		t.Helper()
		rows, err := r.db.ListKanbanPicture([]int64{k.ID})
		testutil.MustNoErr(t, err, "picture")
		for _, row := range rows {
			want := 0
			if row.BinTypeCode == running {
				want = 4
			}
			if got := row.Want() - row.Base; got != want {
				t.Errorf("%s: %s covers %d, want %d", when, row.BinTypeCode, got, want)
			}
		}
	}
	read("steady small", rigSmall)
	for _, c := range []struct{ part, code string }{{"KP-BIG", rigBig}, {"KP-TOTE", rigTotes}, {"KP-32", rig32}, {"KP-SM-L", rigSmall}} {
		r.closeChangeovers()
		r.openChangeover(c.part)
		read("open onto "+c.part, c.code)
	}
}

// statements at each state, read after the tick has settled.
func (r *kanbanRig) inBudget(when string) {
	r.t.Helper()
	r.countTick()
	if got := r.countTick(); len(got) > 4 {
		r.t.Errorf("%s: tick = %d statements, budget 4\n%s", when, len(got), testutil.StatementBreakdown(got))
	}
	if got := r.countBuild(); len(got) > 10 {
		r.t.Errorf("%s: build = %d statements, budget 10\n%s", when, len(got), testutil.StatementBreakdown(got))
	}
}

// TestKanbanPin_TheBudgetsAtEachState: tick at most 4, build at most 10, door at most 8, on
// the rig's plant at steady, after a START, at the door of a cart into K.
func TestKanbanPin_TheBudgetsAtEachState(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	r.inBudget("steady small")
	s := rigSides[0]
	r.leave(s)
	w := r.standing()[0]
	lit := r.lit()[w].BinType
	cart := r.clear(w, lit)
	door, o := r.countDoor(w, cart)
	t.Logf("BUDGET door %d: %s to %s (%q)", len(door), o.Status, o.DeliveryNode, o.QueueCause)
	if len(door) > 8 {
		t.Errorf("the door = %d statements, budget 8\n%s", len(door), testutil.StatementBreakdown(door))
	}
	r.fly()
	r.refill(s)
	r.fly()
	r.openChangeover("KP-32")
	r.inBudget("changeover small to 32x30")
}

// clearStatements counts the CLEAR's door.
func (r *kanbanRig) clearStatements(window, as string) []string {
	r.t.Helper()
	cart := r.cartAt(window)
	raw, err := json.Marshal(protocol.BinClearRequest{NodeName: window, BinID: cart, BinTypeCode: as})
	testutil.MustNoErr(r.t, err, "the CLEAR")
	r.counter.Reset()
	if res := r.doors.AskBinClear(rigStation, raw); res.Status != http.StatusOK {
		r.t.Fatalf("CLEAR at %s refused (%d): %s", window, res.Status, res.Sentence)
	}
	return r.counter.Statements()
}

// TestKanbanPin_TheClearCostsWhatItCost: the family's check reads the bin the door has
// already read. The number at the tree: rigClearStatements.
func TestKanbanPin_TheClearCostsWhatItCost(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	r.leave(rigSides[0])
	got := r.clearStatements(r.standing()[0], rigBig)
	t.Logf("BUDGET the CLEAR: %d statements\n%s", len(got), testutil.StatementBreakdown(got))
	if len(got) > rigClearStatements {
		t.Errorf("the CLEAR = %d statements, was %d", len(got), rigClearStatements)
	}
}

// familyTableRows reads the one truth table.
func familyTableRows(t *testing.T) (map[string]string, []struct {
	Cart, Asked string
	Same        bool
}) {
	t.Helper()
	raw, err := os.ReadFile("../../kanban/testdata/same_family.json")
	testutil.MustNoErr(t, err, "the table")
	var table struct {
		Types map[string]struct {
			Family string `json:"family"`
		} `json:"types"`
		Cases []struct {
			Cart, Asked string
			Same        bool
		} `json:"cases"`
	}
	testutil.MustNoErr(t, json.Unmarshal(raw, &table), "the table")
	types := map[string]string{}
	for code, ty := range table.Types {
		types[code] = ty.Family
	}
	return types, table.Cases
}

// TestKanbanPin_TheClearRefusesAnotherFamily: the truth table at the CLEAR's door: a stamp
// within the family goes through, one across it is refused with a sentence on the board. The
// door checks only that the code exists at the tree, and a cross-family stamp corrupts two
// families' counts.
func TestKanbanPin_TheClearRefusesAnotherFamily(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	types, cases := familyTableRows(t)
	for code, family := range types {
		testutil.MustNoErr(t, r.db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(new(int64)), "type "+code)
		if _, err := r.db.Exec(`INSERT INTO carrier_families (code) VALUES ($1) ON CONFLICT DO NOTHING`, family); err != nil {
			t.Logf("no family table at this tree: %v", err)
			continue
		}
		_, err := r.db.Exec(`UPDATE bin_types SET family_id = (SELECT id FROM carrier_families WHERE code = $1) WHERE code = $2`, family, code)
		testutil.MustNoErr(t, err, "family")
	}
	w, err := r.db.GetNodeByName(r.windows[0])
	testutil.MustNoErr(t, err, "window")
	for i, c := range cases {
		bt, err := r.db.GetBinTypeByCode(c.Cart)
		testutil.MustNoErr(t, err, "cart type")
		id := w.ID
		b := &bins.Bin{BinTypeID: bt.ID, Label: fmt.Sprintf("KB-TABLE-%d", i), NodeID: &id, Status: "available"}
		testutil.MustNoErr(t, r.db.CreateBin(b), "the cart")
		raw, err := json.Marshal(protocol.BinClearRequest{NodeName: w.Name, BinID: b.ID, BinTypeCode: c.Asked})
		testutil.MustNoErr(t, err, "the CLEAR")
		res := r.doors.AskBinClear(rigStation, raw)
		if ok := res.Status == http.StatusOK; ok != c.Same {
			t.Errorf("a %s cleared as %s: status %d %q; the table says the two are one family: %v", c.Cart, c.Asked, res.Status, res.Sentence, c.Same)
		}
		if !c.Same && !strings.Contains(res.Sentence, c.Asked) {
			t.Errorf("the refusal does not name %s: %q", c.Asked, res.Sentence)
		}
		_, err = r.db.Exec(`DELETE FROM bins WHERE id = $1`, b.ID)
		testutil.MustNoErr(t, err, "the cart goes")
	}
}

// TestKanbanPin_AnAdminReTypeCrossesTheFamily: the CLEAR refusal is the floor rule. The
// admin re-type stays; the handler ledgers it (www TestBinsUpdate, requireAudit "updated").
func TestKanbanPin_AnAdminReTypeCrossesTheFamily(t *testing.T) {
	t.Parallel()
	r := newKanbanRig(t)
	cart := r.cartAt("PKB_001")
	b, err := r.db.GetBin(cart)
	testutil.MustNoErr(t, err, "the cart")
	bt, err := r.db.GetBinTypeByCode(rig32)
	testutil.MustNoErr(t, err, "the type")
	id := bt.ID
	testutil.MustNoErr(t, r.eng.BinService().Update(b, nil, nil, &id, nil), "the re-type")
	got, err := r.db.GetBin(cart)
	testutil.MustNoErr(t, err, "the cart")
	if got.BinTypeID != bt.ID {
		t.Errorf("the admin's re-type did not take: %d", got.BinTypeID)
	}
}
