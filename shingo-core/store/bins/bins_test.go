//go:build docker

package bins_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
)

func TestCreateBin_CRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{
		BinTypeID:   std.BinType.ID,
		Label:       "BIN-CRUD-1",
		Description: "first crud bin",
		NodeID:      &std.StorageNode.ID,
		Status:      "available",
	}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")
	if bin.ID == 0 {
		t.Fatalf("bins.Create: expected ID set, got 0")
	}

	t.Run("Get_returns_inserted_row", func(t *testing.T) {
		got, err := bins.Get(db.DB, bin.ID)
		if err != nil {
			t.Fatalf("bins.Get: %v", err)
		}
		if got.Label != "BIN-CRUD-1" {
			t.Errorf("Label = %q, want %q", got.Label, "BIN-CRUD-1")
		}
		if got.BinTypeCode != "DEFAULT" {
			t.Errorf("BinTypeCode = %q, want %q (joined from bin_types)", got.BinTypeCode, "DEFAULT")
		}
		if got.NodeName != "STORAGE-A1" {
			t.Errorf("NodeName = %q, want %q (joined from nodes)", got.NodeName, "STORAGE-A1")
		}
		if got.NodeID == nil || *got.NodeID != std.StorageNode.ID {
			t.Errorf("NodeID = %v, want %d", got.NodeID, std.StorageNode.ID)
		}
	})

	t.Run("GetByLabel_finds_bin", func(t *testing.T) {
		got, err := bins.GetByLabel(db.DB, "BIN-CRUD-1")
		if err != nil {
			t.Fatalf("bins.GetByLabel: %v", err)
		}
		if got.ID != bin.ID {
			t.Errorf("bins.GetByLabel ID = %d, want %d", got.ID, bin.ID)
		}
	})

	t.Run("Update_persists_changes", func(t *testing.T) {
		bin.Label = "BIN-CRUD-1B"
		bin.Description = "renamed"
		bin.Status = "maintenance"
		testutil.MustNoErr(t, bins.Update(db.DB, bin), "bins.Update")
		got, err := bins.Get(db.DB, bin.ID)
		if err != nil {
			t.Fatalf("bins.Get after update: %v", err)
		}
		if got.Label != "BIN-CRUD-1B" {
			t.Errorf("post-update Label = %q, want %q", got.Label, "BIN-CRUD-1B")
		}
		if got.Description != "renamed" {
			t.Errorf("post-update Description = %q, want %q", got.Description, "renamed")
		}
		if got.Status != "maintenance" {
			t.Errorf("post-update Status = %q, want %q", got.Status, "maintenance")
		}
	})

	t.Run("Delete_removes_bin", func(t *testing.T) {
		testutil.MustNoErr(t, bins.Delete(db.DB, bin.ID), "bins.Delete")
		if _, err := bins.Get(db.DB, bin.ID); err == nil {
			t.Error("bins.Get after bins.Delete: expected error, got nil")
		}
	})
}

func TestList_And_ListByNode_And_Counts(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Three bins at storage, one at line node.
	mkSimple := func(label string, nodeID int64) *bins.Bin {
		b := &bins.Bin{BinTypeID: std.BinType.ID, Label: label, NodeID: &nodeID, Status: "available"}
		if err := bins.Create(db.DB, b); err != nil {
			t.Fatalf("bins.Create %s: %v", label, err)
		}
		return b
	}
	mkSimple("BIN-LIST-1", std.StorageNode.ID)
	mkSimple("BIN-LIST-2", std.StorageNode.ID)
	mkSimple("BIN-LIST-3", std.StorageNode.ID)
	mkSimple("BIN-LIST-4", std.LineNode.ID)

	t.Run("List_returns_all", func(t *testing.T) {
		got, err := bins.List(db.DB)
		if err != nil {
			t.Fatalf("bins.List: %v", err)
		}
		if len(got) != 4 {
			t.Errorf("bins.List len = %d, want 4", len(got))
		}
		// Ordered DESC by ID — first element should be the most recent insert.
		if len(got) > 0 && got[0].Label != "BIN-LIST-4" {
			t.Errorf("bins.List[0].Label = %q, want %q (DESC order)", got[0].Label, "BIN-LIST-4")
		}
	})

	t.Run("ListByNode_filters", func(t *testing.T) {
		got, err := bins.ListByNode(db.DB, std.StorageNode.ID)
		if err != nil {
			t.Fatalf("bins.ListByNode: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("bins.ListByNode storage len = %d, want 3", len(got))
		}
		gotLine, err := bins.ListByNode(db.DB, std.LineNode.ID)
		if err != nil {
			t.Fatalf("bins.ListByNode line: %v", err)
		}
		if len(gotLine) != 1 {
			t.Errorf("bins.ListByNode line len = %d, want 1", len(gotLine))
		}
	})

	t.Run("bins.CountByNode", func(t *testing.T) {
		n, err := bins.CountByNode(db.DB, std.StorageNode.ID)
		if err != nil {
			t.Fatalf("bins.CountByNode: %v", err)
		}
		if n != 3 {
			t.Errorf("bins.CountByNode storage = %d, want 3", n)
		}
	})

	t.Run("bins.CountByAllNodes", func(t *testing.T) {
		counts, err := bins.CountByAllNodes(db.DB)
		if err != nil {
			t.Fatalf("bins.CountByAllNodes: %v", err)
		}
		if counts[std.StorageNode.ID] != 3 {
			t.Errorf("counts[storage]=%d, want 3", counts[std.StorageNode.ID])
		}
		if counts[std.LineNode.ID] != 1 {
			t.Errorf("counts[line]=%d, want 1", counts[std.LineNode.ID])
		}
	})
}

func TestMove(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-MOVE-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")

	t.Run("move_to_different_node", func(t *testing.T) {
		testutil.MustNoErr(t, bins.Move(db.DB, bin.ID, std.LineNode.ID), "bins.Move")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.NodeID == nil || *got.NodeID != std.LineNode.ID {
			t.Errorf("after bins.Move, NodeID = %v, want %d", got.NodeID, std.LineNode.ID)
		}
	})

	t.Run("move_to_same_node_errors", func(t *testing.T) {
		// Already at LineNode after previous sub-test.
		if err := bins.Move(db.DB, bin.ID, std.LineNode.ID); err == nil {
			t.Error("bins.Move to same node: expected error, got nil")
		}
	})
}

func TestClaim_And_Unclaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-CLM-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")

	t.Run("Claim_sets_claimed_by", func(t *testing.T) {
		o := testdb.CreateOrder(t, db)
		testdb.ReserveBin(t, db, o.ID, bin.ID) // demoted-CAS guard requires a pending reservation
		testutil.MustNoErr(t, bins.Claim(db.DB, bin.ID, o.ID), "bins.Claim")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.ClaimedBy == nil || *got.ClaimedBy != o.ID {
			t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, o.ID)
		}
	})

	t.Run("Claim_again_errors_when_already_claimed", func(t *testing.T) {
		// bin is already claimed by the prior subtest; a second claim fails on
		// claimed_by IS NULL before the reservation guard, so no reservation needed.
		o := testdb.CreateOrder(t, db)
		if err := bins.Claim(db.DB, bin.ID, o.ID); err == nil {
			t.Error("second bins.Claim: expected error, got nil")
		}
	})

	t.Run("Unclaim_clears_claim", func(t *testing.T) {
		testutil.MustNoErr(t, bins.Unclaim(db.DB, bin.ID), "bins.Unclaim")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.ClaimedBy != nil {
			t.Errorf("ClaimedBy after bins.Unclaim = %v, want nil", got.ClaimedBy)
		}
	})

	t.Run("UnclaimByOrder_clears_all_for_order", func(t *testing.T) {
		bin2 := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-CLM-2", NodeID: &std.StorageNode.ID, Status: "available"}
		bin3 := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-CLM-3", NodeID: &std.StorageNode.ID, Status: "available"}
		testutil.MustNoErr(t, bins.Create(db.DB, bin2), "bins.Create bin2")
		testutil.MustNoErr(t, bins.Create(db.DB, bin3), "bins.Create bin3")
		o := testdb.CreateOrder(t, db)
		testdb.ReserveBin(t, db, o.ID, bin2.ID)
		testdb.ReserveBin(t, db, o.ID, bin3.ID)
		testutil.MustNoErr(t, bins.Claim(db.DB, bin2.ID, o.ID), "bins.Claim bin2")
		testutil.MustNoErr(t, bins.Claim(db.DB, bin3.ID, o.ID), "bins.Claim bin3")
		bins.UnclaimByOrder(db.DB, o.ID)
		g2, _ := bins.Get(db.DB, bin2.ID)
		g3, _ := bins.Get(db.DB, bin3.ID)
		if g2.ClaimedBy != nil || g3.ClaimedBy != nil {
			t.Errorf("after bins.UnclaimByOrder, ClaimedBy bin2=%v bin3=%v, want both nil", g2.ClaimedBy, g3.ClaimedBy)
		}
	})

	t.Run("Claim_locked_bin_errors", func(t *testing.T) {
		bin4 := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-CLM-LOCKED", NodeID: &std.StorageNode.ID, Status: "available"}
		testutil.MustNoErr(t, bins.Create(db.DB, bin4), "bins.Create bin4")
		testutil.MustNoErr(t, bins.Lock(db.DB, bin4.ID, "tester"), "bins.Lock")
		o := testdb.CreateOrder(t, db)
		testdb.ReserveBin(t, db, o.ID, bin4.ID) // reserve so the claim fails on the LOCK, not a missing reservation
		if err := bins.Claim(db.DB, bin4.ID, o.ID); err == nil {
			t.Error("bins.Claim of locked bin: expected error, got nil")
		}
	})
}

func TestLock_And_Unlock(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-LOCK-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")

	testutil.MustNoErr(t, bins.Lock(db.DB, bin.ID, "alice"), "bins.Lock")
	got, _ := bins.Get(db.DB, bin.ID)
	if !got.Locked {
		t.Error("Locked = false, want true")
	}
	if got.LockedBy != "alice" {
		t.Errorf("LockedBy = %q, want %q", got.LockedBy, "alice")
	}
	if got.LockedAt == nil {
		t.Error("LockedAt = nil, want set")
	}

	t.Run("double_Lock_errors", func(t *testing.T) {
		if err := bins.Lock(db.DB, bin.ID, "bob"); err == nil {
			t.Error("bins.Lock already-locked: expected error, got nil")
		}
	})

	t.Run("Unlock_clears", func(t *testing.T) {
		testutil.MustNoErr(t, bins.Unlock(db.DB, bin.ID), "bins.Unlock")
		got2, _ := bins.Get(db.DB, bin.ID)
		if got2.Locked {
			t.Error("Locked after bins.Unlock = true, want false")
		}
		if got2.LockedAt != nil {
			t.Errorf("LockedAt after bins.Unlock = %v, want nil", got2.LockedAt)
		}
	})
}

func TestUpdateStatus(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-STATUS-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")
	testutil.MustNoErr(t, bins.UpdateStatus(db.DB, bin.ID, "flagged"), "bins.UpdateStatus")
	got, _ := bins.Get(db.DB, bin.ID)
	if got.Status != "flagged" {
		t.Errorf("Status = %q, want %q", got.Status, "flagged")
	}
}

func TestStage_Release_And_ReleaseExpired(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-STAGE-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")

	t.Run("Stage_no_expiry", func(t *testing.T) {
		testutil.MustNoErr(t, bins.Stage(db.DB, bin.ID, nil), "bins.Stage")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.Status != "staged" {
			t.Errorf("Status after bins.Stage = %q, want %q", got.Status, "staged")
		}
		if got.StagedAt == nil {
			t.Error("StagedAt = nil, want set")
		}
		if got.StagedExpiresAt != nil {
			t.Errorf("StagedExpiresAt = %v, want nil (no expiry)", got.StagedExpiresAt)
		}
	})

	t.Run("ReleaseStaged_clears", func(t *testing.T) {
		testutil.MustNoErr(t, bins.ReleaseStaged(db.DB, bin.ID), "bins.ReleaseStaged")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.Status != "available" {
			t.Errorf("Status after bins.ReleaseStaged = %q, want %q", got.Status, "available")
		}
		if got.StagedAt != nil {
			t.Errorf("StagedAt after release = %v, want nil", got.StagedAt)
		}
	})

	t.Run("ReleaseExpiredStaged_releases_only_expired", func(t *testing.T) {
		// One bin staged with expiry in the past, one with expiry in the future.
		expired := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-STAGE-EXP", NodeID: &std.StorageNode.ID, Status: "available"}
		future := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-STAGE-FUT", NodeID: &std.StorageNode.ID, Status: "available"}
		testutil.MustNoErr(t, bins.Create(db.DB, expired), "bins.Create expired")
		testutil.MustNoErr(t, bins.Create(db.DB, future), "bins.Create future")
		past := time.Now().Add(-1 * time.Hour)
		soon := time.Now().Add(1 * time.Hour)
		testutil.MustNoErr(t, bins.Stage(db.DB, expired.ID, &past), "bins.Stage expired")
		testutil.MustNoErr(t, bins.Stage(db.DB, future.ID, &soon), "bins.Stage future")

		n, err := bins.ReleaseExpiredStaged(db.DB)
		if err != nil {
			t.Fatalf("bins.ReleaseExpiredStaged: %v", err)
		}
		if n != 1 {
			t.Errorf("bins.ReleaseExpiredStaged released = %d, want 1", n)
		}
		gExp, _ := bins.Get(db.DB, expired.ID)
		gFut, _ := bins.Get(db.DB, future.ID)
		if gExp.Status != "available" {
			t.Errorf("expired bin Status = %q, want %q", gExp.Status, "available")
		}
		if gFut.Status != "staged" {
			t.Errorf("future bin Status = %q, want %q", gFut.Status, "staged")
		}
	})
}

func TestRecordCount_And_UnconfirmManifest(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-CNT-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")
	testutil.MustNoErr(t, bins.SetManifest(db.DB, bin.ID, `{"items":[]}`, std.Payload.Code, 100), "bins.SetManifest")
	testutil.MustNoErr(t, bins.ConfirmManifest(db.DB, bin.ID, ""), "bins.ConfirmManifest")

	t.Run("RecordCount_updates_uop_and_actor", func(t *testing.T) {
		testutil.MustNoErr(t, bins.RecordCount(db.DB, bin.ID, 73, "operator-1"), "bins.RecordCount")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.UOPRemaining != 73 {
			t.Errorf("UOPRemaining = %d, want 73", got.UOPRemaining)
		}
		if got.LastCountedBy != "operator-1" {
			t.Errorf("LastCountedBy = %q, want %q", got.LastCountedBy, "operator-1")
		}
		if got.LastCountedAt == nil {
			t.Error("LastCountedAt = nil, want set")
		}
	})

	t.Run("UnconfirmManifest_clears_flag", func(t *testing.T) {
		// Sanity precondition.
		got, _ := bins.Get(db.DB, bin.ID)
		if !got.ManifestConfirmed {
			t.Fatal("expected ManifestConfirmed=true before bins.UnconfirmManifest")
		}
		testutil.MustNoErr(t, bins.UnconfirmManifest(db.DB, bin.ID), "bins.UnconfirmManifest")
		got2, _ := bins.Get(db.DB, bin.ID)
		if got2.ManifestConfirmed {
			t.Error("ManifestConfirmed after bins.UnconfirmManifest = true, want false")
		}
	})
}

func TestNodeTileStates(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// bins.Bin with confirmed manifest at storage node.
	loaded := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-TILE-LOADED", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, loaded), "bins.Create loaded")
	testutil.MustNoErr(t, bins.SetManifest(db.DB, loaded.ID, `{"items":[]}`, std.Payload.Code, 50), "bins.SetManifest loaded")
	testutil.MustNoErr(t, bins.ConfirmManifest(db.DB, loaded.ID, ""), "bins.ConfirmManifest loaded")

	// Empty (no manifest) bin at line node, claimed by an order.
	empty := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-TILE-EMPTY", NodeID: &std.LineNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, empty), "bins.Create empty")
	emptyOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, empty.ID, emptyOrder.ID)

	states, err := bins.NodeTileStates(db.DB)
	if err != nil {
		t.Fatalf("bins.NodeTileStates: %v", err)
	}
	storage := states[std.StorageNode.ID]
	if !storage.HasPayload {
		t.Error("storage HasPayload = false, want true")
	}
	if storage.HasEmptyBin {
		t.Error("storage HasEmptyBin = true, want false (only confirmed bin lives there)")
	}
	line := states[std.LineNode.ID]
	if !line.HasEmptyBin {
		t.Error("line HasEmptyBin = false, want true")
	}
	if !line.Claimed {
		t.Error("line Claimed = false, want true")
	}
}

func TestListAvailable(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	empty := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-AVAIL-EMPTY", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, empty), "bins.Create empty")
	loaded := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-AVAIL-LOADED", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, loaded), "bins.Create loaded")
	testutil.MustNoErr(t, bins.SetManifest(db.DB, loaded.ID, `{"items":[]}`, std.Payload.Code, 50), "bins.SetManifest")

	got, err := bins.ListAvailable(db.DB)
	if err != nil {
		t.Fatalf("bins.ListAvailable: %v", err)
	}
	// `empty` (no manifest) must show; `loaded` (has manifest + payload_code) must not.
	foundEmpty, foundLoaded := false, false
	for _, b := range got {
		if b.ID == empty.ID {
			foundEmpty = true
		}
		if b.ID == loaded.ID {
			foundLoaded = true
		}
	}
	if !foundEmpty {
		t.Error("empty bin missing from bins.ListAvailable")
	}
	if foundLoaded {
		t.Error("loaded bin should be excluded from bins.ListAvailable")
	}
}

// TestListAvailable_NULLPayloadCode regression-tests that a bin with
// payload_code=NULL (rather than ”) still appears in ListAvailable.
// Pre-fix the filter was `(b.manifest IS NULL OR b.payload_code = ”)`
// where `payload_code = ”` evaluates to NULL when payload_code is NULL
// (falsy in WHERE), so a bin with payload_code=NULL only made it through
// when manifest was also NULL. Post-fix the filter uses
// COALESCE(payload_code, ”) = ” which handles NULL unambiguously.
//
// Same bug class as the original FindEmptyCompatible NULL-safety fix
// (7c274ac / 4337344). Aligned in 2026-04-27 v2.
func TestListAvailable_NULLPayloadCode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// The schema declares payload_code NOT NULL, so we must drop the
	// constraint to inject a legacy/historical row where payload_code is
	// NULL — the COALESCE filter must still handle it correctly.
	if _, err := db.DB.Exec(`ALTER TABLE bins ALTER COLUMN payload_code DROP NOT NULL`); err != nil {
		t.Fatalf("alter bins.payload_code: %v", err)
	}

	var binID int64
	err := db.DB.QueryRow(
		`INSERT INTO bins (bin_type_id, label, description, node_id, status, payload_code, manifest) VALUES ($1, $2, '', $3, 'available', NULL, NULL) RETURNING id`,
		std.BinType.ID, "BIN-AVAIL-NULL-PC", std.StorageNode.ID,
	).Scan(&binID)
	if err != nil {
		t.Fatalf("insert NULL payload_code bin: %v", err)
	}

	got, err := bins.ListAvailable(db.DB)
	if err != nil {
		t.Fatalf("bins.ListAvailable: %v", err)
	}
	for _, b := range got {
		if b.ID == binID {
			return // pass
		}
	}
	t.Errorf("bin %d (NULL payload_code) missing from ListAvailable — NULL-safety regression", binID)
}

func TestFindEmptyCompatible(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Link payload to bin type so the JOIN matches.
	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	// Empty bin in zone A (storage), empty bin at line node (no zone).
	zoneA := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-FEC-A", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, zoneA), "bins.Create zoneA")
	other := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-FEC-B", NodeID: &std.LineNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, other), "bins.Create other")

	t.Run("prefer_zone_returns_zone_match", func(t *testing.T) {
		got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "A", 0)
		if err != nil {
			t.Fatalf("bins.FindEmptyCompatible zone A: %v", err)
		}
		if got.ID != zoneA.ID {
			t.Errorf("got bin %d, want %d (zone A match)", got.ID, zoneA.ID)
		}
	})

	t.Run("no_zone_returns_first_match", func(t *testing.T) {
		got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "", 0)
		if err != nil {
			t.Fatalf("bins.FindEmptyCompatible no zone: %v", err)
		}
		// Lowest ID wins — zoneA was inserted first.
		if got.ID != zoneA.ID {
			t.Errorf("got bin %d, want %d (lowest ID)", got.ID, zoneA.ID)
		}
	})

	t.Run("unknown_payload_advisory_fallback", func(t *testing.T) {
		// Post-2026-04-27 advisory enforcement: a payload with no rules in
		// payload_bin_types means "no restrictions configured" — any
		// compatible empty bin matches. Previously this asserted err != nil
		// (strict semantics), which was the cause of the plant starvation:
		// payloads not yet linked in payload_bin_types produced zero
		// candidate bins even when empties were available.
		got, err := bins.FindEmptyCompatible(db.DB, "DOES-NOT-EXIST", "", 0)
		if err != nil {
			t.Fatalf("expected advisory fallback to return a bin, got err: %v", err)
		}
		if got.ID != zoneA.ID {
			t.Errorf("got bin %d, want %d (lowest ID, advisory fallback)", got.ID, zoneA.ID)
		}
	})

	// Regression for SHINGO_TODO.md "Same-node retrieve" + plant test
	// 2026-04-27 (orders #434-#445): the bin already at the destination
	// must NOT be returned when the caller passes destNode.ID as
	// excludeNodeID. Pre-fix the source-finder was destination-blind, so
	// it picked the bin sitting at the order's delivery node and the
	// fleet got a same-node retrieve to cancel.
	t.Run("excludes_destination_node", func(t *testing.T) {
		// Both candidate bins exist; the zoneA bin lives at StorageNode and
		// the other at LineNode. When excludeNodeID = StorageNode.ID, the
		// finder must return the LineNode bin even though zoneA would have
		// won by zone preference and id ordering.
		got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "A", std.StorageNode.ID)
		if err != nil {
			t.Fatalf("bins.FindEmptyCompatible with exclude: %v", err)
		}
		if got.ID == zoneA.ID {
			t.Errorf("returned bin %d at excluded node %d — destination-blind regression", got.ID, std.StorageNode.ID)
		}
		if got.ID != other.ID {
			t.Errorf("returned bin %d, want %d (the non-excluded bin)", got.ID, other.ID)
		}
	})

	t.Run("exclude_zero_means_no_exclusion", func(t *testing.T) {
		// Sanity: passing 0 (the documented "no exclude" sentinel) returns
		// the same result as the original prefer_zone_returns_zone_match
		// case. Locks down the contract that 0 is not a valid node ID.
		got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "A", 0)
		if err != nil {
			t.Fatalf("bins.FindEmptyCompatible with 0 exclude: %v", err)
		}
		if got.ID != zoneA.ID {
			t.Errorf("got bin %d with excludeNodeID=0, want %d (zone A match) — 0 must mean no exclusion", got.ID, zoneA.ID)
		}
	})
}

// TestFindEmptyCompatible_PrefersAccessibleEmpty pins the 2026-06-13
// empties-are-fungible fix: when several compatible empties exist, the planner
// grabs the one that costs the least to extract — an accessible lane-mouth empty
// before a buried one — rather than the lowest bin id. Before the fix these
// queries ordered by bin id alone, so a deep empty that happened to be inserted
// first was picked and then reactively reshuffled (planning_service.go
// IsSlotAccessible → planBuriedReshuffle). Accessibility-first ordering means the
// dig-out only happens when every empty is buried (see the last-resort test).
func TestFindEmptyCompatible_PrefersAccessibleEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)
	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	// A two-deep lane: S1 at the mouth (depth 1), S2 behind it (depth 2).
	lane := &nodes.Node{Name: "LANE-FEC", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, lane), "nodes.Create lane")
	d1, d2 := 1, 2
	s1 := &nodes.Node{Name: "FEC-S1", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	s2 := &nodes.Node{Name: "FEC-S2", Enabled: true, ParentID: &lane.ID, Depth: &d2}
	testutil.MustNoErr(t, nodes.Create(db.DB, s1), "nodes.Create s1")
	testutil.MustNoErr(t, nodes.Create(db.DB, s2), "nodes.Create s2")

	// Deep empty created FIRST → lower bin id (what the old FIFO-by-id pick
	// would return). Shallow/accessible empty created SECOND → higher bin id.
	deep := &bins.Bin{BinTypeID: std.BinType.ID, Label: "FEC-DEEP", NodeID: &s2.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, deep), "bins.Create deep")
	shallow := &bins.Bin{BinTypeID: std.BinType.ID, Label: "FEC-SHALLOW", NodeID: &s1.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, shallow), "bins.Create shallow")

	got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatible: %v", err)
	}
	if got.ID != shallow.ID {
		t.Errorf("got bin %d (%q), want the accessible mouth empty %d (%q) — accessibility must beat lower bin id",
			got.ID, got.Label, shallow.ID, shallow.Label)
	}
}

// TestFindEmptyCompatible_BuriedEmptyIsLastResort confirms the fallback half of
// the fix: when the ONLY compatible empty is buried, it must still be returned
// (the planner then reshuffles to dig it out). The fix changes which empty is
// preferred, not whether a buried empty is reachable at all.
func TestFindEmptyCompatible_BuriedEmptyIsLastResort(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)
	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	lane := &nodes.Node{Name: "LANE-LR", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, lane), "nodes.Create lane")
	d1, d2 := 1, 2
	s1 := &nodes.Node{Name: "LR-S1", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	s2 := &nodes.Node{Name: "LR-S2", Enabled: true, ParentID: &lane.ID, Depth: &d2}
	testutil.MustNoErr(t, nodes.Create(db.DB, s1), "nodes.Create s1")
	testutil.MustNoErr(t, nodes.Create(db.DB, s2), "nodes.Create s2")

	// S1 (mouth) is occupied by a non-available bin — not an empty candidate, but
	// physically present, so it buries S2. The only compatible EMPTY is at S2.
	occupier := &bins.Bin{BinTypeID: std.BinType.ID, Label: "LR-OCCUPIER", NodeID: &s1.ID, Status: "staged"}
	testutil.MustNoErr(t, bins.Create(db.DB, occupier), "bins.Create occupier")
	buried := &bins.Bin{BinTypeID: std.BinType.ID, Label: "LR-BURIED", NodeID: &s2.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, buried), "bins.Create buried")

	got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatible: %v", err)
	}
	if got == nil || got.ID != buried.ID {
		t.Fatalf("got %+v, want the buried empty %d — a buried empty must stay selectable as last resort", got, buried.ID)
	}
}

// TestFindEmptyCompatible_AdvisoryFallback_NoRules verifies that a payload
// with NO rows in payload_bin_types matches any compatible empty bin.
// This is the post-2026-04-27 v2 advisory-enforcement contract: absence of
// rules in the allow-list table means "no restrictions configured."
//
// Regression for the plant starvation: pre-fix, FindEmptyCompatible used
// hard INNER JOINs to payload_bin_types/payloads which eliminated all
// candidates when no rules existed. Order #462 stuck on awaiting inventory
// despite SMN_002 / SMN_003 visibly empty in the supermarket UI.
func TestFindEmptyCompatible_AdvisoryFallback_NoRules(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Note: deliberately NOT calling db.SetPayloadBinTypes — the payload
	// has no rows in payload_bin_types. Under advisory semantics this
	// means "no restrictions" and every empty bin is eligible.

	zoneA := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-NORULES-A", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, zoneA), "bins.Create zoneA")

	got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "", 0)
	if err != nil {
		t.Fatalf("expected advisory match for unconstrained payload, got err: %v", err)
	}
	if got.ID != zoneA.ID {
		t.Errorf("got bin %d, want %d (advisory should match the only empty bin)", got.ID, zoneA.ID)
	}
}

// TestFindEmptyCompatible_RulesEnforced_ExcludesIncompatibleType verifies
// that when payload_bin_types DOES have rows for the payload, only matching
// bin types are considered. The advisory pattern preserves strict
// enforcement when rules are present.
func TestFindEmptyCompatible_RulesEnforced_ExcludesIncompatibleType(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Add a second, incompatible bin type and a bin of that type. The
	// payload's rules will only allow std.BinType (DEFAULT), so the second
	// bin must NOT be returned even if it's the only empty bin in the
	// preferred zone.
	otherType := &bins.BinType{Code: "OTHER", Description: "Incompatible type"}
	testutil.MustNoErr(t, db.CreateBinType(otherType), "CreateBinType OTHER")

	// Rule: PART-A is allowed only in DEFAULT.
	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	// Bin of OTHER type at zone A (would normally win zone preference).
	otherBin := &bins.Bin{BinTypeID: otherType.ID, Label: "BIN-OTHER-A", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, otherBin), "bins.Create otherBin")
	// Bin of DEFAULT type at line node (no zone — falls through any-zone fallback).
	defaultBin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-DEFAULT-LINE", NodeID: &std.LineNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, defaultBin), "bins.Create defaultBin")

	got, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "A", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatible: %v", err)
	}
	if got.ID == otherBin.ID {
		t.Errorf("returned bin %d (OTHER type) — rules-enforced should exclude it", got.ID)
	}
	if got.ID != defaultBin.ID {
		t.Errorf("returned bin %d, want %d (the DEFAULT-type bin)", got.ID, defaultBin.ID)
	}
}

// TestFindEmptyCompatible_RulesEnforced_NoMatchReturnsErr verifies that
// when rules exist for the payload and no bin of an allowed type is empty,
// the function returns an error (no advisory fallback to incompatible types).
func TestFindEmptyCompatible_RulesEnforced_NoMatchReturnsErr(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	otherType := &bins.BinType{Code: "OTHER", Description: "Incompatible type"}
	testutil.MustNoErr(t, db.CreateBinType(otherType), "CreateBinType OTHER")

	// Rule: PART-A is allowed only in DEFAULT.
	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	// Only an OTHER-type empty bin exists. Rules say PART-A needs DEFAULT.
	otherBin := &bins.Bin{BinTypeID: otherType.ID, Label: "BIN-OTHER-A", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, otherBin), "bins.Create otherBin")

	_, err := bins.FindEmptyCompatible(db.DB, std.Payload.Code, "", 0)
	if err == nil {
		t.Error("expected ErrNoRows when only incompatible-type bins exist with rules in place, got nil")
	}
}

func TestHasNotes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-NOTES-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")
	noNote := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-NOTES-2", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, noNote), "bins.Create noNote")

	// Insert an audit log entry for `bin`.
	if _, err := db.DB.Exec(
		`INSERT INTO audit_log (entity_type, entity_id, actor, action, new_value) VALUES ($1, $2, $3, $4, $5)`,
		"bin", bin.ID, "tester", "noted", "test note",
	); err != nil {
		t.Fatalf("insert audit_log: %v", err)
	}

	t.Run("empty_input_returns_empty_map", func(t *testing.T) {
		got, err := bins.HasNotes(db.DB, nil)
		if err != nil {
			t.Fatalf("bins.HasNotes empty: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("bins.HasNotes empty map len = %d, want 0", len(got))
		}
	})

	t.Run("flags_only_bins_with_notes", func(t *testing.T) {
		got, err := bins.HasNotes(db.DB, []int64{bin.ID, noNote.ID})
		if err != nil {
			t.Fatalf("bins.HasNotes: %v", err)
		}
		if !got[bin.ID] {
			t.Errorf("bins.HasNotes[bin]=%v, want true", got[bin.ID])
		}
		if got[noNote.ID] {
			t.Errorf("bins.HasNotes[noNote]=%v, want false", got[noNote.ID])
		}
	})
}

// ---------- bin_types.go ----------

func TestBinType_CRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	bt := &bins.BinType{Code: "TOTE-X", Description: "extra-large tote", WidthIn: 24.5, HeightIn: 18.0}
	testutil.MustNoErr(t, bins.CreateType(db.DB, bt), "bins.CreateType")
	if bt.ID == 0 {
		t.Fatal("bins.CreateType: expected ID set")
	}

	t.Run("bins.GetType", func(t *testing.T) {
		got, err := bins.GetType(db.DB, bt.ID)
		if err != nil {
			t.Fatalf("bins.GetType: %v", err)
		}
		if got.Code != "TOTE-X" {
			t.Errorf("Code = %q, want %q", got.Code, "TOTE-X")
		}
		if got.WidthIn != 24.5 || got.HeightIn != 18.0 {
			t.Errorf("dims = (%v, %v), want (24.5, 18.0)", got.WidthIn, got.HeightIn)
		}
	})

	t.Run("bins.GetTypeByCode", func(t *testing.T) {
		got, err := bins.GetTypeByCode(db.DB, "TOTE-X")
		if err != nil {
			t.Fatalf("bins.GetTypeByCode: %v", err)
		}
		if got.ID != bt.ID {
			t.Errorf("ID = %d, want %d", got.ID, bt.ID)
		}
	})

	t.Run("bins.UpdateType", func(t *testing.T) {
		bt.Description = "renamed"
		bt.WidthIn = 30
		testutil.MustNoErr(t, bins.UpdateType(db.DB, bt), "bins.UpdateType")
		got, _ := bins.GetType(db.DB, bt.ID)
		if got.Description != "renamed" {
			t.Errorf("Description = %q, want %q", got.Description, "renamed")
		}
		if got.WidthIn != 30 {
			t.Errorf("WidthIn = %v, want 30", got.WidthIn)
		}
	})

	t.Run("bins.ListTypes", func(t *testing.T) {
		// Add another for ordering check.
		other := &bins.BinType{Code: "AAAA-FIRST", Description: "alphabetically first"}
		testutil.MustNoErr(t, bins.CreateType(db.DB, other), "bins.CreateType other")
		got, err := bins.ListTypes(db.DB)
		if err != nil {
			t.Fatalf("bins.ListTypes: %v", err)
		}
		if len(got) < 2 {
			t.Fatalf("bins.ListTypes len=%d, want >=2", len(got))
		}
		if got[0].Code != "AAAA-FIRST" {
			t.Errorf("bins.ListTypes[0].Code = %q, want %q (asc by code)", got[0].Code, "AAAA-FIRST")
		}
	})

	t.Run("bins.DeleteType", func(t *testing.T) {
		testutil.MustNoErr(t, bins.DeleteType(db.DB, bt.ID), "bins.DeleteType")
		if _, err := bins.GetType(db.DB, bt.ID); err == nil {
			t.Error("bins.GetType after bins.Delete: expected error, got nil")
		}
	})
}

func TestListTypesForPayload(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	other := &bins.BinType{Code: "TOTE-OTHER", Description: "not linked"}
	testutil.MustNoErr(t, bins.CreateType(db.DB, other), "bins.CreateType other")

	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	got, err := bins.ListTypesForPayload(db.DB, std.Payload.ID)
	if err != nil {
		t.Fatalf("bins.ListTypesForPayload: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ID != std.BinType.ID {
		t.Errorf("got bin type %d, want %d (the linked one)", got[0].ID, std.BinType.ID)
	}
}

// ---------- bin_manifest.go ----------

func TestManifest_Set_Get_Confirm_Clear(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	bin := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-MAN-1", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, bin), "bins.Create")

	manifestJSON := `{"items":[{"catid":"PART-001","qty":5},{"catid":"PART-002","qty":3,"lot_code":"LOT-A"}]}`

	t.Run("SetManifest_persists_payload_and_uop", func(t *testing.T) {
		testutil.MustNoErr(t, bins.SetManifest(db.DB, bin.ID, manifestJSON, std.Payload.Code, 42), "bins.SetManifest")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.PayloadCode != std.Payload.Code {
			t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, std.Payload.Code)
		}
		if got.UOPRemaining != 42 {
			t.Errorf("UOPRemaining = %d, want 42", got.UOPRemaining)
		}
		if got.Manifest == nil {
			t.Fatal("bins.Manifest is nil, want JSON")
		}
		var gotParsed, wantParsed any
		testutil.MustNoErr(t, json.Unmarshal([]byte(*got.Manifest), &gotParsed), "unmarshal got manifest")
		testutil.MustNoErr(t, json.Unmarshal([]byte(manifestJSON), &wantParsed), "unmarshal want manifest")
		gotBytes, _ := json.Marshal(gotParsed)
		wantBytes, _ := json.Marshal(wantParsed)
		if string(gotBytes) != string(wantBytes) {
			t.Errorf("bins.Manifest = %s, want %s", gotBytes, wantBytes)
		}
		if got.ManifestConfirmed {
			t.Error("ManifestConfirmed = true, want false (Set should reset)")
		}
	})

	t.Run("GetManifest_parses_items", func(t *testing.T) {
		m, err := bins.GetManifest(db.DB, bin.ID)
		if err != nil {
			t.Fatalf("bins.GetManifest: %v", err)
		}
		if len(m.Items) != 2 {
			t.Fatalf("Items len = %d, want 2", len(m.Items))
		}
		if m.Items[0].CatID != "PART-001" || m.Items[0].Quantity != 5 {
			t.Errorf("item 0 = %+v, want PART-001 qty 5", m.Items[0])
		}
		if m.Items[1].LotCode != "LOT-A" {
			t.Errorf("item 1 LotCode = %q, want %q", m.Items[1].LotCode, "LOT-A")
		}
	})

	t.Run("ConfirmManifest_default_timestamp", func(t *testing.T) {
		testutil.MustNoErr(t, bins.ConfirmManifest(db.DB, bin.ID, ""), "bins.ConfirmManifest")
		got, _ := bins.Get(db.DB, bin.ID)
		if !got.ManifestConfirmed {
			t.Error("ManifestConfirmed = false, want true")
		}
		if got.LoadedAt == nil {
			t.Error("LoadedAt = nil, want set")
		}
	})

	t.Run("ConfirmManifest_explicit_timestamp", func(t *testing.T) {
		// Re-set so we can re-confirm with an explicit timestamp.
		testutil.MustNoErr(t, bins.SetManifest(db.DB, bin.ID, manifestJSON, std.Payload.Code, 42), "bins.SetManifest")
		ts := "2024-06-15 12:34:56"
		testutil.MustNoErr(t, bins.ConfirmManifest(db.DB, bin.ID, ts), "bins.ConfirmManifest explicit")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.LoadedAt == nil {
			t.Fatal("LoadedAt = nil, want set")
		}
		// Loose check: loaded_at should be in 2024.
		if got.LoadedAt.Year() != 2024 {
			t.Errorf("LoadedAt year = %d, want 2024", got.LoadedAt.Year())
		}
	})

	t.Run("ClearManifest_empties_bin", func(t *testing.T) {
		testutil.MustNoErr(t, bins.ClearManifest(db.DB, bin.ID), "bins.ClearManifest")
		got, _ := bins.Get(db.DB, bin.ID)
		if got.PayloadCode != "" {
			t.Errorf("PayloadCode = %q, want empty", got.PayloadCode)
		}
		if got.Manifest != nil {
			t.Errorf("bins.Manifest = %v, want nil", got.Manifest)
		}
		if got.UOPRemaining != 0 {
			t.Errorf("UOPRemaining = %d, want 0", got.UOPRemaining)
		}
		if got.ManifestConfirmed {
			t.Error("ManifestConfirmed = true, want false")
		}
		if got.LoadedAt != nil {
			t.Errorf("LoadedAt = %v, want nil", got.LoadedAt)
		}
	})

	t.Run("GetManifest_on_empty_bin_returns_empty", func(t *testing.T) {
		// `bin` was just cleared.
		m, err := bins.GetManifest(db.DB, bin.ID)
		if err != nil {
			t.Fatalf("bins.GetManifest empty: %v", err)
		}
		if m == nil {
			t.Fatal("bins.GetManifest returned nil bins.Manifest")
		}
		if len(m.Items) != 0 {
			t.Errorf("empty bins.Manifest.Items len = %d, want 0", len(m.Items))
		}
	})
}

func TestFindSourceFIFO(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Use the testdb helper which sets a confirmed manifest matching the payload.
	binOlder := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-FIFO-OLD")
	// Force loaded_at to a clearly older value so FIFO is deterministic.
	if _, err := db.DB.Exec(`UPDATE bins SET loaded_at = $1 WHERE id = $2`,
		time.Now().Add(-2*time.Hour), binOlder.ID); err != nil {
		t.Fatalf("backdate older bin: %v", err)
	}
	binNewer := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-FIFO-NEW")
	_ = binNewer

	got, err := bins.FindSourceFIFO(db.DB, std.Payload.Code, 0)
	if err != nil {
		t.Fatalf("bins.FindSourceFIFO: %v", err)
	}
	if got.ID != binOlder.ID {
		t.Errorf("bins.FindSourceFIFO returned bin %d (%s), want %d (older)", got.ID, got.Label, binOlder.ID)
	}

	t.Run("unknown_payload_errors", func(t *testing.T) {
		// FindSourceFIFO filters by exact payload_code match, so a payload
		// no bin carries returns ErrNoRows regardless of advisory state.
		// (Distinct from FindEmptyCompatible's "no rules = match anything"
		// because empty bins have no payload_code to match against.)
		if _, err := bins.FindSourceFIFO(db.DB, "MISSING-PAYLOAD", 0); err == nil {
			t.Error("expected error for unknown payload, got nil")
		}
	})
}

// TestFindSourceFIFO_AdvisoryFallback_NoRules verifies that when
// payload_bin_types has no rows for the payload, FindSourceFIFO returns
// any matching full bin regardless of bin type. This is the post-2026-04-27
// v2 advisory contract, applied symmetrically to the full-bin retrieve path.
//
// Pre-fix, FindSourceFIFO ignored payload_bin_types entirely. Post-fix it
// enforces the advisory pattern, so when rules ARE present the constraint
// takes effect — but absence of rules continues to allow any compatible
// bin (preserving the previous behavior for plants that haven't populated
// the table).
func TestFindSourceFIFO_AdvisoryFallback_NoRules(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Note: deliberately NOT calling SetPayloadBinTypes — payload has no
	// rules. Advisory fallback should match any full bin of any type.

	// Add a second bin type and load a bin of that type with the payload.
	// Without rules, FindSourceFIFO should still return this bin.
	otherType := &bins.BinType{Code: "OTHER", Description: "Other type"}
	testutil.MustNoErr(t, db.CreateBinType(otherType), "CreateBinType OTHER")
	otherBin := &bins.Bin{BinTypeID: otherType.ID, Label: "BIN-FIFO-OTHER", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, otherBin), "bins.Create otherBin")
	testutil.MustNoErr(t, db.SetBinManifest(otherBin.ID, `{"items":[]}`, std.Payload.Code, 100), "SetBinManifest")
	testutil.MustNoErr(t, db.ConfirmBinManifest(otherBin.ID, ""), "ConfirmBinManifest")

	got, err := bins.FindSourceFIFO(db.DB, std.Payload.Code, 0)
	if err != nil {
		t.Fatalf("FindSourceFIFO no-rules: %v", err)
	}
	if got.ID != otherBin.ID {
		t.Errorf("got bin %d, want %d (advisory should match the OTHER-type bin when no rules)", got.ID, otherBin.ID)
	}
}

// TestFindSourceFIFO_RulesEnforced verifies that when payload_bin_types DOES
// have rows for the payload, only matching bin types are returned. A full
// bin in a non-matching type is filtered out.
//
// This is the closing of the prior asymmetry: pre-fix, FindEmptyCompatible
// strictly enforced these rules but FindSourceFIFO ignored them entirely.
// A plant could have rules saying "PART-A goes in DEFAULT only" yet
// accidentally load PART-A into an OTHER-type bin via SetManifest (which
// has no compat check), and then FindSourceFIFO would happily retrieve it.
// Post-fix the rules-enforced path is symmetric: that incompatible bin
// becomes invisible to FindSourceFIFO.
func TestFindSourceFIFO_RulesEnforced(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Rules: PART-A is allowed only in DEFAULT.
	testutil.MustNoErr(t, db.SetPayloadBinTypes(std.Payload.ID, []int64{std.BinType.ID}), "SetPayloadBinTypes")

	// One full bin of OTHER type (forbidden by rules) — must NOT be returned.
	otherType := &bins.BinType{Code: "OTHER", Description: "Other type"}
	testutil.MustNoErr(t, db.CreateBinType(otherType), "CreateBinType OTHER")
	otherBin := &bins.Bin{BinTypeID: otherType.ID, Label: "BIN-FIFO-OTHER", NodeID: &std.StorageNode.ID, Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, otherBin), "bins.Create otherBin")
	testutil.MustNoErr(t, db.SetBinManifest(otherBin.ID, `{"items":[]}`, std.Payload.Code, 100), "SetBinManifest other")
	testutil.MustNoErr(t, db.ConfirmBinManifest(otherBin.ID, ""), "ConfirmBinManifest other")

	// One full bin of DEFAULT type (allowed by rules) — should be returned.
	defaultBin := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-FIFO-DEFAULT")

	got, err := bins.FindSourceFIFO(db.DB, std.Payload.Code, 0)
	if err != nil {
		t.Fatalf("FindSourceFIFO rules-enforced: %v", err)
	}
	if got.ID == otherBin.ID {
		t.Errorf("returned bin %d (OTHER type) — rules should exclude it", got.ID)
	}
	if got.ID != defaultBin.ID {
		t.Errorf("returned bin %d, want %d (the DEFAULT-type bin)", got.ID, defaultBin.ID)
	}
}

// FindSourceFIFO must reject empty payloadCode at the function
// boundary. After the bin-as-truth refactor, unattached bins store
// payload_code = ” instead of NULL; without this guard, a caller
// passing payloadCode = "" would silently match every unattached bin.
func TestFindSourceFIFO_RejectsEmptyPayloadCode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Seed a real loaded bin so we'd otherwise have something to match.
	_ = testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-FIFO-REAL")

	got, err := bins.FindSourceFIFO(db.DB, "", 0)
	if err == nil {
		t.Fatalf("expected error for empty payloadCode, got bin %v", got)
	}
	if got != nil {
		t.Errorf("expected nil bin for empty payloadCode, got %+v", got)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected sql.ErrNoRows for empty payloadCode (mirrors no-match sentinel), got %v", err)
	}
}

// The boundary guard alone covers the empty-input path; this covers
// the inverse — the SQL itself must not collapse ” rows into a non-
// empty payload search via accidental coercion. An unattached bin
// (payload_code = ”) must not be returned when the caller queries
// for a real payload.
func TestFindSourceFIFO_DoesNotMatchUnattachedBins(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// Real, manifest-confirmed bin for payload "X".
	realBin := testdb.CreateBinAtNode(t, db, std.Payload.Code, std.StorageNode.ID, "BIN-FIFO-REAL")

	// Unattached bin: same node, empty payload_code, manifest unconfirmed.
	// Mirrors the post-UOP storage shape (payload_code = '' on the row,
	// not NULL).
	unattached := &bins.Bin{
		BinTypeID: std.BinType.ID,
		Label:     "BIN-FIFO-UNATTACHED",
		NodeID:    &std.StorageNode.ID,
		Status:    "available",
	}
	testutil.MustNoErr(t, bins.Create(db.DB, unattached), "bins.Create unattached")
	if _, err := db.DB.Exec(`UPDATE bins SET payload_code = '' WHERE id = $1`, unattached.ID); err != nil {
		t.Fatalf("force unattached payload_code = '': %v", err)
	}

	got, err := bins.FindSourceFIFO(db.DB, std.Payload.Code, 0)
	if err != nil {
		t.Fatalf("FindSourceFIFO(real payload): %v", err)
	}
	if got.ID != realBin.ID {
		t.Errorf("got bin %d (%s), want %d (the real bin) — unattached bin must not match", got.ID, got.Label, realBin.ID)
	}
}

// ---------- node_bin_types.go ----------

func TestSetNodeTypes_And_ListTypesForNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	other := &bins.BinType{Code: "TOTE-Z", Description: "second type"}
	testutil.MustNoErr(t, bins.CreateType(db.DB, other), "bins.CreateType other")

	t.Run("bind_two_types", func(t *testing.T) {
		testutil.MustNoErr(t, bins.SetNodeTypes(db.DB, std.StorageNode.ID, []int64{std.BinType.ID, other.ID}), "bins.SetNodeTypes")
		got, err := bins.ListTypesForNode(db.DB, std.StorageNode.ID)
		if err != nil {
			t.Fatalf("bins.ListTypesForNode: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("len=%d, want 2", len(got))
		}
	})

	t.Run("replace_with_one", func(t *testing.T) {
		testutil.MustNoErr(t, bins.SetNodeTypes(db.DB, std.StorageNode.ID, []int64{other.ID}), "bins.SetNodeTypes replace")
		got, _ := bins.ListTypesForNode(db.DB, std.StorageNode.ID)
		if len(got) != 1 {
			t.Fatalf("after replace len=%d, want 1", len(got))
		}
		if got[0].ID != other.ID {
			t.Errorf("got bin type %d, want %d", got[0].ID, other.ID)
		}
	})

	t.Run("unbind_all", func(t *testing.T) {
		testutil.MustNoErr(t, bins.SetNodeTypes(db.DB, std.StorageNode.ID, nil), "bins.SetNodeTypes unbind")
		got, _ := bins.ListTypesForNode(db.DB, std.StorageNode.ID)
		if len(got) != 0 {
			t.Errorf("after unbind len=%d, want 0", len(got))
		}
	})
}

func TestListEffectiveTypesInherited(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	// bins.Create child node parented under StorageNode.
	child := &domain.Node{Name: "STORAGE-A1-CHILD", ParentID: &std.StorageNode.ID, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(child), "CreateNode child")

	t.Run("no_assignments_anywhere_returns_empty", func(t *testing.T) {
		got, err := bins.ListEffectiveTypesInherited(db.DB, child.ID)
		if err != nil {
			t.Fatalf("bins.ListEffectiveTypesInherited: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty, got %d types", len(got))
		}
	})

	t.Run("inherits_from_parent", func(t *testing.T) {
		testutil.MustNoErr(t, bins.SetNodeTypes(db.DB, std.StorageNode.ID, []int64{std.BinType.ID}), "bins.SetNodeTypes parent")
		got, err := bins.ListEffectiveTypesInherited(db.DB, child.ID)
		if err != nil {
			t.Fatalf("bins.ListEffectiveTypesInherited: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len=%d, want 1 (inherited from parent)", len(got))
		}
		if got[0].ID != std.BinType.ID {
			t.Errorf("got bin type %d, want %d", got[0].ID, std.BinType.ID)
		}
	})

	t.Run("self_assignment_overrides_parent", func(t *testing.T) {
		// Add a second type at the child level.
		otherType := &bins.BinType{Code: "TOTE-CHILD", Description: "child-only type"}
		testutil.MustNoErr(t, bins.CreateType(db.DB, otherType), "bins.CreateType")
		testutil.MustNoErr(t, bins.SetNodeTypes(db.DB, child.ID, []int64{otherType.ID}), "bins.SetNodeTypes child")
		got, err := bins.ListEffectiveTypesInherited(db.DB, child.ID)
		if err != nil {
			t.Fatalf("bins.ListEffectiveTypesInherited: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("len=%d, want 1 (own assignment beats parent)", len(got))
		}
		if got[0].ID != otherType.ID {
			t.Errorf("got bin type %d, want %d (child-level)", got[0].ID, otherType.ID)
		}
	})
}

// ---------- bins.go bins.ScanBin via row passthrough ----------

func TestScanBin_via_Get(t *testing.T) {
	t.Parallel()
	// bins.ScanBin is exercised by every bins.Get/bins.List call above; this test just pins the
	// nullable-field behavior: a bin with no node should produce NodeID=nil and
	// empty NodeName.
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	floating := &bins.Bin{BinTypeID: std.BinType.ID, Label: "BIN-NO-NODE", Status: "available"}
	testutil.MustNoErr(t, bins.Create(db.DB, floating), "bins.Create")
	got, err := bins.Get(db.DB, floating.ID)
	if err != nil {
		t.Fatalf("bins.Get: %v", err)
	}
	if got.NodeID != nil {
		t.Errorf("NodeID = %v, want nil (no node bound)", got.NodeID)
	}
	if got.NodeName != "" {
		t.Errorf("NodeName = %q, want empty", got.NodeName)
	}
	if got.ClaimedBy != nil {
		t.Errorf("ClaimedBy = %v, want nil", got.ClaimedBy)
	}
	if got.Manifest != nil {
		t.Errorf("bins.Manifest = %v, want nil", got.Manifest)
	}
}
