//go:build docker

package dispatch

import (
	"encoding/json"
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

// vacant_pickup_docker_test.go — a press pair whose index leg names a vacant
// on-deck position: Core waits for a carrier coming to it, holds for a carrier
// named by a move taking it away, and otherwise fetches the carrier from the
// claim's inbound source (reanchorVacantPickup), for both roles, flipped and
// unflipped; and every pickup the fallback must leave alone.

// vpCell is one fed press cell, its claim mirrored for station line-1: the
// press, its on-deck (DECK), the claim's inbound source (IN, holding the
// carrier the fallback fetches), the node R1's refill fetches from (REFILL)
// and the outbound (OUT).
type vpCell struct {
	t                            *testing.T
	db                           *store.DB
	d                            *Dispatcher
	role                         protocol.ClaimRole
	part                         string
	binType                      int64
	press, deck, in, refill, out *nodes.Node
	inCarrier                    *bins.Bin
}

func newVPCell(t *testing.T, prefix string, role protocol.ClaimRole) vpCell {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	c := vpCell{t: t, db: db, d: d, role: role, part: sd.Payload.Code, binType: sd.BinType.ID,
		press: prNode(t, db, prefix+"-PRESS"), deck: prNode(t, db, prefix+"-DECK"),
		in: prNode(t, db, prefix+"-IN"), refill: prNode(t, db, prefix+"-REFILL"), out: prNode(t, db, prefix+"-OUT")}
	prResident(t, db, c.press, sd.BinType.ID, c.part, prefix+"-ON-PRESS")
	c.inCarrier = c.carrierAt(c.in, prefix+"-AT-IN")
	c.carrierAt(c.refill, prefix+"-AT-REFILL")
	c.claim(t, "", nil)
	return c
}

// carrierAt stands the carrier the role's index carries at node: an empty for
// produce, a full of the part for consume.
func (c vpCell) carrierAt(node *nodes.Node, label string) *bins.Bin {
	c.t.Helper()
	if c.role == protocol.ClaimRoleConsume {
		return prResident(c.t, c.db, node, c.binType, c.part, label)
	}
	return testdb.CreateBinAtNode(c.t, c.db, "", node.ID, label)
}

// reread is b as the store holds it now.
func (c vpCell) reread(b *bins.Bin) *bins.Bin {
	c.t.Helper()
	got, err := c.db.GetBin(b.ID)
	testutil.MustNoErr(c.t, err, "re-read "+b.Label)
	return got
}

// claim mirrors the press's claim; extra rows ride the same report.
func (c vpCell) claim(t *testing.T, secondPaired string, extra []plantclaims.ClaimRow) {
	t.Helper()
	rows := append([]plantclaims.ClaimRow{{
		ProcessID: "VP-PROC", StyleID: "VP-STYLE", CoreNodeName: c.press.Name, Role: c.role,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: c.part, InboundSource: c.in.Name,
		OutboundDestination: c.out.Name, PairedCoreNode: c.deck.Name, SecondPairedCoreNode: secondPaired,
	}}, extra...)
	_, err := c.db.ReplacePlantClaimsFromStation("VP-PROC", "line-1",
		[]plantclaims.StyleRow{{ProcessID: "VP-PROC", StyleID: "VP-STYLE", IsActive: true}}, rows, nil, 0)
	testutil.MustNoErr(t, err, "the press's claim")
}

// pickDeck is the index leg's on-deck pickup as the Edge's steady-state
// builder writes it: Empty-flagged on a produce press, plain on a consume one.
func (c vpCell) pickDeck() protocol.ComplexOrderStep {
	s := prPick(c.deck.Name)
	s.Empty = c.role == protocol.ClaimRoleProduce
	return s
}

func (c vpCell) pickRefill() protocol.ComplexOrderStep {
	s := prPick(c.refill.Name)
	s.Empty = c.role == protocol.ClaimRoleProduce
	return s
}

// submitPair sends the steady-state pair (R1 lifts the press and, unflipped,
// refills the on-deck; R2 indexes the on-deck onto the press and, flipped,
// refills it), each leg naming the partner it is given ("" for a blank link).
func (c vpCell) submitPair(prefix string, flipped bool, r1Names, r2Names string) (r1, r2 string) {
	r1, r2 = prefix+"-r1", prefix+"-r2"
	r1Steps := []protocol.ComplexOrderStep{prWait(c.press.Name), prPick(c.press.Name), prDrop(c.out.Name)}
	r2Steps := []protocol.ComplexOrderStep{prWait(c.deck.Name), c.pickDeck(), prDrop(c.press.Name)}
	if flipped {
		r2Steps = append(r2Steps, c.pickRefill(), prDrop(c.deck.Name))
	} else {
		r1Steps = append(r1Steps, c.pickRefill(), prDrop(c.deck.Name))
	}
	prSubmitLeg(c.d, r1, r1Names, c.part, c.press.Name, r1Steps...)
	prSubmitLeg(c.d, r2, r2Names, c.part, c.press.Name, r2Steps...)
	return r1, r2
}

func (c vpCell) scan(uuids ...string) {
	for range 3 {
		prScanPass(c.t, c.d, c.db, uuids...)
	}
}

// requireTook says the pair went to the fleet with the index leg's on-deck
// pickup moved to the carrier at want.
func (c vpCell) requireTook(r1, r2 string, want *nodes.Node) {
	c.t.Helper()
	l1, l2 := prReloadUUID(c.t, c.db, r1), prReloadUUID(c.t, c.db, r2)
	if l1.VendorOrderID == "" || l2.VendorOrderID == "" {
		c.t.Fatalf("the pair did not go: r1 %s code %q cause %q, r2 %s code %q cause %q", l1.Status, l1.QueueCode,
			l1.QueueCause, l2.Status, l2.QueueCode, l2.QueueCause)
	}
	var steps []protocol.ComplexOrderStep
	testutil.MustNoErr(c.t, json.Unmarshal([]byte(l2.StepsJSON), &steps), "r2 steps")
	if steps[1].Node != want.Name {
		c.t.Errorf("the index leg's on-deck pickup is at %s, want %s", steps[1].Node, want.Name)
	}
}

// requireHolds says both legs hold, unsent, on the code given.
func (c vpCell) requireHolds(code protocol.QueueCode, uuids ...string) {
	c.t.Helper()
	for _, u := range uuids {
		o := prReloadUUID(c.t, c.db, u)
		if o.VendorOrderID != "" || o.QueueCode != string(code) {
			c.t.Errorf("%s: %s vendor %q code %q cause %q, want held on %s", u, o.Status, o.VendorOrderID,
				o.QueueCode, o.QueueCause, code)
		}
	}
}

// TestVacantPickup_APairNeverWaitsOnItsOwnRefill: with nothing else coming to
// the vacant on-deck, the pair fetches the index leg's carrier from the claim's
// inbound source, never waiting on its own R1's refill to that position, for
// both roles, flipped and unflipped, and whichever side carries the sibling
// link (it is best-effort from the supply back to the evac). A consume pair
// fetches a full of the part.
func TestVacantPickup_APairNeverWaitsOnItsOwnRefill(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
		for _, flipped := range []bool{false, true} {
			for _, link := range []string{"both", "r1 only", "r2 only"} {
				name := string(role) + map[bool]string{false: "/unflipped", true: "/flipped"}[flipped] + "/" + link
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					c := newVPCell(t, "VP1", role)
					r1Names, r2Names := "VP1-r2", "VP1-r1"
					switch link {
					case "r1 only":
						r2Names = ""
					case "r2 only":
						r1Names = ""
					}
					r1, r2 := c.submitPair("VP1", flipped, r1Names, r2Names)
					c.scan(r1, r2)
					c.requireTook(r1, r2, c.in)
				})
			}
		}
	}
}

// liveOrder stands another live order for the coming read: placing at the
// on-deck by its steps (a complex leg) or its delivery node (a plain one).
func (c vpCell) liveOrder(uuid string, orderType protocol.OrderType, status protocol.Status, delivery string, steps []resolvedStep) *orders.Order {
	c.t.Helper()
	raw := ""
	if steps != nil {
		j, err := json.Marshal(steps)
		testutil.MustNoErr(c.t, err, "steps")
		raw = string(j)
	}
	o := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: orderType, Status: status, Quantity: 1,
		PayloadCode: c.part, DeliveryNode: delivery, ProcessNode: c.press.Name, StepsJSON: raw}
	testutil.MustNoErr(c.t, c.db.CreateOrder(o), "order "+uuid)
	return o
}

// TestVacantPickup_ACarrierComingIsWaitedFor: another live order placing a
// carrier at the vacant on-deck holds the pair, both legs on
// waiting_for_outside (parkPair copies the wait to the partner): a previous
// pair's refill still travelling, and a prime the market cannot fill yet
// (queued). Cancelled, it is no longer coming and the pair fetches its own.
func TestVacantPickup_ACarrierComingIsWaitedFor(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		make func(c vpCell) *orders.Order
	}{
		{"a previous pair's refill still travelling", func(c vpCell) *orders.Order {
			return c.liveOrder("prev-r1", protocol.OrderTypeComplex, protocol.StatusInTransit, c.deck.Name,
				prResolved(prWait(c.press.Name), prPick(c.press.Name), prDrop(c.out.Name), prPick(c.refill.Name), prDrop(c.deck.Name)))
		}},
		{"a prime the market cannot fill yet (queued)", func(c vpCell) *orders.Order {
			return c.liveOrder("prime", protocol.OrderTypeRetrieve, protocol.StatusQueued, c.deck.Name, nil)
		}},
		{"a complex leg whose delivery node is blank", func(c vpCell) *orders.Order {
			return c.liveOrder("blank-leg", protocol.OrderTypeComplex, protocol.StatusDispatched, "",
				prResolved(prPick(c.refill.Name), prDrop(c.deck.Name)))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			cell := newVPCell(t, "VP2", protocol.ClaimRoleProduce)
			coming := c.make(cell)
			r1, r2 := cell.submitPair("VP2", false, "VP2-r2", "VP2-r1")
			cell.scan(r1, r2)
			cell.requireHolds(protocol.QueueWaitingForOutside, r1, r2)

			_, err := cell.db.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, coming.ID)
			testutil.MustNoErr(t, err, "cancel the coming order")
			cell.scan(r1, r2)
			cell.requireTook(r1, r2, cell.in)
		})
	}
}

// TestVacantPickup_ALegThatOnlyLiftsThereIsNotComing: a live leg that lifts
// from the on-deck and sets nothing down there is not a carrier coming to it.
func TestVacantPickup_ALegThatOnlyLiftsThereIsNotComing(t *testing.T) {
	t.Parallel()
	c := newVPCell(t, "VP3", protocol.ClaimRoleProduce)
	c.liveOrder("lifter", protocol.OrderTypeComplex, protocol.StatusInTransit, c.out.Name,
		prResolved(prPick(c.deck.Name), prDrop(c.out.Name)))
	r1, r2 := c.submitPair("VP3", false, "VP3-r2", "VP3-r1")
	c.scan(r1, r2)
	c.requireTook(r1, r2, c.in)
}

// TestVacantPickup_ANamedMoveOffTheOutsideHoldsThePair is SEND BACK then
// REQUEST SWAP: the cart on the on-deck is the cart of a station's move
// taking it to the outbound, issued through the wire door, which holds it from
// the issue, so the index leg's find passes over it and the pair holds on
// waiting_for_outside. The move lifts and a carrier is coming: still
// waiting_for_outside. It lands: the pair takes it.
func TestVacantPickup_ANamedMoveOffTheOutsideHoldsThePair(t *testing.T) {
	t.Parallel()
	c := newVPCell(t, "VP4", protocol.ClaimRoleProduce)
	outside := testdb.CreateBinAtNode(t, c.db, "", c.deck.ID, "VP4-OUTSIDE")
	c.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "send-back-move",
		OrderType: protocol.OrderTypeMove, Quantity: 1, SourceNode: c.deck.Name, DeliveryNode: c.out.Name,
		BinID: outside.ID})
	move := prReloadUUID(t, c.db, "send-back-move")
	if move.BinID == nil || *move.BinID != outside.ID {
		t.Fatalf("fixture: SEND BACK's move holds %v, want its cart %d from its issue", move.BinID, outside.ID)
	}

	r1, r2 := c.submitPair("VP4", false, "VP4-r2", "VP4-r1")
	c.scan(r1, r2)
	c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)
	if b := c.reread(outside); b.ClaimedBy != nil {
		t.Fatalf("the named cart is claimed by %d; the index leg must leave it to its move", *b.ClaimedBy)
	}

	// The move is sent and claims its cart before it lifts it: the find no
	// longer returns the cart, and the pair still holds for outside material.
	_, err := c.db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, move.ID, outside.ID)
	testutil.MustNoErr(t, err, "the move claims its cart")
	_, err = c.db.Exec(`UPDATE orders SET status = 'dispatched' WHERE id = $1`, move.ID)
	testutil.MustNoErr(t, err, "the move is sent")
	c.scan(r1, r2)
	c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)

	// The move lifts it; the pull bringing tops & bottoms is coming.
	_, err = c.db.Exec(`UPDATE bins SET node_id = $1, claimed_by = NULL WHERE id = $2`, c.out.ID, outside.ID)
	testutil.MustNoErr(t, err, "the move lifts the cart")
	_, err = c.db.Exec(`UPDATE orders SET status = 'in_transit' WHERE id = $1`, move.ID)
	testutil.MustNoErr(t, err, "the move travels")
	pull := c.liveOrder("pull", protocol.OrderTypeRetrieve, protocol.StatusDispatched, c.deck.Name, nil)
	c.scan(r1, r2)
	c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)

	// It lands.
	landed := testdb.CreateBinAtNode(t, c.db, "", c.deck.ID, "VP4-LANDED")
	_, err = c.db.Exec(`UPDATE orders SET status = 'confirmed' WHERE id IN ($1, $2)`, move.ID, pull.ID)
	testutil.MustNoErr(t, err, "both land")
	c.scan(r1, r2)
	c.requireTook(r1, r2, c.deck)
	if b := c.reread(landed); b.ClaimedBy == nil {
		t.Error("the landed carrier is not the index leg's")
	}
}

// TestVacantPickup_SendBackTakesThePairsHoldOutLoud: the pair's index leg has
// promised itself the on-deck's cart (a pending hold, before its hand). SEND
// BACK's move for that cart is a station's act, so a person's: its issue takes
// the pair's hold and holds the cart itself, and the pair, finding the cart
// held on its next pass, waits for outside material.
func TestVacantPickup_SendBackTakesThePairsHoldOutLoud(t *testing.T) {
	t.Parallel()
	c := newVPCell(t, "VP9", protocol.ClaimRoleProduce)
	cart := testdb.CreateBinAtNode(t, c.db, "", c.deck.ID, "VP9-ON-DECK")
	r1, r2 := c.submitPair("VP9", false, "VP9-r2", "VP9-r1")
	testdb.ReserveBin(t, c.db, prReloadUUID(t, c.db, r2).ID, cart.ID)

	c.d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: "vp9-send-back",
		OrderType: protocol.OrderTypeMove, Quantity: 1, SourceNode: c.deck.Name, DeliveryNode: c.out.Name,
		BinID: cart.ID})
	move := prReloadUUID(t, c.db, "vp9-send-back")
	var holder int64
	testutil.MustNoErr(t, c.db.QueryRow(`SELECT order_id FROM reservations WHERE bin_id = $1 AND state = 'pending'`,
		cart.ID).Scan(&holder), "the cart's hold")
	if holder != move.ID {
		t.Fatalf("the cart is held by order %d, want SEND BACK's move %d: a station's move takes a pending hold", holder, move.ID)
	}
	c.scan(r1, r2)
	c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)
}

// TestVacantPickup_ACarrierBeingTakenOffHoldsForOutside: the carrier on the
// on-deck is named by a live move taking it away (a wrong carrier's clear,
// SEND BACK's move) that has been sent and claims it, so the find no longer
// returns it; the pair holds on waiting_for_outside, never waiting_for_material,
// for both roles, flipped and unflipped. And a carrier the produce filter
// refuses (it holds the part) holds the pair on waiting_for_material while
// nothing is taking it (an unknown seat's residual), then on
// waiting_for_outside once its clear is live, before the clear is sent.
//
// MUTATION: drop missPickup's miss-with-bins arm and every row but the first
// filter one reads waiting_for_material.
func TestVacantPickup_ACarrierBeingTakenOffHoldsForOutside(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
		for _, flipped := range []bool{false, true} {
			t.Run(string(role)+map[bool]string{false: "/unflipped", true: "/flipped"}[flipped], func(t *testing.T) {
				t.Parallel()
				c := newVPCell(t, "VP7", role)
				cart := testdb.CreateBinAtNode(t, c.db, "", c.deck.ID, "VP7-ON-DECK")
				move := &orders.Order{EdgeUUID: "vp7-move", StationID: "line-1", OrderType: protocol.OrderTypeMove,
					Status: protocol.StatusDispatched, Quantity: 1, SourceNode: c.deck.Name, DeliveryNode: c.out.Name,
					NamedBinID: &cart.ID}
				testutil.MustNoErr(t, c.db.CreateOrder(move), "the named move, sent")
				_, err := c.db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, move.ID, cart.ID)
				testutil.MustNoErr(t, err, "the move claims its cart")
				r1, r2 := c.submitPair("VP7", flipped, "VP7-r2", "VP7-r1")
				c.scan(r1, r2)
				c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)
			})
		}
	}
	t.Run("produce/a carrier the filter refuses", func(t *testing.T) {
		t.Parallel()
		c := newVPCell(t, "VP8", protocol.ClaimRoleProduce)
		full := prResident(t, c.db, c.deck, c.binType, c.part, "VP8-FULL-ON-DECK")
		r1, r2 := c.submitPair("VP8", false, "VP8-r2", "VP8-r1")
		c.scan(r1, r2)
		c.requireHolds(protocol.QueueWaitingForMaterial, r1, r2)
		clearMove := &orders.Order{EdgeUUID: "vp8-clear", StationID: "line-1", OrderType: protocol.OrderTypeMove,
			Status: protocol.StatusQueued, Quantity: 1, SourceNode: c.deck.Name, DeliveryNode: c.out.Name,
			NamedBinID: &full.ID}
		testutil.MustNoErr(t, c.db.CreateOrder(clearMove), "its clear")
		c.scan(r1, r2)
		c.requireHolds(protocol.QueueWaitingForOutside, r1, r2)
	})
}

// TestVacantPickup_TheRefillFromTheSameSourceKeepsItsCarrier: the claim's
// inbound source is a group of two empties, the on-deck is vacant with nothing
// coming, and the pair's refill of the on-deck draws from that group too
// (flipped, the index leg's own later pickup; unflipped, the other leg's),
// already resolved to the slot the fallback's finder ranks first. The
// fallback leaves that carrier to the refill and takes the other, and the pair
// goes. Before, it took the refill's carrier and the pair held its partial set
// for good (the press tranche's sim, 2026-10-23, run 2, PRESS-1).
//
// MUTATION: answer reserveComplexPlan's misses in step order, inline with the
// finds, and flipped holds (its fallback runs before its own refill reserves).
// Unflipped goes either way: the outside leg leads the pair's pass and reserves
// its refill first.
func TestVacantPickup_TheRefillFromTheSameSourceKeepsItsCarrier(t *testing.T) {
	t.Parallel()
	for _, flipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unflipped", true: "flipped"}[flipped], func(t *testing.T) {
			t.Parallel()
			c := newVPCell(t, "VP9", protocol.ClaimRoleProduce)
			grpID, err := c.db.CreateNodeGroup("VP9-EMPTIES")
			testutil.MustNoErr(t, err, "the inbound group")
			testdb.MarkGroup(t, c.db, grpID)
			grp, err := c.db.GetNode(grpID)
			testutil.MustNoErr(t, err, "read the group")
			slots := map[string]bool{}
			var first string
			for i := 1; i <= 2; i++ {
				s := &nodes.Node{Name: fmt.Sprintf("VP9-EMPTIES-%d", i), Enabled: true, ParentID: &grpID}
				testutil.MustNoErr(t, c.db.CreateNode(s), "slot")
				// An empty of the claim's carrier, as the refill's group
				// resolution asks for (it reads the carrier type the leg carries).
				e := &bins.Bin{BinTypeID: c.binType, Label: fmt.Sprintf("VP9-E%d", i), NodeID: &s.ID, Status: "available"}
				testutil.MustNoErr(t, c.db.CreateBin(e), "an empty in the group")
				slots[s.Name] = true
				if first == "" {
					first = s.Name
				}
			}
			c.in = grp
			c.claim(t, "", nil)

			// The refill arrives resolved, as the plant's did (run 2's order 390:
			// the group's slot its finder ranks first), before the fallback runs.
			refill := prPick(first)
			refill.Empty = true
			r1Steps := []protocol.ComplexOrderStep{prWait(c.press.Name), prPick(c.press.Name), prDrop(c.out.Name)}
			r2Steps := []protocol.ComplexOrderStep{prWait(c.deck.Name), c.pickDeck(), prDrop(c.press.Name)}
			if flipped {
				r2Steps = append(r2Steps, refill, prDrop(c.deck.Name))
			} else {
				r1Steps = append(r1Steps, refill, prDrop(c.deck.Name))
			}
			prSubmitLeg(c.d, "VP9-r1", "VP9-r2", c.part, c.press.Name, r1Steps...)
			prSubmitLeg(c.d, "VP9-r2", "VP9-r1", c.part, c.press.Name, r2Steps...)
			for range 3 {
				c.scan("VP9-r1", "VP9-r2")
			}

			l1, l2 := prReloadUUID(t, c.db, "VP9-r1"), prReloadUUID(t, c.db, "VP9-r2")
			if l1.VendorOrderID == "" || l2.VendorOrderID == "" {
				t.Fatalf("the pair did not go: r1 %s %q %q, r2 %s %q %q", l1.Status, l1.QueueCode, l1.QueueReason,
					l2.Status, l2.QueueCode, l2.QueueReason)
			}
			var s1, s2 []protocol.ComplexOrderStep
			testutil.MustNoErr(t, json.Unmarshal([]byte(l1.StepsJSON), &s1), "r1 steps")
			testutil.MustNoErr(t, json.Unmarshal([]byte(l2.StepsJSON), &s2), "r2 steps")
			refills := s1
			if flipped {
				refills = s2
			}
			index, refilled := s2[1].Node, refills[3].Node
			if !slots[index] || !slots[refilled] || index == refilled {
				t.Errorf("the index leg took the carrier at %s and the refill the one at %s; want one each from %v",
					index, refilled, slots)
			}
		})
	}
}

// TestVacantPickup_PickupsTheFallbackLeavesAlone: each of these holds at the
// vacant position (waiting_for_material), never fetched from the inbound
// source: a pickup that names its part on the step (a changeover's index leg),
// a pickup that names its bin, and a sequential cell's partner position.
func TestVacantPickup_PickupsTheFallbackLeavesAlone(t *testing.T) {
	t.Parallel()
	t.Run("a changeover's index leg names the part it lifts", func(t *testing.T) {
		t.Parallel()
		c := newVPCell(t, "VP5", protocol.ClaimRoleProduce)
		pick := prPick(c.deck.Name)
		pick.PayloadCode = c.part
		prSubmitLeg(c.d, "co-r2", "", c.part, c.press.Name, prWait(c.deck.Name), pick, prDrop(c.press.Name))
		c.scan("co-r2")
		c.requireHolds(protocol.QueueWaitingForMaterial, "co-r2")
	})
	t.Run("a pickup that names its bin", func(t *testing.T) {
		t.Parallel()
		c := newVPCell(t, "VP6", protocol.ClaimRoleProduce)
		// The cart the pickup names stands on the on-deck when the order is
		// made (intake sends a named pickup to wherever its bin stands), and
		// leaves before the reserve: the pickup holds for the cart it names.
		named := testdb.CreateBinAtNode(t, c.db, "", c.deck.ID, "VP6-NAMED")
		pick := c.pickDeck()
		pick.BinID = named.ID
		prSubmitLeg(c.d, "named-r2", "", c.part, c.press.Name, prWait(c.deck.Name), pick, prDrop(c.press.Name))
		_, err := c.db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, c.out.ID, named.ID)
		testutil.MustNoErr(t, err, "the named cart leaves the on-deck")
		c.scan("named-r2")
		c.requireHolds(protocol.QueueWaitingForMaterial, "named-r2")
		if b := c.reread(c.inCarrier); b.ClaimedBy != nil {
			t.Errorf("the inbound source's carrier is claimed by %d: a named pickup fetched another", *b.ClaimedBy)
		}
	})
	t.Run("a sequential partner", func(t *testing.T) {
		t.Parallel()
		c := newVPCell(t, "VP7", protocol.ClaimRoleProduce)
		// The on-deck is a core node of the same style: a sequential pair.
		c.claim(t, "", []plantclaims.ClaimRow{{ProcessID: "VP-PROC", StyleID: "VP-STYLE", CoreNodeName: c.deck.Name,
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeSequential, PayloadCode: c.part,
			InboundSource: c.in.Name, OutboundDestination: c.out.Name, PairedCoreNode: c.press.Name, Seq: 1}})
		prSubmitLeg(c.d, "seq-leg", "", c.part, c.press.Name, prWait(c.deck.Name), c.pickDeck(), prDrop(c.press.Name))
		c.scan("seq-leg")
		c.requireHolds(protocol.QueueWaitingForMaterial, "seq-leg")
	})
}

// TestVacantPickup_TheSecondPairedPositionToo: on a three-position press the
// back position is a dependent position as well.
func TestVacantPickup_TheSecondPairedPositionToo(t *testing.T) {
	t.Parallel()
	c := newVPCell(t, "VP8", protocol.ClaimRoleProduce)
	back := prNode(t, c.db, "VP8-BACK")
	c.claim(t, back.Name, nil)
	prSubmitLeg(c.d, "back-leg", "", c.part, c.press.Name, prWait(back.Name),
		protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: back.Name, Empty: true}, prDrop(c.deck.Name))
	c.scan("back-leg")
	o := prReloadUUID(t, c.db, "back-leg")
	var steps []protocol.ComplexOrderStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "steps")
	if o.VendorOrderID == "" || steps[1].Node != c.in.Name {
		t.Errorf("the back position's pickup: %s vendor %q at %s, want sent from %s", o.Status, o.VendorOrderID, steps[1].Node, c.in.Name)
	}
}

// TestVacantPickup_TheStampedOnDeckPickupBindsAsBefore: the step payload a
// changeover's index leg now carries on its on-deck pickup is the order's own
// part, so the bin Core binds there is the one it bound without it.
func TestVacantPickup_TheStampedOnDeckPickupBindsAsBefore(t *testing.T) {
	t.Parallel()
	for _, stamped := range []bool{false, true} {
		t.Run(map[bool]string{false: "as before", true: "stamped"}[stamped], func(t *testing.T) {
			t.Parallel()
			c := newVPCell(t, "VP9", protocol.ClaimRoleProduce)
			standing := testdb.CreateBinAtNode(t, c.db, "", c.deck.ID, "VP9-STANDING")
			pick := prPick(c.deck.Name)
			if stamped {
				pick.PayloadCode = c.part
			}
			prSubmitLeg(c.d, "co-r2", "", c.part, c.press.Name, prWait(c.deck.Name), pick, prDrop(c.press.Name))
			c.scan("co-r2")
			o := prReloadUUID(t, c.db, "co-r2")
			b, err := c.db.GetBin(standing.ID)
			testutil.MustNoErr(t, err, "the standing carrier")
			if o.VendorOrderID == "" || b.ClaimedBy == nil || *b.ClaimedBy != o.ID {
				t.Errorf("%s vendor %q, standing carrier claimed by %v: want the leg sent with it", o.Status, o.VendorOrderID, b.ClaimedBy)
			}
		})
	}
}
