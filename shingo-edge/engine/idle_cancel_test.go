package engine

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// idle_cancel_test.go — the one cancel rule (idle_cancel.go).

// idleLine is a counted line: process IDLE-PROC on counter PLC1/COUNT, read
// since boot, its last part ago before now, with one armed consume position
// at 0 (so the level pass asks whenever nothing holds it).
type idleLine struct {
	eng       *Engine
	db        *store.DB
	processID int64
	nodeID    int64
	styleID   int64
}

func newIdleLine(t *testing.T, ago time.Duration) *idleLine {
	t.Helper()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	procID, err := db.CreateProcess("IDLE-PROC", "", "PLC1", "COUNT", true)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: "ALN_IDLE", Code: "I1", Name: "ALN_IDLE", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	styleID, err := db.CreateStyle("IDLE-STYLE", "", procID)
	testutil.MustNoErr(t, err, "create style")
	testutil.MustNoErr(t, db.SetActiveStyle(procID, &styleID), "set active style")
	claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "ALN_IDLE", Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PANEL-I",
		UOPCapacity: 40, ReorderPoint: 25, AutoReorder: domain.Ptr(true),
		InboundSource: "SYN_MARKET", OutboundDestination: "SYN_MARKET",
		InboundStaging: "SLN_I05", OutboundStaging: "SLN_I06",
	})
	testutil.MustNoErr(t, err, "upsert claim")
	_, err = db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	setLevel(t, db, nodeID, claimID, 0)
	_, err = db.CreateReportingPoint("PLC1", "COUNT", styleID)
	testutil.MustNoErr(t, err, "create reporting point")
	l := &idleLine{eng: eng, db: db, processID: procID, nodeID: nodeID, styleID: styleID}
	l.lastPart(t, ago)
	eng.stationHealth.onRead("PLC1", "COUNT", "")
	bridgeCore(t, eng, headOccupancyStub(t, false).URL)
	holdPlaces(t, eng, everyAs(false))
	return l
}

// lastPart sets the line's last counted part ago before now (the engine's
// clock, as the poll stamps it).
func (l *idleLine) lastPart(t *testing.T, ago time.Duration) {
	t.Helper()
	_, err := l.db.DB.Exec(`UPDATE reporting_points SET last_poll_at=? WHERE plc_name='PLC1' AND tag_name='COUNT'`,
		time.Now().Add(-ago).UTC().Format("2006-01-02 15:04:05"))
	testutil.MustNoErr(t, err, "set the last part")
}

// leg is one order at the line's position, put in the shape a test names.
type leg struct {
	status   protocol.Status
	waitKind string
	steps    bool // an Edge-authored step plan (a plan-less staged leg is lane-held)
	core     bool // authored by Core
}

var parked = leg{status: protocol.StatusStaged, waitKind: protocol.WaitKindStation, steps: true}

// addLegs creates the legs at the position, linked as a pair when there are
// two, and returns their ids.
func (l *idleLine) addLegs(t *testing.T, legs ...leg) []int64 {
	t.Helper()
	var ids []int64
	for i, g := range legs {
		id, err := l.db.CreateOrder("idle-leg-"+string(rune('a'+i)), orders.TypeComplex, &l.nodeID, false, 1,
			"ALN_IDLE", "", "SYN_MARKET", protocol.Height{}, false, "PANEL-I", "", "")
		testutil.MustNoErr(t, err, "create a leg")
		steps, author := "", "edge"
		if g.steps {
			steps = `[{"action":"wait","node":"ALN_IDLE"}]`
		}
		if g.core {
			author = "core"
		}
		_, err = l.db.DB.Exec(`UPDATE orders SET status=?, wait_kind=?, steps_json=?, authored_by=? WHERE id=?`,
			string(g.status), g.waitKind, steps, author, id)
		testutil.MustNoErr(t, err, "shape the leg")
		ids = append(ids, id)
	}
	if len(ids) == 2 {
		_, err := l.db.DB.Exec(`UPDATE orders SET sibling_order_id = CASE id WHEN ? THEN ? ELSE ? END WHERE id IN (?, ?)`,
			ids[0], ids[1], ids[0], ids[0], ids[1])
		testutil.MustNoErr(t, err, "link the pair")
	}
	return ids
}

func (l *idleLine) status(t *testing.T, id int64) protocol.Status {
	t.Helper()
	o, err := l.db.GetOrder(id)
	testutil.MustNoErr(t, err, "read the order")
	return o.Status
}

func (l *idleLine) hold(t *testing.T) domain.ReorderHold {
	t.Helper()
	rt, err := l.db.GetProcessNodeRuntime(l.nodeID)
	testutil.MustNoErr(t, err, "read the runtime row")
	return rt.ReorderHold
}

// TestIdleCancel_TheHourSendsBackAParkedPairAndSwitchesOff: a line silent past
// its hour with both legs of a swap parked at a station wait. One demand pass
// cancels both with the rule's reason, sends Core a cancel that never expires
// for each, switches the position off, and the same pass's level half asks for
// nothing though the position is armed at 0 with nothing coming.
func TestIdleCancel_TheHourSendsBackAParkedPairAndSwitchesOff(t *testing.T) {
	t.Parallel()
	l := newIdleLine(t, 61*time.Minute)
	ids := l.addLegs(t, parked, parked)

	l.eng.reconcileDemand()

	for _, id := range ids {
		if got := l.status(t, id); got != protocol.StatusCancelled {
			t.Errorf("order %d is %s after the idle hour, want cancelled", id, got)
		}
	}
	if got := l.hold(t); got != domain.ReorderHoldOff {
		t.Errorf("the position holds %q, want off", got)
	}
	if got := countOrders(t, l.db); got != 2 {
		t.Errorf("orders after the pass = %d, want 2: the level half re-asked at a position the rule switched off", got)
	}
	rows, err := l.db.DB.Query(`SELECT payload FROM outbox WHERE msg_type=?`, protocol.TypeOrderCancel)
	testutil.MustNoErr(t, err, "read the outbox")
	defer rows.Close()
	var sent int
	for rows.Next() {
		var raw []byte
		testutil.MustNoErr(t, rows.Scan(&raw), "scan a cancel")
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(raw, &env), "decode a cancel")
		var p protocol.OrderCancel
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &p), "decode its payload")
		if p.Reason != protocol.CancelReasonLineIdle || !env.ExpiresAt.IsZero() {
			t.Errorf("cancel for %s: reason %q, exp %v; want %q and no expiry", p.OrderUUID, p.Reason, env.ExpiresAt, protocol.CancelReasonLineIdle)
		}
		sent++
	}
	if sent != 2 {
		t.Errorf("Core was sent %d cancels, want 2", sent)
	}
}

// TestIdleCancel_WhatItLeavesAlone: the rule cancels nothing on a line that
// has counted within its hour, nor a group with a leg that is not a robot
// parked at a station wait on a row this Edge wrote, nor while it cannot trust
// the hour.
func TestIdleCancel_WhatItLeavesAlone(t *testing.T) {
	t.Parallel()
	moving := leg{status: protocol.StatusInTransit, steps: true}
	for _, tc := range []struct {
		name  string
		ago   time.Duration
		legs  []leg
		setup func(t *testing.T, l *idleLine)
	}{
		{name: "the line counted 59 minutes ago", ago: 59 * time.Minute, legs: []leg{parked, parked}},
		{name: "the partner is moving", ago: 2 * time.Hour, legs: []leg{parked, moving}},
		{name: "no robot has reached it", ago: 2 * time.Hour, legs: []leg{{status: protocol.StatusQueued, steps: true}}},
		{name: "a leg Core wrote", ago: 2 * time.Hour, legs: []leg{parked, {status: protocol.StatusStaged, waitKind: protocol.WaitKindStation, steps: true, core: true}}},
		{name: "held at a lane gate", ago: 2 * time.Hour, legs: []leg{{status: protocol.StatusStaged}}},
		{name: "at a wait Core releases", ago: 2 * time.Hour, legs: []leg{{status: protocol.StatusStaged, waitKind: protocol.WaitKindVacate, steps: true}}},
		{name: "a changeover is open", ago: 2 * time.Hour, legs: []leg{parked}, setup: func(t *testing.T, l *idleLine) {
			_, err := l.db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, started_at) VALUES (?, ?, 'running', ?)`,
				l.processID, l.styleID, time.Now().UTC().Format("2006-01-02 15:04:05"))
			testutil.MustNoErr(t, err, "open a changeover")
		}},
		{name: "a changeover ended half an hour ago", ago: 2 * time.Hour, legs: []leg{parked}, setup: func(t *testing.T, l *idleLine) {
			at := time.Now().Add(-30 * time.Minute).UTC().Format("2006-01-02 15:04:05")
			_, err := l.db.DB.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, started_at, completed_at) VALUES (?, ?, 'completed', ?, ?)`,
				l.processID, l.styleID, at, at)
			testutil.MustNoErr(t, err, "end a changeover")
		}},
		{name: "someone said go half an hour ago", ago: 2 * time.Hour, legs: []leg{parked}, setup: func(t *testing.T, l *idleLine) {
			testutil.MustNoErr(t, processes.GoReorder(l.db.DB, l.nodeID, time.Now().Add(-30*time.Minute)), "go")
		}},
		{name: "the counter has failed since its last read", ago: 2 * time.Hour, legs: []leg{parked}, setup: func(t *testing.T, l *idleLine) {
			l.eng.stationHealth.onRead("PLC1", "COUNT", "not connected")
		}},
		{name: "the counter has not read since boot", ago: 2 * time.Hour, legs: []leg{parked}, setup: func(t *testing.T, l *idleLine) {
			l.eng.stationHealth = stationHealthTracker{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newIdleLine(t, tc.ago)
			ids := l.addLegs(t, tc.legs...)
			if tc.setup != nil {
				tc.setup(t, l)
			}
			l.eng.idleCancelLines()
			for _, id := range ids {
				if got := l.status(t, id); got == protocol.StatusCancelled {
					t.Errorf("order %d was cancelled", id)
				}
			}
			if got := l.hold(t); got != domain.ReorderHoldNone {
				t.Errorf("the position holds %q, want nothing", got)
			}
		})
	}
}

// TestIdleCancel_APartAfterTheStopEndsTheOff: the rule's off ends at the first
// part the line counts after the stop. An off an older Core wrote at a running
// line does not end with the line running on, only after a stop.
func TestIdleCancel_APartAfterTheStopEndsTheOff(t *testing.T) {
	t.Parallel()
	t.Run("the rule's own off", func(t *testing.T) {
		t.Parallel()
		l := newIdleLine(t, 61*time.Minute)
		l.addLegs(t, parked)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldOff {
			t.Fatalf("the position holds %q after the hour, want off", got)
		}
		l.lastPart(t, -time.Second) // a part, a second after now: after the off was set
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldNone {
			t.Errorf("the position holds %q after the line counted again, want nothing", got)
		}
	})
	t.Run("an older Core's off at a running line", func(t *testing.T) {
		t.Parallel()
		l := newIdleLine(t, time.Minute)
		_, err := processes.HoldReorder(l.db.DB, l.nodeID, domain.ReorderHoldOff, time.Now().Add(-time.Minute), "Core gave up")
		testutil.MustNoErr(t, err, "an older Core's off")
		l.eng.idleCancelLines()
		l.lastPart(t, -time.Second)
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldOff {
			t.Fatalf("the line ran on and the off became %q; only a stop and a restart, or a go, end it", got)
		}
		l.lastPart(t, 61*time.Minute) // the line stops
		l.eng.idleCancelLines()
		l.lastPart(t, -2*time.Second) // and starts again
		l.eng.idleCancelLines()
		if got := l.hold(t); got != domain.ReorderHoldNone {
			t.Errorf("after a stop and a restart the position holds %q, want nothing", got)
		}
	})
}
