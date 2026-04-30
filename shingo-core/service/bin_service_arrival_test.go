//go:build docker

package service

import (
	"testing"
	"time"

	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestApplyArrival exercises BinService.ApplyArrival's two-branch contract:
// staged vs. unstaged, claim-release in both. The orchestration body lives
// in this package as of Phase 6.4a (moved from store/completion.go's
// (db *DB).ApplyBinArrival, which has been deleted).
func TestApplyArrival(t *testing.T) {
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "AB-BT", Description: "tote"}
	db.CreateBinType(bt)

	startNode := &nodes.Node{Name: "AB-START", Enabled: true}
	db.CreateNode(startNode)
	destNode := &nodes.Node{Name: "AB-DEST", Enabled: true}
	db.CreateNode(destNode)

	cases := []struct {
		name      string
		staged    bool
		expiresAt *time.Time
		wantStat  domain.BinStatus
	}{
		{"unstaged arrival", false, nil, "available"},
		{
			"staged arrival",
			true,
			func() *time.Time { tt := time.Now().Add(2 * time.Hour); return &tt }(),
			"staged",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := &bins.Bin{BinTypeID: bt.ID, Label: "AB-" + tc.name, NodeID: &startNode.ID, Status: "available"}
			if err := db.CreateBin(bin); err != nil {
				t.Fatalf("create bin: %v", err)
			}
			// Claim so we can verify ApplyArrival releases it.
			db.ClaimBin(bin.ID, 7)

			if err := svc.ApplyArrival(bin.ID, destNode.ID, tc.staged, tc.expiresAt); err != nil {
				t.Fatalf("ApplyArrival: %v", err)
			}

			got, _ := db.GetBin(bin.ID)
			if got.NodeID == nil || *got.NodeID != destNode.ID {
				t.Errorf("NodeID = %v, want %d", got.NodeID, destNode.ID)
			}
			if got.ClaimedBy != nil {
				t.Errorf("ClaimedBy = %v, want nil after arrival", got.ClaimedBy)
			}
			if got.Status != tc.wantStat {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStat)
			}
			if tc.staged {
				if got.StagedAt == nil {
					t.Error("StagedAt should be set when staged=true")
				}
				if tc.expiresAt != nil {
					if got.StagedExpiresAt == nil {
						t.Error("StagedExpiresAt should be set when expiresAt provided")
					} else {
						// Compare to within a second to allow for round-trip precision.
						diff := got.StagedExpiresAt.Sub(*tc.expiresAt)
						if diff < -time.Second || diff > time.Second {
							t.Errorf("StagedExpiresAt = %v, want ~%v", got.StagedExpiresAt, tc.expiresAt)
						}
					}
				}
			} else {
				if got.StagedAt != nil {
					t.Errorf("StagedAt = %v, want nil for unstaged", got.StagedAt)
				}
				if got.StagedExpiresAt != nil {
					t.Errorf("StagedExpiresAt = %v, want nil for unstaged", got.StagedExpiresAt)
				}
			}
		})
	}
}
