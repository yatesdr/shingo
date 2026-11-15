package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// TestBuildView_AHeldTileCarriesItsChip: a held tile's JSON carries the chip
// (domain.ReorderHoldChip) and the decider's sentence for its panel, and not
// the kind, which the board no longer reads. The chip reads the runtime row's
// note (an idle off says LINE IDLE) and the process's switch flag (a person's
// off draws none where the header carries the line's switch; the sentence
// stays). Before 2026-11-14 the tile carried the kind and the board spelled
// the chip from it.
func TestBuildView_AHeldTileCarriesItsChip(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		hold     domain.ReorderHold
		note     string
		switchOn bool
		chip     any
	}{
		{"the idle rule's off", domain.ReorderHoldOff, domain.IdleOffNote("1 hour"), false, "REORDER AT 110: LINE IDLE"},
		{"a person's off, no switch on the board", domain.ReorderHoldPersonOff, "LINE-OPS", false, "REORDER AT 110: SWITCHED OFF"},
		{"a person's off, the switch on the board", domain.ReorderHoldPersonOff, "LINE-OPS", true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			processID, err := db.CreateProcess("SYN-HOLD-PROC", "", "", "", false)
			if err != nil {
				t.Fatalf("process: %v", err)
			}
			stationID, err := db.CreateOperatorStation(stations.Input{ProcessID: processID, Code: "SYN-HOLD-ST", Name: "HOLD", Sequence: 1, Enabled: true})
			if err != nil {
				t.Fatalf("station: %v", err)
			}
			nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: processID, OperatorStationID: &stationID,
				CoreNodeName: "SYN-WLN-1", Code: "WLN1", Name: "WLN 1", Sequence: 1, Enabled: true})
			if err != nil {
				t.Fatalf("node: %v", err)
			}
			styleID, err := db.CreateStyle("SYN-STYLE", "", processID)
			if err != nil {
				t.Fatalf("style: %v", err)
			}
			if err := db.SetActiveStyle(processID, &styleID); err != nil {
				t.Fatalf("active style: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO payload_catalog (id, name, code, uop_capacity) VALUES (1, 'SYN-PART-240', 'SYN-PART-240', 240)`); err != nil {
				t.Fatalf("catalog: %v", err)
			}
			armed := true
			if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
				StyleID: styleID, CoreNodeName: "SYN-WLN-1", Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
				PayloadCode: "SYN-PART-240", InboundStaging: "SYN-STG", InboundSource: "SYN-SRC", OutboundDestination: "SYN-DOCK",
				ReorderPoint: 110, AutoReorder: &armed,
			}); err != nil {
				t.Fatalf("claim: %v", err)
			}
			if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
				t.Fatalf("runtime: %v", err)
			}
			if _, err := processes.HoldReorder(db.DB, nodeID, c.hold, time.Now(), c.note); err != nil {
				t.Fatalf("hold: %v", err)
			}
			if err := processes.SetAutoReorderSwitchHMI(db.DB, processID, c.switchOn); err != nil {
				t.Fatalf("switch on the board: %v", err)
			}

			view, err := NewStationService(db).BuildView(context.Background(), stationID)
			if err != nil {
				t.Fatalf("BuildView: %v", err)
			}
			if len(view.Nodes) != 1 {
				t.Fatalf("nodes = %d, want 1", len(view.Nodes))
			}
			raw, err := json.Marshal(view.Nodes[0])
			if err != nil {
				t.Fatalf("marshal tile: %v", err)
			}
			tile := map[string]any{}
			if err := json.Unmarshal(raw, &tile); err != nil {
				t.Fatalf("read tile: %v", err)
			}
			if got := tile["auto_reorder_chip"]; got != c.chip {
				t.Errorf("auto_reorder_chip = %v, want %v", got, c.chip)
			}
			want := domain.AutoReorderHeld(view.Nodes[0].ActiveClaim, view.Nodes[0].Runtime, time.Now()).Sentence
			if got := tile["auto_reorder_note"]; want == "" || got != want {
				t.Errorf("auto_reorder_note = %v, want the decider's %q", got, want)
			}
			if _, ok := tile["auto_reorder_hold"]; ok {
				t.Error("the tile's JSON carries auto_reorder_hold; the board reads the chip")
			}
		})
	}
}
