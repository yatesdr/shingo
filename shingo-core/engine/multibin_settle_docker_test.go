//go:build docker

package engine

import (
	"shingo/protocol"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// settleFixture builds the shape the whole-order settle actually runs on: ONE
// complex order with two junction rows — a leg whose bin is already sitting at
// its destination, and a leg still in flight at _TRANSIT.
//
// That pairing is the swap, and it is not contrived. A swap's intermediate
// dropoff is recorded early, at the block report, by ApplyIntermediateStore —
// which deliberately KEEPS the claim because the order is coming back for the
// bin. The final leg is deferred to whole-order FINISHED. So by the time this
// settle runs, one of the order's bins is genuinely placed and claimed, and the
// other genuinely is not.
func settleFixture(t *testing.T, db *store.DB, tag string) (order *orders.Order, landed, inFlight *bins.Bin, destA, destB *nodes.Node) {
	t.Helper()
	bt := &bins.BinType{Code: tag + "-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT (migration v15)")

	src := &nodes.Node{Name: tag + "-SRC", Enabled: true}
	destA = &nodes.Node{Name: tag + "-DEST-A", Enabled: true}
	destB = &nodes.Node{Name: tag + "-DEST-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	testutil.MustNoErr(t, db.CreateNode(destA), "create destA")
	testutil.MustNoErr(t, db.CreateNode(destB), "create destB")

	landed = &bins.Bin{BinTypeID: bt.ID, Label: tag + "-LANDED", NodeID: &destA.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(landed), "create landed bin")
	inFlight = &bins.Bin{BinTypeID: bt.ID, Label: tag + "-INFLIGHT", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(inFlight), "create in-flight bin")

	order = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.SourceNode = src.Name
		o.DeliveryNode = destB.Name
		o.Status = "delivered"
	})
	testdb.ClaimBinForTest(t, db, landed.ID, order.ID)
	testdb.ClaimBinForTest(t, db, inFlight.ID, order.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, landed.ID, 3, "dropoff", src.Name, destA.Name),
		"junction row: the already-placed leg")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, inFlight.ID, 5, "dropoff", src.Name, destB.Name),
		"junction row: the in-flight leg")
	return order, landed, inFlight, destA, destB
}

// TestMultiBinSettle_RefusalWritesNothing is the R.26 assert: a settlement that
// finds one leg's bin no longer belonging to the order commits NOTHING.
//
// The old shape placed the bins that passed, returned the refusals, and let
// handleOrderDelivered fail the order — so a swap lost half a delivery into the
// ledger and Edge was never told about the bin that landed (D4). Wrong under every
// disposition the round considered, which is why the mechanics were fixed
// regardless of how the policy landed.
//
// The policy landed on a plant fact: digs work LANES and never reclaim a leg of
// another process's in-flight order, so a foreign bin inside a settlement is not a
// race the design permits — it is an integrity failure. Assert, do not compensate.
//
// The GOOD bin staying put is the whole assertion. It is the one that would have
// been written under the old shape, and its position is the only durable evidence
// that nothing was half-committed.
//
// MUTATION: delete the `if len(refusals) > 0 { return refusals }` guard and the
// good bin lands — a partial settlement, exactly as before.
func TestMultiBinSettle_RefusalWritesNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())

	bt := &bins.BinType{Code: "PARTIAL-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT")
	src := &nodes.Node{Name: "PARTIAL-SRC", Enabled: true}
	destA := &nodes.Node{Name: "PARTIAL-DEST-A", Enabled: true}
	destB := &nodes.Node{Name: "PARTIAL-DEST-B", Enabled: true}
	for _, n := range []*nodes.Node{src, destA, destB} {
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
	}

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.SourceNode = src.Name
		o.DeliveryNode = destB.Name
		o.Status = "delivered"
	})

	// Leg one: genuinely this order's bin, in flight, ready to settle.
	good := &bins.Bin{BinTypeID: bt.ID, Label: "PARTIAL-GOOD", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(good), "create good bin")
	testdb.ClaimBinForTest(t, db, good.ID, order.ID)

	// Leg two: the corruption. A bin this order's junction row still names, held by
	// somebody else and sitting nowhere near the destination.
	foreign := &bins.Bin{BinTypeID: bt.ID, Label: "PARTIAL-FOREIGN", NodeID: &src.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(foreign), "create foreign bin")
	// THE RIGHTFUL OWNER MUST BE LIVE *AND* INVISIBLE TO THE SCANNER, and the
	// second half is what this fixture was missing.
	//
	// testdb.CreateOrder defaults to `queued` with no payload code. `queued` is in
	// the acquiring set, so the running engine's fulfillment scanner picks the
	// order up, finds it has no payload to source against, and STRUCTURALLY FAILS
	// it — and failing an order releases its bin claims. The assertion at the end
	// of this test then reads a nil claim and reports that the carrier stole it,
	// which is not what happened at all.
	//
	// It is a race, not a certainty: it only bites when the scanner wins before the
	// assertion. Locally it never did; CI under -race is slower and it did (job
	// 94088537674 at 35445099, "order 1 failed: structural - order has empty
	// payload_code"). Latent since this fixture was written.
	//
	// `in_transit` fixes it by being MORE faithful rather than less: a bin claimed
	// by a live order that is not acquiring is exactly the real situation this test
	// describes — a robot already en route to it — and it is outside
	// IsAcquiring{queued, sourcing}, so nothing sweeps it.
	thief := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	if protocol.IsAcquiring(thief.Status) {
		t.Fatalf("fixture: the rightful owner is %q, which is in the acquiring set — the fulfillment "+
			"scanner will structurally fail it and release the very claim this test asserts on. That "+
			"is a RACE, so it will pass locally and flake in CI; keep this order out of {queued, "+
			"sourcing}", thief.Status)
	}
	testdb.ClaimBinForTest(t, db, foreign.ID, thief.ID)

	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, good.ID, 0, "pickup", src.Name, destA.Name), "row: good")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, foreign.ID, 2, "pickup", src.Name, destB.Name), "row: foreign")

	obs, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction rows")

	refusals := eng.applyMultiBinArrivalForOrder(order, obs)
	if len(refusals) != 1 {
		t.Fatalf("refusals = %d, want 1 — the foreign leg must be refused", len(refusals))
	}

	gotGood, err := db.GetBin(good.ID)
	testutil.MustNoErr(t, err, "reload the good bin")
	if gotGood.NodeID == nil || *gotGood.NodeID != transit.ID {
		t.Errorf("the good bin moved to %v — a settlement with a refused leg must write NOTHING, or "+
			"the order fails having already recorded half a delivery nobody was told about (D4)",
			gotGood.NodeID)
	}
	if gotGood.ClaimedBy == nil || *gotGood.ClaimedBy != order.ID {
		t.Errorf("the good bin's claim = %v, want order %d — nothing committed means the claim is "+
			"untouched too", gotGood.ClaimedBy, order.ID)
	}

	// And the foreign bin is not stolen on the way out.
	gotForeign, err := db.GetBin(foreign.ID)
	testutil.MustNoErr(t, err, "reload the foreign bin")
	if gotForeign.ClaimedBy == nil || *gotForeign.ClaimedBy != thief.ID {
		t.Errorf("the foreign bin's claim = %v, want its real owner order %d",
			gotForeign.ClaimedBy, thief.ID)
	}

	// THE REGRESSION GUARD, in the same shape minus the corruption: two in-flight
	// legs of one order settle exactly as before.
	healthy := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.SourceNode = src.Name
		o.DeliveryNode = destB.Name
		o.Status = "delivered"
	})
	one := &bins.Bin{BinTypeID: bt.ID, Label: "PARTIAL-OK1", NodeID: &transit.ID, Status: "available"}
	two := &bins.Bin{BinTypeID: bt.ID, Label: "PARTIAL-OK2", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(one), "create ok1")
	testutil.MustNoErr(t, db.CreateBin(two), "create ok2")
	testdb.ClaimBinForTest(t, db, one.ID, healthy.ID)
	testdb.ClaimBinForTest(t, db, two.ID, healthy.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(healthy.ID, one.ID, 0, "pickup", src.Name, destA.Name), "row: ok1")
	testutil.MustNoErr(t, db.InsertOrderBin(healthy.ID, two.ID, 2, "pickup", src.Name, destB.Name), "row: ok2")

	healthyObs, err := db.ListOrderBins(healthy.ID)
	testutil.MustNoErr(t, err, "list healthy junction rows")
	if rs := eng.applyMultiBinArrivalForOrder(healthy, healthyObs); len(rs) != 0 {
		t.Fatalf("healthy settlement refused %d legs, want 0", len(rs))
	}
	for _, want := range []struct {
		bin  *bins.Bin
		node *nodes.Node
	}{{one, destA}, {two, destB}} {
		got, err := db.GetBin(want.bin.ID)
		testutil.MustNoErr(t, err, "reload settled bin")
		if got.NodeID == nil || *got.NodeID != want.node.ID {
			t.Errorf("bin %s is at %v, want %s — the healthy settlement must land both legs exactly "+
				"as before", want.bin.Label, got.NodeID, want.node.Name)
		}
	}
}

// TestMultiBinSettle_UnsetStatusDoesNotDeleteTheJunction is D3, at the site that
// destroys data.
//
// The completion handler deletes an order's junction rows once the order is
// terminal — correct, and the natural cleanup point. But it asked with a bare
// protocol.IsTerminal, and IsTerminal("") is TRUE, so an order whose status did
// not load read as finished and its per-bin destinations were thrown away.
//
// Those are the rows whose absence made two specimens unreconstructable after
// the fact (PLAN §R.5's bin 17, §R.9's bin 20): order_bins is deleted at
// terminal and there is no bin-position history, so once they are gone the
// per-bin destinations cannot be recovered from anything. An order that could
// not be read is the last one whose evidence should be discarded.
//
// MUTATION: put the bare protocol.IsTerminal(order.Status) back and this fires —
// the rows are gone.
func TestMultiBinSettle_UnsetStatusDoesNotDeleteTheJunction(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())

	order, _, _, _, _ := settleFixture(t, db, "D3JUNC")
	// The zero-value status: a row that did not load, or loaded partially. Written
	// straight to the column so the struct and the DB agree on the bad state.
	if _, err := db.Exec(`UPDATE orders SET status='' WHERE id=$1`, order.ID); err != nil {
		t.Fatalf("force unset status: %v", err)
	}
	order.Status = ""

	obs, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction rows before")
	if len(obs) != 2 {
		t.Fatalf("junction rows before = %d, want 2", len(obs))
	}

	eng.handleMultiBinCompleted(order, obs)

	after, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction rows after")
	if len(after) != 2 {
		t.Fatalf("junction rows after = %d, want 2 — an order whose status could not be read was "+
			"treated as terminal and its per-bin destinations were deleted, which is exactly the "+
			"evidence two prior specimens needed and did not have (D3)", len(after))
	}

	// And the control: a genuinely terminal order still gets cleaned up, or the
	// fix would trade a data-loss bug for a junction-row leak.
	live, _, _, _, _ := settleFixture(t, db, "D3TERM")
	if _, err := db.Exec(`UPDATE orders SET status='confirmed' WHERE id=$1`, live.ID); err != nil {
		t.Fatalf("set confirmed: %v", err)
	}
	live.Status = "confirmed"
	liveObs, err := db.ListOrderBins(live.ID)
	testutil.MustNoErr(t, err, "list terminal order's rows")

	eng.handleMultiBinCompleted(live, liveObs)

	remaining, err := db.ListOrderBins(live.ID)
	testutil.MustNoErr(t, err, "list after terminal completion")
	if len(remaining) != 0 {
		t.Errorf("junction rows after a terminal completion = %d, want 0 — the cleanup must still "+
			"happen for an order that really is finished", len(remaining))
	}
}

// TestMultiBinSettle_DriftTripwireIsWiredAtBothSites proves the counter is
// actually reached from the two settle paths, which the unit tests next to the
// tripwire cannot: they call it directly.
//
// Two call sites, one line each, and a line that is never called is the whole
// failure mode of an instrument. The per-site split is the point of asserting
// both — this handler fires on (X → delivered) and again on (delivered →
// confirmed), so "drifted at delivery" and "still drifted when the safety net
// looked" are different facts and the tally keeps them apart.
//
// MUTATION: delete either noteDestNodeDrift call and that site's count is 0.
func TestMultiBinSettle_DriftTripwireIsWiredAtBothSites(t *testing.T) {
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())
	resetDestNodeDriftTally()
	t.Cleanup(resetDestNodeDriftTally)

	bt := &bins.BinType{Code: "DRIFTW-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT")
	press := &nodes.Node{Name: "DRIFTW-PRESS", Enabled: true}
	empties := &nodes.Node{Name: "DRIFTW-EMPTIES", Enabled: true}
	slot := &nodes.Node{Name: "DRIFTW-SLOT", Enabled: true}
	stale := &nodes.Node{Name: "DRIFTW-STALE", Enabled: true}
	for _, n := range []*nodes.Node{press, empties, slot, stale} {
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
	}

	full := &bins.Bin{BinTypeID: bt.ID, Label: "DRIFTW-FULL", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(full), "create full bin")
	empty := &bins.Bin{BinTypeID: bt.ID, Label: "DRIFTW-EMPTY", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(empty), "create empty bin")

	// The plan sends the full bin to the slot and the empty back to the press.
	plan := `[{"action":"pickup","node":"DRIFTW-PRESS"},` +
		`{"action":"dropoff","node":"DRIFTW-SLOT"},` +
		`{"action":"pickup","node":"DRIFTW-EMPTIES","empty":true},` +
		`{"action":"dropoff","node":"DRIFTW-PRESS"}]`

	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.SourceNode = press.Name
		o.DeliveryNode = press.Name
		o.Status = "delivered"
		o.StepsJSON = plan
	})
	if _, err := db.Exec(`UPDATE orders SET steps_json=$2 WHERE id=$1`, order.ID, plan); err != nil {
		t.Fatalf("persist plan: %v", err)
	}
	order, err = db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "reload order")

	testdb.ClaimBinForTest(t, db, full.ID, order.ID)
	testdb.ClaimBinForTest(t, db, empty.ID, order.ID)
	// The full bin's row agrees with the plan. The empty's row is STALE — it still
	// names a slot, while the plan takes the empty back to the press. That is D2's
	// shape: a writer moved the plan and left the projection behind.
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, full.ID, 0, "pickup", press.Name, slot.Name), "row: full")
	testutil.MustNoErr(t, db.InsertOrderBin(order.ID, empty.ID, 2, "pickup", empties.Name, stale.Name), "row: empty")

	obs, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction rows")

	eng.applyMultiBinArrivalForOrder(order, obs)
	if n := DestNodeDriftTally()[driftSiteDelivery]; n != 1 {
		t.Errorf("%s = %d, want 1 — the delivery-time settle must read the tripwire before it places",
			driftSiteDelivery, n)
	}

	eng.handleMultiBinCompleted(order, obs)
	if n := DestNodeDriftTally()[driftSiteCompleted]; n != 1 {
		t.Errorf("%s = %d, want 1 — the completion-time settle must read it too, under its own key",
			driftSiteCompleted, n)
	}
}

// TestMultiBinSettle_LandedBinGetsNoInstruction pins the arrived check at the
// DELIVERY-TIME settle, and the regression guard beside it.
//
// The completion-time sibling has asked "did it already land?" before asking
// about ownership since cb7ed41d, and the reasoning is written out at
// reapplyRefused. This loop asked nothing: it built an instruction for every
// junction row and re-placed every bin at order_bins.dest_node — a value written
// once at allocation and updated by nothing (D2). That is the loop that can write
// over a position the fleet already reported.
//
// It also removes a benign shape from this site's refusal tally. On a repeat
// `delivered` event — the at-least-once shape — the first settle has already
// released the claim, so an ordinary completed delivery reached refuseArrival
// with claimed_by = nil and was logged as a refusal against an instrument whose
// whole value is reading zero. That is the same mistake the completion site made
// 121 times in a nine-minute soak.
//
// SKIPPING DOES NOT STRAND THE CLAIM, which is the thing worth checking before
// believing this is safe: TerminalizeOrderWithReason clears claimed_by for every
// bin this order holds, unconditionally across every terminal, and stamps
// anomaly_at only on bins still parked at _TRANSIT — which a landed bin is not.
//
// MUTATION: delete the binAlreadyAt skip and the landed bin's claim assertion
// fires — the settle re-places it and unclaims it.
func TestMultiBinSettle_LandedBinGetsNoInstruction(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())

	order, landed, inFlight, destA, destB := settleFixture(t, db, "SETTLE1")

	obs, err := db.ListOrderBins(order.ID)
	testutil.MustNoErr(t, err, "list junction rows")
	if len(obs) != 2 {
		t.Fatalf("junction rows = %d, want 2", len(obs))
	}

	refusals := eng.applyMultiBinArrivalForOrder(order, obs)
	if len(refusals) != 0 {
		t.Fatalf("refusals = %d, want 0 — both legs are this order's own bins", len(refusals))
	}
	// The RETURNED slice is what this test may assert on. ArrivalRefusalTally is a
	// process-global instrument that every parallel sibling writes to, so a test
	// that reads it is asserting on the rest of the suite — which is how this file
	// first went red.

	// THE LANDED LEG — no instruction was built for it, so the settle neither
	// re-placed it nor released its claim. The retained claim is the observable:
	// ApplyMultiBinArrival unclaims every bin it touches, so a claim that survives
	// proves the bin was never in the batch.
	gotLanded, err := db.GetBin(landed.ID)
	testutil.MustNoErr(t, err, "reload the landed bin")
	if gotLanded.NodeID == nil || *gotLanded.NodeID != destA.ID {
		t.Errorf("landed bin is at %v, want destA (%d) — it must not be moved by a settle that "+
			"had nothing to place", gotLanded.NodeID, destA.ID)
	}
	if gotLanded.ClaimedBy == nil || *gotLanded.ClaimedBy != order.ID {
		t.Errorf("landed bin claim = %v, want order %d still holding it — the settle skipped this bin "+
			"entirely, and TerminalizeOrderWithReason is what releases it", gotLanded.ClaimedBy, order.ID)
	}

	// THE REGRESSION GUARD. The leg that has NOT landed settles exactly as before:
	// placed at its destination, claim released, in the same call.
	gotFlight, err := db.GetBin(inFlight.ID)
	testutil.MustNoErr(t, err, "reload the in-flight bin")
	if gotFlight.NodeID == nil || *gotFlight.NodeID != destB.ID {
		t.Errorf("in-flight bin is at %v, want destB (%d) — the healthy leg must still settle",
			gotFlight.NodeID, destB.ID)
	}
	if gotFlight.ClaimedBy != nil {
		t.Errorf("in-flight bin still claimed by %d — a settled bin releases its claim in the arrival tx",
			*gotFlight.ClaimedBy)
	}
}

// TestMultiBinSettle_CompoundChildScopesItsUnclaim is THE ONE TEST that settles
// whether store/order_bins.go's unclaim is guarded-by-caller or live. The
// source-of-truth round split on it — one reading called it a drifted copy of
// the pre-445f79eb shape, the other called it safe by caller precondition — and
// a dispute about what a caller permits is answered by running the caller, not
// by reading the SQL. Reading the SQL only ever answers "is this UPDATE
// scoped", which is a narrower question that looks exactly like the one asked.
//
// SO IT RUNS THE CALLER. applyMultiBinArrivalForOrder is the delivery-time
// settle, and its guard is refuseArrival.
//
// THE VERDICT IS: BOTH READINGS WERE PARTLY RIGHT, AND IT IS LIVE.
//
// For an ORDINARY order the caller precondition is complete. refuseArrival
// refuses any bin whose claimed_by is not that order, and the R.26 assert
// commits NOTHING when any leg is refused — so the unclaim provably cannot meet
// a foreign claim by that door. That is the safe-by-caller reading, and within
// its population it holds.
//
// But refuseArrival's first line is `if order.ParentOrderID != nil { return
// nil }`. A compound child skips the ownership question ENTIRELY. The exemption
// was written for siblings, whose claims legitimately overlap — and it does not
// ask whether the claim it is waving through belongs to a sibling. So a dig leg
// reaches the writer holding whatever claim was on the bin, which is the
// lane-stress scenario 445f79eb was written for (order 9 setting bin 6 down at
// LSD_010 and wiping order 1's claim) arriving through the multi-bin door
// instead of the single-bin one that got fixed.
//
// TWO ARMS, because the fix has two ways to be wrong and only one of them is
// the obvious one. Over-tightening is the other: scope strictly to the placer
// and a sibling's claim survives the whole compound, stalling re-reservation of
// bins the dig has finished with — which is why 445f79eb exempts same-compound
// and why that exemption is asserted here rather than assumed.
//
// MUTATION (verified): make the claimed_by predicate in ApplyMultiBinArrival
// always true — i.e. restore the unconditional unclaim — and the STRANGER arm
// fires with the rig's own failure, "claimed by nobody, not by this order".
// Delete the EXISTS clause instead and the SIBLING arm fires.
func TestMultiBinSettle_CompoundChildScopesItsUnclaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, testdb.NewSuccessBackend())

	bt := &bins.BinType{Code: "SCOPE-BT", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT")
	src := &nodes.Node{Name: "SCOPE-SRC", Enabled: true}
	destF := &nodes.Node{Name: "SCOPE-DEST-F", Enabled: true}
	destS := &nodes.Node{Name: "SCOPE-DEST-S", Enabled: true}
	for _, n := range []*nodes.Node{src, destF, destS} {
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
	}

	// The compound: a parent and the child leg doing the placing.
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.Status = "in_transit"
	})
	child := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.ParentOrderID = &parent.ID
		o.SourceNode = src.Name
		o.DeliveryNode = destS.Name
		o.Status = "delivered"
	})

	// Both claim holders stay OUT of the acquiring set, for the reason spelled
	// out in TestMultiBinSettle_RefusalWritesNothing: a `queued` order with no
	// payload code is structurally failed by the running fulfillment scanner,
	// and failing it releases the very claim these arms assert on. That is a
	// race that passes locally and flakes in CI.
	sibling := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.ParentOrderID = &parent.ID
		o.Status = "in_transit"
	})
	stranger := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = "in_transit" })
	for _, o := range []*orders.Order{sibling, stranger} {
		if protocol.IsAcquiring(o.Status) {
			t.Fatalf("fixture: claim holder is %q, which is in the acquiring set — the scanner will "+
				"fail it and release the claim under test; keep it out of {queued, sourcing}", o.Status)
		}
	}

	// ARM 1 — a STRANGER's claim. Not a sibling, not the placer. This is the
	// blocker bin an ordinary demand had already claimed.
	foreign := &bins.Bin{BinTypeID: bt.ID, Label: "SCOPE-FOREIGN", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(foreign), "create foreign bin")
	testdb.ClaimBinForTest(t, db, foreign.ID, stranger.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(child.ID, foreign.ID, 0, "dropoff", src.Name, destF.Name),
		"junction row: the stranger-claimed leg")

	// ARM 2 — a SIBLING's claim, which the compound exemption must still clear.
	shared := &bins.Bin{BinTypeID: bt.ID, Label: "SCOPE-SIBLING", NodeID: &transit.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(shared), "create sibling-claimed bin")
	testdb.ClaimBinForTest(t, db, shared.ID, sibling.ID)
	testutil.MustNoErr(t, db.InsertOrderBin(child.ID, shared.ID, 1, "dropoff", src.Name, destS.Name),
		"junction row: the sibling-claimed leg")

	obs, err := db.ListOrderBins(child.ID)
	testutil.MustNoErr(t, err, "list junction rows")
	if len(obs) != 2 {
		t.Fatalf("junction rows = %d, want 2", len(obs))
	}

	refusals := eng.applyMultiBinArrivalForOrder(child, obs)

	// THE SETTLING OBSERVATION, and it is this assertion rather than the claim
	// ones that answers the round's question. Zero refusals on a batch where
	// BOTH bins are claimed by somebody other than the placer is the caller
	// precondition being absent: the writer is reached with foreign claims in
	// hand, so its unclaim is the only thing standing between a stranger's claim
	// and the floor. If this ever reads non-zero the guard has been tightened
	// and the writer's scoping is belt-and-braces rather than the guarantee.
	if len(refusals) != 0 {
		t.Fatalf("refusals = %d, want 0 — refuseArrival exempts compound children "+
			"(order.ParentOrderID != nil) from the ownership question entirely, so this batch "+
			"must reach the writer unguarded. A refusal here means the caller now guards this "+
			"path and the dispute has moved.", len(refusals))
	}

	// ARM 1: placed, and the stranger still owns it. Attribution never blocks
	// transport — the bin lands either way — but the order coming for it keeps
	// its claim.
	gotForeign, err := db.GetBin(foreign.ID)
	testutil.MustNoErr(t, err, "reload the stranger-claimed bin")
	if gotForeign.NodeID == nil || *gotForeign.NodeID != destF.ID {
		t.Errorf("stranger-claimed bin is at %v, want destF (%d) — the placement itself is never "+
			"refused; only the claim is spared", gotForeign.NodeID, destF.ID)
	}
	if gotForeign.ClaimedBy == nil {
		t.Errorf("stranger-claimed bin: claim cleared by a compound child that does not own it. " +
			"This is the 445f79eb defect reached through the multi-bin door — the stranger next " +
			"picks up its OWN bin with no claim on it, its intermediate set-down finds 0 bins in " +
			"transit under this claim, and final delivery refuses a robot carrying a bin nobody owns.")
	} else if *gotForeign.ClaimedBy != stranger.ID {
		t.Errorf("stranger-claimed bin claim = %d, want stranger %d", *gotForeign.ClaimedBy, stranger.ID)
	}

	// ARM 2: placed, and the sibling's claim IS cleared. Same compound is the
	// same owner; leaving it would strand the bin claimed until the compound
	// terminalized and stall re-reservation of bins the dig has finished with.
	gotShared, err := db.GetBin(shared.ID)
	testutil.MustNoErr(t, err, "reload the sibling-claimed bin")
	if gotShared.NodeID == nil || *gotShared.NodeID != destS.ID {
		t.Errorf("sibling-claimed bin is at %v, want destS (%d)", gotShared.NodeID, destS.ID)
	}
	if gotShared.ClaimedBy != nil {
		t.Errorf("sibling-claimed bin still claimed by %d — CreateCompoundChildren writes claims for "+
			"every step in one transaction and the last write wins, so a leg routinely finds a "+
			"SIBLING's id on the bin it is putting down. Same compound is the same owner: scoping "+
			"strictly to the placer stalls re-reservation of bins the dig has finished with.",
			*gotShared.ClaimedBy)
	}
}
