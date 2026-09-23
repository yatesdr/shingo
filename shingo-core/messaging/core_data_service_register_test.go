//go:build docker

package messaging

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

func registerEnv(stationID string) *protocol.Envelope {
	return &protocol.Envelope{
		Src: protocol.Address{Role: protocol.RoleEdge, Station: stationID},
		Dst: protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}
}

func stationThresholds(t *testing.T, db *store.DB, stationID string) map[string]int {
	t.Helper()
	rows, err := db.DB.Query(`SELECT core_node_name || '/' || payload_code, replenish_uop_threshold
		FROM demand_registry WHERE station_id=$1`, stationID)
	testutil.MustNoErr(t, err, "read demand_registry")
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var thr int
		testutil.MustNoErr(t, rows.Scan(&k, &thr), "scan")
		out[k] = thr
	}
	return out
}

// seedRegisterLoader is one dedicated loader with one payload-bearing home.
func seedRegisterLoader(t *testing.T, db *store.DB, nodeName, payload string, threshold int) (loaderID, nodeID int64) {
	t.Helper()
	id, err := db.CreateLoader(loaders.Loader{Name: "L-" + nodeName, Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentThreshold})
	testutil.MustNoErr(t, err, "create loader")
	n := &nodes.Node{Name: nodeName, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create node")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: n.ID,
		PayloadCode: payload, UOPThreshold: threshold}), "upsert home")
	return id, n.ID
}

// TestHandleEdgeRegisterPin_DerivesRegistry pins the register writer: a
// (re)connect derives the station's registry from the aggregate, and a second
// register after the loader is archived commits the now-empty derivation.
func TestHandleEdgeRegisterPin_DerivesRegistry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const st = "ST-REG-PIN"
	_, err := db.EnrollEdge(st, "", st)
	testutil.MustNoErr(t, err, "enroll edge")
	id, _ := seedRegisterLoader(t, db, "REG-PIN-SLOT", "P-REG", 12)

	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})

	got := stationThresholds(t, db, st)
	if len(got) != 1 || got["REG-PIN-SLOT/P-REG"] != 12 {
		t.Fatalf("registry after register = %v, want REG-PIN-SLOT/P-REG=12", got)
	}

	testutil.MustNoErr(t, db.DeleteLoader(id), "archive loader")
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})
	if got := stationThresholds(t, db, st); len(got) != 0 {
		t.Fatalf("registry after archive + register = %v, want empty — every loader archived is config with no demand", got)
	}
}

// TestHandleEdgeRegister_UnresolvableNodeKeepsRows is the refusal on the
// register writer: a (re)connect whose derivation could not resolve a home's
// node must leave the station's rows where they are, and say so.
//
// Not parallel: it reads the process-global logger.
func TestHandleEdgeRegister_UnresolvableNodeKeepsRows(t *testing.T) {
	db := testdb.Open(t)
	const st = "ST-REG-RED"
	_, err := db.EnrollEdge(st, "", st)
	testutil.MustNoErr(t, err, "enroll edge")
	_, nodeID := seedRegisterLoader(t, db, "REG-RED-SLOT", "P-RED", 7)

	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})
	if got := stationThresholds(t, db, st); got["REG-RED-SLOT/P-RED"] != 7 {
		t.Fatalf("seeded registry = %v, want REG-RED-SLOT/P-RED=7", got)
	}

	_, err = db.DB.Exec(`ALTER TABLE bin_loader_homes DROP CONSTRAINT IF EXISTS bin_loader_homes_position_node_id_fkey`)
	testutil.MustNoErr(t, err, "drop fk")
	_, err = db.DB.Exec(`DELETE FROM nodes WHERE id=$1`, nodeID)
	testutil.MustNoErr(t, err, "delete node")

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, &buf))
	svc.HandleEdgeRegister(registerEnv(st), &protocol.EdgeRegister{StationID: st, Hostname: "h", Version: "v"})
	log.SetOutput(prev)

	if got := stationThresholds(t, db, st); got["REG-RED-SLOT/P-RED"] != 7 {
		t.Errorf("registry after an unresolvable register = %v, want REG-RED-SLOT/P-RED=7 kept", got)
	}
	if out := buf.String(); !strings.Contains(out, "station="+st) || !strings.Contains(out, "outcome=refused") || !strings.Contains(out, "node_gone=1") {
		t.Errorf("log = %q, want one derive line for %s with outcome=refused and node_gone=1", out, st)
	}
}
