//go:build docker

package demands_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/demands"
)

func TestCoverage_SyncDemandRegistry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	initial := []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "consume", PayloadCode: "WIDGET-A", OutboundDest: "LINE1-IN"},
		{StationID: "line-1", CoreNodeName: "MS-B", Role: "produce", PayloadCode: "WIDGET-B", OutboundDest: ""},
	}
	if _, err := demands.SyncRegistry(db.DB, "line-1", initial); err != nil {
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
	if _, err := demands.SyncRegistry(db.DB, "line-1", replacement); err != nil {
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
	if _, err := demands.SyncRegistry(db.DB, "line-2", other); err != nil {
		t.Fatalf("SyncRegistry other station: %v", err)
	}
	full, _ := demands.ListRegistry(db.DB)
	if len(full) != 2 {
		t.Errorf("full list len = %d, want 2 (line-1 and line-2)", len(full))
	}
}

func TestCoverage_LookupDemandRegistry(t *testing.T) {
	t.Parallel()
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

// TestCoverage_SyncRegistry_ThresholdChangeDetection — SyncRegistry
// returns a RegistryChange for any (loader, payload) whose
// replenish_uop_threshold value moved. New rows (old=0 → new>0),
// changed rows (old=X → new=Y), and deleted rows (old>0 → new=0) all
// fire a change. Threshold-monitor consumes this to reset debounce
// timers — the opt-in default depends on the registry-driven change
// detection working correctly.
func TestCoverage_SyncRegistry_ThresholdChangeDetection(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// First sync: introduce a binding with threshold > 0.
	changes, err := demands.SyncRegistry(db.DB, "line-1", []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "produce", PayloadCode: "P-1", ReplenishUOPThreshold: 10},
	})
	if err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes len = %d, want 1 (new threshold binding)", len(changes))
	}
	if changes[0].OldThreshold != 0 || changes[0].NewThreshold != 10 {
		t.Errorf("change = %+v, want old=0 new=10", changes[0])
	}

	// Re-sync with same threshold: no change.
	changes, _ = demands.SyncRegistry(db.DB, "line-1", []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "produce", PayloadCode: "P-1", ReplenishUOPThreshold: 10},
	})
	if len(changes) != 0 {
		t.Errorf("re-sync with same threshold yielded %d changes, want 0", len(changes))
	}

	// Change the threshold value.
	changes, _ = demands.SyncRegistry(db.DB, "line-1", []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "produce", PayloadCode: "P-1", ReplenishUOPThreshold: 25},
	})
	if len(changes) != 1 {
		t.Fatalf("change-value sync yielded %d changes, want 1", len(changes))
	}
	if changes[0].OldThreshold != 10 || changes[0].NewThreshold != 25 {
		t.Errorf("change = %+v, want old=10 new=25", changes[0])
	}

	// Remove the binding entirely — should surface as a change from 25→0.
	changes, _ = demands.SyncRegistry(db.DB, "line-1", nil)
	if len(changes) != 1 {
		t.Fatalf("removal sync yielded %d changes, want 1", len(changes))
	}
	if changes[0].OldThreshold != 25 || changes[0].NewThreshold != 0 {
		t.Errorf("change = %+v, want old=25 new=0", changes[0])
	}
}

// TestCoverage_LookupThresholdsByPayload returns only the monitored
// (threshold > 0) bindings. Opt-out rows (threshold = 0) are excluded
// per the C-push contract — Core never monitors those pairs, Edge
// owns them via the legacy bin-count path.
func TestCoverage_LookupThresholdsByPayload(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	demands.SyncRegistry(db.DB, "line-1", []demands.RegistryEntry{
		{StationID: "line-1", CoreNodeName: "MS-A", Role: "produce", PayloadCode: "P-1", ReplenishUOPThreshold: 10},
		{StationID: "line-1", CoreNodeName: "MS-B", Role: "produce", PayloadCode: "P-1", ReplenishUOPThreshold: 0},
		{StationID: "line-1", CoreNodeName: "MS-C", Role: "produce", PayloadCode: "P-2", ReplenishUOPThreshold: 5},
	})
	hits, err := demands.LookupThresholdsByPayload(db.DB, "P-1")
	if err != nil {
		t.Fatalf("LookupThresholdsByPayload: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("P-1 hits = %d, want 1 (opt-out row excluded)", len(hits))
	}
	if len(hits) > 0 && hits[0].CoreNodeName != "MS-A" {
		t.Errorf("P-1 monitored binding = %s, want MS-A", hits[0].CoreNodeName)
	}
}
