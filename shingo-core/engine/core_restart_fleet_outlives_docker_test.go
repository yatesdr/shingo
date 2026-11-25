//go:build docker

package engine

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/notify"
)

// A CORE RESTART WITH AN ORDER IN FLIGHT, AGAINST A FLEET THAT OUTLIVES IT.
//
// At a plant the fleet (RDS) is its own process, and a Core restart leaves its
// missions running. The sim rig cannot show that: its fleet lives inside Core
// and dies with it. Here one simulator outlives the first engine: an order is
// dispatched and running, that engine stops, a second engine starts on the same
// database and the same fleet, and the mission finishes against the new Core.
// The restarted Core reloads the mission, the fleet still holds it, and the
// order lands its bin and confirms as if nothing had happened.
func TestCoreRestart_AnOrderInFlightFinishesAgainstTheFleetThatOutlivedIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := testdb.SetupTestData(t, db)
	bin := createTestBinAtNode(t, db, "PART-A", storageNode.ID, "RESTART-BIN")
	sim := &attachCountingFleet{SimulatorBackend: simulator.New()}

	first := newUnstartedEngine(t, db, sim)
	first.Start()
	env := testEnvelope()
	first.Dispatcher().HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{
		OrderUUID: "restart-move", Quantity: 1,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionPickup, Node: storageNode.Name},
			{Action: protocol.ActionDropoff, Node: lineNode.Name},
		},
	})
	before := testdb.RequireOrder(t, db, "restart-move")
	if before.VendorOrderID == "" || testdb.OnlyCart(t, before) == nil || *testdb.OnlyCart(t, before) != bin.ID {
		t.Fatalf("fixture: the order was not dispatched with its bin: vendor %q bin %v", before.VendorOrderID, testdb.OnlyCart(t, before))
	}
	sim.DriveState(before.VendorOrderID, "RUNNING")
	if o := testdb.RequireOrder(t, db, "restart-move"); o.Status != protocol.StatusInTransit {
		t.Fatalf("fixture: the order is %q before the restart, want in_transit", o.Status)
	}

	// Core goes down with the robot mid-mission; the fleet does not. The new
	// Core is handed the fleet that outlived the first: every check and every
	// drive below is through the fleet the new Core holds.
	first.Stop()
	fleetAfter := sim
	second := newTestEngine(t, db, fleetAfter)
	if !fleetAfter.HasOrder(before.VendorOrderID) {
		t.Fatalf("the fleet the new Core holds does not know mission %s", before.VendorOrderID)
	}
	if fleetAfter.attached != 2 {
		t.Fatalf("the fleet was attached %d time(s), want twice: the new Core did not take up the fleet that outlived the first", fleetAfter.attached)
	}
	if o := testdb.RequireOrder(t, db, "restart-move"); o.Status != protocol.StatusInTransit || testdb.OnlyCart(t, o) == nil || *testdb.OnlyCart(t, o) != bin.ID {
		t.Fatalf("after the restart the order is %q holding %v, want in_transit holding bin %d", o.Status, testdb.OnlyCart(t, o), bin.ID)
	}

	// The mission finishes against the new Core: the fleet's FINISHED alone
	// carries the order to delivered, before any receipt.
	fleetAfter.SimulatorBackend.DriveState(before.VendorOrderID, "FINISHED")
	if o := testdb.RequireOrder(t, db, "restart-move"); o.Status != protocol.StatusDelivered {
		t.Fatalf("after the fleet finished the mission the order is %q, want delivered: the new Core did not hear the fleet", o.Status)
	}
	second.Dispatcher().HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "restart-move", ReceiptType: "confirmed", FinalCount: 1})
	after := testdb.RequireOrder(t, db, "restart-move")
	if after.Status != protocol.StatusConfirmed {
		t.Errorf("after the restart and the fleet's finish the order is %q, want confirmed", after.Status)
	}
	testdb.RequireBinAtNode(t, db, bin.ID, lineNode.ID)
	got, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "bin")
	if got.ClaimedBy != nil {
		t.Errorf("the bin is still claimed by %d after its order confirmed", *got.ClaimedBy)
	}
}

// A CORE RESTART WITH A COMPOUND PARENT MID-DIG, the same fleet outliving it.
//
// The parent is `reshuffling`, its first leg is out with the blocker on the
// robot, and Core goes down. The new Core takes up the fleet and the dig: the
// leg finishes against it, the next leg goes, the parent finishes with its
// bin at the line, the lane is unlocked and no bin is left claimed.
func TestCoreRestart_ADigInFlightFinishesAgainstTheFleetThatOutlivedIt(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "RESTARTDIG", NumSlots: 3, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	sim := &attachCountingFleet{SimulatorBackend: simulator.New()}

	first := newUnstartedEngine(t, db, sim)
	first.Start()
	env := testEnvelope()
	first.Dispatcher().HandleOrderRequest(env, &protocol.OrderRequest{
		OrderUUID: "restart-dig", OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sc.Payload.Code,
		SourceNode: sc.Grp.Name, DeliveryNode: sc.LineNode.Name, Quantity: 1,
	})
	parent := testdb.RequireOrderStatus(t, db, "restart-dig", dispatch.StatusReshuffling)
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "the dig's legs")
	if len(children) < 2 {
		t.Fatalf("fixture: %d leg(s) under the dig, want 2 (unbury, retrieve)", len(children))
	}
	leg := children[0]
	if leg.VendorOrderID == "" {
		t.Fatalf("fixture: the first leg was not dispatched (%s)", leg.Status)
	}
	sim.DriveState(leg.VendorOrderID, "RUNNING")

	// Core goes down with the dig's first robot moving; the fleet does not.
	first.Stop()
	second := newTestEngine(t, db, sim)
	if sim.attached != 2 {
		t.Fatalf("the fleet was attached %d time(s), want twice: the new Core did not take up the fleet", sim.attached)
	}
	if !sim.HasOrder(leg.VendorOrderID) {
		t.Fatalf("the fleet the new Core holds does not know the leg's mission %s", leg.VendorOrderID)
	}
	if p := testdb.RequireOrder(t, db, "restart-dig"); p.Status != dispatch.StatusReshuffling {
		t.Fatalf("after the restart the parent is %q, want reshuffling", p.Status)
	}

	// The dig finishes against the new Core, leg by leg.
	for _, c := range children {
		c, err := db.GetOrder(c.ID)
		testutil.MustNoErr(t, err, "leg")
		if c.VendorOrderID == "" {
			t.Fatalf("leg %d (seq %d) was never dispatched after the restart (%s, %q)", c.ID, c.Sequence, c.Status, c.QueueReason)
		}
		driveLegRunToFinish(t, db, sim.SimulatorBackend, c.ID, c.VendorOrderID)
		second.Dispatcher().HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: c.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
	}

	testdb.RequireBinAtNode(t, db, sc.TargetBin.ID, sc.LineNode.ID)
	for _, id := range []int64{sc.TargetBin.ID, sc.Blockers[0].ID} {
		b, err := db.GetBin(id)
		testutil.MustNoErr(t, err, "bin")
		if b.ClaimedBy != nil {
			t.Errorf("bin %d is still claimed by %d after the dig", id, *b.ClaimedBy)
		}
	}
	if second.Dispatcher().LaneLock().IsLocked(sc.Lane.ID) {
		t.Errorf("lane %d is still locked after the dig finished against the new Core", sc.Lane.ID)
	}
	if p := testdb.RequireOrder(t, db, "restart-dig"); p.Status != protocol.StatusConfirmed {
		t.Errorf("the parent is %q (%s), want confirmed: its last leg carried its bin to the line", p.Status, p.ErrorDetail)
	}
}

// attachCountingFleet is the simulator, counting the engines that attach to it
// (InitTracker): a Core takes up a fleet by attaching its tracker, and a
// restarted Core attaches to the fleet that outlived the first.
type attachCountingFleet struct {
	*simulator.SimulatorBackend
	attached int
}

func (f *attachCountingFleet) InitTracker(emitter fleet.TrackerEmitter, resolver fleet.OrderIDResolver) {
	f.attached++
	f.SimulatorBackend.InitTracker(emitter, resolver)
}

// A CORE RESTART WITH A FAULT STILL INSIDE ITS ALERT BUFFER (docket B17).
//
// A fault is mailed once it has lasted three minutes, and its "cleared" is
// mailed as a reply to that alert. Both rest on memory: the pending alert is a
// timer and the sent alert is a map entry, each a local of wireEventHandlers.
// A Core that boots with the order already `faulted` never hears the fault
// event again (the fleet's FAILED maps to the status the order already has),
// so it arms nothing.
//
// One simulator outlives the first engine, as above. The fault is seconds old
// when the first Core stops, so no alert has gone, which is right. The fault's
// start is durable (its order_history row, the instant the board's fault clock
// reads): the test moves that row back four minutes, so the fault is past its
// buffer when the second Core boots. That Core owes the alert, and the fault's
// end then owes a "cleared" answering it. A failure alert sent through each
// Core first shows its mail reaches the catcher.
//
// RED today at both: the second Core mails no fault alert, and the cleared has
// no alert to answer (the second half is the hole engine/wiring.go records as
// accepted on 2026-09-24). Skipped unless SHINGO_KNOWN_RED=1: no fix in this
// brief; a fix that survives the restart needs a durable record of the alert
// sent, which is a column.
func TestCoreRestart_AFaultInsideItsAlertBufferIsStillMailed(t *testing.T) {
	if os.Getenv("SHINGO_KNOWN_RED") == "" {
		t.Skip("docket B17 red pin, red until a restarted Core arms the fault alert from the faulted row; run with SHINGO_KNOWN_RED=1")
	}
	t.Parallel()
	mail := catchMail(t)
	mailOn := func(c *config.Config) {
		c.Notifications = config.NotificationsConfig{
			Enabled: true, SMTPHost: "127.0.0.1", SMTPPort: mail.port,
			FromAddress: "core@example.invalid", Recipients: []string{"ops@example.invalid"},
		}
	}
	// mailWorks sends one failure alert through eng (it goes at once) and waits
	// for it: n is how many the catcher holds once it has arrived.
	mailWorks := func(eng *Engine, n int) {
		t.Helper()
		eng.Events.Emit(Event{Type: EventOrderFailed, Payload: OrderFailedEvent{OrderID: 1 << 40, ErrorCode: "pin"}})
		testutil.EventuallyWithInterval(t, 10*time.Millisecond, 10*time.Second, func() bool {
			return len(mail.subjects(notify.FailSubject(""))) == n
		})
	}

	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "BIN-RESTART-FAULT")
	sim := simulator.New()

	first := newUnstartedEngineWith(t, db, sim, mailOn)
	first.Start()
	first.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "restart-fault", OrderType: dispatch.OrderTypeRetrieve, PayloadCode: sd.Payload.Code,
		DeliveryNode: sd.LineNode.Name, Quantity: 1,
	})
	order := testdb.RequireOrderStatus(t, db, "restart-fault", dispatch.StatusDispatched)
	sim.DriveStateWithRobot(order.VendorOrderID, "RUNNING", "AMB-01")
	sim.DriveState(order.VendorOrderID, "FAILED")
	testdb.RequireOrderStatus(t, db, "restart-fault", dispatch.StatusFaulted)
	mailWorks(first, 1)

	// Core goes down with the fault seconds old; the fleet does not.
	first.Stop()
	alert := notify.FaultSubject("AMB-01")
	if got := mail.subjects(alert); len(got) != 0 {
		t.Fatalf("fixture: %d fault alert(s) before the restart, want none: the fault is seconds old", len(got))
	}
	res, err := db.Exec(`UPDATE order_history SET created_at = created_at - interval '4 minutes'
		WHERE order_id = $1 AND status = $2`, order.ID, dispatch.StatusFaulted)
	testutil.MustNoErr(t, err, "age the fault past its buffer")
	n, err := res.RowsAffected()
	testutil.MustNoErr(t, err, "count the aged rows")
	if n != 1 {
		t.Fatalf("fixture: aged %d faulted history row(s) of order %d, want 1", n, order.ID)
	}

	second := newUnstartedEngineWith(t, db, sim, mailOn)
	second.Start()
	t.Cleanup(func() { second.Stop() })
	select {
	case <-second.bootScanned:
	case <-time.After(30 * time.Second):
		t.Fatal("the second engine's boot scan did not finish in 30 s")
	}
	testdb.RequireOrderStatus(t, db, "restart-fault", dispatch.StatusFaulted)
	mailWorks(second, 2)

	alerted := testutil.AssertEventually(t, 20*time.Millisecond, 3*time.Second, func() bool { return len(mail.subjects(alert)) == 1 })
	if !alerted {
		t.Errorf("the restarted Core mailed %d fault alert(s) for order %d, want 1: it has been faulted four minutes by its "+
			"history row and nobody was told. The pending alert was a timer in the stopped Core's memory "+
			"(wireEventHandlers, faultTimers): arm it at boot from the faulted row", len(mail.subjects(alert)), order.ID)
	}

	// The fault ends under the new Core.
	sim.DriveState(order.VendorOrderID, "RUNNING")
	testdb.RequireOrderStatus(t, db, "restart-fault", dispatch.StatusInTransit)
	cleared := notify.ReplySubject(alert)
	if !testutil.AssertEventually(t, 20*time.Millisecond, 3*time.Second, func() bool { return len(mail.subjects(cleared)) == 1 }) {
		t.Errorf("the fault cleared and %d mail(s) answered its alert, want 1: the cleared has no alert to answer "+
			"(wireEventHandlers, faultSent)", len(mail.subjects(cleared)))
	} else if alerted {
		id := header(mail.subjects(alert)[0], "Message-ID")
		if got := header(mail.subjects(cleared)[0], "In-Reply-To"); id == "" || got != id {
			t.Errorf("the cleared replies to %q, want the alert's Message-ID %q", got, id)
		}
	}
}

// mailCatcher is a mail server that takes every message it is sent and keeps
// it, headers and body, for a test to read.
type mailCatcher struct {
	port int
	mu   sync.Mutex
	kept []string
}

// catchMail starts a mailCatcher on a free local port, closed with the test.
func catchMail(t *testing.T) *mailCatcher {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	testutil.MustNoErr(t, err, "mail catcher")
	t.Cleanup(func() { ln.Close() })
	m := &mailCatcher{port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go m.serve(c)
		}
	}()
	return m
}

// serve speaks the server's half of one SMTP conversation: yes to every
// command, and the DATA block kept.
func (m *mailCatcher) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(line string) { fmt.Fprintf(c, "%s\r\n", line) }
	say("220 catcher")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case cmd == "DATA":
			say("354 go on")
			var msg strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				msg.WriteString(l)
			}
			m.mu.Lock()
			m.kept = append(m.kept, msg.String())
			m.mu.Unlock()
			say("250 kept")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// subjects is the kept messages whose Subject is exactly subject.
func (m *mailCatcher) subjects(subject string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.kept {
		if header(msg, "Subject") == subject {
			out = append(out, msg)
		}
	}
	return out
}

// header is the value of a message's header line, "" when it has none.
func header(msg, name string) string {
	head, _, _ := strings.Cut(msg, "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if v, ok := strings.CutPrefix(line, name+": "); ok {
			return v
		}
	}
	return ""
}
