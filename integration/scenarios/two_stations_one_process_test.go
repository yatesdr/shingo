// Two stations publishing one process, both sides real: Core's plant_claims_reports
// rows and the claims its heartbeat ack quotes (shingocore messaging
// HandlePlantClaims, HandleEdgeHeartbeat; store/plantclaims reports.go), and
// each Edge's own plant spec, publisher and resend guard (the engine's owned
// map: its quote on the heartbeat, its answer in Engine.OnCoreAck, its sends
// through shingoedge messaging PlantClaimsPublisher.SendClaims, wired as
// cmd/shingoedge main.go wires them). The Edges' clock is a testing/synctest
// bubble's, so ten minutes of heartbeats run in no real time.
//
//go:build docker

package scenarios

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coreharness "shingocore/testharness"

	"shingoedge/config"
	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgemessaging "shingoedge/messaging"
	"shingoedge/store"
	"shingoedge/store/processes"
	edgeharness "shingoedge/testharness"
)

// twinEdge is one of the two stations: its engine and store.
type twinEdge struct {
	uid string
	eng *edgeengine.Engine
	db  *store.DB
}

// Each station heartbeats every minute, the second 30 s after the first. Core
// keeps one report row per station and process (plant_claims_reports, keyed by
// both since v186) and quotes each station its own: so each station's first
// report is quoted back to it on every ack after, and neither re-sends, while
// the mirror holds the last writer's. Core's "reported by both" flag goes up on
// both stations at the second station's first report and stays up; neither
// Edge flags anything of its own. All four are asserted below.
//
// Until v186 a process had one report row, the last writer's: each report took
// the process off the other station's ack, and each station re-sent every 2
// minutes, forever (the first on the guard, protocol/resend Guard; the second
// on the "Core lost what it held" rule, resend.Record.Send), and the first
// station's Edge flagged the process not converging.
func TestScenario_TwoStationsOneProcess_EachSendsOnce(t *testing.T) {
	t.Parallel()
	const process = "TWIN-PROC"

	coreDB := coreharness.OpenDB(t)
	// The bubble's clock should not meet a pool timer running on the real one:
	// no lifetime or idle limit, so database/sql runs no connection cleaner,
	// and the connections the run should need opened here, outside the bubble.
	coreDB.SetConnMaxLifetime(0)
	coreDB.SetConnMaxIdleTime(0)
	coreDB.SetMaxIdleConns(8)
	var held []*sql.Conn
	for i := 0; i < 6; i++ {
		c, err := coreDB.Conn(context.Background())
		mustNil(t, err, "open a Core connection")
		held = append(held, c)
	}
	for _, c := range held {
		mustNil(t, c.Close(), "return a Core connection")
	}

	twins := make([]*twinEdge, 2)
	for i, uid := range []string{"edge.twin-a", "edge.twin-b"} {
		_, err := coreDB.EnrollEdge(uid, "")
		mustNil(t, err, "enroll "+uid)
		db := edgeharness.OpenDB(t)
		pid, err := db.CreateProcess(process, "", "", "", false)
		mustNil(t, err, "create process on "+uid)
		sid, err := db.CreateStyle("TWIN-S1", "", pid)
		mustNil(t, err, "create style on "+uid)
		// A different node on each station, so the two reports differ as two
		// stations' specs for one process name would.
		_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: []string{"TW-N1", "TW-N2"}[i], Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapModeSequential, PayloadCode: "SYN-TW-1", UOPCapacity: 10,
		})
		mustNil(t, err, "claim on "+uid)
		cfg := &config.Config{StationUID: uid}
		eng := edgeengine.New(edgeengine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
		// The claims family reads and sends through the publisher, and the
		// publisher's own reads report back, as cmd/shingoedge main.go wires them.
		pub := edgemessaging.NewPlantClaimsPublisher(db, uid, 0)
		pub.Reported, pub.ReportedAll = eng.ClaimsReported, eng.ClaimsReportedAll
		eng.SetClaimsSource(pub)
		// The engine's direct sends (its bins watch list) ride the outbox to the
		// bus, as main.go's data sender publishes them.
		eng.SetSendFunc(func(env *protocol.Envelope) error {
			data, err := env.Encode()
			if err != nil {
				return err
			}
			_, err = db.EnqueueOutbox(data, "data.send")
			return err
		})
		twins[i] = &twinEdge{uid: uid, eng: eng, db: db}
	}
	a, b := twins[0], twins[1]

	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		reports := map[string][]time.Duration{} // station → when Core received each of its reports

		// Core: main's tables with no engine, routing the heartbeat and the
		// claims report, each report of the process timed on the way in. The
		// ack carries the bins watch digest, and an Edge re-sends a list Core
		// does not hold under the same guard: Core takes it too.
		core := coreharness.NewTables(t, coreDB, nil,
			coreharness.RoutesOnly(protocol.SubjectEdgeHeartbeat, protocol.SubjectBinsWatch, protocol.SubjectPlantClaims),
			coreharness.Tap(func(env *protocol.Envelope, _ string) {
				var data protocol.Data
				mustNil(t, env.DecodePayload(&data), "decode a claims report's envelope")
				var r protocol.PlantClaimsReport
				mustNil(t, json.Unmarshal(data.Body, &r), "decode a claims report")
				if r.ProcessID == process {
					mu.Lock()
					reports[env.Src.Station] = append(reports[env.Src.Station], time.Since(start))
					mu.Unlock()
				}
			}, protocol.SubjectPlantClaims))

		// One Edge-side door for both: the ack goes to the station it names.
		edgeSubjects := router.NewSubject()
		router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeHeartbeatAck, func(env *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
			for _, x := range twins {
				if x.uid == env.Dst.Station {
					x.eng.OnCoreAck(ack)
				}
			}
		})
		edgeRouter := router.New[string]()
		router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
		edgeIngestor := protocol.NewIngestor(nil)
		edgeIngestor.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }

		buses := map[string]*harness.Bus{}
		for _, x := range twins {
			buses[x.uid] = harness.NewBus(t,
				harness.EdgeSide{EdgeStore: x.db, EdgeIngestor: edgeIngestor},
				harness.CoreSide{CoreStore: coreDB, CoreIngestor: core.Ingestor})
		}
		// beat is one heartbeat from x and everything it sets moving: Core's
		// ack, the Edge's reconcile, the report it sends, until both sides are
		// quiet. Feeds and Owned are the engine's own (FeedDigests, OwnedDigests,
		// as cmd/shingoedge wires the heartbeater); the feeds Core sends back are
		// not applied here, which leaves the claims, the bins watch list and the
		// ack's quote of each as they are.
		beat := func(x *twinEdge) {
			env, err := protocol.NewDataEnvelope(protocol.SubjectEdgeHeartbeat,
				protocol.Address{Role: protocol.RoleEdge, Station: x.uid}, protocol.Address{Role: protocol.RoleCore},
				&protocol.EdgeHeartbeat{Feeds: x.eng.FeedDigests(), Owned: x.eng.OwnedDigests()})
			mustNil(t, err, "build heartbeat")
			data, err := env.Encode()
			mustNil(t, err, "encode heartbeat")
			_, err = x.db.EnqueueOutbox(data, "data."+protocol.SubjectEdgeHeartbeat)
			mustNil(t, err, "enqueue heartbeat")
			buses[x.uid].PumpAll()
		}
		bothFlagged := func(when string) {
			for _, x := range twins {
				if !slices.ContainsFunc(core.Data.EdgeFeedFlags(x.uid), func(f string) bool {
					return strings.Contains(f, "process "+process+" reported by both")
				}) {
					t.Errorf("%s: Core's flags for %s are %v, want the process reported by both", when, x.uid, core.Data.EdgeFeedFlags(x.uid))
				}
			}
		}

		for m := 0; m < 10; m++ {
			beat(a)
			time.Sleep(30 * time.Second)
			beat(b)
			bothFlagged(time.Since(start).String())
			time.Sleep(30 * time.Second)
		}

		at := func(offsets ...string) []time.Duration {
			out := make([]time.Duration, len(offsets))
			for i, o := range offsets {
				d, err := time.ParseDuration(o)
				mustNil(t, err, "duration "+o)
				out[i] = d
			}
			return out
		}
		want := map[string][]time.Duration{
			a.uid: at("0s"),
			b.uid: at("30s"),
		}
		for _, x := range twins {
			if got := reports[x.uid]; !slices.Equal(got, want[x.uid]) {
				t.Errorf("%s's reports of %s reached Core at %v, want %v: each station sends once, and is quoted its "+
					"own digest after (plant_claims_reports keyed by station and process); a re-send every 2 minutes "+
					"is the one-row-per-process key back", x.uid, process, got, want[x.uid])
			}
		}
		for _, x := range twins {
			if got := x.eng.FeedFlags(); len(got) != 0 {
				t.Errorf("%s's own flags are %v, want none: its one send is quoted back on every ack after", x.uid, got)
			}
		}
	})
}

// Every Edge-owned family on two Edges, both sides real: each Edge's plant
// claims (its publisher), its bins watch list (a process node: every process
// node is on the list, shingoedge service withClaimPlaces) and its cell catalog
// (a reporting point on its process's counter, read as cmd/shingoedge main.go
// reads it), quoted on its heartbeat (Engine.OwnedDigests) and taken by Core's
// own handlers (HandleEdgeHeartbeat's owned answer, HandlePlantClaims,
// HandleBinWatch, HandleEdgeCells), after the register a boot burst sends with
// the catalog on it. Heartbeats every minute, the second Edge 30 s after the
// first, for seven minutes: past the Edge's five-minute rebuild of every family
// (engine ownedFloor).
//
// At the first ack Core holds none of an Edge's claims and none of its watch
// list, and its cells with no digest (the register's: HandleEdgeRegister keeps
// them unconfirmed), so every family differs and the Edge sends each once: its
// report, its list, its catalog. From then on Core's digest for every family is
// the Edge's quote and the answer carries no members, and nothing is sent
// again, the floor's rebuild included. A family that never converges fails here
// three ways: the last answer is not the quote, its carrier reaches Core again
// (the resend guard's two minutes), and the Edge flags it not converging.
func TestScenario_TwoEdges_EveryOwnedFamilyConverges(t *testing.T) {
	t.Parallel()

	coreDB := coreharness.OpenDB(t)
	// As in TestScenario_TwoStationsOneProcess_EachSendsOnce: no pool timer on
	// the real clock, and the connections the run needs opened outside the
	// bubble.
	coreDB.SetConnMaxLifetime(0)
	coreDB.SetConnMaxIdleTime(0)
	coreDB.SetMaxIdleConns(8)
	var conns []*sql.Conn
	for i := 0; i < 6; i++ {
		c, err := coreDB.Conn(context.Background())
		mustNil(t, err, "open a Core connection")
		conns = append(conns, c)
	}
	for _, c := range conns {
		mustNil(t, c.Close(), "return a Core connection")
	}

	type ownedEdge struct {
		twinEdge
		catalog func() ([]protocol.CellCatalogEntry, error)
		quoted  map[string]string          // the last heartbeat's Owned
		ack     *protocol.EdgeHeartbeatAck // the last ack
	}
	edges := make([]*ownedEdge, 2)
	for i, uid := range []string{"edge.owned-a", "edge.owned-b"} {
		_, err := coreDB.EnrollEdge(uid, "")
		mustNil(t, err, "enroll "+uid)
		db := edgeharness.OpenDB(t)
		suffix := []string{"A", "B"}[i]
		node, plc := "OWN-N"+suffix, "OWN-PLC-"+suffix
		pid, err := db.CreateProcess("OWN-PROC-"+suffix, "", "", "", false)
		mustNil(t, err, "create process on "+uid)
		sid, err := db.CreateStyle("OWN-S1", "", pid)
		mustNil(t, err, "create style on "+uid)
		_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: sid, CoreNodeName: node, Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapModeSequential, PayloadCode: "SYN-OWN-1", UOPCapacity: 10,
		})
		mustNil(t, err, "claim on "+uid)
		_, err = db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: node, Code: "N1", Name: node, Enabled: true})
		mustNil(t, err, "process node on "+uid)
		_, err = db.CreateReportingPoint(plc, "COUNT", sid)
		mustNil(t, err, "reporting point on "+uid)
		mustNil(t, db.SetProcessCounter(pid, plc, "COUNT", true, "", true), "counter on "+uid)

		cfg := &config.Config{StationUID: uid}
		eng := edgeengine.New(edgeengine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
		// As cmd/shingoedge main.go wires them: claims through the publisher,
		// cells from the counted reporting points, and the engine's direct
		// sends (the watch list) through the outbox to the bus.
		pub := edgemessaging.NewPlantClaimsPublisher(db, uid, 0)
		pub.Reported, pub.ReportedAll = eng.ClaimsReported, eng.ClaimsReportedAll
		eng.SetClaimsSource(pub)
		catalog := func() ([]protocol.CellCatalogEntry, error) {
			pts, err := db.ListCountedReportingPoints()
			if err != nil {
				return nil, err
			}
			return edgemessaging.BuildCellCatalog(pts), nil
		}
		eng.SetCellCatalogFunc(catalog)
		eng.SetSendFunc(func(env *protocol.Envelope) error {
			data, err := env.Encode()
			if err != nil {
				return err
			}
			_, err = db.EnqueueOutbox(data, "data.send")
			return err
		})
		edges[i] = &ownedEdge{twinEdge: twinEdge{uid: uid, eng: eng, db: db}, catalog: catalog}
	}

	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		carried := map[string]map[string]int{} // station → carrier subject → messages that reached Core
		counted := func(env *protocol.Envelope, subject string) {
			mu.Lock()
			if carried[env.Src.Station] == nil {
				carried[env.Src.Station] = map[string]int{}
			}
			carried[env.Src.Station][subject]++
			mu.Unlock()
		}

		// Core: main's tables with no engine, routing what a boot and its
		// heartbeats send, each family's carrier counted on the way in.
		core := coreharness.NewTables(t, coreDB, nil,
			coreharness.RoutesOnly(protocol.SubjectEdgeRegister, protocol.SubjectEdgeHeartbeat, protocol.SubjectPlantClaims,
				protocol.SubjectBinsWatch, protocol.SubjectEdgeCells),
			coreharness.Tap(counted, protocol.SubjectPlantClaims, protocol.SubjectBinsWatch, protocol.SubjectEdgeCells))

		// One Edge-side door for both: the ack goes to the station it names.
		edgeSubjects := router.NewSubject()
		router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeHeartbeatAck, func(env *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
			for _, x := range edges {
				if x.uid == env.Dst.Station {
					x.ack = ack
					x.eng.OnCoreAck(ack)
				}
			}
		})
		edgeRouter := router.New[string]()
		router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
		edgeIngestor := protocol.NewIngestor(nil)
		edgeIngestor.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }

		buses := map[string]*harness.Bus{}
		for _, x := range edges {
			buses[x.uid] = harness.NewBus(t,
				harness.EdgeSide{EdgeStore: x.db, EdgeIngestor: edgeIngestor},
				harness.CoreSide{CoreStore: coreDB, CoreIngestor: core.Ingestor})
		}
		// send is one message from x and everything it sets moving, until both
		// sides are quiet.
		send := func(x *ownedEdge, subject string, body any) {
			env, err := protocol.NewDataEnvelope(subject,
				protocol.Address{Role: protocol.RoleEdge, Station: x.uid}, protocol.Address{Role: protocol.RoleCore}, body)
			mustNil(t, err, "build "+subject)
			data, err := env.Encode()
			mustNil(t, err, "encode "+subject)
			_, err = x.db.EnqueueOutbox(data, "data."+subject)
			mustNil(t, err, "enqueue "+subject)
			buses[x.uid].PumpAll()
		}
		// A heartbeat's Feeds and Owned are the engine's own (FeedDigests,
		// OwnedDigests, as cmd/shingoedge wires the heartbeater); the feeds Core
		// sends back are not applied, which leaves each family as its Edge's
		// store holds it.
		beat := func(x *ownedEdge) {
			x.quoted = x.eng.OwnedDigests()
			send(x, protocol.SubjectEdgeHeartbeat, &protocol.EdgeHeartbeat{Feeds: x.eng.FeedDigests(), Owned: x.quoted})
		}

		for _, x := range edges {
			cells, err := x.catalog()
			mustNil(t, err, "read the catalog of "+x.uid)
			if len(cells) == 0 {
				t.Fatalf("%s has no cell: its catalog family would converge on none, and show nothing", x.uid)
			}
			send(x, protocol.SubjectEdgeRegister, &protocol.EdgeRegister{Hostname: "owned-host", Version: "v-test", Catalog: cells})
			if d, held, err := coreDB.EdgeCellsDigest(x.uid); err != nil || !held || d != "" {
				t.Fatalf("%s: after the register Core holds cells %v at digest %q (err %v), want held with none", x.uid, held, d, err)
			}
		}
		for m := 0; m < 7; m++ {
			beat(edges[0])
			time.Sleep(30 * time.Second)
			beat(edges[1])
			time.Sleep(30 * time.Second)
		}

		none, err := protocol.OwnedDigest(nil)
		mustNil(t, err, "digest of no members")
		for _, x := range edges {
			for _, family := range protocol.AllOwned() {
				quote, named := x.quoted[family]
				if !named || quote == none {
					t.Errorf("%s's last heartbeat quotes %s as %q (named %v), want a family holding something: a family "+
						"of none converges with nothing sent, which shows nothing", x.uid, family, quote, named)
					continue
				}
				switch a, answered := x.ack.Owned[family]; {
				case !answered:
					t.Errorf("%s: the last ack does not answer %s (owned %v): Core left the family off", x.uid, family, x.ack.Owned)
				case a.Digest != quote || a.Members != nil:
					t.Errorf("%s: the last ack answers %s at %s with members %v, want the quote %s and no members: "+
						"the family has not converged", x.uid, family, a.Digest, a.Members, quote)
				}
			}
			for _, subject := range []string{protocol.SubjectPlantClaims, protocol.SubjectBinsWatch, protocol.SubjectEdgeCells} {
				if got := carried[x.uid][subject]; got != 1 {
					t.Errorf("%s: %d %s messages reached Core in seven minutes, want 1: each family is sent once and then "+
						"quoted back", x.uid, got, subject)
				}
			}
			if got := x.eng.FeedFlags(); len(got) != 0 {
				t.Errorf("%s's own flags are %v, want none", x.uid, got)
			}
			cells, err := x.catalog()
			mustNil(t, err, "read the catalog of "+x.uid)
			want, err := protocol.CellCatalogDigest(cells)
			mustNil(t, err, "digest the catalog of "+x.uid)
			if d, held, err := coreDB.EdgeCellsDigest(x.uid); err != nil || !held || d != want {
				t.Errorf("%s: Core holds cells %v at digest %q (err %v), want the Edge's catalog at %s", x.uid, held, d, err, want)
			}
		}
	})
}
