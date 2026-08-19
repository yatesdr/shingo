//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// orphan_lane_config_fault_docker_test.go — the THIRD arm of the node read, the
// one the read/absence split left untested at every one of its three sites.
//
// A lane read can come back three ways and the planners branch three ways: the
// database did not answer (park), there is no such lane (terminal, "does not
// exist"), and — the arm here — the lane is real, readable, and has no parent
// group. That last one is not an absence and its message must not read like one:
// nothing is missing, the lane simply has nowhere to park a blocker, and an
// engineer sent looking for a lane that "does not exist" will find it sitting
// right there and learn nothing.
//
// It is terminal, and correctly so. The parent group is what supplies the
// shuffle slots a dig moves blockers into, so a dig in a parentless lane has no
// plan available to it at any future moment — the wait-not-fail rule does not
// apply to a demand no amount of waiting can serve.
//
// Before this file a repo-wide grep for the sentence found three production
// sites and zero tests.

// orphanLane builds a LANE-typed node with NO parent group, plus the slots and
// bins that make a burial in it real.
//
// The parentless lane is not a contrivance: a lane created before its group, a
// group deleted out from under its lanes, and a plant CSV that names a group
// column nothing was imported for all produce exactly this row.
func orphanLane(t *testing.T, db *store.DB, prefix string) (lane *nodes.Node, slots []*nodes.Node, bp *payloads.Payload) {
	t.Helper()
	lanType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get the LANE node type")

	bp = &payloads.Payload{Code: prefix + "-PAY"}
	testutil.MustNoErr(t, db.CreatePayload(bp), "create payload")

	// ParentID deliberately left nil — that IS the fixture.
	lane = &nodes.Node{Name: prefix + "-ORPHAN-L1", NodeTypeID: &lanType.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create the parentless lane")

	slots = make([]*nodes.Node, 2)
	for d := 1; d <= 2; d++ {
		depth := d
		s := &nodes.Node{
			Name:     fmt.Sprintf("%s-ORPHAN-L1-S%d", prefix, depth),
			ParentID: &lane.ID, Enabled: true, Depth: &depth,
		}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		slots[d-1] = s
	}

	lane, err = db.GetNode(lane.ID)
	testutil.MustNoErr(t, err, "read the lane back")
	if lane.ParentID != nil {
		t.Fatalf("fixture: the lane came back with parent %d — this test would be exercising the "+
			"ordinary planning path", *lane.ParentID)
	}
	return lane, slots, bp
}

// TestPlanBuriedReshuffle_LaneInNoNodeGroup_TerminalWithItsOwnMessage is the dig
// planner's arm.
//
// The distinctness assertion is the substance. Both arms are terminal and both
// carry codeInvalidNode, so the CODE cannot tell an engineer which of the two
// facts is true — only the sentence can, and the two facts have different fixes
// (create the lane / attach the lane to a group).
//
// MUTATION (verified): in planning_service.go planBuriedReshuffle, fold the
// parentless arm into the one above it — `if err != nil || lane == nil ||
// lane.ParentID == nil { ...configFailureID("lane node", buried.LaneID)... }`,
// which is the shape the code had before the split. The disposition and the code
// are unchanged, so the message assertions are the whole signal: three wording
// checks and the "does not exist" check all fire on
// `config failure: lane node id 2 does not exist`.
func TestPlanBuriedReshuffle_LaneInNoNodeGroup_TerminalWithItsOwnMessage(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lane, slots, bp := orphanLane(t, db, "PBRORPH")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-PBRORPH-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-PBRORPH-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	order := mkDigOrder(t, db, "dig-orphan-lane", bp.Code, "LINE-PBRORPH")

	_, pe := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})
	if pe == nil {
		t.Fatal("the dig planned a compound in a lane with no group — PlanReshuffle would be searching " +
			"for shuffle slots under a group that does not exist")
	}
	if pe.Code != codeInvalidNode {
		t.Fatalf("refused with code %q (%s), want %q", pe.Code, pe.Detail, codeInvalidNode)
	}
	if pe.Transient() {
		t.Fatal("a parentless lane is Transient(), so the dig parks and retries forever against a " +
			"group that will not appear until someone configures it")
	}
	for _, want := range []string{"config failure", lane.Name, "not in a node group", "nowhere to park a blocker"} {
		if !strings.Contains(pe.Detail, want) {
			t.Errorf("message %q is missing %q — it has to name the lane and say what about it is "+
				"wrong, because the fix is a configuration change and nobody can make it from a code",
				pe.Detail, want)
		}
	}

	// DISTINCT FROM THE MISSING-NODE ARM. Compared against the sentence that arm
	// would produce FOR THIS LANE, not against the sentence it produced for some
	// other id: two messages that differ only in the number they carry are the
	// same message, and the id is the one part an engineer cannot use to tell the
	// two faults apart.
	if pe.Detail == configFailureID("lane node", lane.ID) {
		t.Fatalf("the parentless lane produces the missing-lane sentence (%q) — the two have "+
			"different fixes (create the lane / attach it to a group) and nobody reading this can "+
			"tell which one they have", pe.Detail)
	}
	if strings.Contains(pe.Detail, "does not exist") {
		t.Errorf("the parentless-lane message %q says the lane does not exist. It does exist; it is "+
			"lane %d, and someone will go looking for a missing node and find one sitting there",
			pe.Detail, lane.ID)
	}

	// The missing arm still says its own thing, driven through the same planner so
	// the pair is pinned rather than one side of it.
	_, missing := d.planner.planBuriedReshuffle(order, &BuriedError{Bin: target, Slot: slots[1], LaneID: 9_000_001})
	if missing == nil || missing.Code != codeInvalidNode {
		t.Fatalf("an absent lane must still be a terminal invalid_node, got %v", missing)
	}
	if !strings.Contains(missing.Detail, "does not exist") {
		t.Errorf("the absent-lane message %q no longer says the lane does not exist", missing.Detail)
	}
}

// TestComplexBuriedOnReplay_LaneInNoNodeGroup_FailsWithTheConfigMessage is the
// same fact at the complex-order site, and it is a separate test rather than a
// loop because the DISPOSITION is reached differently: the dig planner returns a
// planningError for its caller to act on, while this path terminalizes the order
// itself through failOrderInternal. A shared arm is not the same as a shared
// outcome, and the outcome is what the row is about.
//
// MUTATION (verified): in complex_reshuffle.go handleComplexBuriedOnReplay,
// replace the parentless arm's message with configFailureID("lane node",
// buried.LaneID) — the same swap as the dig planner's mutation. The order still
// fails and still carries invalid_node; the error_detail assertions fire.
func TestComplexBuriedOnReplay_LaneInNoNodeGroup_FailsWithTheConfigMessage(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	lane, slots, bp := orphanLane(t, db, "CPXORPH")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-CPXORPH-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-CPXORPH-TGT")

	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "complex-orphan-lane"
		o.OrderType = OrderTypeComplex
		o.Status = StatusQueued
		o.PayloadCode = bp.Code
	})

	d.handleComplexBuriedOnReplay(parent, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	after, err := db.GetOrder(parent.ID)
	testutil.MustNoErr(t, err, "read the parent back")
	if after.Status != protocol.StatusFailed {
		t.Fatalf("parent status = %q, want %q — nothing about this lane changes without a human, so "+
			"leaving the order acquiring parks it forever", after.Status, protocol.StatusFailed)
	}
	for _, want := range []string{"config failure", lane.Name, "not in a node group"} {
		if !strings.Contains(after.ErrorDetail, want) {
			t.Errorf("error_detail %q is missing %q", after.ErrorDetail, want)
		}
	}
	if strings.Contains(after.ErrorDetail, "does not exist") {
		t.Errorf("error_detail %q reports the lane as absent; it is lane %d and it is right there",
			after.ErrorDetail, lane.ID)
	}

	// THE CODE REACHES THE EDGE TOO. The row's detail is what a human reads; the
	// emitted code is what the station's own surface routes on.
	if len(emitter.failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(emitter.failed))
	}
	if emitter.failed[0].errorCode != codeInvalidNode {
		t.Errorf("emitted error code = %q, want %q", emitter.failed[0].errorCode, codeInvalidNode)
	}
}
