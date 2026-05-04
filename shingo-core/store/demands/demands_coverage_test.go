
//go:build docker

package demands_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/demands"
)

func TestCoverage_DemandRemaining(t *testing.T) {
	cases := []struct {
		name     string
		demand   int64
		produced int64
		want     int64
	}{
		{"fresh", 100, 0, 100},
		{"partial", 100, 40, 60},
		{"equal", 50, 50, 0},
		{"over-produced", 50, 75, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &demands.Demand{DemandQty: tc.demand, ProducedQty: tc.produced}
			if got := d.Remaining(); got != tc.want {
				t.Errorf("Remaining = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCoverage_DemandCRUD(t *testing.T) {
	db := testdb.Open(t)

	id, err := demands.Create(db.DB, "CAT-001", "Widget catalog", 100)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == 0 {
		t.Fatal("Create returned 0 id")
	}

	got, err := demands.Get(db.DB, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
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

	byCat, err := demands.GetByCatID(db.DB, "CAT-001")
	if err != nil {
		t.Fatalf("GetByCatID: %v", err)
	}
	if byCat.ID != id {
		t.Errorf("GetByCatID ID = %d, want %d", byCat.ID, id)
	}

	if err := demands.Update(db.DB, id, "CAT-001", "Widget catalog v2", 250, 30); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, _ := demands.Get(db.DB, id)
	if after.Description != "Widget catalog v2" {
		t.Errorf("Description after update = %q", after.Description)
	}
	if after.DemandQty != 250 {
		t.Errorf("DemandQty after update = %d, want 250", after.DemandQty)
	}
	if after.ProducedQty != 30 {
		t.Errorf("ProducedQty after update = %d, want 30", after.ProducedQty)
	}

	if err := demands.UpdateAndResetProduced(db.DB, id, "Widget catalog v3", 400); err != nil {
		t.Fatalf("UpdateAndResetProduced: %v", err)
	}
	reset, _ := demands.Get(db.DB, id)
	if reset.DemandQty != 400 {
		t.Errorf("DemandQty after reset = %d, want 400", reset.DemandQty)
	}
	if reset.ProducedQty != 0 {
		t.Errorf("ProducedQty after reset = %d, want 0", reset.ProducedQty)
	}
	if reset.Description != "Widget catalog v3" {
		t.Errorf("Description after reset = %q", reset.Description)
	}

	if err := demands.Delete(db.DB, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := demands.Get(db.DB, id); err == nil {
		t.Error("Get after delete should error")
	}
}

func TestCoverage_DemandProducedOps(t *testing.T) {
	db := testdb.Open(t)

	id1, _ := demands.Create(db.DB, "CAT-A", "A", 100)
	id2, _ := demands.Create(db.DB, "CAT-B", "B", 200)

	if err := demands.IncrementProduced(db.DB, "CAT-A", 10); err != nil {
		t.Fatalf("IncrementProduced: %v", err)
	}
	if err := demands.IncrementProduced(db.DB, "CAT-A", 5); err != nil {
		t.Fatalf("IncrementProduced 2: %v", err)
	}
	d1, _ := demands.Get(db.DB, id1)
	if d1.ProducedQty != 15 {
		t.Errorf("CAT-A produced = %d, want 15", d1.ProducedQty)
	}

	if err := demands.SetProduced(db.DB, id2, 77); err != nil {
		t.Fatalf("SetProduced: %v", err)
	}
	d2, _ := demands.Get(db.DB, id2)
	if d2.ProducedQty != 77 {
		t.Errorf("CAT-B produced after SetProduced = %d, want 77", d2.ProducedQty)
	}

	if err := demands.ClearProduced(db.DB, id1); err != nil {
		t.Fatalf("ClearProduced: %v", err)
	}
	d1b, _ := demands.Get(db.DB, id1)
	if d1b.ProducedQty != 0 {
		t.Errorf("CAT-A produced after ClearProduced = %d, want 0", d1b.ProducedQty)
	}
	d2b, _ := demands.Get(db.DB, id2)
	if d2b.ProducedQty != 77 {
		t.Errorf("CAT-B should be untouched by ClearProduced, got %d", d2b.ProducedQty)
	}

	demands.SetProduced(db.DB, id1, 3)
	if err := demands.ClearAllProduced(db.DB); err != nil {
		t.Fatalf("ClearAllProduced: %v", err)
	}
	d1c, _ := demands.Get(db.DB, id1)
	d2c, _ := demands.Get(db.DB, id2)
	if d1c.ProducedQty != 0 || d2c.ProducedQty != 0 {
		t.Errorf("after ClearAllProduced CAT-A=%d CAT-B=%d, want both 0",
			d1c.ProducedQty, d2c.ProducedQty)
	}
}

func TestCoverage_ListDemands(t *testing.T) {
	db := testdb.Open(t)

	demands.Create(db.DB, "CAT-C", "C desc", 10)
	demands.Create(db.DB, "CAT-A", "A desc", 20)
	demands.Create(db.DB, "CAT-B", "B desc", 30)

	list, err := demands.List(db.DB)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	if list[0].CatID != "CAT-A" || list[1].CatID != "CAT-B" || list[2].CatID != "CAT-C" {
		t.Errorf("order = [%s,%s,%s], want [CAT-A,CAT-B,CAT-C]",
			list[0].CatID, list[1].CatID, list[2].CatID)
	}
}

func TestCoverage_LogProduction(t *testing.T) {
	db := testdb.Open(t)

	if err := demands.LogProduction(db.DB, "CAT-X", "line-1", 5); err != nil {
		t.Fatalf("LogProduction 1: %v", err)
	}
	if err := demands.LogProduction(db.DB, "CAT-X", "line-2", 10); err != nil {
		t.Fatalf("LogProduction 2: %v", err)
	}
	if err := demands.LogProduction(db.DB, "CAT-Y", "line-1", 7); err != nil {
		t.Fatalf("LogProduction 3: %v", err)
	}

	entries, err := demands.ListProductionLog(db.DB, "CAT-X", 10)
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
	var total int64
	for _, e := range entries {
		total += e.Quantity
	}
	if total != 15 {
		t.Errorf("CAT-X total = %d, want 15", total)
	}
}

func TestCoverage_SyncDemandRegistry(t *testing.T) {
	db := testdb.Open(t)

	initial := []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "consume", PayloadCode: "WIDGET-A", OutboundDest: "LINE1-IN"},
		{StationID: "line-1", CoreNodeName: "MS-B", Role: "produce", PayloadCode: "WIDGET-B", OutboundDest: ""},
	}
	if err := demands.SyncRegistry(db.DB, "line-1", initial); err != nil {
		t.Fatalf("SyncRegistry initial: %v", err)
	}

	all, err := demands.ListRegistry(db.DB)
	if err != nil {
		t.Fatalf("ListRegistry initial: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("initial list len = %d, want 2", len(all))
	}
	codes := map[string]demands.RegistryEntry{}
	for _, e := range all {
		codes[e.PayloadCode] = e
	}
	if codes["WIDGET-A"].CoreNodeName != "MS-A" {
		t.Errorf("WIDGET-A core node = %q, want MS-A", codes["WIDGET-A"].CoreNodeName)
	}
	if codes["WIDGET-A"].Role != "consume" {
		t.Errorf("WIDGET-A role = %q, want consume", codes["WIDGET-A"].Role)
	}
	if codes["WIDGET-B"].Role != "produce" {
		t.Errorf("WIDGET-B role = %q, want produce", codes["WIDGET-B"].Role)
	}

	replacement := []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-Z", Role: "consume", PayloadCode: "WIDGET-Z", OutboundDest: ""},
	}
	if err := demands.SyncRegistry(db.DB, "line-1", replacement); err != nil {
		t.Fatalf("SyncRegistry resync: %v", err)
	}
	after, err := demands.ListRegistry(db.DB)
	if err != nil {
		t.Fatalf("ListRegistry after resync: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after resync len = %d, want 1", len(after))
	}
	if after[0].PayloadCode != "WIDGET-Z" {
		t.Errorf("after resync code = %q, want WIDGET-Z", after[0].PayloadCode)
	}

	other := []demands.RegistryEntry{
		{StationID: "line-2", CoreNodeName: "MS-OTHER", Role: "consume", PayloadCode: "WIDGET-Y", OutboundDest: ""},
	}
	if err := demands.SyncRegistry(db.DB, "line-2", other); err != nil {
		t.Fatalf("SyncRegistry other station: %v", err)
	}
	full, _ := demands.ListRegistry(db.DB)
	if len(full) != 2 {
		t.Errorf("full list len = %d, want 2 (line-1 and line-2)", len(full))
	}
}

func TestCoverage_LookupDemandRegistry(t *testing.T) {
	db := testdb.Open(t)

	demands.SyncRegistry(db.DB, "line-1", []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "N1", Role: "consume", PayloadCode: "P-1", OutboundDest: ""},
	})
	demands.SyncRegistry(db.DB, "line-2", []demands.RegistryEntry{
		{StationID: "line-2", CoreNodeName: "N2", Role: "produce", PayloadCode: "P-1", OutboundDest: ""},
	})
	demands.SyncRegistry(db.DB, "line-3", []demands.RegistryEntry{
		{StationID: "line-3", CoreNodeName: "N3", Role: "consume", PayloadCode: "P-OTHER", OutboundDest: ""},
	})

	hits, err := demands.LookupRegistry(db.DB, "P-1")
	if err != nil {
		t.Fatalf("LookupRegistry: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits len = %d, want 2", len(hits))
	}
	stations := map[string]bool{}
	for _, e := range hits {
		stations[e.StationID] = true
	}
	if !stations["line-1"] || !stations["line-2"] {
		t.Errorf("hit stations = %+v, want line-1+line-2", stations)
	}

	none, err := demands.LookupRegistry(db.DB, "P-NONEXISTENT")
	if err != nil {
		t.Fatalf("LookupRegistry miss: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("miss returned %d entries, want 0", len(none))
	}
}

