package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
)

// idle_cancel_core_release_test.go — what the Edge's row reads after Core
// releases a robot itself, and that the idle rule never sends that robot back.
//
// Core tells the Edge nothing at a release: every append ends in
// lifecycle.Release with no send (shingo-core dispatch.appendSegment), and the
// fleet's RUNNING after it is the same status. So after a Core-side release the
// row reads what Core's last order.staged left: staged, with that wait's kind.
// Core releases only a wait it owns (dispatch.coreOwnsWait): its hard release
// refuses a station's wait (TestHardReleaseIsScopedToCoreOwnedWaits), and the
// lane gate, the vacate release and the table turn advance only their own. Its
// order.staged names that owner (dispatch.stationPoint; TestStationPoint,
// TestStationPoint_VacateIsCoreOwned): lane at a lane wait, and vacate or turn
// when it takes the station's RELEASE and holds it (vacate_hold.go holdRelease).
// A staged notice at a Core wait carries no station ordinal, so it leaves the
// station's sent intent standing (release.Intent.OnStaged).

// coreStaged is Core's order.staged as messaging.EdgeHandler.HandleOrderStaged
// applies it: the staged reply, then the wait point.
func (l *idleLine) coreStaged(t *testing.T, uuid string, stationWait *int, kind string) {
	t.Helper()
	testutil.MustNoErr(t, l.eng.orderMgr.HandleDispatchReply(uuid, orders.ReplyStaged, "", "", "robot dwelling at staging node"), "staged reply")
	testutil.MustNoErr(t, l.eng.orderMgr.NoteStaged(uuid, stationWait, kind), "note the wait point")
}

// stationPress is the station's RELEASE of its first wait, as the release act sends
// it: the intent, then the send, which marks it sent and moves the row in
// transit at the click.
func (l *idleLine) stationPress(t *testing.T, id int64) {
	t.Helper()
	sw := 0
	testutil.MustNoErr(t, l.db.SetOrderReleaseIntent(id, `{"station_wait":0}`), "the press's intent")
	testutil.MustNoErr(t, l.eng.orderMgr.ReleaseOrder(id, nil, "operator", &sw), "the release")
}

// TestIdleCancel_ARobotCoreReleasedIsNotSentBack: one leg at a line silent two
// hours, driven through the messages Core sends for each of its own releases.
// After each the row reads staged with Core's kind, and the rule leaves it.
// The station's own wait, which Core never releases, is sent back.
//
// A leg the Edge learned was staged only from its boot reconcile carries no
// wait at all: Core's order.staged lives 10 minutes (protocol.FallbackTTL),
// and the reconcile's snapshot names no wait (protocol.OrderStatusSnapshot).
// Its first wait may be Core's lane wait, which Core releases when it chooses.
func TestIdleCancel_ARobotCoreReleasedIsNotSentBack(t *testing.T) {
	t.Parallel()
	zero := 0
	for _, tc := range []struct {
		name     string
		wire     func(t *testing.T, l *idleLine, id int64, uuid string)
		kind     string
		intent   bool // the station's sent RELEASE still on the row
		sentBack bool
	}{
		{
			name: "the lane gate releases its lane wait",
			wire: func(t *testing.T, l *idleLine, id int64, uuid string) {
				l.coreStaged(t, uuid, nil, protocol.WaitKindLane)
			},
			kind: protocol.WaitKindLane,
		},
		{
			name: "the vacate release, after Core took and held the station's RELEASE",
			wire: func(t *testing.T, l *idleLine, id int64, uuid string) {
				l.coreStaged(t, uuid, &zero, protocol.WaitKindStation)
				l.stationPress(t, id)
				l.coreStaged(t, uuid, nil, protocol.WaitKindVacate)
			},
			kind:   protocol.WaitKindVacate,
			intent: true,
		},
		{
			name: "the table turn's release, after Core took and held the station's RELEASE",
			wire: func(t *testing.T, l *idleLine, id int64, uuid string) {
				l.coreStaged(t, uuid, &zero, protocol.WaitKindStation)
				l.stationPress(t, id)
				l.coreStaged(t, uuid, nil, protocol.WaitKindTurn)
			},
			kind:   protocol.WaitKindTurn,
			intent: true,
		},
		{
			name: "Core's Orders page (hard release), at a lane wait: the only waits it releases are Core's",
			wire: func(t *testing.T, l *idleLine, id int64, uuid string) {
				l.coreStaged(t, uuid, nil, protocol.WaitKindLane)
			},
			kind: protocol.WaitKindLane,
		},
		{
			name: "the station's own wait, which only a person releases",
			wire: func(t *testing.T, l *idleLine, id int64, uuid string) {
				l.coreStaged(t, uuid, &zero, protocol.WaitKindStation)
			},
			kind:     protocol.WaitKindStation,
			sentBack: true,
		},
		{
			// It was sent back: a blank kind read as the station's.
			name: "staged only by the boot reconcile, at a wait it was never told of",
			wire: func(t *testing.T, l *idleLine, id int64, uuid string) {
				testutil.MustNoErr(t, l.eng.orderMgr.ApplyCoreStatusSnapshot(protocol.OrderStatusSnapshot{
					OrderUUID: uuid, Found: true, Status: string(protocol.StatusStaged),
				}), "the boot reconcile")
			},
			kind: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newIdleLine(t, 2*time.Hour)
			id, err := l.db.CreateOrder("core-release-leg", orders.TypeComplex, &l.nodeID, false, 1,
				"ALN_IDLE", "", "SYN_MARKET", protocol.Height{}, false, "PANEL-I", "", "")
			testutil.MustNoErr(t, err, "create the leg")
			_, err = l.db.DB.Exec(`UPDATE orders SET status=?, authored_by='edge', steps_json=? WHERE id=?`,
				string(protocol.StatusAcknowledged),
				`[{"action":"wait","node":"ALN_IDLE"},{"action":"pickup","node":"ALN_IDLE"},{"action":"dropoff","node":"SYN_MARKET"}]`, id)
			testutil.MustNoErr(t, err, "shape the leg")
			o, err := l.db.GetOrder(id)
			testutil.MustNoErr(t, err, "read the leg")
			testutil.MustNoErr(t, l.eng.orderMgr.HandleDispatchReply(o.UUID, orders.ReplyWaybill, "wb-1", "", ""), "the waybill")

			tc.wire(t, l, id, o.UUID)
			// Core releases it here, and sends nothing. The robot parked two
			// hours ago, at a line silent as long.
			l.parkedAgo(t, 2*time.Hour, id)

			o, err = l.db.GetOrder(id)
			testutil.MustNoErr(t, err, "read the leg after the release")
			if o.Status != protocol.StatusStaged || (o.ReleaseIntent != "") != tc.intent || o.WaitKind != tc.kind {
				t.Errorf("after the release the row reads %s, intent %q, kind %q; want staged, an intent %v, kind %q",
					o.Status, o.ReleaseIntent, o.WaitKind, tc.intent, tc.kind)
			}

			l.eng.idleCancelLines()

			got := l.status(t, id) == protocol.StatusCancelled
			if got != tc.sentBack {
				t.Errorf("sent back = %v, want %v", got, tc.sentBack)
			}
			if want := domain.ReorderHoldNone; !tc.sentBack && l.hold(t) != want {
				t.Errorf("the position holds %q, want nothing", l.hold(t))
			}
		})
	}
}
