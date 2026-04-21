//go:build docker

package store

import "testing"

func TestVerifyTag_NoOrder(t *testing.T) {
	db := testDB(t)

	// Unknown order UUID — best-effort: Match=true, helpful Detail.
	res := db.VerifyTag("no-such-uuid", "TAG-1", "LINE-1")
	if res == nil {
		t.Fatal("result should never be nil")
	}
	if !res.Match {
		t.Errorf("no-order Match = false, want true (best-effort)")
	}
	if res.Detail == "" {
		t.Error("no-order Detail should be populated")
	}
}

func TestVerifyTag_NoBin(t *testing.T) {
	db := testDB(t)

	// Order with no bin assigned
	o := &Order{EdgeUUID: "tv-no-bin", Status: "pending"}
	db.CreateOrder(o)

	res := db.VerifyTag("tv-no-bin", "TAG-2", "LINE-1")
	if !res.Match {
		t.Errorf("no-bin Match = false, want true (best-effort)")
	}
}

func TestVerifyTag_LearnEmptyLabel(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "TV-BT", Description: "tv"}
	db.CreateBinType(bt)

	node := &Node{Name: "TV-NODE", Enabled: true}
	db.CreateNode(node)

	// Bin with NO label (empty string) — this is the "learn" branch
	bin := &Bin{BinTypeID: bt.ID, NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	o := &Order{EdgeUUID: "tv-learn", Status: "pending", BinID: &bin.ID}
	db.CreateOrder(o)

	res := db.VerifyTag("tv-learn", "LEARNED-TAG", "LINE-1")
	if !res.Match {
		t.Errorf("learn Match = false, want true")
	}

	// The bin's label should now be set to the scanned tag.
	got, _ := db.GetBin(bin.ID)
	if got.Label != "LEARNED-TAG" {
		t.Errorf("Label after learn = %q, want LEARNED-TAG", got.Label)
	}
}

func TestVerifyTag_MatchAndMismatch(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "TV-BT2", Description: "tv2"}
	db.CreateBinType(bt)
	node := &Node{Name: "TV-NODE2", Enabled: true}
	db.CreateNode(node)

	bin := &Bin{BinTypeID: bt.ID, Label: "TAG-EXPECTED", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	o := &Order{EdgeUUID: "tv-verify", Status: "pending", BinID: &bin.ID}
	db.CreateOrder(o)

	// Match path
	match := db.VerifyTag("tv-verify", "TAG-EXPECTED", "LINE-2")
	if !match.Match {
		t.Errorf("exact match Match = false, want true")
	}
	if match.Expected != "" {
		t.Errorf("match Expected = %q, want empty (only set on mismatch)", match.Expected)
	}

	// Mismatch path — Match=false, Expected=existing label
	mism := db.VerifyTag("tv-verify", "WRONG-TAG", "LINE-2")
	if mism.Match {
		t.Error("mismatch Match = true, want false")
	}
	if mism.Expected != "TAG-EXPECTED" {
		t.Errorf("mismatch Expected = %q, want TAG-EXPECTED", mism.Expected)
	}
	if mism.Detail == "" {
		t.Error("mismatch Detail should be populated")
	}

	// Mismatch should not mutate the bin's label — best-effort, log only.
	after, _ := db.GetBin(bin.ID)
	if after.Label != "TAG-EXPECTED" {
		t.Errorf("Label after mismatch = %q, should be unchanged", after.Label)
	}
}
