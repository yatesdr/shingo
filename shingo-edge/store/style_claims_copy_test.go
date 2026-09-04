package store

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// CopyStyleClaims is the "Copy Node Claims" action: one style's claim set,
// verbatim, onto a sibling style — the same column list Clone Style uses,
// but into an EXISTING style (a replace, not a scaffold). These pin the
// mechanics: replace semantics, the two payload modes, and the guards the
// store itself owns. The active-style and cross-process RULES live in the
// service layer and are tested there.

// seedClaimRow adds one claim with just the fields the copy test cares about.
func seedClaimRow(t *testing.T, db *DB, styleID int64, coreNode string, role protocol.ClaimRole, payload string, uop int) {
	t.Helper()
	_, err := processes.UpsertClaim(db.DB, processes.NodeClaimInput{
		StyleID:      styleID,
		CoreNodeName: coreNode,
		Role:         role,
		SwapMode:     protocol.SwapModeSequential,
		PayloadCode:  payload,
		UOPCapacity:  uop,
	})
	testutil.MustNoErr(t, err, "seed claim "+coreNode)
}

func claimByNode(t *testing.T, db *DB, styleID int64, coreNode string) *processes.NodeClaim {
	t.Helper()
	claims, err := processes.ListClaims(db.DB, styleID)
	testutil.MustNoErr(t, err, "ListClaims")
	for i := range claims {
		if claims[i].CoreNodeName == coreNode {
			return &claims[i]
		}
	}
	return nil
}

func TestCopyStyleClaims_VerbatimReplace(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyVerbatim") // process + one style + one claim
	// Reuse the seeded style as the source; add a target with DIFFERENT claims.
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("CopyTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	seedClaimRow(t, db, srcID, "N-PRESS", "produce", "PART-SRC", 40)
	seedClaimRow(t, db, tgtID, "N-PRESS", "consume", "PART-TGT", 99)
	seedClaimRow(t, db, tgtID, "N-ONLY-TGT", "produce", "PART-TGT", 5)

	testutil.MustNoErr(t, db.CopyStyleClaims(srcID, tgtID, true), "copy verbatim")

	// The target now mirrors the source exactly: role, payload, capacity.
	got := claimByNode(t, db, tgtID, "N-PRESS")
	if got == nil || got.Role != "produce" || got.PayloadCode != "PART-SRC" || got.UOPCapacity != 40 {
		t.Errorf("target N-PRESS = %+v, want produce/PART-SRC/40 from source", got)
	}
	// Replace semantics: a node the source does not have is GONE.
	if claimByNode(t, db, tgtID, "N-ONLY-TGT") != nil {
		t.Errorf("target-only node survived a replace copy")
	}
	// The source is untouched.
	if srcClaim := claimByNode(t, db, srcID, "N-PRESS"); srcClaim == nil || srcClaim.PayloadCode != "PART-SRC" {
		t.Errorf("source claim changed: %+v", srcClaim)
	}
}

func TestCopyStyleClaims_KeepsTargetPayloads(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyKeepPayload")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID
	tgtID, err := db.CreateStyle("KeepPayloadTarget", "", processID)
	testutil.MustNoErr(t, err, "create target")
	seedClaimRow(t, db, srcID, "N-PRESS", "produce", "PART-SRC", 40)
	seedClaimRow(t, db, srcID, "N-STAGE", "consume", "PART-SRC", 0)
	seedClaimRow(t, db, tgtID, "N-PRESS", "consume", "PART-MINE", 99)

	// includePayloads=false: choreography comes from the source, payloads
	// stay the TARGET's on the node it shares; a source node the target
	// never had arrives with the source's payload (nothing to preserve).
	testutil.MustNoErr(t, db.CopyStyleClaims(srcID, tgtID, false), "copy keep-payloads")

	press := claimByNode(t, db, tgtID, "N-PRESS")
	if press == nil || press.Role != "produce" || press.PayloadCode != "PART-MINE" {
		t.Errorf("N-PRESS = %+v, want source role with target's PART-MINE", press)
	}
	stage := claimByNode(t, db, tgtID, "N-STAGE")
	if stage == nil || stage.PayloadCode != "PART-SRC" {
		t.Errorf("N-STAGE = %+v, want source's PART-SRC (no target payload to keep)", stage)
	}
}

func TestCopyStyleClaims_SameStyleAndMissingSource(t *testing.T) {
	db := coverageDB(t)
	processID, _, _, _ := seedProcessWithChildren(t, db, "CopyGuards")
	src, err := processes.ListStylesByProcess(db.DB, processID)
	testutil.MustNoErr(t, err, "list styles")
	srcID := src[0].ID

	if err := db.CopyStyleClaims(srcID, srcID, true); err == nil {
		t.Errorf("copy onto self must error")
	}
	if err := db.CopyStyleClaims(srcID, 999999, true); err == nil {
		t.Errorf("missing target must error")
	}
}
