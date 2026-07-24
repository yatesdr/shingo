package service

import (
	"context"
	"testing"

	"shingoedge/domain"
)

// TestBuildView_StrandedResolverPopulatesChip locks P2-C8's view wiring: the
// engine's parked-ticks resolver, injected via SetStrandedResolver, lands the
// alarm sentence on the matching node's StrandedAlarm (which the tile renders as
// the amber chip) and leaves non-matching nodes blank. Unset resolver stays a
// clean no-op (the lighter constructors don't wire it).
func TestBuildView_StrandedResolverPopulatesChip(t *testing.T) {
	db, stationID, pressNodeID, seatNodeID, _ := seatScenario(t)
	svc := NewStationService(db)

	const detail = "CARRIER-9 staged 3h at PLN_A1, not bound — Record Count on the bin tab."
	svc.SetStrandedResolver(func(coreNodeName string) string {
		if coreNodeName == "PLN_A1" {
			return detail
		}
		return ""
	})

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}

	var press, seat *domain.StationNodeView
	for i := range view.Nodes {
		switch view.Nodes[i].Node.ID {
		case pressNodeID:
			press = &view.Nodes[i]
		case seatNodeID:
			seat = &view.Nodes[i]
		}
	}
	if press == nil {
		t.Fatal("press node absent from view")
	}
	if press.StrandedAlarm != detail {
		t.Errorf("press StrandedAlarm = %q, want the resolver detail %q", press.StrandedAlarm, detail)
	}
	if seat != nil && seat.StrandedAlarm != "" {
		t.Errorf("seat StrandedAlarm = %q, want empty (resolver returned \"\" for it)", seat.StrandedAlarm)
	}
}
