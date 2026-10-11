//go:build docker

package dispatch

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// keep_staged_forecast_docker_test.go — a keep-staged refill beside a forecast.
//
// A keep-staged refill is a plain Edge retrieve order to the line's staging
// spot (Core auto-confirms it: the request does not set SkipAutoConfirm),
// carrying the live leg's episode or no_demand. A forecast is a demand_origins
// row that has not opened (opened_at and closed_at NULL),
// planted here with raw SQL because nothing writes one yet. Core admits the
// refill exactly as before and counts it live at its delivery node (the count a
// plan nets against), and the forecast at the line's key gains no child and is
// neither closed nor promoted: a refill is open demand, not an earmark on a
// forecast.
func TestKeepStaged_RefillBesideAForecastIsLiveDemand(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	key := protocol.CellEpisodeKey("SNF-KS", bp.Code, protocol.ClaimRoleConsume)
	fc := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO demand_origins (origin_id, revision, episode_key, kind, station_id, payload_code, opened_at)
		VALUES ($1, 0, $2, 'cell', 'line-1', $3, NULL)`, fc, key, bp.Code)
	testutil.MustNoErr(t, err, "plant the forecast at the line's key")

	live := uuid.NewString()
	testutil.MustNoErr(t, db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID: live, Revision: 1, EpisodeKey: protocol.CellEpisodeKey("SNF-KS2", bp.Code, protocol.ClaimRoleConsume),
		Kind: protocol.EpisodeKindCell, Direction: protocol.ClaimRoleConsume, StationID: "line-1",
		PayloadCode: bp.Code, OpenedAt: time.Now().UTC().Add(-time.Minute),
	}), "the live leg's episode")

	refills := []struct {
		uuid, originID, class string
	}{
		{"ks-refill-attached", live, protocol.OriginClassAttached},
		{"ks-refill-nodemand", "", protocol.OriginClassNoDemand},
	}
	for _, r := range refills {
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID: r.uuid, OrderType: OrderTypeRetrieve, PayloadCode: bp.Code, Quantity: 1,
			DeliveryNode: lineNode.Name, OriginID: r.originID, OriginClass: r.class,
		})
	}
	if len(emitter.failed) > 0 {
		t.Fatalf("a refill was refused beside a forecast: %s", emitter.failed[0].errorCode)
	}
	for _, r := range refills {
		o, err := db.GetOrderByUUID(r.uuid)
		testutil.MustNoErr(t, err, "read "+r.uuid)
		if protocol.IsTerminal(o.Status) || o.OriginID != r.originID || o.OriginClass != r.class {
			t.Errorf("%s: status=%s origin=%q/%q, want live with %q/%q", r.uuid, o.Status, o.OriginClass, o.OriginID, r.class, r.originID)
		}
	}

	n, err := orders.CountLiveByDeliveryNode(db.DB, lineNode.Name)
	testutil.MustNoErr(t, err, "CountLiveByDeliveryNode")
	if n != len(refills) {
		t.Errorf("live orders at %s = %d, want %d: the refills are demand already open", lineNode.Name, n, len(refills))
	}

	var children int
	var openedNull, closedNull bool
	testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM orders WHERE origin_id = $1`, fc).Scan(&children), "children")
	testutil.MustNoErr(t, db.QueryRow(`SELECT opened_at IS NULL, closed_at IS NULL FROM demand_origins WHERE origin_id = $1`, fc).
		Scan(&openedNull, &closedNull), "forecast")
	if children != 0 || !openedNull || !closedNull {
		t.Errorf("forecast children=%d opened_at NULL=%v closed_at NULL=%v, want 0, true, true: "+
			"a refill neither rides, opens nor settles a forecast", children, openedNull, closedNull)
	}
}
