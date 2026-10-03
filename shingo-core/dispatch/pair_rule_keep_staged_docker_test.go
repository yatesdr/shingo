//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// pair_rule_keep_staged_docker_test.go — census 31: a supply that empties a
// staging node and refills it, the shape the step-aware occupancy fix is pinned
// on (binsAtStep). Helpers are pair_rule_helpers_docker_test.go's.

// ── census 31: the clear-and-restage pair ────────────────────────────────────

// TestPairRule_KeepStagedCombinedPairIsOneJob is census 31: a supply that
// collects the bin standing on inbound staging, returns it to the market, fetches
// a new carrier, stages it, waits and delivers, and its evac, go together or not
// at all.
//
// THIS IS A binsAtStep PIN, NOT A KEEP-STAGED PIN. The shape was the old
// keep-staged combined changeover (BuildKeepStagedCombinedSteps, deleted). The
// keep-staged that shipped never drops a complex leg on its spot: a wrong spare
// goes out by a plain move and the new one comes in by a plain retrieve. The
// steps here are literals, so they pin what Core does with ANY plan that empties
// a node and refills it, whoever writes one.
//
// DEFECT PIN, "both source". Fails at bcbde0d2, and would have at origin/main,
// behind two walls that ask one question the wrong way — what is on inbound
// staging NOW, when the plan needs to know what is on it at the step it gets
// there:
//
//   - the relay rule: the supply's last pickup re-collects its own staged
//     carrier, but the node holds the kept bin the supply's FIRST pickup takes
//     away, so the re-collect read as a real source and missed on every pass;
//   - the slot claim: past the reserve, ConfirmSlotClaim refused the staging slot
//     because the kept bin was on it.
//
// The destination gate already answered it by steps alone. binsAtStep is now the
// one answer all three ask. At origin/main the supply wedged on its own with its
// evac held behind it by swap_hold; under the pair rule both legs park.
//
// "new carrier dry" is COVERAGE: neither goes and neither holds anything.
//
// "both source, two payloads" is the twin the single-payload rows could not be:
// with one payload the bin collected first and the bin staged later are the
// same part, so a step that judged the wrong one by payload would still pass.
// Here the standing bin is the order's (outgoing) part and the new carrier is
// another, named on its own pickup, as a changeover's refill names it.
func TestPairRule_KeepStagedCombinedPairIsOneJob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		prefix string
		dry    bool
		twoPay bool
	}{
		{"both source", "PR31A", false, false},
		{"new carrier dry", "PR31B", true, false},
		{"both source, two payloads", "PR31C", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
			market := prNode(t, db, tc.prefix+"-MARKET")
			newSrc := prNode(t, db, tc.prefix+"-NEW-SRC")
			inStage := prNode(t, db, tc.prefix+"-IN-STAGE")
			outDest := prNode(t, db, tc.prefix+"-OUT")
			prResident(t, db, inStage, sd.BinType.ID, sd.Payload.Code, tc.prefix+"-KEPT")
			prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, tc.prefix+"-OLD")
			newPay, newAt := sd.Payload.Code, newSrc
			fetch := prPick(newSrc.Name)
			if tc.twoPay {
				// The new carrier is another part, named on its own pickup, and it
				// comes from a node group, as a market is. Widen judges a pre-wait
				// pickup at a CONCRETE node by the order's payload, not the step's
				// (complex_steps.go widenSupplyPickups; P2 in the keep-staged SHAPE),
				// which would park this row for a reason that has nothing to do
				// with binsAtStep. A group anchor is not widened.
				newPay = tc.prefix + "-NEWPART"
				testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: newPay, UOPCapacity: 100}), "new payload")
				grpType, err := db.GetNodeTypeByCode("NGRP")
				testutil.MustNoErr(t, err, "NGRP type")
				grp := &nodes.Node{Name: tc.prefix + "-NEW-GRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
				testutil.MustNoErr(t, db.CreateNode(grp), "new-carrier group")
				newAt = &nodes.Node{Name: tc.prefix + "-NEW-SLOT", Enabled: true, ParentID: &grp.ID}
				testutil.MustNoErr(t, db.CreateNode(newAt), "new-carrier slot")
				fetch = prPick(grp.Name)
				fetch.PayloadCode = newPay
				// A group pickup needs the resolver the base table's dispatcher
				// goes without.
				d, _ = newTestDispatcherWithResolver(t, db)
			}
			if !tc.dry {
				testdb.CreateBinAtNode(t, db, newPay, newAt.ID, tc.prefix+"-NEW")
			}
			supplyUUID, evacUUID := tc.prefix+"-supply", tc.prefix+"-evac"
			prSubmitLeg(d, supplyUUID, evacUUID, sd.Payload.Code, sd.LineNode.Name,
				prPick(inStage.Name), prDrop(market.Name), fetch, prDropExcl(inStage.Name),
				prWait(""), prPick(inStage.Name), prDrop(sd.LineNode.Name))
			prSubmitLeg(d, evacUUID, supplyUUID, sd.Payload.Code, sd.LineNode.Name,
				prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(outDest.Name))

			prScanPass(t, d, db, evacUUID, supplyUUID)

			s, e := prReloadUUID(t, db, supplyUUID), prReloadUUID(t, db, evacUUID)
			if !tc.dry {
				if s.VendorOrderID == "" || e.VendorOrderID == "" {
					t.Fatalf("the clear-and-restage pair did not go in one pass: supply %q/%q (%q), evac %q/%q (%q)",
						s.Status, s.QueueCause, s.QueueReason, e.Status, e.QueueCause, e.QueueReason)
				}
				return
			}
			if s.VendorOrderID != "" || e.VendorOrderID != "" {
				t.Fatalf("a clear-and-restage leg went with the new carrier dry: supply %q, evac %q",
					s.VendorOrderID, e.VendorOrderID)
			}
			for _, o := range []*orders.Order{s, e} {
				prAssertHoldsNothing(t, db, o, "a leg of a parked clear-and-restage pair")
			}
		})
	}
}
