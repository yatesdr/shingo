// A station removed from Core by mistake comes back on its own (owner ruling
// 2026-11-22, Remove station): its next heartbeat has Core ask it to register,
// it registers fresh, and the same ack has it report its claims again. Both
// sides real: the Core data service, and the Edge engine with its claims
// publisher.
//
//go:build docker

package scenarios

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	"shingocore/service"
	corestore "shingocore/store"
	corebins "shingocore/store/bins"
	corenodes "shingocore/store/nodes"
	corepayloads "shingocore/store/payloads"
	coreharness "shingocore/testharness"

	"shingoedge/config"
	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// removeStation is Remove station as cmd/shingocore wires it: the node
// service door, stale only, with the data service forgetting the feeds state.
func removeStation(t *testing.T, db *corestore.DB, svc *coremessaging.CoreDataService, uid string) {
	t.Helper()
	_, err := db.Exec(`UPDATE edge_registry SET status = 'stale' WHERE station_uid = $1`, uid)
	mustNil(t, err, "the station goes stale")
	door := service.NewNodeService(db)
	door.SetStationRemovedFunc(svc.StationRemoved)
	mustNil(t, door.RemoveEdge(uid, "scenario"), "remove the station")
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
		sendData(protocol.SubjectEdgeRegister, &protocol.EdgeRegister{Hostname: "rm-host", SchemaVersion: db.AppliedSchemaVersion()})
	})
	edgeRouter := router.New[string]()
	router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
	edgeIn := protocol.NewIngestor(nil)
	edgeIn.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }
	bus := harness.NewBus(t, harness.EdgeSide{EdgeStore: db, EdgeIngestor: edgeIn},
		harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIn})
	beat := func() {
		sendData(protocol.SubjectEdgeHeartbeat, &protocol.EdgeHeartbeat{Feeds: eng.FeedDigests(), Owned: eng.OwnedDigests()})
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

	removeStation(t, coreDB, svc, uid)
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

// After Remove station Core addresses nothing to the removed uid (owner and orc
// ruling 2026-11-22, the closing task): not a refill its derived loader rows
// would make, not an order, not a correction. Run on a real Core: a loader
// whose window the removed station published, a seat only it claimed, and a
// live station beside it. The count that drives the refill comes through the
// data service, as a station's count does.
func TestScenario_RemovedStationIsAddressedByNothing(t *testing.T) {
	t.Parallel()
	const live, gone, part = "edge.rm-live", "edge.rm-gone", "RM-PART"
	core := startKeepStagedCore(t)
	db := core.eng.DB()
	announce := service.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "core"}
	h := coremessaging.NewCoreHandler(db, nil, "core", "shingo.dispatch", core.eng.Dispatcher())
	svc := coremessaging.NewCoreDataService(db, h, announce)
	svc.SetThresholdMonitor(core.eng.ThresholdMonitor())
	core.eng.SetStationRemovedFunc(svc.StationRemoved)
	for uid, name := range map[string]string{live: "Line A", gone: "Line G"} {
		_, err := db.EnrollEdge(uid, name)
		mustNil(t, err, "enroll "+uid)
		_, err = db.RegisterEdge(uid, "host-"+uid, "i", "test", "", sql.NullInt64{Int64: 60, Valid: true})
		mustNil(t, err, "register "+uid)
	}
	node := func(name string) *corenodes.Node {
		n := &corenodes.Node{Name: name, Enabled: true}
		mustNil(t, db.CreateNode(n), "node "+name)
		return n
	}
	window, seat := node("RM-W1"), node("RM-SEAT")
	node("RM-MKT")
	node("RM-DEST")
	tote := &corebins.BinType{Code: "RM-TOTE"}
	mustNil(t, db.CreateBinType(tote), "bin type")
	pl := &corepayloads.Payload{Code: part, UOPCapacity: 10}
	mustNil(t, db.CreatePayload(pl), "payload")
	mustNil(t, db.SetPayloadBinTypes(pl.ID, []int64{tote.ID}), "carrier rule")
	// 60 parts stand at the seat, above the loader's threshold of 50, so
	// nothing is refilled before the removal.
	b := &corebins.Bin{BinTypeID: tote.ID, Label: "RM-BIN", NodeID: &seat.ID, Status: "available"}
	mustNil(t, db.CreateBin(b), "a bin at the seat")
	mustNil(t, db.SetBinManifest(b.ID, `{"items":[]}`, part, 60), "manifest")
	mustNil(t, db.ConfirmBinManifest(b.ID, ""), "confirm")

	// The removed station publishes the window and claims the seat, then the
	// loader is made over the window it published.
	report := func(process string, claims []protocol.PlantClaim, positions ...string) {
		svc.HandlePlantClaims(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: gone}},
			&protocol.PlantClaimsReport{ProcessID: process, Digest: process, Positions: positions,
				Styles: []protocol.PlantClaimsStyle{{StyleID: "RUN", Active: true, Claims: claims}}})
	}
	report("RM-LOADER", nil, window.Name)
	report("RM-WELD", []protocol.PlantClaim{{CoreNodeName: seat.Name, Role: protocol.ClaimRoleConsume,
		SwapMode: protocol.SwapModeSequential, PayloadCode: part, UOPCapacity: 10}})
	loader, err := core.eng.LoaderService().CreateLoader(service.LoaderCreate{Name: "RM-LDR", Role: "produce",
		Replenishment: "threshold", InboundSource: "RM-MKT", OutboundDest: "RM-DEST"})
	mustNil(t, err, "loader")
	mustNil(t, core.eng.LoaderService().SetPayload(loader, part, 50), "the part, threshold 50")
	mustNil(t, core.eng.LoaderService().SetHome(loader, window.ID, "", "home", 0), "the window")
	var owned int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM demand_registry WHERE station_uid = $1`, gone).Scan(&owned), "registry")
	if owned == 0 {
		t.Fatal("setup: the removed station owns no loader row")
	}
	var before int
	mustNil(t, db.QueryRow(`SELECT count(*) FROM orders`).Scan(&before), "orders")
	if before != 0 {
		t.Fatalf("setup: %d orders before the removal, want none", before)
	}

	_, err = db.Exec(`UPDATE edge_registry SET status = 'stale' WHERE station_uid = $1`, gone)
	mustNil(t, err, "the station goes stale")
	var mark int64
	mustNil(t, db.QueryRow(`SELECT COALESCE(max(id), 0) FROM outbox`).Scan(&mark), "outbox mark")
	mustNil(t, core.eng.NodeService().RemoveEdge(gone, "scenario"), "remove the station")

	// A person counts the bin at the seat only the removed station claimed:
	// 8, below the threshold. Then a count from the live station drives the
	// threshold check on the inbound path, as a station's count does.
	fresh, err := db.GetBin(b.ID)
	mustNil(t, err, "bin")
	_, err = core.eng.BinService().RecordCount(fresh, 8, "clerk")
	mustNil(t, err, "the count")
	fresh, err = db.GetBin(b.ID)
	mustNil(t, err, "bin")
	now := time.Now().UTC()
	svc.HandleBinUOPDelta(&protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: live}},
		&protocol.BinUOPDelta{BinID: b.ID, PayloadCode: part, Delta: -1, Reason: protocol.ReasonConsumeTick,
			SequenceID: 1, Epoch: fresh.DeltaEpoch, WindowStart: now.Add(-time.Second), WindowEnd: now})

	refills := func() (to map[string]int) {
		rows, err := db.Query(`SELECT station_uid FROM orders WHERE delivery_node = $1 OR source_node = $1`, window.Name)
		mustNil(t, err, "orders")
		defer rows.Close()
		to = map[string]int{}
		for rows.Next() {
			var st string
			mustNil(t, rows.Scan(&st), "scan")
			to[st]++
		}
		return to
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(refills()) == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	got := refills()
	t.Logf("refill orders by station: %v", got)
	if got[gone] != 0 || got[live] == 0 {
		t.Errorf("refill orders by station %v, want some to %s and none to %s", got, live, gone)
	}
	rows, err := db.Query(`SELECT msg_type FROM outbox WHERE id > $1 AND dst_station = $2`, mark, gone)
	mustNil(t, err, "outbox")
	defer rows.Close()
	var sent []string
	for rows.Next() {
		var m string
		mustNil(t, rows.Scan(&m), "scan")
		sent = append(sent, m)
	}
	if len(sent) != 0 {
		t.Errorf("after the removal Core addressed %v to %s, want nothing", sent, gone)
	}
}
