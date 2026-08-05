//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// lane_gate_authority_docker_test.go — which orders a station may command at all.
//
// The wait fence (complex_release.go) answers "which wait may be popped". This
// answers the question underneath it, and it fixes the family rather than the
// instance: FOUR handlers sit behind getOwnedOrder — HandleOrderRelease,
// HandleOrderCancel, HandleOrderReceipt, HandleOrderRedirect — and every one of
// them accepted a station's command for a Core-created compound child.
//
// ── Why a station can reach a leg at all ──────────────────────────────────
// A reshuffle leg inherits StationID from its parent (compound.go:90), and the
// parent is a real station-originated retrieve. So checkOwnership's
// `env.Src.Station == order.StationID` comparison PASSES for a leg.
//
// The grounding pass claimed legs were unexposed because Edge holds no row for
// them. That is wrong, and this is the finding that makes this step load-bearing
// rather than theoretical: CoreDataService.unlistedFor
// (messaging/core_data_service.go) heals an Edge with every active order for the
// asking station, filtering only on nil / empty EdgeUUID / already-asked. It calls
// ListActiveOrdersByStation, whose query carries no parent_order_id filter
// (store/orders/orders.go ListActiveByStation). Legs carry a Core-minted EdgeUUID
// (compound.go:89), so the reconcile sends them down and Edge's
// ApplyOrderProjection CREATES rows for them. After one reconcile a station has a
// row for a reshuffle leg it never asked for.
//
// ── Why ParentOrderID and not station_id ──────────────────────────────────
// station_id is carrying three jobs at once: authorization (this), addressing
// (which Edge gets the status), and attribution (whose demand caused it). Only the
// first is wrong for a leg. Blanking it would lose the audit actor on the
// Fail/Cancel paths (compound.go:255, :452, :456), and a new originating-station
// column would duplicate origin_id/origin_class, which is already copied
// parent→child at compound.go:132. ParentOrderID != nil already means exactly
// "Core created this", structurally and durably.

// TestAuthority_StationCannotCancelAReshuffleLeg is the red, and cancel is chosen
// over release because acceptance here has an effect the fence in
// complex_release.go cannot mask: it cancels a leg mid-dig, and cancelCompoundChildren
// plus CancelOrder then unwind a reshuffle that Core is in the middle of running.
//
// Vacuity: asserting "the leg was not cancelled" would also pass if the handler
// never reached the leg — if the UUID did not resolve, or ownership failed for some
// unrelated reason. So the test first asserts the leg IS reachable the ordinary
// way (a Core-role envelope cancels it fine), then asserts the STATION envelope
// does not. Two envelopes, one order, one difference.
//
// MUTATION (verified): remove the ParentOrderID arm from checkOwnership. The
// station cancel then succeeds and this test's own "still non-terminal" assertion
// fires. The status precondition does not shield it — the leg is pending, which is
// exactly the state a station could catch it in.
func TestAuthority_StationCannotCancelAReshuffleLeg(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	sd := testdb.SetupStandardData(t, db)

	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.StationID = "line-1"
		o.Status = protocol.StatusReshuffling
	})
	leg := testdb.CreateOrder(t, db, func(o *orders.Order) {
		// Exactly what CreateCompoundChildrenOnly writes: the parent's station,
		// a Core-minted edge uuid, a parent pointer.
		o.StationID = parent.StationID
		o.OrderType = OrderTypeMove
		o.Status = StatusPending
		o.ParentOrderID = &parent.ID
		o.Sequence = 1
		o.DeliveryNode = sd.LineNode.Name
	})

	// The station the leg claims to belong to sends a cancel.
	stationEnv := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: parent.StationID}}
	d.HandleOrderCancel(stationEnv, &protocol.OrderCancel{OrderUUID: leg.EdgeUUID, Reason: "operator cancel"})

	after, err := db.GetOrder(leg.ID)
	if err != nil {
		t.Fatalf("reload leg: %v", err)
	}
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("a station cancelled a reshuffle leg (status %s). The leg is a STEP of a dig Core is "+
			"running; it inherits station_id from its parent (compound.go:90) and the reconcile hands "+
			"the station a row for it, so the ownership check passes on an order the station never "+
			"asked for and cannot correctly reason about", after.Status)
	}

	// Non-vacuity: the leg IS cancellable — by Core, which is who owns it. If this
	// arm failed, the assertion above would be proving the UUID does not resolve.
	coreEnv := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleCore, Station: "core"}}
	d.HandleOrderCancel(coreEnv, &protocol.OrderCancel{OrderUUID: leg.EdgeUUID, Reason: "core cancel"})

	final, err := db.GetOrder(leg.ID)
	if err != nil {
		t.Fatalf("reload leg after core cancel: %v", err)
	}
	if !protocol.IsTerminal(final.Status) {
		t.Fatalf("Core could not cancel the leg either (status %s) — the station arm above proves "+
			"nothing, because the handler never reached this order at all", final.Status)
	}
}

// TestAuthority_StationStillCommandsItsOwnOrder is the other half: the fence is
// scoped to Core-created orders, not to stations.
//
// MUTATION (verified): drop the `order.ParentOrderID != nil` condition and refuse
// every non-Core envelope. This test's "must remain cancellable" assertion fires —
// which is the failure mode worth guarding, since a fence that refuses everything
// looks identical to a correct one in the test above.
func TestAuthority_StationStillCommandsItsOwnOrder(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	sd := testdb.SetupStandardData(t, db)

	own := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.StationID = "line-1"
		o.Status = StatusPending
		o.DeliveryNode = sd.LineNode.Name
	})

	stationEnv := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: own.StationID}}
	d.HandleOrderCancel(stationEnv, &protocol.OrderCancel{OrderUUID: own.EdgeUUID, Reason: "operator cancel"})

	after, err := db.GetOrder(own.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !protocol.IsTerminal(after.Status) {
		t.Fatalf("a station could not cancel its OWN order (status %s) — the authority fence is scoped "+
			"to orders Core created (ParentOrderID != nil), not to stations", after.Status)
	}
}

// TestAuthority_ForeignStationStillRefused pins the check the new arm sits in front
// of, so a rewrite cannot drop it silently while the two tests above stay green.
//
// MUTATION (verified): return true unconditionally for RoleEdge. This test fires —
// and so does the reshuffle-leg one, because that mutation drops both arms at once.
// A cleaner mutation isolating this arm alone would have to invent a third
// discriminator; what matters is that no mutation of the ParentOrderID arm on its
// own touches this test, so the two are not proving the same thing.
func TestAuthority_ForeignStationStillRefused(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	sd := testdb.SetupStandardData(t, db)

	own := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.StationID = "line-1"
		o.Status = StatusPending
		o.DeliveryNode = sd.LineNode.Name
	})

	foreignEnv := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "line-2"}}
	d.HandleOrderCancel(foreignEnv, &protocol.OrderCancel{OrderUUID: own.EdgeUUID, Reason: "wrong station"})

	after, err := db.GetOrder(own.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if protocol.IsTerminal(after.Status) {
		t.Fatalf("station line-2 cancelled line-1's order (status %s)", after.Status)
	}
}
