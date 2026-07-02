//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// newBinSvc returns a BinService wired with a fresh manifest service.
func newBinSvc(db *store.DB) *BinService {
	return NewBinService(db, NewBinManifestService(db))
}

// ensureDefaultBinType returns (and lazily creates) the DEFAULT bin type used
// by the createTestBin helper from bin_manifest_test.go.
func ensureDefaultBinType(t *testing.T, db *store.DB) *bins.BinType {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt = &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
		testutil.MustNoErr(t, db.CreateBinType(bt), "create default bin type")
	}
	return bt
}

func TestBinService_Manifest_ReturnsComposedService(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	manifest := NewBinManifestService(db)
	svc := NewBinService(db, manifest)

	if svc.Manifest() != manifest {
		t.Errorf("Manifest() returned different instance than was passed in")
	}
}

func TestBinService_Create_AtPhysicalNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := &bins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BS-CREATE-1",
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	testutil.MustNoErr(t, svc.Create(bin), "Create")
	if bin.ID == 0 {
		t.Fatal("expected bin ID to be assigned after Create")
	}

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get created bin: %v", err)
	}
	if got.Label != "BS-CREATE-1" {
		t.Errorf("Label = %q, want %q", got.Label, "BS-CREATE-1")
	}
	if got.NodeID == nil || *got.NodeID != sd.StorageNode.ID {
		t.Errorf("NodeID = %v, want %d", got.NodeID, sd.StorageNode.ID)
	}
}

func TestBinService_Create_NoNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := &bins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BS-CREATE-NIL",
		Status:    "available",
	}
	testutil.MustNoErr(t, svc.Create(bin), "Create with nil NodeID")
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if got.NodeID != nil {
		t.Errorf("NodeID = %v, want nil", got.NodeID)
	}
}

func TestBinService_Create_FailsWhenPhysicalNodeOccupied(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	// Pre-load the storage node with a bin.
	createTestBin(t, db, sd.StorageNode.ID, "BS-OCC-1", "", 0)

	bin := &bins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BS-OCC-2",
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	err := svc.Create(bin)
	if err == nil {
		t.Fatal("expected Create to fail on occupied physical node")
	}
	if !strings.Contains(err.Error(), "already has") {
		t.Errorf("error = %q, want occupancy message", err.Error())
	}
}

func TestBinService_Create_FailsWhenNodeMissing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	missing := int64(99999)
	bin := &bins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BS-MISS",
		NodeID:    &missing,
		Status:    "available",
	}
	if err := svc.Create(bin); err == nil {
		t.Fatal("expected Create to fail when node does not exist")
	}
}

func TestBinService_CreateBatch_AtSyntheticNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	syn := &nodes.Node{Name: "GRP-BATCH", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(syn), "create synthetic node")

	svc := newBinSvc(db)
	template := bins.Bin{
		BinTypeID: bt.ID,
		NodeID:    &syn.ID,
		Status:    "available",
	}
	testutil.MustNoErr(t, svc.CreateBatch(template, "BATCH-", 3), "CreateBatch")

	bins, err := db.ListBinsByNode(syn.ID)
	if err != nil {
		t.Fatalf("list bins: %v", err)
	}
	if len(bins) != 3 {
		t.Fatalf("len(bins) = %d, want 3", len(bins))
	}

	wantLabels := map[string]bool{"BATCH-0001": false, "BATCH-0002": false, "BATCH-0003": false}
	for _, b := range bins {
		if _, ok := wantLabels[b.Label]; !ok {
			t.Errorf("unexpected label %q", b.Label)
			continue
		}
		wantLabels[b.Label] = true
	}
	for label, seen := range wantLabels {
		if !seen {
			t.Errorf("missing label %q", label)
		}
	}
}

func TestBinService_CreateBatch_RejectsMultipleAtPhysicalNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	template := bins.Bin{
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	err := svc.CreateBatch(template, "BAD-", 2)
	if err == nil {
		t.Fatal("expected CreateBatch(2) at physical node to fail")
	}

	// And confirm nothing was inserted.
	existing, _ := db.CountBinsByNode(sd.StorageNode.ID)
	if existing != 0 {
		t.Errorf("CountBinsByNode = %d, want 0 (no bins should be created on rejection)", existing)
	}
}

func TestBinService_CreateBatch_SingleAtPhysicalNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	template := bins.Bin{
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	// count==1 uses the entered label verbatim: no numeric suffix.
	testutil.MustNoErr(t, svc.CreateBatch(template, "OK-1", 1), "CreateBatch(1)")

	bins, err := db.ListBinsByNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("list bins: %v", err)
	}
	if len(bins) != 1 || bins[0].Label != "OK-1" {
		t.Errorf("bins = %+v, want one bin with verbatim label OK-1", bins)
	}
}

func TestBinService_CreateBatch_DefaultsCountToOne(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	// count <= 0 should be normalized to 1.
	template := bins.Bin{
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	testutil.MustNoErr(t, svc.CreateBatch(template, "DC-", 0), "CreateBatch(0)")
	bins, _ := db.ListBinsByNode(sd.StorageNode.ID)
	if len(bins) != 1 {
		t.Errorf("len(bins) = %d, want 1 (count=0 should default to 1)", len(bins))
	}
}

// TestBinService_CreateBatch_WidthPreservingIncrement verifies the count>1
// label rule: a trailing digit run is incremented from its parsed value,
// preserving zero-pad width (CART-08 → CART-08, CART-09, CART-10).
func TestBinService_CreateBatch_WidthPreservingIncrement(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)
	syn := &nodes.Node{Name: "GRP-WIDTH", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(syn), "create synthetic node")
	svc := newBinSvc(db)

	template := bins.Bin{BinTypeID: bt.ID, NodeID: &syn.ID, Status: "available"}
	testutil.MustNoErr(t, svc.CreateBatch(template, "CART-08", 3), "CreateBatch")

	got, err := db.ListBinsByNode(syn.ID)
	if err != nil {
		t.Fatalf("list bins: %v", err)
	}
	want := map[string]bool{"CART-08": false, "CART-09": false, "CART-10": false}
	if len(got) != len(want) {
		t.Fatalf("len(bins) = %d, want %d (%+v)", len(got), len(want), got)
	}
	for _, b := range got {
		if _, ok := want[b.Label]; !ok {
			t.Errorf("unexpected label %q", b.Label)
			continue
		}
		want[b.Label] = true
	}
	for label, seen := range want {
		if !seen {
			t.Errorf("missing label %q", label)
		}
	}
}

// TestBinService_CreateBatch_CollisionCreatesNothing verifies the count==1
// collision case: re-creating an existing verbatim label is rejected and no
// duplicate is created.
func TestBinService_CreateBatch_CollisionCreatesNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)
	syn := &nodes.Node{Name: "GRP-DUP", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(syn), "create synthetic node")
	svc := newBinSvc(db)

	template := bins.Bin{BinTypeID: bt.ID, NodeID: &syn.ID, Status: "available"}
	testutil.MustNoErr(t, svc.CreateBatch(template, "DUP-1", 1), "seed bin")

	err := svc.CreateBatch(template, "DUP-1", 1)
	if err == nil {
		t.Fatal("expected collision error on duplicate label")
	}
	if !strings.Contains(err.Error(), "already exist") {
		t.Errorf("error = %q, want 'already exist'", err.Error())
	}

	got, _ := db.ListBinsByNode(syn.ID)
	dup := 0
	for _, b := range got {
		if b.Label == "DUP-1" {
			dup++
		}
	}
	if dup != 1 {
		t.Errorf("DUP-1 bins = %d, want 1 (collision must not create a duplicate)", dup)
	}
}

// TestBinService_CreateBatch_MidBatchCollisionAtomic verifies the
// all-or-nothing contract: when one label in a batch collides, none of the
// batch is created — even the labels that were free. CART-09 pre-exists, so
// CreateBatch(CART-08, 3) (→ CART-08, CART-09, CART-10) must create neither
// CART-08 nor CART-10.
func TestBinService_CreateBatch_MidBatchCollisionAtomic(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)
	syn := &nodes.Node{Name: "GRP-MIDDUP", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(syn), "create synthetic node")
	svc := newBinSvc(db)

	// Seed the middle label of the batch.
	seed := &bins.Bin{BinTypeID: bt.ID, Label: "CART-09", NodeID: &syn.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(seed), "seed CART-09")

	template := bins.Bin{BinTypeID: bt.ID, NodeID: &syn.ID, Status: "available"}
	err := svc.CreateBatch(template, "CART-08", 3)
	if err == nil {
		t.Fatal("expected mid-batch collision to fail the whole batch")
	}

	got, _ := db.ListBinsByNode(syn.ID)
	if len(got) != 1 || got[0].Label != "CART-09" {
		t.Errorf("bins = %+v, want only the pre-existing CART-09 (no partial batch)", got)
	}
}

func TestBinService_ChangeStatus(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-STAT-1", "", 0)
	testutil.MustNoErr(t, svc.ChangeStatus(bin.ID, "reserved"), "ChangeStatus")
	got, _ := db.GetBin(bin.ID)
	if got.Status != "reserved" {
		t.Errorf("Status = %q, want %q", got.Status, "reserved")
	}
}

func TestBinService_Release_ClearsStaging(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-REL-1", "", 0)

	testutil.MustNoErr(t, db.StageBin(bin.ID, nil), "StageBin")
	staged, _ := db.GetBin(bin.ID)
	if staged.StagedAt == nil {
		t.Fatal("expected staged_at to be set after StageBin")
	}

	testutil.MustNoErr(t, svc.Release(bin.ID), "Release")
	got, _ := db.GetBin(bin.ID)
	if got.StagedAt != nil {
		t.Errorf("StagedAt = %v, want nil after Release", got.StagedAt)
	}
	if got.Status != "available" {
		t.Errorf("Status = %q, want %q", got.Status, "available")
	}
}

func TestBinService_Lock_RequiresActor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LOCK-NA", "", 0)
	if err := svc.Lock(bin.ID, ""); err == nil {
		t.Fatal("expected Lock with empty actor to error")
	}

	got, _ := db.GetBin(bin.ID)
	if got.Locked {
		t.Errorf("Locked = true, want false (Lock with empty actor must not change state)")
	}
}

func TestBinService_Lock_Unlock_RoundTrip(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LOCK-1", "", 0)

	testutil.MustNoErr(t, svc.Lock(bin.ID, "tester"), "Lock")
	got, _ := db.GetBin(bin.ID)
	if !got.Locked {
		t.Error("Locked = false, want true after Lock")
	}
	if got.LockedBy != "tester" {
		t.Errorf("LockedBy = %q, want %q", got.LockedBy, "tester")
	}

	// Locking again should fail (already locked).
	if err := svc.Lock(bin.ID, "tester"); err == nil {
		t.Error("expected second Lock to fail (already locked)")
	}

	testutil.MustNoErr(t, svc.Unlock(bin.ID), "Unlock")
	got, _ = db.GetBin(bin.ID)
	if got.Locked {
		t.Error("Locked = true, want false after Unlock")
	}
	if got.LockedBy != "" {
		t.Errorf("LockedBy = %q, want empty after Unlock", got.LockedBy)
	}
}

func TestBinService_LoadPayload_RequiresPayloadCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-EMPTY", "", 0)
	if _, err := svc.LoadPayload(bin.ID, "", 0); err == nil {
		t.Fatal("expected LoadPayload with empty payload code to fail")
	}
}

func TestBinService_LoadPayload_RejectsUnknownPayload(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-UNK", "", 0)
	_, err := svc.LoadPayload(bin.ID, "DOES-NOT-EXIST", 0)
	if err == nil {
		t.Fatal("expected LoadPayload with unknown payload to fail")
	}
	if !strings.Contains(err.Error(), "DOES-NOT-EXIST") {
		t.Errorf("error = %q, want it to mention the unknown code", err.Error())
	}
}

func TestBinService_LoadPayload_AppliesTemplate(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	// Add a manifest item to the standard payload so SetBinManifestFromTemplate
	// has something concrete to write.
	item := &payloads.ManifestItem{
		PayloadID:  sd.Payload.ID,
		PartNumber: "PART-X",
		Quantity:   7,
	}
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(item), "create payload manifest item")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-OK", "", 0)
	if _, err := svc.LoadPayload(bin.ID, sd.Payload.Code, 25); err != nil {
		t.Fatalf("LoadPayload: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.PayloadCode != sd.Payload.Code {
		t.Errorf("PayloadCode = %q, want %q", got.PayloadCode, sd.Payload.Code)
	}
	if got.UOPRemaining != 25 {
		t.Errorf("UOPRemaining = %d, want 25 (override)", got.UOPRemaining)
	}
	if got.Manifest == nil || !strings.Contains(*got.Manifest, "PART-X") {
		t.Errorf("Manifest = %v, want it to contain PART-X", got.Manifest)
	}
}

func TestBinService_LoadPayload_RejectsIncompatibleBinType(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	otherBT := &bins.BinType{Code: "OTHER-BT", Description: "non-default test bin type"}
	testutil.MustNoErr(t, db.CreateBinType(otherBT), "create other bin type")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(sd.Payload.ID, []int64{otherBT.ID}), "SetPayloadBinTypes")

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-INCOMPAT", "", 0)
	_, err := svc.LoadPayload(bin.ID, sd.Payload.Code, 0)
	if err == nil {
		t.Fatal("expected LoadPayload to reject payload not compatible with bin type")
	}
	if !strings.Contains(err.Error(), "not compatible") {
		t.Errorf("error = %q, want 'not compatible'", err.Error())
	}
}

func TestBinService_LoadPayload_AllowsWhenAllowlistEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	// No SetPayloadBinTypes → empty allow-list → unrestricted (advisory semantics).
	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-EMPTYCOMPAT", "", 0)
	if _, err := svc.LoadPayload(bin.ID, sd.Payload.Code, 0); err != nil {
		t.Fatalf("LoadPayload with empty compat list should succeed: %v", err)
	}
}

func TestBinService_Move_HappyPath(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	from := &nodes.Node{Name: "MOVE-FROM", Enabled: true}
	to := &nodes.Node{Name: "MOVE-TO", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(from), "create from")
	testutil.MustNoErr(t, db.CreateNode(to), "create to")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-1", NodeID: &from.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	svc := newBinSvc(db)
	res, err := svc.Move(bin, to.ID)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if res == nil || res.DestNode == nil || res.DestNode.ID != to.ID {
		t.Errorf("DestNode = %+v, want node id %d", res, to.ID)
	}

	got, _ := db.GetBin(bin.ID)
	if got.NodeID == nil || *got.NodeID != to.ID {
		t.Errorf("NodeID = %v, want %d", got.NodeID, to.ID)
	}
}

func TestBinService_Move_RejectsZeroNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-MV-Z", "", 0)
	if _, err := svc.Move(bin, 0); err == nil {
		t.Fatal("expected Move(0) to error")
	}
}

func TestBinService_Move_RejectsSameNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-MV-SAME", "", 0)
	_, err := svc.Move(bin, sd.StorageNode.ID)
	if err == nil {
		t.Fatal("expected Move to same node to error")
	}
}

func TestBinService_Move_RejectsOccupiedPhysicalDestination(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	from := &nodes.Node{Name: "MV-OCC-FROM", Enabled: true}
	to := &nodes.Node{Name: "MV-OCC-TO", Enabled: true}
	db.CreateNode(from)
	db.CreateNode(to)

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-OCC-1", NodeID: &from.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create source bin")
	occupant := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-OCC-OCC", NodeID: &to.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(occupant), "create occupant bin")

	svc := newBinSvc(db)
	if _, err := svc.Move(bin, to.ID); err == nil {
		t.Fatal("expected Move to occupied physical node to error")
	}
	got, _ := db.GetBin(bin.ID)
	if got.NodeID == nil || *got.NodeID != from.ID {
		t.Errorf("NodeID = %v, want %d (no move on occupied dest)", got.NodeID, from.ID)
	}
}

func TestBinService_Move_ClearsStagingOnMoveToStorage(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v", err)
	}
	grp := &nodes.Node{Name: "MV-STG-NGRP", Enabled: true, IsSynthetic: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create NGRP")
	slot := &nodes.Node{Name: "MV-STG-NGRP-S1", Enabled: true, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create storage slot")

	from := &nodes.Node{Name: "MV-STG-FROM", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(from), "create lineside from")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-STG", NodeID: &from.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create staged bin")

	svc := newBinSvc(db)
	if _, err := svc.Move(bin, slot.ID); err != nil {
		t.Fatalf("Move: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "available" {
		t.Errorf("Status = %q, want available (staging cleared on move to storage slot)", got.Status)
	}
	if got.NodeID == nil || *got.NodeID != slot.ID {
		t.Errorf("NodeID = %v, want %d", got.NodeID, slot.ID)
	}
}

func TestBinService_Move_KeepsStagingOnMoveToLineside(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	// Plain nodes with no NGRP/LANE in their path are lineside, not storage.
	from := &nodes.Node{Name: "MV-KEEP-FROM", Enabled: true}
	to := &nodes.Node{Name: "MV-KEEP-TO", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(from), "create from")
	testutil.MustNoErr(t, db.CreateNode(to), "create to")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-KEEP", NodeID: &from.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create staged bin")

	svc := newBinSvc(db)
	if _, err := svc.Move(bin, to.ID); err != nil {
		t.Fatalf("Move: %v", err)
	}

	got, _ := db.GetBin(bin.ID)
	if got.Status != "staged" {
		t.Errorf("Status = %q, want staged (move to a non-storage node must not clear staging)", got.Status)
	}
}

func TestBinService_Move_RejectsMissingDestination(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-MV-MISS", "", 0)
	if _, err := svc.Move(bin, 99999); err == nil {
		t.Fatal("expected Move to missing node to error")
	}
}

func TestBinService_Move_AllowsSyntheticDestinationWithBins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	from := &nodes.Node{Name: "MV-SYN-FROM", Enabled: true}
	syn := &nodes.Node{Name: "MV-SYN-DEST", Enabled: true, IsSynthetic: true}
	db.CreateNode(from)
	db.CreateNode(syn)

	// Pre-load the synthetic destination with a bin.
	pre := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-SYN-PRE", NodeID: &syn.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(pre), "pre-load bin")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-SYN-1", NodeID: &from.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")

	svc := newBinSvc(db)
	if _, err := svc.Move(bin, syn.ID); err != nil {
		t.Fatalf("Move to synthetic destination: %v", err)
	}
	got, _ := db.GetBin(bin.ID)
	if got.NodeID == nil || *got.NodeID != syn.ID {
		t.Errorf("NodeID = %v, want %d", got.NodeID, syn.ID)
	}
}

func TestBinService_RecordCount_NoDiscrepancy(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CNT-1", "PART-A", 100)
	res, err := svc.RecordCount(bin, 100, "counter")
	if err != nil {
		t.Fatalf("RecordCount: %v", err)
	}
	if res.Expected != 100 {
		t.Errorf("Expected = %d, want 100", res.Expected)
	}
	if res.Actual != 100 {
		t.Errorf("Actual = %d, want 100", res.Actual)
	}
	if res.Discrepancy {
		t.Error("Discrepancy = true, want false (counts match)")
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 100 {
		t.Errorf("UOPRemaining = %d, want 100", got.UOPRemaining)
	}
	if got.LastCountedBy != "counter" {
		t.Errorf("LastCountedBy = %q, want %q", got.LastCountedBy, "counter")
	}
	if got.LastCountedAt == nil {
		t.Error("LastCountedAt = nil, want non-nil")
	}
}

func TestBinService_RecordCount_WithDiscrepancy(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CNT-2", "PART-A", 100)
	res, err := svc.RecordCount(bin, 88, "counter")
	if err != nil {
		t.Fatalf("RecordCount: %v", err)
	}
	if !res.Discrepancy {
		t.Error("Discrepancy = false, want true (88 vs 100)")
	}
	if res.Expected != 100 || res.Actual != 88 {
		t.Errorf("Expected/Actual = %d/%d, want 100/88", res.Expected, res.Actual)
	}

	got, _ := db.GetBin(bin.ID)
	if got.UOPRemaining != 88 {
		t.Errorf("UOPRemaining = %d, want 88", got.UOPRemaining)
	}
}

// TestBinService_RecordCount_WritesAuditRow pins the Item 19 audit-
// completeness contract: every cycle count writes a bin_uop_audit row
// (op=cycle_count) capturing the operator-vs-system divergence.
// Pre-Item-19 cycle counts were silent in bin_uop_audit and therefore
// invisible in Item 10's audit timeline UI.
func TestBinService_RecordCount_WritesAuditRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-CNT-AUDIT", "PART-A", 100)
	if _, err := svc.RecordCount(bin, 73, "counter-X"); err != nil {
		t.Fatalf("RecordCount: %v", err)
	}

	var (
		op         string
		beforeUOP  int
		afterUOP   int
		actor      string
		auditCount int
	)
	if err := db.QueryRow(`SELECT COUNT(*) FROM bin_uop_audit
		WHERE bin_id=$1 AND op='cycle_count'`, bin.ID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("cycle_count audit rows = %d, want 1", auditCount)
	}
	if err := db.QueryRow(`SELECT op, before_uop, after_uop, actor
		FROM bin_uop_audit WHERE bin_id=$1 AND op='cycle_count'`,
		bin.ID).Scan(&op, &beforeUOP, &afterUOP, &actor); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if beforeUOP != 100 {
		t.Errorf("before_uop = %d, want 100 (system's expected count)", beforeUOP)
	}
	if afterUOP != 73 {
		t.Errorf("after_uop = %d, want 73 (operator's submitted count)", afterUOP)
	}
	if actor != "counter-X" {
		t.Errorf("actor = %q, want counter-X", actor)
	}
}

func TestBinService_AddNote_RequiresMessage(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOTE-EMPTY", "", 0)
	if err := svc.AddNote(bin.ID, "general", "", "actor"); err == nil {
		t.Fatal("expected AddNote with empty message to fail")
	}
}

func TestBinService_AddNote_DefaultsType(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOTE-1", "", 0)
	testutil.MustNoErr(t, svc.AddNote(bin.ID, "", "hello world", "actor"), "AddNote")

	entries, err := db.ListEntityAudit("bin", bin.ID)
	if err != nil {
		t.Fatalf("ListEntityAudit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one audit entry")
	}
	// The most recent entry should be our note (action = "note:general").
	found := false
	for _, e := range entries {
		if e.Action == "note:general" && e.NewValue == "hello world" && e.Actor == "actor" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("did not find note:general entry with our message; got %+v", entries)
	}
}

func TestBinService_AddNote_PassesThroughExplicitType(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOTE-2", "", 0)
	testutil.MustNoErr(t, svc.AddNote(bin.ID, "discrepancy", "off by one", "qa"), "AddNote")

	entries, _ := db.ListEntityAudit("bin", bin.ID)
	found := false
	for _, e := range entries {
		if e.Action == "note:discrepancy" && e.NewValue == "off by one" && e.Actor == "qa" {
			found = true
		}
	}
	if !found {
		t.Errorf("did not find note:discrepancy entry; got %+v", entries)
	}
}

func TestBinService_Update_AppliesPartialChanges(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-UPD-1", "", 0)

	// Create a second bin type so we can swap.
	bt2 := &bins.BinType{Code: "DEFAULT2", Description: "Second type"}
	testutil.MustNoErr(t, db.CreateBinType(bt2), "create second bin type")

	newLabel := "BS-UPD-1-RENAMED"
	newDesc := "renamed"
	testutil.MustNoErr(t, svc.Update(bin, &newLabel, &newDesc, &bt2.ID), "Update")

	got, _ := db.GetBin(bin.ID)
	if got.Label != "BS-UPD-1-RENAMED" {
		t.Errorf("Label = %q, want %q", got.Label, "BS-UPD-1-RENAMED")
	}
	if got.Description != "renamed" {
		t.Errorf("Description = %q, want %q", got.Description, "renamed")
	}
	if got.BinTypeID != bt2.ID {
		t.Errorf("BinTypeID = %d, want %d", got.BinTypeID, bt2.ID)
	}
}

func TestBinService_Update_NilPointersLeaveFieldsAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-UPD-NIL", "", 0)
	originalLabel := bin.Label
	originalBT := bin.BinTypeID

	testutil.MustNoErr(t, svc.Update(bin, nil, nil, nil), "Update with all nils")

	got, _ := db.GetBin(bin.ID)
	if got.Label != originalLabel {
		t.Errorf("Label changed from %q to %q (nil pointer should leave it alone)", originalLabel, got.Label)
	}
	if got.BinTypeID != originalBT {
		t.Errorf("BinTypeID changed from %d to %d (nil pointer should leave it alone)", originalBT, got.BinTypeID)
	}
}

// --- PR 3a.2 absorbed methods --------------------------------------------

func TestBinService_GetBin_ReturnsCreated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-GET-1", "", 0)
	got, err := svc.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("GetBin: %v", err)
	}
	if got == nil || got.ID != bin.ID {
		t.Fatalf("GetBin returned %+v, want id %d", got, bin.ID)
	}
	if got.Label != "BS-GET-1" {
		t.Errorf("Label = %q, want %q", got.Label, "BS-GET-1")
	}
}

func TestBinService_GetBin_MissingErrors(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newBinSvc(db)
	if _, err := svc.GetBin(99999); err == nil {
		t.Fatal("expected GetBin on missing id to error")
	}
}

func TestBinService_ListBins_IncludesCreated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LIST-1", "", 0)
	bins, err := svc.ListBins()
	if err != nil {
		t.Fatalf("ListBins: %v", err)
	}
	found := false
	for _, b := range bins {
		if b.ID == bin.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListBins did not include id %d (got %d bins)", bin.ID, len(bins))
	}
}

func TestBinService_Delete_RemovesBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-DEL-1", "", 0)
	testutil.MustNoErr(t, svc.Delete(bin.ID), "Delete")
	if _, err := db.GetBin(bin.ID); err == nil {
		t.Fatal("expected GetBin to error after Delete")
	}
}

// TestBinService_Retire_MarksRetiredAndClearsNode pins Item B's NULL
// approach: Retire flips the status and vacates node_id atomically so
// the carrier disappears from operational readers (CountByAllNodes,
// ListByNode) without losing history.
func TestBinService_Retire_MarksRetiredAndClearsNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-RET-1", "", 0)
	if bin.NodeID == nil {
		t.Fatalf("precondition: created bin should have NodeID set")
	}
	testutil.MustNoErr(t, svc.Retire(bin.ID), "Retire")

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("GetBin after Retire: %v — row should still exist (history preserved)", err)
	}
	if got.Status != domain.BinStatusRetired {
		t.Errorf("after Retire status = %q, want %q", got.Status, domain.BinStatusRetired)
	}
	if got.NodeID != nil {
		t.Errorf("after Retire NodeID = %v, want nil (carrier must vacate production)", *got.NodeID)
	}

	count, err := db.CountBinsByNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("CountBinsByNode: %v", err)
	}
	if count != 0 {
		t.Errorf("retired bin must not count against the old node; CountBinsByNode = %d, want 0", count)
	}

	// Idempotent: second call should not error.
	testutil.MustNoErr(t, svc.Retire(bin.ID), "Retire (idempotent)")
}

// TestBinService_Retire_ClaimedBin pins Retire's behavior on a bin
// claimed by an active order. The carrier still retires — claim state
// is informational on the bin row; the order is the source of truth
// for in-flight semantics. Pre-Round-3 the admin Delete path FK-failed
// here because the order row referenced the bin.
func TestBinService_Retire_ClaimedBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-RET-CLAIMED", "", 0)
	claimer := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, bin.ID, claimer.ID)

	testutil.MustNoErr(t, svc.Retire(bin.ID), "Retire claimed bin")

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("GetBin after Retire: %v", err)
	}
	if got.Status != domain.BinStatusRetired {
		t.Errorf("status = %q, want %q", got.Status, domain.BinStatusRetired)
	}
	if got.NodeID != nil {
		t.Errorf("NodeID = %v, want nil", *got.NodeID)
	}
}

// TestBinService_Retire_LockedBin pins Retire's behavior on a locked
// bin. Lock state is operator-asserted "do not auto-move" — it should
// not block an explicit admin Retire action.
func TestBinService_Retire_LockedBin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-RET-LOCKED", "", 0)
	testutil.MustNoErr(t, svc.Lock(bin.ID, "admin"), "Lock")

	testutil.MustNoErr(t, svc.Retire(bin.ID), "Retire locked bin")

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("GetBin after Retire: %v", err)
	}
	if got.Status != domain.BinStatusRetired {
		t.Errorf("status = %q, want %q", got.Status, domain.BinStatusRetired)
	}
}

func TestBinService_HasNotes_ReportsBoth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	withNote := createTestBin(t, db, sd.StorageNode.ID, "BS-HN-1", "", 0)
	withoutNote := createTestBin(t, db, sd.StorageNode.ID, "BS-HN-2", "", 0)
	testutil.MustNoErr(t, db.AddBinNote(withNote.ID, "general", "hello", "tester"), "seed note")

	got, err := svc.HasNotes([]int64{withNote.ID, withoutNote.ID})
	if err != nil {
		t.Fatalf("HasNotes: %v", err)
	}
	if !got[withNote.ID] {
		t.Errorf("HasNotes[%d] = false, want true", withNote.ID)
	}
	if got[withoutNote.ID] {
		t.Errorf("HasNotes[%d] = true, want false", withoutNote.ID)
	}
}

func TestBinService_CreateBinType_Persists(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-CR", Description: "create test"}
	testutil.MustNoErr(t, svc.CreateBinType(bt), "CreateBinType")
	if bt.ID == 0 {
		t.Fatal("expected bin type ID assignment")
	}
	got, err := db.GetBinType(bt.ID)
	if err != nil {
		t.Fatalf("GetBinType: %v", err)
	}
	if got.Code != "BS-BT-CR" || got.Description != "create test" {
		t.Errorf("readback = %+v, want code=BS-BT-CR description=create test", got)
	}
}

func TestBinService_GetBinType_RoundTrip(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-GET", Description: "get test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "seed bin type")
	got, err := svc.GetBinType(bt.ID)
	if err != nil {
		t.Fatalf("GetBinType: %v", err)
	}
	if got.Code != "BS-BT-GET" {
		t.Errorf("Code = %q, want %q", got.Code, "BS-BT-GET")
	}
}

func TestBinService_UpdateBinType_Persists(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-UP", Description: "before"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "seed bin type")
	bt.Description = "after"
	testutil.MustNoErr(t, svc.UpdateBinType(bt), "UpdateBinType")
	got, _ := db.GetBinType(bt.ID)
	if got.Description != "after" {
		t.Errorf("Description = %q, want %q", got.Description, "after")
	}
}

func TestBinService_DeleteBinType_Removes(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-DEL", Description: "delete test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "seed bin type")
	testutil.MustNoErr(t, svc.DeleteBinType(bt.ID), "DeleteBinType")
	if _, err := db.GetBinType(bt.ID); err == nil {
		t.Fatal("expected GetBinType to error after DeleteBinType")
	}
}

func TestBinService_ListBinTypes_IncludesCreated(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-LIST", Description: "list test"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "seed bin type")
	types, err := svc.ListBinTypes()
	if err != nil {
		t.Fatalf("ListBinTypes: %v", err)
	}
	found := false
	for _, b := range types {
		if b.ID == bt.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ListBinTypes did not include id %d (got %d types)", bt.ID, len(types))
	}
}

// ── PR 3a.5.1 additions: tests for methods absorbed from engine_db_methods.go ──

func TestBinService_CountBinsByAllNodes_GroupsByNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	// Two bins at the storage node, one at the line node.
	mk := func(label string, nodeID int64) {
		b := &bins.Bin{
			BinTypeID: sd.BinType.ID,
			Label:     label,
			NodeID:    &nodeID,
			Status:    "available",
		}
		if err := db.CreateBin(b); err != nil {
			t.Fatalf("CreateBin %s: %v", label, err)
		}
	}
	// Storage node is a physical node (one-bin-per-node rule), so the
	// "two bins at storage" case has to use a synthetic parent. Seed a
	// synthetic NGRP node and put two bins there, plus one at the line
	// node.
	grp := &nodes.Node{Name: "CBBAN-GRP", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(grp), "create synthetic grp")
	mk("CBBAN-1", grp.ID)
	mk("CBBAN-2", grp.ID)
	mk("CBBAN-3", sd.LineNode.ID)

	counts, err := svc.CountBinsByAllNodes()
	if err != nil {
		t.Fatalf("CountBinsByAllNodes: %v", err)
	}
	if counts[grp.ID] != 2 {
		t.Errorf("counts[%d] = %d, want 2", grp.ID, counts[grp.ID])
	}
	if counts[sd.LineNode.ID] != 1 {
		t.Errorf("counts[%d] = %d, want 1", sd.LineNode.ID, counts[sd.LineNode.ID])
	}
}
