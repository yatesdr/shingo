//go:build docker

package store

import "testing"

func TestDemandRemaining(t *testing.T) {
	cases := []struct {
		name    string
		demand  int64
		produced int64
		want    int64
	}{
		{"fresh", 100, 0, 100},
		{"partial", 100, 40, 60},
		{"equal", 50, 50, 0},
		{"over-produced", 50, 75, 0}, // clamped to 0
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Demand{DemandQty: tc.demand, ProducedQty: tc.produced}
			if got := d.Remaining(); got != tc.want {
				t.Errorf("Remaining = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDemandCRUD(t *testing.T) {
	db := testDB(t)

	// Create
	id, err := db.CreateDemand("CAT-001", "Widget catalog", 100)
	if err != nil {
		t.Fatalf("CreateDemand: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateDemand returned 0 id")
	}

	// Get — read back verifies insert persisted
	got, err := db.GetDemand(id)
	if err != nil {
		t.Fatalf("GetDemand: %v", err)
	}
	if got.CatID != "CAT-001" {
		t.Errorf("CatID = %q, want CAT-001", got.CatID)
	}
	if got.Description != "Widget catalog" {
		t.Errorf("Description = %q, want Widget catalog", got.Description)
	}
	if got.DemandQty != 100 {
		t.Errorf("DemandQty = %d, want 100", got.DemandQty)
	}
	if got.ProducedQty != 0 {
		t.Errorf("ProducedQty = %d, want 0", got.ProducedQty)
	}

	// GetByCatID — same row via alternate lookup
	byCat, err := db.GetDemandByCatID("CAT-001")
	if err != nil {
		t.Fatalf("GetDemandByCatID: %v", err)
	}
	if byCat.ID != id {
		t.Errorf("GetDemandByCatID ID = %d, want %d", byCat.ID, id)
	}

	// Update
	if err := db.UpdateDemand(id, "CAT-001", "Widget catalog v2", 250, 30); err != nil {
		t.Fatalf("UpdateDemand: %v", err)
	}
	after, _ := db.GetDemand(id)
	if after.Description != "Widget catalog v2" {
		t.Errorf("Description after update = %q", after.Description)
	}
	if after.DemandQty != 250 {
		t.Errorf("DemandQty after update = %d, want 250", after.DemandQty)
	}
	if after.ProducedQty != 30 {
		t.Errorf("ProducedQty after update = %d, want 30", after.ProducedQty)
	}

	// UpdateDemandAndResetProduced — resets produced to zero
	if err := db.UpdateDemandAndResetProduced(id, "Widget catalog v3", 400); err != nil {
		t.Fatalf("UpdateDemandAndResetProduced: %v", err)
	}
	reset, _ := db.GetDemand(id)
	if reset.DemandQty != 400 {
		t.Errorf("DemandQty after reset = %d, want 400", reset.DemandQty)
	}
	if reset.ProducedQty != 0 {
		t.Errorf("ProducedQty after reset = %d, want 0", reset.ProducedQty)
	}
	if reset.Description != "Widget catalog v3" {
		t.Errorf("Description after reset = %q", reset.Description)
	}

	// Delete
	if err := db.DeleteDemand(id); err != nil {
		t.Fatalf("DeleteDemand: %v", err)
	}
	if _, err := db.GetDemand(id); err == nil {
		t.Error("GetDemand after delete should error")
	}
}

func TestDemandProducedOps(t *testing.T) {
	db := testDB(t)

	id1, _ := db.CreateDemand("CAT-A", "A", 100)
	id2, _ := db.CreateDemand("CAT-B", "B", 200)

	// IncrementProduced (by cat_id)
	if err := db.IncrementProduced("CAT-A", 10); err != nil {
		t.Fatalf("IncrementProduced: %v", err)
	}
	if err := db.IncrementProduced("CAT-A", 5); err != nil {
		t.Fatalf("IncrementProduced 2: %v", err)
	}
	d1, _ := db.GetDemand(id1)
	if d1.ProducedQty != 15 {
		t.Errorf("CAT-A produced = %d, want 15", d1.ProducedQty)
	}

	// SetProduced sets absolute value
	if err := db.SetProduced(id2, 77); err != nil {
		t.Fatalf("SetProduced: %v", err)
	}
	d2, _ := db.GetDemand(id2)
	if d2.ProducedQty != 77 {
		t.Errorf("CAT-B produced after SetProduced = %d, want 77", d2.ProducedQty)
	}

	// ClearProduced — single
	if err := db.ClearProduced(id1); err != nil {
		t.Fatalf("ClearProduced: %v", err)
	}
	d1b, _ := db.GetDemand(id1)
	if d1b.ProducedQty != 0 {
		t.Errorf("CAT-A produced after ClearProduced = %d, want 0", d1b.ProducedQty)
	}
	// CAT-B should still be 77
	d2b, _ := db.GetDemand(id2)
	if d2b.ProducedQty != 77 {
		t.Errorf("CAT-B should be untouched by ClearProduced, got %d", d2b.ProducedQty)
	}

	// ClearAllProduced — resets every row
	db.SetProduced(id1, 3)
	if err := db.ClearAllProduced(); err != nil {
		t.Fatalf("ClearAllProduced: %v", err)
	}
	d1c, _ := db.GetDemand(id1)
	d2c, _ := db.GetDemand(id2)
	if d1c.ProducedQty != 0 || d2c.ProducedQty != 0 {
		t.Errorf("after ClearAllProduced CAT-A=%d CAT-B=%d, want both 0",
			d1c.ProducedQty, d2c.ProducedQty)
	}
}

func TestListDemands(t *testing.T) {
	db := testDB(t)

	// Insert in unsorted order; ListDemands orders by cat_id
	db.CreateDemand("CAT-C", "C desc", 10)
	db.CreateDemand("CAT-A", "A desc", 20)
	db.CreateDemand("CAT-B", "B desc", 30)

	list, err := db.ListDemands()
	if err != nil {
		t.Fatalf("ListDemands: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Check alphabetical ordering by cat_id
	if list[0].CatID != "CAT-A" || list[1].CatID != "CAT-B" || list[2].CatID != "CAT-C" {
		t.Errorf("order = [%s,%s,%s], want [CAT-A,CAT-B,CAT-C]",
			list[0].CatID, list[1].CatID, list[2].CatID)
	}
}

func TestLogProduction(t *testing.T) {
	db := testDB(t)

	// Log several production entries
	if err := db.LogProduction("CAT-X", "line-1", 5); err != nil {
		t.Fatalf("LogProduction 1: %v", err)
	}
	if err := db.LogProduction("CAT-X", "line-2", 10); err != nil {
		t.Fatalf("LogProduction 2: %v", err)
	}
	if err := db.LogProduction("CAT-Y", "line-1", 7); err != nil {
		t.Fatalf("LogProduction 3: %v", err)
	}

	// Read back via ListProductionLog
	entries, err := db.ListProductionLog("CAT-X", 10)
	if err != nil {
		t.Fatalf("ListProductionLog: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("CAT-X entries = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.CatID != "CAT-X" {
			t.Errorf("entry CatID = %q, want CAT-X", e.CatID)
		}
	}
	// Verify per-station fidelity
	var total int64
	for _, e := range entries {
		total += e.Quantity
	}
	if total != 15 {
		t.Errorf("CAT-X total = %d, want 15", total)
	}
}
