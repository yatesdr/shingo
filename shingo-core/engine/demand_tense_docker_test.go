//go:build docker

package engine

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// demand_tense_docker_test.go — Core's episode loops beside a forecast row.
//
// A forecast is a demand_origins row with opened_at NULL and closed_at NULL.
// Nothing writes one in this phase; these pins write it with raw SQL so the
// sweep, the keeper and the threshold monitor are shown to read it as what it
// is: a record that has not opened, holds nothing and produces no order.

// insertForecastRow writes a forecast row at key and returns its origin id.
// node and payload are the place's, as a forecaster would stamp them.
func insertForecastRow(t *testing.T, db *store.DB, key, kind, station, node, payload string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO demand_origins (origin_id, revision, episode_key, kind, station_id,
		                            core_node_name, payload_code, opened_at)
		VALUES ($1, 0, $2, $3, $4, $5, $6, NULL)`, id, key, kind, station, node, payload)
	testutil.MustNoErr(t, err, "insert synthetic forecast")
	return id
}

// forecastStillUnsettled reports whether the forecast row is still a forecast:
// not closed, not opened.
func forecastStillUnsettled(t *testing.T, db *store.DB, id string) (openedNull, closedNull bool) {
	t.Helper()
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT opened_at IS NULL, closed_at IS NULL FROM demand_origins WHERE origin_id = $1`, id).
		Scan(&openedNull, &closedNull), "read forecast")
	return openedNull, closedNull
}

// noOrderRidesAForecast fails if any order's origin is a row that has not
// opened. A forecast produces no order.
func noOrderRidesAForecast(t *testing.T, db *store.DB) {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`
		SELECT COUNT(*) FROM orders o JOIN demand_origins d ON d.origin_id = o.origin_id
		 WHERE d.opened_at IS NULL`).Scan(&n), "count orders on forecasts")
	if n != 0 {
		t.Errorf("%d order(s) carry the origin of a row that never opened; a forecast produces no order", n)
	}
}

// TestDemandReconciler_AForecastDoesNotStopTheSweep: the childless sweep reads
// every open episode in one query. A forecast row in the table must neither
// stop that pass nor be closed by it as `unattributed` (it has no children
// because it has not opened, which is not a finding).
func TestDemandReconciler_AForecastDoesNotStopTheSweep(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	const station = "PLANT.LINE1"
	registerActiveEdge(t, db, station)
	fc := insertForecastRow(t, db, protocol.CellEpisodeKey("SNF-FC", "PANEL-FC1", protocol.ClaimRoleConsume),
		protocol.EpisodeKindCell, station, "", "PANEL-FC1")
	stale := openCellEpisode(t, db, station, "PANEL-FC2", "SNF-FC2", time.Hour)

	eng.reconcileDemandEpisodes()

	if got := mustGetOrigin(t, db, stale); got.ClosedAt == nil || got.CloseReason != protocol.CloseReasonUnattributed {
		t.Errorf("hour-old childless episode closed=%v reason=%q, want closed %q: the forecast row stopped the sweep",
			got.ClosedAt != nil, got.CloseReason, protocol.CloseReasonUnattributed)
	}
	if openedNull, closedNull := forecastStillUnsettled(t, db, fc); !openedNull || !closedNull {
		t.Errorf("forecast opened_at NULL=%v closed_at NULL=%v after the sweep, want both NULL", openedNull, closedNull)
	}
}

// TestThresholdSweep_AForecastIsNotOpen: the binding sweep closes open
// threshold episodes whose binding is gone. It must still do that with a
// threshold forecast in the table, and must not close the forecast as
// `threshold_removed`: a forecast is not an open episode.
func TestThresholdSweep_AForecastIsNotOpen(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newTestEngine(t, db, simulator.New())
	openID := uuid.NewString()
	testutil.MustNoErr(t, db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID: openID, Revision: 1, EpisodeKey: protocol.ThresholdEpisodeKey("SLN_GONE", "PANEL-TS"),
		Kind: protocol.EpisodeKindThreshold, StationID: "PLANT.TS", CoreNodeName: "SLN_GONE",
		PayloadCode: "PANEL-TS", OpenedAt: time.Now().UTC().Add(-time.Hour),
	}), "open a threshold episode with no binding")
	fc := insertForecastRow(t, db, protocol.ThresholdEpisodeKey("SLN_FC", "PANEL-TS"),
		protocol.EpisodeKindThreshold, "PLANT.TS", "SLN_FC", "PANEL-TS")

	eng.reconcileDemandEpisodes()

	if got := mustGetOrigin(t, db, openID); got.ClosedAt == nil || got.CloseReason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("unbound threshold episode closed=%v reason=%q, want closed %q: the forecast row stopped the sweep",
			got.ClosedAt != nil, got.CloseReason, protocol.CloseReasonThresholdRemoved)
	}
	if openedNull, closedNull := forecastStillUnsettled(t, db, fc); !openedNull || !closedNull {
		t.Errorf("forecast opened_at NULL=%v closed_at NULL=%v after the sweep, want both NULL", openedNull, closedNull)
	}
}

// TestMaintainer_AForecastIsNotItsEpisode: the keeper's open set is its whole
// memory. A forecast at one group's key must not stop the tick for every other
// group, must not be treated as that group's open episode, and must never be
// the origin an ask rides on while it is still a forecast.
func TestMaintainer_AForecastIsNotItsEpisode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	mntFixture(t, db, "MNT-FCA", 2, "MNT-FCA-T", 1)
	mntFixture(t, db, "MNT-FCB", 2, "MNT-FCB-T", 1)
	insertForecastRow(t, db, protocol.MaintainEpisodeKey("MNT-FCA", "MNT-FCA-T"),
		protocol.EpisodeKindMaintain, "test-core", "MNT-FCA", "")

	m := eng.Maintainer()
	m.Tick()

	b := mntState(t, m, "MNT-FCB", "MNT-FCB-T")
	if b.OriginID == "" || b.Created != 1 {
		t.Errorf("group without a forecast: origin=%q created=%d, want an episode and one ask: "+
			"the forecast at another group's key stopped the keeper's tick", b.OriginID, b.Created)
	}
	noOrderRidesAForecast(t, db)
}

// TestOpenCoreEpisode_PromotesTheForecastAtItsKey: Core's two open doors (the
// threshold falling edge and the keeper's gap) find a forecast holding the key
// and turn THAT row into the open episode, rather than failing on the
// one-unsettled-record index and never firing. The opened row keeps the
// forecast's origin id, and the order or ask that follows rides it.
func TestOpenCoreEpisode_PromotesTheForecastAtItsKey(t *testing.T) {
	t.Parallel()

	t.Run("threshold", func(t *testing.T) {
		t.Parallel()
		r := newRTRig(t, "PANEL-FC-PRO", 500)
		fc := insertForecastRow(t, r.eng.db, placeKey(r.b.coreNodeName, r.b.payloadCode),
			protocol.EpisodeKindThreshold, r.b.stationID, r.b.coreNodeName, r.b.payloadCode)
		r.boot()
		r.setUOP(40)
		r.delta()

		rows := r.rows()
		if len(rows) != 1 || rows[0].originID != fc || !rows[0].open {
			t.Fatalf("episodes at the place = %+v, want one open row, the forecast %s, promoted", rows, fc)
		}
		if openedNull, _ := forecastStillUnsettled(t, r.eng.db, fc); openedNull {
			t.Error("the promoted row still has no opened_at: it did not change tense")
		}
		fired := r.fired()
		if len(fired) != 1 || fired[0].OriginID != fc {
			t.Errorf("fires = %+v, want one carrying the promoted origin %s", fired, fc)
		}
		if n := r.failedMints(); n != 0 {
			t.Errorf("%d failed mint(s): the forecast blocked the falling edge", n)
		}
	})

	t.Run("maintain", func(t *testing.T) {
		t.Parallel()
		db := testdb.Open(t)
		eng := newTestEngine(t, db, simulator.New())
		mntFixture(t, db, "MNT-FCP", 2, "MNT-FCP-T", 1)
		fc := insertForecastRow(t, db, protocol.MaintainEpisodeKey("MNT-FCP", "MNT-FCP-T"),
			protocol.EpisodeKindMaintain, "test-core", "MNT-FCP", "")

		m := eng.Maintainer()
		m.Tick()

		st := mntState(t, m, "MNT-FCP", "MNT-FCP-T")
		if st.OriginID != fc || st.Created != 1 {
			t.Errorf("keeper origin=%q created=%d, want the promoted forecast %s and one ask", st.OriginID, st.Created, fc)
		}
		if openedNull, closedNull := forecastStillUnsettled(t, db, fc); openedNull || !closedNull {
			t.Errorf("promoted row opened_at NULL=%v closed_at NULL=%v, want opened and not closed", openedNull, closedNull)
		}
		noOrderRidesAForecast(t, db)

		// A second tick sees the promoted row as its open episode: no re-mint.
		m.Tick()
		if st := mntState(t, m, "MNT-FCP", "MNT-FCP-T"); st.OriginID != fc || st.Created != 0 {
			t.Errorf("second tick origin=%q created=%d, want %s and 0", st.OriginID, st.Created, fc)
		}
	})
}
