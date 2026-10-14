package service

import (
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
)

// TestCanSkipChangeoverStep: the view offers ABANDON - SKIP THIS STEP from the
// node's own board orders, by the abandon's rule — a queued delivery is
// skippable; once it moves, or while the partner evac is with the fleet, the
// button is not offered.
func TestCanSkipChangeoverStep(t *testing.T) {
	t.Parallel()
	supply, evac := int64(1), int64(2)
	task := &domain.NodeTask{NodeName: "ALN_001", State: domain.NodeTaskAwaitingMaterial,
		NextMaterialOrderID: &supply, OldMaterialReleaseOrderID: &evac}
	orders := func(s, e protocol.Status) []domain.Order {
		return []domain.Order{{ID: 1, Status: s}, {ID: 2, Status: e}}
	}
	if !canSkipChangeoverStep(task, orders(protocol.StatusQueued, protocol.StatusQueued)) {
		t.Error("both legs queued on material: the skip must be offered")
	}
	if canSkipChangeoverStep(task, orders(protocol.StatusInTransit, protocol.StatusQueued)) {
		t.Error("delivery moving: the skip must not be offered")
	}
	if canSkipChangeoverStep(task, orders(protocol.StatusQueued, protocol.StatusStaged)) {
		t.Error("partner evac staged with the fleet: the skip must not be offered")
	}
	if canSkipChangeoverStep(nil, nil) {
		t.Error("no changeover task: nothing to skip")
	}
}
