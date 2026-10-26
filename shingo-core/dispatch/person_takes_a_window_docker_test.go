//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// person_takes_a_window_docker_test.go — ground rule 3 for a position as for a
// cart: a station's move is a person's act, so another order's PENDING hold on
// the drop it declares (Need.Window) is taken out loud at its issue, the holder
// told slot-taken as a dig's ranked take tells it, and a CONFIRMED hold, a
// robot's, refuses the move, naming its holder.

// windowHeldBy issues a station's move of one cart onto a storage drop another
// order holds: a Core ask, queued, holding it from its issue; or, confirmed, a
// robot's order on its way there, its slot confirmed at its hand. It returns the
// database, the drop, the holder and what the station was told.
func windowHeldBy(t *testing.T, prefix string, confirmed bool) (*store.DB, *nodes.Node, int64, []protocol.OrderError) {
	t.Helper()
	db := testdb.IssueCensusDB(t)
	storage, _, bp := setupTestData(t, db)
	drop := &nodes.Node{Name: prefix + "-DROP", Enabled: true, NodeTypeID: storage.NodeTypeID}
	testutil.MustNoErr(t, db.CreateNode(drop), "the move's drop")
	cart := testdb.CreateBinAtNode(t, db, bp.Code, storage.ID, prefix+"-CART")
	holder := &orders.Order{EdgeUUID: strings.ToLower(prefix) + "-ask", StationID: "line-1",
		OrderType: OrderTypeRetrieveEmpty, Status: StatusQueued, Quantity: 1, DeliveryNode: drop.Name,
		OriginClass: protocol.OriginClassNoDemand}
	if confirmed {
		holder.Status = StatusDispatched
	}
	testutil.MustNoErr(t, db.CreateOrder(holder), "the holder")
	testutil.MustNoErr(t, db.ReserveSlot(drop.ID, holder.ID), "the holder's hold on the drop")
	if confirmed {
		testutil.MustNoErr(t, db.ConfirmSlotReservation(drop.ID, holder.ID), "its hand confirms it")
	}
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	env := testEnvelope()
	env.ID = "env-" + strings.ToLower(prefix)
	told := d.RefusalsFor(env, func() {
		d.HandleOrderRequest(env, &protocol.OrderRequest{OrderUUID: strings.ToLower(prefix) + "-move",
			OrderType: OrderTypeMove, PayloadCode: bp.Code, Quantity: 1, BinID: cart.ID,
			SourceNode: storage.Name, DeliveryNode: drop.Name})
	})
	return db, drop, holder.ID, told
}

func TestNamedMove_TakesAPendingHoldOnItsDropOutLoud(t *testing.T) {
	t.Parallel()
	db, drop, holder, told := windowHeldBy(t, "PTWP", false)
	if len(told) != 0 {
		t.Fatalf("the move was refused: %+v", told)
	}
	move := orderIDByUUID(t, db, "ptwp-move")
	if at, _, ok := testdb.IssuedAtCommit(t, db, move); !ok || !at.SlotRow {
		t.Errorf("the move held %s at its issue's commit, want %s's slot row", at, drop.Name)
	}
	h, err := db.GetOrder(holder)
	testutil.MustNoErr(t, err, "reload the holder")
	var rows, history int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE order_id=$1 AND resource_kind='slot'`,
		holder).Scan(&rows), "the holder's slot rows")
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM order_history WHERE order_id=$1 AND code='slot-taken'`,
		holder).Scan(&history), "the holder's history")
	if rows != 0 || h.QueueCause != string(CauseSlotTaken) || !strings.Contains(h.QueueReason, fmt.Sprint(move)) || history != 1 {
		t.Errorf("the holder kept %d slot row(s), waits %q (%q), and %d history row(s) say slot-taken; "+
			"want none, slot-taken naming order %d, and one", rows, h.QueueCause, h.QueueReason, history, move)
	}
}

func TestNamedMove_IsRefusedByAConfirmedHoldOnItsDropNamingItsHolder(t *testing.T) {
	t.Parallel()
	db, drop, holder, told := windowHeldBy(t, "PTWC", true)
	if len(told) != 1 || told[0].ErrorCode != string(protocol.TermClaimFailed) ||
		!strings.Contains(told[0].Detail, drop.Name) || !strings.Contains(told[0].Detail, fmt.Sprintf("order %d", holder)) {
		t.Fatalf("the station was told %+v, want one claim_failed naming %s and order %d", told, drop.Name, holder)
	}
	if move := orderIDByUUID(t, db, "ptwc-move"); move != 0 {
		t.Errorf("a move onto a drop a robot holds was issued: order %d", move)
	}
	var rows int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM reservations WHERE order_id=$1 AND resource_kind='slot'`,
		holder).Scan(&rows), "the holder's slot rows")
	if rows != 1 {
		t.Errorf("the robot's order holds %d slot row(s), want its 1", rows)
	}
}
