//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// rule5_bind_docker_test.go — ground rule 5: a pickup takes a carrier that
// answers what its step names; the same rule for produce and consume.
//
// THE SEAM IS THE BIND, findAvailableForNeed. The reserve pass
// (reserveComplexPlan) asks it for every pickup the order holds no carrier for
// yet, and it is the one place the pass judges what a carrier is: matchHeldToNeed
// keeps what an earlier bind took, reservePickup refuses a carrier a live move
// names, missPickup answers a pickup that bound nothing. Each pickup here is
// built as the pass builds it: complexPickups over the leg's steps at the cell's
// press, the step's part over the order's (resolvedStepPayload).
//
// THE THREE STEP KINDS:
//
//   - Lift: a pickup at the order's own process node (a removal). Whatever
//     stands there.
//   - Empty: an Empty-flagged pickup. A carrier with no part that is not the
//     bare marker, which the height wanted where it is set down admits
//     (binresolver.BindableEmpties). On a cell's own position tops & bottoms binds
//     whatever that height (the house rule; the height's other arm is
//     TestTopsAndBottoms_AConcreteSupplyNodeHoldingOnlyIt's).
//   - Stock: a pickup naming the part. A carrier loaded with that part, its
//     manifest confirmed and counted above 0 (the stock fragment's rule,
//     helpers.BinCarriesSourceableStockSQL).
//
// THE ROLE IS THE CLAIM'S, AND IT CHANGES NO STEP. The bind reads no role: the
// Edge decides which kind a role's pickup is (its steady-state on-deck pickup is
// Empty on a produce press and names the part on a consume press; a changeover's
// names the outgoing part on both). Both roles are asked every kind, so a bind
// keyed on the role (a consume-only refusal) leaves the produce rows red.
//
// A STOCK PICKUP BINDS STOCK OF ITS PART ALONE (binresolver.StockOnly): a
// carrier counted out, one with no part, tops & bottoms and the bare marker
// hold it, on either role's press. It bound all four while the part-supply arm
// judged by BinUnavailableReasonFor, which asks no count and refuses only
// ANOTHER part: a consume press's on-deck set a part-less carrier on the press
// while a produce pair held. The part here states no carrier rule (the zero
// rule permits every type), so the carrier-rule arm is out of play and the
// marker is refused as no stock, not by its type.
func TestRule5_EveryStepKindAgainstEveryCarrierState(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	part := sd.Payload.Code

	eight, zero := 8, 0
	tote := &bins.BinType{Code: "R5-TOTE", TotesPerLayer: &eight}
	testutil.MustNoErr(t, db.CreateBinType(tote), "a tote type")
	marker, err := db.EnsureBareMarker(sd.BinType.ID)
	testutil.MustNoErr(t, err, "the bare marker")
	states := []r5State{
		{"loaded with the part, counted above 0", sd.BinType.ID, part, 100, nil, protocol.CarrierLoaded},
		{"counted out (0)", sd.BinType.ID, part, 0, nil, protocol.CarrierLoaded},
		{"part-less (an empty)", sd.BinType.ID, "", 0, nil, protocol.CarrierEmpty},
		{"tops & bottoms (a tote cart at 0 totes)", tote.ID, "", 0, &zero, protocol.CarrierTopsAndBottoms},
		{"bare (the marker type)", marker, "", 0, nil, protocol.CarrierBare},
	}

	const binds, holds = true, false
	kinds := []struct {
		name  string
		kind  pickupKind
		steps func(p r5Position, out string) []protocol.ComplexOrderStep
		cells [5]bool // whether it binds, by state, in the order of states
	}{
		{"Lift", pickupRemoval, func(p r5Position, out string) []protocol.ComplexOrderStep {
			return []protocol.ComplexOrderStep{prWait(p.press.Name), prPick(p.press.Name), prDrop(out)}
		}, [5]bool{binds, binds, binds, binds, binds}},
		{"Empty", pickupEmptySupply, func(p r5Position, _ string) []protocol.ComplexOrderStep {
			return []protocol.ComplexOrderStep{prWait(p.deck.Name),
				{Action: protocol.ActionPickup, Node: p.deck.Name, Empty: true}, prDrop(p.press.Name)}
		}, [5]bool{holds, holds, binds, binds, holds}},
		{"Stock", pickupPartSupply, func(p r5Position, _ string) []protocol.ComplexOrderStep {
			return []protocol.ComplexOrderStep{prWait(p.deck.Name),
				{Action: protocol.ActionPickup, Node: p.deck.Name, PayloadCode: part}, prDrop(p.press.Name)}
		}, [5]bool{binds, holds, holds, holds, holds}},
	}

	out := prNode(t, db, "R5-OUT")
	in := prNode(t, db, "R5-IN")
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
		positions := r5Press(t, db, role, part, in.Name, out.Name, states)
		for _, k := range kinds {
			for i, st := range states {
				p := positions[i]
				pk := complexPickups(prResolved(k.steps(p, out.Name)...), p.press.Name)[0]
				standing := p.onDeck
				if k.kind == pickupRemoval {
					standing = p.onPress
				}
				name := fmt.Sprintf("%s / %s / %s", role, k.name, st.name)
				if pk.kind != k.kind {
					t.Fatalf("%s: the pickup reads as kind %d, not the step kind it is written as (fixture drift)", name, pk.kind)
				}
				got, hadBins, err := d.allocator.findAvailableForNeed(pk, resolvedStepPayload(pk.step, part))
				testutil.MustNoErr(t, err, name)
				if !hadBins || (got != nil && got.ID != standing.ID) {
					t.Fatalf("%s: the node does not hold its one carrier %s (fixture drift)", name, standing.Label)
				}
				if took := got != nil; took != k.cells[i] {
					t.Errorf("%s: binds=%v, rule 5 says binds=%v", name, took, k.cells[i])
				}
			}
		}
	}
}

// r5State is one carrier state the table stands on a position: its type, its
// part and count (a confirmed manifest when it names a part), its totes on the
// cart, and the state CarrierStateOf must read from them.
type r5State struct {
	name    string
	binType int64
	part    string
	uop     int
	totes   *int
	want    protocol.CarrierState
}

// r5Position is one press of the table's: the press and its on-deck, each with
// a carrier of one state standing on it.
type r5Position struct {
	press, deck     *nodes.Node
	onPress, onDeck *bins.Bin
}

// r5Press stands one fed press per carrier state, its claim of the role
// mirrored for station line-1 (so its on-deck is a claim's own position), with
// a carrier of that state on the press and on the on-deck.
func r5Press(t *testing.T, db *store.DB, role protocol.ClaimRole, part, in, out string, states []r5State) []r5Position {
	t.Helper()
	process := "R5-" + string(role)
	var rows []plantclaims.ClaimRow
	positions := make([]r5Position, len(states))
	for i, st := range states {
		p := &positions[i]
		p.press = prNode(t, db, fmt.Sprintf("%s-PRESS-%d", process, i))
		p.deck = prNode(t, db, fmt.Sprintf("%s-DECK-%d", process, i))
		p.onPress = r5Stand(t, db, p.press, st)
		p.onDeck = r5Stand(t, db, p.deck, st)
		rows = append(rows, plantclaims.ClaimRow{ProcessID: process, StyleID: "R5-STYLE", CoreNodeName: p.press.Name,
			Role: role, SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: part, Seq: i,
			InboundSource: in, OutboundDestination: out, PairedCoreNode: p.deck.Name})
	}
	_, err := db.ReplacePlantClaimsFromStation(process, "line-1",
		[]plantclaims.StyleRow{{ProcessID: process, StyleID: "R5-STYLE", IsActive: true}}, rows, nil, 0)
	testutil.MustNoErr(t, err, "the "+string(role)+" presses' claims")
	return positions
}

// r5Stand stands a carrier of state st on node, as a carrier arriving at a line
// position stands (staged), and checks it reads as the state it is for.
func r5Stand(t *testing.T, db *store.DB, node *nodes.Node, st r5State) *bins.Bin {
	t.Helper()
	label := node.Name + "-CARRIER"
	b := &bins.Bin{BinTypeID: st.binType, Label: label, NodeID: &node.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(b), label)
	if st.part != "" {
		testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, st.part, st.uop), "manifest "+label)
		testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm "+label)
	}
	if st.totes != nil {
		_, err := db.Exec(`UPDATE bins SET totes_on_cart = $1 WHERE id = $2`, *st.totes, b.ID)
		testutil.MustNoErr(t, err, "totes on "+label)
	}
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "re-read "+label)
	if s := got.CarrierState(); s != st.want || got.UOPRemaining != st.uop || got.ManifestConfirmed != (st.part != "") {
		t.Fatalf("%s reads %s, count %d, confirmed %v: not %q (fixture drift)", label, s, got.UOPRemaining,
			got.ManifestConfirmed, st.name)
	}
	return got
}
