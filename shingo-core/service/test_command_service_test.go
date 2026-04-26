//go:build docker

package service

import (
	"testing"

	"shingocore/store/diagnostics"
)

func makeTestCommand(t *testing.T, svc *TestCommandService, cmdType, robotID string) *diagnostics.TestCommand {
	t.Helper()
	tc := &diagnostics.TestCommand{
		CommandType: cmdType,
		RobotID:     robotID,
		VendorState: "queued",
	}
	if err := svc.Create(tc); err != nil {
		t.Fatalf("Create test command: %v", err)
	}
	if tc.ID == 0 {
		t.Fatal("expected Create to populate ID")
	}
	return tc
}

func TestTestCommandService_Create_PersistsRow(t *testing.T) {
	db := testDB(t)
	svc := NewTestCommandService(db)

	tc := &diagnostics.TestCommand{
		CommandType:   "move",
		RobotID:       "R1",
		VendorOrderID: "vo-1",
		VendorState:   "queued",
		Location:      "STN-A",
	}
	if err := svc.Create(tc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tc.ID == 0 {
		t.Fatal("ID not populated")
	}

	got, err := db.GetTestCommand(tc.ID)
	if err != nil {
		t.Fatalf("GetTestCommand: %v", err)
	}
	if got.CommandType != "move" || got.RobotID != "R1" || got.Location != "STN-A" {
		t.Errorf("row = %+v, want move/R1/STN-A", got)
	}
}

func TestTestCommandService_Get_ReturnsRow(t *testing.T) {
	db := testDB(t)
	svc := NewTestCommandService(db)
	seed := makeTestCommand(t, svc, "pick", "R2")

	got, err := svc.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != seed.ID || got.CommandType != "pick" {
		t.Errorf("Get = %+v, want ID=%d type=pick", got, seed.ID)
	}
}

func TestTestCommandService_UpdateStatus_PersistsChanges(t *testing.T) {
	db := testDB(t)
	svc := NewTestCommandService(db)
	seed := makeTestCommand(t, svc, "place", "R3")

	if err := svc.UpdateStatus(seed.ID, "running", "started OK"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, _ := db.GetTestCommand(seed.ID)
	if got.VendorState != "running" || got.Detail != "started OK" {
		t.Errorf("row = %+v, want running/started OK", got)
	}
	if got.CompletedAt != nil {
		t.Errorf("CompletedAt = %v, want nil after UpdateStatus", got.CompletedAt)
	}
}

func TestTestCommandService_Complete_SetsCompletedAt(t *testing.T) {
	db := testDB(t)
	svc := NewTestCommandService(db)
	seed := makeTestCommand(t, svc, "charge", "R4")

	if err := svc.Complete(seed.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, _ := db.GetTestCommand(seed.ID)
	if got.CompletedAt == nil {
		t.Error("CompletedAt = nil, want non-nil after Complete")
	}
}

func TestTestCommandService_List_LimitsRows(t *testing.T) {
	db := testDB(t)
	svc := NewTestCommandService(db)

	for i := 0; i < 5; i++ {
		makeTestCommand(t, svc, "type", "R-many")
	}

	// Limit 3 — only 3 rows.
	rows, err := svc.List(3)
	if err != nil {
		t.Fatalf("List(3): %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("len(rows) = %d, want 3", len(rows))
	}

	// Limit 10 — all 5 rows.
	rows, err = svc.List(10)
	if err != nil {
		t.Fatalf("List(10): %v", err)
	}
	if len(rows) != 5 {
		t.Errorf("len(rows) = %d, want 5", len(rows))
	}

	// Sanity: direct DB call returns the same count.
	dbRows, _ := db.ListTestCommands(10)
	if len(dbRows) != len(rows) {
		t.Errorf("db rows = %d, svc rows = %d, should match", len(dbRows), len(rows))
	}
}
