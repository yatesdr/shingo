//go:build docker

package store

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/inventory"
	"shingocore/store/nodes"
)

func TestCorrectionCRUD(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	node := &nodes.Node{Name: "S1", Enabled: true}
	db.CreateNode(node)

	c := &inventory.Correction{
		CorrectionType: "add",
		NodeID:         node.ID,
		Quantity:       5.0,
		Reason:         "physical count mismatch",
		Actor:          "admin",
	}
	testutil.MustNoErr(t, db.CreateCorrection(c), "create")
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
