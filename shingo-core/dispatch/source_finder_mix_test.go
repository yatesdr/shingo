package dispatch

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// THE DECLARED CARRIER MIX, HONOURED.
//
// A loader can say what it wants on hand — three 45x48, one 32x32, one tote —
// and each window can say what it physically fits. When a window frees, the
// loader asks for whatever it is most short of that THIS window can hold.
//
// Two rules, and they are not the same strength:
//
//   - CAPABILITY IS HARD. A carrier that does not fit the slot is not a
//     carrier, whatever the mix says.
//   - QUOTA IS HONOURED, NOT APPROXIMATED. If the type it is short of is not
//     available, the pull WAITS. It does not substitute — declaring a mix and
//     abandoning it when inconvenient makes declaring it pointless (owner,
//     2026-08-02).
//
// A loader that declares NOTHING keeps taking whatever it finds, which is what
// every loader does today and what "first come, first served" means.

func mixLoader(db *fakeFinderDB, loaderID int64, windows map[int64]string, quota map[string]int, caps map[int64][]string) {
	db.loaders[loaderID] = &loaders.Loader{ID: loaderID, Role: loaders.RoleProduce, Layout: loaders.LayoutSharedWindow}
	for nodeID, name := range windows {
		db.addNode(&nodes.Node{ID: nodeID, Name: name, Enabled: true})
		h := loaders.Home{LoaderID: loaderID, PositionNodeID: nodeID}
		db.homes = append(db.homes, h)
		db.homeByPos[nodeID] = &db.homes[len(db.homes)-1]
	}
	for code, want := range quota {
		db.quotas[loaderID] = append(db.quotas[loaderID], loaders.Quota{
			LoaderID: loaderID, BinTypeCode: code, Want: want,
		})
	}
	if caps != nil {
		db.homeBinTypes[loaderID] = caps
	}
}

func TestMix_AsksForWhatTheLoaderIsShortOf(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	// Wants 3 × BIG and 1 × SMALL. Two BIG are already standing there.
	mixLoader(db, 1, map[int64]string{10: "W1", 11: "W2", 12: "W3"},
		map[string]int{"BIG": 3, "SMALL": 1}, nil)
	big := int64(11)
	db.addBin(&bins.Bin{ID: 1, BinTypeCode: "BIG", NodeID: &big})
	big2 := int64(12)
	db.addBin(&bins.Bin{ID: 2, BinTypeCode: "BIG", NodeID: &big2})

	f := NewSourceFinder(db, nil, nil)
	got := f.wantedBinType(SourceNeed{Intent: IntentEmpty, DeliveryNode: "W1"})
	if got != "SMALL" {
		t.Errorf("wanted type = %q, want SMALL — the loader holds 2 of 3 BIG and 0 of 1 "+
			"SMALL, so SMALL is the larger shortfall", got)
	}
}

func TestMix_NeverAsksForSomethingTheWindowCannotHold(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	// Short of BIG by 3, but W1 only fits SMALL.
	mixLoader(db, 1, map[int64]string{10: "W1", 11: "W2"},
		map[string]int{"BIG": 3, "SMALL": 1},
		map[int64][]string{10: {"SMALL"}})

	f := NewSourceFinder(db, nil, nil)
	got := f.wantedBinType(SourceNeed{Intent: IntentEmpty, DeliveryNode: "W1"})
	if got != "SMALL" {
		t.Errorf("wanted type = %q, want SMALL — BIG is the bigger shortfall but this "+
			"window does not fit one, and capability is not a preference", got)
	}
}

func TestMix_UndeclaredLoaderTakesWhateverItFinds(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	mixLoader(db, 1, map[int64]string{10: "W1"}, nil, nil)

	f := NewSourceFinder(db, nil, nil)
	if got := f.wantedBinType(SourceNeed{Intent: IntentEmpty, DeliveryNode: "W1"}); got != "" {
		t.Errorf("wanted type = %q, want empty — a loader with no declared mix keeps "+
			"taking whatever compatible carrier it finds, which is every loader today", got)
	}
}

func TestMix_SatisfiedLoaderAsksForNothing(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	mixLoader(db, 1, map[int64]string{10: "W1", 11: "W2"}, map[string]int{"BIG": 1}, nil)
	n := int64(11)
	db.addBin(&bins.Bin{ID: 1, BinTypeCode: "BIG", NodeID: &n})

	f := NewSourceFinder(db, nil, nil)
	if got := f.wantedBinType(SourceNeed{Intent: IntentEmpty, DeliveryNode: "W1"}); got != "" {
		t.Errorf("wanted type = %q, want empty — the mix is already satisfied, so there is "+
			"no shortfall to name", got)
	}
}

// The mix is about empties. A FULL retrieve is answering a different question
// and must not be narrowed by it.
func TestMix_DoesNotApplyToFullRetrieves(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	mixLoader(db, 1, map[int64]string{10: "W1"}, map[string]int{"BIG": 3}, nil)

	f := NewSourceFinder(db, nil, nil)
	if got := f.wantedBinType(SourceNeed{Intent: IntentFull, DeliveryNode: "W1"}); got != "" {
		t.Errorf("wanted type = %q on a FULL retrieve, want empty", got)
	}
}

// A destination that is not a loader window at all — an ordinary cell — has no
// mix and must not acquire one.
func TestMix_OrdinaryDestinationIsUnaffected(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(&nodes.Node{ID: 20, Name: "ALN_003", Enabled: true})

	f := NewSourceFinder(db, nil, nil)
	if got := f.wantedBinType(SourceNeed{Intent: IntentEmpty, DeliveryNode: "ALN_003"}); got != "" {
		t.Errorf("wanted type = %q at a non-loader destination, want empty", got)
	}
}
