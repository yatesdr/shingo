package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// withResidentEvacDest must be a pure override of ONE field on a COPY. The
// copy matters: the caller keeps using the real claim afterwards, and a mutation
// that leaked would rewrite the cell's configured outbound for every later read
// in the same request.
func TestWithResidentEvacDest(t *testing.T) {
	t.Parallel()
	base := &processes.NodeClaim{
		ID:                  52,
		CoreNodeName:        "ALN_006",
		PayloadCode:         "63145-6TA1B.10",
		InboundSource:       "SMN_029",
		OutboundDestination: "SMN_029",
	}

	t.Run("blank override returns the claim untouched", func(t *testing.T) {
		t.Parallel()
		got := withResidentEvacDest(base, "")
		if got != base {
			t.Errorf("blank override returned a copy; it must return the same pointer so the "+
				"common path allocates nothing (got %p, want %p)", got, base)
		}
	})

	t.Run("override replaces only OutboundDestination", func(t *testing.T) {
		t.Parallel()
		got := withResidentEvacDest(base, "SMN_031")
		if got.OutboundDestination != "SMN_031" {
			t.Errorf("OutboundDestination = %q, want SMN_031", got.OutboundDestination)
		}
		if got.InboundSource != "SMN_029" {
			t.Errorf("InboundSource = %q, want SMN_029 unchanged — the SUPPLY leg still fetches "+
				"the style that was requested; only the evac leg is redirected", got.InboundSource)
		}
		if got.PayloadCode != base.PayloadCode || got.ID != base.ID || got.CoreNodeName != base.CoreNodeName {
			t.Errorf("override altered identity fields: %+v", got)
		}
	})

	t.Run("the original is not mutated", func(t *testing.T) {
		t.Parallel()
		origDest, origSource := base.OutboundDestination, base.InboundSource
		_ = withResidentEvacDest(base, "SMN_031")
		if base.OutboundDestination != origDest || base.InboundSource != origSource {
			t.Fatalf("withResidentEvacDest mutated its argument — the caller's claim must survive "+
				"intact (OutboundDestination %q want %q, InboundSource %q want %q)",
				base.OutboundDestination, origDest, base.InboundSource, origSource)
		}
	})

	t.Run("nil claim is not a panic", func(t *testing.T) {
		t.Parallel()
		if got := withResidentEvacDest(nil, "SMN_031"); got != nil {
			t.Errorf("nil claim returned %+v, want nil", got)
		}
	})
}

// residentClaimFixture builds the Springfield shape: ONE line node that two
// styles take turns on, each with its own dedicated home as both source and
// destination. Returns the two claim ids and the process node id.
func residentClaimFixture(t *testing.T, db *store.DB) (nodeID, oldClaimID, newClaimID int64) {
	t.Helper()
	processID, nodeID := seedProcessNode(t, db)

	oldStyle, err := db.CreateStyle("STYLE-OLD", "outgoing", processID)
	if err != nil {
		t.Fatalf("create outgoing style: %v", err)
	}
	newStyle, err := db.CreateStyle("STYLE-NEW", "incoming", processID)
	if err != nil {
		t.Fatalf("create incoming style: %v", err)
	}

	// Claim 44's shape: 74871-6SA0A.06 → SMN_031.
	oldClaimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: oldStyle, CoreNodeName: "TEST-NODE", Role: "consume",
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-OLD", UOPCapacity: 100,
		InboundSource: "HOME-OLD", InboundStaging: "STAGE-IN", OutboundStaging: "STAGE-OUT",
		OutboundDestination: "HOME-OLD",
	})
	if err != nil {
		t.Fatalf("upsert outgoing claim: %v", err)
	}
	// Claim 52's shape: 63145-6TA1B.10 → SMN_029.
	newClaimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: newStyle, CoreNodeName: "TEST-NODE", Role: "consume",
		SwapMode: protocol.SwapModeTwoRobot, PayloadCode: "PART-NEW", UOPCapacity: 100,
		InboundSource: "HOME-NEW", InboundStaging: "STAGE-IN", OutboundStaging: "STAGE-OUT",
		OutboundDestination: "HOME-NEW",
	})
	if err != nil {
		t.Fatalf("upsert incoming claim: %v", err)
	}
	// The runtime row is created on first use in production; the setters below
	// are UPDATEs and silently affect nothing without it.
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime row: %v", err)
	}
	return nodeID, oldClaimID, newClaimID
}

// THE SPRINGFIELD 2026-09-02 REGRESSION.
//
// A changeover completed with this node abandoned, so the cell was recorded as
// running PART-NEW while the PART-OLD carrier still stood on it. The next
// routine request for PART-NEW must send that outgoing carrier to PART-OLD's
// home — not to PART-NEW's, which is where the requested style's claim points.
func TestResidentEvacDest_ForeignResident_RoutesToResidentHome(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, oldClaimID, newClaimID := residentClaimFixture(t, db)

	// The cell still holds the OUTGOING style's carrier.
	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &oldClaimID, nil, 0); err != nil {
		t.Fatalf("set runtime to the outgoing claim: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	requested, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read requested claim: %v", err)
	}

	got := e.residentEvacDest(rt, requested)
	if got != "HOME-OLD" {
		t.Fatalf("residentEvacDest = %q, want HOME-OLD — the carrier on the cell belongs to the "+
			"OUTGOING style, so it goes to that style's home. Returning %q would drive it onto the "+
			"incoming style's dedicated home, which is SPR 2026-09-02 exactly.",
			got, requested.OutboundDestination)
	}

	// And the override must reach the evac leg while leaving supply alone.
	swapClaim := withResidentEvacDest(requested, got)
	disp, err := BuildSwapDispatch(nil, swapClaim)
	if err != nil {
		t.Fatalf("build swap dispatch: %v", err)
	}
	if disp == nil {
		t.Fatal("nil dispatch for a two_robot claim")
	}
	if dest := finalDropoff(disp.StepsB); dest != "HOME-OLD" {
		t.Errorf("evac leg final dropoff = %q, want HOME-OLD", dest)
	}
	if src := disp.StepsA[0].Node; src != "HOME-NEW" {
		t.Errorf("supply leg opens at %q, want HOME-NEW — the supply still fetches what was "+
			"REQUESTED; only the evac leg is redirected", src)
	}
}

// The ordinary case, and the one that must not change: the resident IS the
// requested style. A blank return keeps every same-style swap in the plant on
// exactly today's path.
func TestResidentEvacDest_SameStyleResident_NoOverride(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)

	if err := db.SetProcessNodeRuntimeWithBin(nodeID, &newClaimID, nil, 0); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	rt, err := db.GetProcessNodeRuntime(nodeID)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	requested, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}

	if got := e.residentEvacDest(rt, requested); got != "" {
		t.Fatalf("residentEvacDest = %q, want \"\" — the resident claim IS the requested claim, so "+
			"there is nothing to override and the ordinary path must be untouched", got)
	}
}

// Fail-open: every unreadable input yields "", which the callers turn back into
// today's behaviour. An override guessed from a missing runtime row would
// redirect healthy carriers on every swap in the plant.
func TestResidentEvacDest_UnknownsFailOpen(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	e := testEngine(t, db)
	nodeID, _, newClaimID := residentClaimFixture(t, db)
	requested, err := db.GetStyleNodeClaim(newClaimID)
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}

	missing := int64(999999)
	tests := []struct {
		name    string
		runtime *processes.RuntimeState
		claim   *processes.NodeClaim
		because string
	}{
		{"nil runtime", nil, requested, "no runtime row yet — a fresh cell"},
		{"nil claim", &processes.RuntimeState{}, nil, "nothing to compare against"},
		{"no active claim pointer", &processes.RuntimeState{ProcessNodeID: nodeID}, requested,
			"nothing has been delivered here, so nobody owns the cell"},
		{"active claim row is gone", &processes.RuntimeState{ProcessNodeID: nodeID, ActiveClaimID: &missing}, requested,
			"a deleted style must not divert a carrier"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := e.residentEvacDest(tc.runtime, tc.claim); got != "" {
				t.Errorf("residentEvacDest = %q, want \"\" — %s", got, tc.because)
			}
		})
	}
}
