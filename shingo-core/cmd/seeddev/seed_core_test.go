//go:build docker

package main

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/plantspec"
)

// Verifies seedCore writes the demo plant's core topology into a real Postgres
// (testcontainers) and is idempotent on re-run.
func TestSeedCore_DemoPlant(t *testing.T) {
	db := testdb.Open(t)

	plant, err := plantspec.Load("../../../plants/demo.yaml")
	if err != nil {
		t.Fatalf("load demo plant: %v", err)
	}
	if err := plant.Validate(); err != nil {
		t.Fatalf("validate demo plant: %v", err)
	}

	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore: %v", err)
	}

	// Storage hierarchy: NGRP group → deep lane → depth-ordered slot. SMN_002 is the
	// depth-2 (buried) slot of Lane_01 in SYN_SM_Stamp — its seeded BIN-LH-02 sits
	// behind the mouth bin, the geometry FIFO retrieve must reshuffle past. Names
	// follow docs/node-naming-standards.md (SYN_ group, Lane_NN lane, SMN_ slot).
	zone, err := db.GetNodeByName("SYN_SM_Stamp")
	if err != nil || zone == nil {
		t.Fatalf("zone SYN_SM_Stamp: %v", err)
	}
	slot, err := db.GetNodeByName("SMN_002")
	if err != nil || slot == nil {
		t.Fatalf("slot SMN_002: %v", err)
	}
	if slot.Depth == nil || *slot.Depth != 2 {
		t.Fatalf("SMN_002 depth: want 2 (buried), got %v", slot.Depth)
	}
	if slot.ParentID == nil {
		t.Fatal("SMN_002 should have a lane parent")
	}

	// Payload + a loaded bin sitting in the slot.
	pl, err := db.GetPayloadByCode("PANEL-LH")
	if err != nil || pl == nil {
		t.Fatalf("payload PANEL-LH: %v", err)
	}
	if pl.UOPCapacity != 30 {
		t.Fatalf("PANEL-LH capacity: want 30, got %d", pl.UOPCapacity)
	}
	bin, err := db.GetBinByLabel("BIN-LH-02")
	if err != nil || bin == nil {
		t.Fatalf("bin BIN-LH-02: %v", err)
	}
	if bin.PayloadCode != "PANEL-LH" || bin.UOPRemaining != 30 {
		t.Fatalf("BIN-LH-02: want PANEL-LH/30, got %s/%d", bin.PayloadCode, bin.UOPRemaining)
	}
	if bin.NodeID == nil || *bin.NodeID != slot.ID {
		t.Fatalf("BIN-LH-02 should sit at SMN_002 (id %d), got %v", slot.ID, bin.NodeID)
	}

	// A station node exists (composite cell node — WELD-1 consume PANEL-LH).
	if n, err := db.GetNodeByName("ALN_001"); err != nil || n == nil {
		t.Fatalf("station ALN_001: %v", err)
	}

	// Idempotent: a second run must not error or duplicate.
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore re-run (idempotency): %v", err)
	}
}
