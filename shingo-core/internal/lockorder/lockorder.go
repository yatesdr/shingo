// Package lockorder is Core's lock order, written once.
//
// Every lock in Core is declared lockcheck.Mutex[R] or lockcheck.RWMutex[R]
// with one of these as R. A goroutine may take a lock only while every lock it
// already holds has a lower rank; a test or sim build fails loudly on a take
// that breaks this, and on a lock taken twice (protocol/lockcheck). Production
// builds read none of it.
//
// The outer locks, in order (each may be held while taking any below it):
//
//	ConfigSave   the config page's save            www/router.go
//	OnDeck       the on-deck carried-bin recovery  engine/engine.go
//	Stage2Pair   one loader pair's stage-2 pull    engine/stage2_pull.go
//	Scan         the scanner: passes, acquires, doors   fulfillment/scanner.go
//	LaneGate     one node group's lane gate        dispatch/lane_gate_release.go
//	Summon, GateFail, Futility   taken under a lane gate
//
// Everything from rank 100 down is a leaf: it guards a map or a few fields and
// calls nothing that locks while held. The database sits below all of them: a
// transaction body takes no Go lock (LOCKS.md §3), and a lane's advisory lock
// and the row locks are taken inside one transaction, lanes ascending.
//
// The statement counter (QueryCount) runs inside the database driver, so it is
// taken under whatever lock is held across a query: it ranks below every other
// app lock. The locks protocol packages declare (the event bus, the clocks, the
// debug log) rank below every app lock (protocol/lockcheck/ranks.go).
package lockorder

type (
	ConfigSave struct{}
	OnDeck     struct{}
	Stage2Pair struct{}
	Scan       struct{}
	LaneGate   struct{}
	Summon     struct{}
	GateFail   struct{}
	Futility   struct{}

	MessagingClient struct{}
	MessagingFeeds  struct{}
	MessagingBins   struct{}
	RDSClient       struct{}
	RDSPoller       struct{}
	SeerScene       struct{}
	Config          struct{}
	Notifier        struct{}

	ThresholdMonitor     struct{}
	SourceabilityMonitor struct{}
	Maintainer           struct{}
	Stage2Pulls          struct{}
	StrandedNotes        struct{}
	DropObs              struct{}
	Robots               struct{}
	SceneGate            struct{}
	ArrivalTally         struct{}
	DriftTally           struct{}
	FaultTimers          struct{}
	FaultSent            struct{}

	Dissolve        struct{}
	FloorSeen       struct{}
	ReplyWatch      struct{}
	UngatedDigTally struct{}
	ETACache        struct{}
	LaneGateMap     struct{}
	ScanTrigger     struct{}

	BurialShadow struct{}
	FolderShadow struct{}
	StationNames struct{}
	UOPRepaired  struct{}
	QueryCount   struct{}
	CMSPoster    struct{}

	BuildInfo        struct{}
	GoroutineRing    struct{}
	DBWaitGauge      struct{}
	ExpiredDropGauge struct{}
	SSEHub           struct{}
	SSEClient        struct{}

	SimDeck   struct{}
	SimFleet  struct{}
	Simulator struct{}

	TestWedgeSweep struct{}
	TestShared     struct{}

	AfterQueue struct{}
)

func (ConfigSave) Rank() int { return 5 }
func (OnDeck) Rank() int     { return 10 }
func (Stage2Pair) Rank() int { return 11 }
func (Scan) Rank() int       { return 20 }
func (LaneGate) Rank() int   { return 30 }
func (Summon) Rank() int     { return 40 }
func (GateFail) Rank() int   { return 41 }
func (Futility) Rank() int   { return 42 }

func (MessagingClient) Rank() int { return 100 }
func (MessagingFeeds) Rank() int  { return 101 }
func (MessagingBins) Rank() int   { return 105 }
func (RDSClient) Rank() int       { return 102 }
func (RDSPoller) Rank() int       { return 103 }
func (SeerScene) Rank() int       { return 104 }
func (Config) Rank() int          { return 110 }
func (Notifier) Rank() int        { return 120 }

func (ThresholdMonitor) Rank() int     { return 130 }
func (SourceabilityMonitor) Rank() int { return 131 }
func (Maintainer) Rank() int           { return 132 }
func (Stage2Pulls) Rank() int          { return 133 }
func (StrandedNotes) Rank() int        { return 134 }
func (DropObs) Rank() int              { return 135 }
func (Robots) Rank() int               { return 136 }
func (SceneGate) Rank() int            { return 137 }
func (ArrivalTally) Rank() int         { return 138 }
func (DriftTally) Rank() int           { return 139 }
func (FaultTimers) Rank() int          { return 140 }
func (FaultSent) Rank() int            { return 141 }

func (Dissolve) Rank() int        { return 150 }
func (FloorSeen) Rank() int       { return 151 }
func (ReplyWatch) Rank() int      { return 152 }
func (UngatedDigTally) Rank() int { return 153 }
func (ETACache) Rank() int        { return 154 }
func (LaneGateMap) Rank() int     { return 155 }
func (ScanTrigger) Rank() int     { return 156 }

func (BurialShadow) Rank() int { return 160 }
func (FolderShadow) Rank() int { return 161 }
func (StationNames) Rank() int { return 162 }
func (UOPRepaired) Rank() int  { return 163 }
func (QueryCount) Rank() int   { return 990 }
func (CMSPoster) Rank() int    { return 165 }

func (BuildInfo) Rank() int        { return 170 }
func (GoroutineRing) Rank() int    { return 171 }
func (DBWaitGauge) Rank() int      { return 172 }
func (ExpiredDropGauge) Rank() int { return 173 }
func (SSEHub) Rank() int           { return 175 }
func (SSEClient) Rank() int        { return 176 }

func (SimDeck) Rank() int   { return 180 }
func (SimFleet) Rank() int  { return 181 }
func (Simulator) Rank() int { return 182 }

func (TestWedgeSweep) Rank() int { return 185 }
func (TestShared) Rank() int     { return 186 }

func (AfterQueue) Rank() int { return 190 }

func (ConfigSave) Name() string { return "www.configMu" }
func (OnDeck) Name() string     { return "engine.onDeckMu" }
func (Stage2Pair) Name() string { return "engine.stage2 pair lock" }
func (Scan) Name() string       { return "fulfillment.scanMu" }
func (LaneGate) Name() string   { return "dispatch.laneGates[group]" }
func (Summon) Name() string     { return "dispatch.summonMu" }
func (GateFail) Name() string   { return "dispatch.gateFailMu" }
func (Futility) Name() string   { return "dispatch.FutilityDetector.mu" }

func (MessagingClient) Name() string { return "messaging.Client.mu" }
func (MessagingFeeds) Name() string  { return "messaging.feeds.mu" }
func (MessagingBins) Name() string   { return "messaging.binsFeed.mu" }
func (RDSClient) Name() string       { return "rds.Client.mu" }
func (RDSPoller) Name() string       { return "rds.Poller.mu" }
func (SeerScene) Name() string       { return "seerrds.sceneMu" }
func (Config) Name() string          { return "config.Config.mu" }
func (Notifier) Name() string        { return "notify.Notifier.mu" }

func (ThresholdMonitor) Name() string     { return "engine.ThresholdMonitor.mu" }
func (SourceabilityMonitor) Name() string { return "engine.SourceabilityMonitor.mu" }
func (Maintainer) Name() string           { return "engine.Maintainer.mu" }
func (Stage2Pulls) Name() string          { return "engine.stage2Puller.mu" }
func (StrandedNotes) Name() string        { return "engine.strandedNotesMu" }
func (DropObs) Name() string              { return "engine.dropObsMu" }
func (Robots) Name() string               { return "engine.robotsMu" }
func (SceneGate) Name() string            { return "engine.sceneGateMu" }
func (ArrivalTally) Name() string         { return "engine.arrivalRefusalTally.mu" }
func (DriftTally) Name() string           { return "engine.destNodeDriftTally.mu" }
func (FaultTimers) Name() string          { return "engine.faultTimersMu" }
func (FaultSent) Name() string            { return "engine.faultSentMu" }

func (Dissolve) Name() string        { return "dispatch.dissolveMu" }
func (FloorSeen) Name() string       { return "dispatch.floorSeenMu" }
func (ReplyWatch) Name() string      { return "dispatch.ReplySender.watchMu" }
func (UngatedDigTally) Name() string { return "dispatch.ungatedDigTally.mu" }
func (ETACache) Name() string        { return "eta.Cache.mu" }
func (LaneGateMap) Name() string     { return "dispatch.laneGateSerializer.mu" }
func (ScanTrigger) Name() string     { return "fulfillment.triggerMu" }

func (BurialShadow) Name() string { return "service.burialShadow.mu" }
func (FolderShadow) Name() string { return "service.folderShadow.mu" }
func (StationNames) Name() string { return "service.stationNames.mu" }
func (UOPRepaired) Name() string  { return "uop.Applier.repairedMu" }
func (QueryCount) Name() string   { return "store.queryCount.mu" }
func (CMSPoster) Name() string    { return "poster.Poster.mu" }

func (BuildInfo) Name() string        { return "www.buildInfo.mu" }
func (GoroutineRing) Name() string    { return "www.goroutineRing.mu" }
func (DBWaitGauge) Name() string      { return "www.dbWaitGauge.mu" }
func (ExpiredDropGauge) Name() string { return "www.expiredDropGauge.mu" }
func (SSEHub) Name() string           { return "www.EventHub.mu" }
func (SSEClient) Name() string        { return "www.sseClient.mu" }

func (SimDeck) Name() string   { return "simulator.deck.mu" }
func (SimFleet) Name() string  { return "simulator.driver.fleetMu" }
func (Simulator) Name() string { return "simulator.Simulator.mu" }

func (TestWedgeSweep) Name() string { return "testdb.wedgeSweepMu" }
func (TestShared) Name() string     { return "testdb.sharedMu" }

func (AfterQueue) Name() string { return "engine.afterQueue.mu" }
