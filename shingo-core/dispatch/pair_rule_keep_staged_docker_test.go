//go:build docker

package dispatch

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// pair_rule_keep_staged_docker_test.go — census 31: the keep-staged combined
// pair, the shape the step-aware occupancy fix is pinned on (binsAtStep).
// Helpers are pair_rule_helpers_docker_test.go's.

// ── census 31: the keep-staged combined pair ────────────────────────────────

// TestPairRule_KeepStagedCombinedPairIsOneJob is census 31: planKeepStagedAction's
// combined supply (collect the kept bin from inbound staging, return it to the
// market, fetch the new style's carrier, stage it, wait, deliver) and its evac go
// together or not at all. This is how sequential and single_robot reach the pair
// rule at a changeover.
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
// Keep-staged is withheld from plant configuration, so no plant can build this
// plan today; it is still the shape the occupancy fix is pinned on.
//
// "new carrier dry" is COVERAGE: neither goes and neither holds anything.
func TestPairRule_KeepStagedCombinedPairIsOneJob(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		prefix string
		dry    bool
	}{
		{"both source", "PR31A", false},
		{"new carrier dry", "PR31B", true},
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
			if !tc.dry {
				testdb.CreateBinAtNode(t, db, sd.Payload.Code, newSrc.ID, tc.prefix+"-NEW")
			}
			supplyUUID, evacUUID := tc.prefix+"-supply", tc.prefix+"-evac"
			prSubmitLeg(d, supplyUUID, evacUUID, sd.Payload.Code, sd.LineNode.Name,
				prPick(inStage.Name), prDrop(market.Name), prPick(newSrc.Name), prDropExcl(inStage.Name),
				prWait(""), prPick(inStage.Name), prDrop(sd.LineNode.Name))
			prSubmitLeg(d, evacUUID, supplyUUID, sd.Payload.Code, sd.LineNode.Name,
				prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(outDest.Name))

			prScanPass(t, d, db, evacUUID, supplyUUID)

			s, e := prReloadUUID(t, db, supplyUUID), prReloadUUID(t, db, evacUUID)
			if !tc.dry {
				if s.VendorOrderID == "" || e.VendorOrderID == "" {
					t.Fatalf("the keep-staged pair did not go in one pass: supply %q/%q (%q), evac %q/%q (%q)",
						s.Status, s.QueueCause, s.QueueReason, e.Status, e.QueueCause, e.QueueReason)
				}
				return
			}
			if s.VendorOrderID != "" || e.VendorOrderID != "" {
				t.Fatalf("a keep-staged leg went with the new carrier dry: supply %q, evac %q",
					s.VendorOrderID, e.VendorOrderID)
			}
			for _, o := range []*orders.Order{s, e} {
				prAssertHoldsNothing(t, db, o, "a leg of a parked keep-staged pair")
			}
		})
	}
}
