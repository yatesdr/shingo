//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// handleBlockCompleted was the sole subscriber to EventBlockCompleted and it
// discarded the block after moving the bin, so every leg the fleet reported —
// travel-to-source, load, travel-to-dest, unload — existed for one function
// call and was never written down. These pin the mission_events row that
// replaced that.

func seedLegOrder(t *testing.T, db *store.DB, uuid string) int64 {
	t.Helper()
	ord := &orders.Order{
		EdgeUUID:     uuid,
		StationID:    "line-1",
		OrderType:    dispatch.OrderTypeRetrieve,
		Status:       dispatch.StatusInTransit,
		SourceNode:   "S",
		DeliveryNode: "D",
		Quantity:     1,
	}
	testutil.MustNoErr(t, db.CreateOrder(ord), "create order")
	return ord.ID
}

func TestRecordBlockLeg_WritesTimingToMissionEvents(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newUnstartedEngine(t, db, simulator.New())

	orderID := seedLegOrder(t, db, "leg-1")
	testutil.MustNoErr(t, db.UpdateOrderRobotID(orderID, "AMR-03"), "set robot")

	eng.recordBlockLeg(BlockCompletedEvent{
		OrderID:       orderID,
		VendorOrderID: "sg-leg-1",
		BlockID:       "b-pickup",
		Location:      "AP-SOURCE",
		BinTask:       "Load",
		StartTime:     1784956669,
		TerminateTime: 1784956774,
	})

	events, err := db.ListMissionEvents(orderID)
	testutil.MustNoErr(t, err, "list mission events")

	var leg *struct {
		BlockID         string `json:"blockId"`
		Location        string `json:"location"`
		BinTask         string `json:"binTask"`
		StartTime       int64  `json:"startTime"`
		TerminateTime   int64  `json:"terminateTime"`
		DurationSeconds int64  `json:"durationSeconds"`
	}
	var robotID string
	for _, ev := range events {
		if ev.NewState != BlockLegState {
			continue
		}
		robotID = ev.RobotID
		var got []struct {
			BlockID         string `json:"blockId"`
			Location        string `json:"location"`
			BinTask         string `json:"binTask"`
			StartTime       int64  `json:"startTime"`
			TerminateTime   int64  `json:"terminateTime"`
			DurationSeconds int64  `json:"durationSeconds"`
		}
		testutil.MustNoErr(t, json.Unmarshal([]byte(ev.BlocksJSON), &got), "decode blocks_json")
		if len(got) != 1 {
			t.Fatalf("want one leg in blocks_json, got %d", len(got))
		}
		leg = &got[0]
	}
	if leg == nil {
		t.Fatalf("no %s row written; events=%d", BlockLegState, len(events))
	}

	if leg.BlockID != "b-pickup" || leg.Location != "AP-SOURCE" || leg.BinTask != "Load" {
		t.Errorf("leg identity wrong: %+v", *leg)
	}
	if leg.StartTime != 1784956669 || leg.TerminateTime != 1784956774 {
		t.Errorf("vendor times not stored verbatim: %+v", *leg)
	}
	if leg.DurationSeconds != 105 {
		t.Errorf("durationSeconds = %d, want 105", leg.DurationSeconds)
	}
	// Block events carry no vehicle, so the robot has to come off the order —
	// otherwise every leg row is unattributable, the same defect that left
	// mission_telemetry.robot_id blank.
	if robotID != "AMR-03" {
		t.Errorf("robot_id = %q, want AMR-03 (from the order)", robotID)
	}
}

// A vendor that reports no times must leave duration 0 — "unknown", never
// "instant". A zero that reads as a real measurement would put a fake
// zero-second leg into every percentile computed over these rows.
func TestRecordBlockLeg_MissingTimesLeaveDurationZero(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newUnstartedEngine(t, db, simulator.New())

	orderID := seedLegOrder(t, db, "leg-2")
	eng.recordBlockLeg(BlockCompletedEvent{
		OrderID: orderID, VendorOrderID: "sg-leg-2",
		BlockID: "b1", Location: "L", BinTask: "Load",
		// no StartTime / TerminateTime
	})

	events, err := db.ListMissionEvents(orderID)
	testutil.MustNoErr(t, err, "list mission events")

	found := false
	for _, ev := range events {
		if ev.NewState != BlockLegState {
			continue
		}
		found = true
		var got []map[string]any
		testutil.MustNoErr(t, json.Unmarshal([]byte(ev.BlocksJSON), &got), "decode blocks_json")
		if d, _ := got[0]["durationSeconds"].(float64); d != 0 {
			t.Errorf("durationSeconds = %v, want 0 for an unreported leg", d)
		}
	}
	if !found {
		t.Fatal("no leg row written")
	}
}

// A terminate that precedes its start is vendor garbage, not a negative leg.
func TestRecordBlockLeg_InvertedTimesDoNotProduceNegativeDuration(t *testing.T) {
	t.Parallel()

	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newUnstartedEngine(t, db, simulator.New())

	orderID := seedLegOrder(t, db, "leg-3")
	eng.recordBlockLeg(BlockCompletedEvent{
		OrderID: orderID, VendorOrderID: "sg-leg-3",
		BlockID: "b1", Location: "L", BinTask: "Unload",
		StartTime: 1784956774, TerminateTime: 1784956669,
	})

	events, err := db.ListMissionEvents(orderID)
	testutil.MustNoErr(t, err, "list mission events")
	for _, ev := range events {
		if ev.NewState != BlockLegState {
			continue
		}
		var got []map[string]any
		testutil.MustNoErr(t, json.Unmarshal([]byte(ev.BlocksJSON), &got), "decode blocks_json")
		if d, _ := got[0]["durationSeconds"].(float64); d != 0 {
			t.Errorf("durationSeconds = %v, want 0 for inverted vendor times", d)
		}
	}
}
