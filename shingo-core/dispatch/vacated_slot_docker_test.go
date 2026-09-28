//go:build docker

package dispatch

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// vacated_slot_docker_test.go — the vacated-slot rule against a real database.
//
// SPR 2026-09-28, pairs 7060/7061 and 7062/7063: the supply was widened FIFO
// onto a buffer partial, the return found its home holding a bin nobody lifts
// and every buffer full, and the pair parked on loader-park-no-slot every pass.
// The fix is the return landing on the buffer the supply empties; the home is
// never cleared.
//
// The P-numbers are the brief's (BRIEF-builder-swap-return-vacated-slot, v3.5).
// A DEFECT pin names what it did at 6a590181, where the rule does not exist; a
// COVERAGE pin passes there too and says what it holds unchanged.

// ── fixtures ────────────────────────────────────────────────────────────────

// vsSubmitPair sends a two_robot pair through the real intake: the supply lifts
// supplySrc, stages, waits, delivers to the line; the return waits at the line,
// lifts its bin, and drops at returnTo. returnFirst ingests the return first —
// what an outbox drain that carried on past a failed publish produces.
func vsSubmitPair(d *Dispatcher, prefix, payload, line, stage, supplySrc, returnTo string, returnFirst bool) {
	supply := func() {
		prSubmitLeg(d, prefix+"-supply", prefix+"-return", payload, line,
			prPick(supplySrc), prDropExcl(stage), prWait(stage), prPick(stage), prDrop(line))
	}
	ret := func() {
		prSubmitLeg(d, prefix+"-return", prefix+"-supply", payload, line,
			prWait(line), prPick(line), prDrop(returnTo))
	}
	if returnFirst {
		ret()
		supply()
		return
	}
	supply()
	ret()
}

// vsPairRows builds a pair as rows, for the placement- and release-level pins:
// the supply's persisted plan lifts supplySrc (as widen left it), and the return
// comes back to home. The return's plan is returned for the caller to place.
func vsPairRows(t *testing.T, db *store.DB, prefix, line, supplySrc, home string) (supply, ret *orders.Order, retSteps []resolvedStep) {
	t.Helper()
	supplySteps := vsSupply(supplySrc)
	sj := mustJSON(t, supplySteps)
	supply = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = prefix+"-supply", "line-1", OrderTypeComplex, StatusSourcing
		o.SourceNode, o.DeliveryNode, o.ProcessNode, o.PayloadCode = supplySrc, line, line, "PART-X"
		o.SiblingOrderUUID, o.Coordinated, o.StepsJSON = prefix+"-return", true, string(sj)
	})
	retSteps = []resolvedStep{vsWait(line), vsPick(line), vsDrop(home)}
	rj := mustJSON(t, retSteps)
	ret = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = prefix+"-return", "line-1", OrderTypeComplex, StatusSourcing
		o.SourceNode, o.DeliveryNode, o.ProcessNode, o.PayloadCode = line, home, line, "PART-X"
		o.SiblingOrderUUID, o.Coordinated, o.StepsJSON = prefix+"-supply", true, string(rj)
	})
	return supply, ret, retSteps
}

// vsLift hands the pass context a partner that claimed bin at node at step 0 —
// what the supply leaves behind when it acquires first.
func vsLift(t *testing.T, db *store.DB, partner *orders.Order, binID int64, node string) *pairPass {
	t.Helper()
	testdb.ClaimBinForTest(t, db, binID, partner.ID)
	steps, ok := decodeSteps(partner.StepsJSON)
	if !ok {
		t.Fatalf("partner %d plan unreadable", partner.ID)
	}
	return &pairPass{partner: partner, steps: steps,
		claimed: []reservedPickup{{stepIndex: 0, nodeName: node, binID: binID, confirmed: true}}}
}

func vsGroup(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	g := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
	testutil.MustNoErr(t, db.CreateNode(g), "create group "+name)
	return g
}

func vsChildOf(t *testing.T, db *store.DB, parent *nodes.Node, name string) *nodes.Node {
	t.Helper()
	c := &nodes.Node{Name: name, Enabled: true, ParentID: &parent.ID}
	testutil.MustNoErr(t, db.CreateNode(c), "create child "+name)
	return c
}

func vsReparent(t *testing.T, db *store.DB, n, parent *nodes.Node) {
	t.Helper()
	mustExecDispatch(t, db, `UPDATE nodes SET parent_id=$1 WHERE id=$2`, parent.ID, n.ID)
}

// vsFinalDrop is the persisted plan's final dropoff.
func vsFinalDrop(t *testing.T, o *orders.Order) resolvedStep {
	t.Helper()
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatalf("order %d plan unreadable", o.ID)
	}
	f := lastDropIndex(steps)
	if f < 0 {
		t.Fatalf("order %d has no dropoff", o.ID)
	}
	return steps[f]
}

func vsBinAt(t *testing.T, db *store.DB, binID, nodeID int64, claimedBy *int64, what string) {
	t.Helper()
	b, err := db.GetBin(binID)
	testutil.MustNoErr(t, err, "reload bin")
	if b.NodeID == nil || *b.NodeID != nodeID {
		t.Errorf("%s: bin %d moved off node %d", what, binID, nodeID)
	}
	if (claimedBy == nil) != (b.ClaimedBy == nil) || (claimedBy != nil && *claimedBy != *b.ClaimedBy) {
		t.Errorf("%s: bin %d claimed_by = %v, want %v", what, binID, b.ClaimedBy, claimedBy)
	}
}

// ── P1, P3, P12: the incident, whole passes ─────────────────────────────────

// P1 — THE INCIDENT. DEFECT PIN: at 6a590181 the pair parks on
// loader-park-no-slot, and parks again every pass.
func TestVacatedSlot_P1_IncidentReturnLandsOnTheBufferTheSupplyEmpties(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "VS1-LINE"), prNode(t, db, "VS1-STAGE")
	now := time.Now().UTC()
	full := makeLoaderBin(t, db, "PART-X", home.ID, "vs1-home-full", 10, now)
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "vs1-buffer-partial", 4, now.Add(-2*time.Hour))
	makeLoaderBin(t, db, "PART-X", line.ID, "vs1-resident", 3, now)

	vsSubmitPair(d, "vs1", "PART-X", line.Name, stage.Name, home.Name, home.Name, false)
	prScanPass(t, d, db, "vs1-return", "vs1-supply")

	supply, ret := prReloadUUID(t, db, "vs1-supply"), prReloadUUID(t, db, "vs1-return")
	if supply.VendorOrderID == "" || ret.VendorOrderID == "" {
		t.Fatalf("the pair did not go in one pass: supply %s vendor=%q cause=%q, return %s vendor=%q cause=%q",
			supply.Status, supply.VendorOrderID, supply.QueueCause, ret.Status, ret.VendorOrderID, ret.QueueCause)
	}
	if supply.SourceNode != buffer.Name {
		t.Fatalf("supply lifts %s, want the older buffer partial at %s (oldest-first is not touched)",
			supply.SourceNode, buffer.Name)
	}
	if ret.DeliveryNode != buffer.Name {
		t.Fatalf("return lands %s, want the buffer %s its supply empties first", ret.DeliveryNode, buffer.Name)
	}
	fd := vsFinalDrop(t, ret)
	if fd.Vacate == nil || fd.Vacate.Node != buffer.Name || fd.Vacate.Bin != partial.ID || fd.Vacate.Partner != supply.ID {
		t.Errorf("return's drop stamp = %+v, want {%s bin %d partner %d} — the release fence reads it",
			fd.Vacate, buffer.Name, partial.ID, supply.ID)
	}
	if fd.Anchor != home.Name {
		t.Errorf("return's anchor = %q, want the home %q it was authored at", fd.Anchor, home.Name)
	}
	vsBinAt(t, db, full.ID, home.ID, nil, "the home's full is untouched")
}

// P3 — the incident with the RETURN ingested first (outbox inversion).
// DEFECT PIN, written in advance: at 6a590181 the return runs first, reads its
// supply's plan from before this pass's widen (pickup at the home), holds the
// home; the supply then widens to the buffer; both dispatch, and the return is
// sent at a home holding a full nobody lifts.
func TestVacatedSlot_P3_InvertedPairNeverTargetsTheHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "VS3-LINE"), prNode(t, db, "VS3-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", home.ID, "vs3-home-full", 10, now)
	makeLoaderBin(t, db, "PART-X", buffer.ID, "vs3-buffer-partial", 4, now.Add(-2*time.Hour))
	makeLoaderBin(t, db, "PART-X", line.ID, "vs3-resident", 3, now)

	vsSubmitPair(d, "vs3", "PART-X", line.Name, stage.Name, home.Name, home.Name, true)
	if s, r := prReloadUUID(t, db, "vs3-supply"), prReloadUUID(t, db, "vs3-return"); r.ID > s.ID {
		t.Fatalf("fixture: the return must hold the lower id (return %d, supply %d)", r.ID, s.ID)
	}
	prScanPass(t, d, db, "vs3-supply", "vs3-return")

	ret := prReloadUUID(t, db, "vs3-return")
	if ret.DeliveryNode == home.Name {
		t.Fatalf("the inverted return targets the home %s, which holds a full nobody lifts (status %s vendor %q)",
			home.Name, ret.Status, ret.VendorOrderID)
	}
	if ret.DeliveryNode != buffer.Name || ret.VendorOrderID == "" {
		t.Fatalf("return %s at %q vendor=%q cause=%q, want dispatched to the vacated buffer %s",
			ret.Status, ret.DeliveryNode, ret.VendorOrderID, ret.QueueCause, buffer.Name)
	}
}

// failCreateFor is a backend whose fleet create fails for one order only.
type failCreateFor struct {
	*testdb.MockTrackingBackend
	uuid string
}

func (f *failCreateFor) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	if req.ExternalID == f.uuid {
		return fleet.TransportOrderResult{}, fmt.Errorf("mock: create refused for %s", f.uuid)
	}
	return f.MockTrackingBackend.CreateOrder(req)
}

// P12 — the lifter's fleet create fails, so the dropper is never created.
// DEFECT PIN: at 6a590181 an inverted pair creates in id order, the return
// first; the supply's create then fails and the return is left committed on a
// lift that never happened.
func TestVacatedSlot_P12_LifterCreateFailsDropperNotCreated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, _, _ := parkFixture(t, db)
	backend := &failCreateFor{MockTrackingBackend: testdb.NewTrackingBackend(), uuid: "vs12-supply"}
	d, _ := newTestDispatcher(t, db, backend)
	line, stage := prNode(t, db, "VS12-LINE"), prNode(t, db, "VS12-STAGE")
	now := time.Now().UTC()
	// An ordinary home swap: the supply lifts the home's own full, so today's
	// code dispatches both legs — the shape in which create order matters.
	makeLoaderBin(t, db, "PART-X", home.ID, "vs12-home-full", 10, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "vs12-resident", 3, now)

	vsSubmitPair(d, "vs12", "PART-X", line.Name, stage.Name, home.Name, home.Name, true)
	prScanPass(t, d, db, "vs12-supply", "vs12-return")

	ret := prReloadUUID(t, db, "vs12-return")
	if ret.VendorOrderID != "" {
		t.Fatalf("the return was committed to the fleet (vendor %q) although its supply's create failed — "+
			"the lifter's create must go first", ret.VendorOrderID)
	}
}

// ── P2, P4, P5, P13, P25: the other shapes the rule covers ───────────────────

// P2 — the older full on a SECOND home of the same payload (D1). DEFECT PIN:
// at 6a590181 the pair parks on loader-park-no-slot.
func TestVacatedSlot_P2_OlderFullOnASecondHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, loaderID := parkFixture(t, db)
	h2 := prNode(t, db, "LX-P1B")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: h2.ID,
		PayloadCode: "PART-X", Kind: loaders.HomeKindHome}), "second home")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "VS2-LINE"), prNode(t, db, "VS2-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", home.ID, "vs2-h1-full", 10, now)
	makeLoaderBin(t, db, "PART-X", h2.ID, "vs2-h2-older", 10, now.Add(-3*time.Hour))
	makeLoaderBin(t, db, "PART-X", buffer.ID, "vs2-buffer", 4, now.Add(-1*time.Hour))
	makeLoaderBin(t, db, "PART-X", line.ID, "vs2-resident", 3, now)

	vsSubmitPair(d, "vs2", "PART-X", line.Name, stage.Name, home.Name, home.Name, false)
	prScanPass(t, d, db, "vs2-return", "vs2-supply")

	ret := prReloadUUID(t, db, "vs2-return")
	if ret.DeliveryNode != h2.Name || ret.VendorOrderID == "" {
		t.Fatalf("return %s at %q vendor=%q cause=%q, want dispatched to the second home %s its supply empties",
			ret.Status, ret.DeliveryNode, ret.VendorOrderID, ret.QueueCause, h2.Name)
	}
}

// P4 — a two-robot swap into a full market it also pulls from. DEFECT PIN: at
// 6a590181 the evac's group drop re-resolves to capacity every pass
// (ngrp-resolve) and the pair parks.
func TestVacatedSlot_P4_TwoRobotIntoItsOwnFullMarket(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	grp := vsGroup(t, db, "VS4-MKT")
	c1, c2 := vsChildOf(t, db, grp, "VS4-C1"), vsChildOf(t, db, grp, "VS4-C2")
	stage := prNode(t, db, "VS4-STAGE")
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, c1.ID, "VS4-FRESH")
	other := makeEmptyBin(t, db, c2.ID, "VS4-OTHER")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "VS4-RESIDENT")

	vsSubmitPair(d, "vs4", sd.Payload.Code, sd.LineNode.Name, stage.Name, grp.Name, grp.Name, false)
	prScanPass(t, d, db, "vs4-return", "vs4-supply")

	supply, evac := prReloadUUID(t, db, "vs4-supply"), prReloadUUID(t, db, "vs4-return")
	if supply.VendorOrderID == "" || evac.VendorOrderID == "" {
		t.Fatalf("pair did not go in ONE pass: supply %s/%q, evac %s/%q (%q)",
			supply.Status, supply.VendorOrderID, evac.Status, evac.VendorOrderID, evac.QueueCause)
	}
	if evac.DeliveryNode != c1.Name {
		t.Fatalf("evac lands %s, want %s — the child its supply empties first", evac.DeliveryNode, c1.Name)
	}
	n, err := db.GetNode(c1.ID)
	testutil.MustNoErr(t, err, "reload child")
	if n.ClaimedBy == nil || *n.ClaimedBy != evac.ID {
		t.Errorf("the slot claim did not go through the partner arm: claimed_by=%v, want %d", n.ClaimedBy, evac.ID)
	}
	vsBinAt(t, db, other.ID, c2.ID, nil, "the other child is untouched")
	_ = fresh
}

// P5 — a single-robot swap into its own full source market. DEFECT PIN,
// observed at the NGRP re-resolve asker: at 6a590181 step 9 re-resolves to
// capacity every pass.
func TestVacatedSlot_P5_SingleRobotIntoItsOwnSourceMarket(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	grp := vsGroup(t, db, "VS5-MKT")
	c1, c2 := vsChildOf(t, db, grp, "VS5-C1"), vsChildOf(t, db, grp, "VS5-C2")
	is, os := prNode(t, db, "VS5-IS"), prNode(t, db, "VS5-OS")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, c1.ID, "VS5-FRESH")
	makeEmptyBin(t, db, c2.ID, "VS5-OTHER")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "VS5-RESIDENT")
	line := sd.LineNode.Name

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "vs5-single", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: line,
		Steps: []protocol.ComplexOrderStep{
			prPick(grp.Name), prDropExcl(is.Name), prWait(line), prPick(line), prDropExcl(os.Name),
			prPick(is.Name), prDrop(line), prPick(os.Name), prDrop(grp.Name),
		},
	})
	prScanPass(t, d, db, "vs5-single")

	o := prReloadUUID(t, db, "vs5-single")
	if o.VendorOrderID == "" {
		t.Fatalf("single-robot swap did not dispatch: %s cause=%q", o.Status, o.QueueCause)
	}
	steps, _ := decodeSteps(o.StepsJSON)
	if steps[0].Node != c1.Name || steps[8].Node != c1.Name {
		t.Fatalf("step 1 lifts %s, step 9 drops %s — step 9 must land on the slot step 1 frees (%s)",
			steps[0].Node, steps[8].Node, c1.Name)
	}
	if st := stampHonoured(steps[8]); st == nil || st.Partner != 0 {
		t.Errorf("step 9's stamp = %+v, want an own-lift grant (no partner, so no release fence)", steps[8].Vacate)
	}
}

// P13 — an NGRP-child home, on the Edge's own unwidened plan. DEFECT PIN: at
// 6a590181 the final-drop gate reads the home as it is now — the full the
// supply is lifting — and the pair parks dropoff-occupied every pass.
func TestVacatedSlot_P13_NGRPChildHomeUnwidenedPlan(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	grp := vsGroup(t, db, "VS13-MKT")
	vsReparent(t, db, home, grp)
	vsReparent(t, db, buffer, grp)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "VS13-LINE"), prNode(t, db, "VS13-STAGE")
	now := time.Now().UTC()
	full := makeLoaderBin(t, db, "PART-X", home.ID, "vs13-home-full", 10, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "vs13-resident", 3, now)

	vsSubmitPair(d, "vs13", "PART-X", line.Name, stage.Name, home.Name, home.Name, false)
	prScanPass(t, d, db, "vs13-return", "vs13-supply")

	supply, ret := prReloadUUID(t, db, "vs13-supply"), prReloadUUID(t, db, "vs13-return")
	if supply.VendorOrderID == "" || ret.VendorOrderID == "" {
		t.Fatalf("pair parked: supply %s/%q, return %s/%q cause=%q", supply.Status, supply.VendorOrderID,
			ret.Status, ret.VendorOrderID, ret.QueueCause)
	}
	fd := vsFinalDrop(t, ret)
	if ret.DeliveryNode != home.Name || fd.Vacate == nil || fd.Vacate.Bin != full.ID || fd.Vacate.Partner != supply.ID {
		t.Fatalf("return at %s with stamp %+v, want the home %s stamped on bin %d by partner %d",
			ret.DeliveryNode, fd.Vacate, home.Name, full.ID, supply.ID)
	}
}

// P25 — the incident with the loader's slots as NGRP children: the vacated arm,
// then the final-drop ordering and the slot claim's partner arm. DEFECT PIN: at
// 6a590181 the pair parks on loader-park-no-slot.
func TestVacatedSlot_P25_IncidentWithGroupChildLoaderSlots(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	grp := vsGroup(t, db, "VS25-MKT")
	vsReparent(t, db, home, grp)
	vsReparent(t, db, buffer, grp)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "VS25-LINE"), prNode(t, db, "VS25-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", home.ID, "vs25-home-full", 10, now)
	makeLoaderBin(t, db, "PART-X", buffer.ID, "vs25-buffer-partial", 4, now.Add(-2*time.Hour))
	makeLoaderBin(t, db, "PART-X", line.ID, "vs25-resident", 3, now)

	vsSubmitPair(d, "vs25", "PART-X", line.Name, stage.Name, home.Name, home.Name, false)
	prScanPass(t, d, db, "vs25-return", "vs25-supply")

	ret := prReloadUUID(t, db, "vs25-return")
	if ret.DeliveryNode != buffer.Name || ret.VendorOrderID == "" {
		t.Fatalf("return %s at %q vendor=%q cause=%q, want dispatched to the vacated buffer %s",
			ret.Status, ret.DeliveryNode, ret.VendorOrderID, ret.QueueCause, buffer.Name)
	}
	n, err := db.GetNode(buffer.ID)
	testutil.MustNoErr(t, err, "reload buffer")
	if n.ClaimedBy == nil || *n.ClaimedBy != ret.ID {
		t.Errorf("buffer claimed_by=%v, want the return %d through the partner arm", n.ClaimedBy, ret.ID)
	}
}

// ── P6, P7, P10, P14, P14b, P17: the loader arm's refusals and its edges ────

// The loader-arm refusals, each equal to today's WAIT (P6), plus the in-flight
// third order (P17) and the one-leg slice (P10: no pass context, whatever the
// partner's state). COVERAGE PIN for each: 6a590181 also waits.
func TestVacatedSlot_P6_LoaderArmRefusalsWait(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep)
	}{
		{"the partner's lift comes after its wait", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			pc := vsLift(t, db, supply, partial, buffer.Name)
			pc.steps = []resolvedStep{vsWait(buffer.Name), vsPick(buffer.Name), vsDrop("LINE")}
			pc.claimed[0].stepIndex = 1
			return pc, nil
		}},
		{"L has no station wait before its drop", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			return vsLift(t, db, supply, partial, buffer.Name), []resolvedStep{vsPick(ret.ProcessNode), vsDrop(home.Name)}
		}},
		{"the bin on N is not held by the partner", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			steps, _ := decodeSteps(supply.StepsJSON)
			return &pairPass{partner: supply, steps: steps,
				claimed: []reservedPickup{{stepIndex: 0, nodeName: buffer.Name, binID: partial}}}, nil
		}},
		{"N holds two bins", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			makeLoaderBin(t, db, "PART-X", buffer.ID, "vs6-second", 2, time.Now().UTC())
			return vsLift(t, db, supply, partial, buffer.Name), nil
		}},
		{"another order is in flight to N (P17)", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			makeInFlightTo(t, db, "vs6-inflight", buffer.Name)
			return vsLift(t, db, supply, partial, buffer.Name), nil
		}},
		{"a lane slot", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
			testutil.MustNoErr(t, err, "LANE type")
			lane := &nodes.Node{Name: "VS6-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
			testutil.MustNoErr(t, db.CreateNode(lane), "lane")
			vsReparent(t, db, buffer, lane)
			return vsLift(t, db, supply, partial, buffer.Name), nil
		}},
		{"a one-leg slice: no pass context (P10)", func(t *testing.T, db *store.DB, supply, ret *orders.Order, home, buffer *nodes.Node, partial int64) (*pairPass, []resolvedStep) {
			vsLift(t, db, supply, partial, buffer.Name) // it holds the bin, and is committed — not in this pass
			mustExecDispatch(t, db, `UPDATE orders SET status='faulted' WHERE id=$1`, supply.ID)
			return nil, nil
		}},
	}
	for i, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			home, buffer, _, _ := parkFixture(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
			line := prNode(t, db, fmt.Sprintf("VS6-LINE-%d", i))
			now := time.Now().UTC()
			makeLoaderBin(t, db, "PART-X", home.ID, "vs6-home-full", 10, now)
			partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "vs6-partial", 4, now.Add(-time.Hour))
			makeLoaderBin(t, db, "PART-X", line.ID, "vs6-resident", 3, now)
			supply, ret, retSteps := vsPairRows(t, db, fmt.Sprintf("vs6-%d", i), line.Name, buffer.Name, home.Name)
			pc, override := c.setup(t, db, supply, ret, home, buffer, partial.ID)
			if override != nil {
				retSteps = override
			}
			if got := d.placeForDedicatedLoader(ret, retSteps, pc); got != home.Name {
				t.Fatalf("waitHome = %q (DeliveryNode %q), want the wait on %q exactly as today",
					got, ret.DeliveryNode, home.Name)
			}
			if fd := vsFinalDrop(t, ret); fd.Vacate != nil || fd.Anchor != "" {
				t.Errorf("a refused drop was stamped or anchored: %+v", fd)
			}
		})
	}
}

// P6 — a foreign carrier onto a pinned home stays refused on the vacated arm
// too: a PART-Y carrier is not put on PART-X's second home just because the
// supply empties it. COVERAGE PIN.
func TestVacatedSlot_P6_ForeignCarrierNotOntoAVacatedPinnedHome(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, loaderID := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, line := mismatchFixture(t, db, loaderID, "VS6F-LINE")
	h2 := prNode(t, db, "VS6F-H2")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: h2.ID,
		PayloadCode: "PART-X", Kind: loaders.HomeKindHome}), "second home")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-Y", line.ID, "vs6f-carrier", 72, now)
	makeLoaderBin(t, db, "PART-X", home.ID, "vs6f-home", 10, now)
	makeLoaderBin(t, db, "PART-X", buffer.ID, "vs6f-buffer", 4, now)
	older := makeLoaderBin(t, db, "PART-X", h2.ID, "vs6f-h2", 10, now.Add(-time.Hour))

	supply, ret, retSteps := vsPairRows(t, db, "vs6f", line.Name, h2.Name, home.Name)
	pc := vsLift(t, db, supply, older.ID, h2.Name)
	if got := d.placeForDedicatedLoader(ret, retSteps, pc); got == "" && ret.DeliveryNode == h2.Name {
		t.Fatalf("a PART-Y carrier was placed on PART-X's pinned home %s", h2.Name)
	}
}

// P7 — a free buffer wins over a vacated one, and is not stamped. COVERAGE PIN.
func TestVacatedSlot_P7_AFreeBufferStillWins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, loaderID := parkFixture(t, db)
	b2 := prNode(t, db, "LX-B2")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: b2.ID,
		Kind: loaders.HomeKindBuffer}), "second buffer")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	line := prNode(t, db, "VS7-LINE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", home.ID, "vs7-home", 10, now)
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "vs7-partial", 4, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "vs7-resident", 3, now)
	supply, ret, retSteps := vsPairRows(t, db, "vs7", line.Name, buffer.Name, home.Name)
	pc := vsLift(t, db, supply, partial.ID, buffer.Name)

	if got := d.placeForDedicatedLoader(ret, retSteps, pc); got != "" || ret.DeliveryNode != b2.Name {
		t.Fatalf("waitHome=%q DeliveryNode=%q, want the FREE buffer %s", got, ret.DeliveryNode, b2.Name)
	}
	if fd := vsFinalDrop(t, ret); fd.Vacate != nil || fd.Anchor != "" {
		t.Errorf("an ordinary buffer placement was stamped or anchored: %+v", fd)
	}
}

// P14 — a vacated placement, a park, and a partner that lifts elsewhere next
// pass: the return re-derives from its real home. Also P24's anchor row: the
// stamp for the slot it left is gone from the persisted plan.
func TestVacatedSlot_P14_ReplacesFromTheRealHomeAfterAPark(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	line := prNode(t, db, "VS14-LINE")
	now := time.Now().UTC()
	homeFull := makeLoaderBin(t, db, "PART-X", home.ID, "vs14-home", 10, now)
	partial := makeLoaderBin(t, db, "PART-X", buffer.ID, "vs14-partial", 4, now.Add(-time.Hour))
	makeLoaderBin(t, db, "PART-X", line.ID, "vs14-resident", 3, now)
	supply, ret, retSteps := vsPairRows(t, db, "vs14", line.Name, buffer.Name, home.Name)

	// Pass 1: the supply lifts the buffer partial; the return takes the buffer.
	if got := d.placeForDedicatedLoader(ret, retSteps, vsLift(t, db, supply, partial.ID, buffer.Name)); got != "" ||
		ret.DeliveryNode != buffer.Name {
		t.Fatalf("pass 1: waitHome=%q DeliveryNode=%q, want the vacated buffer", got, ret.DeliveryNode)
	}
	// The pair parks: holdings go back; the buffer partial is moved off by hand;
	// and next pass the supply lifts the home's own full.
	testutil.MustNoErr(t, db.ReleaseOrderHoldings(supply.ID), "park the supply")
	testutil.MustNoErr(t, db.DeleteBin(partial.ID), "the buffer partial leaves")
	supplySteps := vsSupply(home.Name)
	sj := mustJSON(t, supplySteps)
	testutil.MustNoErr(t, db.UpdateOrderStepsJSON(supply.ID, string(sj)), "supply now lifts the home")
	supply = prReload(t, db, supply.ID)
	ret = prReload(t, db, ret.ID)
	steps2, _ := decodeSteps(ret.StepsJSON)

	if got := d.placeForDedicatedLoader(ret, steps2, vsLift(t, db, supply, homeFull.ID, home.Name)); got != "" ||
		ret.DeliveryNode != home.Name {
		t.Fatalf("pass 2: waitHome=%q DeliveryNode=%q, want the real home %s (its sibling lifts it). Without "+
			"the anchor the free buffer %s would be read as the home", got, ret.DeliveryNode, home.Name, buffer.Name)
	}
	if fd := vsFinalDrop(t, prReload(t, db, ret.ID)); fd.Vacate != nil {
		t.Errorf("the stamp for the slot it left is still on the persisted plan: %+v", fd.Vacate)
	}
}

// P14b — a return placed on a FREE buffer, whose pair then parks: today's
// stickiness, unchanged. COVERAGE PIN.
func TestVacatedSlot_P14b_OrdinaryBufferPlacementStaysSticky(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, buffer, _, _ := parkFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	line := prNode(t, db, "VS14B-LINE")
	now := time.Now().UTC()
	homeFull := makeLoaderBin(t, db, "PART-X", home.ID, "vs14b-home", 10, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "vs14b-resident", 3, now)
	_, ret, retSteps := vsPairRows(t, db, "vs14b", line.Name, "ELSEWHERE", home.Name)

	if got := d.placeForDedicatedLoader(ret, retSteps, nil); got != "" || ret.DeliveryNode != buffer.Name {
		t.Fatalf("pass 1: waitHome=%q DeliveryNode=%q, want the free buffer", got, ret.DeliveryNode)
	}
	testutil.MustNoErr(t, db.DeleteBin(homeFull.ID), "the home frees")
	ret = prReload(t, db, ret.ID)
	steps2, _ := decodeSteps(ret.StepsJSON)
	if got := d.placeForDedicatedLoader(ret, steps2, nil); got != "" || ret.DeliveryNode != buffer.Name {
		t.Fatalf("pass 2: DeliveryNode=%q, want it still aimed at the buffer %s — today's stickiness, pinned as-is",
			ret.DeliveryNode, buffer.Name)
	}
}

// ── P23, P24: the final-drop ordering and the stamp's writers ────────────────

// P23 — an unstamped fungible drop blocked while a free child exists reverts to
// its group exactly as today, and the rule is not consulted even though it would
// admit. COVERAGE PIN.
func TestVacatedSlot_P23_UnstampedFungibleDropStillReverts(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	grp := vsGroup(t, db, "VS23-MKT")
	c1 := vsChildOf(t, db, grp, "VS23-C1")
	vsChildOf(t, db, grp, "VS23-C2") // free
	bin := makeEmptyBin(t, db, c1.ID, "VS23-BIN")
	supply, ret, _ := vsPairRows(t, db, "vs23", "VS23-LINE", c1.Name, c1.Name)
	steps := []resolvedStep{vsWait("VS23-LINE"), vsPick("VS23-LINE"), {Action: protocol.ActionDropoff, Node: c1.Name, Group: grp.Name}}
	pc := vsLift(t, db, supply, bin.ID, c1.Name)

	if st := d.reserveComplexDestination(ret, steps, pc); !st.done {
		t.Fatal("the blocked fungible drop was admitted — a free sibling exists, so today's revert must run")
	}
	if steps[2].Node != grp.Name || steps[2].Vacate != nil {
		t.Fatalf("drop = %s stamp %+v, want reverted to %s unstamped", steps[2].Node, steps[2].Vacate, grp.Name)
	}
}

// P24 — each writer that re-points a stamped drop leaves no stamp that counts.
func TestVacatedSlot_P24_RepointedStampsDoNotCount(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	grp := vsGroup(t, db, "VS24-MKT")
	c1 := vsChildOf(t, db, grp, "VS24-C1")
	bin := makeEmptyBin(t, db, c1.ID, "VS24-BIN")
	_, ret, _ := vsPairRows(t, db, "vs24", "VS24-LINE", c1.Name, c1.Name)
	stamp := &vacateStamp{Node: c1.Name, Bin: bin.ID, Partner: ret.ID + 1000}

	t.Run("fungible revert", func(t *testing.T) {
		steps := []resolvedStep{vsWait("VS24-LINE"), vsPick("VS24-LINE"),
			{Action: protocol.ActionDropoff, Node: c1.Name, Group: grp.Name, Vacate: stamp}}
		// No pass context: the rule no longer holds, so clause 1 falls to the revert.
		if st := d.reserveComplexDestination(ret, steps, nil); !st.done || steps[2].Vacate != nil || steps[2].Node != grp.Name {
			t.Fatalf("done=%v drop=%s stamp=%+v, want reverted and cleared", st.done, steps[2].Node, steps[2].Vacate)
		}
	})
	t.Run("slot-reserve revert", func(t *testing.T) {
		other := testdb.CreateOrder(t, db)
		testutil.MustNoErr(t, reservations.AcquireSlot(db.DB, other.ID, c1.ID, "test"), "another order holds the slot")
		steps := []resolvedStep{vsWait("VS24-LINE"), vsPick("VS24-LINE"),
			{Action: protocol.ActionDropoff, Node: c1.Name, Group: grp.Name, Vacate: stamp}}
		if _, err := d.allocator.reserveComplexSlots(ret, steps); err != nil {
			t.Fatalf("reserve slots: %v", err)
		}
		if steps[2].Node != grp.Name || steps[2].Vacate != nil {
			t.Fatalf("drop=%s stamp=%+v, want reverted to the group and cleared", steps[2].Node, steps[2].Vacate)
		}
	})
	t.Run("release-time redirect", func(t *testing.T) {
		// Stamped at C1 with C1's bin still there, but redirected while staged: the
		// segment the robot gets drops elsewhere, so the stamp does not count.
		steps := []resolvedStep{vsWait("VS24-LINE"), vsPick("VS24-LINE"),
			{Action: protocol.ActionDropoff, Node: c1.Name, Vacate: stamp}}
		j := mustJSON(t, steps)
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.StationID, o.OrderType, o.Status = "vs24-redirected", "line-1", OrderTypeComplex, StatusStaged
			o.DeliveryNode, o.StepsJSON = "VS24-ELSEWHERE", string(j)
		})
		if refusal := d.vacateReleaseRefusal(o); refusal != "" {
			t.Fatalf("a redirected drop was fenced on a stamp it no longer carries: %s", refusal)
		}
	})
}

// ── P9, P9b, P18-P22: the release fence ─────────────────────────────────────

// vsStagedLeg makes a staged, fleet-committed leg at wait 0 with the given plan.
func vsStagedLeg(t *testing.T, db *store.DB, uuid, sibling, delivery string, status protocol.Status, steps []resolvedStep) *orders.Order {
	t.Helper()
	j := mustJSON(t, steps)
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = uuid, "line-1", OrderTypeComplex, status
		o.DeliveryNode, o.SiblingOrderUUID, o.Coordinated, o.StepsJSON = delivery, sibling, true, string(j)
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, "V-"+uuid, "CREATED", ""), "vendor")
	return prReload(t, db, o.ID)
}

type releaseRig struct {
	db      *store.DB
	d       *Dispatcher
	backend *testdb.MockTrackingBackend
}

func newReleaseRig(t *testing.T) releaseRig {
	t.Helper()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, db, backend)
	return releaseRig{db: db, d: d, backend: backend}
}

// released reports whether a release of o appended a segment.
func (r releaseRig) released(t *testing.T, o *orders.Order) bool {
	t.Helper()
	before := len(r.backend.ReleaseCalls())
	r.d.HandleOrderRelease(r.d.syntheticEnvelope(o.StationID), &protocol.OrderRelease{OrderUUID: o.EdgeUUID})
	return len(r.backend.ReleaseCalls()) > before
}

// swapAtVacatedBuffer stages the incident after dispatch: Z (the return) drops
// on B1, stamped on the partial its partner Y (the supply) lifts; the line holds
// Z's claimed resident. yStatus is where the supply stands.
func swapAtVacatedBuffer(t *testing.T, r releaseRig, prefix string, yStatus protocol.Status) (z, y *orders.Order, b1, carrier *nodes.Node, partialID int64) {
	t.Helper()
	line, stage := prNode(t, r.db, prefix+"-LINE"), prNode(t, r.db, prefix+"-STAGE")
	b1 = prNode(t, r.db, prefix+"-B1")
	carrier = prNode(t, r.db, prefix+"-ROBOT")
	y = vsStagedLeg(t, r.db, prefix+"-supply", prefix+"-return", line.Name, yStatus,
		[]resolvedStep{vsPick(b1.Name), vsDrop(stage.Name), vsWait(stage.Name), vsPick(stage.Name), vsDrop(line.Name)})
	partial := makeEmptyBin(t, r.db, b1.ID, prefix+"-PARTIAL")
	testdb.ClaimBinForTest(t, r.db, partial.ID, y.ID)
	z = vsStagedLeg(t, r.db, prefix+"-return", prefix+"-supply", b1.Name, StatusStaged,
		[]resolvedStep{vsWait(line.Name), vsPick(line.Name),
			{Action: protocol.ActionDropoff, Node: b1.Name, Vacate: &vacateStamp{Node: b1.Name, Bin: partial.ID, Partner: y.ID}}})
	resident := makeEmptyBin(t, r.db, line.ID, prefix+"-RESIDENT")
	testdb.ClaimBinForTest(t, r.db, resident.ID, z.ID)
	return z, y, b1, carrier, partial.ID
}

// P9 — the stamped drop is refused while its bin stands there, then goes once
// it has been lifted. DEFECT PIN: at 6a590181 there is no stamp and no fence;
// the release appends and the robot drives at an occupied slot.
func TestVacatedSlot_P9_ReleaseFenceHoldsUntilTheLift(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, _, _, carrier, partial := swapAtVacatedBuffer(t, r, "VS9", StatusDispatched)
	if r.released(t, z) {
		t.Fatal("the return was released onto a slot its partner has not emptied")
	}
	if fresh := prReload(t, r.db, z.ID); fresh.WaitIndex != 0 || fresh.Status != StatusStaged {
		t.Errorf("a refused release moved the order: wait_index %d status %s", fresh.WaitIndex, fresh.Status)
	}
	mustExecDispatch(t, r.db, `UPDATE bins SET node_id=$1 WHERE id=$2`, carrier.ID, partial) // the supply lifts it
	if !r.released(t, prReload(t, r.db, z.ID)) {
		t.Fatal("the slot is empty now; the release must go")
	}
}

// P9b — the ordinary SPR home swap is not fenced: unstamped, released exactly as
// today even though the supply has not lifted the home's bin. COVERAGE PIN.
func TestVacatedSlot_P9b_OrdinaryHomeSwapReleasesAsToday(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	line, stage, home := prNode(t, r.db, "VS9B-LINE"), prNode(t, r.db, "VS9B-STAGE"), prNode(t, r.db, "VS9B-HOME")
	y := vsStagedLeg(t, r.db, "vs9b-supply", "vs9b-return", line.Name, StatusDispatched,
		[]resolvedStep{vsPick(home.Name), vsDrop(stage.Name), vsWait(stage.Name), vsPick(stage.Name), vsDrop(line.Name)})
	homeBin := makeEmptyBin(t, r.db, home.ID, "VS9B-HOMEBIN")
	testdb.ClaimBinForTest(t, r.db, homeBin.ID, y.ID)
	z := vsStagedLeg(t, r.db, "vs9b-return", "vs9b-supply", home.Name, StatusStaged,
		[]resolvedStep{vsWait(line.Name), vsPick(line.Name), vsDrop(home.Name)})
	if !r.released(t, z) {
		t.Fatal("an ordinary, unstamped home-swap return was refused — the home arm must be byte-unchanged")
	}
}

// P18 / P19 — Y must not go while Z is held: deferred and refired when it stages
// (P18), or already in transit before its wait (P19). DEFECT PINS against v3:
// Y's release appends and it drops a fresh bin onto a line still holding Z's.
func TestVacatedSlot_P18_P19_PartnerHeldWithTheStampedLeg(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		status protocol.Status
	}{{"P18 deferred, refired at staged", StatusStaged}, {"P19 already in transit", StatusInTransit}} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			z, y, _, _, _ := swapAtVacatedBuffer(t, r, fmt.Sprintf("VS18-%s", c.status), StatusDispatched)
			if r.released(t, z) {
				t.Fatal("fixture: Z must be refused first")
			}
			mustExecDispatch(t, r.db, `UPDATE orders SET status=$1 WHERE id=$2`, string(c.status), y.ID)
			if r.released(t, prReload(t, r.db, y.ID)) {
				t.Fatal("Y was released onto a line that still holds Z's bin, while Z is held")
			}
		})
	}
}

// P20 — press-index, unflipped and 3-position, on a normal click: R1 then R2
// both released; the Y check never fires (no stamps). COVERAGE PIN — and the
// reason the stamp condition is load-bearing.
func TestVacatedSlot_P20_PressIndexReleasesAsToday(t *testing.T) {
	t.Parallel()
	for _, threePos := range []bool{false, true} {
		threePos := threePos
		t.Run(fmt.Sprintf("threePosition=%v", threePos), func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			p := fmt.Sprintf("VS20-%v", threePos)
			a, b, out, in := prNode(t, r.db, p+"-A"), prNode(t, r.db, p+"-B"), prNode(t, r.db, p+"-OUT"), prNode(t, r.db, p+"-IN")
			back := b
			r2Steps := []resolvedStep{vsWait(b.Name), vsPick(b.Name), vsDrop(a.Name)}
			if threePos {
				c := prNode(t, r.db, p+"-C")
				back = c
				r2Steps = append(r2Steps, vsPick(c.Name), vsDrop(b.Name))
			}
			r1 := vsStagedLeg(t, r.db, p+"-r1", p+"-r2", back.Name, StatusStaged,
				[]resolvedStep{vsWait(a.Name), vsPick(a.Name), vsDrop(out.Name), vsPick(in.Name), vsDrop(back.Name)})
			r2 := vsStagedLeg(t, r.db, p+"-r2", p+"-r1", a.Name, StatusStaged, r2Steps)
			front := makeEmptyBin(t, r.db, a.ID, p+"-FRONT")
			testdb.ClaimBinForTest(t, r.db, front.ID, r1.ID)
			onDeck := makeEmptyBin(t, r.db, back.ID, p+"-ONDECK")
			testdb.ClaimBinForTest(t, r.db, onDeck.ID, r2.ID)
			if !r.released(t, r1) {
				t.Fatal("R1 was refused on a normal press-index click")
			}
			if !r.released(t, prReload(t, r.db, r2.ID)) {
				t.Fatal("R2 was refused on a normal press-index click")
			}
		})
	}
}

// P21 — Y's release published before Z's (outbox inversion): Y is refused; once
// the lift has happened the next click releases both. DEFECT PIN against v3.
func TestVacatedSlot_P21_InvertedReleaseRefusesYThenBothGo(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, y, _, carrier, partial := swapAtVacatedBuffer(t, r, "VS21", StatusStaged)
	if r.released(t, y) {
		t.Fatal("Y went first while Z is staged and stamped on a bin still standing")
	}
	mustExecDispatch(t, r.db, `UPDATE bins SET node_id=$1 WHERE id=$2`, carrier.ID, partial)
	if !r.released(t, prReload(t, r.db, z.ID)) {
		t.Fatal("next click: Z must go once its slot is empty")
	}
	if !r.released(t, prReload(t, r.db, y.ID)) {
		t.Fatal("next click: Y must go once Z has")
	}
}

// P22 — an unstamped two-robot pair on a normal click: both released, unchanged.
// COVERAGE PIN.
func TestVacatedSlot_P22_UnstampedTwoRobotReleasesAsToday(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	line, stage, out := prNode(t, r.db, "VS22-LINE"), prNode(t, r.db, "VS22-STAGE"), prNode(t, r.db, "VS22-OUT")
	y := vsStagedLeg(t, r.db, "vs22-supply", "vs22-return", line.Name, StatusStaged,
		[]resolvedStep{vsPick("VS22-SRC"), vsDrop(stage.Name), vsWait(stage.Name), vsPick(stage.Name), vsDrop(line.Name)})
	z := vsStagedLeg(t, r.db, "vs22-return", "vs22-supply", out.Name, StatusStaged,
		[]resolvedStep{vsWait(line.Name), vsPick(line.Name), vsDrop(out.Name)})
	resident := makeEmptyBin(t, r.db, line.ID, "VS22-RESIDENT")
	testdb.ClaimBinForTest(t, r.db, resident.ID, z.ID)
	if !r.released(t, z) || !r.released(t, prReload(t, r.db, y.ID)) {
		t.Fatal("an unstamped two-robot pair was refused on a normal click")
	}
}
