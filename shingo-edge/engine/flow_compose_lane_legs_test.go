package engine

import (
	"context"
	"errors"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// TestPreviewFlow_RefusesALaneLegAndFullsFromAnEmptiesGroup is the composer's
// door onto the same refusals the claim editor makes: the flow's validator
// reads the lane and maintained-group facts from Core's node list, so a draft
// whose source names a lane, or whose consume source names an empties group,
// carries the finding in Core's sentence.
func TestPreviewFlow_RefusesALaneLegAndFullsFromAnEmptiesGroup(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	processID, _, toStyleID := seedFlowScenario(t, db)
	eng := testEngine(t, db)
	eng.SetCoreNodes([]protocol.NodeInfo{
		{Name: "FLOW-SUP", NodeType: protocol.NodeClassNGRP},
		{Name: "FLOW-SUP.FLOW-LANE", NodeType: protocol.NodeClassLANE},
		{Name: "FLOW-EMPTIES", NodeType: protocol.NodeClassNGRP, Maintained: true},
	})

	cells := cellsOf(t, db, toStyleID)
	for i := range cells {
		switch cells[i].CoreNodeName {
		case "FLOW-SWAP":
			cells[i].InboundSource = "FLOW-LANE"
		case "FLOW-SAME":
			cells[i].OutboundDestination = "FLOW-LANE"
		case "FLOW-ADD":
			cells[i].InboundSource = "FLOW-EMPTIES"
		}
	}
	p, err := eng.PreviewFlow(context.Background(), processID, FlowPreviewRequest{ToStyleID: toStyleID, Cells: cells})
	if err != nil {
		var noOrders *FlowNoOrdersError
		if !errors.As(err, &noOrders) {
			t.Fatalf("preview: %v", err)
		}
		p = noOrders.Preview
	}
	got := map[string]string{}
	for _, f := range p.Findings {
		got[f.CoreNodeName+"/"+string(f.Field)] = f.Message
	}
	want := map[string]string{
		"FLOW-SWAP/inbound_source":       protocol.MsgLaneIsNotASource + `: "FLOW-LANE"`,
		"FLOW-SAME/outbound_destination": protocol.MsgLaneIsNotADestination + `: "FLOW-LANE"`,
		"FLOW-ADD/inbound_source":        protocol.MsgFullsFromAnEmptiesBank + `: "FLOW-EMPTIES"`,
	}
	for k, msg := range want {
		if got[k] != msg {
			t.Errorf("%s: finding = %q, want %q (all: %+v)", k, got[k], msg, p.Findings)
		}
	}
	for _, f := range p.Findings {
		if f.Severity != domain.SeverityError {
			t.Errorf("non-error finding: %+v", f)
		}
	}
}
