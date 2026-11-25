package testharness

import (
	"fmt"
	"log"
	"slices"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
	"shingocore/asks"
	"shingocore/engine"
	"shingocore/messaging"
	"shingocore/messaging/middleware"
	"shingocore/service"
	"shingocore/www"
)

// Core is Core's messaging side composed around a running engine as the plant
// binary composes it (cmd/shingocore/main.go, routers.go): the order handler,
// the data service with the hooks main sets on it and on the engine, the ask
// server over the doors, the subject and protocol tables, and an ingestor that
// routes through them. A harness Bus delivers the Edge's envelopes into
// Ingestor. The twin of shingoedge/testharness.NewEdge.
//
// What a cell leaves out of a plant's composition it says with options
// (RoutesOnly, HooksOnly, NoInboxDedup, NoExpiredAnswers). NewCore's
// statements are main's own but four that differ on purpose (harnessOnly in
// core_guard_test.go); TestNewCore_MakesWhatMainMakes reads main.go,
// routers.go and this file and fails when they part, naming what to change.
//
// Not made here, as NewEdge leaves out its own: the Kafka client and what
// rides it (the ingestor's subscribe, the robot feed's topic and reader), the
// stale-edge pass and the heartbeat maintenance loops, the debug log (mainOnly
// gives each its reason); the outbox drainer (the Bus pumps the outbox); and
// what main starts on the engine after Start and before this (the scan probe,
// the after-queue watch, the futility detector).
type Core struct {
	Engine   *engine.Engine
	Handler  *messaging.CoreHandler
	Data     *messaging.CoreDataService
	AskKinds []string
	Ingestor *protocol.Ingestor
}

// Hook is one of the calls main makes to tie the engine and the data service
// together, named by what it carries.
type Hook string

const (
	HookThresholdMonitor   Hook = "threshold monitor"   // coreDataService.SetThresholdMonitor
	HookSourcingReport     Hook = "sourcing report"     // coreDataService.SetSourcingReport
	HookPlantTimezone      Hook = "plant timezone"      // coreDataService.SetPlantTimezone
	HookFaultWindow        Hook = "fault window"        // coreDataService.SetFaultWindow
	HookEdgeFeedFlags      Hook = "edge feed flags"     // eng.SetEdgeFeedFlagsFunc
	HookContainmentChanged Hook = "containment changed" // eng.SetContainmentChangedFunc
	HookFeedsChanged       Hook = "feeds changed"       // eng.SetFeedsChangedFunc
	HookCatalogRow         Hook = "catalog row"         // eng.SetCatalogRowFunc
	HookBinChanged         Hook = "bin changed"         // eng.SetBinChangedFunc
	HookBinsRows           Hook = "bins rows"           // eng.SetBinsRowsFunc
	HookGroupLines         Hook = "group lines"         // coreDataService.SetGroupLinesFunc
	HookLevelChanged       Hook = "level changed"       // eng.SetLevelChangedFunc
	HookBinsFeed           Hook = "bins feed"           // eng.SubscribeBinsFeed
	HookAskKinds           Hook = "ask kinds"           // coreDataService.SetAskKinds
	HookCellTick           Hook = "cell tick"           // coreDataService.SetCellTickEmitter
	HookBinCount           Hook = "bin count"           // coreDataService.SetBinCountEmitter
)

// CoreOption narrows NewCore's composition for one cell.
type CoreOption func(*coreOptions)

type coreOptions struct {
	routes  map[string]bool // nil: every type and subject main routes
	hooks   map[Hook]bool   // nil: every hook main sets
	noDedup bool
	noOnExp bool
}

func (o coreOptions) routed(name string) bool { return o.routes == nil || o.routes[name] }
func (o coreOptions) on(h Hook) bool          { return o.hooks == nil || o.hooks[h] }

// RoutesOnly routes only the named order-channel envelope types and data
// subjects, as the cell's own table did; every other one main's tables route
// is left out. The data envelope itself and the reply types Core only sends
// are routed whatever the list says: Core drops a data subject it does not
// route, and should never receive a reply type (routers.go).
func RoutesOnly(names ...string) CoreOption {
	return func(o *coreOptions) {
		o.routes = map[string]bool{}
		for _, n := range names {
			o.routes[n] = true
		}
	}
}

// HooksOnly makes only the named hooks; every other one main makes is left
// out. HooksOnly() with none leaves out every hook.
func HooksOnly(hooks ...Hook) CoreOption {
	return func(o *coreOptions) {
		o.hooks = map[Hook]bool{}
		for _, h := range hooks {
			o.hooks[h] = true
		}
	}
}

// NoInboxDedup leaves out the inbox dedup main puts in front of the
// order-channel types.
func NoInboxDedup() CoreOption { return func(o *coreOptions) { o.noDedup = true } }

// NoExpiredAnswers leaves the ingestor's answer to an expired ask unset.
func NoExpiredAnswers() CoreOption { return func(o *coreOptions) { o.noOnExp = true } }

// NewCore composes Core's messaging side around eng, which the caller has
// started, and doors, the door bodies the caller's web layer was built with
// (www.NewDoors, www.NewRouter), in main's order.
//
// The bins feed (HookBinsFeed) is subscribed here, after the web layer the
// caller built; main subscribes it before (TestSubscriberCensus). So in a cell
// that turns it on, the screens' subscribers run before the feed's for one
// event, where in a plant the feed's run first
// (TestBusOrder_ADeliveryAndACancel). Every cell leaves it out today.
func NewCore(t testing.TB, eng *engine.Engine, doors *www.Doors, opts ...CoreOption) *Core {
	t.Helper()
	o := newCoreOptions(t, opts)
	db, cfg := eng.DB(), eng.AppConfig()

	coreHandler := messaging.NewCoreHandler(db, nil, cfg.Messaging.StationID, cfg.Messaging.DispatchTopic, eng.Dispatcher())
	coreDataService := messaging.NewCoreDataService(db, coreHandler, service.EpochAnnounce{
		Topic:       cfg.Messaging.DispatchTopic,
		CoreStation: cfg.Messaging.StationID,
	})
	if o.on(HookThresholdMonitor) {
		coreDataService.SetThresholdMonitor(eng.ThresholdMonitor())
	}
	if o.on(HookSourcingReport) {
		if sm := eng.SourceabilityMonitor(); sm != nil {
			coreDataService.SetSourcingReport(sm.StationReport)
		}
	}
	if o.on(HookPlantTimezone) {
		coreDataService.SetPlantTimezone(www.OfferedPlantTimezone(cfg.Timezone))
	}
	if o.on(HookFaultWindow) {
		coreDataService.SetFaultWindow(cfg.RDS.FaultGrace, cfg.RDS.FaultNoticeAfter)
	}
	if o.on(HookEdgeFeedFlags) {
		eng.SetEdgeFeedFlagsFunc(coreDataService.EdgeFeedFlags)
	}
	if o.on(HookContainmentChanged) {
		eng.SetContainmentChangedFunc(coreDataService.ContainmentChanged)
	}
	if o.on(HookFeedsChanged) {
		eng.SetFeedsChangedFunc(coreDataService.FeedsChanged)
	}
	if o.on(HookCatalogRow) {
		eng.SetCatalogRowFunc(coreDataService.CatalogRow)
	}
	if o.on(HookBinChanged) {
		eng.SetBinChangedFunc(func(binID int64) {
			coreDataService.BinsChanged(binID)
			eng.EmitBinRowChanged(binID)
		})
	}
	if o.on(HookBinsRows) {
		eng.SetBinsRowsFunc(coreDataService.NodeBinsRows)
	}
	if o.on(HookGroupLines) {
		coreDataService.SetGroupLinesFunc(eng.UnloaderGroupLines)
	}
	if o.on(HookLevelChanged) {
		eng.SetLevelChangedFunc(coreDataService.BinsLevelChanged)
	}
	if o.on(HookBinsFeed) {
		eng.SubscribeBinsFeed(engine.BinsFeedHooks{
			Changed: coreDataService.BinsChanged,
			Started: coreDataService.BinsOrderStarted,
			Ended:   coreDataService.BinsOrderEnded,
		})
	}

	askServer, err := buildAskServer(db, coreHandler.ReplyAnswer, doors)
	if err != nil {
		t.Fatalf("testharness.NewCore: %v", err)
	}
	if o.on(HookAskKinds) {
		coreDataService.SetAskKinds(askServer.Kinds)
	}
	subjectRouter := buildSubjectRouter(coreDataService, askServer, o)
	if o.on(HookCellTick) {
		coreDataService.SetCellTickEmitter(eng.EmitCellTick)
	}
	if o.on(HookBinCount) {
		coreDataService.SetBinCountEmitter(eng.EmitBinRowChanged)
	}

	ingestor := protocol.NewIngestor(func(_ *protocol.RawHeader) bool { return true })
	if !o.noOnExp {
		ingestor.OnExpired = askServer.AnswerExpired
	}
	if cfg.Messaging.SigningKey != "" {
		ingestor.SigningKey = []byte(cfg.Messaging.SigningKey)
		log.Printf("shingocore: envelope signing enabled")
	}
	var dedup router.Middleware
	if !o.noDedup {
		dedup = middleware.NewInboxDedup(db, nil)
	}
	protoRouter := buildProtocolRouter(coreHandler, subjectRouter, dedup, nil, o)
	ingestor.Dispatch = func(env *protocol.Envelope) {
		protoRouter.Dispatch(env, env.Type)
	}
	return &Core{Engine: eng, Handler: coreHandler, Data: coreDataService, AskKinds: askServer.Kinds(), Ingestor: ingestor}
}

// newCoreOptions applies opts, and fails a route RoutesOnly names that Core
// does not route.
func newCoreOptions(t testing.TB, opts []CoreOption) coreOptions {
	t.Helper()
	var o coreOptions
	for _, opt := range opts {
		opt(&o)
	}
	for name := range o.routes {
		if !slices.Contains(coreRoutes, name) {
			t.Fatalf("testharness.RoutesOnly: %q is not a type or subject Core routes (%v)", name, coreRoutes)
		}
	}
	return o
}

// coreRoutes is every order-channel type and data subject main's tables route
// (the guard holds it to routers.go).
var coreRoutes = []string{
	protocol.TypeOrderRequest, protocol.TypeOrderCancel, protocol.TypeOrderReceipt, protocol.TypeOrderRedirect,
	protocol.TypeComplexOrderRequest, protocol.TypeOrderRelease, protocol.TypeOrderIngest,
	protocol.SubjectEdgeRegister, protocol.SubjectEdgeHeartbeat, protocol.SubjectNodeListRequest,
	protocol.SubjectCatalogPayloadsRequest, protocol.SubjectOrderStatusRequest, protocol.SubjectBinUOPDelta,
	protocol.SubjectLinesideBucketLevel, protocol.SubjectProductionTicks, protocol.SubjectPlantClaims,
	protocol.SubjectLinesideLevelReport, protocol.SubjectDemandOrigin, protocol.SubjectSupplyRefusal,
	protocol.SubjectHoldDropped, protocol.SubjectBinsWatch, protocol.SubjectEdgeAsk, protocol.SubjectEdgeCells,
}

// buildSubjectRouter is routers.go's, each registration behind the cell's
// routes.
func buildSubjectRouter(svc *messaging.CoreDataService, askServer *asks.Server, o coreOptions) *router.SubjectRouter {
	r := router.NewSubject()
	if o.routed(protocol.SubjectEdgeRegister) {
		router.RegisterSubject(r, protocol.SubjectEdgeRegister, svc.HandleEdgeRegister)
	}
	if o.routed(protocol.SubjectEdgeHeartbeat) {
		router.RegisterSubject(r, protocol.SubjectEdgeHeartbeat, svc.HandleEdgeHeartbeat)
	}
	if o.routed(protocol.SubjectNodeListRequest) {
		router.RegisterSubject(r, protocol.SubjectNodeListRequest, svc.HandleNodeListRequest)
	}
	if o.routed(protocol.SubjectCatalogPayloadsRequest) {
		router.RegisterSubjectBare(r, protocol.SubjectCatalogPayloadsRequest, svc.HandleCatalogPayloadsRequest)
	}
	if o.routed(protocol.SubjectOrderStatusRequest) {
		router.RegisterSubject(r, protocol.SubjectOrderStatusRequest, svc.HandleOrderStatusRequest)
	}
	if o.routed(protocol.SubjectBinUOPDelta) {
		router.RegisterSubject(r, protocol.SubjectBinUOPDelta, svc.HandleBinUOPDelta)
	}
	if o.routed(protocol.SubjectLinesideBucketLevel) {
		router.RegisterSubject(r, protocol.SubjectLinesideBucketLevel, svc.HandleLinesideBucketLevel)
	}
	if o.routed(protocol.SubjectProductionTicks) {
		router.RegisterSubject(r, protocol.SubjectProductionTicks, svc.HandleProductionTicks)
	}
	if o.routed(protocol.SubjectPlantClaims) {
		router.RegisterSubject(r, protocol.SubjectPlantClaims, svc.HandlePlantClaims)
	}
	if o.routed(protocol.SubjectLinesideLevelReport) {
		router.RegisterSubject(r, protocol.SubjectLinesideLevelReport, svc.HandleLinesideLevelReport)
	}
	if o.routed(protocol.SubjectDemandOrigin) {
		router.RegisterSubject(r, protocol.SubjectDemandOrigin, svc.HandleDemandOrigin)
	}
	if o.routed(protocol.SubjectSupplyRefusal) {
		router.RegisterSubject(r, protocol.SubjectSupplyRefusal, svc.HandleSupplyRefusal)
	}
	if o.routed(protocol.SubjectHoldDropped) {
		router.RegisterSubject(r, protocol.SubjectHoldDropped, svc.HandleHoldDropped)
	}
	if o.routed(protocol.SubjectBinsWatch) {
		router.RegisterSubject(r, protocol.SubjectBinsWatch, svc.HandleBinWatch)
	}
	if o.routed(protocol.SubjectEdgeAsk) {
		router.RegisterSubject(r, protocol.SubjectEdgeAsk, askServer.Handle)
	}
	if o.routed(protocol.SubjectEdgeCells) {
		router.RegisterSubject(r, protocol.SubjectEdgeCells, svc.HandleEdgeCells)
	}
	return r
}

// buildProtocolRouter is routers.go's, each order-channel registration behind
// the cell's routes and the dedup behind its option.
func buildProtocolRouter(
	h *messaging.CoreHandler,
	subjectRouter *router.SubjectRouter,
	dedupMW router.Middleware,
	dataDbg func(string, ...any),
	o coreOptions,
) *router.Router[string] {
	r := router.New[string]()
	if dedupMW != nil {
		r.UseFor(dedupMW,
			protocol.TypeOrderRequest,
			protocol.TypeOrderCancel,
			protocol.TypeOrderReceipt,
			protocol.TypeOrderRedirect,
			protocol.TypeComplexOrderRequest,
			protocol.TypeOrderRelease,
			protocol.TypeOrderIngest,
		)
	}
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		if dataDbg != nil {
			dataDbg("data: subject=%s body_size=%d from=%s", p.Subject, len(p.Body), env.Src.Station)
		}
		subjectRouter.Dispatch(env, p)
	})
	if o.routed(protocol.TypeOrderRequest) {
		router.Register(r, protocol.TypeOrderRequest, h.HandleOrderRequest)
	}
	if o.routed(protocol.TypeOrderCancel) {
		router.Register(r, protocol.TypeOrderCancel, h.HandleOrderCancel)
	}
	if o.routed(protocol.TypeOrderReceipt) {
		router.Register(r, protocol.TypeOrderReceipt, h.HandleOrderReceipt)
	}
	if o.routed(protocol.TypeOrderRedirect) {
		router.Register(r, protocol.TypeOrderRedirect, h.HandleOrderRedirect)
	}
	if o.routed(protocol.TypeComplexOrderRequest) {
		router.Register(r, protocol.TypeComplexOrderRequest, h.HandleComplexOrderRequest)
	}
	if o.routed(protocol.TypeOrderRelease) {
		router.Register(r, protocol.TypeOrderRelease, h.HandleOrderRelease)
	}
	if o.routed(protocol.TypeOrderIngest) {
		router.Register(r, protocol.TypeOrderIngest, h.HandleOrderIngest)
	}
	router.Register(r, protocol.TypeOrderAck, func(*protocol.Envelope, *protocol.OrderAck) {})
	router.Register(r, protocol.TypeOrderWaybill, func(*protocol.Envelope, *protocol.OrderWaybill) {})
	router.Register(r, protocol.TypeOrderUpdate, func(*protocol.Envelope, *protocol.OrderUpdate) {})
	router.Register(r, protocol.TypeOrderDelivered, func(*protocol.Envelope, *protocol.OrderDelivered) {})
	router.Register(r, protocol.TypeOrderError, func(*protocol.Envelope, *protocol.OrderError) {})
	router.Register(r, protocol.TypeOrderCancelled, func(*protocol.Envelope, *protocol.OrderCancelled) {})
	router.Register(r, protocol.TypeOrderStaged, func(*protocol.Envelope, *protocol.OrderStaged) {})
	router.Register(r, protocol.TypeOrderSkipped, func(*protocol.Envelope, *protocol.OrderSkipped) {})
	return r
}

// buildAskServer is routers.go's.
func buildAskServer(db asks.Store, reply asks.Reply, d *www.Doors) (*asks.Server, error) {
	s := asks.New(db, reply)
	for _, a := range d.AskDoors() {
		s.Register(a.Kind, a.Door, a.Replay)
	}
	want := append([]string(nil), protocol.AskKinds()...)
	sort.Strings(want)
	if got := s.Kinds(); !slices.Equal(got, want) {
		return nil, fmt.Errorf("ask server answers %v, protocol.AskKinds is %v — "+
			"register each kind once in www.Doors.AskDoors", got, want)
	}
	return s, nil
}
