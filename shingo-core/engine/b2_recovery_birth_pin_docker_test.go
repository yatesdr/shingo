//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// TestB2Pin_RecoveryBirth is the carried-bin recovery door's row in B2's birth
// differential (cmd/seeddev planner_births_docker_test.go holds the other six
// doors, on the demo plant).
//
// WHY IT IS HERE AND NOT IN THE GOLDEN. The door reaches its INSERT only with a
// dispatchable robot in the engine's robot cache (RecoverCarriedBin →
// robotCanTakeARecoveryOrder), and the cache is filled only by robotRefreshLoop,
// which the golden stops before any case runs. The golden therefore records the
// door's refusal and nothing it writes. A same-package test can write the cache
// (cacheRobot), so the birth is pinned here, on a hand-built plant, with the same
// AFTER INSERT trigger: the row exactly as the one INSERT wrote it.
//
// What recovery writes at birth, today: a `move` born `pending`, station-less,
// from the robot's carrier node to the slot main's chooser picks in a claim's
// declared source group (chooseDeclared, rebase 4), carrying the bin and
// its payload, source intent on_deck, origin no_demand, and no projection (it
// does not route through admission).
func TestB2Pin_RecoveryBirth(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	dest, binID := declaredChildFor(t, eng, db, "B2R", true)
	bin, err := db.GetBin(binID)
	testutil.MustNoErr(t, err, "read the carried bin")
	// After the fixture, so only the door's own INSERT is caught.
	for _, q := range []string{
		`CREATE TABLE golden_births AS SELECT * FROM orders WITH NO DATA`,
		`CREATE FUNCTION golden_birth() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN INSERT INTO golden_births SELECT NEW.*; RETURN NEW; END $$`,
		`CREATE TRIGGER golden_birth AFTER INSERT ON orders FOR EACH ROW EXECUTE FUNCTION golden_birth()`,
	} {
		_, err := db.DB.Exec(q)
		testutil.MustNoErr(t, err, q)
	}
	var outboxBefore int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&outboxBefore), "outbox mark")

	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")

	type birth struct {
		orderType, status, station, source, delivery, payload, intent, originClass, originID string
		binID                                                                                *int64
		quantity                                                                             int64
		steps                                                                                string
		resolvedAt                                                                           bool
	}
	var b birth
	testutil.MustNoErr(t, db.DB.QueryRow(`
		SELECT order_type, status, COALESCE(station_id,''), COALESCE(source_node,''), COALESCE(delivery_node,''),
		       COALESCE(payload_code,''), COALESCE(source_intent,''), COALESCE(origin_class,''),
		       COALESCE(origin_id::text,''), bin_id, quantity, COALESCE(steps_json,''), destination_resolved_at IS NOT NULL
		FROM golden_births WHERE id = $1`, order.ID).Scan(&b.orderType, &b.status, &b.station, &b.source, &b.delivery,
		&b.payload, &b.intent, &b.originClass, &b.originID, &b.binID, &b.quantity, &b.steps, &b.resolvedAt), "read the birth")

	want := birth{
		orderType: "move", status: "pending", station: "", source: bins.CarrierNodePrefix + "AMR-B2R", delivery: dest.Name,
		payload: bin.PayloadCode, intent: "on_deck", originClass: "no_demand", originID: "", quantity: 1,
	}
	if b.orderType != want.orderType || b.status != want.status || b.station != want.station ||
		b.source != want.source || b.delivery != want.delivery || b.payload != want.payload ||
		b.intent != want.intent || b.originClass != want.originClass || b.originID != want.originID ||
		b.quantity != want.quantity || b.steps != "" || b.resolvedAt {
		t.Errorf("recovery birth moved.\n got: %+v\nwant: %+v (no steps, no destination-resolved stamp)", b, want)
	}
	if b.binID == nil || *b.binID != bin.ID {
		t.Errorf("recovery birth carries bin %v, want %d", b.binID, bin.ID)
	}
	var births int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM golden_births WHERE id <> $1`,
		order.ID).Scan(&births), "other births")
	if births != 0 {
		t.Errorf("recovery wrote %d other order rows at birth, want 0", births)
	}
	var projected int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM outbox WHERE id > $1 AND msg_type = 'data.order.projected'`,
		outboxBefore).Scan(&projected), "projections")
	if projected != 0 {
		t.Errorf("recovery projected %d time(s), want 0: it does not route through admission", projected)
	}
}
