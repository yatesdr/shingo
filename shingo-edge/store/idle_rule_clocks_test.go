package store

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
)

// idle_rule_clocks_test.go — the two clocks the idle rule reads (v53), each
// stamped by an UPDATE that already ran, on the engine's clock.

// TestReportingPoints_TheCountedReadStampsTheLastPart: a read that counted
// parts moves last_part_at with last_poll_at; a read that counted none (a
// counter reset to 0, a point's first read) moves last_poll_at alone.
func TestReportingPoints_TheCountedReadStampsTheLastPart(t *testing.T) {
	t.Parallel()
	db := coverageDB(t)
	_, sid := seedProcessStyle(t, db, "P", "S")
	id, err := db.CreateReportingPoint("PLC", "TAG", sid)
	testutil.MustNoErr(t, err, "create point")

	_, err = db.UpdateReportingPointCounter(id, "PLC", "TAG", 10) // a first read: no part
	testutil.MustNoErr(t, err, "first read")
	rp, _ := db.GetReportingPoint(id)
	if rp.LastPollAt == nil || rp.LastPartAt != nil {
		t.Fatalf("after a read with no part: last_poll_at %v, last_part_at %v; want set and nil", rp.LastPollAt, rp.LastPartAt)
	}

	_, err = db.UpdateReportingPointCounted(id, "PLC", "TAG", 12, true) // two parts
	testutil.MustNoErr(t, err, "counted read")
	rp, _ = db.GetReportingPoint(id)
	if rp.LastPartAt == nil || !rp.LastPartAt.Equal(*rp.LastPollAt) {
		t.Fatalf("after a counted read: last_part_at %v, last_poll_at %v; want both, equal", rp.LastPartAt, rp.LastPollAt)
	}
	part := *rp.LastPartAt

	_, err = db.DB.Exec(`UPDATE reporting_points SET last_part_at=?, last_poll_at=? WHERE id=?`,
		part.Add(-time.Hour).Format("2006-01-02 15:04:05"), part.Add(-time.Hour).Format("2006-01-02 15:04:05"), id)
	testutil.MustNoErr(t, err, "age the clocks")
	_, err = db.UpdateReportingPointCounted(id, "PLC", "TAG", 0, false) // a reset to 0
	testutil.MustNoErr(t, err, "reset read")
	rp, _ = db.GetReportingPoint(id)
	if !rp.LastPartAt.Equal(part.Add(-time.Hour)) || !rp.LastPollAt.After(*rp.LastPartAt) {
		t.Errorf("after a reset: last_part_at %v, last_poll_at %v; want the part clock unmoved, the poll clock moved", rp.LastPartAt, rp.LastPollAt)
	}
}

// TestOrders_TheStagedMirrorStampsWhenTheRobotParked: the staged write stamps
// staged_at with the engine's clock; a later status leaves it; staged again
// restamps it.
func TestOrders_TheStagedMirrorStampsWhenTheRobotParked(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	id, err := db.CreateOrder("staged-at", "complex", nil, false, 1, "ALN_1", "", "SM_1", protocol.Height{}, false, "P", "", "")
	testutil.MustNoErr(t, err, "create order")
	o, _ := db.GetOrder(id)
	if o.StagedAt != nil {
		t.Fatalf("a new order carries staged_at %v, want nil", o.StagedAt)
	}

	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage")
	o, _ = db.GetOrder(id)
	if o.StagedAt == nil || clock.Now().Sub(*o.StagedAt) > time.Minute || clock.Now().Sub(*o.StagedAt) < -time.Second {
		t.Fatalf("staged_at = %v after staging, want the engine's now", o.StagedAt)
	}
	_, err = db.DB.Exec(`UPDATE orders SET staged_at='2026-01-01 00:00:00' WHERE id=?`, id)
	testutil.MustNoErr(t, err, "age it")

	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusInTransit)), "move on")
	o, _ = db.GetOrder(id)
	if o.StagedAt == nil || o.StagedAt.Year() != 2026 || o.StagedAt.Month() != time.January {
		t.Fatalf("staged_at = %v after in_transit, want it left", o.StagedAt)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage again")
	o, _ = db.GetOrder(id)
	if o.StagedAt == nil || clock.Now().Sub(*o.StagedAt) > time.Minute {
		t.Errorf("staged_at = %v after staging again, want restamped", o.StagedAt)
	}
}
