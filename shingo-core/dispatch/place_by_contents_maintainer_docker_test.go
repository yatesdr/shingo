//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
)

// place_by_contents_maintainer_docker_test.go — the maintainer arm of the
// by-contents choice (dropByContents): an empty lifted off a line position
// goes to a maintained group short of it, the maintainer winning over the
// planned drop; a tote cart only as its own height, and never off a fed
// press's outside. Driven through each door: a plain move that names its bin
// (decideTransport), a release (placeByContentsAtRelease), and a lift with no
// wait ahead of it at its reserve (placeByContentsAtReserve).

// mtCell is a fed press cell (the press, its outside DECK) whose claim the
// maintained group GRP supports, both positions; OUT is the planned drop and
// AWAY a node no claim names (a loader window's or a storage node's stand-in).
// TOTE is a tote carrier of 8 to a layer, KD a knockdown.
type mtCell struct {
	t                      *testing.T
	db                     *store.DB
	d                      *Dispatcher
	part                   string
	press, deck, out, away *nodes.Node
	grp                    int64
	tote, kd               int64
}

func newMTCell(t *testing.T, prefix string, role protocol.ClaimRole) mtCell {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	// With the group resolver production wires: the maintainer arm chooses a
	// group's slot through it.
	d := NewDispatcher(db, testdb.NewTrackingBackend(), &mockEmitter{}, "core", "shingo.dispatch", &DefaultResolver{DB: db})
	d.DebugLog = testDebugLog(t)
	c := mtCell{t: t, db: db, d: d, part: sd.Payload.Code,
		press: prNode(t, db, prefix+"-PRESS"), deck: prNode(t, db, prefix+"-DECK"),
		out: prNode(t, db, prefix+"-OUT"), away: prNode(t, db, prefix+"-AWAY")}
	_, err := db.ReplacePlantClaimsFromStation("MT-PROC", "line-1",
		[]plantclaims.StyleRow{{ProcessID: "MT-PROC", StyleID: "MT-STYLE", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "MT-PROC", StyleID: "MT-STYLE", CoreNodeName: c.press.Name, Role: role,
			SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: c.part, InboundSource: c.away.Name,
			OutboundDestination: c.out.Name, PairedCoreNode: c.deck.Name}}, nil, 0)
	testutil.MustNoErr(t, err, "the press's claim")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ($1, 8) RETURNING id`,
		prefix+"-TOTE").Scan(&c.tote), "tote type")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`,
		prefix+"-KD").Scan(&c.kd), "knockdown type")
	c.grp, err = db.CreateNodeGroup(prefix + "-GRP")
	testutil.MustNoErr(t, err, "group")
	testdb.MarkGroup(t, db, c.grp)
	for i := 1; i <= 6; i++ {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: fmt.Sprintf("%s-GRP-%d", prefix, i), Enabled: true,
			ParentID: &c.grp}), "slot")
	}
	testutil.MustNoErr(t, db.SetMaintainSupports(c.grp, []int64{c.press.ID, c.deck.ID}), "supports")
	return c
}

// keep declares the group's level of a type, with its layer wants.
func (c mtCell) keep(binType int64, want int, heights map[int]int) {
	c.t.Helper()
	var wants []store.MaintainLayerWant
	for layers, n := range heights {
		wants = append(wants, store.MaintainLayerWant{Layers: layers, Want: n})
	}
	testutil.MustNoErr(c.t, c.db.SetMaintainLevelAndLayerWants(store.MaintainLevel{GroupNodeID: c.grp, BinTypeID: binType,
		Want: want}, wants), "level")
}

// cart stands a cart of binType on node at totes (nil: unknown), no part.
func (c mtCell) cart(node *nodes.Node, binType int64, label string, totes any) *bins.Bin {
	c.t.Helper()
	var id int64
	testutil.MustNoErr(c.t, c.db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
		VALUES ($1,$2,$3,'available',$4) RETURNING id`, binType, label, node.ID, totes).Scan(&id), "cart "+label)
	b, err := c.db.GetBin(id)
	testutil.MustNoErr(c.t, err, "read back "+label)
	return b
}

// moveOff sends the move a station sends for one cart (a clear, SEND BACK's
// move): it names the bin, off from, planned to OUT. It answers where the
// move now goes.
func (c mtCell) moveOff(uuid string, from *nodes.Node, b *bins.Bin) string {
	c.t.Helper()
	c.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid, OrderType: OrderTypeMove,
		SourceNode: from.Name, DeliveryNode: c.out.Name, Quantity: 1, BinID: b.ID})
	o, err := c.db.GetOrderByUUID(uuid)
	testutil.MustNoErr(c.t, err, "the move")
	return c.where(o.DeliveryNode)
}

// where says "GRP" for a slot of the maintained group, else the node's name.
func (c mtCell) where(node string) string {
	c.t.Helper()
	n, err := c.db.GetNodeByDotName(node)
	testutil.MustNoErr(c.t, err, "node "+node)
	if n.ParentID != nil && *n.ParentID == c.grp {
		return "GRP"
	}
	return n.Name
}

// TestMaintainerArm_ToteCartsByHeight: off the press's own position, a tote
// cart goes to the group only as its own height: 3 high to a group short of 3
// highs, tops & bottoms to a group short of its row; 3 high + 2 and an
// unknown count keep the planned drop.
func TestMaintainerArm_ToteCartsByHeight(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		totes any
		want  string
	}{
		{"3 high", 24, "GRP"},
		{"3 high + 2", 26, "OUT"},
		{"tops & bottoms", 0, "GRP"},
		{"unknown", nil, "OUT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cell := newMTCell(t, "MTH", protocol.ClaimRoleProduce)
			cell.keep(cell.tote, 6, map[int]int{3: 2, 0: 1})
			b := cell.cart(cell.press, cell.tote, "MTH-CART", c.totes)
			want := c.want
			if want == "OUT" {
				want = cell.out.Name
			}
			if got := cell.moveOff("mth-move", cell.press, b); got != want {
				t.Errorf("the cart went to %s, want %s", got, want)
			}
		})
	}
}

// TestMaintainerArm_AToteCartOffAFedOutsideKeepsItsDrop: SEND BACK's case, a
// tote cart lifted off the press's outside set down 5 high with 2 drawn,
// keeps its planned drop though the group is short of 5 highs; and so does a
// cart standing exactly 3 high there with the group short of 3 highs: its count
// is exact only at its lift, so the unloader's card decides.
func TestMaintainerArm_AToteCartOffAFedOutsideKeepsItsDrop(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		totes int
	}{{"5 high with 2 drawn", 38}, {"exactly 3 high", 24}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cell := newMTCell(t, "MTF", protocol.ClaimRoleProduce)
			cell.keep(cell.tote, 6, map[int]int{3: 2, 5: 2})
			b := cell.cart(cell.deck, cell.tote, "MTF-CART", c.totes)
			if got := cell.moveOff("mtf-move", cell.deck, b); got != cell.out.Name {
				t.Errorf("the cart off the outside went to %s, want the planned drop %s", got, cell.out.Name)
			}
		})
	}
}

// TestMaintainerArm_AKnockdownOffALinePosition: a named-bin move of an empty
// knockdown off a line position (the shape of a clear) goes to the short
// group; the same move off a node that is no line position (a drain off a
// loader window, a pull off storage) keeps its drop; a group at its level
// wants nothing; a cart with a part is no empty.
func TestMaintainerArm_AKnockdownOffALinePosition(t *testing.T) {
	t.Parallel()
	t.Run("off the press: a short group", func(t *testing.T) {
		t.Parallel()
		c := newMTCell(t, "MTK1", protocol.ClaimRoleConsume)
		c.keep(c.kd, 2, nil)
		if got := c.moveOff("mtk1", c.press, c.cart(c.press, c.kd, "MTK1-KD", nil)); got != "GRP" {
			t.Errorf("went to %s, want the group", got)
		}
	})
	t.Run("off no line position: its drop", func(t *testing.T) {
		t.Parallel()
		c := newMTCell(t, "MTK2", protocol.ClaimRoleConsume)
		c.keep(c.kd, 2, nil)
		if err := c.db.SetMaintainSupports(c.grp, []int64{c.press.ID, c.deck.ID, c.away.ID}); err != nil {
			t.Fatal(err)
		}
		if got := c.moveOff("mtk2", c.away, c.cart(c.away, c.kd, "MTK2-KD", nil)); got != c.out.Name {
			t.Errorf("went to %s, want the planned drop %s", got, c.out.Name)
		}
	})
	t.Run("a group at its level: its drop", func(t *testing.T) {
		t.Parallel()
		c := newMTCell(t, "MTK3", protocol.ClaimRoleConsume)
		c.keep(c.kd, 1, nil)
		slot, err := c.db.GetNodeByDotName("MTK3-GRP-1")
		testutil.MustNoErr(t, err, "slot")
		c.cart(slot, c.kd, "MTK3-RESIDENT", nil)
		if got := c.moveOff("mtk3", c.press, c.cart(c.press, c.kd, "MTK3-KD", nil)); got != c.out.Name {
			t.Errorf("went to %s, want the planned drop %s", got, c.out.Name)
		}
	})
}

// TestMaintainerArm_TwoShortGroupsTheLargestGap: two groups support the press
// and both are short of the knockdown; the one short by more takes it.
func TestMaintainerArm_TwoShortGroupsTheLargestGap(t *testing.T) {
	t.Parallel()
	c := newMTCell(t, "MTG", protocol.ClaimRoleConsume)
	c.keep(c.kd, 1, nil)
	big, err := c.db.CreateNodeGroup("MTG-BIG")
	testutil.MustNoErr(t, err, "second group")
	testdb.MarkGroup(t, c.db, big)
	testutil.MustNoErr(t, c.db.CreateNode(&nodes.Node{Name: "MTG-BIG-1", Enabled: true, ParentID: &big}), "slot")
	testutil.MustNoErr(t, c.db.SetMaintainSupports(big, []int64{c.press.ID}), "supports")
	testutil.MustNoErr(t, c.db.SetMaintainLevelAndLayerWants(store.MaintainLevel{GroupNodeID: big, BinTypeID: c.kd, Want: 3}, nil), "level")
	c.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "mtg", OrderType: OrderTypeMove,
		SourceNode: c.press.Name, DeliveryNode: c.out.Name, Quantity: 1, BinID: c.cart(c.press, c.kd, "MTG-KD", nil).ID})
	o, err := c.db.GetOrderByUUID("mtg")
	testutil.MustNoErr(t, err, "the move")
	if o.DeliveryNode != "MTG-BIG-1" {
		t.Errorf("went to %s, want MTG-BIG-1 (short 3, against 1)", o.DeliveryNode)
	}
}

// mtLeg writes the evac a release diverts: staged at its station wait on the
// press, lifting the press's bin to OUT, the bin claimed by it.
func (c mtCell) mtLeg(uuid string, b *bins.Bin) *orders.Order {
	c.t.Helper()
	steps := []resolvedStep{rlWait(c.press.Name, WaitKindStation), vsPick(c.press.Name), vsDrop(c.out.Name)}
	o := testdb.CreateOrder(c.t, c.db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = uuid, "line-1", OrderTypeComplex, StatusStaged
		o.ProcessNode, o.DeliveryNode, o.StepsJSON = c.press.Name, c.out.Name, string(mustJSON(c.t, steps))
	})
	testutil.MustNoErr(c.t, c.db.UpdateOrderVendor(o.ID, "V-"+uuid, "WAITING", ""), "vendor")
	testutil.MustNoErr(c.t, c.db.UpdateOrderBinID(o.ID, b.ID), "bind bin")
	_, err := c.db.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, o.ID, b.ID)
	testutil.MustNoErr(c.t, err, "claim bin for the leg")
	return o
}

// TestMaintainerArm_AtTheRelease: a consume evac's drained empty goes to the
// short group at its release; a produce evac's full keeps its planned drop.
func TestMaintainerArm_AtTheRelease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		role  protocol.ClaimRole
		count int
		want  string
	}{
		{"a consume evac's drained empty", protocol.ClaimRoleConsume, 0, "GRP"},
		{"a produce evac's full", protocol.ClaimRoleProduce, 100, "OUT"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cell := newMTCell(t, "MTR", c.role)
			cell.keep(cell.kd, 2, nil)
			b := cell.cart(cell.press, cell.kd, "MTR-BIN", nil)
			testutil.MustNoErr(t, cell.db.SetBinManifest(b.ID, `{"items":[]}`, cell.part, 40), "a count on it")
			o := cell.mtLeg("mtr-leg", b)
			count := c.count
			cell.d.HandleOrderRelease(cell.d.syntheticEnvelope("line-1"), &protocol.OrderRelease{OrderUUID: o.EdgeUUID,
				RemainingUOP: &count})
			got := prReload(t, cell.db, o.ID)
			steps, ok := decodeSteps(got.StepsJSON)
			if !ok {
				t.Fatalf("steps: %s", got.StepsJSON)
			}
			want := c.want
			if want == "OUT" {
				want = cell.out.Name
			}
			if w := cell.where(steps[2].Node); w != want {
				t.Errorf("the evac's drop is %s, want %s", w, want)
			}
		})
	}
}

// TestMaintainerArm_AtTheReserve: the tooling evac lifts at its first step,
// with no wait ahead of it, so no release can divert it. Its drop is
// re-pointed at the reserve, once its removal pickup has bound the empty; the
// keeper's count sees it coming before the confirm (no second ask), and the
// order goes to the fleet with the group's slot.
func TestMaintainerArm_AtTheReserve(t *testing.T) {
	t.Parallel()
	c := newMTCell(t, "MTV", protocol.ClaimRoleConsume)
	c.keep(c.kd, 1, nil)
	b := c.cart(c.press, c.kd, "MTV-KD", nil)
	prSubmitLeg(c.d, "mtv-evac", "", "", c.press.Name, prPick(c.press.Name), prDrop(c.out.Name))
	o, err := c.db.GetOrderByUUID("mtv-evac")
	testutil.MustNoErr(t, err, "the evac")
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatalf("steps: %s", o.StepsJSON)
	}

	// The reserve alone: no hand, no confirm.
	if _, _, st := c.d.reserveComplexSources(o, steps); st.done {
		t.Fatalf("the reserve stopped: %v", st.err)
	}
	pre := prReload(t, c.db, o.ID)
	if c.where(pre.DeliveryNode) != "GRP" {
		t.Fatalf("pre-confirm: the evac goes to %s, want the group", pre.DeliveryNode)
	}
	group, err := c.db.GetNode(c.grp)
	testutil.MustNoErr(t, err, "group")
	lc, err := c.db.CountLevel(group, store.MaintainLevel{GroupNodeID: c.grp, BinTypeID: c.kd, BinTypeCode: "MTV-KD", Want: 1},
		store.DemandOrigin{})
	testutil.MustNoErr(t, err, "the keeper's count")
	if lc.Coming != 1 || lc.Short() != 0 {
		t.Errorf("pre-confirm: the keeper counts coming %d, short %d; want 1 and 0: it must not ask again", lc.Coming, lc.Short())
	}

	prScanPass(t, c.d, c.db, "mtv-evac")
	sent := prReload(t, c.db, o.ID)
	if sent.VendorOrderID == "" || c.where(sent.DeliveryNode) != "GRP" {
		t.Errorf("the evac: %s vendor %q to %s, want sent to the group", sent.Status, sent.VendorOrderID, sent.DeliveryNode)
	}
	held, err := c.db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "the evac's bin")
	if held.ClaimedBy == nil || *held.ClaimedBy != o.ID {
		t.Errorf("the evac does not hold its bin (claimed by %v)", held.ClaimedBy)
	}
}
