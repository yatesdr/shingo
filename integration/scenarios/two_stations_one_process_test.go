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
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coremessaging "shingocore/messaging"
	"shingocore/service"
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

	svc := coremessaging.NewCoreDataService(coreDB,
		coremessaging.NewCoreHandler(coreDB, nil, "core", "shingo.dispatch", nil), service.EpochAnnounce{})

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

		coreSubjects := router.NewSubject()
		router.RegisterSubject(coreSubjects, protocol.SubjectEdgeHeartbeat, svc.HandleEdgeHeartbeat)
		// The ack carries the bins watch digest, and an Edge re-sends a list
		// Core does not hold under the same guard: Core takes it, as
		// cmd/shingocore buildSubjectRouter routes it.
		router.RegisterSubject(coreSubjects, protocol.SubjectBinsWatch, svc.HandleBinWatch)
		router.RegisterSubject(coreSubjects, protocol.SubjectPlantClaims, func(env *protocol.Envelope, r *protocol.PlantClaimsReport) {
			if r.ProcessID == process {
				mu.Lock()
				reports[env.Src.Station] = append(reports[env.Src.Station], time.Since(start))
				mu.Unlock()
			}
			svc.HandlePlantClaims(env, r)
		})
		coreRouter := router.New[string]()
		router.Register(coreRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { coreSubjects.Dispatch(env, p) })
		coreIngestor := protocol.NewIngestor(nil)
		coreIngestor.Dispatch = func(env *protocol.Envelope) { coreRouter.Dispatch(env, env.Type) }

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
				harness.CoreSide{CoreStore: coreDB, CoreIngestor: coreIngestor})
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
				&protocol.EdgeHeartbeat{StationID: x.uid, Feeds: x.eng.FeedDigests(), Owned: x.eng.OwnedDigests()})
			mustNil(t, err, "build heartbeat")
			data, err := env.Encode()
			mustNil(t, err, "encode heartbeat")
			_, err = x.db.EnqueueOutbox(data, "data."+protocol.SubjectEdgeHeartbeat)
			mustNil(t, err, "enqueue heartbeat")
			buses[x.uid].PumpAll()
		}
		bothFlagged := func(when string) {
			for _, x := range twins {
				if !slices.ContainsFunc(svc.EdgeFeedFlags(x.uid), func(f string) bool {
					return strings.Contains(f, "process "+process+" reported by both")
				}) {
					t.Errorf("%s: Core's flags for %s are %v, want the process reported by both", when, x.uid, svc.EdgeFeedFlags(x.uid))
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
