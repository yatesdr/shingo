//go:build docker

package dispatch

import (
	"fmt"
	"testing"
	"time"

	"shingocore/fleet"
	"shingocore/internal/testdb"
)

// c2b_pair_pins_docker_test.go — C2b A.2, Q7: a pair whose second create fails
// after the first was accepted.
//
// Source: RULINGS-C2b-P0-2026-10-06.md §3 Q7 (a). The lifter is with the fleet
// and is never preempted (v4 §2.7 ⚖); the return demotes alone to
// `queued`/fleet_unavailable holding nothing, bound to its sibling, and
// re-acquires through the hand. "Both or neither" is the hand's rule, not the
// fleet's.

// failCreateOnce refuses the first create for one external id and accepts every
// later one: the fleet's blip.
type failCreateOnce struct {
	*testdb.MockTrackingBackend
	uuid    string
	refused bool
}

func (f *failCreateOnce) CreateOrder(req fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	if req.ExternalID == f.uuid && !f.refused {
		f.refused = true
		return fleet.TransportOrderResult{}, fmt.Errorf("mock: create refused once for %s", f.uuid)
	}
	return f.MockTrackingBackend.CreateOrder(req)
}

// At the base the return's refused create terminal-failed it as `fleet_failed`
// (dispatchComplexToFleet → failOrderInternal), and in the engine the death rule
// then cancelled the lifter (HandleSwapPeerTerminal): a robot already sent was
// called back for a blip.
//
// WHAT "HOLDING NOTHING" MEANS HERE is the hand's: no claim, no confirmed row,
// no lane. The return goes through the one door (DemoteAfterFleetRefusal), which
// keeps its paper as pending reservations, a wait's soft holds (v4 §2.6); the
// retry re-confirms them in its hand.
//
// THE LEADER'S PASS ONLY. The pair runs on its leader's call (the lower id, the
// supply here); a second call for the return is the return's own retry, as a
// solo order bound to a committed sibling.
func TestC2bPin_APairWhoseReturnIsRefusedKeepsItsLifter(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	home, _, _, _ := parkFixture(t, db)
	backend := &failCreateOnce{MockTrackingBackend: testdb.NewTrackingBackend(), uuid: "c2bq7-return"}
	d, _ := newTestDispatcher(t, db, backend)
	line, stage := prNode(t, db, "C2BQ7-LINE"), prNode(t, db, "C2BQ7-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-X", home.ID, "c2bq7-home-full", 10, now)
	makeLoaderBin(t, db, "PART-X", line.ID, "c2bq7-resident", 3, now)

	vsSubmitPair(d, "c2bq7", "PART-X", line.Name, stage.Name, home.Name, home.Name, false)
	prScanPass(t, d, db, "c2bq7-supply")

	supply := prReloadUUID(t, db, "c2bq7-supply")
	if supply.VendorOrderID == "" {
		t.Fatalf("fixture: the lifter was not sent (status %s, cause %q) — the pin needs the lifter accepted "+
			"and the return refused", supply.Status, supply.QueueCause)
	}
	ret := prReloadUUID(t, db, "c2bq7-return")
	var violations []string
	if ret.Status != StatusQueued || ret.QueueCause != string(CauseFleetRefusedCreate) {
		violations = append(violations, fmt.Sprintf("the refused return is %s under %q (error %q), want %s under %q",
			ret.Status, ret.QueueCause, ret.ErrorDetail, StatusQueued, CauseFleetRefusedCreate))
	}
	claimed, _, lanes := prHoldings(t, db, ret.ID)
	if claimed+len(lanes) > 0 {
		violations = append(violations, fmt.Sprintf("the refused return holds %d bin claim(s) and %d lane(s) "+
			"while it waits", claimed, len(lanes)))
	}
	violations = append(violations, testdb.HoldsAtRest(t, db)...)
	if len(violations) == 0 {
		// The retry: the fleet takes the return now, and the lifter was never touched.
		prScanPass(t, d, db, "c2bq7-return")
		if again := prReloadUUID(t, db, "c2bq7-return"); again.VendorOrderID == "" {
			violations = append(violations, fmt.Sprintf("the return did not dispatch on the retry (%s, %q)",
				again.Status, again.QueueCause))
		}
	}
	if s := prReloadUUID(t, db, "c2bq7-supply"); s.Status == StatusCancelled || s.Status == StatusFailed {
		violations = append(violations, fmt.Sprintf("the lifter was ended (%s) for its partner's blip", s.Status))
	}
	if len(violations) != 0 {
		t.Fatalf("Q7: %v", violations)
	}
}
