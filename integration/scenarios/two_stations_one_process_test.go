// Two stations publishing one process, both sides real: Core's plant_claims_reports
// row and the claims its heartbeat ack quotes (shingocore messaging
// HandlePlantClaims, HandleEdgeHeartbeat; store/plantclaims reports.go), and
// each Edge's own plant spec, publisher and resend guard (shingoedge messaging
// PlantClaimsPublisher.ReconcileClaims behind Engine.OnCoreAck, wired as
// cmd/shingoedge main.go wires it). The Edges' clock is a testing/synctest
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
// keeps one report row per process, last writer wins, and quotes a station only
// the rows it holds; so each report takes the process off the other station's
// ack, and that station re-sends as soon as its guard lets it: one station every
// 2 minutes on the guard (protocol/resend Guard), the other every 2 minutes on
// the "Core lost what it held" rule (resend.Record.Send), both forever. Core's
// "reported by both" flag goes up on both stations at the second station's
// first report and stays up; the Edge's own not-converging flag shows on the
// first station only. All four are asserted below.
//
// RED BY DESIGN. Lane C5 is to key plant_claims_reports by (station_uid,
// process_id): each station is then quoted its own digest and should send
// once, while the flag stays on both. This test then fails; replace the
// cadence assertion with that one.
func TestScenario_TwoStationsOneProcess_EachReSendsEveryTwoMinutes(t *testing.T) {
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
		cfg := &config.Config{StationUID: uid, Namespace: "test", LineID: uid, Messaging: config.MessagingConfig{StationID: uid}}
		eng := edgeengine.New(edgeengine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
		pub := edgemessaging.NewPlantClaimsPublisher(db, uid, 0)
		eng.SetClaimsAckFunc(func(claims map[string]string) { pub.ReconcileClaims(claims, eng) })
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
		// quiet. Feeds is the engine's own (FeedDigests, as cmd/shingoedge wires
		// the heartbeater); the feeds Core sends back are not applied here, which
		// leaves the claims, the bins watch list and the ack's quote of each as
		// they are.
		beat := func(x *twinEdge) {
			env, err := protocol.NewDataEnvelope(protocol.SubjectEdgeHeartbeat,
				protocol.Address{Role: protocol.RoleEdge, Station: x.uid}, protocol.Address{Role: protocol.RoleCore},
				&protocol.EdgeHeartbeat{StationID: x.uid, Feeds: x.eng.FeedDigests()})
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
			a.uid: at("0s", "2m", "4m", "6m", "8m"),
			b.uid: at("30s", "2m30s", "4m30s", "6m30s", "8m30s"),
		}
		for _, x := range twins {
			if got := reports[x.uid]; !slices.Equal(got, want[x.uid]) {
				t.Errorf("%s's reports of %s reached Core at %v, want %v: each station re-sends every 2 minutes, forever "+
					"(if lane C5 has keyed plant_claims_reports by station and process, each station sends once: "+
					"replace this assertion with that one; the flag stays on both)", x.uid, process, got, want[x.uid])
			}
		}
		if got := a.eng.FeedFlags(); !slices.Equal(got, []string{"claims:" + process + " not converging: sent 5 times"}) {
			t.Errorf("%s's own flags are %v, want the process not converging after 5 sends", a.uid, got)
		}
		if got := b.eng.FeedFlags(); len(got) != 0 {
			t.Errorf("%s's own flags are %v, want none: each of its sends is quoted back once before the next", b.uid, got)
		}
	})
}
