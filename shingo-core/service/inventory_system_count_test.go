//go:build docker

package service

import (
	"context"
	"testing"

	"shingocore/domain"
	"shingocore/internal/testdb"
)

// TestSystemBinCount_IncludesStaged: a staged bin at a non-storage node
// must still count. The 2026-05-11 SNF2 plant incident — CARRIER-0008
// staged at the consumer line not counting against the 2-bin trigger —
// is the case this test pins.
func TestSystemBinCount_IncludesStaged(t *testing.T) {
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// One available at storage, one staged at the line.
	binStorage := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-SYS-AVAIL")
	binStaged := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.LineNode.ID, "BIN-SYS-STAGED")
	if err := db.UpdateBinStatus(binStaged.ID, domain.BinStatusStaged); err != nil {
		t.Fatalf("set staged: %v", err)
	}
	_ = binStorage // keep linter happy

	svc := NewInventoryService(db)
	result, err := svc.SystemBinCount(context.Background(), []string{std.Payload.Code})
	if err != nil {
		t.Fatalf("SystemBinCount: %v", err)
	}
	if len(result.Counts) != 1 {
		t.Fatalf("Counts len = %d, want 1", len(result.Counts))
	}
	if result.Counts[0].BinCount != 2 {
		t.Errorf("BinCount = %d, want 2 (1 available + 1 staged)", result.Counts[0].BinCount)
	}
}

// TestSystemBinCount_ExcludesOutOfLoop: flagged, maintenance,
// quality_hold, and retired bins are out of the kanban loop. Each is
// individually verified.
func TestSystemBinCount_ExcludesOutOfLoop(t *testing.T) {
	cases := []struct {
		name   string
		status domain.BinStatus
	}{
		{"flagged", domain.BinStatusFlagged},
		{"maintenance", domain.BinStatusMaintenance},
		{"quality_hold", domain.BinStatusQualityHold},
		{"retired", domain.BinStatusRetired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := testdb.Open(t)
			std := testdb.SetupStandardData(t, db)

			// One available, one in the excluded status.
			_ = testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-OUT-A")
			binOut := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-OUT-B")
			if err := db.UpdateBinStatus(binOut.ID, c.status); err != nil {
				t.Fatalf("set %s: %v", c.status, err)
			}

			svc := NewInventoryService(db)
			result, err := svc.SystemBinCount(context.Background(), []string{std.Payload.Code})
			if err != nil {
				t.Fatalf("SystemBinCount: %v", err)
			}
			if len(result.Counts) != 1 || result.Counts[0].BinCount != 1 {
				t.Errorf("BinCount with one %s = %v, want 1 (only the available bin)",
					c.status, result.Counts)
			}
		})
	}
}

// TestSystemBinCount_ZeroForUnseededPayload: payloads with no bins
// appear in Counts with BinCount=0 (callers shouldn't have to handle
// absence as a special case).
func TestSystemBinCount_ZeroForUnseededPayload(t *testing.T) {
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	svc := NewInventoryService(db)
	result, err := svc.SystemBinCount(context.Background(), []string{"PAYLOAD-NO-BINS"})
	if err != nil {
		t.Fatalf("SystemBinCount: %v", err)
	}
	if len(result.Counts) != 1 {
		t.Fatalf("Counts len = %d, want 1", len(result.Counts))
	}
	if result.Counts[0].BinCount != 0 || result.Counts[0].PayloadCode != "PAYLOAD-NO-BINS" {
		t.Errorf("Counts[0] = %+v, want {PAYLOAD-NO-BINS, 0}", result.Counts[0])
	}
}

// TestSystemBinCount_EmptyPayloadCodeRejected: same construction-bug
// guard as PreflightAvailability — silent zero on a typo would mask the
// problem at the wrong layer.
func TestSystemBinCount_EmptyPayloadCodeRejected(t *testing.T) {
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	svc := NewInventoryService(db)
	_, err := svc.SystemBinCount(context.Background(), []string{""})
	if err == nil {
		t.Fatal("expected error for empty payload code in request")
	}
}

// TestSystemBinCount_PreservesRequestOrder: callers can index into
// Counts by request position without re-sorting.
func TestSystemBinCount_PreservesRequestOrder(t *testing.T) {
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	_ = testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-ORDER-1")

	svc := NewInventoryService(db)
	payloads := []string{"PAYLOAD-X", std.Payload.Code, "PAYLOAD-Y"}
	result, err := svc.SystemBinCount(context.Background(), payloads)
	if err != nil {
		t.Fatalf("SystemBinCount: %v", err)
	}
	if len(result.Counts) != 3 {
		t.Fatalf("Counts len = %d, want 3", len(result.Counts))
	}
	for i, want := range payloads {
		if result.Counts[i].PayloadCode != want {
			t.Errorf("Counts[%d].PayloadCode = %q, want %q", i, result.Counts[i].PayloadCode, want)
		}
	}
}
