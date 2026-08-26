package service

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/internal/testdb"
	"shingoedge/store/stations"
)

// The edge claimed-nodes API did no validation at all, and process_nodes are
// matched by core_node_name everywhere. So a node GROUP label pasted where a
// node name belongs produced a row resolving to nothing on Core, rendering on no
// board — and because SetNodes ADOPTS by name, it stayed adoptable onto a live
// station indefinitely. Springfield carried three (`Unloader Pull From`,
// `Unloader Send To`, `SNF2 Loader`).
//
// The two fail-open rules are as load-bearing as the check itself, so they are
// asserted first.

func newStationFixture(t *testing.T) (*StationService, int64) {
	t.Helper()
	db := testdb.Open(t)
	svc := NewStationService(db)
	pid, err := db.CreateProcess("Press 4", "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	id, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Name: "Unloader"})
	if err != nil {
		t.Fatalf("CreateOperatorStation: %v", err)
	}
	return svc, id
}

func nodeNames(t *testing.T, svc *StationService, stationID int64) []string {
	t.Helper()
	nodes, err := svc.db.ListProcessNodesByStation(stationID)
	if err != nil {
		t.Fatalf("ListProcessNodesByStation: %v", err)
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.CoreNodeName)
	}
	return out
}

// No resolver wired: every existing caller and the lighter test constructors
// must behave exactly as they did before the check existed.
func TestSetNodes_NoResolverAcceptsAnything(t *testing.T) {
	t.Parallel()
	svc, id := newStationFixture(t)
	testutil.MustNoErr(t, svc.SetNodes(id, []string{"Unloader Send To"}), "set")
	if got := nodeNames(t, svc, id); len(got) != 1 {
		t.Errorf("expected the name through unchecked, got %v", got)
	}
}

// An edge that has booted but not yet received its first node list knows no
// names. Refusing on that would lock config out during exactly the window
// somebody is most likely to be fixing something.
func TestSetNodes_EmptyCacheAcceptsAnything(t *testing.T) {
	t.Parallel()
	svc, id := newStationFixture(t)
	svc.SetCoreNodeResolver(func() map[string]bool { return map[string]bool{} })

	testutil.MustNoErr(t, svc.SetNodes(id, []string{"ULN_002"}), "set")
	if got := nodeNames(t, svc, id); len(got) != 1 {
		t.Errorf("empty cache must fail open, got %v", got)
	}
}

func TestSetNodes_RejectsANameCoreDoesNotHave(t *testing.T) {
	t.Parallel()
	svc, id := newStationFixture(t)
	svc.SetCoreNodeResolver(func() map[string]bool {
		return map[string]bool{"ULN_002": true, "ULN_003": true}
	})

	err := svc.SetNodes(id, []string{"ULN_002", "Unloader Send To"})
	if !errors.Is(err, ErrUnknownCoreNodes) {
		t.Fatalf("expected ErrUnknownCoreNodes, got %v", err)
	}
	// The message has to name the offender: "one of these is wrong" sends
	// somebody back to a log, which is the failure mode being removed.
	if got := err.Error(); !strings.Contains(got, "Unloader Send To") {
		t.Errorf("error does not name the bad node: %q", got)
	}

	// WHOLESALE REPLACE, so the refusal must be total. Writing the good half
	// would report success for a list the caller never asked for.
	if got := nodeNames(t, svc, id); len(got) != 0 {
		t.Errorf("partial write on a rejected save: %v", got)
	}
}

func TestSetNodes_AcceptsKnownNames(t *testing.T) {
	t.Parallel()
	svc, id := newStationFixture(t)
	svc.SetCoreNodeResolver(func() map[string]bool {
		return map[string]bool{"ULN_002": true, "ULN_003": true}
	})

	testutil.MustNoErr(t, svc.SetNodes(id, []string{"ULN_002", "ULN_003"}), "set")
	if got := nodeNames(t, svc, id); len(got) != 2 {
		t.Errorf("expected both windows bound, got %v", got)
	}
}

// The check reads the INCOMING list only, which is what makes a station holding
// a legacy bad name still fixable: you drop it by not sending it. A check that
// validated stored rows too would refuse every edit and strand the station.
func TestSetNodes_CanRemoveAnAlreadyStoredBadName(t *testing.T) {
	t.Parallel()
	svc, id := newStationFixture(t)

	// Stored before the check existed.
	testutil.MustNoErr(t, svc.SetNodes(id, []string{"Unloader Send To"}), "seed")

	svc.SetCoreNodeResolver(func() map[string]bool { return map[string]bool{"ULN_002": true} })
	testutil.MustNoErr(t, svc.SetNodes(id, []string{"ULN_002"}), "replace")

	got := nodeNames(t, svc, id)
	if len(got) != 1 || got[0] != "ULN_002" {
		t.Errorf("bad name not cleared: %v", got)
	}
}
