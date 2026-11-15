//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/domain"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestRobotPositions_NoLeaseNoWork pins the feed's cost on the robot poll by
// counting real statements (store.OpenCounting, pgx's tracer) and real
// publishes: with no live watch a poll reads nothing and sends nothing, with
// no publisher wired (no verified topic) a watch costs nothing either; with
// live watches each poll is ONE statement whatever the station count and one
// message per watching station; a lapsed lease is nothing again.
func TestRobotPositions_NoLeaseNoWork(t *testing.T) {
	t.Parallel()
	db, dbCfg := testdb.OpenWithConfig(t)
	for _, o := range []struct{ uuid, station, robot string }{
		{"u-a1", "edge.a", "AMR-01"}, {"u-b1", "edge.b", "AMR-02"},
	} {
		order := &domain.Order{EdgeUUID: o.uuid, StationID: o.station, OrderType: "move",
			Status: protocol.StatusInTransit, Quantity: 1, SourceNode: "S", DeliveryNode: "D"}
		testutil.MustNoErr(t, db.CreateOrder(order), "create "+o.uuid)
		testutil.MustNoErr(t, db.UpdateOrderVendor(order.ID, "v-"+o.uuid, "RUNNING", o.robot), "robot on "+o.uuid)
	}
	cdb, counter, err := store.OpenCounting(dbCfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })

	cfg := config.Defaults()
	cfg.Messaging.StationID = "core"
	robots := []fleet.RobotStatus{
		{VehicleID: "AMR-01", Connected: true, X: 1, Y: 2, Path: []string{"LM7", "LM31"}},
		{VehicleID: "AMR-02", Connected: true, X: 3, Y: 4},
	}
	watchOn := func(e *Engine, station string, uuids ...string) {
		e.HandleRobotWatch(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: station}},
			&protocol.RobotWatch{StationID: station, OrderUUIDs: uuids})
	}

	// No publisher wired (no verified topic): a watch arrives, and the poll
	// still does nothing.
	unwired := &Engine{db: cdb, cfg: cfg}
	watchOn(unwired, "edge.a", "u-a1")
	counter.Reset()
	for range 3 {
		unwired.publishRobotPositions(robots, time.Now())
	}
	if n := counter.Count(); n != 0 {
		t.Fatalf("with no verified topic, three polls ran %d statements, want 0", n)
	}

	e := &Engine{db: cdb, cfg: cfg}
	var sent []*protocol.Envelope
	e.SetRobotPositionsPublisher(func(env *protocol.Envelope) error { sent = append(sent, env); return nil })
	watch := func(station string, uuids ...string) { watchOn(e, station, uuids...) }
	poll := func(n int, at time.Time) {
		for range n {
			e.publishRobotPositions(robots, at)
		}
	}

	counter.Reset()
	poll(10, time.Now())
	if n := counter.Count(); n != 0 || len(sent) != 0 {
		t.Fatalf("no lease: ten polls ran %d statements and sent %d messages, want 0 and 0", n, len(sent))
	}

	watch("edge.a", "u-a1", "u-b1")
	counter.Reset()
	poll(3, time.Now())
	if n := counter.Count(); n != 3 || len(sent) != 3 {
		t.Fatalf("one lease: three polls ran %d statements and sent %d messages, want 3 and 3", n, len(sent))
	}
	var body protocol.RobotPositions
	testutil.MustNoErr(t, sent[2].DecodePayload(&body), "decode")
	if sent[2].Dst.Station != "edge.a" || len(body.Orders) != 1 || body.Orders[0].OrderUUID != "u-a1" ||
		body.Orders[0].RobotID != "AMR-01" {
		t.Errorf("edge.a was sent %+v to %q, want u-a1 on AMR-01 alone (u-b1 is edge.b's)", body.Orders, sent[2].Dst.Station)
	}

	watch("edge.b", "u-b1")
	counter.Reset()
	sent = nil
	poll(1, time.Now())
	if n := counter.Count(); n != 1 || len(sent) != 2 {
		t.Errorf("two leases: one poll ran %d statements and sent %d messages, want 1 and 2", n, len(sent))
	}

	counter.Reset()
	sent = nil
	poll(3, time.Now().Add(protocol.RobotWatchLease))
	if n := counter.Count(); n != 0 || len(sent) != 0 {
		t.Errorf("leases lapsed: three polls ran %d statements and sent %d messages, want 0 and 0", n, len(sent))
	}
}
