package engine

import (
	"fmt"
	"testing"

	"shingo/protocol"
)

// TestOrderStaged_RecordsTheWaitPoint: Core's OrderStaged carries the station
// wait's number and kind (S3), and the Edge keeps them on the order. A staged
// push from a Core that predates them carries no kind and leaves the last
// recorded point alone.
func TestOrderStaged_RecordsTheWaitPoint(t *testing.T) {
	t.Parallel()
	h := newRelHarness(t)
	h.steadyPair(pairSpec{mode: protocol.SwapModeTwoRobot})
	uuid := h.order("evac").UUID
	point := func() string {
		o := h.order("evac")
		sw := "nil"
		if o.StationWait != nil {
			sw = fmt.Sprint(*o.StationWait)
		}
		return fmt.Sprintf("%s station_wait=%s wait_kind=%q", o.Status, sw, o.WaitKind)
	}
	one := 1
	for _, c := range []struct {
		name string
		msg  protocol.OrderStaged
		want string
	}{
		{"a station wait", protocol.OrderStaged{OrderUUID: uuid, StationWait: &one, WaitKind: protocol.WaitKindStation},
			`staged station_wait=1 wait_kind="station"`},
		{"an older Core: no kind, the point stands", protocol.OrderStaged{OrderUUID: uuid},
			`staged station_wait=1 wait_kind="station"`},
		{"a lane wait", protocol.OrderStaged{OrderUUID: uuid, WaitKind: protocol.WaitKindLane},
			`staged station_wait=nil wait_kind="lane"`},
	} {
		msg := c.msg
		h.handler.HandleOrderStaged(&protocol.Envelope{}, &msg)
		if got := point(); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
