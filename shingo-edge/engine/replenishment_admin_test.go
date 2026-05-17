package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestFindAnyLoaderClaimForPayload_InactiveStyle — the Calculate
// path must resolve bin capacity even when the loader's claim lives
// on a style that isn't currently active. The legacy
// FindLoaderForPayload only walks proc.ActiveStyleID; this regression
// exercises the wider search.
//
// Fixture shape:
//   - One process with two styles ("OLD" active, "NEW" inactive).
//   - Loader node carries a produce manual_swap claim with allowed
//     payload WIDGET-X on the NEW (inactive) style only.
//   - FindLoaderForPayload returns nil for WIDGET-X (active style
//     has no such claim).
//   - FindAnyLoaderClaimForPayload returns the claim, with the
//     UOPCapacity preserved so the UI can render the implied-bin
//     annotation next to the calculated threshold.
func TestFindAnyLoaderClaimForPayload_InactiveStyle(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	processID, err := db.CreateProcess("CAL-PROC", "calculator test", "active_production", "", "", false, false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "CAL-LOADER",
		Code:         "CL1",
		Name:         "Cal Loader",
		Sequence:     1,
		Enabled:      true,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}

	oldStyleID, err := db.CreateStyle("OLD", "old style", processID)
	if err != nil {
		t.Fatalf("create old style: %v", err)
	}
	newStyleID, err := db.CreateStyle("NEW", "new style", processID)
	if err != nil {
		t.Fatalf("create new style: %v", err)
	}
	// OLD is active. NEW carries the loader claim for WIDGET-X.
	testutil.MustNoErr(t, db.SetActiveStyle(processID, &oldStyleID), "set active")
	if _, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID:             newStyleID,
		CoreNodeName:        "CAL-LOADER",
		Role:                protocol.ClaimRoleProduce,
		SwapMode:            protocol.SwapModeManualSwap,
		PayloadCode:         "WIDGET-X",
		AllowedPayloadCodes: []string{"WIDGET-X"},
		UOPCapacity:         200,
		OutboundDestination: "FILLED-STORAGE",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}

	eng := &Engine{db: db}

	// Sanity: active-gated lookup must miss.
	if got := eng.FindLoaderForPayload("WIDGET-X"); got != nil {
		t.Fatalf("FindLoaderForPayload(WIDGET-X) found %+v on the active style — fixture is wrong", got)
	}

	// The wider search must hit and carry UOPCapacity through.
	got := eng.FindAnyLoaderClaimForPayload("WIDGET-X")
	if got == nil {
		t.Fatal("FindAnyLoaderClaimForPayload(WIDGET-X) returned nil — claim on inactive style not found")
	}
	if got.claim.CoreNodeName != "CAL-LOADER" {
		t.Errorf("CoreNodeName = %q, want CAL-LOADER", got.claim.CoreNodeName)
	}
	if got.claim.UOPCapacity != 200 {
		t.Errorf("UOPCapacity = %d, want 200 (implied-bin annotation depends on this)", got.claim.UOPCapacity)
	}

	// Unknown payload still returns nil (no false positives).
	if got := eng.FindAnyLoaderClaimForPayload("NO-SUCH-PAYLOAD"); got != nil {
		t.Errorf("unknown payload returned %+v, want nil", got)
	}
	if got := eng.FindAnyLoaderClaimForPayload(""); got != nil {
		t.Errorf("empty payload returned %+v, want nil", got)
	}
}
