package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store"
)

// TestLoaderMemberNodes_BranchesOnLayout pins the C1 live-bug fix: member-node
// projection branches on the loader's LAYOUT, not on "does it have positions."
// The regression case is a shared_window loader that HAS window homes — the old
// len(Positions)>0 heuristic took the dedicated branch and emitted allowed:[""]
// members; it must now project a single member at the FIRST WINDOW carrying the
// shared set (the loader has no node of its own post-6b).
func TestLoaderMemberNodes_BranchesOnLayout(t *testing.T) {
	t.Parallel()

	t.Run("shared_window with window homes -> first window + shared set (no blank allowed)", func(t *testing.T) {
		l := store.CoreLoader{
			LoaderKey: "loader:WELD-1", Role: "produce", Layout: "shared_window",
			Positions: []store.CoreLoaderPosition{
				{PositionNode: "WELD-1-W1", PayloadCode: "", Kind: "window"},
				{PositionNode: "WELD-1-W2", PayloadCode: "", Kind: "window"},
			},
			Payloads: []store.CoreLoaderPayload{{PayloadCode: "PART-A"}, {PayloadCode: "PART-B"}},
		}
		got := loaderMemberNodes(l)
		if len(got) != 1 {
			t.Fatalf("shared loader should project one member, got %d: %+v", len(got), got)
		}
		m := got[0]
		if m.nodeName != "WELD-1-W1" {
			t.Errorf("member node = %q, want the first window WELD-1-W1", m.nodeName)
		}
		if len(m.allowed) != 2 || m.allowed[0] != "PART-A" || m.allowed[1] != "PART-B" {
			t.Errorf("member allowed = %v, want the full shared set [PART-A PART-B]", m.allowed)
		}
		for _, a := range m.allowed {
			if a == "" {
				t.Fatalf("regression: shared loader emitted a blank allowed payload: %v", m.allowed)
			}
		}
	})

	t.Run("dedicated_positions -> one member per position with its payload", func(t *testing.T) {
		l := store.CoreLoader{
			LoaderKey: "loader:SLN-2", Role: "produce", Layout: "dedicated_positions",
			Positions: []store.CoreLoaderPosition{
				{PositionNode: "HOME-1", PayloadCode: "PART-A", Kind: "dedicated", MinStock: 2},
				{PositionNode: "HOME-2", PayloadCode: "PART-B", Kind: "dedicated", MinStock: 3},
			},
		}
		got := loaderMemberNodes(l)
		if len(got) != 2 {
			t.Fatalf("dedicated loader should project one member per position, got %d", len(got))
		}
		if got[0].nodeName != "HOME-1" || got[0].payload != "PART-A" || got[0].minStock != 2 {
			t.Errorf("member 0 = %+v, want HOME-1/PART-A/2", got[0])
		}
		if len(got[1].allowed) != 1 || got[1].allowed[0] != "PART-B" {
			t.Errorf("dedicated member 1 allowed = %v, want [PART-B]", got[1].allowed)
		}
	})

	t.Run("unknown layout falls back to heuristic (positions -> members)", func(t *testing.T) {
		l := store.CoreLoader{
			LoaderKey: "loader:LEG-1", Role: "produce", Layout: "",
			Positions: []store.CoreLoaderPosition{{PositionNode: "LEG-1-P", PayloadCode: "PART-X", Kind: "dedicated"}},
			Payloads:  []store.CoreLoaderPayload{{PayloadCode: "PART-X"}},
		}
		got := loaderMemberNodes(l)
		if len(got) != 1 || got[0].nodeName != "LEG-1-P" {
			t.Fatalf("legacy loader with positions should resolve to them, got %+v", got)
		}
	})
}

// TestListActiveByDeliveryNodeSet pins the one-query set count the reservation
// seam uses: non-terminal orders across the node set, unioned, excluding nodes
// outside the set (and terminal orders, via the shared NOT IN filter).
func TestListActiveByDeliveryNodeSet(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)

	nA := seedCapManualSwap(t, db, "SET-A", "NODE-A", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	nB := seedCapManualSwap(t, db, "SET-B", "NODE-B", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)
	nC := seedCapManualSwap(t, db, "SET-C", "NODE-C", protocol.ClaimRoleProduce, []string{"P1"}, 0, false)

	mk := func(nodeID int64, delivery string) {
		if _, err := eng.orderMgr.CreateRetrieveOrder(&nodeID, true, 1, delivery, "EMPTY-SUPER", "", "standard", "P1", false, true); err != nil {
			t.Fatalf("create order at %s: %v", delivery, err)
		}
	}
	mk(nA, "NODE-A")
	mk(nA, "NODE-A")
	mk(nB, "NODE-B")
	mk(nC, "NODE-C") // outside the queried set — must be excluded

	got, err := db.ListActiveOrdersByDeliveryNodeSet([]string{"NODE-A", "NODE-B"})
	if err != nil {
		t.Fatalf("ListActiveOrdersByDeliveryNodeSet: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("set {A,B} = %d orders, want 3 (2 at A + 1 at B, none from C)", len(got))
	}
	for _, o := range got {
		if o.DeliveryNode == "NODE-C" {
			t.Errorf("order from NODE-C leaked into set {A,B}")
		}
	}

	if empty, err := db.ListActiveOrdersByDeliveryNodeSet(nil); err != nil || len(empty) != 0 {
		t.Errorf("empty set = (%d, %v), want (0, nil)", len(empty), err)
	}
}
