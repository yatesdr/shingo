//go:build docker

package service

import (
	"strings"
	"testing"

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
		if err := db.CreateBinType(bt); err != nil {
			t.Fatalf("create default bin type: %v", err)
		}
	}
	return bt
}

func TestBinService_Manifest_ReturnsComposedService(t *testing.T) {
	db := testDB(t)
	manifest := NewBinManifestService(db)
	svc := NewBinService(db, manifest)

	if svc.Manifest() != manifest {
		t.Errorf("Manifest() returned different instance than was passed in")
	}
}

func TestBinService_Create_AtPhysicalNode(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := &bins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BS-CREATE-1",
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	if err := svc.Create(bin); err != nil {
		t.Fatalf("Create: %v", err)
	}
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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := &bins.Bin{
		BinTypeID: sd.BinType.ID,
		Label:     "BS-CREATE-NIL",
		Status:    "available",
	}
	if err := svc.Create(bin); err != nil {
		t.Fatalf("Create with nil NodeID: %v", err)
	}
	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}
	if got.NodeID != nil {
		t.Errorf("NodeID = %v, want nil", got.NodeID)
	}
}

func TestBinService_Create_FailsWhenPhysicalNodeOccupied(t *testing.T) {
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
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	syn := &nodes.Node{Name: "GRP-BATCH", Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(syn); err != nil {
		t.Fatalf("create synthetic node: %v", err)
	}

	svc := newBinSvc(db)
	template := bins.Bin{
		BinTypeID: bt.ID,
		NodeID:    &syn.ID,
		Status:    "available",
	}
	if err := svc.CreateBatch(template, "BATCH-", 3); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}

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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	template := bins.Bin{
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	if err := svc.CreateBatch(template, "OK-", 1); err != nil {
		t.Fatalf("CreateBatch(1): %v", err)
	}

	bins, err := db.ListBinsByNode(sd.StorageNode.ID)
	if err != nil {
		t.Fatalf("list bins: %v", err)
	}
	if len(bins) != 1 || bins[0].Label != "OK-0001" {
		t.Errorf("bins = %+v, want one bin with label OK-0001", bins)
	}
}

func TestBinService_CreateBatch_DefaultsCountToOne(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	// count <= 0 should be normalized to 1.
	template := bins.Bin{
		BinTypeID: sd.BinType.ID,
		NodeID:    &sd.StorageNode.ID,
		Status:    "available",
	}
	if err := svc.CreateBatch(template, "DC-", 0); err != nil {
		t.Fatalf("CreateBatch(0): %v", err)
	}
	bins, _ := db.ListBinsByNode(sd.StorageNode.ID)
	if len(bins) != 1 {
		t.Errorf("len(bins) = %d, want 1 (count=0 should default to 1)", len(bins))
	}
}

func TestBinService_ChangeStatus(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-STAT-1", "", 0)
	if err := svc.ChangeStatus(bin.ID, "reserved"); err != nil {
		t.Fatalf("ChangeStatus: %v", err)
	}
	got, _ := db.GetBin(bin.ID)
	if got.Status != "reserved" {
		t.Errorf("Status = %q, want %q", got.Status, "reserved")
	}
}

func TestBinService_Release_ClearsStaging(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-REL-1", "", 0)

	if err := db.StageBin(bin.ID, nil); err != nil {
		t.Fatalf("StageBin: %v", err)
	}
	staged, _ := db.GetBin(bin.ID)
	if staged.StagedAt == nil {
		t.Fatal("expected staged_at to be set after StageBin")
	}

	if err := svc.Release(bin.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, _ := db.GetBin(bin.ID)
	if got.StagedAt != nil {
		t.Errorf("StagedAt = %v, want nil after Release", got.StagedAt)
	}
	if got.Status != "available" {
		t.Errorf("Status = %q, want %q", got.Status, "available")
	}
}

func TestBinService_Lock_RequiresActor(t *testing.T) {
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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LOCK-1", "", 0)

	if err := svc.Lock(bin.ID, "tester"); err != nil {
		t.Fatalf("Lock: %v", err)
	}
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

	if err := svc.Unlock(bin.ID); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	got, _ = db.GetBin(bin.ID)
	if got.Locked {
		t.Error("Locked = true, want false after Unlock")
	}
	if got.LockedBy != "" {
		t.Errorf("LockedBy = %q, want empty after Unlock", got.LockedBy)
	}
}

func TestBinService_LoadPayload_RequiresPayloadCode(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-EMPTY", "", 0)
	if err := svc.LoadPayload(bin.ID, "", 0); err == nil {
		t.Fatal("expected LoadPayload with empty payload code to fail")
	}
}

func TestBinService_LoadPayload_RejectsUnknownPayload(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-UNK", "", 0)
	err := svc.LoadPayload(bin.ID, "DOES-NOT-EXIST", 0)
	if err == nil {
		t.Fatal("expected LoadPayload with unknown payload to fail")
	}
	if !strings.Contains(err.Error(), "DOES-NOT-EXIST") {
		t.Errorf("error = %q, want it to mention the unknown code", err.Error())
	}
}

func TestBinService_LoadPayload_AppliesTemplate(t *testing.T) {
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
	if err := db.CreatePayloadManifestItem(item); err != nil {
		t.Fatalf("create payload manifest item: %v", err)
	}

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-LP-OK", "", 0)
	if err := svc.LoadPayload(bin.ID, sd.Payload.Code, 25); err != nil {
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

func TestBinService_Move_HappyPath(t *testing.T) {
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	from := &nodes.Node{Name: "MOVE-FROM", Enabled: true}
	to := &nodes.Node{Name: "MOVE-TO", Enabled: true}
	if err := db.CreateNode(from); err != nil {
		t.Fatalf("create from: %v", err)
	}
	if err := db.CreateNode(to); err != nil {
		t.Fatalf("create to: %v", err)
	}

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-1", NodeID: &from.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}

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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-MV-Z", "", 0)
	if _, err := svc.Move(bin, 0); err == nil {
		t.Fatal("expected Move(0) to error")
	}
}

func TestBinService_Move_RejectsSameNode(t *testing.T) {
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
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	from := &nodes.Node{Name: "MV-OCC-FROM", Enabled: true}
	to := &nodes.Node{Name: "MV-OCC-TO", Enabled: true}
	db.CreateNode(from)
	db.CreateNode(to)

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-OCC-1", NodeID: &from.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create source bin: %v", err)
	}
	occupant := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-OCC-OCC", NodeID: &to.ID, Status: "available"}
	if err := db.CreateBin(occupant); err != nil {
		t.Fatalf("create occupant bin: %v", err)
	}

	svc := newBinSvc(db)
	if _, err := svc.Move(bin, to.ID); err == nil {
		t.Fatal("expected Move to occupied physical node to error")
	}
	got, _ := db.GetBin(bin.ID)
	if got.NodeID == nil || *got.NodeID != from.ID {
		t.Errorf("NodeID = %v, want %d (no move on occupied dest)", got.NodeID, from.ID)
	}
}

func TestBinService_Move_RejectsMissingDestination(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-MV-MISS", "", 0)
	if _, err := svc.Move(bin, 99999); err == nil {
		t.Fatal("expected Move to missing node to error")
	}
}

func TestBinService_Move_AllowsSyntheticDestinationWithBins(t *testing.T) {
	db := testDB(t)
	bt := ensureDefaultBinType(t, db)

	from := &nodes.Node{Name: "MV-SYN-FROM", Enabled: true}
	syn := &nodes.Node{Name: "MV-SYN-DEST", Enabled: true, IsSynthetic: true}
	db.CreateNode(from)
	db.CreateNode(syn)

	// Pre-load the synthetic destination with a bin.
	pre := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-SYN-PRE", NodeID: &syn.ID, Status: "available"}
	if err := db.CreateBin(pre); err != nil {
		t.Fatalf("pre-load bin: %v", err)
	}

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BS-MV-SYN-1", NodeID: &from.ID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin: %v", err)
	}

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

func TestBinService_AddNote_RequiresMessage(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOTE-EMPTY", "", 0)
	if err := svc.AddNote(bin.ID, "general", "", "actor"); err == nil {
		t.Fatal("expected AddNote with empty message to fail")
	}
}

func TestBinService_AddNote_DefaultsType(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOTE-1", "", 0)
	if err := svc.AddNote(bin.ID, "", "hello world", "actor"); err != nil {
		t.Fatalf("AddNote: %v", err)
	}

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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-NOTE-2", "", 0)
	if err := svc.AddNote(bin.ID, "discrepancy", "off by one", "qa"); err != nil {
		t.Fatalf("AddNote: %v", err)
	}

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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-UPD-1", "", 0)

	// Create a second bin type so we can swap.
	bt2 := &bins.BinType{Code: "DEFAULT2", Description: "Second type"}
	if err := db.CreateBinType(bt2); err != nil {
		t.Fatalf("create second bin type: %v", err)
	}

	newLabel := "BS-UPD-1-RENAMED"
	newDesc := "renamed"
	if err := svc.Update(bin, &newLabel, &newDesc, &bt2.ID); err != nil {
		t.Fatalf("Update: %v", err)
	}

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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-UPD-NIL", "", 0)
	originalLabel := bin.Label
	originalBT := bin.BinTypeID

	if err := svc.Update(bin, nil, nil, nil); err != nil {
		t.Fatalf("Update with all nils: %v", err)
	}

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
	db := testDB(t)
	svc := newBinSvc(db)
	if _, err := svc.GetBin(99999); err == nil {
		t.Fatal("expected GetBin on missing id to error")
	}
}

func TestBinService_ListBins_IncludesCreated(t *testing.T) {
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
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	bin := createTestBin(t, db, sd.StorageNode.ID, "BS-DEL-1", "", 0)
	if err := svc.Delete(bin.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := db.GetBin(bin.ID); err == nil {
		t.Fatal("expected GetBin to error after Delete")
	}
}

func TestBinService_HasNotes_ReportsBoth(t *testing.T) {
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	svc := newBinSvc(db)

	withNote := createTestBin(t, db, sd.StorageNode.ID, "BS-HN-1", "", 0)
	withoutNote := createTestBin(t, db, sd.StorageNode.ID, "BS-HN-2", "", 0)
	if err := db.AddBinNote(withNote.ID, "general", "hello", "tester"); err != nil {
		t.Fatalf("seed note: %v", err)
	}

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
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-CR", Description: "create test"}
	if err := svc.CreateBinType(bt); err != nil {
		t.Fatalf("CreateBinType: %v", err)
	}
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
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-GET", Description: "get test"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("seed bin type: %v", err)
	}
	got, err := svc.GetBinType(bt.ID)
	if err != nil {
		t.Fatalf("GetBinType: %v", err)
	}
	if got.Code != "BS-BT-GET" {
		t.Errorf("Code = %q, want %q", got.Code, "BS-BT-GET")
	}
}

func TestBinService_UpdateBinType_Persists(t *testing.T) {
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-UP", Description: "before"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("seed bin type: %v", err)
	}
	bt.Description = "after"
	if err := svc.UpdateBinType(bt); err != nil {
		t.Fatalf("UpdateBinType: %v", err)
	}
	got, _ := db.GetBinType(bt.ID)
	if got.Description != "after" {
		t.Errorf("Description = %q, want %q", got.Description, "after")
	}
}

func TestBinService_DeleteBinType_Removes(t *testing.T) {
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-DEL", Description: "delete test"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("seed bin type: %v", err)
	}
	if err := svc.DeleteBinType(bt.ID); err != nil {
		t.Fatalf("DeleteBinType: %v", err)
	}
	if _, err := db.GetBinType(bt.ID); err == nil {
		t.Fatal("expected GetBinType to error after DeleteBinType")
	}
}

func TestBinService_ListBinTypes_IncludesCreated(t *testing.T) {
	db := testDB(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "BS-BT-LIST", Description: "list test"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("seed bin type: %v", err)
	}
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
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create synthetic grp: %v", err)
	}
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
