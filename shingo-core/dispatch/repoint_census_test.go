package dispatch

import "testing"

// repoint_census_test.go — who moves a step of a stored plan, found by parsing
// (callSites), so a new writer has to arrive here and say what it changes.
//
// repoint is the one writer of a step's node: before the splice it moves the
// step in the plan the pass sends and in the stored one, after the splice it
// plans the rest of the route again. Every other writer of steps_json is named
// with what it writes, and none of them moves a step that has waits spliced
// for it.

// TestCensus_RepointIsTheOneStepNodeWriter pins the doors that move a step and
// the other writers of a stored plan.
func TestCensus_RepointIsTheOneStepNodeWriter(t *testing.T) {
	t.Parallel()
	sources := scanCoreSources(t)
	sameSites(t, "the doors that move a step of a plan", callSites(t, sources, "repoint"), map[string]string{
		"dispatch/place_by_contents.go:repointAtRelease":  "after the splice: a carrier goes where it goes by what it holds, at the release that lifts it",
		"dispatch/lane_gate_choose.go:patchChosenDrop":    "after the splice: the lane gate's slot of its group, chosen when the robot stands at the group's spot",
		"dispatch/lane_gate_release.go:rebindGatedPickup": "after the splice: a dug bin's pickup, moved to the slot of the same lane it stands in",
		"dispatch/loader_place.go:setParkDestination":     "before the splice: a dedicated loader's return, parked at its home, a buffer or a vacated slot",
		"dispatch/loader_place.go:placeForContainment":    "before the splice: a contained part's drop, diverted to containment",
	}, "A new door that moves a step goes through repoint and is named here; a step moved any other way after the "+
		"splice keeps waits spliced for where it was (the lane gate refuses that plan on every pass).")

	sameSites(t, "the writers of a stored plan", callSites(t, sources, "UpdateOrderStepsJSON"), map[string]string{
		"dispatch/repoint.go:storePlan":                      "repoint's: a moved step and the rest of the route",
		"dispatch/complex_dispatch.go:persistWidenedPlan":    "before the splice: the pass's group choices, widened pickups and the reserve's by-contents drop (placeByContentsAtReserve)",
		"dispatch/allocator.go:reserveComplexSlots":          "before the splice: a group drop put back to its group (a fungible revert)",
		"dispatch/lane_gate_dispatch.go:buildUnsealedCreate": "the splice itself: the spliced plan, persisted before the create",
		"dispatch/lane_gate_release.go:rebindWaitLane":       "the lane the wait the robot stands at names, never a step's node",
		"dispatch/dig_dwell.go:bindDwellTail":                "a dwelling dig leg's tail, appended after its last wait: no step moves",
		"dispatch/vacate_hold.go:markHeld":                   "a held release's stamps and the turn's mark, never a node",
		"dispatch/vacated_slot.go:persistStepStamp":          "a vacated-slot stamp and its anchor, on a step repoint already moved",
	}, "A new writer of steps_json says here what it writes. One that moves a step's node goes through repoint instead.")
}
