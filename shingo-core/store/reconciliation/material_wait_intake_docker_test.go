//go:build docker

package reconciliation_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// TestListAnomalies_AMaterialWaitParkedByTheResolverKeepsTheLongerBound: the
// NGRP resolver parks a complex order on material under its OWN causes —
// intake-resolve at intake, ngrp-resolve on replay — and both carry the
// waiting_for_material code. They are the same shortage the finder tiers report,
// found one layer earlier, so they get the same two hours.
//
// A SLOT-kind resolver park (the group has no free child) writes the same two
// causes under waiting_for_slot. That is a throughput wait, like every other
// slot cause, and keeps the 30-minute alarm — the code is what separates them.
//
// DEFECT PIN. Fails at bcbde0d2: materialWaitCauseLiterals lists only the finder
// tiers and reserve-holding, so an hour-old material wait parked by the resolver
// is flagged at 30 minutes — and before the birth-rung move, when that order sat
// `queued`, it got two hours (verdigris-otter F4, ochre-marten F7).
func TestListAnomalies_AMaterialWaitParkedByTheResolverKeepsTheLongerBound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	node := &nodes.Node{Name: "RESOLVE-LINE", Enabled: true}
	if err := nodes.Create(db.DB, node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	mk := func(uuid, code, cause string, ageSeconds int) int64 {
		o := &orders.Order{EdgeUUID: uuid, StationID: "edge.1", OrderType: "complex",
			Status: "sourcing", Quantity: 1, DeliveryNode: node.Name}
		if err := orders.Create(db.DB, o); err != nil {
			t.Fatalf("create %s: %v", uuid, err)
		}
		if _, err := db.DB.Exec(`UPDATE orders
			SET queue_code=$1, queue_cause=$2,
			    updated_at = NOW() - ($3 * INTERVAL '1 second'),
			    created_at = NOW() - ($3 * INTERVAL '1 second')
			WHERE id=$4`, code, cause, ageSeconds, o.ID); err != nil {
			t.Fatalf("backdate %s: %v", uuid, err)
		}
		if _, err := db.DB.Exec(
			`UPDATE order_history SET created_at = NOW() - ($1 * INTERVAL '1 second') WHERE order_id=$2`,
			ageSeconds, o.ID); err != nil {
			t.Fatalf("backdate %s birth row: %v", uuid, err)
		}
		return o.ID
	}

	intakeMaterial := mk("ir-mat", "waiting_for_material", "intake-resolve", 3600)
	replayMaterial := mk("nr-mat", "waiting_for_material", "ngrp-resolve", 3600)
	replaySlot := mk("nr-slot", "waiting_for_slot", "ngrp-resolve", 3600)
	occupied := mk("do-slot", "waiting_for_slot", "dropoff-occupied", 3600)
	oldIntake := mk("ir-old", "waiting_for_material", "intake-resolve", 10800)

	anomalies, err := reconciliation.ListAnomalies(db.DB)
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	flagged := map[int64]bool{}
	for _, a := range anomalies {
		if a.Issue == "active_order_stuck" && a.OrderID != nil {
			flagged[*a.OrderID] = true
		}
	}

	if flagged[intakeMaterial] {
		t.Error("an hour-old material wait parked by the resolver at INTAKE (intake-resolve) was flagged; " +
			"it is the same shortage the finder tiers report and gets the same two hours")
	}
	if flagged[replayMaterial] {
		t.Error("an hour-old material wait parked by the resolver on REPLAY (ngrp-resolve) was flagged")
	}
	if !flagged[replaySlot] {
		t.Error("an hour-old SLOT-kind resolver park (waiting_for_slot / ngrp-resolve) was not flagged — " +
			"the same cause under the slot code is a throughput wait and keeps the 30-minute alarm")
	}
	if !flagged[occupied] {
		t.Error("an hour-old dropoff-occupied wait was not flagged — the slot family alarms at 30 minutes")
	}
	if !flagged[oldIntake] {
		t.Error("a 3h intake-resolve material wait raised nothing — the longer bound is still a bound")
	}
}
