//go:build docker

package dispatch

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// NOTHING IS CLAIMED, AND NO JUNCTION ROW WRITTEN, FOR AN ORDER THAT HAS ENDED.
//
// The claim writers (bins.claimed_by through the three bin claim statements,
// nodes.claimed_by through ClaimSlotTx) and the order_bins writers run from a
// row read before a cancel. A claim checked only that the owner holds a
// reservation; with a terminalize open, that reservation is still visible, so
// the claim landed after the terminalize had released the order's claims. The
// junction is written by its own transaction after the claims, so a terminalize
// that commits between them left rows on an ended order.

// openTerminalize begins a transaction that does what TerminalizeOrder does, in
// its order: the order row, then the order's claims, junction rows and
// reservations. It is left open for the caller to commit.
func openTerminalize(t *testing.T, f *endedOwnerFixture, orderID int64) func() {
	t.Helper()
	tx, err := f.db.DB.Begin()
	testutil.MustNoErr(t, err, "begin the terminalize")
	for _, stmt := range []string{
		`UPDATE orders SET status='cancelled' WHERE id=$1`,
		`UPDATE bins SET claimed_by=NULL WHERE claimed_by=$1`,
		`UPDATE nodes SET claimed_by=NULL WHERE claimed_by=$1`,
		`DELETE FROM order_bins WHERE order_id=$1`,
		`DELETE FROM reservations WHERE order_id=$1`,
	} {
		_, err := tx.Exec(stmt, orderID)
		testutil.MustNoErr(t, err, stmt)
	}
	return func() { testutil.MustNoErr(t, tx.Commit(), "commit the terminalize") }
}

type claimWriter struct {
	name  string
	setup func(t *testing.T, f *endedOwnerFixture, owner int64) // while the owner is live
	write func(f *endedOwnerFixture, owner int64) error
}

func claimWriters() []claimWriter {
	reserveBin := func(t *testing.T, f *endedOwnerFixture, owner int64) {
		testutil.MustNoErr(t, reservations.Acquire(f.db.DB, owner, owner, f.binID(t), "test"), "reserve the bin")
	}
	uop := func(n int) *int { return &n }
	return []claimWriter{
		{"claim", reserveBin, func(f *endedOwnerFixture, owner int64) error {
			return f.d.binManifest.ConfirmClaim(f.bin, owner, nil)
		}},
		{"clear-and-claim", reserveBin, func(f *endedOwnerFixture, owner int64) error {
			return f.d.binManifest.ConfirmClaim(f.bin, owner, uop(0))
		}},
		{"sync-and-claim", reserveBin, func(f *endedOwnerFixture, owner int64) error {
			return f.d.binManifest.ConfirmClaim(f.bin, owner, uop(5))
		}},
		{"slot", func(t *testing.T, f *endedOwnerFixture, owner int64) {
			testutil.MustNoErr(t, reservations.AcquireSlot(f.db.DB, owner, f.slot.ID, "test"), "reserve the slot")
		}, func(f *endedOwnerFixture, owner int64) error {
			return f.db.ConfirmSlotClaim(f.slot.ID, owner, nil)
		}},
		{"replace-order-bins", func(t *testing.T, f *endedOwnerFixture, owner int64) {
			// A row already there, so the DELETE has something to lock.
			testutil.MustNoErr(t, f.db.InsertOrderBin(owner, f.bin, 0, "pickup", f.src.Name, ""), "a junction row")
		}, func(f *endedOwnerFixture, owner int64) error {
			return f.db.ReplaceOrderBins(owner, []orders.OrderBinRow{{BinID: f.bin, StepIndex: 0, Action: "pickup", NodeName: f.src.Name}})
		}},
		{"insert-order-bin", nil, func(f *endedOwnerFixture, owner int64) error {
			return f.db.InsertOrderBin(owner, f.bin, 0, "pickup", f.src.Name, "")
		}},
	}
}

// holdsNoClaims: nothing claimed by the order and no junction row for it.
func (f *endedOwnerFixture) holdsNoClaims(t *testing.T, orderID int64) {
	t.Helper()
	var b, n, j int
	testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM bins WHERE claimed_by=$1`, orderID).Scan(&b), "bins")
	testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM nodes WHERE claimed_by=$1`, orderID).Scan(&n), "nodes")
	testutil.MustNoErr(t, f.db.DB.QueryRow(`SELECT count(*) FROM order_bins WHERE order_id=$1`, orderID).Scan(&j), "order_bins")
	if b+n+j != 0 {
		t.Fatalf("the ended order holds bins=%d nodes=%d order_bins=%d", b, n, j)
	}
}

// Every writer, against an open terminalize.
func TestClaims_NoneWrittenAlongsideAnOpenTerminalize(t *testing.T) {
	t.Parallel()
	for _, w := range claimWriters() {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			f := seedEndedOwner(t, "COE-"+strings.ToUpper(w.name[:4]))
			owner := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = StatusSourcing })
			if w.setup != nil {
				w.setup(t, f, owner.ID)
			}
			commit := openTerminalize(t, f, owner.ID)
			done := make(chan error, 1)
			go func() { done <- w.write(f, owner.ID) }()
			time.Sleep(300 * time.Millisecond) // the write runs while the terminalize is open
			commit()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, reservations.ErrOwnerEnded) {
					t.Logf("write returned %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the write never returned")
			}
			f.holdsNoClaims(t, owner.ID)
		})
	}
}

// order_bins has a sequential window too: the junction is its own transaction
// after the claims, and a terminalize can commit between them.
func TestClaims_NoJunctionWrittenForAnOrderAlreadyEnded(t *testing.T) {
	t.Parallel()
	f := seedEndedOwner(t, "COE-SEQ")
	for _, w := range claimWriters()[4:] {
		owner := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = StatusSourcing })
		_, err := f.db.TerminalizeOrder(owner.ID, protocol.StatusCancelled, "test: ended between its claims and its junction")
		testutil.MustNoErr(t, err, "cancel")
		if err := w.write(f, owner.ID); !errors.Is(err, reservations.ErrOwnerEnded) {
			t.Errorf("%s for an ended order = %v, want ErrOwnerEnded", w.name, err)
		}
		f.holdsNoClaims(t, owner.ID)
	}
}

// The writer and a real terminalize, against each other, repeatedly: the owner
// lock is the first row lock in every transaction, so nothing deadlocks.
func TestClaims_NoDeadlockAgainstTerminalize(t *testing.T) {
	t.Parallel()
	for _, w := range claimWriters() {
		t.Run(w.name, func(t *testing.T) {
			t.Parallel()
			f := seedEndedOwner(t, "COD-"+strings.ToUpper(w.name[:4]))
			for i := 0; i < 25; i++ {
				owner := testdb.CreateOrder(t, f.db, func(o *orders.Order) { o.Status = StatusSourcing })
				if w.setup != nil {
					w.setup(t, f, owner.ID)
				}
				var wg sync.WaitGroup
				var werr, terr error
				wg.Add(2)
				go func() { defer wg.Done(); werr = w.write(f, owner.ID) }()
				go func() {
					defer wg.Done()
					_, terr = f.db.TerminalizeOrder(owner.ID, protocol.StatusCancelled, "test: race")
				}()
				wg.Wait()
				for _, err := range []error{werr, terr} {
					if err != nil && strings.Contains(err.Error(), "40P01") {
						t.Fatalf("round %d: deadlock: %v", i, err)
					}
				}
				f.holdsNoClaims(t, owner.ID)
				testutil.MustNoErr(t, reservations.ReleaseByOrder(f.db.DB, owner.ID), "clean")
			}
		})
	}
}

// An order that ends after its claims and before the fleet handover sends
// nothing: the handover's status CAS refuses it ahead of the create.
func TestHandover_AnOrderEndedAfterItsClaimsSendsNothing(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "HANDOVER-ENDED-BIN")
	o := &orders.Order{
		EdgeUUID: "HANDOVER-ended", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusQueued, Quantity: 1,
		SourceNode: sd.StorageNode.Name, DeliveryNode: sd.LineNode.Name,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order")
	testdb.ClaimBinForTest(t, db, bin.ID, o.ID)
	stale, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "the row the pass read")
	_, err = db.TerminalizeOrder(o.ID, protocol.StatusCancelled, "test: cancelled after its claims")
	testutil.MustNoErr(t, err, "cancel")
	src, err := db.GetNodeByDotName(sd.StorageNode.Name)
	testutil.MustNoErr(t, err, "source")
	dst, err := db.GetNodeByDotName(sd.LineNode.Name)
	testutil.MustNoErr(t, err, "dest")

	if _, err := d.dispatchToFleetCore(stale, src, dst); err == nil {
		t.Fatal("the handover accepted an ended order")
	}
	if n := len(backend.CreateRequests()); n != 0 {
		t.Fatalf("fleet saw %d create(s) for an order ended before its handover", n)
	}
}
