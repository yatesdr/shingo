//go:build docker

package service

import (
	"context"
	"testing"

	"shingocore/internal/testdb"
)

// TestInventoryPreflight_ReturnsMissingPayloads seeds bins for one payload,
// queries for two payloads, and asserts that the unseeded payload appears
// in Missing while the seeded one shows BinCount > 0 in Available.
func TestInventoryPreflight_ReturnsMissingPayloads(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Seed two bins for the standard payload at the standard storage node.
	testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-PRE-1")
	testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-PRE-2")

	svc := NewInventoryService(db)
	result, err := svc.PreflightAvailability(context.Background(), "station-x", []string{std.Payload.Code, "MISSING-PAYLOAD"})
	if err != nil {
		t.Fatalf("PreflightAvailability: %v", err)
	}

	if len(result.Missing) != 1 || result.Missing[0] != "MISSING-PAYLOAD" {
		t.Errorf("Missing = %v, want [MISSING-PAYLOAD]", result.Missing)
	}
	if len(result.Available) != 2 {
		t.Fatalf("Available count = %d, want 2", len(result.Available))
	}
	for _, a := range result.Available {
		switch a.PayloadCode {
		case std.Payload.Code:
			if a.BinCount != 2 {
				t.Errorf("BinCount(%s) = %d, want 2", a.PayloadCode, a.BinCount)
			}
		case "MISSING-PAYLOAD":
			if a.BinCount != 0 {
				t.Errorf("BinCount(MISSING) = %d, want 0", a.BinCount)
			}
		default:
			t.Errorf("unexpected payload in Available: %q", a.PayloadCode)
		}
	}
}

// TestInventoryPreflight_AllAvailable: every requested payload has bins;
// Missing is empty.
func TestInventoryPreflight_AllAvailable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-AVAIL-1")

	svc := NewInventoryService(db)
	result, err := svc.PreflightAvailability(context.Background(), "", []string{std.Payload.Code})
	if err != nil {
		t.Fatalf("PreflightAvailability: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", result.Missing)
	}
	if len(result.Available) != 1 || result.Available[0].BinCount < 1 {
		t.Errorf("Available = %v, want one entry with BinCount >= 1", result.Available)
	}
}

// TestInventoryPreflight_EmptyPayloadCodeRejected: empty payload code in
// the request is a construction bug — return an error rather than silently
// counting unattached bins.
func TestInventoryPreflight_EmptyPayloadCodeRejected(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	svc := NewInventoryService(db)
	_, err := svc.PreflightAvailability(context.Background(), "", []string{""})
	if err == nil {
		t.Fatal("expected error for empty payload code in request")
	}
}
