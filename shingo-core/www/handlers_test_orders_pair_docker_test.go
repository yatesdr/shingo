//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestDirectTwoRobotSwap_SupplyWaitsForItsRemoval is the D7 door of census 6:
// the engineers' /test-orders page, two_robot. Both uuids are minted before
// either order, so the supply reaches intake as half of a pair and waits for its
// removal, instead of going to the fleet alone and leaving the removal to follow
// as a second, unrelated dispatch.
//
// Order of events is read from order_history row ids, not timestamps: the ids
// are one sequence, and the database's clock and this process's are not.
//
// DEFECT PIN. Fails at bcbde0d2: the supply went to intake naming no sibling, so
// its own intake pass dispatched it before the removal's row existed.
func TestDirectTwoRobotSwap_SupplyWaitsForItsRemoval(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)
	stage := &nodes.Node{Name: "D7-STAGE", Enabled: true}
	if err := db.CreateNode(stage); err != nil {
		t.Fatalf("create staging node: %v", err)
	}
	out := &nodes.Node{Name: "D7-OUT", Enabled: true}
	if err := db.CreateNode(out); err != nil {
		t.Fatalf("create outbound node: %v", err)
	}
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "D7-FRESH")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "D7-RESIDENT")

	rec := postJSON(t, h.apiDirectComplexOrderSubmit, "/api/test-orders/direct/complex",
		map[string]any{
			"cycle_mode":           "two_robot",
			"location":             sd.LineNode.Name,
			"inbound_staging":      stage.Name,
			"inbound_source":       sd.StorageNode.Name,
			"outbound_destination": out.Name,
			"payload_code":         sd.Payload.Code,
			"priority":             1,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Orders []struct {
			Role      string `json:"role"`
			OrderUUID string `json:"order_uuid"`
		} `json:"orders"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Orders) != 2 {
		t.Fatalf("decode the page's answer (%v): %s", err, rec.Body.String())
	}
	supply, err := db.GetOrderByUUID(resp.Orders[0].OrderUUID)
	if err != nil || supply == nil {
		t.Fatalf("load the supply (%s): %v", resp.Orders[0].Role, err)
	}
	removal, err := db.GetOrderByUUID(resp.Orders[1].OrderUUID)
	if err != nil || removal == nil {
		t.Fatalf("load the removal (%s): %v", resp.Orders[1].Role, err)
	}
	if supply.VendorOrderID == "" || removal.VendorOrderID == "" {
		t.Fatalf("fixture: the pair did not go (supply %q/%q, removal %q/%q) — both legs need to be able "+
			"to source for the ordering to mean anything", supply.Status, supply.QueueCause, removal.Status, removal.QueueCause)
	}

	removalHist, err := db.ListOrderHistory(removal.ID)
	if err != nil || len(removalHist) == 0 {
		t.Fatalf("the removal has no history (%v)", err)
	}
	born := removalHist[0].ID
	for _, row := range removalHist {
		if row.ID < born {
			born = row.ID
		}
	}
	supplyHist, err := db.ListOrderHistory(supply.ID)
	if err != nil {
		t.Fatalf("supply history: %v", err)
	}
	for _, row := range supplyHist {
		if string(row.Status) == "dispatched" && row.ID < born {
			t.Errorf("the supply was dispatched (history row %d) before its removal's row existed (born at row "+
				"%d): a pair leg went to the fleet alone, so the pair rule never saw this pair", row.ID, born)
		}
	}
}
