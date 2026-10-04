package engine

// release_fixtures_test.go — plant-shaped fixtures for the release harness.
//
// Every fixture builds its orders through the doors a plant uses —
// RequestProduceSwap, RequestNodeMaterial, StartProcessChangeover — so the
// steps are the real builders' output, never a restatement of it. A fixture
// that writes its own steps can only pin the shapes its author already had in
// mind; that is how the 3-position unflipped pair's drop at C went unread.
//
// The runtime row is stamped the way a node looks after it has completed an
// order (active_claim_id set, a count on the slot, a bound bin) unless the
// fixture says otherwise: an unstamped slot is a node's FIRST cycle, a real
// state with its own behaviour, and a fixture that is unstamped by accident
// measures the wrong door (the curtain review's error 9).

import (
	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// Synthetic plant vocabulary. No customer part numbers.
const (
	fxPart     = "PART-X"
	fxPartNext = "PART-Y"
	fxPress    = "SYN-PRESS"
	fxPressB   = "SYN-PRESS-B"
	fxPressC   = "SYN-PRESS-C"
	fxInSrc    = "SYN-MARKET-IN"
	fxInStage  = "SYN-IN-STAGING"
	fxOutStage = "SYN-OUT-STAGING"
	fxOutDest  = "SYN-MARKET-OUT"
	fxBin      = int64(9001)
	fxCount    = 42
)

// pairSpec describes a steady-state swap node.
type pairSpec struct {
	mode      protocol.SwapMode
	role      protocol.ClaimRole // ClaimRoleProduce (default) or ClaimRoleConsume
	threePos  bool               // press-index with a second paired position
	flipped   bool               // IndexRobotSupplies
	unstamped bool               // the node's first cycle: runtime.active_claim_id never written
}

func (s pairSpec) claimRole() protocol.ClaimRole {
	if s.role == "" {
		return protocol.ClaimRoleProduce
	}
	return s.role
}

// claimInput is the one claim shape every steady-state fixture uses.
func (s pairSpec) claimInput(styleID int64, payload string) processes.NodeClaimInput {
	in := processes.NodeClaimInput{
		StyleID:             styleID,
		CoreNodeName:        fxPress,
		Role:                s.claimRole(),
		SwapMode:            s.mode,
		PayloadCode:         payload,
		UOPCapacity:         100,
		InboundSource:       fxInSrc,
		InboundStaging:      fxInStage,
		OutboundStaging:     fxOutStage,
		OutboundDestination: fxOutDest,
	}
	if s.flipped {
		flip := true
		in.IndexRobotSupplies = &flip
	}
	switch s.mode {
	case protocol.SwapModeSingleRobot:
	case protocol.SwapModeSequential:
		in.PairedCoreNode = fxPressB
	default:
		in.PairedCoreNode = fxPressB
		if s.threePos {
			in.SecondPairedCoreNode = fxPressC
		}
	}
	return in
}

// seedNode creates the process, the front node, one style and its claim, and
// stamps the runtime unless the spec says first cycle. Returns the claim.
func (h *relHarness) seedNode(s pairSpec) *processes.NodeClaim {
	h.t.Helper()
	db := h.db
	pid, err := db.CreateProcess("SYN-PROC", "release harness", "active_production", "", "", false)
	testutil.MustNoErr(h.t, err, "create process")
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "SYN-STATION"})
	testutil.MustNoErr(h.t, err, "create station")
	h.stationID = sid
	nid, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: fxPress, Code: "SYN1", Name: fxPress, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(h.t, err, "create node")
	styleID, err := db.CreateStyle("SYN-STYLE", "", pid)
	testutil.MustNoErr(h.t, err, "create style")
	testutil.MustNoErr(h.t, db.SetActiveStyle(pid, &styleID), "set active style")
	claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, s.claimInput(styleID, fxPart))
	testutil.MustNoErr(h.t, err, "upsert claim")
	_, err = db.EnsureProcessNodeRuntime(nid)
	testutil.MustNoErr(h.t, err, "ensure runtime")
	if !s.unstamped {
		testutil.MustNoErr(h.t, db.SetProcessNodeRuntime(nid, &claimID, fxCount), "stamp claim + count")
		bin := fxBin
		testutil.MustNoErr(h.t, db.SetProcessNodeActiveBinID(nid, &bin), "bind bin")
	}
	h.processID, h.nodeID = pid, nid
	node, err := db.GetProcessNode(nid)
	testutil.MustNoErr(h.t, err, "get node")
	claim := requestedClaimAtNode(db, node)
	if claim == nil {
		h.t.Fatal("fixture: claim did not resolve")
	}
	return claim
}

// steadyPair builds a steady-state two-robot swap through the operator's own
// REQUEST door and names its legs by their STEPS: evac lifts the node's bin,
// supply places one. Both legs are left where the door left them; a cell sets
// the statuses it needs.
func (h *relHarness) steadyPair(s pairSpec) {
	h.t.Helper()
	claim := h.seedNode(s)
	var res *NodeOrderResult
	var err error
	if s.claimRole() == protocol.ClaimRoleProduce {
		res, err = h.eng.RequestProduceSwap(h.nodeID)
	} else {
		res, err = h.eng.RequestNodeMaterial(h.nodeID, 1)
	}
	testutil.MustNoErr(h.t, err, "request swap")
	if res == nil || res.OrderA == nil || res.OrderB == nil {
		h.t.Fatalf("fixture: %s did not build a two-leg swap: %+v", s.mode, res)
	}
	evac, supply, ok := h.eng.classifySwapLegsBySteps(claim.CoreNodeName, res.OrderB.ID, res.OrderA.ID)
	if !ok {
		h.t.Fatalf("fixture: legs %d/%d did not classify by steps", res.OrderA.ID, res.OrderB.ID)
	}
	h.addLeg("evac", evac)
	h.addLeg("supply", supply)
}

// ── Changeovers ──────────────────────────────────────────────────────────

// coSpec describes a changeover at the front node. The changeover is built by
// StartProcessChangeover, so the planner, the tooling decorator and the node
// tasks are all the real ones.
type coSpec struct {
	mode     protocol.SwapMode
	role     protocol.ClaimRole
	threePos bool
	flipped  bool
	// tooling selects the evacuate situation (same payload, EvacuateOnChangeover
	// on the outgoing claim): the shape whose evac leg carries a second station
	// wait, "tooling done".
	tooling bool
	// marked marks the press's positions for tooling clearance
	// (ChangeoverEvacNodes on the outgoing claim; the incoming claim stages at
	// SYN-IN-STAGING). The tooling decorator then gives every inbound leg a
	// staging hold (holdInbound / holdComplexInbound).
	marked bool
	// markedOnly overrides the marked set (a subset of the press's positions).
	markedOnly []string
	// perPosition gives the two styles different bin types, so the planner
	// fans a press-index press out into one order per position
	// (buildPressIndexPerPositionSwap).
	perPosition bool
	// carryover marks the positions, keeps the part, and has the cell take the
	// bin out to outbound staging and back (setCarryoverRoundTrip).
	carryover bool
	// drop: the incoming style has no claim at the node.
	drop bool
	// toRole gives the incoming style's claim a different role (a produce press
	// changing over to a consume part: produce→consume).
	toRole protocol.ClaimRole
	// neighbour adds a second node in the same process whose claim is identical
	// in both styles — an `unchanged` task — with its own steady-state pair.
	neighbour bool
	// curtained arms the front node's curtain, reading safe, before the
	// changeover starts, so its creations see the node curtained (S7).
	curtained bool
}

// changeover seeds from/to styles, starts the changeover, and names the front
// node task's legs "evac" (OldMaterialReleaseOrderID) and "supply"
// (NextMaterialOrderID). Returns the changeover id.
func (h *relHarness) changeover(s coSpec) int64 {
	h.t.Helper()
	db := h.db
	ps := pairSpec{mode: s.mode, role: s.role, threePos: s.threePos, flipped: s.flipped}
	pid, err := db.CreateProcess("SYN-CO-PROC", "release harness changeover", "active_production", "", "", false)
	testutil.MustNoErr(h.t, err, "create process")
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "SYN-CO-STATION"})
	testutil.MustNoErr(h.t, err, "create station")
	h.stationID = sid
	nid, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, OperatorStationID: &sid, CoreNodeName: fxPress, Code: "SYN1", Name: fxPress, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(h.t, err, "create node")
	fromID, err := db.CreateStyle("SYN-FROM", "", pid)
	testutil.MustNoErr(h.t, err, "create from style")
	toID, err := db.CreateStyle("SYN-TO", "", pid)
	testutil.MustNoErr(h.t, err, "create to style")
	testutil.MustNoErr(h.t, db.SetActiveStyle(pid, &fromID), "set active style")

	from := ps.claimInput(fromID, fxPart)
	to := ps.claimInput(toID, fxPartNext)
	if s.toRole != "" {
		to.Role = s.toRole
	}
	if s.tooling {
		to.PayloadCode = fxPart
		from.EvacuateOnChangeover = true
	}
	if s.marked {
		positions := []string{fxPress}
		if from.PairedCoreNode != "" {
			positions = append(positions, from.PairedCoreNode)
		}
		if from.SecondPairedCoreNode != "" {
			positions = append(positions, from.SecondPairedCoreNode)
		}
		from.ChangeoverEvacNodes = &positions
	}
	if s.carryover {
		to.PayloadCode = fxPart
		positions := []string{fxPress}
		from.ChangeoverEvacNodes = &positions
		d := domain.CarryoverOutboundStaging
		from.ChangeoverCarryoverDisposition = &d
	}
	if s.perPosition {
		h.core.binTypes = map[string]string{fxPart: "SYN-BIN-A", fxPartNext: "SYN-BIN-B"}
	}
	if len(s.markedOnly) > 0 {
		positions := append([]string(nil), s.markedOnly...)
		from.ChangeoverEvacNodes = &positions
	}
	fromClaimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, from)
	testutil.MustNoErr(h.t, err, "from claim")
	if !s.drop {
		_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, to)
		testutil.MustNoErr(h.t, err, "to claim")
	}
	_, err = db.EnsureProcessNodeRuntime(nid)
	testutil.MustNoErr(h.t, err, "ensure runtime")
	testutil.MustNoErr(h.t, db.SetProcessNodeRuntime(nid, &fromClaimID, fxCount), "stamp claim + count")
	bin := fxBin
	testutil.MustNoErr(h.t, db.SetProcessNodeActiveBinID(nid, &bin), "bind bin")

	if s.mode == protocol.SwapModeSequential {
		// The back position is its own node with its own claim in both
		// styles, paired to the front; the line pulls from the front.
		back, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: fxPressB, Code: "SYN1B", Name: fxPressB, Sequence: 2, Enabled: true,
		})
		testutil.MustNoErr(h.t, err, "create back position")
		var backFrom int64
		for _, sid := range []int64{fromID, toID} {
			payload := fxPartNext
			if sid == fromID {
				payload = fxPart
			}
			in := ps.claimInput(sid, payload)
			in.CoreNodeName, in.PairedCoreNode = fxPressB, fxPress
			id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
			testutil.MustNoErr(h.t, err, "back claim")
			if sid == fromID {
				backFrom = id
			}
		}
		_, err = db.EnsureProcessNodeRuntime(back)
		testutil.MustNoErr(h.t, err, "back runtime")
		testutil.MustNoErr(h.t, db.SetProcessNodeRuntime(back, &backFrom, fxCount), "stamp back")
		backBin := fxBin + 1
		testutil.MustNoErr(h.t, db.SetProcessNodeActiveBinID(back, &backBin), "bind back bin")
		testutil.MustNoErr(h.t, h.eng.writePullSide(nid, back), "line pulls from the front")
		h.partnerID = back
	}
	if s.neighbour {
		nb, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: "SYN-NEIGHBOUR", Code: "SYN2", Name: "SYN-NEIGHBOUR", Sequence: 2, Enabled: true,
		})
		testutil.MustNoErr(h.t, err, "create neighbour")
		var nbFromClaim int64
		for _, sid := range []int64{fromID, toID} {
			in := pairSpec{mode: protocol.SwapModeTwoRobot}.claimInput(sid, fxPart)
			in.CoreNodeName = "SYN-NEIGHBOUR"
			in.PairedCoreNode = ""
			id, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
			testutil.MustNoErr(h.t, err, "neighbour claim")
			if sid == fromID {
				nbFromClaim = id
			}
		}
		_, err = db.EnsureProcessNodeRuntime(nb)
		testutil.MustNoErr(h.t, err, "neighbour runtime")
		testutil.MustNoErr(h.t, db.SetProcessNodeRuntime(nb, &nbFromClaim, fxCount), "stamp neighbour")
		nbBin := fxBin + 7
		testutil.MustNoErr(h.t, db.SetProcessNodeActiveBinID(nb, &nbBin), "bind neighbour bin")
		// The neighbour's own steady-state pair, requested before the changeover
		// starts — the pair an operator releases at an unchanged node while
		// another node of the process is changing over.
		res, err := h.eng.RequestProduceSwap(nb)
		testutil.MustNoErr(h.t, err, "neighbour request swap")
		ev, sp, ok := h.eng.classifySwapLegsBySteps("SYN-NEIGHBOUR", res.OrderB.ID, res.OrderA.ID)
		if !ok {
			h.t.Fatal("fixture: neighbour legs did not classify")
		}
		h.addLeg("nevac", ev)
		h.addLeg("nsupply", sp)
		h.partnerID = nb
	}

	h.processID, h.nodeID = pid, nid
	if s.curtained {
		h.armCurtain(curtainSafe)
	}
	co, err := h.eng.StartProcessChangeover(pid, toID, "harness", "release harness")
	testutil.MustNoErr(h.t, err, "start changeover")
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, nid)
	testutil.MustNoErr(h.t, err, "front node task")
	if task.OldMaterialReleaseOrderID != nil {
		h.addLeg("evac", *task.OldMaterialReleaseOrderID)
	}
	if task.NextMaterialOrderID != nil {
		h.addLeg("supply", *task.NextMaterialOrderID)
	}
	return co.ID
}

// sequentialAB builds a sequential A/B press: two positions, each its own node
// with a claim paired to the other, the line pulling from the front. The
// removal leg (Order A) at the front is built by the operator's REQUEST door
// and named "removal". partnerReady puts a bin on the back position — the
// steady-state readiness flipTargetReady asks for.
func (h *relHarness) sequentialAB(role protocol.ClaimRole, partnerReady bool) {
	h.t.Helper()
	h.sequentialPair(role, partnerReady)
	h.requestRemoval(role)
}

// sequentialPair seeds the sequential A/B press without requesting anything.
func (h *relHarness) sequentialPair(role protocol.ClaimRole, partnerReady bool) {
	h.t.Helper()
	db := h.db
	pid, err := db.CreateProcess("SYN-SEQ-PROC", "release harness sequential", "active_production", "", "", false)
	testutil.MustNoErr(h.t, err, "create process")
	styleID, err := db.CreateStyle("SYN-SEQ", "", pid)
	testutil.MustNoErr(h.t, err, "create style")
	testutil.MustNoErr(h.t, db.SetActiveStyle(pid, &styleID), "set active style")
	ids := map[string]int64{}
	for i, pos := range []string{fxPress, fxPressB} {
		nid, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: pos, Code: pos, Name: pos, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(h.t, err, "create "+pos)
		in := pairSpec{mode: protocol.SwapModeSequential, role: role}.claimInput(styleID, fxPart)
		in.CoreNodeName = pos
		in.PairedCoreNode = map[string]string{fxPress: fxPressB, fxPressB: fxPress}[pos]
		claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
		testutil.MustNoErr(h.t, err, "claim "+pos)
		_, err = db.EnsureProcessNodeRuntime(nid)
		testutil.MustNoErr(h.t, err, "runtime "+pos)
		testutil.MustNoErr(h.t, db.SetProcessNodeRuntime(nid, &claimID, fxCount), "stamp "+pos)
		ids[pos] = nid
	}
	bin := fxBin
	testutil.MustNoErr(h.t, db.SetProcessNodeActiveBinID(ids[fxPress], &bin), "bind front bin")
	if partnerReady {
		back := fxBin + 1
		testutil.MustNoErr(h.t, db.SetProcessNodeActiveBinID(ids[fxPressB], &back), "bind back bin")
	}
	testutil.MustNoErr(h.t, h.eng.writePullSide(ids[fxPress], ids[fxPressB]), "line pulls from the front")
	h.processID, h.nodeID, h.partnerID = pid, ids[fxPress], ids[fxPressB]
}

// requestRemoval is the operator's REQUEST at the front of a sequential press;
// its removal leg (Order A) is named "removal".
func (h *relHarness) requestRemoval(role protocol.ClaimRole) {
	h.t.Helper()
	var res *NodeOrderResult
	var err error
	if role == protocol.ClaimRoleConsume {
		res, err = h.eng.RequestNodeMaterial(h.nodeID, 1)
	} else {
		res, err = h.eng.RequestProduceSwap(h.nodeID)
	}
	testutil.MustNoErr(h.t, err, "request removal")
	o := res.Order
	if o == nil {
		o = res.OrderA
	}
	if o == nil {
		h.t.Fatalf("fixture: sequential request built no order: %+v", res)
	}
	h.addLeg("removal", o.ID)
}

// activePull reports which position the line pulls from.
func (h *relHarness) activePull() string {
	h.t.Helper()
	for _, id := range []int64{h.nodeID, h.partnerID} {
		rt, err := h.db.GetProcessNodeRuntime(id)
		testutil.MustNoErr(h.t, err, "runtime")
		if rt.ActivePull {
			n, err := h.db.GetProcessNode(id)
			testutil.MustNoErr(h.t, err, "node")
			return "pull=" + n.CoreNodeName
		}
	}
	return "pull=none"
}
