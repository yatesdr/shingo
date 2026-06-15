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

	// Multi-window synthetic loader. PLK_LOADER is a shared_window loader keyed on a
	// SYNTHETIC identity — it must NOT be a node — with its three windows as homes
	// and BRKT as its shared payload. This is the clean multi-window shape: no phantom
	// anchor node that never receives a bin; the windows are the loader's only nodes.
	if n, _ := db.GetNodeByName("PLK_LOADER"); n != nil {
		t.Fatalf("PLK_LOADER must NOT be a node (synthetic identity), got node id %d", n.ID)
	}
	loader, err := db.GetLoaderByName("PLK_LOADER", "produce")
	if err != nil || loader == nil {
		t.Fatalf("bin_loaders PLK_LOADER: %v", err)
	}
	if loader.Layout != "shared_window" {
		t.Fatalf("PLK_LOADER layout: want shared_window, got %q", loader.Layout)
	}
	homes, err := db.ListLoaderHomes(loader.ID)
	if err != nil {
		t.Fatalf("ListLoaderHomes: %v", err)
	}
	gotWindows := map[string]bool{}
	for _, h := range homes {
		hn, err := db.GetNode(h.PositionNodeID)
		if err != nil || hn == nil {
			t.Fatalf("home node %d: %v", h.PositionNodeID, err)
		}
		gotWindows[hn.Name] = true
	}
	if len(homes) != 3 {
		t.Fatalf("PLK_LOADER homes: want 3 windows, got %d (%v)", len(homes), gotWindows)
	}
	for _, w := range []string{"PLK_W1", "PLK_W2", "PLK_W3"} {
		if !gotWindows[w] {
			t.Fatalf("PLK_LOADER missing window home %s (got %v)", w, gotWindows)
		}
	}
	pls, err := db.ListLoaderPayloads(loader.ID)
	if err != nil {
		t.Fatalf("ListLoaderPayloads: %v", err)
	}
	if len(pls) != 1 || pls[0].PayloadCode != "BRKT" {
		t.Fatalf("PLK_LOADER payloads: want [BRKT], got %+v", pls)
	}
	// The loader has no node of its own (core_node_name dropped), so its demand rows are
	// addressed at its window node(s); query by loader_id (order-independent). Role +
	// threshold resolve from the window claim: produce, threshold 80.
	var dRole string
	var dThresh int
	if err := db.QueryRow(`SELECT role, replenish_uop_threshold FROM demand_registry
		WHERE loader_id=$1 AND payload_code='BRKT'`, loader.ID).Scan(&dRole, &dThresh); err != nil {
		t.Fatalf("demand_registry for PLK_LOADER (loader_id=%d): %v", loader.ID, err)
	}
	if dRole != "produce" || dThresh != 80 {
		t.Fatalf("PLK_LOADER demand: want produce/80, got %s/%d", dRole, dThresh)
	}

	// Single-window loader: PLK_X1 is its own real-node shared_window loader (the classic
	// single-node shape — distinct from the synthetic multi-window PLK_LOADER).
	x1, err := db.GetLoaderByName("PLK_X1", "produce")
	if err != nil || x1 == nil {
		t.Fatalf("bin_loaders PLK_X1: %v", err)
	}
	if x1.Layout != "shared_window" {
		t.Fatalf("PLK_X1 layout: want shared_window, got %q", x1.Layout)
	}
	// Step 1: its anchor is materialised as the sole window member (no zero-member loader).
	x1homes, err := db.ListLoaderHomes(x1.ID)
	if err != nil {
		t.Fatalf("ListLoaderHomes PLK_X1: %v", err)
	}
	if len(x1homes) != 1 {
		t.Fatalf("PLK_X1 homes: want 1 materialised anchor window, got %d", len(x1homes))
	}
	if hn, _ := db.GetNode(x1homes[0].PositionNodeID); hn == nil || hn.Name != "PLK_X1" {
		t.Fatalf("PLK_X1 sole window must be its own anchor node PLK_X1, got %+v", hn)
	}

	// Dedicated-positions loader: PLK_DECK keyed on a SYNTHETIC id (must not be a node),
	// two BRKT positions (same-payload-two-position), buffer_dest wired for step 7.
	if n, _ := db.GetNodeByName("PLK_DECK"); n != nil {
		t.Fatalf("PLK_DECK must NOT be a node (synthetic identity), got node id %d", n.ID)
	}
	deck, err := db.GetLoaderByName("PLK_DECK", "produce")
	if err != nil || deck == nil {
		t.Fatalf("bin_loaders PLK_DECK: %v", err)
	}
	if deck.Layout != "dedicated_positions" {
		t.Fatalf("PLK_DECK layout: want dedicated_positions, got %q", deck.Layout)
	}
	if deck.BufferDest != "SYN_BUF_Deck" {
		t.Fatalf("PLK_DECK buffer_dest: want SYN_BUF_Deck, got %q", deck.BufferDest)
	}
	dh, err := db.ListLoaderHomes(deck.ID)
	if err != nil {
		t.Fatalf("ListLoaderHomes PLK_DECK: %v", err)
	}
	if len(dh) != 2 {
		t.Fatalf("PLK_DECK homes: want 2 positions, got %d", len(dh))
	}
	gotPos := map[string]string{} // node name → payload
	for _, h := range dh {
		hn, err := db.GetNode(h.PositionNodeID)
		if err != nil || hn == nil {
			t.Fatalf("home node %d: %v", h.PositionNodeID, err)
		}
		gotPos[hn.Name] = h.PayloadCode
	}
	for _, pn := range []string{"PLK_D1", "PLK_D2"} {
		if gotPos[pn] != "STUD" {
			t.Fatalf("PLK_DECK position %s payload: want STUD (same-payload-two-position, de-confounded loop), got %q", pn, gotPos[pn])
		}
	}
	// Per-position demand rows: same payload on both positions, each its own registry row
	// at threshold 80 (the pooled-detection / member-aware-routing fixture).
	for _, pn := range []string{"PLK_D1", "PLK_D2"} {
		var thr int
		if err := db.QueryRow(`SELECT replenish_uop_threshold FROM demand_registry
			WHERE core_node_name=$1 AND payload_code='STUD'`, pn).Scan(&thr); err != nil {
			t.Fatalf("demand_registry %s: %v", pn, err)
		}
		if thr != 80 {
			t.Fatalf("%s demand threshold: want 80, got %d", pn, thr)
		}
	}

	// Step 1 invariant: NO loader resolves to zero members. Every shared loader has its
	// windows (or a materialised anchor window); every dedicated loader has its positions.
	all, err := db.ListLoaders()
	if err != nil {
		t.Fatalf("ListLoaders: %v", err)
	}
	for _, l := range all {
		h, err := db.ListLoaderHomes(l.ID)
		if err != nil {
			t.Fatalf("ListLoaderHomes %s: %v", l.Name, err)
		}
		if len(h) == 0 {
			t.Fatalf("loader %s/%s resolves to ZERO members — step-1 invariant violated", l.Name, l.Role)
		}
	}

	// Idempotent: a second run must not error or duplicate.
	if err := seedCore(db, plant, map[string]int64{}); err != nil {
		t.Fatalf("seedCore re-run (idempotency): %v", err)
	}
}
