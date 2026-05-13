//go:build docker

package uop_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

func testDB(t *testing.T) *store.DB {
	return testdb.Open(t)
}

// createTestBin creates a bin at the given node with a manifest and returns it.
func createTestBin(t *testing.T, db *store.DB, nodeID int64, label, payloadCode string, uop int) *bins.Bin {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt = &bins.BinType{Code: "DEFAULT", Description: "Default test bin type"}
		if err := db.CreateBinType(bt); err != nil {
			t.Fatalf("create default bin type: %v", err)
		}
	}
	bin := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &nodeID, Status: "available"}
	if err := db.CreateBin(bin); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	if payloadCode != "" {
		if err := db.SetBinManifest(bin.ID, `{"items":[{"catid":"PART","qty":100}]}`, payloadCode, uop); err != nil {
			t.Fatalf("set manifest for bin %s: %v", label, err)
		}
		if err := db.ConfirmBinManifest(bin.ID, ""); err != nil {
			t.Fatalf("confirm manifest for bin %s: %v", label, err)
		}
	}
	got, _ := db.GetBin(bin.ID)
	return got
}
