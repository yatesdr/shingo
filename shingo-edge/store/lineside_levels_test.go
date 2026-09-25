package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// lineside_levels_test.go — the lineside report's three gates, one test each.
//
// SPRINGFIELD, 2026-09-02 TO 2026-09-05. Core's audit line printed once a
// minute for three days: "payload=SYN-PART09D.06 … ledger total=0 would FIRE,
// edge-adjusted total=7032 would hold; DECIDING OFF edge_reports". The carrier
// at ALN_007 held SYN-PART01E.06. Of SYN-PART09D.06 there were zero plant-wide.
// Replenishment of the part that was actually running had been suppressed for
// three days by a report naming a part that was not there. (The report decided
// replenishment then; since the 2026-09-23 seat-count ruling it is a checksum
// Core compares, and a wrong name now points that comparison at the wrong part.)
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
	claimID, err = upsertClaimRetiredMode(t, db, processes.NodeClaimInput{
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
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "SYN-PART01E.06", true, "delivery"); err != nil {
		t.Fatalf("record carrier: %v", err)
	}

	got := levelFor(t, db, "ALN_007")
	if got == nil {
		t.Fatal("no row for ALN_007 — a bound carrier with an established identity must report")
	}
	if got.PayloadCode != "SYN-PART01E.06" {
		t.Errorf("PayloadCode = %q, want SYN-PART01E.06. REQUESTED-PART means the report went "+
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
	otherClaim, err := upsertClaimRetiredMode(t, db, processes.NodeClaimInput{
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

// NOTHING BOUND AND NO BUCKET IS STILL A SEAT THE EDGE RUNS, and the row says
// so: no carrier, 0 in it, 0 in the bucket. The count the departed carrier
// left on the runtime row does not come with it. It used to be left out, which
// made an empty seat indistinguishable from one the Edge does not run.
func TestListLinesideLevels_EmptySlotIsListedEmpty(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, _, nodeID, claimID := linesideFixture(t, db, "ALN_011")
	bindCarrier(t, db, nodeID, claimID, 44, 250)
	if err := db.SetProcessNodeActiveBinID(nodeID, nil); err != nil {
		t.Fatalf("clear bin pointer: %v", err)
	}

	got := levelFor(t, db, "ALN_011")
	if got == nil {
		t.Fatal("no row for a seat the running style consumes at")
	}
	if got.BinID != nil || got.BinCount != 0 || got.BinUOP != 0 || got.BucketQty != 0 {
		t.Errorf("got %+v, want no carrier, 0, 0 (250 is the departed carrier's count)", got)
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

// The bucket term sums ACTIVE piles of the carrier's part only: an inactive pile
// of that same part at the seat is not in the report, although Core's mirror
// still counts it today.
// Stays: the report sums active rows only; under change #2 the excluded row is
// the stranded one.
func TestListLinesideLevels_InactivePileIsNotSummedIn(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, styleID, nodeID, claimID := linesideFixture(t, db, "SYN-SEAT-16")
	bindCarrier(t, db, nodeID, claimID, 61, 300)
	if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "REAL-PART", true, "delivery"); err != nil {
		t.Fatalf("record carrier: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "REAL-PART", 25); err != nil {
		t.Fatalf("capture active pile: %v", err)
	}
	// An inactive pile of the same part, left by an earlier style's run.
	if _, err := db.DB.Exec(`INSERT INTO node_lineside_bucket
		(node_id, pair_key, style_id, payload_code, qty, state) VALUES (?, '', ?, 'REAL-PART', 400, 'inactive')`,
		nodeID, styleID+1); err != nil {
		t.Fatalf("seed inactive pile: %v", err)
	}

	got := levelFor(t, db, "SYN-SEAT-16")
	if got == nil {
		t.Fatal("no row for the seat")
	}
	if got.BucketQty != 25 {
		t.Errorf("BucketQty = %d, want 25. 425 means the inactive pile was summed in.", got.BucketQty)
	}
}

// upsertClaimRetiredMode upserts a claim carrying a swap mode the allowlist no
// longer accepts, by upserting with a configurable placeholder and rewriting the
// column — the pre-lockdown row shape the read paths still tolerate.
//
// manual_swap is retired as a PERSISTED value: Core owns loader configuration
// and SynthClaim serves it, so a stored loader claim is a second authority and
// SetCoreLoaders quarantines any that appear. The lineside READ path still has
// to handle one — a legacy row survives until its Edge takes the sync — which is
// what the fixtures above are for. The engine package carries the same seam for
// the same reason; store cannot borrow it (internal/testdb imports store, so
// this package cannot import back).
func upsertClaimRetiredMode(t *testing.T, db *DB, in processes.NodeClaimInput) (int64, error) {
	t.Helper()
	want := in.SwapMode
	in.SwapMode = protocol.SwapModeSequential // placeholder to pass the allowlist
	id, err := db.UpsertStyleNodeClaim(in)
	if err != nil {
		return id, err
	}
	_, err = db.DB.Exec(`UPDATE style_node_claims SET swap_mode=? WHERE id=?`, string(want), id)
	return id, err
}

// THE REPORT COSTS ONE STATEMENT, WHATEVER THE SEAT COUNT. PIN, green before
// and after lane A of the memory build: lane A puts the carrier's bin id, epoch
// and flushed seq on each row as columns of this same SELECT (the flushed seq by
// a JOIN to inventory_delta_seq), so the count must stay 1. A second statement
// per seat would be one per row per minute on a Pi whose store is a single
// SQLite connection.
func TestListLinesideLevels_OneStatementForEverySeat(t *testing.T) {
	t.Parallel()
	db, counter, err := OpenCounting(filepath.Join(t.TempDir(), "levels.db"))
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for i, node := range []string{"ALN_020", "ALN_021", "ALN_022"} {
		_, styleID, nodeID, claimID := linesideFixture(t, db, node)
		bindCarrier(t, db, nodeID, claimID, int64(60+i), 100+i)
		if err := db.SetProcessNodeRuntimeLinesidePayload(nodeID, "PART-A", true, "delivery"); err != nil {
			t.Fatalf("record carrier: %v", err)
		}
		if _, err := db.CaptureLinesideBucket(nodeID, "", styleID, "PART-A", 5); err != nil {
			t.Fatalf("capture bucket: %v", err)
		}
	}

	counter.Reset()
	levels, err := db.ListLinesideLevels()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(levels) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(levels), levels)
	}
	if got := counter.Count(); got != 1 {
		t.Errorf("ListLinesideLevels issued %d statements for 3 seats, want 1", got)
	}
}
