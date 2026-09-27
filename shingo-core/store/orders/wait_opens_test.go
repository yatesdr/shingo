package orders_test

import (
	"testing"

	"shingocore/store/orders"
)

// TestWaitOpens_TheHistoryOpensARowWhereTheStationIsTold pins the one predicate
// for a change of wait: a history row opens on the first wait and on a change
// the station is told (WaitTold), and on nothing else. A count moving inside
// the sentence, or a cause changing under the same sentence, rewrites the live
// columns only; the row it would have opened would repeat the one before it.
func TestWaitOpens_TheHistoryOpensARowWhereTheStationIsTold(t *testing.T) {
	t.Parallel()
	material := orders.Wait{Code: "waiting_for_material", Cause: "reserve-holding", Reason: "Waiting for material: PART-A"}
	for _, tc := range []struct {
		name       string
		prev, next orders.Wait
		opens      bool
	}{
		{"the first wait", orders.Wait{}, material, true},
		{"a new code", material, orders.Wait{Code: "waiting_for_slot", Cause: "read-failed", Reason: "Waiting for a slot"}, true},
		{"a new cause with a new sentence", material,
			orders.Wait{Code: material.Code, Cause: "claim-failed", Reason: "Waiting for material: PART-B"}, true},
		{"a new cause under the same sentence", material,
			orders.Wait{Code: material.Code, Cause: "claim-failed", Reason: material.Reason}, false},
		{"a count moving in the sentence", material,
			orders.Wait{Code: material.Code, Cause: material.Cause, Reason: "Waiting for material: PART-A (2 ahead)"}, false},
		{"a clear", material, orders.Wait{}, false},
	} {
		if got := orders.WaitOpens(tc.prev, tc.next); got != tc.opens {
			t.Errorf("%s: WaitOpens = %v, want %v", tc.name, got, tc.opens)
		}
		if tc.prev.Code != "" && tc.next.Code != "" && orders.WaitTold(tc.prev, tc.next) != tc.opens {
			t.Errorf("%s: the history and the station disagree: WaitTold = %v, WaitOpens = %v",
				tc.name, orders.WaitTold(tc.prev, tc.next), tc.opens)
		}
	}
}
