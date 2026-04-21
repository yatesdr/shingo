//go:build docker

package store

import "testing"

func TestTestCommandCRUD(t *testing.T) {
	db := testDB(t)

	tc := &TestCommand{
		CommandType:   "move",
		RobotID:       "AMB-1",
		VendorOrderID: "rds-1",
		VendorState:   "CREATED",
		Location:      "LINE-1",
		ConfigID:      "cfg-a",
		Detail:        "initial",
	}
	if err := db.CreateTestCommand(tc); err != nil {
		t.Fatalf("CreateTestCommand: %v", err)
	}
	if tc.ID == 0 {
		t.Fatal("ID not assigned")
	}

	// Get — read back, verify every column persisted
	got, err := db.GetTestCommand(tc.ID)
	if err != nil {
		t.Fatalf("GetTestCommand: %v", err)
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
		t.Error("CompletedAt should be nil on fresh row")
	}

	// UpdateTestCommandStatus
	if err := db.UpdateTestCommandStatus(tc.ID, "RUNNING", "robot accepted"); err != nil {
		t.Fatalf("UpdateTestCommandStatus: %v", err)
	}
	afterU, _ := db.GetTestCommand(tc.ID)
	if afterU.VendorState != "RUNNING" {
		t.Errorf("VendorState after update = %q, want RUNNING", afterU.VendorState)
	}
	if afterU.Detail != "robot accepted" {
		t.Errorf("Detail after update = %q", afterU.Detail)
	}

	// CompleteTestCommand
	if err := db.CompleteTestCommand(tc.ID); err != nil {
		t.Fatalf("CompleteTestCommand: %v", err)
	}
	afterC, _ := db.GetTestCommand(tc.ID)
	if afterC.CompletedAt == nil {
		t.Error("CompletedAt should be set after complete")
	}
}

func TestListTestCommands(t *testing.T) {
	db := testDB(t)

	cmds := []*TestCommand{
		{CommandType: "move", RobotID: "AMB-1"},
		{CommandType: "stop", RobotID: "AMB-1"},
		{CommandType: "move", RobotID: "AMB-2"},
	}
	for _, c := range cmds {
		db.CreateTestCommand(c)
	}
	// Complete one
	db.CompleteTestCommand(cmds[1].ID)

	// List all
	all, err := db.ListTestCommands(10)
	if err != nil {
		t.Fatalf("ListTestCommands: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all len = %d, want 3", len(all))
	}
	// DESC order by id — cmds[2] should be first.
	if all[0].ID != cmds[2].ID {
		t.Errorf("all[0].ID = %d, want %d (DESC order)", all[0].ID, cmds[2].ID)
	}

	// Limit honored
	two, err := db.ListTestCommands(2)
	if err != nil {
		t.Fatalf("ListTestCommands(2): %v", err)
	}
	if len(two) != 2 {
		t.Errorf("limited len = %d, want 2", len(two))
	}

	// ListActiveTestCommands — excludes the completed one.
	active, err := db.ListActiveTestCommands()
	if err != nil {
		t.Fatalf("ListActiveTestCommands: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active len = %d, want 2", len(active))
	}
	for _, c := range active {
		if c.CompletedAt != nil {
			t.Errorf("active row %d has CompletedAt set, should be excluded", c.ID)
		}
		if c.ID == cmds[1].ID {
			t.Errorf("completed command %d should not appear in active list", c.ID)
		}
	}
}
