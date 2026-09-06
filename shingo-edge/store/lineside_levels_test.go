package store

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// lineside_levels_test.go — the R1 report's three gates, one test each.
//
// SPRINGFIELD, 2026-09-02 TO 2026-09-05. Core's audit line printed once a
// minute for three days: "payload=74871-6SA1A.06 … ledger total=0 would FIRE,
// edge-adjusted total=7032 would hold; DECIDING OFF edge_reports". The carrier
// at ALN_007 held 63125-6TA0A.06. Of 74871-6SA1A.06 there were zero plant-wide.
// Replenishment of the part that was actually running had been suppressed for
// three days by a report naming a part that was not there.
//
// Everything the report said came off one mutable pointer — active_claim_id —
// which eighteen paths write and most of them fill from the process's active
// style. WHETHER a node reported, WHAT it said was there, and the consume role
// filter all hung off it, so one stale pointer moved all three at once.
//
// These tests hold the three apart.

// linesideFixture builds one process running one style that consumes at one
// node, plus the node's runtime row. Returns the ids the tests steer with.
func linesideFixture(t *testing.T, db *DB, coreNode string) (processID, styleID, nodeID, claimID int64) {
	t.Helper()
	processID, err := db.CreateProcess("LS-"+coreNode, "", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: coreNode, Code: "C-" + coreNode,
		Name: coreNode, Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	styleID, err = db.CreateStyle("LS-STYLE-"+coreNode, "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	claimID, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: coreNode, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeManualSwap, PayloadCode: "REQUESTED-PART", UOPCapacity: 500, OutboundDestination: "OUT",
	})
	if err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("ensure runtime: %v", err)
	}
	return processID, styleID, nodeID, claimID
}

// bindCarrier puts a carrier on the node with a count, without saying what it
// is — the identity is the thing under test and each case sets it explicitly.
func bindCarrier(t *testing.T, db *DB, nodeID, claimID, binID int64, uop int) {
	t.Helper()
	if err := db.SetProcessNodeRuntimeForDeliveredBin(nodeID, &claimID, binID, 1, uop); err != nil {
		t.Fatalf("bind carrier: %v", err)
	}
}

func levelFor(t *testing.T, db *DB, coreNode string) *LinesideLevel {
	t.Helper()
	levels, err := db.ListLinesideLevels()
	if err != nil {
		t.Fatalf("list lineside levels: %v", err)
	}
	for i := range levels {
		if levels[i].CoreNodeName == coreNode {
			return &levels[i]
		}
	}
	return nil
}

// THE INCIDENT ITSELF. The pointer names the requested part; the carrier is
// something else; the report must name the carrier.
func TestListLinesideLevels_ReportsTheCarrierNotTheClaim(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, _, nodeID, claimID := linesideFixture(t, db, "ALN_007")
	bindCarrier(t, db, nodeID, claimID, 15, 7032)
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "63125-6TA0A.06", true, "delivery"); err != nil {
		t.Fatalf("record carrier: %v", err)
	}

	got := levelFor(t, db, "ALN_007")
	if got == nil {
		t.Fatal("no row for ALN_007 — a bound carrier with an established identity must report")
	}
	if got.PayloadCode != "63125-6TA0A.06" {
		t.Errorf("PayloadCode = %q, want 63125-6TA0A.06. REQUESTED-PART means the report went "+
			"back to reading the claim, which is the incident: a part number of which zero "+
			"existed plant-wide, shipped against a carrier holding 7032 of something else.",
			got.PayloadCode)
	}
	if !got.PayloadKnown {
		t.Error("PayloadKnown = false for an identity a delivery envelope established")
	}
	if got.BinUOP != 7032 || got.BinCount != 1 {
		t.Errorf("BinUOP/BinCount = %d/%d, want 7032/1", got.BinUOP, got.BinCount)
	}
}

// A carrier nobody could identify is not an empty one and is not a report. The
// row still comes back so the reporter can name what it is withholding; the
// flag is what stops it going on the wire.
func TestListLinesideLevels_UnknownIdentityIsFlagged(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, _, nodeID, claimID := linesideFixture(t, db, "ALN_002")
	bindCarrier(t, db, nodeID, claimID, 21, 400)

	got := levelFor(t, db, "ALN_002")
	if got == nil {
		t.Fatal("no row at all — the reporter needs the row to know it is withholding one")
	}
	if got.PayloadKnown {
		t.Error("PayloadKnown = true for a carrier no envelope and no person ever named")
	}
	if got.PayloadCode == "REQUESTED-PART" {
		t.Error("PayloadCode fell back to the claim. There is no safe guess here: the fallback " +
			"is what shipped a part number nobody established, keyed as a primary key on Core.")
	}
}

// A KNOWN EMPTY carrier is an answer, and it is a different answer. It has no
// part number to report under, so it carries the known bit and an empty code —
// the reporter drops it rather than reporting zero of the requested part.
func TestListLinesideLevels_KnownEmptyCarrierIsNotTheRequestedPart(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, _, nodeID, claimID := linesideFixture(t, db, "ALN_004")
	bindCarrier(t, db, nodeID, claimID, 22, 0)
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "", true, "operator"); err != nil {
		t.Fatalf("record empty carrier: %v", err)
	}

	got := levelFor(t, db, "ALN_004")
	if got == nil {
		t.Fatal("no row for a node with a bound carrier")
	}
	if !got.PayloadKnown {
		t.Error("PayloadKnown = false — the operator cleared this carrier and can see it is empty")
	}
	if got.PayloadCode != "" {
		t.Errorf("PayloadCode = %q, want empty. An empty carrier holds no part, and naming the "+
			"claim's part here reports zero on-hand of a part that may be sitting in a bucket.",
			got.PayloadCode)
	}
}

// WHETHER a node reports is configuration, not a pointer. Aim active_claim_id
// at a claim from a style nobody is running: the row must still describe the
// node the way the RUNNING style configures it.
func TestListLinesideLevels_ExistenceFollowsConfigNotThePointer(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	processID, _, nodeID, claimID := linesideFixture(t, db, "ALN_009")
	otherStyle, err := db.CreateStyle("LS-RETIRED", "", processID)
	if err != nil {
		t.Fatalf("create other style: %v", err)
	}
	// A produce claim on a style the process is NOT running. Under the old
	// query this decided both the role filter and the payload.
	otherClaim, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: otherStyle, CoreNodeName: "ALN_009", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeManualSwap, PayloadCode: "STALE-PART", UOPCapacity: 500, OutboundDestination: "OUT",
	})
	if err != nil {
		t.Fatalf("upsert other claim: %v", err)
	}
	bindCarrier(t, db, nodeID, claimID, 30, 120)
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "REAL-PART", true, "delivery"); err != nil {
		t.Fatalf("record carrier: %v", err)
	}
	if err := db.SetProcessNodeRuntime(nodeID, &otherClaim, 120); err != nil {
		t.Fatalf("aim the pointer at the stale claim: %v", err)
	}

	got := levelFor(t, db, "ALN_009")
	if got == nil {
		t.Fatal("the node dropped out of the report because a pointer moved. The running style " +
			"still consumes here; that is what decides whether there is anything to say.")
	}
	if got.PayloadCode != "REAL-PART" {
		t.Errorf("PayloadCode = %q, want REAL-PART", got.PayloadCode)
	}
}

// Nothing bound and no bucket: there is no on-hand to report, and a zero would
// assert something about a slot nobody has looked at.
func TestListLinesideLevels_EmptySlotShipsNothing(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, _, _, _ = linesideFixture(t, db, "ALN_011")

	if got := levelFor(t, db, "ALN_011"); got != nil {
		t.Errorf("got a row %+v for a node with no carrier and no bucket", got)
	}
}

// A bucket keeps the node in the report after the carrier leaves, and the count
// must not come along: remaining_uop_cached is the departed carrier's number
// until the next delivery overwrites it.
func TestListLinesideLevels_BucketWithoutBinReportsZeroBinUOP(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, styleID, nodeID, claimID := linesideFixture(t, db, "ALN_013")
	bindCarrier(t, db, nodeID, claimID, 41, 640)
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "REAL-PART", true, "delivery"); err != nil {
		t.Fatalf("record carrier: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "REAL-PART", 90); err != nil {
		t.Fatalf("capture bucket: %v", err)
	}
	// The carrier goes. The count it left behind stays on the row.
	if err := db.SetProcessNodeActiveBinID(nodeID, nil); err != nil {
		t.Fatalf("clear bin pointer: %v", err)
	}

	got := levelFor(t, db, "ALN_013")
	if got == nil {
		t.Fatal("no row — the bucket still holds parts at this node")
	}
	if got.BinCount != 0 {
		t.Errorf("BinCount = %d, want 0", got.BinCount)
	}
	if got.BinUOP != 0 {
		t.Errorf("BinUOP = %d, want 0. 640 is the departed carrier's count; a count with no "+
			"carrier behind it is not inventory.", got.BinUOP)
	}
	if got.BucketQty != 90 {
		t.Errorf("BucketQty = %d, want 90 — the parts are pulled to lineside and still there", got.BucketQty)
	}
}

// The bucket term is attributed to the carrier's part, not summed node-wide.
// Another part's bucket showing up under this part's name is the same defect
// as the claim fallback, one table over.
func TestListLinesideLevels_BucketOfAnotherPartIsNotSummedIn(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, styleID, nodeID, claimID := linesideFixture(t, db, "ALN_015")
	bindCarrier(t, db, nodeID, claimID, 51, 300)
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "REAL-PART", true, "delivery"); err != nil {
		t.Fatalf("record carrier: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "REAL-PART", 25); err != nil {
		t.Fatalf("capture matching bucket: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "SOME-OTHER-PART", 400); err != nil {
		t.Fatalf("capture foreign bucket: %v", err)
	}

	// One node, one row. A bucket for another part must not multiply the node
	// into two rows either -- Core keys on (node, payload), so a second row
	// would mint an authoritative on-hand for a part this carrier is not.
	levels, err := db.ListLinesideLevels()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var rows []LinesideLevel
	for _, l := range levels {
		if l.CoreNodeName == "ALN_015" {
			rows = append(rows, l)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows for ALN_015, want 1: %+v", len(rows), rows)
	}
	got := &rows[0]
	if got.BucketQty != 25 {
		t.Errorf("BucketQty = %d, want 25. 425 means the foreign part's bucket was summed in "+
			"under REAL-PART's name.", got.BucketQty)
	}
}
