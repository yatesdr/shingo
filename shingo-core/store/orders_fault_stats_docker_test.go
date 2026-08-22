//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// The Faults card's whole claim is that a faulted order's outcome and dwell are
// measured against the row that ACTUALLY FOLLOWED IT. DwellStats cannot do this:
// it measures MAX(faulted)→MAX(to), so an order that faults twice lands in two
// outcome buckets at once and its recovery dwell spans a fault it had already
// recovered from. The third fixture below is that order, and it is the reason
// this query exists rather than four DwellStats pairs.

func seedFaultOrder(t *testing.T, db *store.DB, uuid, robot string, rows []struct {
	status string
	at     time.Time
	code   int
	desc   string
},
) {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "edge.test", OrderType: "retrieve",
		Status: protocol.StatusInTransit, Quantity: 1, DeliveryNode: "DELV.1",
		RobotID: robot,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create order "+uuid)
	// orders.Create does not persist robot_id — a robot is assigned at dispatch
	// — so the fixture sets it directly. The by-robot breakdown reads the
	// column, not the struct.
	_, rerr := db.DB.Exec(`UPDATE orders SET robot_id=$1 WHERE id=$2`, robot, o.ID)
	testutil.MustNoErr(t, rerr, "set robot_id")
	for _, r := range rows {
		_, err := db.DB.Exec(
			`INSERT INTO order_history (order_id, status, detail, actor, ref, created_at)
			 VALUES ($1, $2, '', 'fleet',
			         jsonb_build_object('node','ALN_003','vendor_code',$3::int,'vendor_desc',$4::text), $5)`,
			o.ID, r.status, r.code, r.desc, r.at)
		testutil.MustNoErr(t, err, "insert history")
	}
}

func TestGetFaultStats(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	type row = struct {
		status string
		at     time.Time
		code   int
		desc   string
	}
	base := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)

	// 1. Faulted, recovered after 18s. The 97% case.
	seedFaultOrder(t, db, "fs-recovered", "AMR-04", []row{
		{"faulted", base, 60011, "cannot replan"},
		{"in_transit", base.Add(18 * time.Second), 0, ""},
	})
	// 2. Faulted, gave up after 45m.
	seedFaultOrder(t, db, "fs-failed", "AMR-10", []row{
		{"faulted", base.Add(time.Minute), 60011, "cannot replan"},
		{"failed", base.Add(time.Minute + 45*time.Minute), 0, ""},
	})
	// 3. THE ONE THAT BREAKS MAX()-PAIRING. Faults, recovers at 10s, faults
	// again, then fails 30m later. Correct: two faults, one recovery (10s), one
	// give-up (30m). MAX(faulted)→MAX(in_transit) would also report a recovery
	// for the SECOND fault, spanning backwards, and count this order in two
	// outcome buckets.
	seedFaultOrder(t, db, "fs-twice", "AMR-04", []row{
		{"faulted", base.Add(2 * time.Hour), 60011, "cannot replan"},
		{"in_transit", base.Add(2*time.Hour + 10*time.Second), 0, ""},
		{"faulted", base.Add(3 * time.Hour), 54018, "robot suspended"},
		{"failed", base.Add(3*time.Hour + 30*time.Minute), 0, ""},
	})

	stats, err := db.GetFaultStats(
		orders.LeadTimeRange{Start: base.Add(-time.Hour), End: time.Now().UTC()},
		60*time.Second)
	testutil.MustNoErr(t, err, "GetFaultStats")

	// Four faulted rows across three orders.
	var total int64
	byStatus := map[string]orders.FaultOutcome{}
	for _, o := range stats.Outcomes {
		total += o.Count
		byStatus[o.Status] = o
	}
	if total != 4 {
		t.Errorf("total faults = %d, want 4 (one order faulted twice)", total)
	}
	if got := byStatus["in_transit"].Count; got != 2 {
		t.Errorf("recovered = %d, want 2", got)
	}
	if got := byStatus["failed"].Count; got != 2 {
		t.Errorf("gave up = %d, want 2", got)
	}
	// The recovery p50 across 18s and 10s is 14s. If the pairing were
	// MAX-based, the second fault would contribute a NEGATIVE or absurd span
	// here and this number would not be 14.
	if p := byStatus["in_transit"].P50Seconds; p < 13 || p > 15 {
		t.Errorf("recovery p50 = %.1fs, want ~14s — the pairing is not next-row", p)
	}
	if p := byStatus["failed"].P50Seconds; p < 2000 || p > 2800 {
		t.Errorf("gave-up p50 = %.1fs, want between 30m and 45m", p)
	}

	// Replanning vs notice: the two recoveries (18s, 10s) are replans, the two
	// give-ups are faults. That split IS the card.
	var replan, notice int64
	for _, d := range stats.PerDay {
		replan += d.Replanning
		notice += d.Notice
	}
	if replan != 2 || notice != 2 {
		t.Errorf("split = %d replanning / %d notice, want 2 / 2", replan, notice)
	}
	if stats.NoticeAfterSeconds != 60 {
		t.Errorf("notice threshold = %d, want 60", stats.NoticeAfterSeconds)
	}

	// By robot: AMR-04 has three faults (one order once, one order twice),
	// AMR-10 has one.
	if len(stats.ByRobot) == 0 || stats.ByRobot[0].Key != "AMR-04" || stats.ByRobot[0].Count != 3 {
		t.Fatalf("by robot = %+v, want AMR-04 leading with 3", stats.ByRobot)
	}
	// Only one of AMR-04's three lasted past the threshold.
	if stats.ByRobot[0].NoticeHits != 1 {
		t.Errorf("AMR-04 notice hits = %d, want 1", stats.ByRobot[0].NoticeHits)
	}

	if len(stats.ByNode) == 0 || stats.ByNode[0].Key != "ALN_003" || stats.ByNode[0].Count != 4 {
		t.Fatalf("by node = %+v, want ALN_003 with 4", stats.ByNode)
	}

	// The fleet's reasons, with the code as the key and its own text as the
	// label — no translation table.
	reasons := map[string]orders.FaultGroup{}
	for _, r := range stats.ByReason {
		reasons[r.Key] = r
	}
	if r := reasons["60011"]; r.Count != 3 || r.Label != "cannot replan" {
		t.Errorf("reason 60011 = %+v, want 3 with the fleet's own text", r)
	}
	if r := reasons["54018"]; r.Count != 1 || r.Label != "robot suspended" {
		t.Errorf("reason 54018 = %+v", r)
	}
}

// A fault that has not moved yet is the most interesting row on the page, not a
// missing one: it is counted, its dwell runs to now, and it keeps its own
// outcome bucket rather than being folded into a recovery.
func TestGetFaultStats_AnOpenFaultIsCountedAndNamed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	type row = struct {
		status string
		at     time.Time
		code   int
		desc   string
	}
	since := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Second)
	seedFaultOrder(t, db, "fs-open", "AMR-07", []row{
		{"faulted", since, 60011, "cannot replan"},
	})

	stats, err := db.GetFaultStats(
		orders.LeadTimeRange{Start: since.Add(-time.Hour), End: time.Now().UTC()},
		60*time.Second)
	testutil.MustNoErr(t, err, "GetFaultStats")

	var open *orders.FaultOutcome
	for i := range stats.Outcomes {
		if stats.Outcomes[i].Status == "" {
			open = &stats.Outcomes[i]
		}
	}
	if open == nil {
		t.Fatal("an order still faulted must have its own outcome row")
	}
	if open.Count != 1 {
		t.Errorf("open faults = %d, want 1", open.Count)
	}
	// Its dwell runs to now, so it counts as a notice fault at 20 minutes.
	if open.P50Seconds < 1000 {
		t.Errorf("open fault dwell = %.0fs, want ~1200s measured to now", open.P50Seconds)
	}
	var notice int64
	for _, d := range stats.PerDay {
		notice += d.Notice
	}
	if notice != 1 {
		t.Errorf("an open 20-minute fault must count as a notice fault, got %d", notice)
	}
}

// An empty window returns empty slices and no error — the card renders its
// em-dash rather than failing.
func TestGetFaultStats_EmptyWindow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	end := time.Now().UTC().Add(-365 * 24 * time.Hour)
	stats, err := db.GetFaultStats(
		orders.LeadTimeRange{Start: end.Add(-time.Hour), End: end}, 60*time.Second)
	testutil.MustNoErr(t, err, "GetFaultStats on an empty window")
	if len(stats.Outcomes) != 0 || len(stats.PerDay) != 0 || len(stats.ByRobot) != 0 {
		t.Errorf("an empty window must be empty, got %+v", stats)
	}
}
