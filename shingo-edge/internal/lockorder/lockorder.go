// Package lockorder is the Edge's lock order, written once.
//
// Every lock on the Edge is declared lockcheck.Mutex[R] or lockcheck.RWMutex[R]
// with one of these as R. A goroutine may take a lock only while every lock it
// already holds has a lower rank; a test or sim build fails loudly on a take
// that breaks this, and on a lock taken twice (protocol/lockcheck). Production
// builds read none of it.
//
// The outer locks, in order (each may be held while taking any below it):
//
//	ConfigSave     a config save                       config/config.go
//	Node           a release act's process nodes, by id   engine/release_act.go
//	Prime          a keep-staged cell, by core node     engine/operator_produce.go
//	LoaderBudget   one loader's count-and-fire          engine/operator_demand_loader.go
//	CATIDMonitor   the style changeover monitor         engine/plc_catid_monitor.go
//	Count          the counter fence                    engine/engine.go
//	UOPFlush, UOPBin, UOPBucket   the unit accumulator  uop/accumulator.go
//	PLCManager, ManagedPLC        the PLCs              plc/manager.go
//	TickShipper, MessagingClient  the wire              messaging/
//
// Node and Prime are keyed: a release act takes its nodes sorted by id, and the
// keep-staged changeover its cells sorted by name, so instances of each are held
// together (Nests). Everything from rank 100 down is a leaf. SQLite sits below
// all of them: a transaction body takes no Go lock (LOCKS.md §5).
//
// The statement counters (QueryCount, and the sim's SimStatements) run inside
// the database driver, so they are taken under whatever lock is held across a
// query: they rank below every other app lock. The locks protocol packages
// declare rank below every app lock (protocol/lockcheck/ranks.go).
package lockorder

type (
	ConfigSave      struct{}
	Node            struct{}
	Prime           struct{}
	LoaderBudget    struct{}
	CATIDMonitor    struct{}
	Count           struct{}
	UOPFlush        struct{}
	UOPBin          struct{}
	UOPBucket       struct{}
	PLCManager      struct{}
	ManagedPLC      struct{}
	TickShipper     struct{}
	MessagingClient struct{}
	NodeLocks       struct{}

	Config          struct{}
	Backup          struct{}
	SpecChange      struct{}
	CoreNodes       struct{}
	PayloadBinTypes struct{}
	SceneGraph      struct{}
	SceneGeometry   struct{}
	Feeds           struct{}
	LoaderBurst     struct{}
	IntentQueue     struct{}
	StationScene    struct{}
	StationCurtain  struct{}
	StationTouch    struct{}
	StationView     struct{}
	SSEHub          struct{}
	QueryCount      struct{}

	SimOperator     struct{}
	SimStatements   struct{}
	SimDowntime     struct{}
	MachineDowntime struct{}
	SimPLC          struct{}
)

func (ConfigSave) Rank() int      { return 5 }
func (Node) Rank() int            { return 10 }
func (Prime) Rank() int           { return 20 }
func (LoaderBudget) Rank() int    { return 21 }
func (CATIDMonitor) Rank() int    { return 30 }
func (Count) Rank() int           { return 35 }
func (UOPFlush) Rank() int        { return 36 }
func (UOPBin) Rank() int          { return 37 }
func (UOPBucket) Rank() int       { return 38 }
func (PLCManager) Rank() int      { return 40 }
func (ManagedPLC) Rank() int      { return 41 }
func (TickShipper) Rank() int     { return 45 }
func (MessagingClient) Rank() int { return 46 }
func (NodeLocks) Rank() int       { return 50 }

func (Config) Rank() int          { return 100 }
func (Backup) Rank() int          { return 101 }
func (SpecChange) Rank() int      { return 102 }
func (CoreNodes) Rank() int       { return 110 }
func (PayloadBinTypes) Rank() int { return 111 }
func (SceneGraph) Rank() int      { return 112 }
func (SceneGeometry) Rank() int   { return 113 }
func (Feeds) Rank() int           { return 120 }
func (LoaderBurst) Rank() int     { return 121 }
func (IntentQueue) Rank() int     { return 122 }
func (StationScene) Rank() int    { return 130 }
func (StationCurtain) Rank() int  { return 131 }
func (StationTouch) Rank() int    { return 132 }
func (StationView) Rank() int     { return 133 }
func (SSEHub) Rank() int          { return 140 }
func (QueryCount) Rank() int      { return 990 }

func (SimOperator) Rank() int     { return 180 }
func (SimStatements) Rank() int   { return 991 }
func (SimDowntime) Rank() int     { return 182 }
func (MachineDowntime) Rank() int { return 183 }
func (SimPLC) Rank() int          { return 184 }

func (Node) Nests() bool  { return true }
func (Prime) Nests() bool { return true }

func (ConfigSave) Name() string      { return "config.Config.saveMu" }
func (Node) Name() string            { return "engine.nodeLock(node)" }
func (Prime) Name() string           { return "engine.primeNodeLock(cell)" }
func (LoaderBudget) Name() string    { return "engine.loaderBudgetLock(loader)" }
func (CATIDMonitor) Name() string    { return "engine.catidMonitor.mu" }
func (Count) Name() string           { return "engine.countMu" }
func (UOPFlush) Name() string        { return "uop.Accumulator.flushMu" }
func (UOPBin) Name() string          { return "uop.binDeltaEntry.mu" }
func (UOPBucket) Name() string       { return "uop.bucketLevelEntry.mu" }
func (PLCManager) Name() string      { return "plc.Manager.mu" }
func (ManagedPLC) Name() string      { return "plc.ManagedPLC.mu" }
func (TickShipper) Name() string     { return "messaging.TickShipper.shipMu" }
func (MessagingClient) Name() string { return "messaging.Client.mu" }
func (NodeLocks) Name() string       { return "engine.nodeLocks.mu" }

func (Config) Name() string          { return "config.Config.mu" }
func (Backup) Name() string          { return "backup.Service.mu" }
func (SpecChange) Name() string      { return "www.specChangeMu" }
func (CoreNodes) Name() string       { return "engine.coreNodesMu" }
func (PayloadBinTypes) Name() string { return "engine.payloadBinTypesMu" }
func (SceneGraph) Name() string      { return "engine.sceneGraphMu" }
func (SceneGeometry) Name() string   { return "engine.sceneGeometryMu" }
func (Feeds) Name() string           { return "engine.feedBook.mu" }
func (LoaderBurst) Name() string     { return "engine.loaderBurstTracker.mu" }
func (IntentQueue) Name() string     { return "engine.intentQueue.mu" }
func (StationScene) Name() string    { return "service.sceneMu" }
func (StationCurtain) Name() string  { return "service.curtainMu" }
func (StationTouch) Name() string    { return "service.touchMu" }
func (StationView) Name() string     { return "www.stationViewGroup.mu" }
func (SSEHub) Name() string          { return "www.EventHub.mu" }
func (QueryCount) Name() string      { return "store.queryCount.mu" }

func (SimOperator) Name() string     { return "engine.simOperator.mu" }
func (SimStatements) Name() string   { return "main.stmtObserver.mu" }
func (SimDowntime) Name() string     { return "simwarlink.DowntimeModel.mu" }
func (MachineDowntime) Name() string { return "simwarlink.MachineDowntime.mu" }
func (SimPLC) Name() string          { return "simwarlink.FakeClient.mu" }
