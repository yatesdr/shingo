package store

import "testing"

func TestCorrectionCRUD(t *testing.T) {
	db := testDB(t)

	node := &Node{Name: "S1", NodeType: "storage", Enabled: true}
	db.CreateNode(node)

	c := &Correction{
		CorrectionType: "add",
		NodeID:         node.ID,
		Quantity:       5.0,
		Reason:         "physical count mismatch",
		Actor:          "admin",
	}
	if err := db.CreateCorrection(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c.ID == 0 {
		t.Fatal("ID should be assigned")
	}

	corrections, err := db.ListCorrections(10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(corrections) != 1 {
		t.Fatalf("len = %d, want 1", len(corrections))
	}
	if corrections[0].CorrectionType != "add" {
		t.Errorf("type = %q, want %q", corrections[0].CorrectionType, "add")
	}
	if corrections[0].Reason != "physical count mismatch" {
		t.Errorf("reason = %q, want %q", corrections[0].Reason, "physical count mismatch")
	}
}
