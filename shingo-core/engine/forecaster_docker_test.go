//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
	"shingocore/store/reservations"
)

// forecaster_docker_test.go — the forecast on the two-minute pass (D2.1).

// forecastFixture is a process running one consume cell (its level 2, its
// supply from a market node holding two bins of its part) and the monitor.
func forecastFixture(t *testing.T, prefix string) (*Engine, *store.DB, string) {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newUnstartedEngine(t, db, simulator.New())
	eng.Start()
	t.Cleanup(eng.Stop)
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), name)
		return n
	}
	market, line := mk(prefix+"-MARKET"), mk(prefix+"-LINE")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, market.ID, prefix+"-B1")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, market.ID, prefix+"-B2")
	process := prefix + "-PROC"
	_, err := db.ReplacePlantClaimsFromStation(process, "line-1",
		[]plantclaims.StyleRow{{ProcessID: process, StyleID: "S", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: process, StyleID: "S", CoreNodeName: line.Name,
			Role: protocol.ClaimRoleConsume, SwapMode: protocol.SwapModeTwoRobot,
			PayloadCode: sd.Payload.Code, InboundSource: market.Name, ReorderPoint: 2, UOPCapacity: 10}},
		nil, 0)
	testutil.MustNoErr(t, err, "the cell's claim")
	return eng, db, protocol.CellEpisodeKey(process, sd.Payload.Code, protocol.ClaimRoleConsume)
}

func forecastRow(t *testing.T, db *store.DB, key string) (id, plan, verdict string, open bool) {
	t.Helper()
	testutil.MustNoErr(t, db.QueryRow(`SELECT origin_id, plan, plan_verdict, opened_at IS NOT NULL
		FROM demand_origins WHERE episode_key = $1 AND closed_at IS NULL`, key).Scan(&id, &plan, &verdict, &open),
		"the place's unsettled record")
	return id, plan, verdict, open
}

// A running cell with stock gets one forecast record with the pick the firing
// door would make; the forecast creates no order and holds nothing; a pass with
// nothing changed writes nothing.
func TestForecast_ARunningCellIsForecastAndHoldsNothing(t *testing.T) {
	t.Parallel()
	eng, db, key := forecastFixture(t, "FC1")
	before := func() (int, int) {
		var o, r int
		testutil.MustNoErr(t, db.QueryRow(`SELECT (SELECT COUNT(*) FROM orders), (SELECT COUNT(*) FROM reservations)`).Scan(&o, &r), "count")
		return o, r
	}
	o0, r0 := before()

	eng.SourceabilityMonitor().recomputeAll()
	id, plan, verdict, open := forecastRow(t, db, key)
	if open || plan == "" || verdict != "" {
		t.Fatalf("the cell's record: open %v, plan %q, verdict %q; want a forecast with a plan and a satisfiable verdict", open, plan, verdict)
	}
	if o1, r1 := before(); o1 != o0 || r1 != r0 {
		t.Fatalf("the forecast created %d orders and %d reservations; it creates and holds nothing", o1-o0, r1-r0)
	}

	eng.SourceabilityMonitor().recomputeAll()
	id2, plan2, _, _ := forecastRow(t, db, key)
	if id2 != id || plan2 != plan {
		t.Fatalf("a pass with nothing changed moved the forecast: %s %s -> %s %s", id, plan, id2, plan2)
	}
}

// A live order's soft hold on the planned bin makes the plan stale at the next
// pass, and the forecast re-plans to the other bin: the forecast never hides a
// bin (law 2).
func TestForecast_AHoldOnThePlannedBinRePlans(t *testing.T) {
	t.Parallel()
	eng, db, key := forecastFixture(t, "FC2")
	eng.SourceabilityMonitor().recomputeAll()
	_, plan, _, _ := forecastRow(t, db, key)

	var planned int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT (plan::json->>'bin_id')::bigint FROM demand_origins WHERE episode_key = $1 AND closed_at IS NULL`, key).Scan(&planned), "planned bin")
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) { o.EdgeUUID = "fc2-holder" })
	testutil.MustNoErr(t, reservations.Acquire(db.DB, holder.ID, holder.ID, planned, "test"), "a live order holds the planned bin")

	eng.SourceabilityMonitor().recomputeAll()
	_, plan2, verdict2, _ := forecastRow(t, db, key)
	if plan2 == plan || plan2 == "" || verdict2 != "" {
		t.Fatalf("after a hold on its bin the forecast plan is %q (was %q, verdict %q); want the other bin", plan2, plan, verdict2)
	}
}

// A door that opens the place's demand changes the forecast's tense; the next
// pass leaves an open record alone.
func TestForecast_AnOpenDemandIsLeftToItsDoor(t *testing.T) {
	t.Parallel()
	eng, db, key := forecastFixture(t, "FC3")
	eng.SourceabilityMonitor().recomputeAll()
	id, _, _, _ := forecastRow(t, db, key)

	_, err := db.SupersedeOpenEpisode(key, "11111111-1111-1111-1111-111111111111", time.Now())
	testutil.MustNoErr(t, err, "the cell's open re-keys the forecast")
	testutil.MustNoErr(t, db.QueryRow(`UPDATE demand_origins SET opened_at = NOW(), revision = 1 WHERE episode_key = $1 AND closed_at IS NULL RETURNING origin_id`, key).Scan(&id), "opened")

	eng.SourceabilityMonitor().recomputeAll()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM demand_origins WHERE episode_key = $1`, key).Scan(&n), "records")
	if n != 1 {
		t.Fatalf("%d records at the place after it opened; want the one, changed tense", n)
	}
}

// D2.2 · A forecast whose projected need passed by the grace without opening
// is closed expired by the pass, and the next pass births the place's next
// forecast.
func TestForecast_AnUnopenedForecastPastItsNeedExpires(t *testing.T) {
	t.Parallel()
	eng, db, key := forecastFixture(t, "FC4")
	eng.SourceabilityMonitor().recomputeAll()
	id, _, _, _ := forecastRow(t, db, key)
	_, err := db.Exec(`UPDATE demand_origins SET need_at = NOW() - interval '1 hour' WHERE origin_id = $1`, id)
	testutil.MustNoErr(t, err, "its need passed an hour ago")

	eng.SourceabilityMonitor().recomputeAll()
	var reason, closedBy string
	testutil.MustNoErr(t, db.QueryRow(`SELECT close_reason, closed_by FROM demand_origins WHERE origin_id = $1`, id).Scan(&reason, &closedBy), "the old forecast")
	if reason != "expired" || closedBy != "sweep" {
		t.Fatalf("the forecast past its need: close %q by %q; want expired by sweep", reason, closedBy)
	}
	eng.SourceabilityMonitor().recomputeAll()
	if id2, _, _, _ := forecastRow(t, db, key); id2 == id {
		t.Fatal("no new forecast after the expired one")
	}
	// D2.3 · the score reads it: two born, one still forecasting, one miss.
	score, err := db.ForecastScore(time.Time{})
	testutil.MustNoErr(t, err, "the score")
	if score.Born != 2 || score.Forecasting != 1 || score.Expired != 1 || score.Opened != 0 {
		t.Errorf("the score: %+v; want born 2, forecasting 1, expired 1, opened 0", score)
	}
}

// D2.2 · A place that leaves the population (its claim gone from the running
// style) has its forecast closed withdrawn.
func TestForecast_APlaceThatLeavesThePopulationIsWithdrawn(t *testing.T) {
	t.Parallel()
	eng, db, key := forecastFixture(t, "FC5")
	eng.SourceabilityMonitor().recomputeAll()
	id, _, _, _ := forecastRow(t, db, key)
	_, err := db.Exec(`DELETE FROM style_claims WHERE process_id = 'FC5-PROC'`)
	testutil.MustNoErr(t, err, "the claim retired")

	eng.SourceabilityMonitor().recomputeAll()
	var reason string
	testutil.MustNoErr(t, db.QueryRow(`SELECT close_reason FROM demand_origins WHERE origin_id = $1`, id).Scan(&reason), "the forecast")
	if reason != "withdrawn" {
		t.Fatalf("the forecast of a place gone from the population: close %q, want withdrawn", reason)
	}
}

// F6 (a) · a produce cell is a place by its level, the claim's capacity when it
// sets no reorder point (the Edge's DemandLevel, one spelling in protocol), and
// its need is projected from the produce rate: the bin at the line filling to
// the level.
func TestForecast_AProduceCellIsProjectedFromItsProduceRate(t *testing.T) {
	t.Parallel()
	threshold, needAt := produceForecast(t, "FC6")
	if threshold != 10 {
		t.Errorf("threshold %d, want the capacity 10", threshold)
	}
	if needAt == nil || needAt.Before(time.Now()) {
		t.Errorf("need_at %v, want a projection ahead: 6 to fill at the line's produce rate", needAt)
	}
}

// The forecast's clock is the wall clock its rates are measured in (the
// ledger's applied_at, Postgres NOW()) and the Edges stamp opens in, never the
// sim clock: a projection is a wall time. RED at 3d8dce49: under a process
// clock ten hours ahead the projection landed ten hours out. Not parallel: it
// sets the process clock, which no other test may see.
func TestForecast_TheProjectionIsWallTime(t *testing.T) {
	prev := clock.Default()
	clock.SetDefault(clock.NewManual(time.Now().Add(10 * time.Hour)))
	t.Cleanup(func() { clock.SetDefault(prev) })
	_, needAt := produceForecast(t, "FC7")
	if needAt == nil || needAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("need_at %v under a process clock ten hours ahead; want a wall-time projection minutes out", needAt)
	}
}

// produceForecast is a produce cell (no reorder point, capacity 10) whose bin
// at the line holds 4 and which made 60 in the window, after one pass: its
// forecast's threshold and need_at.
func produceForecast(t *testing.T, prefix string) (int, *time.Time) {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newUnstartedEngine(t, db, simulator.New())
	eng.Start()
	t.Cleanup(eng.Stop)
	line := &nodes.Node{Name: prefix + "-LINE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(line), "line")
	market := &nodes.Node{Name: prefix + "-MARKET", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "market")
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, line.ID, prefix+"-B1")
	_, err := db.Exec(`UPDATE bins SET uop_remaining = 4 WHERE id = $1`, bin.ID)
	testutil.MustNoErr(t, err, "the bin at the line holds 4")
	_, err = db.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, node_id, reason, applied_at)
		VALUES ($1, 0, 60, 'bin_uop_delta', 'test', $2, 'test', '{}'::jsonb, $3, 'produce_tick', NOW() - interval '60 seconds')`,
		bin.ID, sd.Payload.Code, line.ID)
	testutil.MustNoErr(t, err, "the line made 60 in the window")
	process := prefix + "-PROC"
	_, err = db.ReplacePlantClaimsFromStation(process, "line-1",
		[]plantclaims.StyleRow{{ProcessID: process, StyleID: "S", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: process, StyleID: "S", CoreNodeName: line.Name,
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot,
			PayloadCode: sd.Payload.Code, InboundSource: market.Name, UOPCapacity: 10}},
		nil, 0)
	testutil.MustNoErr(t, err, "the produce claim, no reorder point")

	eng.SourceabilityMonitor().recomputeAll()
	key := protocol.CellEpisodeKey(process, sd.Payload.Code, protocol.ClaimRoleProduce)
	var threshold int
	var needAt *time.Time
	testutil.MustNoErr(t, db.QueryRow(`SELECT threshold, need_at FROM demand_origins
		WHERE episode_key = $1 AND closed_at IS NULL`, key).Scan(&threshold, &needAt), "the produce place's forecast")
	return threshold, needAt
}
