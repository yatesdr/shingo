//go:build docker

package store

import "testing"

func TestSyncDemandRegistry_InsertAndResync(t *testing.T) {
	db := testDB(t)

	initial := []DemandRegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "consume", PayloadCode: "WIDGET-A", OutboundDest: "LINE1-IN"},
		{StationID: "line-1", CoreNodeName: "MS-B", Role: "produce", PayloadCode: "WIDGET-B", OutboundDest: ""},
	}
	if err := db.SyncDemandRegistry("line-1", initial); err != nil {
		t.Fatalf("SyncDemandRegistry initial: %v", err)
	}

	all, err := db.ListDemandRegistry()
	if err != nil {
		t.Fatalf("ListDemandRegistry initial: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("initial list len = %d, want 2", len(all))
	}
	codes := map[string]DemandRegistryEntry{}
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

	// Re-sync replaces the previous entries for the station
	replacement := []DemandRegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-Z", Role: "consume", PayloadCode: "WIDGET-Z", OutboundDest: ""},
	}
	if err := db.SyncDemandRegistry("line-1", replacement); err != nil {
		t.Fatalf("SyncDemandRegistry resync: %v", err)
	}
	after, err := db.ListDemandRegistry()
	if err != nil {
		t.Fatalf("ListDemandRegistry after resync: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("after resync len = %d, want 1", len(after))
	}
	if after[0].PayloadCode != "WIDGET-Z" {
		t.Errorf("after resync code = %q, want WIDGET-Z", after[0].PayloadCode)
	}

	// Sync a different station — prior station should be unaffected
	other := []DemandRegistryEntry{
		{StationID: "line-2", CoreNodeName: "MS-OTHER", Role: "consume", PayloadCode: "WIDGET-Y", OutboundDest: ""},
	}
	if err := db.SyncDemandRegistry("line-2", other); err != nil {
		t.Fatalf("SyncDemandRegistry other station: %v", err)
	}
	full, _ := db.ListDemandRegistry()
	if len(full) != 2 {
		t.Errorf("full list len = %d, want 2 (line-1 and line-2)", len(full))
	}
}

func TestLookupDemandRegistry(t *testing.T) {
	db := testDB(t)

	db.SyncDemandRegistry("line-1", []DemandRegistryEntry{
		{StationID: "line-1", CoreNodeName: "N1", Role: "consume", PayloadCode: "P-1", OutboundDest: ""},
	})
	db.SyncDemandRegistry("line-2", []DemandRegistryEntry{
		{StationID: "line-2", CoreNodeName: "N2", Role: "produce", PayloadCode: "P-1", OutboundDest: ""},
	})
	db.SyncDemandRegistry("line-3", []DemandRegistryEntry{
		{StationID: "line-3", CoreNodeName: "N3", Role: "consume", PayloadCode: "P-OTHER", OutboundDest: ""},
	})

	hits, err := db.LookupDemandRegistry("P-1")
	if err != nil {
		t.Fatalf("LookupDemandRegistry: %v", err)
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

	// Miss on a code that is not registered -> empty slice
	none, err := db.LookupDemandRegistry("P-NONEXISTENT")
	if err != nil {
		t.Fatalf("LookupDemandRegistry miss: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("miss returned %d entries, want 0", len(none))
	}
}
