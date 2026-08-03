package scenarios

import (
	"sort"
	"testing"

	"shingo/shared/loadervectors"
	"shingocore/dispatch"
	coreloaders "shingocore/store/loaders"
	"shingoedge/domain"
)

// coreSizing is Core's sizing under the name this file compares by.
func coreSizing(threshold, currentUOP, perBinCapacity int) (int, dispatch.SizingOutcome, string) {
	return dispatch.BinsToReachThreshold(threshold, currentUOP, perBinCapacity)
}

// loader_parity_test.go — the live parity harness for the loader cutover.
//
// Core and the Edge each decide, from their own code in their own module, which
// windows an inbound carrier may go to and how many carriers a loop needs.
// The checked-in golden vectors pin the cases a person thought of. This file
// does the other job: it sweeps a large space of loader shapes, runs BOTH
// implementations over every one, and reports any disagreement.
//
// IT IS A GENERATOR, NOT THE GATE. The vectors are the gate, because they
// survive the cutover and this cannot: Deploy 5 deletes the Edge threshold path,
// and with it the implementation this file compares against. When a sweep here
// finds a divergence, the fix is to decide which side is right, change it, AND
// add the case to the vectors — so the knowledge outlives the harness. When this
// file is deleted as part of the demolition, what is lost is a way of finding
// new cases, not a way of checking known ones.
//
// This lives in integration/ because it is the only module that imports both
// shingocore and shingoedge. Neither module's own test package can compile the
// other's code.

// TestLoaderParity_SweepDeliveryTargets runs both implementations over every
// combination in a generated space and fails on the first divergence, printing
// the shape in enough detail to paste into the vectors.
func TestLoaderParity_SweepDeliveryTargets(t *testing.T) {
	t.Parallel()

	// Window/position name sets chosen to include the orderings that actually
	// bite: names where ASCII order matches intent, and names where it does not.
	nameSets := [][]string{
		{"W1"},
		{"W1", "W2"},
		{"W1", "W2", "W3"},
		{"W2", "W10"},             // W10 sorts first
		{"WB", "WA", "WC"},        // written out of order
		{"W1", "W2", "W3", "W10"}, // both traps at once
	}
	payloadSets := [][]string{
		{"P1"},
		{"P1", "P2"},
	}
	asks := []string{"", "P1", "P2", "P-UNKNOWN"}

	var checked int

	// Shared-window shapes.
	for _, names := range nameSets {
		for _, set := range payloadSets {
			for _, funnel := range []bool{false, true} {
				for _, ask := range asks {
					for _, member := range append([]string{""}, names...) {
						checked++
						coreNodes, coreBudget := coreSharedTargets(names, set, funnel, member, ask)
						edgeNodes, edgeBudget := edgeSharedTargets(t, names, set, funnel, member, ask)
						if !agree(coreNodes, coreBudget, edgeNodes, edgeBudget) {
							t.Errorf("DIVERGENCE (shared): windows=%v payloads=%v funnel=%v member=%q ask=%q\n"+
								"  core: nodes=%v budget=%d\n  edge: nodes=%v budget=%d\n"+
								"  Decide which is right, fix that side, and add the case to shared/loadervectors/vectors.json.",
								names, set, funnel, member, ask, coreNodes, coreBudget, edgeNodes, edgeBudget)
						}
					}
				}
			}
		}
	}

	// Dedicated shapes: each position gets a payload, cycling through the set so
	// the two-positions-one-payload case is covered.
	for _, names := range nameSets {
		for _, set := range payloadSets {
			pinned := make([]string, len(names))
			for i := range names {
				pinned[i] = set[i%len(set)]
			}
			for _, ask := range asks {
				for _, member := range append([]string{""}, names...) {
					checked++
					coreNodes, coreBudget := coreDedicatedTargets(names, pinned, member, ask)
					edgeNodes, edgeBudget := edgeDedicatedTargets(t, names, pinned, member, ask)
					if !agree(coreNodes, coreBudget, edgeNodes, edgeBudget) {
						t.Errorf("DIVERGENCE (dedicated): positions=%v payloads=%v member=%q ask=%q\n"+
							"  core: nodes=%v budget=%d\n  edge: nodes=%v budget=%d\n"+
							"  Decide which is right, fix that side, and add the case to shared/loadervectors/vectors.json.",
							names, pinned, member, ask, coreNodes, coreBudget, edgeNodes, edgeBudget)
					}
				}
			}
		}
	}

	// A sweep that checked nothing passes silently, which is the failure mode a
	// generator is most prone to.
	if checked < 500 {
		t.Errorf("swept only %d shapes; the space collapsed and this test is not doing its job", checked)
	}
	t.Logf("swept %d loader shapes", checked)
}

// TestLoaderParity_SweepSizing does the same for the sizing arithmetic, over a
// range that deliberately includes broken readings and broken catalog values —
// the inputs a plant actually produces when something is wrong.
func TestLoaderParity_SweepSizing(t *testing.T) {
	t.Parallel()
	var checked int
	for threshold := -10; threshold <= 500; threshold += 13 {
		for current := -600; current <= 600; current += 29 {
			for capacity := -3; capacity <= 120; capacity += 7 {
				checked++
				coreBins, coreOutcome, _ := coreSizing(threshold, current, capacity)
				edgeBins, edgeOutcome := edgeSizing(threshold, current, capacity)
				if coreBins != edgeBins || string(coreOutcome) != edgeOutcome {
					t.Fatalf("DIVERGENCE (sizing): threshold=%d current=%d capacity=%d\n"+
						"  core: %d/%s\n  edge: %d/%s\n"+
						"  Decide which is right, fix that side, and add the case to shared/loadervectors/vectors.json.",
						threshold, current, capacity, coreBins, coreOutcome, edgeBins, edgeOutcome)
				}
			}
		}
	}
	if checked < 5000 {
		t.Errorf("swept only %d sizing inputs; the space collapsed", checked)
	}
	t.Logf("swept %d sizing inputs", checked)
}

// TestLoaderParity_VectorsAreASubsetOfTheSweep checks that the checked-in
// vectors describe behaviour this sweep also produces. It is the link between
// the temporary generator and the permanent gate: a vector the sweep cannot
// reproduce is a vector that has drifted from the system.
func TestLoaderParity_VectorsAreASubsetOfTheSweep(t *testing.T) {
	t.Parallel()
	v := loadervectors.MustLoad()
	for _, c := range v.Sizing {
		coreBins, coreOutcome, _ := coreSizing(c.Threshold, c.CurrentUOP, c.PerBinCapacity)
		edgeBins, edgeOutcome := edgeSizing(c.Threshold, c.CurrentUOP, c.PerBinCapacity)
		if coreBins != c.WantBins || string(coreOutcome) != c.WantOutcome {
			t.Errorf("vector %q: core gives %d/%s, vector says %d/%s", c.Name, coreBins, coreOutcome, c.WantBins, c.WantOutcome)
		}
		if edgeBins != c.WantBins || edgeOutcome != c.WantOutcome {
			t.Errorf("vector %q: edge gives %d/%s, vector says %d/%s", c.Name, edgeBins, edgeOutcome, c.WantBins, c.WantOutcome)
		}
	}
}

// ── the two implementations, called the way production calls them ────────────

func coreSharedTargets(names, payloadSet []string, funnel bool, member, ask string) ([]string, int) {
	homes := make([]coreloaders.Home, len(names))
	nodeNames := map[int64]string{}
	for i, n := range names {
		homes[i] = coreloaders.Home{PositionNodeID: int64(i + 1)}
		nodeNames[int64(i+1)] = n
	}
	payloads := make([]coreloaders.Payload, len(payloadSet))
	for i, p := range payloadSet {
		payloads[i] = coreloaders.Payload{PayloadCode: p}
	}
	targets, budget := coreloaders.DeliveryTargets(coreloaders.DeliveryTargetsInput{
		Layout:        coreloaders.LayoutSharedWindow,
		FunnelWindows: funnel,
		Homes:         homes,
		NodeNames:     nodeNames,
		Payloads:      payloads,
		Member:        member,
		PayloadCode:   ask,
	})
	return targetNames(targets), budget
}

func coreDedicatedTargets(names, pinned []string, member, ask string) ([]string, int) {
	homes := make([]coreloaders.Home, len(names))
	nodeNames := map[int64]string{}
	for i, n := range names {
		homes[i] = coreloaders.Home{PositionNodeID: int64(i + 1), PayloadCode: pinned[i]}
		nodeNames[int64(i+1)] = n
	}
	targets, budget := coreloaders.DeliveryTargets(coreloaders.DeliveryTargetsInput{
		Layout:      coreloaders.LayoutDedicatedPositions,
		Homes:       homes,
		NodeNames:   nodeNames,
		Member:      member,
		PayloadCode: ask,
	})
	return targetNames(targets), budget
}

// edgeSharedTargets builds the Edge aggregate and asks it the same question.
//
// Windows go in in NAME order, because that is the order the running Edge builds
// them in: its cache read sorts by node name and no ordinal survives the trip
// down from Core. Feeding them in written order would compare Core against an
// Edge that does not exist.
func edgeSharedTargets(t *testing.T, names, payloadSet []string, funnel bool, member, ask string) ([]string, int) {
	t.Helper()
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	windows := make([]domain.Window, len(sorted))
	for i, n := range sorted {
		windows[i] = domain.Window{Node: domain.NodeID(n)}
	}
	set := make([]domain.PayloadCode, len(payloadSet))
	for i, p := range payloadSet {
		set[i] = domain.PayloadCode(p)
	}
	l, err := domain.NewSharedWindowLoader("loader:parity", "parity", domain.RoleProduce,
		domain.ReplenishmentThreshold, windows, set, domain.WithFunnelWindows(funnel))
	if err != nil {
		t.Fatalf("build edge shared loader %v/%v: %v", names, payloadSet, err)
	}
	nodes, budget := l.ReservationTarget(domain.NodeID(member), domain.PayloadCode(ask), !funnel)
	return nodeIDNames(nodes), budget
}

func edgeDedicatedTargets(t *testing.T, names, pinned []string, member, ask string) ([]string, int) {
	t.Helper()
	positions := make([]domain.Position, len(names))
	for i, n := range names {
		positions[i] = domain.Position{Node: domain.NodeID(n), Payload: domain.PayloadCode(pinned[i])}
	}
	l, err := domain.NewDedicatedPositionsLoader("loader:parity", "parity", domain.RoleProduce,
		domain.ReplenishmentThreshold, positions)
	if err != nil {
		t.Fatalf("build edge dedicated loader %v: %v", names, err)
	}
	nodes, budget := l.ReservationTarget(domain.NodeID(member), domain.PayloadCode(ask), true)
	return nodeIDNames(nodes), budget
}

// edgeSizing transcribes HandleLoopBelowThreshold's inline sizing, with the
// per-bin-capacity guard from its caller pulled in. Kept trivial on purpose: a
// cleverer transcription would be asserting that a rewrite matches a rewrite.
//
//	shingo-edge/engine/operator_demand_loader.go:126     capacity guard (caller)
//	shingo-edge/engine/operator_demand_loader.go:149-154 negative clamp
//	shingo-edge/engine/operator_demand_loader.go:159-165 gap, skip, round up
func edgeSizing(threshold, currentUOP, perBinCapacity int) (int, string) {
	if perBinCapacity <= 0 {
		return 0, "no_per_bin_capacity"
	}
	current := currentUOP
	if current < 0 {
		current = 0
	}
	gap := threshold - current
	if gap <= 0 {
		return 0, "at_threshold"
	}
	return (gap + perBinCapacity - 1) / perBinCapacity, "ok"
}

// ── plumbing ─────────────────────────────────────────────────────────────────

func targetNames(ts []coreloaders.Target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.NodeName
	}
	return out
}

func nodeIDNames(ns []domain.NodeID) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = string(n)
	}
	return out
}

// agree compares in ORDER. Same set in a different order is a divergence, not a
// match: the funnel case takes the first node and the spreading case fills in
// this order, so ordering decides which window a carrier physically goes to.
func agree(aNodes []string, aBudget int, bNodes []string, bBudget int) bool {
	if aBudget != bBudget || len(aNodes) != len(bNodes) {
		return false
	}
	for i := range aNodes {
		if aNodes[i] != bNodes[i] {
			return false
		}
	}
	return true
}
