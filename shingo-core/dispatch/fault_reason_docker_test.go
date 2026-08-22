//go:build docker

// fault_reason_docker_test.go — the fault's reason reaches the history row.
//
// Before 2026-08-22 all 730 faulted rows in a 30-day Springfield window carried
// the identical detail "fleet state: FAILED", a NULL code, and a ref that said
// where and had nowhere to say why. These tests pin the three rows a fault can
// write — the fault, the recovery, and the give-up — and assert what each one
// records about itself.

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/orders"
)

// latestHistory returns the order's most recent row for a status, failing the
// test if there is none.
func latestHistory(t *testing.T, db *store.DB, orderID int64, status protocol.Status) *orders.History {
	t.Helper()
	h, err := db.LatestOrderHistoryForStatus(orderID, status)
	testutil.MustNoErr(t, err, "latest history")
	if h == nil {
		t.Fatalf("order %d has no %s history row", orderID, status)
	}
	return h
}

// The fault row records the fleet's reason, keeps its code empty, and does not
// lose where the order was.
func TestMarkFaulted_RowCarriesTheFleetsReasonAndStaysUncoded(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lc, _ := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fault-ref-1", StatusInTransit)

	ref := protocol.TermRef{
		Node: "ALN_003", Payload: "74577-6SA0A.06",
		VendorCode: 60011, VendorDesc: "cannot replan",
	}
	testutil.MustNoErr(t, lc.MarkFaulted(ord, "AMR-04", ref, "Replanning"), "MarkFaulted")

	h := latestHistory(t, db, ord.ID, StatusFaulted)
	if h.Ref == nil {
		t.Fatal("faulted row stored a NULL ref — the fleet's reason was dropped")
	}
	if h.Ref.VendorCode != 60011 || h.Ref.VendorDesc != "cannot replan" {
		t.Errorf("vendor pair = %+v, want 60011/cannot replan", *h.Ref)
	}
	// Where must survive alongside why. historyReason only defaults node and
	// payload when the ref is EMPTY, so a ref carrying just a vendor code would
	// record the reason and silently lose the location.
	if h.Ref.Node != "ALN_003" || h.Ref.Payload != "74577-6SA0A.06" {
		t.Errorf("faulted row lost where it happened: %+v", *h.Ref)
	}
	// code stays empty. "" is the documented value for uncoded, and one vendor
	// code over 22 events is not a vocabulary — inventing a FaultCode here
	// would be the category error historyReason's comment warns about.
	if h.Code != "" {
		t.Errorf("faulted row code = %q, want empty — faulted rows are uncoded", h.Code)
	}
	if h.Actor != "fleet" {
		t.Errorf("actor = %q, want fleet", h.Actor)
	}
	if h.Detail != "Replanning" {
		t.Errorf("detail = %q, want the sentence that was true when it was written", h.Detail)
	}
}

// A ref with no vendor fields still gets the order's own node and payload — the
// pre-existing behaviour, unchanged.
func TestMarkFaulted_EmptyRefStillDefaultsToTheOrdersOwnNode(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lc, _ := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fault-ref-2", StatusInTransit)

	testutil.MustNoErr(t, lc.MarkFaulted(ord, "AMR-04", protocol.TermRef{}, "Replanning"), "MarkFaulted")

	h := latestHistory(t, db, ord.ID, StatusFaulted)
	if h.Ref == nil || h.Ref.Node != "DELV.1" {
		t.Errorf("ref = %+v, want the order's own delivery node", h.Ref)
	}
	if h.Ref.VendorCode != 0 {
		t.Errorf("a fleet that said nothing must not produce a code: %+v", *h.Ref)
	}
}

// The recovery row says it recovered, and says what it recovered from. 706 of
// 730 faults end here and every one of them used to write the same row an
// order that was never in trouble writes.
func TestMarkFaultedRecovered_RowSaysRecoveredAndCarriesTheFaultsRef(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lc, emitter := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fault-recover-ref-1", StatusFaulted)

	ref := protocol.TermRef{Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan"}
	testutil.MustNoErr(t,
		lc.MarkFaultedRecovered(ord, "AMR-04", ref, "Recovered after 18 s"),
		"MarkFaultedRecovered")

	if ord.Status != StatusInTransit {
		t.Fatalf("status = %q, want in_transit", ord.Status)
	}
	h := latestHistory(t, db, ord.ID, StatusInTransit)
	if h.Detail != "Recovered after 18 s" {
		t.Errorf("detail = %q — a recovery must be distinguishable from an ordinary transit row", h.Detail)
	}
	if h.Ref == nil || h.Ref.VendorCode != 60011 {
		t.Errorf("recovery row must carry the reason it recovered from, got %+v", h.Ref)
	}
	// The {faulted → in_transit} action still fires, so the engine's recovery
	// subscribers are unaffected by the reason threading.
	if len(emitter.faultedRecovered) != 1 {
		t.Errorf("expected 1 faultedRecovered emit, got %d", len(emitter.faultedRecovered))
	}
}

// Grace expiry: the terminal row carries the typed code AND the fault's reason.
// The poller has dropped its deadline entry by the time this fires, so the
// faulted history row is the last place that reason exists.
func TestFailWithRef_GraceTimeoutRowCarriesTheVendorReason(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lc, _ := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fault-graceout-1", StatusFaulted)

	ref := protocol.TermRef{Node: "ALN_003", VendorCode: 60011, VendorDesc: "cannot replan"}
	testutil.MustNoErr(t, lc.FailWithRef(ord, "edge.test",
		string(protocol.TermGraceTimeout), "Gave up after 45m · cannot replan (60011)", ref),
		"FailWithRef")

	h := latestHistory(t, db, ord.ID, StatusFailed)
	if h.Code != string(protocol.TermGraceTimeout) {
		t.Errorf("code = %q, want the typed grace_timeout constant", h.Code)
	}
	if h.Ref == nil || h.Ref.VendorCode != 60011 {
		t.Errorf("terminal row must carry the fault's reason, got %+v", h.Ref)
	}
	if h.Detail != "Gave up after 45m · cannot replan (60011)" {
		t.Errorf("detail = %q", h.Detail)
	}
}

// Fail without a ref is unchanged: the order's own node and payload, as before.
func TestFail_WithoutARefIsUnchanged(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lc, _ := newLifecycleForTest(t, db)
	ord := makeOrderAt(t, db, "fault-plainfail-1", StatusInTransit)

	testutil.MustNoErr(t, lc.Fail(ord, "edge.test", "fleet_failed", "fleet order failed"), "Fail")

	h := latestHistory(t, db, ord.ID, StatusFailed)
	if h.Code != "fleet_failed" {
		t.Errorf("code = %q, want fleet_failed", h.Code)
	}
	if h.Ref == nil || h.Ref.Node != "DELV.1" {
		t.Errorf("ref = %+v, want the order's own delivery node", h.Ref)
	}
	if h.Ref.VendorCode != 0 {
		t.Errorf("a plain Fail must not invent a vendor code: %+v", *h.Ref)
	}
}
