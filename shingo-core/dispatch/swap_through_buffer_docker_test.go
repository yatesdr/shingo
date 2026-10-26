//go:build docker

package dispatch

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// swap_through_buffer_docker_test.go — a swap's supply picks its bin knowing
// where the returning carrier lands (returnLiftFrom, plan.Need.LiftFrom).
//
// Hopkinsville 4x2, 2026-10-26: a changeover from one part to another at a
// dedicated loader with a pinned home per part and two buffers. FIFO lifted the
// incoming part's oldest cart from its pinned home; that home will not take the
// outgoing part, the outgoing part's home and both buffers were full, and the
// pair waited until it was cancelled. The incoming part's other cart stood in a
// buffer, and lifting that one gives the returning cart its spot.

// stbLoader is a dedicated consume loader with two pinned homes, one per part,
// and two buffers.
type stbLoader struct {
	homeIn, homeOut, buf1, buf2 *nodes.Node
}

func stbFixture(t *testing.T, db *store.DB) stbLoader {
	t.Helper()
	setupTestData(t, db)
	for _, code := range []string{"PART-IN", "PART-OUT", "PART-Z"} {
		testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: code, Description: code, UOPCapacity: 100}), "payload "+code)
	}
	l := stbLoader{
		homeIn: prNode(t, db, "STB-HOME-IN"), homeOut: prNode(t, db, "STB-HOME-OUT"),
		buf1: prNode(t, db, "STB-BUF-1"), buf2: prNode(t, db, "STB-BUF-2"),
	}
	loaderID, err := db.CreateLoader(store.Loader{Name: "STB", Role: "consume", Layout: "dedicated_positions", Replenishment: "operator"})
	testutil.MustNoErr(t, err, "create loader")
	for _, h := range []store.LoaderHome{
		{LoaderID: loaderID, PositionNodeID: l.homeIn.ID, PayloadCode: "PART-IN", Kind: loaders.HomeKindHome},
		{LoaderID: loaderID, PositionNodeID: l.homeOut.ID, PayloadCode: "PART-OUT", Kind: loaders.HomeKindHome},
		{LoaderID: loaderID, PositionNodeID: l.buf1.ID, Kind: loaders.HomeKindBuffer},
		{LoaderID: loaderID, PositionNodeID: l.buf2.ID, Kind: loaders.HomeKindBuffer},
	} {
		testutil.MustNoErr(t, db.UpsertLoaderHome(h), "loader member")
	}
	return l
}

// stbSubmit sends a changeover-shaped pair through the real intake: the supply
// fetches incoming from its home (the anchor), stages, waits, delivers to the
// line; the return lifts the line's carrier and takes it to returnTo.
func stbSubmit(d *Dispatcher, prefix, incoming, outgoing, line, stage, supplySrc, returnTo string) {
	prSubmitLeg(d, prefix+"-supply", prefix+"-return", incoming, line,
		prPick(supplySrc), prDropExcl(stage), prWait(stage), prPick(stage), prDrop(line))
	prSubmitLeg(d, prefix+"-return", prefix+"-supply", outgoing, line,
		prWait(line), prPick(line), prDrop(returnTo))
}

// stbRecorded counts the swap_through_buffer records written for an order.
func stbRecorded(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM recovery_actions
		WHERE action='swap_through_buffer' AND target_type='order' AND target_id=$1`, orderID).Scan(&n), "count records")
	return n
}

// THE INCIDENT: the incoming part's cart in a buffer is taken over its older
// cart in the pinned home, and the returning carrier lands on that buffer.
func TestSwapThroughBuffer_ChangeoverTakesTheBufferCart(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	l := stbFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "STB1-LINE"), prNode(t, db, "STB1-STAGE")
	now := time.Now().UTC()
	homeCart := makeLoaderBin(t, db, "PART-IN", l.homeIn.ID, "stb1-in-home", 80, now.Add(-72*time.Hour))
	bufCart := makeLoaderBin(t, db, "PART-IN", l.buf1.ID, "stb1-in-buffer", 32, now.Add(-48*time.Hour))
	makeLoaderBin(t, db, "PART-OUT", l.homeOut.ID, "stb1-out-spare", 90, now.Add(-24*time.Hour))
	makeLoaderBin(t, db, "PART-Z", l.buf2.ID, "stb1-other-partial", 40, now.Add(-24*time.Hour))
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")
	prResident(t, db, line, bt.ID, "PART-OUT", "stb1-outgoing")

	stbSubmit(d, "stb1", "PART-IN", "PART-OUT", line.Name, stage.Name, l.homeIn.Name, l.homeOut.Name)
	prScanPass(t, d, db, "stb1-return", "stb1-supply")

	supply, ret := prReloadUUID(t, db, "stb1-supply"), prReloadUUID(t, db, "stb1-return")
	if supply.VendorOrderID == "" || ret.VendorOrderID == "" {
		t.Fatalf("the pair did not go: supply %s cause=%q reason=%q, return %s cause=%q reason=%q",
			supply.Status, supply.QueueCause, supply.QueueReason, ret.Status, ret.QueueCause, ret.QueueReason)
	}
	if supply.SourceNode != l.buf1.Name {
		t.Errorf("supply lifts %s, want the buffer cart at %s", supply.SourceNode, l.buf1.Name)
	}
	if ret.DeliveryNode != l.buf1.Name {
		t.Errorf("return lands %s, want the buffer %s its supply empties", ret.DeliveryNode, l.buf1.Name)
	}
	if fd := vsFinalDrop(t, ret); fd.Vacate == nil || fd.Vacate.Bin != bufCart.ID || fd.Vacate.Partner != supply.ID {
		t.Errorf("return's drop stamp = %+v, want bin %d lifted first by partner %d", fd.Vacate, bufCart.ID, supply.ID)
	}
	vsBinAt(t, db, homeCart.ID, l.homeIn.ID, nil, "the older home cart is passed over")
	if n := stbRecorded(t, db, supply.ID); n != 1 {
		t.Errorf("swap_through_buffer records for the supply = %d, want 1", n)
	}
}

// A return with room keeps plain FIFO: a free buffer takes the returning
// carrier, and the supply lifts the incoming part's oldest cart from its home.
func TestSwapThroughBuffer_FIFOWhenTheReturnHasRoom(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	l := stbFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "STB2-LINE"), prNode(t, db, "STB2-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-IN", l.homeIn.ID, "stb2-in-home", 80, now.Add(-72*time.Hour))
	makeLoaderBin(t, db, "PART-IN", l.buf1.ID, "stb2-in-buffer", 32, now.Add(-48*time.Hour))
	makeLoaderBin(t, db, "PART-OUT", l.homeOut.ID, "stb2-out-spare", 90, now.Add(-24*time.Hour))
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")
	prResident(t, db, line, bt.ID, "PART-OUT", "stb2-outgoing")

	stbSubmit(d, "stb2", "PART-IN", "PART-OUT", line.Name, stage.Name, l.homeIn.Name, l.homeOut.Name)
	prScanPass(t, d, db, "stb2-return", "stb2-supply")

	supply, ret := prReloadUUID(t, db, "stb2-supply"), prReloadUUID(t, db, "stb2-return")
	if supply.VendorOrderID == "" || ret.VendorOrderID == "" {
		t.Fatalf("the pair did not go: supply cause=%q, return cause=%q", supply.QueueCause, ret.QueueCause)
	}
	if supply.SourceNode != l.homeIn.Name {
		t.Errorf("supply lifts %s, want FIFO's oldest cart at %s", supply.SourceNode, l.homeIn.Name)
	}
	if ret.DeliveryNode != l.buf2.Name {
		t.Errorf("return lands %s, want the free buffer %s", ret.DeliveryNode, l.buf2.Name)
	}
	if n := stbRecorded(t, db, supply.ID); n != 0 {
		t.Errorf("swap_through_buffer records = %d, want none: FIFO's pick stood", n)
	}
}

// A steady-state swap is unchanged: the returning carrier is the same part, so
// its own home is a spot that takes it, and FIFO's pick there stands.
func TestSwapThroughBuffer_SteadyStateSwapUnchanged(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	l := stbFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "STB3-LINE"), prNode(t, db, "STB3-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-IN", l.homeIn.ID, "stb3-in-home", 80, now.Add(-72*time.Hour))
	makeLoaderBin(t, db, "PART-IN", l.buf1.ID, "stb3-in-buffer", 32, now.Add(-48*time.Hour))
	makeLoaderBin(t, db, "PART-Z", l.buf2.ID, "stb3-other-partial", 40, now.Add(-24*time.Hour))
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")
	prResident(t, db, line, bt.ID, "PART-IN", "stb3-spent")

	stbSubmit(d, "stb3", "PART-IN", "PART-IN", line.Name, stage.Name, l.homeIn.Name, l.homeIn.Name)
	prScanPass(t, d, db, "stb3-return", "stb3-supply")

	supply, ret := prReloadUUID(t, db, "stb3-supply"), prReloadUUID(t, db, "stb3-return")
	if supply.VendorOrderID == "" || ret.VendorOrderID == "" {
		t.Fatalf("the pair did not go: supply cause=%q, return cause=%q", supply.QueueCause, ret.QueueCause)
	}
	if supply.SourceNode != l.homeIn.Name || ret.DeliveryNode != l.homeIn.Name {
		t.Errorf("supply lifts %s and return lands %s, want both at its home %s (unchanged)",
			supply.SourceNode, ret.DeliveryNode, l.homeIn.Name)
	}
	if n := stbRecorded(t, db, supply.ID); n != 0 {
		t.Errorf("swap_through_buffer records = %d, want none", n)
	}
}

// No cart of the incoming part on a spot that takes the returning carrier:
// the return waits, as before, and its sentence names the spots to empty.
func TestSwapThroughBuffer_NoBufferCartWaitsAndSaysWhatToEmpty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	l := stbFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	line, stage := prNode(t, db, "STB4-LINE"), prNode(t, db, "STB4-STAGE")
	now := time.Now().UTC()
	makeLoaderBin(t, db, "PART-IN", l.homeIn.ID, "stb4-in-home", 80, now.Add(-72*time.Hour))
	makeLoaderBin(t, db, "PART-OUT", l.homeOut.ID, "stb4-out-spare", 90, now.Add(-24*time.Hour))
	makeLoaderBin(t, db, "PART-Z", l.buf1.ID, "stb4-z-1", 40, now.Add(-24*time.Hour))
	makeLoaderBin(t, db, "PART-Z", l.buf2.ID, "stb4-z-2", 40, now.Add(-24*time.Hour))
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")
	prResident(t, db, line, bt.ID, "PART-OUT", "stb4-outgoing")

	stbSubmit(d, "stb4", "PART-IN", "PART-OUT", line.Name, stage.Name, l.homeIn.Name, l.homeOut.Name)
	prScanPass(t, d, db, "stb4-return", "stb4-supply")

	supply, ret := prReloadUUID(t, db, "stb4-supply"), prReloadUUID(t, db, "stb4-return")
	if supply.VendorOrderID != "" || ret.VendorOrderID != "" {
		t.Fatalf("the pair went with nowhere for the return to land: supply %q, return %q",
			supply.VendorOrderID, ret.VendorOrderID)
	}
	if ret.QueueCause != string(CauseLoaderParkNoSlot) {
		t.Errorf("return waits on %q, want %q", ret.QueueCause, CauseLoaderParkNoSlot)
	}
	want := "empty one of " + strings.Join([]string{l.homeOut.Name, l.buf1.Name, l.buf2.Name}, ", ") + " and it goes"
	if !strings.Contains(ret.QueueReason, want) {
		t.Errorf("return's sentence = %q, want it to say %q", ret.QueueReason, want)
	}
	if n := stbRecorded(t, db, supply.ID); n != 0 {
		t.Errorf("swap_through_buffer records = %d, want none: nothing was swapped", n)
	}
}
