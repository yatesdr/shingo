//go:build docker

package diagnostics_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/diagnostics"
)

func TestCoverage_TestCommandCRUD(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	tc := &diagnostics.TestCommand{CommandType: "move", RobotID: "AMB-1", VendorOrderID: "rds-1", VendorState: "CREATED", Location: "LINE-1", ConfigID: "cfg-a", Detail: "initial"}
	if err := diagnostics.Create(db.DB, tc); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tc.ID == 0 {
		t.Fatal("ID not assigned")
	}
	got, err := diagnostics.Get(db.DB, tc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CommandType != "move" {
		t.Errorf("CommandType = %q", got.CommandType)
	}
	if got.RobotID != "AMB-1" {
		t.Errorf("RobotID = %q", got.RobotID)
	}
	if got.VendorOrderID != "rds-1" {
		t.Errorf("VendorOrderID = %q", got.VendorOrderID)
	}
	if got.VendorState != "CREATED" {
		t.Errorf("VendorState = %q", got.VendorState)
	}
	if got.Location != "LINE-1" {
		t.Errorf("Location = %q", got.Location)
	}
	if got.ConfigID != "cfg-a" {
		t.Errorf("ConfigID = %q", got.ConfigID)
	}
	if got.Detail != "initial" {
		t.Errorf("Detail = %q", got.Detail)
	}
	if got.CompletedAt != nil {
		t.Error("CompletedAt should be nil")
	}
	if err := diagnostics.UpdateStatus(db.DB, tc.ID, "RUNNING", "robot accepted"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	afterU, _ := diagnostics.Get(db.DB, tc.ID)
	if afterU.VendorState != "RUNNING" {
		t.Errorf("VendorState after update = %q, want RUNNING", afterU.VendorState)
	}
	if afterU.Detail != "robot accepted" {
		t.Errorf("Detail after update = %q", afterU.Detail)
	}
	if err := diagnostics.Complete(db.DB, tc.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	afterC, _ := diagnostics.Get(db.DB, tc.ID)
	if afterC.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestCoverage_ListTestCommands(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cmds := []*diagnostics.TestCommand{
		{CommandType: "move", RobotID: "AMB-1"},
		{CommandType: "stop", RobotID: "AMB-1"},
		{CommandType: "move", RobotID: "AMB-2"},
	}
	for _, c := range cmds {
		diagnostics.Create(db.DB, c)
	}
	diagnostics.Complete(db.DB, cmds[1].ID)
	all, err := diagnostics.List(db.DB, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all len = %d, want 3", len(all))
	}
	if all[0].ID != cmds[2].ID {
		t.Errorf("all[0].ID = %d, want %d (DESC)", all[0].ID, cmds[2].ID)
	}
	two, err := diagnostics.List(db.DB, 2)
	if err != nil {
		t.Fatalf("List(2): %v", err)
	}
	if len(two) != 2 {
		t.Errorf("limited len = %d, want 2", len(two))
	}
	active, err := diagnostics.ListActive(db.DB)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active len = %d, want 2", len(active))
	}
	for _, c := range active {
		if c.CompletedAt != nil {
			t.Errorf("active row %d has CompletedAt set", c.ID)
		}
		if c.ID == cmds[1].ID {
			t.Errorf("completed command %d should not appear in active list", c.ID)
		}
	}
}
