//go:build docker

package messaging

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

// demand_origin_tense_docker_test.go — the Edge's open message at a key a
// forecast holds.
//
// A forecast is a demand_origins row with opened_at NULL and closed_at NULL,
// Core's alone; the Edge never sees one. When the Edge opens an episode at that
// key, the forecast is the same record changing tense: Core re-keys it to the
// Edge's origin id and the Edge's message lands on it. It is not closed as
// superseded and no second row is made. Nothing writes a forecast in this
// phase, so the row is planted with raw SQL.

func plantForecast(t *testing.T, db *store.DB, key string, revision, signalCount int) string {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO demand_origins (origin_id, revision, episode_key, kind, station_id, signal_count, opened_at)
		VALUES ($1, $2, $3, 'cell', 'edge.fc', $4, NULL)`, id, revision, key, signalCount)
	testutil.MustNoErr(t, err, "plant forecast")
	return id
}

type keyRow struct {
	originID    string
	revision    int64
	openedNull  bool
	closedNull  bool
	closeReason string
	openedTotal int
	payload     string
	signalCount int
}

func rowsAtKey(t *testing.T, db *store.DB, key string) []keyRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT origin_id, revision, opened_at IS NULL, closed_at IS NULL, close_reason,
		       opened_total, payload_code, signal_count
		  FROM demand_origins WHERE episode_key = $1 ORDER BY origin_id`, key)
	testutil.MustNoErr(t, err, "rows at key")
	defer rows.Close()
	var out []keyRow
	for rows.Next() {
		var r keyRow
		testutil.MustNoErr(t, rows.Scan(&r.originID, &r.revision, &r.openedNull, &r.closedNull,
			&r.closeReason, &r.openedTotal, &r.payload, &r.signalCount), "scan row at key")
		out = append(out, r)
	}
	return out
}

func edgeOpen(key, originID string, revision int64, opened time.Time) *protocol.DemandOriginState {
	return &protocol.DemandOriginState{
		OriginID: originID, Revision: revision, EpisodeKey: key, Kind: protocol.EpisodeKindCell,
		Direction: protocol.ClaimRoleConsume, ProcessID: "SNF-FC", PayloadCode: "PANEL-FCE",
		OpenedAt: opened, OpenedTotal: 40,
	}
}

func edgeEnv() *protocol.Envelope {
	return &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.fc"}}
}

// TestEdgeOpen_PromotesTheForecastNotSupersedes: one row at the key after the
// Edge's open, the forecast's own row (its Core-owned signal_count survives),
// now open under the Edge's origin id. Nothing is closed as superseded.
func TestEdgeOpen_PromotesTheForecastNotSupersedes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	key := protocol.CellEpisodeKey("SNF-FC", "PANEL-FCE", protocol.ClaimRoleConsume)
	plantForecast(t, db, key, 0, 3)

	edgeID := uuid.NewString()
	svc.HandleDemandOrigin(edgeEnv(), edgeOpen(key, edgeID, 1, time.Now().UTC().Add(-time.Minute)))

	got := rowsAtKey(t, db, key)
	if len(got) != 1 {
		t.Fatalf("%d rows at the key after the Edge's open, want 1 (the forecast, promoted): %+v", len(got), got)
	}
	r := got[0]
	if r.originID != edgeID || r.openedNull || !r.closedNull {
		t.Errorf("row at key = %+v, want origin %s, opened, not closed", r, edgeID)
	}
	if r.closeReason == store.CloseReasonSuperseded {
		t.Error("the forecast was closed as superseded: the Edge's open is the same record changing tense")
	}
	if r.signalCount != 3 {
		t.Errorf("signal_count = %d, want 3: the Edge's open made a new row instead of opening the forecast's", r.signalCount)
	}
}

// TestEdgeOpen_FirstRevisionWinsOverAPromotedForecast: whatever revision Core
// gave its forecast, the promotion resets it below the Edge's first (1), so the
// Edge's open lands in full under the revision guard; a redelivery is a no-op
// and the Edge's close settles the same row: forecast, open, settled, one record.
func TestEdgeOpen_FirstRevisionWinsOverAPromotedForecast(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	key := protocol.CellEpisodeKey("SNF-FC", "PANEL-FCR", protocol.ClaimRoleConsume)
	plantForecast(t, db, key, 5, 0)

	edgeID := uuid.NewString()
	opened := time.Now().UTC().Add(-time.Minute)
	open := edgeOpen(key, edgeID, 1, opened)
	svc.HandleDemandOrigin(edgeEnv(), open)
	svc.HandleDemandOrigin(edgeEnv(), open) // redelivery

	got := rowsAtKey(t, db, key)
	if len(got) != 1 || got[0].originID != edgeID || got[0].revision != 1 ||
		got[0].openedTotal != 40 || got[0].payload != "PANEL-FCE" || got[0].openedNull {
		t.Fatalf("rows at key = %+v, want one row %s at revision 1 carrying the Edge's fields", got, edgeID)
	}

	closed := *open
	closed.Revision = 2
	at := opened.Add(30 * time.Second)
	closed.ClosedAt = &at
	closed.CloseReason = protocol.CloseReasonRecovered
	svc.HandleDemandOrigin(edgeEnv(), &closed)

	got = rowsAtKey(t, db, key)
	if len(got) != 1 || got[0].closedNull || got[0].closeReason != protocol.CloseReasonRecovered {
		t.Errorf("rows at key after the close = %+v, want the one row settled %q", got, protocol.CloseReasonRecovered)
	}
}

// TestEdgeOpen_AStaleRedeliveryDoesNotPromote: an open redelivered for an
// episode that already ended (its row exists, closed) says nothing about the
// key's forecast. The forecast stays a forecast, and the ended row stays
// ended under the revision guard.
func TestEdgeOpen_AStaleRedeliveryDoesNotPromote(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	key := protocol.CellEpisodeKey("SNF-FC", "PANEL-FCS", protocol.ClaimRoleConsume)

	edgeID := uuid.NewString()
	opened := time.Now().UTC().Add(-time.Hour)
	open := edgeOpen(key, edgeID, 1, opened)
	svc.HandleDemandOrigin(edgeEnv(), open)
	closed := *open
	closed.Revision = 2
	at := opened.Add(time.Minute)
	closed.ClosedAt = &at
	closed.CloseReason = protocol.CloseReasonRecovered
	svc.HandleDemandOrigin(edgeEnv(), &closed)

	fc := plantForecast(t, db, key, 0, 0)
	svc.HandleDemandOrigin(edgeEnv(), open) // the stale open, redelivered

	byID := map[string]keyRow{}
	for _, r := range rowsAtKey(t, db, key) {
		byID[r.originID] = r
	}
	if r, ok := byID[fc]; !ok || !r.openedNull || !r.closedNull {
		t.Errorf("forecast after a stale redelivery = %+v (present=%v), want still a forecast", r, ok)
	}
	if r := byID[edgeID]; r.closedNull || r.revision != 2 {
		t.Errorf("ended episode after a stale redelivery = %+v, want still closed at revision 2", r)
	}
}
