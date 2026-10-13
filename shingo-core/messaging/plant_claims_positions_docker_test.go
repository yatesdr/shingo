//go:build docker

package messaging

import (
	"encoding/json"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store/loaders"
	"shingocore/store/plantclaims"
)

// Lane 0 item 6 (BRIEF-unification-D1; C2c DECISIONS D9, D30; DESIGN-H §4 S1).
// A loader / FG seat is in no style claim, so before the positions the mirror
// named no station for it: a landing there with no order station fell to the
// last arm (every Edge). A position the Edge publishes is mirrored and the
// landing's address read finds the publishing station.

// positionsReport decodes a report from the wire, so the pin reads the same at
// a base whose report type has no positions field.
func positionsReport(t *testing.T, raw string) *protocol.PlantClaimsReport {
	t.Helper()
	var r protocol.PlantClaimsReport
	testutil.MustNoErr(t, json.Unmarshal([]byte(raw), &r), "decode report")
	return &r
}

func TestPlantClaims_ALoaderPositionResolvesToItsStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	const a, b = "edge-pos-a.line", "edge-pos-b.line"
	for _, st := range []string{a, b} {
		_, err := db.Exec(`INSERT INTO edge_registry (station_id, schema_version) VALUES ($1, $2)`,
			st, protocol.EdgeSchemaBinLanded)
		testutil.MustNoErr(t, err, "register "+st)
	}

	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: a}}
	svc.HandlePlantClaims(env, positionsReport(t,
		`{"process_id":"LOADER-POS","styles":[{"style_id":"LOADER-POS-RUN","claims":[],"active":true}],"positions":["PLK-POS-W1"]}`))

	addr, err := plantclaims.ResolveLandingAddress(db.DB, "PLK-POS-W1", 0)
	testutil.MustNoErr(t, err, "address read")
	var claiming []string
	for _, c := range addr.Claiming {
		claiming = append(claiming, c.StationID)
	}
	if !reflect.DeepEqual(claiming, []string{a}) || len(addr.Plant) != 0 {
		t.Fatalf("a loader window station %s publishes resolves to claiming %v, last arm %d Edge(s); "+
			"want claiming [%s] and no last arm", a, claiming, len(addr.Plant), a)
	}
	sts, err := db.StationsClaimingNode("PLK-POS-W1")
	testutil.MustNoErr(t, err, "stations claiming")
	if !reflect.DeepEqual(sts, []string{a}) {
		t.Errorf("StationsClaimingNode(PLK-POS-W1) = %v, want [%s]", sts, a)
	}

	// The next report for the process is authoritative: without the position
	// the seat resolves to nobody again (replaced, not accumulated).
	svc.HandlePlantClaims(env, positionsReport(t,
		`{"process_id":"LOADER-POS","styles":[{"style_id":"LOADER-POS-RUN","claims":[],"active":true}]}`))
	sts, err = db.StationsClaimingNode("PLK-POS-W1")
	testutil.MustNoErr(t, err, "stations claiming after the replace")
	if len(sts) != 0 {
		t.Errorf("after a report with no positions, PLK-POS-W1 still resolves to %v", sts)
	}
}

// A loader's registry rows move to the station that owns its position as soon
// as that station's plant.claims names it.
// Before it arrives both registered stations hold the loader (no owner yet, as
// before); after it, only the owner. RED at 7a341f92: the other station keeps
// the row until something else re-derives it.
func TestPlantClaims_ALoaderPositionMovesItsRegistryRowsToItsStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	const a, b = "edge-own-a.line", "edge-own-b.line"
	for _, st := range []string{a, b} {
		_, err := db.Exec(`INSERT INTO edge_registry (station_id, schema_version) VALUES ($1, $2)`,
			st, protocol.EdgeSchemaBinLanded)
		testutil.MustNoErr(t, err, "register "+st)
	}
	var ntID, nodeID int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO node_types (code, name) VALUES ('NT-OWN-M', 't') RETURNING id`).Scan(&ntID), "node type")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO nodes (name, is_synthetic, node_type_id, enabled) VALUES ('OWN-M-W', false, $1, true) RETURNING id`, ntID).Scan(&nodeID), "node")
	id, err := db.CreateLoader(loaders.Loader{Name: "OWN-M", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	testutil.MustNoErr(t, err, "loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: nodeID, PayloadCode: "P-M", UOPThreshold: 10}), "home")
	rows := func(st string) int {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM demand_registry WHERE station_id = $1 AND core_node_name = 'OWN-M-W'`, st).Scan(&n), "rows")
		return n
	}
	for _, st := range []string{a, b} {
		_, _, err := db.DeriveDemandRegistry(st)
		testutil.MustNoErr(t, err, "derive "+st)
	}
	if rows(a) != 1 || rows(b) != 1 {
		t.Fatalf("before any station owns the position: rows %d and %d, want 1 and 1", rows(a), rows(b))
	}

	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: b}}
	svc.HandlePlantClaims(env, positionsReport(t,
		`{"process_id":"OWN-M-PROC","styles":[{"style_id":"OWN-M-RUN","claims":[],"active":true}],"positions":["OWN-M-W"]}`))
	if rows(a) != 0 || rows(b) != 1 {
		t.Fatalf("after %s's plant.claims names the position: rows %d (%s) and %d (%s), want 0 and 1", b, rows(a), a, rows(b), b)
	}
}
