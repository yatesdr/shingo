// A station removed from Core by mistake comes back on its own (owner ruling
// 2026-11-22, Remove station): its next heartbeat has Core ask it to register,
// it registers fresh, and the same ack has it report its claims again. Both
// sides real: the Core data service, and the Edge engine with its claims
// publisher.
//
//go:build docker

package scenarios

import (
	"slices"
	"testing"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	"shingocore/service"
	corestore "shingocore/store"
	corenodes "shingocore/store/nodes"
	coreharness "shingocore/testharness"

	"shingoedge/config"
	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// removeStation takes away the three things Remove station names: the claims
// mirror of the station, its reports, and its registry row. A stand-in while
// Core has no door.
func removeStation(t *testing.T, db *corestore.DB, uid string) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM style_claims WHERE process_id IN (SELECT process_id FROM process_styles WHERE station_uid = $1)`,
		`DELETE FROM process_positions WHERE process_id IN (SELECT process_id FROM process_styles WHERE station_uid = $1)`,
		`DELETE FROM process_styles WHERE station_uid = $1`,
		`DELETE FROM plant_claims_reports WHERE station_uid = $1`,
		`DELETE FROM edge_registry WHERE station_uid = $1`,
	} {
		_, err := db.Exec(q, uid)
		mustNil(t, err, "remove the station")
	}
}

func TestScenario_RemovedStationComesBack(t *testing.T) {
	t.Parallel()
	const uid, process, seat, name = "edge.removed", "RM-PROC", "RM-SEAT", "Line A"
	coreDB := coreharness.OpenDB(t)
	mustNil(t, coreDB.CreateNode(&corenodes.Node{Name: seat, Enabled: true}), "seat")
	_, err := coreDB.EnrollEdge(uid, name)
	mustNil(t, err, "enroll")
	svc := coremessaging.NewCoreDataService(coreDB,
		coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", nil), service.EpochAnnounce{})

	db := edgeharness.OpenDB(t)
	pid, err := db.CreateProcess(process, "", "", "", false)
	mustNil(t, err, "process")
	sid, err := db.CreateStyle("RM-S1", "", pid)
	mustNil(t, err, "style")
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: seat, Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSequential, PayloadCode: "RM-PART", UOPCapacity: 10,
	})
	mustNil(t, err, "claim")
	eng := edgeengine.New(edgeengine.Config{AppConfig: &config.Config{StationUID: uid}, DB: db, LogFunc: t.Logf})
	pub := edgemessaging.NewPlantClaimsPublisher(db, uid, 0)
	pub.Reported, pub.ReportedAll = eng.ClaimsReported, eng.ClaimsReportedAll
	eng.SetClaimsSource(pub)
	send := func(env *protocol.Envelope) error {
		data, err := env.Encode()
		if err != nil {
			return err
		}
		_, err = db.EnqueueOutbox(data, "data.send")
		return err
	}
	eng.SetSendFunc(send)
	sendData := func(subject string, body any) {
		env, err := protocol.NewDataEnvelope(subject,
			protocol.Address{Role: protocol.RoleEdge, Station: uid}, protocol.Address{Role: protocol.RoleCore}, body)
		mustNil(t, err, "build "+subject)
		mustNil(t, send(env), "enqueue "+subject)
	}

	coreSubjects := router.NewSubject()
	router.RegisterSubject(coreSubjects, protocol.SubjectEdgeHeartbeat, svc.HandleEdgeHeartbeat)
	router.RegisterSubject(coreSubjects, protocol.SubjectEdgeRegister, svc.HandleEdgeRegister)
	router.RegisterSubject(coreSubjects, protocol.SubjectPlantClaims, svc.HandlePlantClaims)
	router.RegisterSubject(coreSubjects, protocol.SubjectBinsWatch, svc.HandleBinWatch)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { coreSubjects.Dispatch(env, p) })
	coreIn := protocol.NewIngestor(nil)
	coreIn.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

	registers := 0
	edgeSubjects := router.NewSubject()
	router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeHeartbeatAck, func(_ *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
		eng.OnCoreAck(ack)
	})
	// As cmd/shingoedge main.go answers it: a register.
	router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeRegisterRequest, func(*protocol.Envelope, *protocol.EdgeRegisterRequest) {
		registers++
		sendData(protocol.SubjectEdgeRegister, &protocol.EdgeRegister{StationID: uid, Hostname: "rm-host", SchemaVersion: protocol.EdgeSchemaBinLanded})
	})
	edgeRouter := router.New[string]()
	router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
	edgeIn := protocol.NewIngestor(nil)
	edgeIn.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }
	bus := harness.NewBus(t, harness.EdgeSide{EdgeStore: db, EdgeIngestor: edgeIn},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIn})
	beat := func() {
		sendData(protocol.SubjectEdgeHeartbeat, &protocol.EdgeHeartbeat{StationID: uid, Feeds: eng.FeedDigests(), Owned: eng.OwnedDigests()})
		bus.PumpAll()
	}
	claimants := func(when string, want ...string) {
		got, err := coreDB.StationsClaimingNode(seat)
		mustNil(t, err, "claimants")
		if !slices.Equal(got, want) {
			t.Fatalf("%s: claimants of %s = %v, want %v", when, seat, got, want)
		}
	}

	beat()
	beat() // the report is quoted back: nothing left to send
	claimants("reported", uid)
	if got := eng.StationName(); got != name {
		t.Fatalf("the Edge shows %q, want %q", got, name)
	}

	removeStation(t, coreDB, uid)
	claimants("removed")

	beat()
	if registers != 1 {
		t.Fatalf("the first heartbeat after the removal drew %d register requests, want 1", registers)
	}
	claimants("one heartbeat after the removal", uid)
	e, err := coreDB.GetEdgeByUID(uid)
	mustNil(t, err, "the fresh row")
	if e.Status != "active" || e.DisplayName != "" || e.ClaimedAt != nil || e.Hostname != "rm-host" {
		t.Fatalf("the fresh row is status %q, name %q, claimed at %v, host %q; want active, no name, unclaimed, rm-host",
			e.Status, e.DisplayName, e.ClaimedAt, e.Hostname)
	}
	if got := eng.StationName(); got != name {
		t.Fatalf("the Edge shows %q on the ack that found no row, want it to keep %q", got, name)
	}

	// The name lived on the row: the next ack says there is none.
	beat()
	if got := eng.StationName(); got != "" {
		t.Fatalf("the Edge shows %q after the fresh row, want no name", got)
	}
}
