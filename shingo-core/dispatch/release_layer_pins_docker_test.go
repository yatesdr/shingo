//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// release_layer_pins_docker_test.go — S0's Core pins for the release layer
// (SHAPE-release-layer §2 F2, §3.7; SYNTH-round3 §2 N-a and N3).
//
// Each cell drives HandleOrderRelease, the operator release, and reads back one
// short outcome: how many segments the fleet was handed, where wait_index and
// the status ended, and which order.error codes went to the Edge. The outcome is
// the whole answer on purpose. A release has three effects (the robot, the row,
// the Edge), and a pin that checks one of them passes while another moves.

// pinOutcome asserts one characterised outcome. While bug names a known defect,
// the cell holds today's outcome and says what it should be; the fix commit
// deletes the tag, and from then on the cell asserts want.
func pinOutcome(t *testing.T, bug, got, today, want string) {
	t.Helper()
	if bug != "" {
		if got != today {
			t.Fatalf("bug:%s characterisation moved: got %q, today was %q (want after the fix: %q)", bug, got, today, want)
		}
		t.Logf("bug:%s RED as expected: got %q, want %q", bug, got, want)
		return
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ── fixtures ────────────────────────────────────────────────────────────────

func rlWait(node, kind string) resolvedStep {
	return resolvedStep{Action: protocol.ActionWait, Node: node, WaitKind: kind}
}

// rlChangeoverLeg is a single_robot changeover leg's shape: wait at the press
// ("ready"), carry the old tooling's bin out, a bare station wait ("tooling
// done"), then bring the new bin in.
func rlChangeoverLeg() []resolvedStep {
	return []resolvedStep{
		rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-PRESS"), vsDrop("SYN-OUT-STAGING"),
		rlWait("", WaitKindStation), vsPick("SYN-IN-STAGING"), vsDrop("SYN-PRESS"),
	}
}

// rlLeg writes a fleet-committed, uncoordinated complex order in the given
// status at the given wait_index.
func rlLeg(t *testing.T, db *store.DB, uuid string, status protocol.Status, waitIndex int, stepsJSON string) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = uuid, "line-1", OrderTypeComplex, status
		o.DeliveryNode, o.StepsJSON = "SYN-PRESS", stepsJSON
	})
	testutil.MustNoErr(t, db.UpdateOrderVendor(o.ID, "V-"+uuid, "WAITING", ""), "vendor")
	if waitIndex != 0 {
		testutil.MustNoErr(t, db.UpdateOrderWaitIndex(o.ID, waitIndex), "wait_index")
	}
	return prReload(t, db, o.ID)
}

// orderErrorCodes lists the order.error codes enqueued for one order, in order.
func orderErrorCodes(t *testing.T, db *store.DB, uuid string) []string {
	t.Helper()
	msgs, err := db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "list outbox")
	var codes []string
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderError {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var p protocol.OrderError
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &p), "unmarshal order.error")
		if p.OrderUUID == uuid {
			codes = append(codes, p.ErrorCode)
		}
	}
	return codes
}

// releaseOutcome presses RELEASE once for uuid and reads back what it did.
func releaseOutcome(t *testing.T, db *store.DB, d *Dispatcher, calls func() int, uuid string) string {
	t.Helper()
	appendsBefore, errsBefore := calls(), len(orderErrorCodes(t, db, uuid))
	d.HandleOrderRelease(d.syntheticEnvelope("line-1"), &protocol.OrderRelease{OrderUUID: uuid})
	codes := orderErrorCodes(t, db, uuid)[errsBefore:]
	o, err := db.GetOrderByUUID(uuid)
	state := "row=none"
	if err == nil && o != nil {
		state = fmt.Sprintf("wait_index=%d status=%s", o.WaitIndex, o.Status)
	}
	return fmt.Sprintf("appends=%d %s errors=[%s]", calls()-appendsBefore, state, strings.Join(codes, ","))
}

func (r releaseRig) outcome(t *testing.T, uuid string) string {
	t.Helper()
	return releaseOutcome(t, r.db, r.d, func() int { return len(r.backend.ReleaseCalls()) }, uuid)
}

// ── N-a: a second release on an in_transit multi-wait order ─────────────────

// TestReleaseLayer_Na_SecondReleaseWhileDrivingToToolingDone is N-a's Core half.
//
// The changeover leg is released at "ready" and drives off with its old bin. It
// is in_transit, wait_index 1, heading for the bare "tooling done" wait. A
// second RELEASE now (the changeover sweep's re-click, or the pickup chain)
// appends the NEXT segment: the robot is handed "fetch the new bin and drop it
// on the press" before anyone has said the tooling is done. It should be a
// no-op: no append, wait_index unchanged, and no error the Edge would act on.
func TestReleaseLayer_Na_SecondReleaseWhileDrivingToToolingDone(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	rlLeg(t, r.db, "na-evac", StatusStaged, 0, string(mustJSON(t, rlChangeoverLeg())))

	pinOutcome(t, "", r.outcome(t, "na-evac"),
		"", "appends=1 wait_index=1 status=in_transit errors=[]")

	pinOutcome(t, "", r.outcome(t, "na-evac"),
		"appends=1 wait_index=2 status=in_transit errors=[]",
		"appends=0 wait_index=1 status=in_transit errors=[]")
}

// TestReleaseLayer_Na_Neighbours pins the releases the N-a fix must not move.
func TestReleaseLayer_Na_Neighbours(t *testing.T) {
	t.Parallel()
	laneThenStation := []resolvedStep{
		{Action: protocol.ActionWait, Node: "SYN-LANE-PT", WaitKind: WaitKindLane},
		vsPick("SYN-LANE-SLOT"), vsDrop("SYN-STAGE"),
		rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-STAGE"), vsDrop("SYN-PRESS"),
	}
	stationThenLane := []resolvedStep{
		rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-PRESS"),
		{Action: protocol.ActionWait, Node: "SYN-LANE-PT", WaitKind: WaitKindLane},
		vsDrop("SYN-LANE-SLOT"),
	}
	singleWait := []resolvedStep{rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-PRESS"), vsDrop("SYN-OUT")}
	for _, c := range []struct {
		name      string
		status    protocol.Status
		waitIndex int
		steps     []resolvedStep
		bug       string
		today     string
		want      string
	}{
		// The robot is parked at "tooling done" and the station releases it:
		// the legitimate second release.
		{"staged at wait 1 appends the next segment", StatusStaged, 1, rlChangeoverLeg(), "",
			"", "appends=1 wait_index=2 status=in_transit errors=[]"},
		// Driving to its FIRST wait, released on the way (the early-release
		// ruling, and the consolidated fan-out reaching a leg that has not
		// staged yet). The first segment goes, as today.
		{"in_transit at wait 0 releases the first wait early", StatusInTransit, 0, rlChangeoverLeg(), "",
			"", "appends=1 wait_index=1 status=in_transit errors=[]"},
		// Past its final wait: today's silent no-op.
		{"in_transit past the final wait is a no-op", StatusInTransit, 1, singleWait, "",
			"", "appends=0 wait_index=1 status=in_transit errors=[]"},
		// Core released a LANE wait, and the robot drives to the station's
		// first wait. The station has not pressed for this wait yet, so this is
		// an early release like wait 0 above, not a repeat.
		{"in_transit past a lane wait releases the station wait early", StatusInTransit, 1, laneThenStation, "",
			"", "appends=1 wait_index=2 status=in_transit errors=[]"},
		// The station's wait was released and the robot drives to a LANE wait.
		// Today the gate fence answers invalid_state, which rolls a moving Edge
		// leg back to staged. The press is a repeat of the one that already
		// went, so it is N-a's no-op.
		{"in_transit past a station wait toward a lane wait", StatusInTransit, 1, stationThenLane, "",
			"appends=0 wait_index=1 status=in_transit errors=[invalid_state]",
			"appends=0 wait_index=1 status=in_transit errors=[]"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			rlLeg(t, r.db, "nb-leg", c.status, c.waitIndex, string(mustJSON(t, c.steps)))
			pinOutcome(t, c.bug, r.outcome(t, "nb-leg"), c.today, c.want)
		})
	}
}

// ── N3: release-path errors and the code Core sends ─────────────────────────
//
// The Edge routes order.error by code (shingo-edge/messaging/edge_handler.go
// HandleOrderError): invalid_state rolls an in_transit leg back to staged with a
// "Core rejected the release" chip, manifest_sync_failed rolls back for retry,
// and every other code is a terminal Failed. A release-path error that is not a
// verdict on the order must not reach the Edge as a terminal code while Core
// keeps the order alive.

// landedBackend is the tracking backend with a hook that runs after a
// successful append: the fleet has the blocks, and whatever the hook does is
// what Core meets on the way out.
type landedBackend struct {
	*testdb.MockTrackingBackend
	after func()
}

func (b *landedBackend) ReleaseOrder(vendorOrderID string, blocks []fleet.OrderBlock, complete bool) error {
	if err := b.MockTrackingBackend.ReleaseOrder(vendorOrderID, blocks, complete); err != nil {
		return err
	}
	if b.after != nil {
		b.after()
	}
	return nil
}

func landedAudits(t *testing.T, db *store.DB, orderID int64) int {
	t.Helper()
	entries, err := db.ListEntityAudit("order", orderID)
	testutil.MustNoErr(t, err, "list audit")
	n := 0
	for _, e := range entries {
		if e.Action == "release_append_landed" {
			n++
		}
	}
	return n
}

func TestReleaseLayer_N3_ReleasePathErrorCodes(t *testing.T) {
	t.Parallel()
	single := func(t *testing.T) string {
		return string(mustJSON(t, []resolvedStep{rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-PRESS"), vsDrop("SYN-OUT")}))
	}

	// (a) The fleet refused the append. Nothing landed and the order is still
	// staged at Core, so the Edge must roll back, not fail.
	t.Run("a append did not land", func(t *testing.T) {
		t.Parallel()
		r := newReleaseRig(t)
		rlLeg(t, r.db, "n3a", StatusStaged, 0, single(t))
		r.backend.SetFail(true)
		pinOutcome(t, "", r.outcome(t, "n3a"),
			"appends=0 wait_index=0 status=staged errors=[fleet_failed]",
			"appends=0 wait_index=0 status=staged errors=[invalid_state]")
	})

	// (b) The fleet took the blocks, then the order faulted before Core could put
	// it in transit: AppendLandedError. The robot is driving the segment, so no
	// error goes to the Edge. Core logs it and records it on the order.
	t.Run("b append landed then Core's write failed", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		testdb.SetupStandardData(t, db)
		backend := &landedBackend{MockTrackingBackend: testdb.NewTrackingBackend()}
		d, _ := newTestDispatcher(t, db, backend)
		o := rlLeg(t, db, "n3b", StatusStaged, 0, single(t))
		backend.after = func() {
			mustExecDispatch(t, db, `UPDATE orders SET status=$1 WHERE id=$2`, string(StatusFaulted), o.ID)
		}
		got := releaseOutcome(t, db, d, func() int { return len(backend.ReleaseCalls()) }, "n3b")
		got += fmt.Sprintf(" audits=%d", landedAudits(t, db, o.ID))
		pinOutcome(t, "", got,
			"appends=1 wait_index=1 status=faulted errors=[fleet_failed] audits=0",
			"appends=1 wait_index=1 status=faulted errors=[] audits=1")
	})

	// (c) The stored plan does not parse. Nothing was appended; the order is
	// still staged. The chip, not Failed.
	t.Run("c unparseable steps", func(t *testing.T) {
		t.Parallel()
		r := newReleaseRig(t)
		rlLeg(t, r.db, "n3c", StatusStaged, 0, `{"not":"a plan"`)
		pinOutcome(t, "", r.outcome(t, "n3c"),
			"appends=0 wait_index=0 status=staged errors=[internal_error]",
			"appends=0 wait_index=0 status=staged errors=[invalid_state]")
	})

	// (d) Core has no such order (or not for this station). On the release path
	// that is the chip too (SHAPE §3.7).
	t.Run("d not found", func(t *testing.T) {
		t.Parallel()
		r := newReleaseRig(t)
		pinOutcome(t, "", r.outcome(t, "n3d-unknown"),
			"appends=0 row=none errors=[not_found]",
			"appends=0 row=none errors=[invalid_state]")
	})
}

// ── The pair death rule, both directions (swap_peer.go HandleSwapPeerTerminal) ──

// TestReleaseLayer_PairDeathRule pins "either leg dies, both die" on a
// two_robot clear-then-fill pair, from each side, with the rule's two
// exceptions: a SKIPPED evac (the line's bin was already gone, so the supply is
// the thing that should put one back) and an ABANDONED supply (the operator
// accepted the half-swap). A later step that deletes the Edge survivor arm
// relies on this holding at Core. GREEN, to be kept; the per-arm tests in
// swap_peer_test.go cover the press-index and half-completed shapes.
func TestReleaseLayer_PairDeathRule(t *testing.T) {
	t.Parallel()
	supplySteps := []resolvedStep{
		vsPick("SYN-SRC"), vsDrop("SYN-STAGE"), rlWait("SYN-STAGE", WaitKindStation),
		vsPick("SYN-STAGE"), vsDrop("SYN-LINE"),
	}
	evacSteps := []resolvedStep{rlWait("SYN-LINE", WaitKindStation), vsPick("SYN-LINE"), vsDrop("SYN-OUT")}
	for _, c := range []struct {
		name       string
		dead       string // "evac" or "supply"
		deadStatus protocol.Status
		kind       string
		peerStatus protocol.Status
		want       string
	}{
		{"evac fails, staged supply dies", "evac", StatusFailed, SwapTerminalFailed, StatusStaged, "peer=cancelled"},
		{"evac cancelled, in_transit supply dies", "evac", StatusCancelled, SwapTerminalCancelled, StatusInTransit, "peer=cancelled"},
		{"supply fails, staged evac dies", "supply", StatusFailed, SwapTerminalFailed, StatusStaged, "peer=cancelled"},
		{"supply cancelled, in_transit evac dies", "supply", StatusCancelled, SwapTerminalCancelled, StatusInTransit, "peer=cancelled"},
		{"exception: evac skipped, supply lives", "evac", StatusSkipped, SwapTerminalSkipped, StatusStaged, "peer=staged"},
		{"exception: supply abandoned, evac lives", "supply", StatusCancelled, SwapTerminalAbandoned, StatusStaged, "peer=staged"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			supplyStatus, evacStatus := c.peerStatus, c.deadStatus
			if c.dead == "supply" {
				supplyStatus, evacStatus = c.deadStatus, c.peerStatus
			}
			supply := vsStagedLeg(t, r.db, "dr-supply", "dr-evac", "SYN-LINE", supplyStatus, supplySteps)
			evac := vsStagedLeg(t, r.db, "dr-evac", "dr-supply", "SYN-OUT", evacStatus, evacSteps)
			for _, o := range []*orders.Order{supply, evac} {
				mustExecDispatch(t, r.db, `UPDATE orders SET process_node=$1 WHERE id=$2`, "SYN-LINE", o.ID)
			}
			dead, peer := evac, supply
			if c.dead == "supply" {
				dead, peer = supply, evac
			}
			r.d.HandleSwapPeerTerminal(dead.ID, c.kind)
			pinOutcome(t, "", "peer="+string(prReload(t, r.db, peer.ID).Status), "", c.want)
		})
	}
}
