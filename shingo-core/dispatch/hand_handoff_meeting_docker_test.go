//go:build docker

package dispatch

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// A HAND WITH LANES AND A HANDOFF TO THE SAME PICKER, AT THE SAME MOMENT.
//
// The hand takes the order's row first (its status CAS) and the lane's advisory
// lock after it (acquireLanesTx). HandOffLaneToPicker takes the lane's advisory
// lock first and then, in its INSERT, OwnerLiveSQL's share lock on the picker's
// row. Those are opposite orders, recorded in OwnerLiveSQL's comment. A meeting
// is a cycle, and Postgres ends one side with a deadlock error.
//
// This pins that shape as it stands: one side gets 40P01, the other completes,
// the order holds nothing hard unless the hand committed, and nothing wedges.
// C2b gives the hand main's order (lane lock, then the row), and this test then
// asserts that neither side aborts.
//
// The handoff's side is its lock sequence spelt out on a transaction of the
// test's own, so the meeting is forced without a hook in the handoff. Its DELETE
// finds no dig row here: the lane is free, so the hand can commit when it is the
// side that survives. The DELETE takes no lock the hand wants either way.
func TestHandMeetsHandOff_PostgresAbortsOneSideAndNothingWedges(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	_, laneID, _ := gatedLane(t, db, "MEET", "")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "MEET-BIN")
	picker := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "meet-picker"; o.Status = StatusQueued })
	digOwner := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "meet-dig"; o.Status = StatusQueued })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, picker.ID, picker.ID, bin.ID, "test"), "soft-hold the picker's bin")

	// 1. The handoff takes the lane.
	handoff, err := db.DB.Begin()
	testutil.MustNoErr(t, err, "begin the handoff")
	defer handoff.Rollback()
	_, err = handoff.Exec(`SELECT pg_advisory_xact_lock($1)`, laneID)
	testutil.MustNoErr(t, err, "the handoff's lane lock")
	_, err = handoff.Exec(`DELETE FROM reservations WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2 AND mode=$3`,
		digOwner.ID, laneID, string(reservations.ModeDig))
	testutil.MustNoErr(t, err, "the handoff's dig-row delete")

	// 2. The picker's hand takes its row and blocks on the lane.
	handDone := make(chan error, 1)
	go func() {
		handDone <- d.acquireHand(picker, Hand{
			Bins:  []HandBin{{BinID: bin.ID}},
			Lanes: []laneHold{{laneID: laneID, mode: reservations.ModeOutbound}},
		}, nil, "test", "meeting")
	}()
	waitForAdvisoryWaiter(t, db.DB, laneID)

	// 3. The handoff asks for the picker's row, as its INSERT does, with the tag
	// it writes (C1.5: a dig handoff's row is a dig lock, not a hold at rest).
	insertDone := make(chan error, 1)
	go func() {
		_, err := handoff.Exec(
			`INSERT INTO reservations (order_id, resource_kind, node_id, state, reserved_by, mode, created_at)
			 SELECT $1, 'mouth', $2, 'confirmed', $4, $3, now() WHERE `+reservations.OwnerLiveSQL(1),
			picker.ID, laneID, string(reservations.ModeOutbound), reservations.ByDigHandoff)
		if err == nil {
			err = handoff.Commit()
		}
		insertDone <- err
	}()

	var handErr, handoffErr error
	for got := 0; got < 2; got++ {
		select {
		case handErr = <-handDone:
		case handoffErr = <-insertDone:
		case <-time.After(20 * time.Second):
			t.Fatal("the hand and the handoff wedged: neither returned within 20 s")
		}
	}

	handAborted, handoffAborted := isDeadlock(handErr), isDeadlock(handoffErr)
	switch {
	case handAborted && handoffAborted:
		t.Fatalf("both sides aborted: hand %v; handoff %v", handErr, handoffErr)
	case !handAborted && !handoffAborted:
		t.Fatalf("neither side aborted (C2b's lock order?): hand %v; handoff %v. Flip this pin with C2b", handErr, handoffErr)
	}

	var status string
	var claimedBy sql.NullInt64
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT status FROM orders WHERE id=$1`, picker.ID).Scan(&status), "picker status")
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT claimed_by FROM bins WHERE id=$1`, bin.ID).Scan(&claimedBy), "bin claim")
	var mouthRows int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM reservations
		 WHERE order_id=$1 AND resource_kind='mouth' AND node_id=$2 AND mode=$3`,
		picker.ID, laneID, string(reservations.ModeOutbound)).Scan(&mouthRows), "mouth rows")

	if handoffAborted {
		t.Logf("Postgres ended the handoff: %v", handoffErr)
		if handErr != nil {
			t.Fatalf("the handoff aborted and the hand failed too: %v", handErr)
		}
		if status != string(StatusDispatched) || !claimedBy.Valid || claimedBy.Int64 != picker.ID || mouthRows != 1 {
			t.Fatalf("the hand committed but holds status=%s bin claimed_by=%v mouth rows=%d; want dispatched, %d, 1",
				status, claimedBy, mouthRows, picker.ID)
		}
		return
	}
	t.Logf("Postgres ended the hand: %v", handErr)
	if handoffErr != nil {
		t.Fatalf("the hand aborted and the handoff failed too: %v", handoffErr)
	}
	var refusal *HandRefusal
	if !errors.As(handErr, &refusal) || refusal.Leg != HandLegLane {
		t.Fatalf("the aborted hand returned %v; want a lane-leg refusal (the order is asked again next pass)", handErr)
	}
	if status != string(StatusQueued) || claimedBy.Valid {
		t.Fatalf("the hand aborted but the order is %s with its bin claimed_by=%v; want queued, holding nothing hard", status, claimedBy)
	}
	if mouthRows != 1 {
		t.Fatalf("the handoff committed but the picker holds %d outbound rows on the lane, want 1", mouthRows)
	}
}

// waitForAdvisoryWaiter returns once a session in this database is waiting on
// the advisory lock keyed laneID.
func waitForAdvisoryWaiter(t *testing.T, db *sql.DB, laneID int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM pg_locks l
			 JOIN pg_database d ON d.oid = l.database
			 WHERE d.datname = current_database() AND l.locktype = 'advisory' AND NOT l.granted
			   AND l.classid = 0 AND l.objid::bigint = $1`, laneID).Scan(&n), "read pg_locks")
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the hand never reached the lane lock")
}

func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}
