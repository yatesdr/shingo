//go:build sim

package main

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// TestReadinessGate_PassesThePLCsOverrunSwitch pins the wiring from the fake
// PLC's overrun switch to engine.SimMachineReady: the gate asks the switch of
// the PLC it is gating, so a PLC whose produce seat is at its pack stops with
// the switch off and keeps counting with it on.
func TestReadinessGate_PassesThePLCsOverrunSwitch(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "edge.db"))
	testutil.MustNoErr(t, err, "open")
	t.Cleanup(func() { db.Close() })

	procID, err := db.CreateProcess("OVERRUN-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_005", Code: "A05", Name: "ALN_005", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "node")
	styleID, err := db.CreateStyle("OVERRUN-STYLE", "", procID)
	testutil.MustNoErr(t, err, "style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "active style")
	claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALN_005", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "ASSY",
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_007", OutboundStaging: "SLN_008",
	})
	testutil.MustNoErr(t, err, "claim")
	// Capacity resolves from the catalog, not the claim.
	_, err = db.Exec(`INSERT INTO payload_catalog (id, name, code, uop_capacity, updated_at)
		VALUES (1, 'ASSY', 'ASSY', 20, datetime('now'))`)
	testutil.MustNoErr(t, err, "catalog")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	bin := int64(39)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &claimID, &bin, 20), "full seat")
	_, err = db.CreateReportingPoint("PRESS-1", "PRESS-1_COUNTER", styleID)
	testutil.MustNoErr(t, err, "reporting point")

	var asked string
	overrunOn := map[string]bool{}
	gate := makeReadinessGate(db.DB, func(plcName string) bool { asked = plcName; return overrunOn[plcName] })
	if gate("PRESS-1") {
		t.Error("switch off: a full produce seat let the PLC count")
	}
	if asked != "PRESS-1" {
		t.Errorf("the gate asked the switch of %q; want PRESS-1", asked)
	}
	overrunOn["PRESS-1"] = true
	if !gate("PRESS-1") {
		t.Error("switch on: a full produce seat stopped the PLC")
	}
}
