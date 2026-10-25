//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// tops_and_bottoms_bind_docker_test.go — where a complex pickup binds tops &
// bottoms (binresolver.BindableEmpties): on a cell's own position whatever height the
// press asks for (the house rule: a carrier there belongs to that cell), and
// elsewhere only where it is set down at a height that admits it. And where an
// empty need naming a concrete source looks (tier 4's namedEmpty): there, and
// nowhere else.

// tbCell is a produce press cell (vpCell) on a tote type of 8 to a layer; with
// layers its part states tote_layers 2, so an empty set down on the outside is
// wanted two high and one set down on the press (a fed core) at 0.
func tbCell(t *testing.T, prefix string, layers bool) vpCell {
	t.Helper()
	c := newVPCell(t, prefix, protocol.ClaimRoleProduce)
	_, err := c.db.Exec(`UPDATE bin_types SET totes_per_layer = 8 WHERE id = $1`, c.binType)
	testutil.MustNoErr(t, err, "a tote type")
	if layers {
		_, err = c.db.Exec(`UPDATE payloads SET tote_layers = 2 WHERE code = $1`, c.part)
		testutil.MustNoErr(t, err, "the part's layers")
	}
	return c
}

// totesOn stands a cart of the cell's type on node at totes on the cart.
func (c vpCell) totesOn(node *nodes.Node, label string, totes int) *bins.Bin {
	c.t.Helper()
	b := testdb.CreateBinAtNode(c.t, c.db, "", node.ID, label)
	_, err := c.db.Exec(`UPDATE bins SET totes_on_cart = $1 WHERE id = $2`, totes, b.ID)
	testutil.MustNoErr(c.t, err, "totes on "+label)
	return b
}

// requireClaimedBy says the leg uuid was sent and holds the carrier.
func (c vpCell) requireClaimedBy(uuid string, b *bins.Bin) {
	c.t.Helper()
	o := prReloadUUID(c.t, c.db, uuid)
	got, err := c.db.GetBin(b.ID)
	testutil.MustNoErr(c.t, err, "the carrier")
	if o.VendorOrderID == "" || got.ClaimedBy == nil || *got.ClaimedBy != o.ID {
		c.t.Errorf("%s: %s vendor %q code %q cause %q, carrier %s claimed by %v: want sent with it",
			uuid, o.Status, o.VendorOrderID, o.QueueCode, o.QueueCause, b.Label, got.ClaimedBy)
	}
}

// requireUnclaimed says nothing holds the carrier.
func (c vpCell) requireUnclaimed(b *bins.Bin) {
	c.t.Helper()
	got, err := c.db.GetBin(b.ID)
	testutil.MustNoErr(c.t, err, "the carrier")
	if got.ClaimedBy != nil {
		c.t.Errorf("carrier %s is claimed by %d, want left where it stands", b.Label, *got.ClaimedBy)
	}
}

// TestTopsAndBottoms_R2LiftsItOffTheOutside: the index leg lifts the tops &
// bottoms SEND BACK left on the press's outside: a two-position tote press,
// unflipped and flipped (set down on the press, which wants 0); a
// three-position press's second paired position (set down on the outside, which
// wants a stack); and a press whose part states no tote_layers (no height said,
// which admits no tops & bottoms). The last two bind by the house rule alone.
func TestTopsAndBottoms_R2LiftsItOffTheOutside(t *testing.T) {
	t.Parallel()
	for _, flipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "two positions, unflipped", true: "two positions, flipped"}[flipped], func(t *testing.T) {
			t.Parallel()
			c := tbCell(t, "TBR", true)
			tb := c.totesOn(c.deck, "TBR-OUTSIDE", 0)
			r1, r2 := c.submitPair("TBR", flipped, "TBR-r2", "TBR-r1")
			c.scan(r1, r2)
			c.requireTook(r1, r2, c.deck)
			c.requireClaimedBy(r2, tb)
		})
	}
	t.Run("three positions, the second paired position", func(t *testing.T) {
		t.Parallel()
		c := tbCell(t, "TB3", true)
		back := prNode(t, c.db, "TB3-BACK")
		c.claim(t, back.Name, nil)
		onDeck := c.totesOn(c.deck, "TB3-OUTSIDE", 0)
		atBack := c.totesOn(back, "TB3-AT-BACK", 0)
		prSubmitLeg(c.d, "tb3-r2", "", c.part, c.press.Name, prWait(c.deck.Name), c.pickDeck(), prDrop(c.press.Name),
			protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: back.Name, Empty: true}, prDrop(c.deck.Name))
		c.scan("tb3-r2")
		c.requireClaimedBy("tb3-r2", onDeck)
		c.requireClaimedBy("tb3-r2", atBack)
		var steps []protocol.ComplexOrderStep
		testutil.MustNoErr(t, json.Unmarshal([]byte(prReloadUUID(t, c.db, "tb3-r2").StepsJSON), &steps), "steps")
		if steps[3].Node != back.Name {
			t.Errorf("the second paired pickup is at %s, want %s", steps[3].Node, back.Name)
		}
		c.requireUnclaimed(c.inCarrier)
	})
	t.Run("a part that states no tote_layers", func(t *testing.T) {
		t.Parallel()
		c := tbCell(t, "TBN", false)
		tb := c.totesOn(c.deck, "TBN-OUTSIDE", 0)
		r1, r2 := c.submitPair("TBN", false, "TBN-r2", "TBN-r1")
		c.scan(r1, r2)
		c.requireTook(r1, r2, c.deck)
		c.requireClaimedBy(r2, tb)
	})
}

// TestTopsAndBottoms_AConcreteSupplyNodeHoldingOnlyIt: a step naming a supply
// node that is no cell's position, holding only tops & bottoms, waits when it
// sets the carrier down on an outside (wanted two high), and takes it when it
// sets it down on a fed press's core (wanted at 0).
func TestTopsAndBottoms_AConcreteSupplyNodeHoldingOnlyIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		onto  func(c vpCell) *nodes.Node
		takes bool
	}{
		{"set down on an outside", func(c vpCell) *nodes.Node { return c.deck }, false},
		{"set down on a fed core", func(c vpCell) *nodes.Node { return c.press }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cell := tbCell(t, "TBS", true)
			supply := prNode(t, cell.db, "TBS-SUPPLY")
			tb := cell.totesOn(supply, "TBS-AT-SUPPLY", 0)
			prSubmitLeg(cell.d, "tbs-leg", "", cell.part, cell.press.Name,
				protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: supply.Name, Empty: true}, prDrop(c.onto(cell).Name))
			cell.scan("tbs-leg")
			if c.takes {
				cell.requireClaimedBy("tbs-leg", tb)
				return
			}
			cell.requireHolds(protocol.QueueWaitingForMaterial, "tbs-leg")
			cell.requireUnclaimed(tb)
		})
	}
}

// TestTopsAndBottoms_ReanchorTakesItOnTheConcreteSourceOnly: W5's vacant
// on-deck fetch from a concrete inbound source, set down on the press (wanted
// at 0), takes the tops & bottoms standing there; with nothing there it waits,
// and never takes the tops & bottoms standing elsewhere in the plant.
func TestTopsAndBottoms_ReanchorTakesItOnTheConcreteSourceOnly(t *testing.T) {
	t.Parallel()
	t.Run("tops & bottoms on the inbound source", func(t *testing.T) {
		t.Parallel()
		c := tbCell(t, "TBA", true)
		_, err := c.db.Exec(`UPDATE bins SET totes_on_cart = 0 WHERE id = $1`, c.inCarrier.ID)
		testutil.MustNoErr(t, err, "tops & bottoms on the inbound source")
		elsewhere := c.totesOn(prNode(t, c.db, "TBA-ELSEWHERE"), "TBA-AT-ELSEWHERE", 0)
		r1, r2 := c.submitPair("TBA", false, "TBA-r2", "TBA-r1")
		c.scan(r1, r2)
		c.requireTook(r1, r2, c.in)
		c.requireClaimedBy(r2, c.inCarrier)
		c.requireUnclaimed(elsewhere)
	})
	t.Run("nothing on the inbound source", func(t *testing.T) {
		t.Parallel()
		c := tbCell(t, "TBB", true)
		_, err := c.db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, c.out.ID, c.inCarrier.ID)
		testutil.MustNoErr(t, err, "the inbound source's carrier leaves")
		elsewhere := c.totesOn(prNode(t, c.db, "TBB-ELSEWHERE"), "TBB-AT-ELSEWHERE", 0)
		r1, r2 := c.submitPair("TBB", false, "TBB-r2", "TBB-r1")
		c.scan(r1, r2)
		for _, u := range []string{r1, r2} {
			if o := prReloadUUID(t, c.db, u); o.VendorOrderID != "" {
				t.Errorf("%s was sent; the pair must wait for its inbound source", u)
			}
		}
		c.requireUnclaimed(elsewhere)
	})
}

// TestTopsAndBottoms_AGroupPickupForAStackNeverResolvesToIt: an empty pickup
// naming a group (complex_steps' group arm asks the finder exactly this), set
// down on the outside (wanted two high), or with no height said, never resolves
// to the tops & bottoms the group holds; a stack beside it is taken. Set down
// on the press (wanted at 0), the tops & bottoms is taken first.
func TestTopsAndBottoms_AGroupPickupForAStackNeverResolvesToIt(t *testing.T) {
	t.Parallel()
	c := tbCell(t, "TBG", true)
	grpID, err := nodes.CreateGroup(c.db.DB, "TBG-GRP")
	testutil.MustNoErr(t, err, "group")
	testdb.MarkGroup(t, c.db, grpID)
	slot := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, nodes.Create(c.db.DB, n), name)
		return n
	}
	tb := c.totesOn(slot("TBG-SLOT-1"), "TBG-TB", 0)
	need := func(setDown string) SourceNeed {
		return SourceNeed{SourceNode: "TBG-GRP", PayloadCode: c.part, ProcessNode: c.press.Name,
			SetDownNode: setDown, Intent: IntentEmpty}
	}
	pick := func(setDown string) int64 {
		res := NewSourceFinder(c.db, nil, nil).FindSourceForNeed(need(setDown))
		if res.Bin == nil {
			return 0
		}
		return res.Bin.ID
	}
	for _, setDown := range []string{c.deck.Name, ""} {
		if got := pick(setDown); got != 0 {
			t.Errorf("only tops & bottoms in the group, set down on %q: took %d, want nothing", setDown, got)
		}
	}
	stack := c.totesOn(slot("TBG-SLOT-2"), "TBG-STACK", 16)
	for _, setDown := range []string{c.deck.Name, ""} {
		if got := pick(setDown); got != stack.ID {
			t.Errorf("tops & bottoms beside a stack, set down on %q: took %d, want the stack %d", setDown, got, stack.ID)
		}
	}
	if got := pick(c.press.Name); got != tb.ID {
		t.Errorf("set down on the press: took %d, want tops & bottoms %d first", got, tb.ID)
	}
}
