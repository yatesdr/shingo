//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
)

// release_echo_docker_test.go — the echo (S3). An Edge that sends
// OrderRelease.StationWait names the station wait the release is for; Core
// applies it only when the order's wait_index points at that station wait,
// parked there or driving to it. Anything else does nothing, with no error;
// a staged order is re-announced (OrderStaged) at the wait Core holds.

// stagedReplies lists the OrderStaged re-announcements enqueued for one order,
// as "station:N" or "lane".
func stagedReplies(t *testing.T, db *store.DB, uuid string) []string {
	t.Helper()
	msgs, err := db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "list outbox")
	var out []string
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderStaged {
			continue
		}
		var env protocol.Envelope
		testutil.MustNoErr(t, json.Unmarshal(m.Payload, &env), "unmarshal envelope")
		var p protocol.OrderStaged
		testutil.MustNoErr(t, json.Unmarshal(env.Payload, &p), "unmarshal order.staged")
		if p.OrderUUID != uuid {
			continue
		}
		if p.StationWait != nil {
			out = append(out, fmt.Sprintf("%s:%d", p.WaitKind, *p.StationWait))
		} else {
			out = append(out, p.WaitKind)
		}
	}
	return out
}

// echoOutcome presses RELEASE once with the given echo and reads back what it
// did, including any OrderStaged re-announcement.
func (r releaseRig) echoOutcome(t *testing.T, uuid string, echo int) string {
	t.Helper()
	appendsBefore := len(r.backend.ReleaseCalls())
	errsBefore := len(orderErrorCodes(t, r.db, uuid))
	stagedBefore := len(stagedReplies(t, r.db, uuid))
	r.d.HandleOrderRelease(r.d.syntheticEnvelope("line-1"), &protocol.OrderRelease{OrderUUID: uuid, StationWait: &echo})
	o, err := r.db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "reload")
	return fmt.Sprintf("appends=%d wait_index=%d status=%s errors=[%s] restaged=[%s]",
		len(r.backend.ReleaseCalls())-appendsBefore, o.WaitIndex, o.Status,
		strings.Join(orderErrorCodes(t, r.db, uuid)[errsBefore:], ","),
		strings.Join(stagedReplies(t, r.db, uuid)[stagedBefore:], ","))
}

func TestReleaseEcho(t *testing.T) {
	t.Parallel()
	laneThenStation := []resolvedStep{
		{Action: protocol.ActionWait, Node: "SYN-LANE-PT", WaitKind: WaitKindLane},
		vsPick("SYN-LANE-SLOT"), vsDrop("SYN-STAGE"),
		rlWait("SYN-PRESS", WaitKindStation), vsPick("SYN-STAGE"), vsDrop("SYN-PRESS"),
	}
	for _, c := range []struct {
		name      string
		status    protocol.Status
		waitIndex int
		steps     []resolvedStep
		echo      int
		want      string
	}{
		{"staged at its first station wait, echo 0: released", StatusStaged, 0, rlChangeoverLeg(), 0,
			"appends=1 wait_index=1 status=in_transit errors=[] restaged=[]"},
		{"staged at tooling done, echo 1: released", StatusStaged, 1, rlChangeoverLeg(), 1,
			"appends=1 wait_index=2 status=in_transit errors=[] restaged=[]"},
		{"driving to its first wait, echo 0: released early", StatusInTransit, 0, rlChangeoverLeg(), 0,
			"appends=1 wait_index=1 status=in_transit errors=[] restaged=[]"},
		// N-a: released at "ready", driving to "tooling done", and pressed again
		// for the wait it already left.
		{"driving to tooling done, echo 0 (a repeat): no-op", StatusInTransit, 1, rlChangeoverLeg(), 0,
			"appends=0 wait_index=1 status=in_transit errors=[] restaged=[]"},
		// The Edge names a wait Core has left: nothing goes, and the Edge is told
		// where the robot is.
		{"staged at tooling done, echo 0: no-op, re-staged at 1", StatusStaged, 1, rlChangeoverLeg(), 0,
			"appends=0 wait_index=1 status=staged errors=[] restaged=[station:1]"},
		// Core released its own lane wait first; the station's first wait is
		// wait_index 1 and station wait 0.
		{"past a lane wait, driving to station wait 0: released early", StatusInTransit, 1, laneThenStation, 0,
			"appends=1 wait_index=2 status=in_transit errors=[] restaged=[]"},
		{"staged at a lane wait, echo 0: no-op, re-staged at the lane wait", StatusStaged, 0, laneThenStation, 0,
			"appends=0 wait_index=0 status=staged errors=[] restaged=[lane]"},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newReleaseRig(t)
			rlLeg(t, r.db, "echo-leg", c.status, c.waitIndex, string(mustJSON(t, c.steps)))
			if got := r.echoOutcome(t, "echo-leg", c.echo); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}
