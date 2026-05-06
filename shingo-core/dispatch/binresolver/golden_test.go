package binresolver

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"testing"
	"time"

	"shingocore/store/nodes"
)

var updateFlag = flag.Bool("update", false, "update golden files")

// Golden test scenarios capture the current behavior of each retrieve
// algorithm. Phase 2's collapsed scanner MUST produce identical results.
//
// Run with -update to regenerate golden files:
//
//	go test -run TestGolden -update ./dispatch/binresolver/

type goldenScenario struct {
	Name        string `json:"name"`
	Algorithm   string `json:"algorithm"`
	Payload     string `json:"payload"`
	ResultBinID int64  `json:"result_bin_id,omitempty"`
	ResultNode  string `json:"result_node,omitempty"`
	BuriedBinID int64  `json:"buried_bin_id,omitempty"`
	BuriedLane  int64  `json:"buried_lane,omitempty"`
	Error       string `json:"error,omitempty"`
}

func TestGolden_RetrieveAlgorithms(t *testing.T) {
	now := time.Now()

	scenarios := []struct {
		name       string
		setup      func(f *fakeStore) (*nodes.Node, string)
		algorithms []string
		lockLane   int64 // if nonzero, lock this lane ID before resolving
	}{
		{
			name: "single_lane_two_bins",
			setup: func(f *fakeStore) (*nodes.Node, string) {
				group := ngrpNode(1, "GRP")
				lane := laneChild(10, "L1")
				f.children[group.ID] = []*nodes.Node{lane}
				s1 := slotInLane(100, "S1")
				s2 := slotInLane(101, "S2")
				f.nodes[s1.ID] = s1
				f.nodes[s2.ID] = s2
				b1 := availBin(1001, "P1", now.Add(-2*time.Hour))
				b2 := availBin(1002, "P1", now.Add(-1*time.Hour))
				attachSlot(b1, s1)
				attachSlot(b2, s2)
				f.sourceInLane[lane.ID] = b1
				return group, "P1"
			},
			algorithms: []string{RetrieveFIFO, RetrieveCOST, RetrieveFAVL},
		},
		{
			name: "two_lanes_oldest_in_back",
			setup: func(f *fakeStore) (*nodes.Node, string) {
				group := ngrpNode(2, "GRP2")
				laneA := laneChild(20, "LA")
				laneB := laneChild(21, "LB")
				f.children[group.ID] = []*nodes.Node{laneA, laneB}
				sa := slotInLane(200, "SA")
				sb := slotInLane(201, "SB")
				f.nodes[sa.ID] = sa
				f.nodes[sb.ID] = sb
				binNew := availBin(2001, "P1", now)
				binOld := availBin(2002, "P1", now.Add(-3*time.Hour))
				attachSlot(binNew, sa)
				attachSlot(binOld, sb)
				f.sourceInLane[laneA.ID] = binNew
				f.sourceInLane[laneB.ID] = binOld
				return group, "P1"
			},
			algorithms: []string{RetrieveFIFO, RetrieveCOST, RetrieveFAVL},
		},
		{
			name: "empty_group",
			setup: func(f *fakeStore) (*nodes.Node, string) {
				group := ngrpNode(3, "GRP3")
				lane := laneChild(30, "L3")
				f.children[group.ID] = []*nodes.Node{lane}
				return group, "P1"
			},
			algorithms: []string{RetrieveFIFO, RetrieveCOST, RetrieveFAVL},
		},
		// buried_accessible_mismatch: the accessible bin at the front of the
		// lane carries P2 (wrong payload), while a buried bin deeper in the
		// lane carries the requested P1. No accessible P1 exists, so:
		//   - FIFO: FindOldestBuriedBin returns the P1 bin → BuriedError
		//   - COST: FindBuriedBin returns the P1 bin (shallowest buried) → BuriedError
		//   - FAVL: skips burial entirely, no accessible P1 → error
		{
			name: "buried_accessible_mismatch",
			setup: func(f *fakeStore) (*nodes.Node, string) {
				group := ngrpNode(4, "GRP4")
				lane := laneChild(40, "L4")
				f.children[group.ID] = []*nodes.Node{lane}
				frontSlot := slotInLane(400, "front")
				buriedSlot := slotInLane(401, "buried")
				f.nodes[frontSlot.ID] = frontSlot
				f.nodes[buriedSlot.ID] = buriedSlot
				// Accessible bin at front is P2 (wrong payload)
				front := availBin(4001, "P2", now.Add(-1*time.Hour))
				attachSlot(front, frontSlot)
				f.sourceInLane[lane.ID] = front
				// Buried P1 bin deeper in lane
				buried := availBin(4002, "P1", now.Add(-3*time.Hour))
				attachSlot(buried, buriedSlot)
				f.oldestBuried[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
				f.buriedAny[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
				return group, "P1"
			},
			algorithms: []string{RetrieveFIFO, RetrieveCOST, RetrieveFAVL},
		},
		// buried_older_behind_accessible: the accessible bin carries the
		// requested P1 but is newer; a buried bin also carries P1 and is
		// older. This is the FIFO-vs-COST behavioral split:
		//   - FIFO: buried older bin wins → BuriedError (reshuffle to get oldest)
		//   - COST: accessible bin wins (avoids reshuffle) → returns accessible
		//   - FAVL: returns first available (accessible bin) → no reshuffle
		{
			name: "buried_older_behind_accessible",
			setup: func(f *fakeStore) (*nodes.Node, string) {
				group := ngrpNode(5, "GRP5")
				lane := laneChild(50, "L5")
				f.children[group.ID] = []*nodes.Node{lane}
				accSlot := slotInLane(500, "acc")
				buriedSlot := slotInLane(501, "buried")
				f.nodes[accSlot.ID] = accSlot
				f.nodes[buriedSlot.ID] = buriedSlot
				// Accessible P1 bin is 10min old
				acc := availBin(5001, "P1", now.Add(-10*time.Minute))
				attachSlot(acc, accSlot)
				f.sourceInLane[lane.ID] = acc
				// Buried P1 bin is 2h old — FIFO wants this one
				buried := availBin(5002, "P1", now.Add(-2*time.Hour))
				attachSlot(buried, buriedSlot)
				f.oldestBuried[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
				f.buriedAny[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
				return group, "P1"
			},
			algorithms: []string{RetrieveFIFO, RetrieveCOST, RetrieveFAVL},
		},
		// locked_lane_buried_bin: one lane has a buried P1 bin but is
		// locked by another retrieve. No other lanes have bins.
		//   - FIFO: skips locked lane → no accessible, no buried found → error
		//   - COST: skips locked lane → no accessible, no buried found → error
		//   - FAVL: skips locked lane → no first match → error
		//
		// This locks in the behavioral change where the buried-bin scan
		// now respects LaneLock.IsLocked (old COST did not).
		{
			name: "locked_lane_buried_bin",
			setup: func(f *fakeStore) (*nodes.Node, string) {
				group := ngrpNode(6, "GRP6")
				lane := laneChild(60, "L6")
				f.children[group.ID] = []*nodes.Node{lane}
				buriedSlot := slotInLane(600, "buried")
				f.nodes[buriedSlot.ID] = buriedSlot
				buried := availBin(6001, "P1", now.Add(-3*time.Hour))
				attachSlot(buried, buriedSlot)
				f.oldestBuried[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
				f.buriedAny[lane.ID] = laneBuried{bin: buried, slot: buriedSlot}
				return group, "P1"
			},
			algorithms: []string{RetrieveFIFO, RetrieveCOST, RetrieveFAVL},
			lockLane:   60,
		},
	}

	var results []goldenScenario
	for _, sc := range scenarios {
		for _, algo := range sc.algorithms {
			f := newFakeStore()
			group, payload := sc.setup(f)
			f.setProp(group.ID, "retrieve_algorithm", algo)

			ll := NewLaneLock()
			if sc.lockLane != 0 {
				ll.TryLock(sc.lockLane, 999) // locked by some other order
			}
			gr := &GroupResolver{DB: f, LaneLock: ll}
			got, err := gr.ResolveRetrieve(group, payload)

			gs := goldenScenario{
				Name:      sc.name,
				Algorithm: algo,
				Payload:   payload,
			}
			if err != nil {
				var bErr *BuriedError
				if errors.As(err, &bErr) {
					gs.BuriedBinID = bErr.Bin.ID
					gs.BuriedLane = bErr.LaneID
				} else {
					gs.Error = err.Error()
				}
			} else if got != nil {
				gs.ResultBinID = got.Bin.ID
				if got.Node != nil {
					gs.ResultNode = got.Node.Name
				}
			}
			results = append(results, gs)
		}
	}

	goldenPath := "testdata/golden/retrieve_algorithms.json"

	if *updateFlag {
		data, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(goldenPath, data, 0644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d scenarios)", goldenPath, len(results))
		return
	}

	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden file %s not found (run with -update to create): %v", goldenPath, err)
	}
	var expected []goldenScenario
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	if len(results) != len(expected) {
		t.Fatalf("scenario count: got %d, want %d", len(results), len(expected))
	}
	for i, got := range results {
		want := expected[i]
		if got.Name != want.Name || got.Algorithm != want.Algorithm {
			t.Errorf("scenario %d: got %s/%s, want %s/%s", i, got.Name, got.Algorithm, want.Name, want.Algorithm)
		}
		if got.ResultBinID != want.ResultBinID {
			t.Errorf("%s/%s: bin_id = %d, want %d", got.Name, got.Algorithm, got.ResultBinID, want.ResultBinID)
		}
		if got.ResultNode != want.ResultNode {
			t.Errorf("%s/%s: node = %q, want %q", got.Name, got.Algorithm, got.ResultNode, want.ResultNode)
		}
		if got.BuriedBinID != want.BuriedBinID {
			t.Errorf("%s/%s: buried_bin_id = %d, want %d", got.Name, got.Algorithm, got.BuriedBinID, want.BuriedBinID)
		}
		if got.BuriedLane != want.BuriedLane {
			t.Errorf("%s/%s: buried_lane = %d, want %d", got.Name, got.Algorithm, got.BuriedLane, want.BuriedLane)
		}
		if got.Error != want.Error && want.Error != "" {
			t.Errorf("%s/%s: error = %q, want %q", got.Name, got.Algorithm, got.Error, want.Error)
		}
	}
}
