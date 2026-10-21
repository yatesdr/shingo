//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
)

// TestMaintainer_MintsNoAskForATypeNothingOutsideCanSource: a group short of a
// type that no empty outside the group can supply gets no ask. The episode is
// kept (the group is short, and says so), but nothing is minted and no
// position is spoken for.
//
// The keeper used to mint one ask per free typed slot (createAsks: a loop on
// PlanMaintainedSlot, bounded by the slots, never by the source). Each ask is
// pre-resolved to a position and names it as its delivery, and ResolveStore
// counts a position with an order inbound as taken, so every ask held its
// position while it parked for a carrier that does not exist. At a group only
// an unloader feeds, those are the positions the unloader's empty-out needs.
// The keeper's tests that are about asking now stand an empty outside the
// group (mntOutsideEmpty).
func TestMaintainer_MintsNoAskForATypeNothingOutsideCanSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	mntFixture(t, db, "MNA-GRP", 3, "MNA-TOTE-1", 3) // no MNA-TOTE-1 empty anywhere

	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "MNA-GRP", "MNA-TOTE-1")

	if st.Gap != 3 {
		t.Fatalf("fixture: gap = %d, want 3 (want 3, nothing resident, asked or coming)", st.Gap)
	}
	if st.OriginID == "" {
		t.Errorf("no episode: the group is short of 3 and the episode is how it says so")
	}
	if st.Created != 0 {
		t.Errorf("created %d asks for a type no empty outside the group can supply; want 0", st.Created)
	}
	if st.HeldBy == "" {
		t.Errorf("the state does not say why nothing was asked (held_by is blank)")
	}
	var live int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM orders
		WHERE delivery_node LIKE 'MNA-GRP%' AND status NOT IN (`+protocol.TerminalStatusSQLList()+`)`).Scan(&live),
		"count live asks")
	if live != 0 {
		t.Errorf("%d live order(s) name a position of the group as their delivery; want none", live)
	}
}
