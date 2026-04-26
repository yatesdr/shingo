package testdb

import (
	"fmt"
	"testing"
	"time"

	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// CompoundScenario holds all entities for a compound reshuffle test.
// Tests access fields directly to inspect or mutate entities after setup.
type CompoundScenario struct {
	Grp          *nodes.Node
	Lane         *nodes.Node
	Slots        []*nodes.Node   // indexed by depth-1 (Slots[0] = depth 1, front)
	ShuffleSlots []*nodes.Node
	LineNode     *nodes.Node
	TargetBin    *bins.Bin
	Blockers     []*bins.Bin    // indexed front-to-back (Blockers[0] = shallowest)
	Payload      *payloads.Payload
	BinType      *bins.BinType
}

// CompoundConfig controls the layout of a compound reshuffle test scenario.
type CompoundConfig struct {
	// Prefix is used for all node and bin names (e.g., "SWAP", "STRAND", "TTL").
	// Required — each test must use a unique prefix to avoid name collisions.
	Prefix string

	// NumSlots is the lane depth (number of storage slots). Default: 2.
	NumSlots int

	// NumShuffles is the number of shuffle slots under the NGRP. Default: 1.
	NumShuffles int

	// TargetSlot is the 1-indexed slot where the target bin sits. Default: NumSlots (deepest).
	TargetSlot int

	// TargetAge is how far back to set the target bin's loaded_at. Default: 2h.
	TargetAge time.Duration

	// BlockerAges maps 1-indexed slot positions to loaded_at offsets for blockers.
	// If nil, blockers get no explicit loaded_at (database default = NOW()).
	// Example: map[int]time.Duration{2: 1*time.Hour} sets blocker at slot 2 to 1h ago.
	BlockerAges map[int]time.Duration
}

// SetupCompound creates an NGRP → LANE → slots → shuffle → line → bins layout
// for compound reshuffle tests. Returns the full scenario struct so tests can
// inspect and modify individual entities.
//
// The layout:
//
//	NGRP (Grp)
//	├── LANE (Lane)
//	│   ├── Slot 1 (depth 1, front) — blocker bin
//	│   ├── Slot 2 (depth 2)        — blocker bin
//	│   └── Slot N (depth N)        — target bin (oldest loaded_at)
//	├── Shuffle 1
//	└── Shuffle M
//	LINE (LineNode) — delivery destination
func SetupCompound(t *testing.T, db *store.DB, cfg CompoundConfig) *CompoundScenario {
	t.Helper()

	// Apply defaults
	if cfg.NumSlots < 1 {
		cfg.NumSlots = 2
	}
	if cfg.NumShuffles < 1 {
		cfg.NumShuffles = 1
	}
	if cfg.TargetSlot < 1 {
		cfg.TargetSlot = cfg.NumSlots
	}
	if cfg.TargetAge == 0 {
		cfg.TargetAge = 2 * time.Hour
	}

	// Get node type IDs
	grpType, err := db.GetNodeTypeByCode("NGRP")
	if err != nil {
		t.Fatalf("get NGRP node type: %v", err)
	}
	lanType, err := db.GetNodeTypeByCode("LANE")
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}

	// Create payload and bin type
	bp := &payloads.Payload{Code: fmt.Sprintf("PART-%s", cfg.Prefix), Description: fmt.Sprintf("%s test payload", cfg.Prefix)}
	if err := db.CreatePayload(bp); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	btCode := fmt.Sprintf("DEFAULT-%s", cfg.Prefix)
	bt := &bins.BinType{Code: btCode, Description: fmt.Sprintf("%s bin type", cfg.Prefix)}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}

	// Create NGRP
	grp := &nodes.Node{Name: fmt.Sprintf("GRP-%s", cfg.Prefix), NodeTypeID: &grpType.ID, Enabled: true, IsSynthetic: true}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create NGRP: %v", err)
	}

	// Create LANE under NGRP
	lane := &nodes.Node{
		Name: fmt.Sprintf("GRP-%s-L1", cfg.Prefix), NodeTypeID: &lanType.ID,
		ParentID: &grp.ID, Enabled: true, IsSynthetic: true,
	}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create LANE: %v", err)
	}

	// Create storage slots under LANE
	slots := make([]*nodes.Node, cfg.NumSlots)
	for i := 0; i < cfg.NumSlots; i++ {
		depth := i + 1
		slot := &nodes.Node{
			Name:     fmt.Sprintf("GRP-%s-L1-S%d", cfg.Prefix, depth),
			ParentID: &lane.ID, Enabled: true, Depth: &depth,
		}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %d: %v", depth, err)
		}
		slots[i] = slot
	}

	// Create shuffle slots under NGRP
	shuffleSlots := make([]*nodes.Node, cfg.NumShuffles)
	for i := 0; i < cfg.NumShuffles; i++ {
		name := fmt.Sprintf("GRP-%s-SHUF%d", cfg.Prefix, i+1)
		if cfg.NumShuffles == 1 {
			name = fmt.Sprintf("GRP-%s-SHUF", cfg.Prefix)
		}
		ss := &nodes.Node{Name: name, ParentID: &grp.ID, Enabled: true}
		if err := db.CreateNode(ss); err != nil {
			t.Fatalf("create shuffle slot %d: %v", i+1, err)
		}
		shuffleSlots[i] = ss
	}

	// Create line node (delivery destination)
	lineNode := &nodes.Node{Name: fmt.Sprintf("LINE-%s", cfg.Prefix), Enabled: true}
	if err := db.CreateNode(lineNode); err != nil {
		t.Fatalf("create line node: %v", err)
	}

	// Create target bin at the target slot (deepest by default)
	targetSlotNode := slots[cfg.TargetSlot-1]
	targetBin := &bins.Bin{BinTypeID: bt.ID, Label: fmt.Sprintf("BIN-%s-TARGET", cfg.Prefix), NodeID: &targetSlotNode.ID, Status: "available"}
	if err := db.CreateBin(targetBin); err != nil {
		t.Fatalf("create target bin: %v", err)
	}
	if err := db.SetBinManifest(targetBin.ID, `{"items":[]}`, bp.Code, 100); err != nil {
		t.Fatalf("set target bin manifest: %v", err)
	}
	if err := db.ConfirmBinManifest(targetBin.ID, ""); err != nil {
		t.Fatalf("confirm target bin manifest: %v", err)
	}
	// Backdate target to make it the oldest
	_, err = db.Exec(fmt.Sprintf(`UPDATE bins SET loaded_at = NOW() - interval '%d seconds' WHERE id = $1`, int(cfg.TargetAge.Seconds())), targetBin.ID)
	if err != nil {
		t.Fatalf("backdate target bin: %v", err)
	}

	// Create blocker bins at all other slots
	var blockers []*bins.Bin
	for i := 0; i < cfg.NumSlots; i++ {
		if i == cfg.TargetSlot-1 {
			continue // skip the target slot
		}
		blk := &bins.Bin{
			BinTypeID: bt.ID,
			Label:     fmt.Sprintf("BIN-%s-BLK%d", cfg.Prefix, i+1),
			NodeID:    &slots[i].ID,
			Status:    "available",
		}
		if err := db.CreateBin(blk); err != nil {
			t.Fatalf("create blocker bin at slot %d: %v", i+1, err)
		}
		if err := db.SetBinManifest(blk.ID, `{"items":[]}`, bp.Code, 50); err != nil {
			t.Fatalf("set blocker bin manifest: %v", err)
		}
		if err := db.ConfirmBinManifest(blk.ID, ""); err != nil {
			t.Fatalf("confirm blocker bin manifest: %v", err)
		}

		// Apply blocker age if specified
		if cfg.BlockerAges != nil {
			if age, ok := cfg.BlockerAges[i+1]; ok {
				_, err = db.Exec(fmt.Sprintf(`UPDATE bins SET loaded_at = NOW() - interval '%d seconds' WHERE id = $1`, int(age.Seconds())), blk.ID)
				if err != nil {
					t.Fatalf("backdate blocker bin at slot %d: %v", i+1, err)
				}
			}
		}
		blockers = append(blockers, blk)
	}

	return &CompoundScenario{
		Grp:          grp,
		Lane:         lane,
		Slots:        slots,
		ShuffleSlots: shuffleSlots,
		LineNode:     lineNode,
		TargetBin:    targetBin,
		Blockers:     blockers,
		Payload:      bp,
		BinType:      bt,
	}
}
