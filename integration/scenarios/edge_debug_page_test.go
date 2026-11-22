// The Edge's debug page (Kafka Direct: shingoedge www handlers_manual_message.go)
// sends its edge.register and edge.heartbeat as the station it runs on, so Core
// takes each as that station's own and its answer goes to that station's Edge.
// Both sides real: Core's register, heartbeat and owned-family handlers
// (shingocore messaging HandleEdgeRegister, HandleEdgeHeartbeat, HandleBinWatch,
// HandlePlantClaims, HandleEdgeCells) and its landing (service BinService); the
// Edge's engine, its heartbeater's body (shingoedge messaging Heartbeater, wired
// as cmd/shingoedge main.go wires it) and the page itself, served by the Edge's
// own router (www.NewRouter) and posted as manual-message.js posts it.
//
//go:build docker

package scenarios

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/debuglog"
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
	"shingoedge/store"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
	edgewww "shingoedge/www"
)

const (
	debugStation = "edge.debug-page"
	debugZone    = "America/Chicago"
)

// debugRig is one station's Edge, with its web server, and the Core it talks to
// over the bus, booted as a plant Edge boots and heartbeating until every family
// it owns has converged.
type debugRig struct {
	t      *testing.T
	core   *corestore.DB
	sd     *coreharness.StandardData
	edgeDB *store.DB
	eng    *edgeengine.Engine
	hb     *edgemessaging.Heartbeater
	bus    *harness.Bus
	page   string       // the Edge's web server
	cookie *http.Cookie // an admin's session on it

	mu      sync.Mutex
	reached map[string]int // Edge-sent subject → messages that reached Core
	acks    int            // heartbeat acks the Edge took
}

// countIn routes subject to handle on Core's subjects, counting each message
// that reaches it.
func countIn[T any](r *debugRig, subjects *router.SubjectRouter, subject string, handle func(*protocol.Envelope, *T)) {
	router.RegisterSubject(subjects, subject, func(env *protocol.Envelope, body *T) {
		r.mu.Lock()
		r.reached[subject]++
		r.mu.Unlock()
		handle(env, body)
	})
}

func newDebugRig(t *testing.T) *debugRig {
	t.Helper()
	r := &debugRig{t: t, reached: map[string]int{}}
	r.core = coreharness.OpenDB(t)
	r.sd = coreharness.SetupStandardData(t, r.core)
	_, err := r.core.EnrollEdge(debugStation, "")
	mustNil(t, err, "enroll the station")
	svc := coremessaging.NewCoreDataService(r.core,
		coremessaging.NewCoreHandler(r.core, nil, "core", "shingo.dispatch", nil), service.EpochAnnounce{})
	// The ack names the kinds Core answers, as cmd/shingocore hands it its ask
	// server's (SetAskKinds).
	svc.SetAskKinds(protocol.AskKinds)

	// Core: what a boot, its heartbeats and the page send, as cmd/shingocore
	// buildSubjectRouter routes them, each counted on the way in.
	coreSubjects := router.NewSubject()
	countIn(r, coreSubjects, protocol.SubjectEdgeRegister, svc.HandleEdgeRegister)
	countIn(r, coreSubjects, protocol.SubjectEdgeHeartbeat, svc.HandleEdgeHeartbeat)
	countIn(r, coreSubjects, protocol.SubjectBinsWatch, svc.HandleBinWatch)
	countIn(r, coreSubjects, protocol.SubjectPlantClaims, svc.HandlePlantClaims)
	countIn(r, coreSubjects, protocol.SubjectEdgeCells, svc.HandleEdgeCells)
	coreRouter := router.New[string]()
	router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { coreSubjects.Dispatch(env, p) })
	coreIngestor := protocol.NewIngestor(nil)
	coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

	// The Edge: a process claiming Core's line node, so the node is on its bins
	// watch list (a process node) and in its plant claims, and a counted
	// reporting point, so it has a cell. Every family it owns holds something.
	r.edgeDB = edgeharness.OpenDB(t)
	db := r.edgeDB
	pid, err := db.CreateProcess("DBG-PROC", "", "", "", false)
	mustNil(t, err, "create the process")
	sid, err := db.CreateStyle("DBG-S1", "", pid)
	mustNil(t, err, "create the style")
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: r.sd.LineNode.Name, Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSequential, PayloadCode: r.sd.Payload.Code, UOPCapacity: 10,
	})
	mustNil(t, err, "claim the line node")
	_, err = db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: r.sd.LineNode.Name, Code: "N1",
		Name: r.sd.LineNode.Name, Enabled: true})
	mustNil(t, err, "create the process node")
	_, err = db.CreateReportingPoint("DBG-PLC", "COUNT", sid)
	mustNil(t, err, "create the reporting point")
	mustNil(t, db.SetProcessCounter(pid, "DBG-PLC", "COUNT", true, "", true), "count the process")

	cfg := config.Defaults()
	cfg.StationUID = debugStation
	cfg.Timezone = debugZone
	r.eng = edgeengine.New(edgeengine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
	// As cmd/shingoedge main.go wires them: claims through the publisher, cells
	// from the counted reporting points, and the engine's own sends (its watch
	// list, its cells) and the page's through the outbox to the bus.
	pub := edgemessaging.NewPlantClaimsPublisher(db, debugStation, 0)
	pub.Reported, pub.ReportedAll = r.eng.ClaimsReported, r.eng.ClaimsReportedAll
	r.eng.SetClaimsSource(pub)
	catalog := func() ([]protocol.CellCatalogEntry, error) {
		pts, err := db.ListCountedReportingPoints()
		if err != nil {
			return nil, err
		}
		return edgemessaging.BuildCellCatalog(pts), nil
	}
	r.eng.SetCellCatalogFunc(catalog)
	r.eng.SetSendFunc(r.enqueue)
	r.eng.Start()
	t.Cleanup(r.eng.Stop)

	// The heartbeater, built and wired as main.go builds and wires it. It sends
	// nothing itself here (no Kafka client): the rig sends its heartbeat's body
	// through the outbox, as the page does.
	r.hb = edgemessaging.NewHeartbeater(nil, debugStation, "v-test", "inst-debug", "shingo.orders")
	r.hb.TimezoneFn = func() string {
		c := r.eng.AppConfig()
		c.RLock()
		defer c.RUnlock()
		return c.Timezone
	}
	r.hb.SchemaVersionFn = db.AppliedSchemaVersion
	r.hb.TickLagFn = func() (int64, int64, bool) {
		pending, age, err := r.eng.ProductionTickLag()
		return pending, age, err == nil
	}
	r.hb.FeedsFn = r.eng.FeedDigests
	r.hb.OwnedFn = r.eng.OwnedDigests
	r.hb.CoreSpeaksFeedsFn = r.eng.CoreSpeaksFeeds
	r.eng.SetHeartbeatBodyFunc(r.hb.HeartbeatBody)

	// The Edge's inbound doors for what this rig's Core sends it, as
	// cmd/shingoedge buildInboundRouter routes them.
	edgeSubjects := router.NewSubject()
	router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeHeartbeatAck, func(_ *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
		r.mu.Lock()
		r.acks++
		r.mu.Unlock()
		r.eng.OnCoreAck(ack)
	})
	router.RegisterSubject(edgeSubjects, protocol.SubjectBinsSnapshot, func(_ *protocol.Envelope, snap *protocol.NodeBinsSnapshot) {
		if err := r.eng.ApplyNodeBinsSnapshot(*snap); err != nil {
			t.Errorf("apply the bins snapshot: %v", err)
		}
	})
	router.RegisterSubject(edgeSubjects, protocol.SubjectNodeListResponse, func(_ *protocol.Envelope, resp *protocol.NodeListResponse) {
		r.eng.ApplyNodeList(resp)
	})
	router.RegisterSubject(edgeSubjects, protocol.SubjectCatalogPayloadsResponse, func(_ *protocol.Envelope, resp *protocol.CatalogPayloadsResponse) {
		r.eng.ApplyCatalog(resp)
	})
	edgeRouter := router.New[string]()
	router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
	edgeIngestor := protocol.NewIngestor(func(hdr *protocol.RawHeader) bool {
		return hdr.Dst.Station == debugStation || hdr.Dst.Station == protocol.StationBroadcast
	})
	edgeIngestor.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }
	r.bus = harness.NewBus(t,
		harness.EdgeSide{EdgeStore: db, EdgeIngestor: edgeIngestor},
		harness.CoreSide{CoreStore: r.core, CoreIngestor: coreIngestor})

	// The Edge's web server, as main.go starts it, and an admin signed in: the
	// page is an admin page.
	dbg, err := debuglog.New(64, nil)
	mustNil(t, err, "debug log")
	_, handler, stop := edgewww.NewRouter(r.eng, dbg, nil)
	t.Cleanup(stop)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	r.page = srv.URL
	r.cookie = r.login()

	// The boot burst, as Heartbeater.SendBootBurst sends it but for the node
	// list and catalog asks: the register as the heartbeater builds it, the
	// watch list, a heartbeat. Then a heartbeat a minute later, by which every
	// family Core lacked (the claims; the cells, which the register leaves
	// unconfirmed) has been sent, and one more, which answers every family at
	// the Edge's quote.
	cells, err := catalog()
	mustNil(t, err, "read the cell catalog")
	r.send(protocol.SubjectEdgeRegister, &protocol.EdgeRegister{Hostname: "debug-host",
		Instance: "inst-debug", Version: "v-test", Timezone: debugZone, SchemaVersion: db.AppliedSchemaVersion(), Catalog: cells})
	r.eng.PublishBinWatchAtBoot(func(l *protocol.BinWatchList) error {
		r.send(protocol.SubjectBinsWatch, l)
		return nil
	})
	for i := 0; i < 3; i++ {
		r.beat()
	}
	if flags := r.eng.FeedFlags(); len(flags) != 0 {
		t.Fatalf("after the boot the Edge flags %v, want none: a family has not converged", flags)
	}
	return r
}

// enqueue is the Edge's send: its outbox, which the bus drains to Core.
func (r *debugRig) enqueue(env *protocol.Envelope) error {
	data, err := env.Encode()
	if err != nil {
		return err
	}
	_, err = r.edgeDB.EnqueueOutbox(data, "data.send")
	return err
}

// send is one message from the Edge and everything it sets moving, until both
// sides are quiet.
func (r *debugRig) send(subject string, body any) {
	r.t.Helper()
	env, err := protocol.NewDataEnvelope(subject,
		protocol.Address{Role: protocol.RoleEdge, Station: debugStation}, protocol.Address{Role: protocol.RoleCore}, body)
	mustNil(r.t, err, "build "+subject)
	mustNil(r.t, r.enqueue(env), "enqueue "+subject)
	r.bus.PumpAll()
}

// beat is one of the heartbeater's heartbeats, its own body.
func (r *debugRig) beat() { r.send(protocol.SubjectEdgeHeartbeat, r.hb.HeartbeatBody()) }

// postPage posts one message from the page as manual-message.js posts it
// ({type, payload} to /api/manual-message, signed in) and lets everything it
// sets moving land.
func (r *debugRig) postPage(typ string, payload any) {
	r.t.Helper()
	body, err := json.Marshal(map[string]any{"type": typ, "payload": payload})
	mustNil(r.t, err, "encode the page's "+typ)
	req, err := http.NewRequest(http.MethodPost, r.page+"/api/manual-message", bytes.NewReader(body))
	mustNil(r.t, err, "build the page's "+typ)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(r.cookie)
	res, err := http.DefaultClient.Do(req)
	mustNil(r.t, err, "post the page's "+typ)
	defer res.Body.Close()
	answer, err := io.ReadAll(res.Body)
	mustNil(r.t, err, "read the page's answer")
	if res.StatusCode != http.StatusOK {
		r.t.Fatalf("the page's %s answered %d: %s", typ, res.StatusCode, answer)
	}
	r.bus.PumpAll()
}

// login signs in the first admin, which the first sign-in creates.
func (r *debugRig) login() *http.Cookie {
	r.t.Helper()
	form := url.Values{"username": {"debug"}, "password": {"debug-password"}}
	req, err := http.NewRequest(http.MethodPost, r.page+"/login", strings.NewReader(form.Encode()))
	mustNil(r.t, err, "build the sign-in")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	mustNil(r.t, err, "sign in")
	res.Body.Close()
	for _, c := range res.Cookies() {
		return c
	}
	r.t.Fatalf("the sign-in set no cookie (status %d)", res.StatusCode)
	return nil
}

// reachedCore is how many messages of subject from the Edge have reached Core.
func (r *debugRig) reachedCore(subject string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reached[subject]
}

// acksTaken is how many heartbeat acks the Edge has taken.
func (r *debugRig) acksTaken() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acks
}

// answered checks the station is as its own heartbeat's ack leaves it: the Edge
// asks every kind Core answers, may decide from its bins copy, and Core holds
// the station's zone.
func (r *debugRig) answered(when string) {
	r.t.Helper()
	for _, kind := range protocol.AskKinds() {
		if !r.eng.CoreAnswers(kind) {
			r.t.Errorf("%s: the Edge refuses a %s ask, as for a Core that answers none: the last ack named no such kind", when, kind)
		}
	}
	if ok, why := r.eng.BinsActable(); !ok {
		r.t.Errorf("%s: the Edge's bins copy is not actable (%s): every decision that reads it refuses", when, why)
	}
	var zone string
	mustNil(r.t, r.core.QueryRow(`SELECT COALESCE(timezone, '') FROM edge_registry WHERE station_uid = $1`, debugStation).Scan(&zone),
		"read the station's zone on Core")
	if zone != debugZone {
		r.t.Errorf("%s: Core holds the station's zone as %q, want %s", when, zone, debugZone)
	}
}

// landingVersion is the schema version Core's landing reads for the station at
// the line node (plantclaims.ResolveLandingAddress, the read sendLanding gates
// on), from whichever arm addresses it.
func (r *debugRig) landingVersion() int {
	r.t.Helper()
	addr, err := r.core.ResolveLandingAddress(r.core.DB, r.sd.LineNode.Name, 0)
	mustNil(r.t, err, "read the landing address")
	for _, st := range append(append(addr.Claiming, addr.Order), addr.Plant...) {
		if st.StationID == debugStation {
			return st.SchemaVersion
		}
	}
	r.t.Fatalf("Core's landing at %s addresses %+v, not the station", r.sd.LineNode.Name, addr)
	return 0
}

// newCarrier is a carrier in storage, and the last row of Core's outbox before
// it moves.
func (r *debugRig) newCarrier(label string) (binID, mark int64) {
	r.t.Helper()
	bin := coreharness.CreateBinAtNode(r.t, r.core, r.sd.Payload.Code, r.sd.StorageNode.ID, label)
	mustNil(r.t, r.core.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM outbox`).Scan(&mark), "read Core's outbox")
	return bin.ID, mark
}

// sentToStation counts what Core has written to the station since mark: its
// landings, and the Bounds Core writes in their place to a station too old for
// one.
func (r *debugRig) sentToStation(mark int64) (landed, bound int) {
	r.t.Helper()
	rows, err := r.core.Query(`SELECT msg_type FROM outbox WHERE id > $1 AND dst_station = $2`, mark, debugStation)
	mustNil(r.t, err, "read what Core wrote to the station")
	defer rows.Close()
	for rows.Next() {
		var msgType string
		mustNil(r.t, rows.Scan(&msgType), "scan Core's outbox")
		switch msgType {
		case "data." + protocol.SubjectBinLanded:
			landed++
		case "data." + protocol.SubjectUOPAdjustment:
			bound++
		}
	}
	mustNil(r.t, rows.Err(), "read Core's outbox")
	return landed, bound
}

// The page's register carries the Edge's schema version (engine SchemaVersion),
// so the station keeps its landings. Core keeps the last register's version, and
// a station below protocol.EdgeSchemaBinLanded gets a Bound in place of a
// set-down's landing and nothing for an order's final drop (service
// BinService.sendLanding). The page's register used to carry none: Core stored
// v0, and the station got those until its next real register, which comes at a
// restart, a reconnect or when Core asks for one.
func TestScenario_TheDebugPagesRegisterKeepsTheStationsLandings(t *testing.T) {
	t.Parallel()
	r := newDebugRig(t)
	applied := r.edgeDB.AppliedSchemaVersion()
	if applied < protocol.EdgeSchemaBinLanded {
		t.Fatalf("the Edge's database stands at v%d, below protocol.EdgeSchemaBinLanded (%d): no landing to keep", applied, protocol.EdgeSchemaBinLanded)
	}
	if got := r.landingVersion(); got != applied {
		t.Fatalf("after the Edge's own register Core's landing reads the station at v%d, want v%d", got, applied)
	}

	registers := r.reachedCore(protocol.SubjectEdgeRegister)
	r.postPage("edge.register", map[string]any{"version": "dev"})
	if got := r.reachedCore(protocol.SubjectEdgeRegister) - registers; got != 1 {
		t.Fatalf("%d registers from the page reached Core, want 1", got)
	}
	if got := r.landingVersion(); got != applied {
		t.Errorf("after the page's register Core's landing reads the station at v%d, want the Edge's v%d", got, applied)
	}

	// A set-down at the station's node, and a final drop at a node no station
	// claims, which Core tells every Edge (the last arm): this one alone here.
	announce := service.EpochAnnounce{Topic: "shingo.dispatch", CoreStation: "core.test"}
	bins := service.NewBinService(r.core, service.NewBinManifestService(r.core, announce))
	id, mark := r.newCarrier("DBG-SET-DOWN")
	_, err := bins.ApplyIntermediateStore(id, r.sd.LineNode.ID, false, nil, 0)
	mustNil(t, err, "set the carrier down")
	if landed, bound := r.sentToStation(mark); landed != 1 || bound != 0 {
		t.Errorf("a carrier set down at the station's node after the page's register: %d landing(s) and %d Bound(s) "+
			"to the station, want 1 and 0: Core takes it for an Edge too old to bind a landing", landed, bound)
	}
	drop := &corenodes.Node{Name: "DBG-LINE-2", Enabled: true}
	mustNil(t, r.core.CreateNode(drop), "create the drop node")
	id, mark = r.newCarrier("DBG-DELIVERED")
	_, err = bins.ApplyArrival(id, drop.ID, false, nil, 0)
	mustNil(t, err, "deliver the carrier")
	if landed, bound := r.sentToStation(mark); landed != 1 || bound != 0 {
		t.Errorf("a carrier delivered after the page's register: %d landing(s) and %d Bound(s) to the station, "+
			"want 1 and 0: with no landing the station never binds it", landed, bound)
	}
}

// The page's heartbeat is the heartbeater's own body (engine HeartbeatBody), so
// Core's ack to it, which goes to the station's Edge as every ack does, answers
// the Edge's own digests. After it the Edge still asks every kind Core answers
// and may decide from its bins copy, and Core still holds the station's zone and
// every family the Edge owns: the next heartbeat sends none of them again.
//
// The page used to send a heartbeat naming the station alone, which Core takes
// as an older Edge's (HandleEdgeHeartbeat): an ack with no ask kinds and no feed
// confirmed, the station's watch list forgotten (binWatchFor) and its zone
// blanked. On the Edge, OnCoreAck took that ack as Core's last word: every ask
// refused and every bins decision held until the next real heartbeat, up to a
// minute, and the watch list sent again then.
func TestScenario_TheDebugPagesHeartbeatLeavesTheStationAsItWas(t *testing.T) {
	t.Parallel()
	r := newDebugRig(t)
	r.answered("after the boot")

	beats, acks := r.reachedCore(protocol.SubjectEdgeHeartbeat), r.acksTaken()
	families := []string{protocol.SubjectBinsWatch, protocol.SubjectPlantClaims, protocol.SubjectEdgeCells}
	sent := map[string]int{}
	for _, s := range families {
		sent[s] = r.reachedCore(s)
	}
	r.postPage("edge.heartbeat", map[string]any{})
	if got := r.reachedCore(protocol.SubjectEdgeHeartbeat) - beats; got != 1 {
		t.Fatalf("%d heartbeats from the page reached Core, want 1", got)
	}
	if got := r.acksTaken() - acks; got != 1 {
		t.Fatalf("the Edge took %d acks after the page's heartbeat, want 1: Core's answer to it", got)
	}
	r.answered("after Core's ack to the page's heartbeat")

	// The Edge's next heartbeat: Core held every family all along, so none is
	// sent again.
	r.beat()
	for _, s := range families {
		if got := r.reachedCore(s) - sent[s]; got != 0 {
			t.Errorf("the Edge sent %s to Core %d time(s) at the heartbeat after the page's, want 0: Core had lost it", s, got)
		}
	}
	r.answered("after the next heartbeat")
}
