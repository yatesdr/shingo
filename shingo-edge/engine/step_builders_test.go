package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// countWaits counts the number of "wait" actions in a step list.
func countWaits(steps []protocol.ComplexOrderStep) int {
	n := 0
	for _, s := range steps {
		if s.Action == "wait" {
			n++
		}
	}
	return n
}

// stepActions returns the action sequence (e.g. ["pickup","dropoff","wait"]).
func stepActions(steps []protocol.ComplexOrderStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Action
	}
	return out
}

// ---------------------------------------------------------------------------
// BuildSwapChangeoverSteps — single_robot: 7-step Order B + stage Order A
// ---------------------------------------------------------------------------

// Single-robot legacy choreography: line-side Order B sequence + stage
// Order A.
func TestBuildSwapChangeoverSteps_SingleRobot(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-A",
		OutboundStaging:     "OUT-STAGE",
		OutboundDestination: "DEST-FINAL",
		SwapMode:            "single_robot",
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE-B",
		InboundSource:  "MARKET",
		InboundStaging: "IN-STAGE",
	}

	disp := BuildSwapChangeoverSteps(from, to, "", "")

	if len(disp.StepsB) != 7 {
		t.Fatalf("Order B: expected 7 steps, got %d", len(disp.StepsB))
	}
	if w := countWaits(disp.StepsB); w != 1 {
		t.Errorf("Order B: expected 1 wait, got %d", w)
	}
	if !disp.AutoConfirmB {
		t.Error("Order B: expected AutoConfirmB=true")
	}
	if disp.AutoConfirmA {
		t.Error("Order A (stage): expected AutoConfirmA=false (operator confirms staging)")
	}
	if disp.DeliveryNodeA != "IN-STAGE" {
		t.Errorf("Order A delivery node: got %q, want IN-STAGE", disp.DeliveryNodeA)
	}

	wantB := []protocol.ComplexOrderStep{
		{Action: "wait", Node: "CORE-A"},
		{Action: "pickup", Node: "CORE-A"},
		{Action: "dropoff", Node: "OUT-STAGE"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE-B"},
		{Action: "pickup", Node: "OUT-STAGE"},
		{Action: "dropoff", Node: "DEST-FINAL"},
	}
	for i, s := range disp.StepsB {
		if s != wantB[i] {
			t.Errorf("Order B step %d: got %+v, want %+v", i, s, wantB[i])
		}
	}

	wantA := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "MARKET"},
		{Action: "dropoff", Node: "IN-STAGE"},
	}
	if len(disp.StepsA) != len(wantA) {
		t.Fatalf("Order A: expected %d steps, got %d", len(wantA), len(disp.StepsA))
	}
	for i, s := range disp.StepsA {
		if s != wantA[i] {
			t.Errorf("Order A step %d: got %+v, want %+v", i, s, wantA[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildEvacuateChangeoverSteps — single_robot: 8-step with tooling wait
// ---------------------------------------------------------------------------

func TestBuildEvacuateChangeoverSteps_SingleRobot(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-A",
		OutboundStaging:     "OUT-STAGE",
		OutboundDestination: "DEST-FINAL",
		SwapMode:            "single_robot",
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE-B",
		InboundSource:  "MARKET",
		InboundStaging: "IN-STAGE",
	}

	disp := BuildEvacuateChangeoverSteps(from, to, "", "")

	if len(disp.StepsB) != 8 {
		t.Fatalf("Order B: expected 8 steps, got %d", len(disp.StepsB))
	}
	if w := countWaits(disp.StepsB); w != 2 {
		t.Errorf("Order B: expected 2 waits, got %d", w)
	}
	if disp.StepsB[0].Action != "wait" || disp.StepsB[0].Node != "CORE-A" {
		t.Errorf("step 0: expected wait at CORE-A, got %q at %q", disp.StepsB[0].Action, disp.StepsB[0].Node)
	}
	if disp.StepsB[3].Action != "wait" {
		t.Errorf("step 3: expected wait (tooling done), got %q", disp.StepsB[3].Action)
	}

	wantActions := []string{"wait", "pickup", "dropoff", "wait", "pickup", "dropoff", "pickup", "dropoff"}
	for i, a := range stepActions(disp.StepsB) {
		if a != wantActions[i] {
			t.Errorf("step %d action: got %q, want %q", i, a, wantActions[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildSwapChangeoverSteps per SwapMode
// ---------------------------------------------------------------------------

// two_robot Swap — Order A pre-stages → waits at staging → delivers; Order B
// drives to line → waits → evacuates straight to OutboundDestination.
func TestBuildSwapChangeoverSteps_TwoRobot(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE",
		OutboundStaging:     "OUT-STAGE",
		OutboundDestination: "DEST",
		SwapMode:            "two_robot",
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE",
		InboundSource:  "MARKET",
		InboundStaging: "IN-STAGE",
	}

	disp := BuildSwapChangeoverSteps(from, to, "", "")

	if disp.StepsA == nil || disp.StepsB == nil {
		t.Fatalf("two_robot: expected both StepsA and StepsB, got A=%v B=%v", disp.StepsA, disp.StepsB)
	}
	if !disp.AutoConfirmA || !disp.AutoConfirmB {
		t.Error("two_robot: expected both legs to auto-confirm (the wait IS the gate)")
	}
	if disp.DeliveryNodeA != "CORE" {
		t.Errorf("Order A delivery node: got %q, want CORE", disp.DeliveryNodeA)
	}

	wantA := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "MARKET"},
		{Action: "dropoff", Node: "IN-STAGE"},
		{Action: "wait", Node: "IN-STAGE"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE"},
	}
	if len(disp.StepsA) != len(wantA) {
		t.Fatalf("Order A: expected %d steps, got %d", len(wantA), len(disp.StepsA))
	}
	for i, s := range disp.StepsA {
		if s != wantA[i] {
			t.Errorf("Order A step %d: got %+v, want %+v", i, s, wantA[i])
		}
	}
	if w := countWaits(disp.StepsA); w != 1 {
		t.Errorf("two_robot Swap Order A: expected 1 wait, got %d", w)
	}

	wantB := []protocol.ComplexOrderStep{
		{Action: "wait", Node: "CORE"},
		{Action: "pickup", Node: "CORE"},
		{Action: "dropoff", Node: "DEST"},
	}
	if len(disp.StepsB) != len(wantB) {
		t.Fatalf("Order B: expected %d steps, got %d", len(wantB), len(disp.StepsB))
	}
	for i, s := range disp.StepsB {
		if s != wantB[i] {
			t.Errorf("Order B step %d: got %+v, want %+v", i, s, wantB[i])
		}
	}
}

// two_robot Evacuate has no second "tooling done" wait: with two
// independent robots, Robot B clears the line while Robot A delivers,
// so the line is naturally clear without an operator gate between the
// two legs. Step shape is identical to two_robot Swap.
func TestBuildEvacuateChangeoverSteps_TwoRobot(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE",
		OutboundStaging:     "OUT-STAGE",
		OutboundDestination: "DEST",
		SwapMode:            "two_robot",
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE",
		InboundSource:  "MARKET",
		InboundStaging: "IN-STAGE",
	}
	swap := BuildSwapChangeoverSteps(from, to, "", "")
	evac := BuildEvacuateChangeoverSteps(from, to, "", "")

	// Order A: only the shared "ready" wait, same as Swap.
	if w := countWaits(evac.StepsA); w != 1 {
		t.Errorf("Order A: expected 1 wait (ready only — no second tooling-done gate), got %d", w)
	}
	if len(evac.StepsA) != len(swap.StepsA) {
		t.Fatalf("Order A length: evac=%d swap=%d, expected match", len(evac.StepsA), len(swap.StepsA))
	}
	for i := range evac.StepsA {
		if evac.StepsA[i] != swap.StepsA[i] {
			t.Errorf("Order A step %d: evac=%+v swap=%+v, expected match", i, evac.StepsA[i], swap.StepsA[i])
		}
	}

	// Order B unchanged from Swap.
	if w := countWaits(evac.StepsB); w != 1 {
		t.Errorf("Order B: expected 1 wait, got %d", w)
	}
	if len(evac.StepsB) != len(swap.StepsB) {
		t.Fatalf("Order B length mismatch: evac=%d swap=%d", len(evac.StepsB), len(swap.StepsB))
	}
}

// two_robot_press_index Swap — 2-position layout (no SecondPairedCoreNode).
// R1 evacuates from CoreNodeName, drops at OutboundDestination, picks new
// from InboundSource, drops at PairedCoreNode (back). R2 indexes B → A.
func TestBuildSwapChangeoverSteps_PressIndex_2Pos(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "FRONT",
		PairedCoreNode:      "BACK",
		OutboundDestination: "DEST",
		SwapMode:            "two_robot_press_index",
	}
	to := &processes.NodeClaim{
		CoreNodeName:  "FRONT",
		InboundSource: "MARKET",
	}

	disp := BuildSwapChangeoverSteps(from, to, "", "")

	wantR1 := []protocol.ComplexOrderStep{
		{Action: "wait", Node: "FRONT"},
		{Action: "pickup", Node: "FRONT"},
		{Action: "dropoff", Node: "DEST"},
		{Action: "pickup", Node: "MARKET"},
		{Action: "dropoff", Node: "BACK"},
	}
	if len(disp.StepsA) != len(wantR1) {
		t.Fatalf("R1: expected %d steps, got %d", len(wantR1), len(disp.StepsA))
	}
	for i, s := range disp.StepsA {
		if s != wantR1[i] {
			t.Errorf("R1 step %d: got %+v, want %+v", i, s, wantR1[i])
		}
	}

	wantR2 := []protocol.ComplexOrderStep{
		{Action: "wait", Node: "BACK"},
		{Action: "pickup", Node: "BACK"},
		{Action: "dropoff", Node: "FRONT"},
	}
	if len(disp.StepsB) != len(wantR2) {
		t.Fatalf("R2: expected %d steps, got %d", len(wantR2), len(disp.StepsB))
	}
	for i, s := range disp.StepsB {
		if s != wantR2[i] {
			t.Errorf("R2 step %d: got %+v, want %+v", i, s, wantR2[i])
		}
	}
}

// two_robot_press_index Swap — 3-position layout. R1 refills SecondPairedCoreNode;
// R2 indexes C → B → A.
func TestBuildSwapChangeoverSteps_PressIndex_3Pos(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:         "FRONT",
		PairedCoreNode:       "MID",
		SecondPairedCoreNode: "BACK",
		OutboundDestination:  "DEST",
		SwapMode:             "two_robot_press_index",
	}
	to := &processes.NodeClaim{
		CoreNodeName:  "FRONT",
		InboundSource: "MARKET",
	}

	disp := BuildSwapChangeoverSteps(from, to, "", "")

	wantR1Last := protocol.ComplexOrderStep{Action: "dropoff", Node: "BACK"}
	if disp.StepsA[len(disp.StepsA)-1] != wantR1Last {
		t.Errorf("R1 last step: got %+v, want %+v", disp.StepsA[len(disp.StepsA)-1], wantR1Last)
	}

	wantR2 := []protocol.ComplexOrderStep{
		{Action: "wait", Node: "MID"},
		{Action: "pickup", Node: "MID"},
		{Action: "dropoff", Node: "FRONT"},
		{Action: "pickup", Node: "BACK"},
		{Action: "dropoff", Node: "MID"},
	}
	if len(disp.StepsB) != len(wantR2) {
		t.Fatalf("R2: expected %d steps, got %d", len(wantR2), len(disp.StepsB))
	}
	for i, s := range disp.StepsB {
		if s != wantR2[i] {
			t.Errorf("R2 step %d: got %+v, want %+v", i, s, wantR2[i])
		}
	}
}

// two_robot_press_index Evacuate — extra tooling-done wait on R1 between
// dropoff outbound and pickup inbound.
func TestBuildEvacuateChangeoverSteps_PressIndex_2Pos(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "FRONT",
		PairedCoreNode:      "BACK",
		OutboundDestination: "DEST",
		SwapMode:            "two_robot_press_index",
	}
	to := &processes.NodeClaim{
		CoreNodeName:  "FRONT",
		InboundSource: "MARKET",
	}
	disp := BuildEvacuateChangeoverSteps(from, to, "", "")

	if w := countWaits(disp.StepsA); w != 2 {
		t.Errorf("R1: expected 2 waits (ready + tooling done), got %d", w)
	}
	// Sequence: wait(FRONT), pickup(FRONT), dropoff(DEST), wait(""), pickup(MARKET), dropoff(BACK)
	if len(disp.StepsA) != 6 {
		t.Fatalf("R1: expected 6 steps, got %d", len(disp.StepsA))
	}
	if disp.StepsA[3].Action != "wait" || disp.StepsA[3].Node != "" {
		t.Errorf("R1 step 3: expected bare tooling-done wait, got %+v", disp.StepsA[3])
	}
}

// Per-position press-index swap dispatch. Each synthesized per-position
// diff routes to this builder via the SwapMode == pressPositionSwapMode
// case. Single complex order, 4 steps, no operator gate inside.
func TestBuildPressIndexPerPositionSwap_FourStepSequence(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "POS-A",
		PayloadCode:         "PART-A",
		SwapMode:            pressPositionSwapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	to := &processes.NodeClaim{
		CoreNodeName:        "POS-A",
		PayloadCode:         "PART-B",
		SwapMode:            pressPositionSwapMode,
		InboundSource:       "MARKET",
		OutboundDestination: "DEST",
	}
	disp := buildPressIndexPerPositionSwap(from, to)

	want := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "POS-A"},
		{Action: "dropoff", Node: "DEST"},
		{Action: "pickup", Node: "MARKET"},
		{Action: "dropoff", Node: "POS-A"},
	}
	if len(disp.StepsA) != len(want) {
		t.Fatalf("expected %d steps, got %d", len(want), len(disp.StepsA))
	}
	for i, s := range disp.StepsA {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
	if disp.StepsB != nil {
		t.Errorf("StepsB must be nil (single-order shape), got %+v", disp.StepsB)
	}
	if w := countWaits(disp.StepsA); w != 0 {
		t.Errorf("expected 0 waits (auto-confirm, no operator gate), got %d", w)
	}
	if !disp.AutoConfirmA {
		t.Error("expected AutoConfirmA=true")
	}
	if disp.DeliveryNodeA != "POS-A" {
		t.Errorf("DeliveryNodeA = %q, want POS-A", disp.DeliveryNodeA)
	}
}

// Per-position press-index missing required config → empty dispatch.
func TestBuildPressIndexPerPositionSwap_MissingConfig_EmptyDispatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		from *processes.NodeClaim
		to   *processes.NodeClaim
	}{
		{"no_outbound_destination",
			&processes.NodeClaim{CoreNodeName: "POS-A", SwapMode: pressPositionSwapMode},
			&processes.NodeClaim{InboundSource: "MARKET"}},
		{"no_inbound_source",
			&processes.NodeClaim{CoreNodeName: "POS-A", OutboundDestination: "DEST", SwapMode: pressPositionSwapMode},
			&processes.NodeClaim{}},
		{"no_core_node_name",
			&processes.NodeClaim{OutboundDestination: "DEST", SwapMode: pressPositionSwapMode},
			&processes.NodeClaim{InboundSource: "MARKET"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := buildPressIndexPerPositionSwap(tc.from, tc.to)
			if disp.StepsA != nil || disp.StepsB != nil {
				t.Errorf("expected empty dispatch, got A=%v B=%v", disp.StepsA, disp.StepsB)
			}
		})
	}
}

// Sequential Swap is a single robot, single complex order, with a
// single mid-sequence wait. The planner reads ActivePull at plan time
// and passes inactive/active node names. Pre-cutover-click steps swap
// the inactive position; the wait at the active position gates on the
// cutover click; post-cutover steps swap the active position.
//
// Direct-trip pattern (no InboundStaging hop), mirroring sequential's
// steady-state choreography (steady-state backfill goes pickup
// InboundSource → dropoff CoreNodeName, no staging).
func TestBuildSwapChangeoverSteps_Sequential(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-A",
		PairedCoreNode:      "CORE-B",
		OutboundDestination: "DEST",
		SwapMode:            "sequential",
	}
	to := &processes.NodeClaim{
		CoreNodeName:  "CORE-A",
		InboundSource: "MARKET",
	}

	// Planner-resolved: line currently pulling from CORE-B → swap CORE-A
	// (inactive) first, then wait at CORE-B (active) for cutover.
	disp := BuildSwapChangeoverSteps(from, to, "CORE-A" /* inactive */, "CORE-B" /* active */)

	want := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "CORE-A"},  // evac old inactive
		{Action: "dropoff", Node: "DEST"},   // old inactive to destination
		{Action: "pickup", Node: "MARKET"},  // fetch new inactive
		{Action: "dropoff", Node: "CORE-A"}, // deliver new inactive
		{Action: "wait", Node: "CORE-B"},    // cutover gate, parked at active
		{Action: "pickup", Node: "CORE-B"},  // evac old active (after cutover flip)
		{Action: "dropoff", Node: "DEST"},   // old active to destination
		{Action: "pickup", Node: "MARKET"},  // fetch new active
		{Action: "dropoff", Node: "CORE-B"}, // deliver new active
	}
	if len(disp.StepsA) != len(want) {
		t.Fatalf("StepsA: expected %d steps, got %d", len(want), len(disp.StepsA))
	}
	for i, s := range disp.StepsA {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
	// Single-order shape — StepsB must be nil.
	if disp.StepsB != nil {
		t.Errorf("sequential Swap is single-order; StepsB must be nil, got %+v", disp.StepsB)
	}
	// Single wait, mid-sequence, at the active position.
	if w := countWaits(disp.StepsA); w != 1 {
		t.Fatalf("expected exactly 1 wait, got %d", w)
	}
	if disp.StepsA[4].Action != "wait" || disp.StepsA[4].Node != "CORE-B" {
		t.Errorf("step 4: expected wait at active (CORE-B), got %+v", disp.StepsA[4])
	}
}

// Swap order respects ActivePull resolution. When the line is currently
// pulling from CORE-A, the inactive side is CORE-B — swap CORE-B first,
// wait at CORE-A for cutover.
func TestBuildSwapChangeoverSteps_Sequential_ActiveOnA_SwapsBFirst(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-A",
		PairedCoreNode:      "CORE-B",
		OutboundDestination: "DEST",
		SwapMode:            "sequential",
	}
	to := &processes.NodeClaim{CoreNodeName: "CORE-A", InboundSource: "MARKET"}

	disp := BuildSwapChangeoverSteps(from, to, "CORE-B" /* inactive */, "CORE-A" /* active */)

	if len(disp.StepsA) != 9 {
		t.Fatalf("expected 9 steps, got %d", len(disp.StepsA))
	}
	if disp.StepsA[0].Node != "CORE-B" {
		t.Errorf("first pickup should target inactive (CORE-B), got %+v", disp.StepsA[0])
	}
	if disp.StepsA[3].Node != "CORE-B" {
		t.Errorf("first delivery should target inactive (CORE-B), got %+v", disp.StepsA[3])
	}
	if disp.StepsA[4].Node != "CORE-A" {
		t.Errorf("cutover wait should be at active (CORE-A), got %+v", disp.StepsA[4])
	}
	if disp.StepsA[5].Node != "CORE-A" {
		t.Errorf("post-cutover pickup should target active (CORE-A), got %+v", disp.StepsA[5])
	}
}

// Sequential Evacuate emits backfill steps. Each robot:
//
//	pickup(my position) → dropoff(OutboundDestination)
//	pickup(InboundSource) → wait() → dropoff(my position)
//
// A single tooling-done click releases both bare waits.
func TestBuildEvacuateChangeoverSteps_Sequential(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-A",
		PairedCoreNode:      "CORE-B",
		OutboundDestination: "DEST",
		SwapMode:            "sequential",
	}
	to := &processes.NodeClaim{CoreNodeName: "CORE-A", InboundSource: "MARKET"}

	disp := BuildEvacuateChangeoverSteps(from, to, "", "" /* evac doesn't use inactive/active */)

	wantA := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "CORE-A"},  // evac old A
		{Action: "dropoff", Node: "DEST"},   // old A to destination
		{Action: "pickup", Node: "MARKET"},  // fetch new A (during tooling)
		{Action: "wait"},                    // bare — tooling-done shared gate
		{Action: "dropoff", Node: "CORE-A"}, // deliver new A
	}
	if len(disp.StepsA) != len(wantA) {
		t.Fatalf("StepsA: expected %d steps, got %d", len(wantA), len(disp.StepsA))
	}
	for i, s := range disp.StepsA {
		if s != wantA[i] {
			t.Errorf("StepsA step %d: got %+v, want %+v", i, s, wantA[i])
		}
	}

	wantB := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "CORE-B"},
		{Action: "dropoff", Node: "DEST"},
		{Action: "pickup", Node: "MARKET"},
		{Action: "wait"},
		{Action: "dropoff", Node: "CORE-B"},
	}
	if len(disp.StepsB) != len(wantB) {
		t.Fatalf("StepsB: expected %d steps, got %d", len(wantB), len(disp.StepsB))
	}
	for i, s := range disp.StepsB {
		if s != wantB[i] {
			t.Errorf("StepsB step %d: got %+v, want %+v", i, s, wantB[i])
		}
	}

	// One bare wait per robot — released by the tooling-done click.
	if w := countWaits(disp.StepsA); w != 1 {
		t.Errorf("StepsA: expected 1 wait, got %d", w)
	}
	if disp.StepsA[3].Node != "" {
		t.Errorf("StepsA wait should be bare (no Node), got %q", disp.StepsA[3].Node)
	}
	if w := countWaits(disp.StepsB); w != 1 {
		t.Errorf("StepsB: expected 1 wait, got %d", w)
	}
	if disp.StepsB[3].Node != "" {
		t.Errorf("StepsB wait should be bare (no Node), got %q", disp.StepsB[3].Node)
	}
}

// Sequential without PairedCoreNode → empty dispatch (planner emits
// NodeAction.Err). Tested for both Swap and Evacuate.
func TestBuildSwapChangeoverSteps_SequentialUnpaired_EmptyDispatch(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE",
		OutboundDestination: "DEST",
		SwapMode:            "sequential",
	}
	to := &processes.NodeClaim{CoreNodeName: "CORE", InboundSource: "MARKET"}
	disp := BuildSwapChangeoverSteps(from, to, "CORE", "")
	if disp.StepsA != nil || disp.StepsB != nil {
		t.Errorf("unpaired sequential should produce empty dispatch, got A=%v B=%v", disp.StepsA, disp.StepsB)
	}
}

func TestBuildEvacuateChangeoverSteps_SequentialUnpaired_EmptyDispatch(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE",
		OutboundDestination: "DEST",
		SwapMode:            "sequential",
	}
	to := &processes.NodeClaim{CoreNodeName: "CORE", InboundSource: "MARKET"}
	disp := BuildEvacuateChangeoverSteps(from, to, "", "")
	if disp.StepsA != nil || disp.StepsB != nil {
		t.Errorf("unpaired sequential should produce empty dispatch, got A=%v B=%v", disp.StepsA, disp.StepsB)
	}
}

// Sequential builder with missing inactive/active inputs → empty
// dispatch (the planner is expected to provide them; the empty-dispatch
// fallthrough surfaces a loud error in planNodeAction).
func TestBuildSwapChangeoverSteps_Sequential_MissingActivePull_EmptyDispatch(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-A",
		PairedCoreNode:      "CORE-B",
		OutboundDestination: "DEST",
		SwapMode:            "sequential",
	}
	to := &processes.NodeClaim{CoreNodeName: "CORE-A", InboundSource: "MARKET"}
	disp := BuildSwapChangeoverSteps(from, to, "", "")
	if disp.StepsA != nil {
		t.Errorf("missing inactive/active should produce empty dispatch, got %d steps", len(disp.StepsA))
	}
}

// resolveSequentialActivePull resolves inactive/active node names from
// the active-pull snapshot. Tie-break (both false) uses convention:
// CoreNodeName=inactive, PairedCoreNode=active.
func TestResolveSequentialActivePull(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{CoreNodeName: "CORE-A", PairedCoreNode: "CORE-B"}

	tests := []struct {
		name         string
		snap         map[string]bool
		wantInactive string
		wantActive   string
	}{
		{"active_on_paired", map[string]bool{"CORE-A": false, "CORE-B": true}, "CORE-A", "CORE-B"},
		{"active_on_core", map[string]bool{"CORE-A": true, "CORE-B": false}, "CORE-B", "CORE-A"},
		{"both_false_tiebreak", map[string]bool{"CORE-A": false, "CORE-B": false}, "CORE-A", "CORE-B"},
		{"empty_snapshot_tiebreak", nil, "CORE-A", "CORE-B"},
		{"both_true_tiebreak", map[string]bool{"CORE-A": true, "CORE-B": true}, "CORE-A", "CORE-B"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotInactive, gotActive := resolveSequentialActivePull(claim, tc.snap)
			if gotInactive != tc.wantInactive {
				t.Errorf("inactive: got %q, want %q", gotInactive, tc.wantInactive)
			}
			if gotActive != tc.wantActive {
				t.Errorf("active: got %q, want %q", gotActive, tc.wantActive)
			}
		})
	}

	t.Run("unpaired_returns_empty", func(t *testing.T) {
		un := &processes.NodeClaim{CoreNodeName: "CORE-A"}
		gotInactive, gotActive := resolveSequentialActivePull(un, nil)
		if gotInactive != "" || gotActive != "" {
			t.Errorf("unpaired claim should return empty strings, got (%q, %q)", gotInactive, gotActive)
		}
	})
}

// ---------------------------------------------------------------------------
// BuildKeepStagedDeliverSteps — 1 wait, stage→deliver sequence
// ---------------------------------------------------------------------------

func TestBuildKeepStagedDeliverSteps(t *testing.T) {
	t.Parallel()
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE-NODE",
		InboundSource:  "SOURCE",
		InboundStaging: "IN-STAGE",
	}

	steps := BuildKeepStagedDeliverSteps(to)

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "SOURCE"},
		{Action: "dropoff", Node: "IN-STAGE"},
		{Action: "wait"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE-NODE"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildKeepStagedEvacSteps — 1 wait, pre-position→evacuate→final
// ---------------------------------------------------------------------------

func TestBuildKeepStagedEvacSteps(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		CoreNodeName:        "CORE-NODE",
		OutboundDestination: "DEST-FINAL",
	}

	steps := BuildKeepStagedEvacSteps(from)

	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "wait", Node: "CORE-NODE"},
		{Action: "pickup", Node: "CORE-NODE"},
		{Action: "dropoff", Node: "DEST-FINAL"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildKeepStagedCombinedSteps — 1 wait, clear-then-stage sequence
// ---------------------------------------------------------------------------

func TestBuildKeepStagedCombinedSteps(t *testing.T) {
	t.Parallel()
	from := &processes.NodeClaim{
		InboundSource: "FROM-SOURCE",
	}
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE-NODE",
		InboundSource:  "TO-SOURCE",
		InboundStaging: "IN-STAGE",
	}

	steps := BuildKeepStagedCombinedSteps(from, to)

	if len(steps) != 7 {
		t.Fatalf("expected 7 steps, got %d", len(steps))
	}
	if w := countWaits(steps); w != 1 {
		t.Errorf("expected 1 wait, got %d", w)
	}

	// Verify the clear-then-stage sequence:
	// step 0: pickup from InboundStaging (grab old staged bin)
	// step 1: dropoff to fromClaim.InboundSource (return to market)
	// step 2: pickup from toClaim.InboundSource (grab new material)
	// step 3: dropoff to InboundStaging (stage new)
	if steps[0].Action != "pickup" || steps[0].Node != "IN-STAGE" {
		t.Errorf("step 0: expected pickup IN-STAGE, got %+v", steps[0])
	}
	if steps[1].Action != "dropoff" || steps[1].Node != "FROM-SOURCE" {
		t.Errorf("step 1: expected dropoff FROM-SOURCE (return old), got %+v", steps[1])
	}
	if steps[2].Action != "pickup" || steps[2].Node != "TO-SOURCE" {
		t.Errorf("step 2: expected pickup TO-SOURCE (grab new), got %+v", steps[2])
	}
	if steps[3].Action != "dropoff" || steps[3].Node != "IN-STAGE" {
		t.Errorf("step 3: expected dropoff IN-STAGE (stage new), got %+v", steps[3])
	}
	if steps[4].Action != "wait" {
		t.Errorf("step 4: expected wait, got %q", steps[4].Action)
	}

	want := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "FROM-SOURCE"},
		{Action: "pickup", Node: "TO-SOURCE"},
		{Action: "dropoff", Node: "IN-STAGE"},
		{Action: "wait"},
		{Action: "pickup", Node: "IN-STAGE"},
		{Action: "dropoff", Node: "CORE-NODE"},
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step %d: got %+v, want %+v", i, s, want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// BuildStageSteps — source → staging route
// ---------------------------------------------------------------------------

func TestBuildStageSteps(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{
		InboundSource:  "MARKET",
		InboundStaging: "STAGING-AREA",
	}
	steps := BuildStageSteps(claim)

	if steps == nil {
		t.Fatal("expected steps, got nil")
	}
	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0] != (protocol.ComplexOrderStep{Action: "pickup", Node: "MARKET"}) {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1] != (protocol.ComplexOrderStep{Action: "dropoff", Node: "STAGING-AREA"}) {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

func TestBuildStageSteps_NoInboundStaging(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{
		InboundSource: "MARKET",
		// InboundStaging empty — cannot pre-stage
	}
	steps := BuildStageSteps(claim)
	if steps != nil {
		t.Errorf("expected nil when InboundStaging is empty, got %d steps", len(steps))
	}
}

// ---------------------------------------------------------------------------
// BuildReleaseSteps — outbound routing (core → destination)
// ---------------------------------------------------------------------------

func TestBuildReleaseSteps(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{
		CoreNodeName:        "CORE-NODE",
		OutboundDestination: "DEST",
	}
	steps := BuildReleaseSteps(claim)

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	if steps[0] != (protocol.ComplexOrderStep{Action: "pickup", Node: "CORE-NODE"}) {
		t.Errorf("step 0: got %+v", steps[0])
	}
	if steps[1] != (protocol.ComplexOrderStep{Action: "dropoff", Node: "DEST"}) {
		t.Errorf("step 1: got %+v", steps[1])
	}
}

// Edge case: missing OutboundDestination → dropoff step has no Node.
// Core uses payload-based routing (global fallback).
func TestBuildReleaseSteps_MissingDestination(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{
		CoreNodeName: "CORE-NODE",
		// OutboundDestination empty
	}
	steps := BuildReleaseSteps(claim)

	if len(steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(steps))
	}
	// Step 0: pickup from core node (always present)
	if steps[0].Action != "pickup" || steps[0].Node != "CORE-NODE" {
		t.Errorf("step 0: got %+v, want pickup CORE-NODE", steps[0])
	}
	// Step 1: dropoff with no node — Core resolves via payloadCode
	if steps[1].Action != "dropoff" {
		t.Errorf("step 1 action: got %q, want dropoff", steps[1].Action)
	}
	if steps[1].Node != "" {
		t.Errorf("step 1 node: got %q, want empty string (payload-based fallback)", steps[1].Node)
	}
}

// ---------------------------------------------------------------------------
// Edge case: missing InboundSource in buildStep callers
// ---------------------------------------------------------------------------

// BuildStageSteps with empty InboundSource: pickup step has no Node.
// Core resolves the source via payloadCode.
func TestBuildStageSteps_MissingInboundSource(t *testing.T) {
	t.Parallel()
	claim := &processes.NodeClaim{
		// InboundSource empty
		InboundStaging: "STAGING-AREA",
	}
	steps := BuildStageSteps(claim)

	if steps == nil {
		t.Fatal("expected steps (InboundStaging is set), got nil")
	}
	if steps[0].Action != "pickup" {
		t.Errorf("step 0 action: got %q, want pickup", steps[0].Action)
	}
	if steps[0].Node != "" {
		t.Errorf("step 0 node: got %q, want empty string (payload-based fallback)", steps[0].Node)
	}
}

// BuildKeepStagedDeliverSteps with empty InboundSource: first pickup has no Node.
func TestBuildKeepStagedDeliverSteps_MissingInboundSource(t *testing.T) {
	t.Parallel()
	to := &processes.NodeClaim{
		CoreNodeName:   "CORE-NODE",
		InboundStaging: "IN-STAGE",
		// InboundSource empty
	}
	steps := BuildKeepStagedDeliverSteps(to)

	if steps[0].Action != "pickup" || steps[0].Node != "" {
		t.Errorf("step 0: expected pickup with empty node (fallback), got %+v", steps[0])
	}
}
